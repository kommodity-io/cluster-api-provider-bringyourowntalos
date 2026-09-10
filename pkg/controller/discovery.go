package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	storageapi "github.com/siderolabs/talos/pkg/machinery/api/storage"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// discoveryLabelPrefix is the controller-managed label prefix. The controller
// owns all byot.io/ labels and never overwrites operator (non-byot.io/) labels.
const discoveryLabelPrefix = "byot.io/"

// Controller-managed discovery labels (low-cardinality, selection-oriented).
const (
	labelAvailable     = discoveryLabelPrefix + "available"
	labelCPUCores      = discoveryLabelPrefix + "cpu-cores"
	labelCPUArch       = discoveryLabelPrefix + "cpu-arch"
	labelMemory        = discoveryLabelPrefix + "memory"
	labelDiskType      = discoveryLabelPrefix + "disk-type"
	labelDiskSize      = discoveryLabelPrefix + "disk-size"
	labelGPUCount      = discoveryLabelPrefix + "gpu-count"
	labelGPUVendor     = discoveryLabelPrefix + "gpu-vendor"
	labelGPUModel      = discoveryLabelPrefix + "gpu-model"
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
	GPUs              *infrav1.HostGPU
	Identity          *infrav1.HostIdentity
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

	err = discoverVersion(ctx, client, &result)
	if err != nil {
		return result, fmt.Errorf("version discovery: %w", err)
	}

	err = discoverMemory(ctx, client, &result)
	if err != nil {
		return result, fmt.Errorf("memory discovery: %w", err)
	}

	err = discoverDisks(ctx, client, &result)
	if err != nil {
		return result, fmt.Errorf("disk discovery: %w", err)
	}

	dmesg, err := readDmesg(ctx, client)
	if err != nil {
		return result, fmt.Errorf("dmesg discovery: %w", err)
	}

	result.CPU = parseCPU(dmesg)
	result.Platform = parsePlatform(dmesg)
	result.GPUs = parseGPUs(dmesg)

	err = discoverNetInterfaces(ctx, client, &result)
	if err != nil {
		return result, fmt.Errorf("net discovery: %w", err)
	}

	// Identity is best-effort: a host without SMBIOS UUID and without a
	// physical NIC still completes discovery, just with an empty Identity.
	// Identity fields that fail to fetch individually are left empty rather
	// than aborting the whole discovery.
	discoverIdentity(ctx, client.COSI, &result)

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
	// pciDeviceRegexp extracts the PCI address, vendor, device, and class
	// from a kernel dmesg enumeration line, e.g.
	//   "pci 0000:01:00.0: [10de:2331] type 00 class 0x030200 PCIe Endpoint".
	pciDeviceRegexp = regexp.MustCompile(
		`pci ([0-9a-f:\.]+): \[([0-9a-f]{4}):([0-9a-f]{4})\] type \d+ class 0x([0-9a-f]{6})`)
)

// gpuVendorNames maps a PCI vendor id (lowercase hex) to the lowercase label
// value used for byot.io/gpu-vendor.
//
//nolint:gochecknoglobals // static vendor table
var gpuVendorNames = map[string]string{
	"10de": "nvidia",
	"1002": "amd",
	"8086": "intel",
}

// pciDisplayClassBase is the PCI base class for display controllers (0x03).
const pciDisplayClassBase = 0x03

