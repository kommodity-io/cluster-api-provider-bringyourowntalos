package controller

import (
	"strings"
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/cluster-api/util/conditions"
	"strconv"
)

func TestParseCPUDefaultsToSingleCPU(t *testing.T) {
	t.Parallel()

	cpu := parseCPU("")
	assert.Equal(t, infrav1.HostCPU{Cores: 1, Packages: 1, NumaNodes: 1}, cpu)
}

func TestParseCPUCoresFromNrCpuIds(t *testing.T) {
	t.Parallel()

	cpu := parseCPU("Linux version ... [ 0.000000] smpboot: nr_cpu_ids: 16")
	assert.Equal(t, int32(16), cpu.Cores)
}

func TestParseCPUIgnoresInvalidCores(t *testing.T) {
	t.Parallel()

	cpu := parseCPU("nr_cpu_ids: 0")
	assert.Equal(t, int32(1), cpu.Cores)
}

func TestParseCPUPackages(t *testing.T) {
	t.Parallel()

	cpu := parseCPU("sockets: 2")
	assert.Equal(t, int32(2), cpu.Packages)
}

func TestParseCPUNumaNodes(t *testing.T) {
	t.Parallel()

	cpu := parseCPU("Node 0 Node 1 Node 2")
	assert.Equal(t, int32(3), cpu.NumaNodes)
}

func TestParseCPUIgnoresTscLine(t *testing.T) {
	t.Parallel()

	// TSC frequency is not captured (field removed); the line must not
	// disturb the default single-CPU topology.
	cpu := parseCPU("tsc: Detected 1996.250 MHz host clock")
	assert.Equal(t, infrav1.HostCPU{Cores: 1, Packages: 1, NumaNodes: 1}, cpu)
}

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "scaleway", parsePlatform("Command line: talos.platform=scaleway console=ttyS0"))
	assert.Empty(t, parsePlatform("no cmdline here"))
}

func TestBucketMemory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ki     int64
		bucket string
	}{
		{3 * 1024 * 1024, "4G"},      // ~3Gi -> 4G
		{7 * 1024 * 1024, "8G"},      // ~7Gi -> 8G
		{15 * 1024 * 1024, "16G"},    // ~15Gi -> 16G
		{30 * 1024 * 1024, "32G"},    // ~30Gi -> 32G
		{60 * 1024 * 1024, "64G"},    // ~60Gi -> 64G
		{120 * 1024 * 1024, "128G"},  // ~120Gi -> 128G
		{200 * 1024 * 1024, "256G"},  // ~200Gi -> 256G
		{400 * 1024 * 1024, "512G"},  // ~400Gi -> 512G
		{800 * 1024 * 1024, "1T"},    // ~800Gi -> 1T
		{1900 * 1024 * 1024, "2T"},   // ~1900Gi -> 2T
		{4000 * 1024 * 1024, "2T"},   // oversized -> clamp
	}

	for _, c := range cases {
		q := resource.MustParse(strconv.FormatInt(c.ki, 10) + "Ki")
		assert.Equal(t, c.bucket, bucketMemory(q))
	}
}

func TestBucketDisk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		gi     int64
		bucket string
	}{
		{15, "20G"},
		{80, "100G"},
		{200, "250G"},
		{400, "500G"},
		{900, "1T"},
		{5000, "1T"}, // clamp
	}

	for _, c := range cases {
		q := resource.MustParse(strconv.FormatInt(c.gi, 10) + "Gi")
		assert.Equal(t, c.bucket, bucketDisk(q))
	}
}

func TestDiskTypeLabel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "nvme", diskTypeLabel("NVME"))
	assert.Equal(t, "ssd", diskTypeLabel("SSD"))
	assert.Equal(t, "hdd", diskTypeLabel("HDD"))
	assert.Equal(t, "sd", diskTypeLabel("SD"))
	assert.Empty(t, diskTypeLabel("UNKNOWN"))
	assert.Empty(t, diskTypeLabel(""))
}

func TestSystemDisk(t *testing.T) {
	t.Parallel()

	disks := []infrav1.HostDisk{
		{Name: "/dev/sda", SystemDisk: false},
		{Name: "/dev/sdb", SystemDisk: true},
	}

	disk := systemDisk(disks)
	require.NotNil(t, disk)
	assert.Equal(t, "/dev/sdb", disk.Name)
}

func TestSystemDiskNilWhenNone(t *testing.T) {
	t.Parallel()

	disks := []infrav1.HostDisk{{Name: "/dev/sda", SystemDisk: false}}
	assert.Nil(t, systemDisk(disks))
}

