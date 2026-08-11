package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

const (
	// talosAPIPort is the secure Talos machine API port. In maintenance mode
	// it accepts ApplyConfiguration with unverified TLS.
	talosAPIPort = "50000"

	// applyTimeout bounds a single ApplyConfiguration attempt.
	applyTimeout = 2 * time.Minute

	// resetTimeout bounds a single Reset attempt.
	resetTimeout = 30 * time.Second

	// probeTimeout bounds a maintenance-mode probe attempt.
	probeTimeout = 10 * time.Second

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