// gpuModelTable maps vendor:device (lowercase hex) to the normalized model
// family and per-GPU HBM in bytes. Derived from the pci.ids database
// (https://pci-ids.ucw.cz). Only datacenter GPUs (Hopper, Blackwell, Ampere
// compute) are listed; workstation/consumer SKUs are excluded. HBM is taken
// from the product name where present, otherwise from vendor specs.
//
//nolint:gochecknoglobals // static device table
var gpuModelTable = map[string]struct {
	model    string
	hbmBytes int64
}{
	// Hopper (GH100).
	"10de:2330": {"h100-sxm5", 80 << 30},  // H100 SXM5 80GB
	"10de:2331": {"h100-pcie", 80 << 30},   // H100 PCIe 80GB
	"10de:2336": {"h100", 80 << 30},        // H100
	"10de:2337": {"h100-sxm5-64g", 64 << 30},
	"10de:2338": {"h100-sxm5-96g", 96 << 30},
	"10de:2339": {"h100-sxm5-94g", 94 << 30},
	"10de:233a": {"h800l-94g", 94 << 30},
	"10de:233b": {"h200-nvl", 141 << 30},   // H200 NVL 141GB
	"10de:233d": {"h100-96g", 96 << 30},
	"10de:2321": {"h100l-94g", 94 << 30},
	"10de:2322": {"h800-pcie", 80 << 30},   // H800 PCIe 80GB
	"10de:2324": {"h800", 80 << 30},         // H800 80GB
	"10de:2342": {"gh200-120g", 96 << 30},   // GH200 120GB (96GB HBM3e)
	"10de:2348": {"gh200-144g", 144 << 30}, // GH200 144G HBM3e
	// Blackwell (GB100/GB110).
	"10de:2901": {"b200", 192 << 30},        // B200 192GB
	"10de:2909": {"hgx-b200-168g", 168 << 30}, // HGX B200 168GB
	"10de:2941": {"hgx-gb200", 192 << 30},   // HGX GB200 192GB
	"10de:3182": {"b300-sxm6", 288 << 30},   // B300 SXM6 288GB
	"10de:31a1": {"gb300-maxq", 288 << 30},
	"10de:31c2": {"gb300", 288 << 30},
	"10de:31c3": {"gb300", 288 << 30},
	// Ampere (GA100) compute.
	"10de:20b0": {"a100-sxm4-40g", 40 << 30},
	"10de:20b1": {"a100-pcie-40g", 40 << 30},
	"10de:20b2": {"a100-sxm4-80g", 80 << 30},
	"10de:20b3": {"a100-sxm-64g", 64 << 30},
	"10de:20b5": {"a100-pcie-80g", 80 << 30},
	"10de:20b7": {"a30-pcie", 24 << 30}, // A30 24GB
	"10de:20bd": {"a800-sxm4-40g", 40 << 30},
	"10de:20f3": {"a800-sxm4-80g", 80 << 30},
	"10de:20f5": {"a800-pcie-80g", 80 << 30},
	"10de:20f6": {"a800-pcie-40g", 40 << 30},
	// AMD Instinct (CDNA) datacenter GPUs.
	// Vega 10 (gfx900): MI25 16GB HBM2.
	"1002:6860": {"mi25", 16 << 30},
	// Vega 20 (gfx906): MI50 16GB; MI60 32GB share the same device id.
	"1002:66a1": {"mi50", 16 << 30},
	// Arcturus (gfx908): MI100 32GB HBM2.
	"1002:738c": {"mi100", 32 << 30},
	"1002:738e": {"mi100", 32 << 30},
	// Aldebaran (gfx90a): MI210 64GB, MI250/MI250X 128GB HBM3.
	"1002:740f": {"mi210", 64 << 30},
	"1002:740c": {"mi250", 128 << 30},
	"1002:7408": {"mi250x", 128 << 30},
	// Aqua Vanjaram (gfx942): MI300 series HBM3/3e.
	"1002:74a0": {"mi300a", 128 << 30},
	"1002:74a1": {"mi300x", 192 << 30},
	"1002:74a2": {"mi308x", 128 << 30},
	"1002:74a5": {"mi325x", 288 << 30},
	"1002:75a0": {"mi350x", 288 << 30},
	"1002:75a3": {"mi355x", 288 << 30},
}

// parseGPUs scans the kernel dmesg log for PCI display-class devices from a
// known GPU vendor and returns an aggregated HostGPU summary. It is
// best-effort: a parse anomaly is logged by the caller and skipped, and a
// host with no GPU returns nil. Mixed (vendor,device) pairs collapse to a
// count+vendor summary with Mixed=true and no model.
func parseGPUs(dmesg string) *infrav1.HostGPU {
	counts := countGPUPairs(dmesg)

	if len(counts) == 0 {
		return nil
	}

	gpu := &infrav1.HostGPU{}

	for _, pairCount := range counts {
		gpu.Count += pairCount
	}

	if len(counts) == 1 {
		populateHomogeneousGPU(gpu, counts)

		return gpu
	}

	populateMixedGPU(gpu, counts)

	return gpu
}

