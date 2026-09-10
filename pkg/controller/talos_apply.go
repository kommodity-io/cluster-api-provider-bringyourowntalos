package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	yamlv3 "gopkg.in/yaml.v3"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// talosAPIPort is the secure Talos machine API port. In maintenance mode
	// it accepts ApplyConfiguration with unverified TLS.
	talosAPIPort = "50000"

	// applyTimeout bounds a single ApplyConfiguration attempt.
	applyTimeout = 2 * time.Minute

	// resetTimeout bounds a single Reset attempt.
	resetTimeout = 30 * time.Second

	// upgradeTimeout bounds a single LifecycleClient.Upgrade stream drain
	// (the install-to-disk phase). Reboot after is bounded by rebootTimeout;
	// the image pull before it by imagePullTimeout.
	upgradeTimeout = 10 * time.Minute

	// imagePullTimeout bounds the pre-upgrade ImageClient.Pull stream. The
	// installer image is pulled into the CRI containerd store before the
	// LifecycleClient.Upgrade server reads it.
	imagePullTimeout = 5 * time.Minute

	// rebootTimeout bounds the post-install Reboot RPC (unary, returns
	// immediately; the machine reboots after). A failure here is non-fatal to
	// the upgrade state machine (install already succeeded).
	rebootTimeout = 30 * time.Second

	// probeTimeout bounds a maintenance-mode probe attempt.
	probeTimeout = 10 * time.Second

	// talosServiceRunning is the Talos service state reported by ServiceInfo for
	// a service that is fully up and healthy.
	talosServiceRunning = "Running"

	// talosLabelState is the STATE system-volume label wiped on reset.
	talosLabelState = "STATE"

	// talosLabelEphemeral is the EPHEMERAL system-volume label wiped on reset.
	talosLabelEphemeral = "EPHEMERAL"
)

// applyMachineConfig applies the given Talos machine configuration to the
// machine at publicIP. When talosConfig is nil, the machine is assumed to run
// in maintenance mode and an unverified TLS client is used; otherwise the
// talosconfig's client credentials authenticate the request.
func applyMachineConfig(ctx context.Context, publicIP string, machineConfig []byte, talosConfig []byte) error {
	var (
		client *talosclient.Client
		err    error
	)

	if talosConfig != nil {
		client, err = authenticatedClient(ctx, publicIP, talosConfig)
	} else {
		client, err = maintenanceClient(ctx, publicIP)
	}

	if err != nil {
		return err
	}

	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), applyTimeout)
	defer cancel()

	_, err = client.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: machineConfig,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	})
	if err != nil {
		return fmt.Errorf("failed to apply machine configuration on %s: %w", publicIP, err)
	}

	return nil
}

// maintenanceClient builds a client for a machine in maintenance mode, which
// performs no client authentication. Intentional for adoption only.
func maintenanceClient(ctx context.Context, publicIP string) (*talosclient.Client, error) {
	//nolint:gosec // Maintenance mode has no PKI material; apply must skip verification.
	tlsConfig := &tls.Config{InsecureSkipVerify: true}

	endpoint := net.JoinHostPort(publicIP, talosAPIPort)

	client, err := talosclient.New(ctx, talosclient.WithTLSConfig(tlsConfig), talosclient.WithEndpoints(endpoint))
	if err != nil {
		return nil, fmt.Errorf("failed to create Talos maintenance client for %s: %w", endpoint, err)
	}

	return client, nil
}