func TestApplyDiscoveryLabelsPromotesCuratedLabels(t *testing.T) {
	t.Parallel()

	fd := "par01"
	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-1"},
		Spec: infrav1.ByotHostSpec{
			PublicIP:      "203.0.113.10",
			FailureDomain: &fd,
		},
		Status: infrav1.ByotHostStatus{
			Phase:       infrav1.HostPhaseAvailable,
			TalosVersion: "v1.13.8",
			Arch:        "amd64",
			Platform:    "scaleway",
			Hardware: &infrav1.HostHardware{
				CPU:    infrav1.HostCPU{Cores: 16},
				Memory: resource.MustParse("64Gi"),
				Disks: []infrav1.HostDisk{
					{Name: "/dev/sda", Size: resource.MustParse("250Gi"), Type: "SSD", SystemDisk: true},
				},
				NetworkInterfaces: []string{"eth0"},
			},
		},
	}

	// Operator labels preserved.
	host.Labels = map[string]string{"site": "gefion"}

	applyDiscoveryLabels(host)

	assert.Equal(t, "true", host.Labels[labelAvailable])
	assert.Equal(t, "16", host.Labels[labelCPUCores])
	assert.Equal(t, "amd64", host.Labels[labelCPUArch])
	assert.Equal(t, "64G", host.Labels[labelMemoryClass])
	assert.Equal(t, "ssd", host.Labels[labelDiskType])
	assert.Equal(t, "250G", host.Labels[labelDiskClass])
	assert.Equal(t, "scaleway", host.Labels[labelPlatform])
	assert.Equal(t, "v1.13.8", host.Labels[labelTalosVersion])
	assert.Equal(t, "par01", host.Labels[labelFailureDomain])
	assert.Equal(t, "gefion", host.Labels["site"]) // operator label preserved
}

func TestApplyDiscoveryLabelsDropsAvailableWhenNotAvailable(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-1"},
		Status:     infrav1.ByotHostStatus{Phase: infrav1.HostPhaseUnavailable},
	}
	host.Labels = map[string]string{labelAvailable: "true", "site": "gefion"}

	applyDiscoveryLabels(host)

	_, hasAvailable := host.Labels[labelAvailable]
	assert.False(t, hasAvailable)
	assert.Equal(t, "gefion", host.Labels["site"])
}

func TestApplyDiscoveryLabelsPreservesOperatorLabelsAcrossRediscovery(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-1"},
		Status:     infrav1.ByotHostStatus{Phase: infrav1.HostPhaseAvailable, Arch: "arm64"},
	}
	host.Labels = map[string]string{
		"site":            "gefion",
		labelCPUArch:      "amd64", // stale, should be overwritten
		labelAvailable:    "true",
	}

	applyDiscoveryLabels(host)

	assert.Equal(t, "arm64", host.Labels[labelCPUArch]) // controller label overwritten
	assert.Equal(t, "gefion", host.Labels["site"])     // operator label preserved
}

func TestDiscoverHostFailsOnUnreachable(t *testing.T) {
	t.Parallel()

	// 127.0.0.1 refuses the Talos API: discovery must fail fast.
	_, err := discoverHost(t.Context(), "127.0.0.1")
	require.Error(t, err)

	assert.True(t, strings.Contains(err.Error(), "version discovery") ||
		strings.Contains(err.Error(), "maintenance client"))
}
// h100DmesgLine is a real kernel dmesg line captured from a Scaleway H100-1-80G
// host in Talos maintenance mode (PLA-6633 probe). 10de:2331 = NVIDIA GH100
// (H100 PCIe 80G), class 0x030200 (headless 3D controller).
const h100DmesgLine = "pci 0000:01:00.0: [10de:2331] type 00 class 0x030200 PCIe Endpoint"

// b300DmesgLine is a synthetic line for the Blackwell B300. The format mirrors
// the verified H100 line; 10de:3182 = GB110 [B300 SXM6 AC] per the pci.ids
// database, but the device id has not been live-verified on a B300 node.
const b300DmesgLine = "pci 0000:41:00.0: [10de:3182] type 00 class 0x030200 PCIe Endpoint"

func TestParseGPUsH100(t *testing.T) {
	t.Parallel()

	gpu := parseGPUs(h100DmesgLine)
	require.NotNil(t, gpu)
	assert.Equal(t, int32(1), gpu.Count)
	assert.Equal(t, "nvidia", gpu.Vendor)
	assert.Equal(t, "h100-pcie", gpu.Model)
	assert.Equal(t, "2331", gpu.DeviceID)
	assert.False(t, gpu.Mixed)
	assert.Equal(t, "80Gi", gpu.MemoryPerGPU.String())
	assert.Equal(t, "80Gi", gpu.TotalMemory.String())
}