// pairKey identifies a (vendor, device) PCI pair.
type pairKey struct {
	vendor, device string
}

// countGPUPairs tallies display-class PCI devices from known GPU vendors in
// the dmesg log, keyed by (vendor, device).
func countGPUPairs(dmesg string) map[pairKey]int32 {
	counts := map[pairKey]int32{}

	for _, match := range pciDeviceRegexp.FindAllStringSubmatch(dmesg, -1) {
		vendor := match[2]
		device := match[3]

		class, err := strconv.ParseUint(match[4], 16, 32)
		if err != nil {
			continue
		}

		// Display class is 0x03xx; the base class is the high byte.
		if class>>16 != pciDisplayClassBase {
			continue
		}

		if _, ok := gpuVendorNames[vendor]; !ok {
			continue
		}

		counts[pairKey{vendor, device}]++
	}

	return counts
}

// populateHomogeneousGPU fills a single-pair summary: vendor, model, device
// id, and per-GPU/total HBM from the model table (empty/zero when unknown).
func populateHomogeneousGPU(gpu *infrav1.HostGPU, counts map[pairKey]int32) {
	for key, pairCount := range counts {
		gpu.Vendor = gpuVendorNames[key.vendor]
		gpu.DeviceID = key.device

		entry, ok := gpuModelTable[key.vendor+":"+key.device]
		if !ok {
			continue
		}

		gpu.Model = entry.model
		gpu.MemoryPerGPU = *resource.NewQuantity(entry.hbmBytes, resource.BinarySI)
		total := int64(pairCount) * entry.hbmBytes
		gpu.TotalMemory = *resource.NewQuantity(total, resource.BinarySI)
	}
}

