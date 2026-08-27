package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	storageapi "github.com/siderolabs/talos/pkg/machinery/api/storage"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"k8s.io/apimachinery/pkg/api/resource"
)

// discoveryLabelPrefix is the controller-managed label prefix. The controller
// owns all byot.io/ labels and never overwrites operator (non-byot.io/) labels.
const discoveryLabelPrefix = "byot.io/"

// Controller-managed discovery labels (low-cardinality, selection-oriented).
const (
	labelAvailable     = discoveryLabelPrefix + "available"
	labelCPUCores      = discoveryLabelPrefix + "cpu-cores"
	labelCPUArch       = discoveryLabelPrefix + "cpu-arch"
	labelMemoryClass   = discoveryLabelPrefix + "memory-class"
	labelDiskType      = discoveryLabelPrefix + "disk-type"
	labelDiskClass     = discoveryLabelPrefix + "disk-class"
	labelPlatform      = discoveryLabelPrefix + "platform"
	labelTalosVersion  = discoveryLabelPrefix + "talos-version"
	labelFailureDomain = discoveryLabelPrefix + "failure-domain"
)

// DiscoveryResult is the controller-side discovery payload, copied into
// ByotHost status and labels by the reconciler.
type DiscoveryResult struct {
	TalosVersion      string
	Arch              string
	Platform          string
	CPU               infrav1.HostCPU
	Memory            resource.Quantity
	Disks             []infrav1.HostDisk
	NetworkInterfaces []string
}

// discoverHost runs the maintenance-mode discovery surface against publicIP:
// Version, Memory, Disks, Dmesg (CPU + platform), and LS /sys/class/net. It
// reuses the insecure maintenance client.
func discoverHost(ctx context.Context, publicIP string) (DiscoveryResult, error) {
	client, err := maintenanceClient(ctx, publicIP)
	if err != nil {
		return DiscoveryResult{}, err
	}

	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), applyTimeout)
	defer cancel()

	result := DiscoveryResult{}

	if err := discoverVersion(ctx, client, &result); err != nil {
		return result, fmt.Errorf("version discovery: %w", err)
	}

	if err := discoverMemory(ctx, client, &result); err != nil {
		return result, fmt.Errorf("memory discovery: %w", err)
	}

	if err := discoverDisks(ctx, client, &result); err != nil {
		return result, fmt.Errorf("disk discovery: %w", err)
	}

	dmesg, err := readDmesg(ctx, client)
	if err != nil {
		return result, fmt.Errorf("dmesg discovery: %w", err)
	}

	result.CPU = parseCPU(dmesg)
	result.Platform = parsePlatform(dmesg)

	if err := discoverNetInterfaces(ctx, client, &result); err != nil {
		return result, fmt.Errorf("net discovery: %w", err)
	}

	return result, nil
}

func discoverVersion(ctx context.Context, client *talosclient.Client, result *DiscoveryResult) error {
	resp, err := client.Version(ctx)
	if err != nil {
		return err
	}

	if len(resp.GetMessages()) > 0 {
		if info := resp.GetMessages()[0].GetVersion(); info != nil {
			result.TalosVersion = info.GetTag()
			result.Arch = info.GetArch()
		}
	}

	return nil
}

func discoverMemory(ctx context.Context, client *talosclient.Client, result *DiscoveryResult) error {
	resp, err := client.Memory(ctx)
	if err != nil {
		return err
	}

	if len(resp.GetMessages()) > 0 {
		if info := resp.GetMessages()[0].GetMeminfo(); info != nil && info.GetMemtotal() > 0 {
			result.Memory = resource.MustParse(fmt.Sprintf("%dKi", info.GetMemtotal()))
		}
	}

	return nil
}

func discoverDisks(ctx context.Context, client *talosclient.Client, result *DiscoveryResult) error {
	resp, err := client.Disks(ctx)
	if err != nil {
		return err
	}

	if len(resp.GetMessages()) > 0 {
		for _, disk := range resp.GetMessages()[0].GetDisks() {
			result.Disks = append(result.Disks, diskToHostDisk(disk))
		}
	}

	return nil
}