func TestParseGPUsMultiGPU(t *testing.T) {
	t.Parallel()

	// Eight identical B300 lines (one per GPU) collapse to a single summary.
	dmesg := strings.Repeat(b300DmesgLine+"\n", 8)
	gpu := parseGPUs(dmesg)
	require.NotNil(t, gpu)
	assert.Equal(t, int32(8), gpu.Count)
	assert.Equal(t, "nvidia", gpu.Vendor)
	assert.Equal(t, "b300-sxm6", gpu.Model)
	assert.Equal(t, "3182", gpu.DeviceID)
	assert.False(t, gpu.Mixed)
	assert.Equal(t, "288Gi", gpu.MemoryPerGPU.String())
	assert.Equal(t, "2304Gi", gpu.TotalMemory.String()) // 8 * 288Gi
}

func TestParseGPUsMixedModels(t *testing.T) {
	t.Parallel()

	dmesg := h100DmesgLine + "\n" + b300DmesgLine
	gpu := parseGPUs(dmesg)
	require.NotNil(t, gpu)
	assert.Equal(t, int32(2), gpu.Count)
	assert.Equal(t, "nvidia", gpu.Vendor) // same vendor
	assert.Empty(t, gpu.Model)
	assert.Empty(t, gpu.DeviceID)
	assert.True(t, gpu.Mixed)
	assert.True(t, gpu.MemoryPerGPU.IsZero())
	assert.True(t, gpu.TotalMemory.IsZero())
}

func TestParseGPUsMixedVendors(t *testing.T) {
	t.Parallel()

	amdLine := "pci 0000:03:00.0: [1002:740f] type 00 class 0x030000 PCIe Endpoint"
	dmesg := h100DmesgLine + "\n" + amdLine
	gpu := parseGPUs(dmesg)
	require.NotNil(t, gpu)
	assert.Equal(t, int32(2), gpu.Count)
	assert.Empty(t, gpu.Vendor) // mixed vendor -> omitted
	assert.Empty(t, gpu.Model)
	assert.True(t, gpu.Mixed)
}

func TestParseGPUsIgnoresNonDisplayNvidia(t *testing.T) {
	t.Parallel()

	// NVIDIA HDA audio controller on the same GPU function: vendor 10de but
	// class 0x0403 (audio), not display. Must not be counted as a GPU.
	hdaLine := "pci 0000:01:00.1: [10de:22f1] type 00 class 0x040300 PCIe Endpoint"
	gpu := parseGPUs(hdaLine)
	assert.Nil(t, gpu)
}

func TestParseGPUsNoGPU(t *testing.T) {
	t.Parallel()

	// dmesg with PCI lines but no display-class device from a known vendor.
	dmesg := "pci 0000:00:1f.2: [8086:2922] type 00 class 0x010601 conventional PCI endpoint"
	assert.Nil(t, parseGPUs(dmesg))
	assert.Nil(t, parseGPUs(""))
}

func TestParseGPUsUnknownNvidiaDevice(t *testing.T) {
	t.Parallel()

	// A display-class NVIDIA device not in the model table: vendor and count
	// are promoted, model and memory are empty, raw device id is recorded.
	unknownLine := "pci 0000:02:00.0: [10de:ffff] type 00 class 0x030200 PCIe Endpoint"
	gpu := parseGPUs(unknownLine)
	require.NotNil(t, gpu)
	assert.Equal(t, int32(1), gpu.Count)
	assert.Equal(t, "nvidia", gpu.Vendor)
	assert.Empty(t, gpu.Model)
	assert.Equal(t, "ffff", gpu.DeviceID)
	assert.True(t, gpu.MemoryPerGPU.IsZero())
}

func TestApplyDiscoveryLabelsGPUs(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-gpu"},
		Status: infrav1.ByotHostStatus{
			Phase: infrav1.HostPhaseAvailable,
			Hardware: &infrav1.HostHardware{
				GPUs: &infrav1.HostGPU{
					Vendor:       "nvidia",
					Model:        "h100-pcie",
					DeviceID:     "2331",
					MemoryPerGPU: resource.MustParse("80Gi"),
					Count:        1,
				},
			},
		},
	}
	host.Labels = map[string]string{"site": "gefion"}

	applyDiscoveryLabels(host)

	assert.Equal(t, "1", host.Labels[labelGPUCount])
	assert.Equal(t, "nvidia", host.Labels[labelGPUVendor])
	assert.Equal(t, "h100-pcie", host.Labels[labelGPUModel])
	assert.Equal(t, "gefion", host.Labels["site"])
}