// populateMixedGPU fills a multi-pair summary: count only, plus vendor when
// all GPUs share one vendor. Model and device id are left empty.
func populateMixedGPU(gpu *infrav1.HostGPU, counts map[pairKey]int32) {
	vendors := map[string]struct{}{}

	for key := range counts {
		vendors[key.vendor] = struct{}{}
	}

	if len(vendors) == 1 {
		for vendor := range vendors {
			gpu.Vendor = gpuVendorNames[vendor]
		}
	}

	gpu.Mixed = true
}

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
		v, err := strconv.Atoi(m[1])
		if err == nil && v > 0 {
			cpu.Cores = int32(v)
		}
	}

	if m := cpuPackagesRegexp.FindStringSubmatch(dmesg); m != nil {
		v, err := strconv.Atoi(m[1])
		if err == nil && v > 0 {
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
		n, err := strconv.Atoi(m[1])
		if err == nil {
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

// discoverIdentity fetches the reboot-stable hardware identity of the host:
// the SMBIOS System Information UUID (and the supporting serial/manufacturer/
// product fields) and the first-up NIC hardware address. It is best-effort:
// each field is fetched independently and a missing/empty value is recorded as
// an empty string rather than failing discovery, so a host with no SMBIOS UUID
// or a virtual NIC still completes discovery with a partial identity. The
// caller treats an all-empty Identity as "no identity available".
func discoverIdentity(
	ctx context.Context,
	cosi state.CoreState,
	result *DiscoveryResult,
) {
	identity := &infrav1.HostIdentity{}

	populateSystemIdentity(ctx, cosi, identity)
	populateHardwareAddr(ctx, cosi, identity)

	// Only store a non-empty identity; an all-zero Identity is meaningless and
	// keeps the status field nil to signal "identity unavailable".
	if identity.SystemUUID != "" || identity.HardwareAddr != "" ||
		identity.SerialNumber != "" || identity.Manufacturer != "" ||
		identity.ProductName != "" {
		result.Identity = identity
	}
}

// populateSystemIdentity fills the SMBIOS SystemInformation fields (UUID, serial,
// manufacturer, product) from the COSI resource, logging fetch errors.
func populateSystemIdentity(ctx context.Context, cosi state.CoreState, identity *infrav1.HostIdentity) {
	sysInfo, err := safe.StateGetByID[*hardware.SystemInformation](ctx, cosi, hardware.SystemInformationID)
	if err != nil {
		log.FromContext(ctx).Error(err, "fetching SMBIOS SystemInformation from COSI")
	} else if sysInfo != nil {
		spec := sysInfo.TypedSpec()
		identity.SystemUUID = spec.UUID
		identity.SerialNumber = spec.SerialNumber
		identity.Manufacturer = spec.Manufacturer
		identity.ProductName = spec.ProductName
	}
}

// populateHardwareAddr fills the first-up NIC hardware address, logging fetch
// errors. All-zero MACs are skipped: some virtual NICs report a 6-byte zero MAC
// that stringifies to "00:00:00:00:00:00" (non-empty) and would false-positive a
// future MAC-equality gate. IsZero() only checks length, not contents.
func populateHardwareAddr(ctx context.Context, cosi state.CoreState, identity *infrav1.HostIdentity) {
	hwAddr, err := safe.StateGetByID[*network.HardwareAddr](ctx, cosi, network.FirstHardwareAddr)
	if err != nil {
		log.FromContext(ctx).Error(err, "fetching first-up NIC HardwareAddr from COSI")
	} else if hwAddr != nil {
		mac := hwAddr.TypedSpec().HardwareAddr
		// Skip all-zero MACs: some virtual NICs report a 6-byte zero MAC that
		// stringifies to "00:00:00:00:00:00" (non-empty) and would false-positive
		// a future MAC-equality gate. IsZero() only checks length, not contents.
		if len(mac) > 0 && !bytes.Equal(mac, make(nethelpers.HardwareAddr, len(mac))) {
			identity.HardwareAddr = mac.String()
		}
	}
}

// roundQuantity rounds q to the nearest binary unit (GiB, or TiB once the
// value reaches 1024 GiB) and formats it with a G/T suffix for selection
// labels. Unlike fixed buckets, every distinct capacity gets its own label.
func roundQuantity(q resource.Quantity) string {
	const (
		gib int64 = 1 << 30
		tib int64 = 1 << 40
	)

	value := q.Value()

	// Round to the nearest GiB.
	gibCount := (value + gib/2) / gib

	if gibCount >= tib/gib {
		// Round to the nearest TiB.
		tibCount := (gibCount + (tib/gib)/2) / (tib / gib)
		return strconv.FormatInt(tibCount, 10) + "T"
	}

	return strconv.FormatInt(gibCount, 10) + "G"
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

// promoteHardwareLabels lifts the curated, rounded hardware subset to labels.
func promoteHardwareLabels(labels map[string]string, hardware *infrav1.HostHardware) {
	if hardware.CPU.Cores > 0 {
		labels[labelCPUCores] = strconv.FormatInt(int64(hardware.CPU.Cores), 10)
	}

	if !hardware.Memory.IsZero() {
		labels[labelMemory] = roundQuantity(hardware.Memory)
	}

	if disk := systemDisk(hardware.Disks); disk != nil {
		if t := diskTypeLabel(disk.Type); t != "" {
			labels[labelDiskType] = t
		}

		if !disk.Size.IsZero() {
			labels[labelDiskSize] = roundQuantity(disk.Size)
		}
	}

	if hardware.GPUs != nil {
		promoteGPULabels(labels, hardware.GPUs)
	}
}

// promoteGPULabels lifts the GPU count, vendor, and model to labels. Model is
// omitted for unknown devices and mixed hosts; vendor is omitted for
// mixed-vendor hosts. Count is always set when GPUs are present.
func promoteGPULabels(labels map[string]string, gpus *infrav1.HostGPU) {
	if gpus.Count > 0 {
		labels[labelGPUCount] = strconv.FormatInt(int64(gpus.Count), 10)
	}

	if gpus.Vendor != "" {
		labels[labelGPUVendor] = gpus.Vendor
	}

	if gpus.Model != "" {
		labels[labelGPUModel] = gpus.Model
	}
}