// diskToHostDisk maps a Talos Disk to the API HostDisk type.
//
//nolint:gosec // disk size is bounded hardware capacity
func diskToHostDisk(disk *storageapi.Disk) infrav1.HostDisk {
	return infrav1.HostDisk{
		Name:       diskDeviceName(disk),
		Size:       *resource.NewQuantity(int64(disk.GetSize()), resource.BinarySI),
		Type:       disk.GetType().String(),
		Model:      disk.GetModel(),
		SystemDisk: disk.GetSystemDisk(),
		BusPath:    disk.GetBusPath(),
	}
}

func diskDeviceName(disk *storageapi.Disk) string {
	if name := disk.GetDeviceName(); name != "" {
		return "/dev/" + name
	}

	return disk.GetName()
}

// readDmesg collects the kernel log buffer into a single string.
func readDmesg(ctx context.Context, client *talosclient.Client) (string, error) {
	stream, err := client.Dmesg(ctx, false, false)
	if err != nil {
		return "", err
	}

	var buf strings.Builder

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return "", err
		}

		if msg == nil {
			continue
		}

		buf.Write(msg.GetBytes())
		buf.WriteByte('\n')
	}

	return buf.String(), nil
}

var (
	// platformRegexp extracts the Talos platform from the kernel cmdline.
	platformRegexp = regexp.MustCompile(`talos\.platform=(\S+)`)
	// cpuCoresRegexp extracts nr_cpu_ids (total logical CPUs).
	cpuCoresRegexp = regexp.MustCompile(`nr_cpu_ids[=: ]+(\d+)`)
	// cpuPackagesRegexp extracts the number of CPU packages/sockets.
	cpuPackagesRegexp = regexp.MustCompile(`(?:packages|sockets)[=: ]+(\d+)`)
	// numaRegexp extracts NUMA node numbers.
	numaRegexp = regexp.MustCompile(`Node\s+(\d+)`)
)

// parseCPU extracts CPU topology from the kernel dmesg log. Missing fields
// default to 1 (a single-CPU, single-package, single-NUMA host).
//
//nolint:gosec // straightforward integer parsing of bounded hardware fields
func parseCPU(dmesg string) infrav1.HostCPU {
	cpu := infrav1.HostCPU{
		Cores:     1,
		Packages:  1,
		NumaNodes: 1,
	}

	if m := cpuCoresRegexp.FindStringSubmatch(dmesg); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			cpu.Cores = int32(v)
		}
	}

	if m := cpuPackagesRegexp.FindStringSubmatch(dmesg); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			cpu.Packages = int32(v)
		}
	}

	if nodes := distinctNumaNodes(dmesg); nodes > 0 {
		cpu.NumaNodes = int32(nodes)
	}

	return cpu
}

// distinctNumaNodes counts distinct NUMA node numbers in the dmesg log.
func distinctNumaNodes(dmesg string) int {
	seen := map[int]struct{}{}

	for _, m := range numaRegexp.FindAllStringSubmatch(dmesg, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			seen[n] = struct{}{}
		}
	}

	return len(seen)
}

// parsePlatform extracts the Talos platform from the kernel cmdline in dmesg.
func parsePlatform(dmesg string) string {
	if m := platformRegexp.FindStringSubmatch(dmesg); m != nil {
		return m[1]
	}

	return ""
}

func discoverNetInterfaces(
	ctx context.Context,
	client *talosclient.Client,
	result *DiscoveryResult,
) error {
	stream, err := client.LS(ctx, &machineapi.ListRequest{Root: "/sys/class/net"})
	if err != nil {
		return err
	}

	var ifaces []string

	for {
		info, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		if info == nil {
			continue
		}

		name := info.GetRelativeName()
		if name == "" {
			name = info.GetName()
		}

		// Skip the loopback interface; only external interfaces matter.
		if name == "" || name == "lo" {
			continue
		}

		ifaces = append(ifaces, name)
	}

	sort.Strings(ifaces)
	result.NetworkInterfaces = ifaces

	return nil
}