// authenticatedClient builds an mTLS client for a machine already booted with
// a machine configuration, using credentials from the given talosconfig.
func authenticatedClient(ctx context.Context, publicIP string, talosConfig []byte) (*talosclient.Client, error) {
	config, err := clientconfig.FromBytes(talosConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse talosconfig: %w", err)
	}

	endpoint := net.JoinHostPort(publicIP, talosAPIPort)

	client, err := talosclient.New(
		ctx,
		talosclient.WithConfig(config),
		talosclient.WithEndpoints(endpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticated Talos client for %s: %w", endpoint, err)
	}

	return client, nil
}

// versionProbeAuthenticated returns the live Talos version tag of the machine
// at publicIP, queried over the cluster talosconfig (mTLS). Used by the
// post-adoption upgrade path: after the bootstrap config is applied the host
// carries the cluster PKI bundle, so only the authenticated client is accepted.
func versionProbeAuthenticated(ctx context.Context, publicIP string, talosConfig []byte) (string, error) {
	return versionProbeWithClient(ctx, publicIP, func(ctx context.Context) (*talosclient.Client, error) {
		return authenticatedClient(ctx, publicIP, talosConfig)
	})
}

// versionProbeWithClient runs the Version RPC against the machine at publicIP
// using the given client builder and returns the reported tag.
func versionProbeWithClient(
	ctx context.Context,
	publicIP string,
	buildClient func(context.Context) (*talosclient.Client, error),
) (string, error) {
	client, err := buildClient(ctx)
	if err != nil {
		return "", err
	}

	defer client.Close() //nolint:errcheck

	probeCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	resp, err := client.Version(probeCtx)
	if err != nil {
		return "", fmt.Errorf("version probe failed on %s: %w", publicIP, err)
	}

	if len(resp.GetMessages()) == 0 {
		return "", fmt.Errorf("version probe returned no messages on %s: %w", publicIP, ErrVersionProbeEmpty)
	}

	info := resp.GetMessages()[0].GetVersion()
	if info == nil {
		return "", fmt.Errorf("version probe returned no version info on %s: %w", publicIP, ErrVersionProbeNoInfo)
	}

	return info.GetTag(), nil
}

// upgradeExitCodeError signals the LifecycleClient.Upgrade stream completed
// with a nonzero exit code. The server sends the exit code as a terminal
// stream message then closes the stream cleanly (no gRPC error), so the
// drain loop must surface it as an error for the state machine's failure
// accounting. Code < 0 (sentinel -1) means the stream ended without an
// exit-code message (unexpected EOF).
type upgradeExitCodeError struct {
	Code int32
	Msg  string
}

func (e *upgradeExitCodeError) Error() string {
	return fmt.Sprintf("upgrade stream exited with code %d: %s", e.Code, e.Msg)
}

// errNoSystemDisk indicates the host reported no system disk via the
// storage API. The lifecycle Upgrade RPC needs a system disk to pass to the
// installer; without one the upgrade cannot proceed.
var errNoSystemDisk = errors.New("no system disk found")

// errBootIDUnauthorized indicates the client credentials cannot read the
// host boot-id. The InFlight poll falls back to a reachability +
// version-change completion check when this is returned at capture time.
var errBootIDUnauthorized = errors.New("boot-id read permission denied")

// upgradeMachine performs a Talos lifecycle upgrade of the machine at
// publicIP to the given installer image ref, over the cluster talosconfig
// (mTLS). It runs synchronously in one reconcile:
//
//  1. Pull the installer image into the CRI containerd store
//     (ImageClient.Pull stream, drained to completion).
//  2. Issue LifecycleClient.Upgrade and drain its stream to the terminal
//     exit-code message (install-to-disk; force=false, preserve is implicit).
//  3. Capture the pre-reboot boot-id (for the InFlight completion gate).
//  4. Issue Reboot (best-effort; install already succeeded at this point).
//
// Requires the host to already carry the cluster PKI bundle (i.e. be
// adopted): the lifecycle Upgrade RPC is Admin-only and the maintenance
// client is Reader-only. Returns the captured pre-reboot boot-id (empty when
// the read failed or was permission-denied) so the caller can stash it on
// status for the InFlight poll.
func upgradeMachine(ctx context.Context, publicIP string, talosConfig []byte, image string) (string, error) {
	client, err := authenticatedClient(ctx, publicIP, talosConfig)
	if err != nil {
		return "", err
	}

	defer client.Close() //nolint:errcheck

	// Containerd instance for the pull + upgrade: CRI driver, SYSTEM
	// namespace — matches talosctl's default so the pulled image lands where
	// the LifecycleClient.Upgrade server's GetImage lookup reads it.
	containerdInst := &common.ContainerdInstance{
		Driver:    common.ContainerDriver_CRI,
		Namespace: common.ContainerdNamespace_NS_SYSTEM,
	}

	pullErr := pullInstallerImage(ctx, client, publicIP, containerdInst, image)
	if pullErr != nil {
		return "", fmt.Errorf("failed to pull installer image %s on %s: %w", image, publicIP, pullErr)
	}

	upgradeErr := runLifecycleUpgrade(ctx, client, publicIP, containerdInst, image)
	if upgradeErr != nil {
		return "", fmt.Errorf("failed to upgrade machine %s to %s: %w", publicIP, image, upgradeErr)
	}

	bootID, bootErr := readBootID(ctx, client, publicIP)
	if bootErr != nil && !errors.Is(bootErr, errBootIDUnauthorized) {
		// Non-permission read failure: proceed with an empty boot-id (the
		// InFlight poll falls back to reachability + version-change). Install
		// already succeeded; never re-run it on a boot-id capture failure.
		bootID = ""
	}

	// Reboot is best-effort: install already succeeded (exit 0). A reboot
	// failure proceeds to InFlight anyway; the host may reboot on its own or
	// an operator nudges it, and the InFlight poll detects the new version.
	rebootErr := rebootMachine(ctx, client, publicIP)
	if rebootErr != nil {
		log.FromContext(ctx).Info("Talos upgrade install succeeded but reboot RPC failed; proceeding to InFlight",
			"byotMachine", "", "publicIP", publicIP, "err", rebootErr)
	}

	return bootID, nil
}

// pullInstallerImage pulls the installer image into the CRI containerd store
// via the streaming ImageClient.Pull RPC, draining progress messages until
// the stream completes (the server sends a terminal Name message then EOF).
func pullInstallerImage(
	ctx context.Context,
	client *talosclient.Client,
	publicIP string,
	inst *common.ContainerdInstance,
	image string,
) error {
	pullCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), imagePullTimeout)
	defer cancel()

	stream, err := client.ImageClient.Pull(pullCtx, &machineapi.ImageServicePullRequest{
		Containerd: inst,
		ImageRef:   image,
	})
	if err != nil {
		return err
	}

	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return nil
			}

			return recvErr
		}

		// Progress messages are informational; the terminal signal is EOF
		// after the server sends a Name (pulled-image) message.
		_ = msg.GetPullProgress()
	}
}