func TestApplyDiscoveryLabelsGPUsMixedOmitsModel(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-mixed"},
		Status: infrav1.ByotHostStatus{
			Phase: infrav1.HostPhaseAvailable,
			Hardware: &infrav1.HostHardware{
				GPUs: &infrav1.HostGPU{
					Vendor: "nvidia",
					Count:  2,
					Mixed:  true,
				},
			},
		},
	}
	host.Labels = map[string]string{}

	applyDiscoveryLabels(host)

	assert.Equal(t, "2", host.Labels[labelGPUCount])
	assert.Equal(t, "nvidia", host.Labels[labelGPUVendor])
	_, hasModel := host.Labels[labelGPUModel]
	assert.False(t, hasModel)
}

func TestApplyDiscoveryLabelsGPUsMixedVendorOmitsVendor(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-mixed-vendor"},
		Status: infrav1.ByotHostStatus{
			Phase: infrav1.HostPhaseAvailable,
			Hardware: &infrav1.HostHardware{
				GPUs: &infrav1.HostGPU{Count: 2, Mixed: true},
			},
		},
	}
	host.Labels = map[string]string{}

	applyDiscoveryLabels(host)

	assert.Equal(t, "2", host.Labels[labelGPUCount])
	_, hasVendor := host.Labels[labelGPUVendor]
	assert.False(t, hasVendor)

	_, hasModel := host.Labels[labelGPUModel]
	assert.False(t, hasModel)
}

func TestApplyDiscoveryLabelsNoGPUOmitsGPULabels(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-cpu"},
		Status: infrav1.ByotHostStatus{
			Phase: infrav1.HostPhaseAvailable,
			Hardware: &infrav1.HostHardware{
				CPU: infrav1.HostCPU{Cores: 4},
			},
		},
	}
	host.Labels = map[string]string{}

	applyDiscoveryLabels(host)

	for _, k := range []string{labelGPUCount, labelGPUVendor, labelGPUModel} {
		_, ok := host.Labels[k]
		assert.False(t, ok, "label %s should be absent on a GPU-less host", k)
	}
}

func TestPopulateFromDiscoveryCopiesGPUs(t *testing.T) {
	t.Parallel()

	reconciler := &ByotHostReconciler{}
	host := &infrav1.ByotHost{ObjectMeta: metav1.ObjectMeta{Name: "h"}}
	result := DiscoveryResult{
		TalosVersion: "v1.13.8",
		GPUs: &infrav1.HostGPU{
			Vendor:       "nvidia",
			Model:        "h100-pcie",
			Count:        1,
			MemoryPerGPU: resource.MustParse("80Gi"),
		},
	}

	reconciler.populateFromDiscovery(host, result)

	require.NotNil(t, host.Status.Hardware)
	require.NotNil(t, host.Status.Hardware.GPUs)
	assert.Equal(t, "h100-pcie", host.Status.Hardware.GPUs.Model)
	assert.True(t, conditions.IsTrue(host, infrav1.HostDiscoveredCondition))
}

func TestPopulateFromDiscoveryMixedGPUsMarksCondition(t *testing.T) {
	t.Parallel()

	reconciler := &ByotHostReconciler{}
	host := &infrav1.ByotHost{ObjectMeta: metav1.ObjectMeta{Name: "h"}}
	result := DiscoveryResult{
		GPUs: &infrav1.HostGPU{Vendor: "nvidia", Count: 2, Mixed: true},
	}

	reconciler.populateFromDiscovery(host, result)

	require.NotNil(t, host.Status.Hardware.GPUs)
	assert.True(t, conditions.IsFalse(host, infrav1.HostDiscoveredCondition))
	assert.Equal(t,
		infrav1.HostDiscoveredReasonMixedGPUModels,
		conditions.Get(host, infrav1.HostDiscoveredCondition).Reason,
	)
}

func TestPopulateFromDiscoveryNoGPUsSucceeds(t *testing.T) {
	t.Parallel()

	reconciler := &ByotHostReconciler{}
	host := &infrav1.ByotHost{ObjectMeta: metav1.ObjectMeta{Name: "h"}}
	result := DiscoveryResult{TalosVersion: "v1.13.8"} // GPUs nil

	reconciler.populateFromDiscovery(host, result)

	require.NotNil(t, host.Status.Hardware)
	assert.Nil(t, host.Status.Hardware.GPUs)
	assert.True(t, conditions.IsTrue(host, infrav1.HostDiscoveredCondition))
}