// memoryBuckets are the selection-oriented memory classes, ascending.
//
//nolint:gochecknoglobals // static bucket table
var memoryBuckets = []struct {
	label string
	bytes int64
}{
	{"4G", 4 << 30},
	{"8G", 8 << 30},
	{"16G", 16 << 30},
	{"32G", 32 << 30},
	{"64G", 64 << 30},
	{"128G", 128 << 30},
}

// diskBuckets are the selection-oriented disk-size classes, ascending.
//
//nolint:gochecknoglobals // static bucket table
var diskBuckets = []struct {
	label string
	bytes int64
}{
	{"20G", 20 << 30},
	{"100G", 100 << 30},
	{"250G", 250 << 30},
	{"500G", 500 << 30},
	{"1T", 1 << 40},
}

// bucketMemory returns the smallest memory bucket at least as large as q.
// Hosts smaller than the smallest bucket round up to it; hosts larger than the
// largest bucket clamp to it.
func bucketMemory(q resource.Quantity) string {
	value := q.Value()

	for _, b := range memoryBuckets {
		if value <= b.bytes {
			return b.label
		}
	}

	return memoryBuckets[len(memoryBuckets)-1].label
}

// bucketDisk returns the smallest disk bucket at least as large as q.
func bucketDisk(q resource.Quantity) string {
	value := q.Value()

	for _, b := range diskBuckets {
		if value <= b.bytes {
			return b.label
		}
	}

	return diskBuckets[len(diskBuckets)-1].label
}

// diskTypeLabel maps a Disk_DiskType enum name to a lowercase label value. It
// returns "" for unknown types so the label is omitted.
func diskTypeLabel(typeName string) string {
	switch strings.ToLower(typeName) {
	case "nvme", "ssd", "hdd", "sd":
		return strings.ToLower(typeName)
	default:
		return ""
	}
}

// systemDisk returns the system disk from a discovery result, or nil.
func systemDisk(disks []infrav1.HostDisk) *infrav1.HostDisk {
	for i := range disks {
		if disks[i].SystemDisk {
			return &disks[i]
		}
	}

	return nil
}

// applyDiscoveryLabels syncs the controller-managed byot.io/ labels on host
// with its current status and spec. Operator (non-byot.io/) labels are
// preserved. It is called whenever discovery or phase changes so labels stay
// in sync with status (the source of truth).
func applyDiscoveryLabels(host *infrav1.ByotHost) {
	labels := make(map[string]string, len(host.Labels))

	for k, v := range host.Labels {
		if !strings.HasPrefix(k, discoveryLabelPrefix) {
			labels[k] = v // preserve operator labels
		}
	}

	if host.Status.Phase == infrav1.HostPhaseAvailable {
		labels[labelAvailable] = "true"
	}

	if host.Status.Arch != "" {
		labels[labelCPUArch] = host.Status.Arch
	}

	if host.Status.TalosVersion != "" {
		labels[labelTalosVersion] = host.Status.TalosVersion
	}

	if host.Status.Platform != "" {
		labels[labelPlatform] = host.Status.Platform
	}

	if hw := host.Status.Hardware; hw != nil {
		promoteHardwareLabels(labels, hw)
	}

	if host.Spec.FailureDomain != nil && *host.Spec.FailureDomain != "" {
		labels[labelFailureDomain] = *host.Spec.FailureDomain
	}

	host.Labels = labels
}

// promoteHardwareLabels lifts the curated, bucketed hardware subset to labels.
func promoteHardwareLabels(labels map[string]string, hardware *infrav1.HostHardware) {
	if hardware.CPU.Cores > 0 {
		labels[labelCPUCores] = strconv.FormatInt(int64(hardware.CPU.Cores), 10)
	}

	if !hardware.Memory.IsZero() {
		labels[labelMemoryClass] = bucketMemory(hardware.Memory)
	}

	if disk := systemDisk(hardware.Disks); disk != nil {
		if t := diskTypeLabel(disk.Type); t != "" {
			labels[labelDiskType] = t
		}

		if !disk.Size.IsZero() {
			labels[labelDiskClass] = bucketDisk(disk.Size)
		}
	}
}