// runLifecycleUpgrade issues LifecycleClient.Upgrade and drains the stream
// to the terminal exit-code message. The server runs /bin/installer install
// from the pre-pulled image; force=false (no same-version overwrite guard at
// this layer), preserve is implicit (the live machine config is carried).
// A nonzero exit code is surfaced as *errUpgradeExitCode.
func runLifecycleUpgrade(
	ctx context.Context,
	client *talosclient.Client,
	publicIP string,
	inst *common.ContainerdInstance,
	image string,
) error {
	upgradeCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), upgradeTimeout)
	defer cancel()

	stream, err := client.LifecycleClient.Upgrade(upgradeCtx, &machineapi.LifecycleServiceUpgradeRequest{
		Containerd: inst,
		Source: &machineapi.InstallArtifactsSource{
			ImageName: image,
		},
	})
	if err != nil {
		return err
	}

	var lastMsg string

	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return &upgradeExitCodeError{Code: -1, Msg: lastMsg}
			}

			return recvErr
		}

		prog := msg.GetProgress()
		if prog == nil {
			continue
		}

		switch resp := prog.GetResponse().(type) {
		case *machineapi.LifecycleServiceInstallProgress_Message:
			lastMsg = resp.Message
		case *machineapi.LifecycleServiceInstallProgress_ExitCode:
			if resp.ExitCode == 0 {
				return nil
			}

			return &upgradeExitCodeError{Code: resp.ExitCode, Msg: lastMsg}
		}
	}
}

// readBootID reads /proc/sys/kernel/random/boot_id from the host via the
// Read RPC. Returns errBootIDUnauthorized when the client credentials lack
// read permission, so the caller can distinguish a hard failure from a
// degraded-but-proceedable one.
func readBootID(ctx context.Context, client *talosclient.Client, publicIP string) (string, error) {
	readCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	reader, err := client.Read(readCtx, "/proc/sys/kernel/random/boot_id")
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			return "", errBootIDUnauthorized
		}

		return "", err
	}

	defer reader.Close() //nolint:errcheck

	body, err := io.ReadAll(reader)
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			return "", errBootIDUnauthorized
		}

		return "", err
	}

	return strings.TrimSpace(string(body)), reader.Close()
}

// rebootMachine issues a Reboot RPC (unary). It returns immediately; the
// machine reboots after. Default reboot mode. Best-effort in the upgrade
// sequence — install already succeeded by the time this is called.
func rebootMachine(ctx context.Context, client *talosclient.Client, publicIP string) error {
	rebootCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), rebootTimeout)
	defer cancel()

	return client.Reboot(rebootCtx)
}

// bootIDAuthenticated returns the live boot-id of the machine at publicIP,
// queried over the cluster talosconfig (mTLS). Used by the InFlight upgrade
// poll to detect a reboot-and-back (completion gate). Returns
// errBootIDUnauthorized when the credentials cannot read boot-id; the caller
// falls back to a reachability + version-change completion check.
func bootIDAuthenticated(ctx context.Context, publicIP string, talosConfig []byte) (string, error) {
	client, err := authenticatedClient(ctx, publicIP, talosConfig)
	if err != nil {
		return "", err
	}

	defer client.Close() //nolint:errcheck

	return readBootID(ctx, client, publicIP)
}

// probeMaintenance reports whether the machine at publicIP answers the Talos
// machine API in maintenance mode (unverified TLS, no client authentication).
func probeMaintenance(ctx context.Context, publicIP string) bool {
	client, err := maintenanceClient(ctx, publicIP)
	if err != nil {
		return false
	}

	defer client.Close() //nolint:errcheck

	probeCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	_, err = client.Version(probeCtx)

	return err == nil
}

// probeAuthenticated reports whether the machine at publicIP answers the
// Talos machine API using the credentials from the given talosconfig. Used by
// the join preflight to determine which PKI bundle a configured machine
// carries.
func probeAuthenticated(ctx context.Context, publicIP string, talosConfig []byte) bool {
	client, err := authenticatedClient(ctx, publicIP, talosConfig)
	if err != nil {
		return false
	}

	defer client.Close() //nolint:errcheck

	probeCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	_, err = client.Version(probeCtx)

	return err == nil
}

// restartService restarts the given Talos service on the machine, using the
// given talosconfig credentials. Used after bundle-match re-adoption: when a
// node was split with splitPolicy=None, Cluster API deletes its Node object
// in the workload cluster, and a restarted kubelet is what re-registers it.
func restartService(ctx context.Context, publicIP string, talosConfig []byte, serviceID string) error {
	client, err := authenticatedClient(ctx, publicIP, talosConfig)
	if err != nil {
		return err
	}

	defer client.Close() //nolint:errcheck

	restartCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), resetTimeout)
	defer cancel()

	_, err = client.ServiceRestart(restartCtx, serviceID)
	if err != nil {
		return fmt.Errorf("failed to restart service %s on %s: %w", serviceID, publicIP, err)
	}

	return nil
}

// serviceRunning reports whether the given Talos service is currently in the
// Running state on the machine.
func serviceRunning(ctx context.Context, publicIP string, talosConfig []byte, serviceID string) (bool, error) {
	client, err := authenticatedClient(ctx, publicIP, talosConfig)
	if err != nil {
		return false, err
	}

	defer client.Close() //nolint:errcheck

	probeCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	services, err := client.ServiceInfo(probeCtx, serviceID)
	if err != nil {
		return false, fmt.Errorf("failed to inspect service %s on %s: %w", serviceID, publicIP, err)
	}

	for _, svc := range services {
		if svc.Service != nil && svc.Service.GetState() == talosServiceRunning {
			return true, nil
		}
	}

	return false, nil
}

// resetMachine wipes the machine's STATE and EPHEMERAL system volumes and
// reboots it into maintenance mode. When talosConfig is nil, an unverified
// TLS client is used (maintenance mode); otherwise the machine's current
// configuration credentials authenticate the request.
func resetMachine(ctx context.Context, publicIP string, talosConfig []byte) error {
	var (
		client *talosclient.Client
		err    error
	)

	if talosConfig != nil {
		client, err = authenticatedClient(ctx, publicIP, talosConfig)
	} else {
		client, err = maintenanceClient(ctx, publicIP)
	}

	if err != nil {
		return err
	}

	defer client.Close() //nolint:errcheck

	resetCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), resetTimeout)
	defer cancel()

	err = client.ResetGeneric(resetCtx, &machineapi.ResetRequest{
		Graceful: false,
		Reboot:   true,
		SystemPartitionsToWipe: []*machineapi.ResetPartitionSpec{
			{Label: talosLabelState, Wipe: true},
			{Label: talosLabelEphemeral, Wipe: true},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to reset machine %s: %w", publicIP, err)
	}

	return nil
}

// detectSystemDisk queries the Talos storage API for the host's system disk
// and returns its device path (e.g. /dev/sda). The Talos lifecycle Upgrade RPC
// passes the system disk to the installer via --disk, but the installer also
// validates the machine config piped on stdin, which requires machine.install
// .disk or diskSelector. cabpt does not set one, so BYOT detects it here and
// injects it into the config before apply.
func detectSystemDisk(ctx context.Context, publicIP string, talosConfig []byte) (string, error) {
	var (
		client *talosclient.Client
		err    error
	)

	if talosConfig != nil {
		client, err = authenticatedClient(ctx, publicIP, talosConfig)
	} else {
		client, err = maintenanceClient(ctx, publicIP)
	}

	if err != nil {
		return "", err
	}

	defer client.Close() //nolint:errcheck

	diskCtx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), probeTimeout)
	defer cancel()

	resp, err := client.Disks(diskCtx)
	if err != nil {
		return "", fmt.Errorf("failed to query disks on %s: %w", publicIP, err)
	}

	for _, msg := range resp.GetMessages() {
		for _, disk := range msg.GetDisks() {
			if disk.GetSystemDisk() {
				return "/dev/" + disk.GetDeviceName(), nil
			}
		}
	}

	return "", fmt.Errorf("%w on %s", errNoSystemDisk, publicIP)
}

// injectInstallDisk sets machine.install.disk in the v1alpha1 machine-config
// document, preserving every other document in the stream (e.g. the CABPT
// HostnameConfig doc). yaml.v3 Unmarshal/Marshal only round-trip the first
// document of a multi-doc stream; a single-doc round-trip silently drops the
// HostnameConfig doc, leaving the host on its SMBIOS hostname.
func injectInstallDisk(machineConfig []byte, disk string) ([]byte, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(machineConfig))

	var docs []map[string]any
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("failed to parse machine config: %w", err)
		}

		docs = append(docs, doc)
	}

	for _, doc := range docs {
		machine, _ := doc["machine"].(map[string]any)
		if machine == nil {
			continue
		}

		install, _ := machine["install"].(map[string]any)
		if install == nil {
			install = map[string]any{}
		}

		// Only inject when unset; an operator-provided disk/diskSelector wins.
		if _, ok := install["disk"]; !ok {
			if _, ok := install["diskSelector"]; !ok {
				install["disk"] = disk
			}
		}

		machine["install"] = install
		doc["machine"] = machine
	}

	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("failed to serialize machine config: %w", err)
		}
	}

	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to flush machine config: %w", err)
	}

	return buf.Bytes(), nil
}

// yamlUnmarshal decodes bytes into the target using gopkg.in/yaml.v3, which
// preserves scalar types (int, bool) that a JSON round-trip would corrupt.
func yamlUnmarshal(b []byte, out any) error {
	return yamlv3.Unmarshal(b, out)
}

// yamlMarshal encodes the value to YAML.
func yamlMarshal(in any) ([]byte, error) {
	return yamlv3.Marshal(in)
}
