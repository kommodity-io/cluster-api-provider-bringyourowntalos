package controller

import (
	"fmt"
	"strings"
	"testing"

	"strconv"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"
	"github.com/cosi-project/runtime/pkg/state/impl/namespaced"
	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/hardware"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
)

// testNICName is the fixed NIC name used across identity discovery tests.
const testNICName = "eth0"

// testSocketCPU0 and testSocketCPU1 are the fixed socket names used across
// CPU discovery tests.
const (
	testSocketCPU0 = "CPU0"
	testSocketCPU1 = "CPU1"
)

func TestDiscoverCPUDefaultsToSingleCPU(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	result := DiscoveryResult{}
	discoverCPU(t.Context(), cosi, &result)

	assert.Equal(t, infrav1.HostCPU{Cores: 1, Packages: 1, NumaNodes: 1}, result.CPU)
}

func TestDiscoverCPUFromProcessors(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	p0 := hardware.NewProcessorInfo(testSocketCPU0)
	p0.TypedSpec().Socket = testSocketCPU0
	p0.TypedSpec().CoreCount = 64
	p0.TypedSpec().ThreadCount = 128
	require.NoError(t, cosi.Create(t.Context(), p0))

	p1 := hardware.NewProcessorInfo(testSocketCPU1)
	p1.TypedSpec().Socket = testSocketCPU1
	p1.TypedSpec().CoreCount = 64
	p1.TypedSpec().ThreadCount = 128
	require.NoError(t, cosi.Create(t.Context(), p1))

	result := DiscoveryResult{}
	discoverCPU(t.Context(), cosi, &result)

	assert.Equal(t, int32(128), result.CPU.Cores)   // 64 + 64
	assert.Equal(t, int32(2), result.CPU.Packages)  // 2 distinct sockets
	assert.Equal(t, int32(1), result.CPU.NumaNodes) // NUMA set later by discoverNumaNodes
}

func TestDiscoverCPUSingleProcessor(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	p := hardware.NewProcessorInfo(testSocketCPU0)
	p.TypedSpec().Socket = testSocketCPU0
	p.TypedSpec().CoreCount = 4
	require.NoError(t, cosi.Create(t.Context(), p))

	result := DiscoveryResult{}
	discoverCPU(t.Context(), cosi, &result)

	assert.Equal(t, int32(4), result.CPU.Cores)
	assert.Equal(t, int32(1), result.CPU.Packages)
}

func TestDiscoverCPUSkipsUnpopulatedSocket(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	// Populated socket.
	p0 := hardware.NewProcessorInfo(testSocketCPU0)
	p0.TypedSpec().Socket = testSocketCPU0
	p0.TypedSpec().CoreCount = 64
	require.NoError(t, cosi.Create(t.Context(), p0))

	// Unpopulated socket: Talos creates a Processor with empty Socket/CoreCount.
	p1 := hardware.NewProcessorInfo(testSocketCPU1)
	p1.TypedSpec().Socket = ""
	p1.TypedSpec().CoreCount = 0
	require.NoError(t, cosi.Create(t.Context(), p1))

	result := DiscoveryResult{}
	discoverCPU(t.Context(), cosi, &result)

	assert.Equal(t, int32(64), result.CPU.Cores)  // only populated socket
	assert.Equal(t, int32(1), result.CPU.Packages)
}

func TestDiscoverPlatformFromCmdline(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	kc := runtime.NewKernelCmdline()
	kc.TypedSpec().Cmdline = "talos.platform=metal console=tty0 printk.devkmsg=on"
	require.NoError(t, cosi.Create(t.Context(), kc))

	result := DiscoveryResult{}
	discoverPlatform(t.Context(), cosi, &result)

	assert.Equal(t, "metal", result.Platform)
}

func TestDiscoverPlatformEmptyWhenNoCmdline(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	result := DiscoveryResult{}
	discoverPlatform(t.Context(), cosi, &result)

	assert.Empty(t, result.Platform)
}

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "scaleway", parsePlatform("Command line: talos.platform=scaleway console=ttyS0"))
	assert.Empty(t, parsePlatform("no cmdline here"))
}

func TestRoundQuantity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ki     int64
		label  string
	}{
		{3 * 1024 * 1024, "3G"},      // ~3Gi -> 3G
		{7 * 1024 * 1024, "7G"},      // ~7Gi -> 7G
		{15 * 1024 * 1024, "15G"},    // ~15Gi -> 15G
		{64 * 1024 * 1024, "64G"},    // ~64Gi -> 64G
		{250 * 1024 * 1024, "250G"},  // ~250Gi -> 250G
		{800 * 1024 * 1024, "800G"},  // ~800Gi -> 800G
		{1024 * 1024 * 1024, "1T"},   // 1Ti -> 1T
		{1500 * 1024 * 1024, "1T"},   // ~1.46Ti -> 1T
		{1900 * 1024 * 1024, "2T"},   // ~1.85Ti -> 2T
		{4000 * 1024 * 1024, "4T"},   // ~3.9Ti -> 4T
	}

	for _, c := range cases {
		q := resource.MustParse(strconv.FormatInt(c.ki, 10) + "Ki")
		assert.Equal(t, c.label, roundQuantity(q))
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
			Phase:        infrav1.HostPhaseAvailable,
			TalosVersion: "v1.13.8",
			Arch:         "amd64",
			Platform:     "scaleway",
			Hardware: &infrav1.HostHardware{
				CPU:    infrav1.HostCPU{Cores: 16},
				Memory: resource.MustParse("64Gi"),
				Disks: []infrav1.HostDisk{
					{Name: "/dev/sda", Size: resource.MustParse("250Gi"), Type: "SSD", SystemDisk: true},
				},
				NetworkInterfaces: []string{testNICName},
			},
		},
	}

	// Operator labels preserved.
	host.Labels = map[string]string{"site": "copenhagen"}

	applyDiscoveryLabels(host)

	assert.Equal(t, "true", host.Labels[labelAvailable])
	assert.Equal(t, "16", host.Labels[labelCPUCores])
	assert.Equal(t, "amd64", host.Labels[labelCPUArch])
	assert.Equal(t, "64G", host.Labels[labelMemory])
	assert.Equal(t, "ssd", host.Labels[labelDiskType])
	assert.Equal(t, "250G", host.Labels[labelDiskSize])
	assert.Equal(t, "scaleway", host.Labels[labelPlatform])
	assert.Equal(t, "v1.13.8", host.Labels[labelTalosVersion])
	assert.Equal(t, "par01", host.Labels[labelFailureDomain])
	assert.Equal(t, "copenhagen", host.Labels["site"]) // operator label preserved
}

func TestApplyDiscoveryLabelsDropsAvailableWhenNotAvailable(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-1"},
		Status:     infrav1.ByotHostStatus{Phase: infrav1.HostPhaseUnavailable},
	}
	host.Labels = map[string]string{labelAvailable: "true", "site": "copenhagen"}

	applyDiscoveryLabels(host)

	_, hasAvailable := host.Labels[labelAvailable]
	assert.False(t, hasAvailable)
	assert.Equal(t, "copenhagen", host.Labels["site"])
}

func TestApplyDiscoveryLabelsPreservesOperatorLabelsAcrossRediscovery(t *testing.T) {
	t.Parallel()

	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-1"},
		Status:     infrav1.ByotHostStatus{Phase: infrav1.HostPhaseAvailable, Arch: "arm64"},
	}
	host.Labels = map[string]string{
		"site":         "copenhagen",
		labelCPUArch:   "amd64", // stale, should be overwritten
		labelAvailable: "true",
	}

	applyDiscoveryLabels(host)

	assert.Equal(t, "arm64", host.Labels[labelCPUArch]) // controller label overwritten
	assert.Equal(t, "copenhagen", host.Labels["site"])  // operator label preserved
}

func TestDiscoverHostFailsOnUnreachable(t *testing.T) {
	t.Parallel()

	// 127.0.0.1 refuses the Talos API: discovery must fail fast.
	_, err := discoverHost(t.Context(), "127.0.0.1")
	require.Error(t, err)

	assert.True(t, strings.Contains(err.Error(), "version discovery") ||
		strings.Contains(err.Error(), "maintenance client"))
}

// makeTestPCIDevice creates a PCIDevice resource in the given COSI state.
func makeTestPCIDevice(t *testing.T, cosi state.CoreState, id string, classID, vendorID, productID string) {
	t.Helper()

	dev := hardware.NewPCIDeviceInfo(id)
	dev.TypedSpec().ClassID = classID
	dev.TypedSpec().VendorID = vendorID
	dev.TypedSpec().ProductID = productID
	require.NoError(t, cosi.Create(t.Context(), dev))
}

func TestDiscoverGPUsH100(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// 10de:2331 = NVIDIA H100 PCIe 80G, display class 0x03.
	makeTestPCIDevice(t, cosi, "0000:01:00.0", "0x03", "0x10de", "0x2331")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(1), result.GPUs.Count)
	assert.Equal(t, "nvidia", result.GPUs.Vendor)
	assert.Equal(t, "h100-pcie", result.GPUs.Model)
	assert.Equal(t, "2331", result.GPUs.DeviceID)
	assert.False(t, result.GPUs.Mixed)
	assert.Equal(t, "80Gi", result.GPUs.MemoryPerGPU.String())
	assert.Equal(t, "80Gi", result.GPUs.TotalMemory.String())
}

func TestDiscoverGPUsMultiB300(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// 10de:3182 = B300 SXM6, 8 identical GPUs.
	for i := range 8 {
		makeTestPCIDevice(t, cosi, fmt.Sprintf("0000:%02x:00.0", 0x1a+i*2), "0x03", "0x10de", "0x3182")
	}

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(8), result.GPUs.Count)
	assert.Equal(t, "nvidia", result.GPUs.Vendor)
	assert.Equal(t, "b300-sxm6", result.GPUs.Model)
	assert.Equal(t, "3182", result.GPUs.DeviceID)
	assert.False(t, result.GPUs.Mixed)
	assert.Equal(t, "288Gi", result.GPUs.MemoryPerGPU.String())
	assert.Equal(t, "2304Gi", result.GPUs.TotalMemory.String()) // 8 * 288Gi
}

func TestDiscoverGPUsMixedModels(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	makeTestPCIDevice(t, cosi, "0000:01:00.0", "0x03", "0x10de", "0x2331") // H100
	makeTestPCIDevice(t, cosi, "0000:02:00.0", "0x03", "0x10de", "0x3182") // B300

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(2), result.GPUs.Count)
	assert.Equal(t, "nvidia", result.GPUs.Vendor) // same vendor
	assert.Empty(t, result.GPUs.Model)
	assert.Empty(t, result.GPUs.DeviceID)
	assert.True(t, result.GPUs.Mixed)
	assert.True(t, result.GPUs.MemoryPerGPU.IsZero())
	assert.True(t, result.GPUs.TotalMemory.IsZero())
}

func TestDiscoverGPUsMixedVendors(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	makeTestPCIDevice(t, cosi, "0000:01:00.0", "0x03", "0x10de", "0x2331") // NVIDIA
	makeTestPCIDevice(t, cosi, "0000:03:00.0", "0x03", "0x1002", "0x740f") // AMD

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(2), result.GPUs.Count)
	assert.Empty(t, result.GPUs.Vendor) // mixed vendor -> omitted
	assert.Empty(t, result.GPUs.Model)
	assert.True(t, result.GPUs.Mixed)
}

func TestDiscoverGPUsIgnoresNonDisplayNvidia(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// NVIDIA HDA audio controller: vendor 10de, class 0x04 (audio), not display.
	makeTestPCIDevice(t, cosi, "0000:01:00.1", "0x04", "0x10de", "0x22f1")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	assert.Nil(t, result.GPUs)
}

func TestDiscoverGPUsNoGPU(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// Intel SATA controller: known vendor (8086), non-display class.
	makeTestPCIDevice(t, cosi, "0000:00:1f.2", "0x01", "0x8086", "0x2922")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	assert.Nil(t, result.GPUs)
}

func TestDiscoverGPUsEmptyCOSI(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)
	assert.Nil(t, result.GPUs)
}

func TestDiscoverGPUsUnknownNvidiaDevice(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	makeTestPCIDevice(t, cosi, "0000:02:00.0", "0x03", "0x10de", "0xffff")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(1), result.GPUs.Count)
	assert.Equal(t, "nvidia", result.GPUs.Vendor)
	assert.Empty(t, result.GPUs.Model)
	assert.Equal(t, "ffff", result.GPUs.DeviceID)
	assert.True(t, result.GPUs.MemoryPerGPU.IsZero())
}

func TestDiscoverGPUsAMDInstinct(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// MI300X: 1002:74a1, display class 0x03. HBM 192GB per vendor specs.
	makeTestPCIDevice(t, cosi, "0000:43:00.0", "0x03", "0x1002", "0x74a1")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	require.NotNil(t, result.GPUs)
	assert.Equal(t, int32(1), result.GPUs.Count)
	assert.Equal(t, "amd", result.GPUs.Vendor)
	assert.Equal(t, "mi300x", result.GPUs.Model)
	assert.Equal(t, "74a1", result.GPUs.DeviceID)
	assert.False(t, result.GPUs.Mixed)
	assert.Equal(t, "192Gi", result.GPUs.MemoryPerGPU.String())
	assert.Equal(t, "192Gi", result.GPUs.TotalMemory.String())
}

func TestDiscoverGPUsIgnoresBMCVGA(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()
	// ASPEED BMC VGA: vendor 1a03, display class 0x03 — not a known GPU vendor.
	makeTestPCIDevice(t, cosi, "0000:29:00.0", "0x03", "0x1a03", "0x2000")

	result := DiscoveryResult{}
	discoverGPUs(t.Context(), cosi, &result)

	assert.Nil(t, result.GPUs, "BMC VGA from unknown vendor must not be counted")
}

func TestIsNumaNodeDir(t *testing.T) {
	t.Parallel()

	assert.True(t, isNumaNodeDir("node0"))
	assert.True(t, isNumaNodeDir("node1"))
	assert.True(t, isNumaNodeDir("node12"))
	assert.False(t, isNumaNodeDir("node"))
	assert.False(t, isNumaNodeDir("node0a"))
	assert.False(t, isNumaNodeDir("online"))
	assert.False(t, isNumaNodeDir("has_memory"))
	assert.False(t, isNumaNodeDir(""))
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
	host.Labels = map[string]string{"site": "copenhagen"}

	applyDiscoveryLabels(host)

	assert.Equal(t, "1", host.Labels[labelGPUCount])
	assert.Equal(t, "nvidia", host.Labels[labelGPUVendor])
	assert.Equal(t, "h100-pcie", host.Labels[labelGPUModel])
	assert.Equal(t, "copenhagen", host.Labels["site"])
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

// TestPopulateFromDiscoveryPreservesIdentityOnEmptyResult guards the review
// concern: a transient COSI failure yields a nil Identity, which must not
// clobber a previously recorded reboot-stable identity.
func TestPopulateFromDiscoveryPreservesIdentityOnEmptyResult(t *testing.T) {
	t.Parallel()

	reconciler := &ByotHostReconciler{}
	prior := &infrav1.HostIdentity{SystemUUID: "11111111-2222-3333-4444-555555555555"}
	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h"},
		Status:     infrav1.ByotHostStatus{Identity: prior},
	}
	result := DiscoveryResult{TalosVersion: "v1.13.8"} // Identity nil (COSI fetch failed)

	reconciler.populateFromDiscovery(host, result)

	require.NotNil(t, host.Status.Identity)
	assert.Equal(t, prior.SystemUUID, host.Status.Identity.SystemUUID)
}

// TestPopulateFromDiscoveryOverwritesIdentityOnNewResult ensures a freshly
// discovered identity replaces a prior one.
func TestPopulateFromDiscoveryOverwritesIdentityOnNewResult(t *testing.T) {
	t.Parallel()

	reconciler := &ByotHostReconciler{}
	prior := &infrav1.HostIdentity{SystemUUID: "old-uuid"}
	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h"},
		Status:     infrav1.ByotHostStatus{Identity: prior},
	}
	result := DiscoveryResult{
		Identity: &infrav1.HostIdentity{SystemUUID: "new-uuid"},
	}

	reconciler.populateFromDiscovery(host, result)

	require.NotNil(t, host.Status.Identity)
	assert.Equal(t, "new-uuid", host.Status.Identity.SystemUUID)
}


// newTestCOSI builds a multi-namespace in-memory COSI state for identity
// discovery tests. The hardware and network namespaces hold the
// SystemInformation and HardwareAddr resources respectively.
func newTestCOSI() state.CoreState {
	return namespaced.NewState(inmem.Build)
}

func TestDiscoverIdentityPopulatesFromCOSI(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	sysInfo := hardware.NewSystemInformation(hardware.SystemInformationID)
	sysInfo.TypedSpec().UUID = "12345678-1234-1234-1234-123456789012"
	sysInfo.TypedSpec().SerialNumber = "SVC0001"
	sysInfo.TypedSpec().Manufacturer = "Supermicro"
	sysInfo.TypedSpec().ProductName = "X12SPA"
	require.NoError(t, cosi.Create(t.Context(), sysInfo))

	hwAddr := network.NewHardwareAddr(network.NamespaceName, network.FirstHardwareAddr)
	hwAddr.TypedSpec().Name = testNICName
	hwAddr.TypedSpec().HardwareAddr = nethelpers.HardwareAddr{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
	require.NoError(t, cosi.Create(t.Context(), hwAddr))

	result := DiscoveryResult{}
	discoverIdentity(t.Context(), cosi, &result)

	require.NotNil(t, result.Identity)
	assert.Equal(t, "12345678-1234-1234-1234-123456789012", result.Identity.SystemUUID)
	assert.Equal(t, "SVC0001", result.Identity.SerialNumber)
	assert.Equal(t, "Supermicro", result.Identity.Manufacturer)
	assert.Equal(t, "X12SPA", result.Identity.ProductName)
	assert.Equal(t, "52:54:00:12:34:56", result.Identity.HardwareAddr)
}

func TestDiscoverIdentityEmptyWhenNoResources(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	result := DiscoveryResult{}
	discoverIdentity(t.Context(), cosi, &result)

	assert.Nil(t, result.Identity, "all-empty identity must stay nil to signal unavailable")
}

func TestDiscoverIdentityPartialMacOnly(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	hwAddr := network.NewHardwareAddr(network.NamespaceName, network.FirstHardwareAddr)
	hwAddr.TypedSpec().Name = testNICName
	hwAddr.TypedSpec().HardwareAddr = nethelpers.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc}
	require.NoError(t, cosi.Create(t.Context(), hwAddr))

	result := DiscoveryResult{}
	discoverIdentity(t.Context(), cosi, &result)

	require.NotNil(t, result.Identity)
	assert.Empty(t, result.Identity.SystemUUID, "SMBIOS UUID absent")
	assert.Equal(t, "52:54:00:aa:bb:cc", result.Identity.HardwareAddr)
}

func TestDiscoverIdentityPartialUUIDOnly(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	sysInfo := hardware.NewSystemInformation(hardware.SystemInformationID)
	sysInfo.TypedSpec().UUID = "abcdefab-cdef-abcd-efab-cdefabcdefab"
	require.NoError(t, cosi.Create(t.Context(), sysInfo))

	result := DiscoveryResult{}
	discoverIdentity(t.Context(), cosi, &result)

	require.NotNil(t, result.Identity)
	assert.Equal(t, "abcdefab-cdef-abcd-efab-cdefabcdefab", result.Identity.SystemUUID)
	assert.Empty(t, result.Identity.HardwareAddr, "first-up NIC absent")
}

// TestDiscoverIdentitySkipsAllZeroMAC guards the virtual-NIC zero-MAC case
// from the review: a 6-byte all-zero MAC stringifies to
// "00:00:00:00:00:00" (non-empty) and must not be stored as a valid identity.
func TestDiscoverIdentitySkipsAllZeroMAC(t *testing.T) {
	t.Parallel()

	cosi := newTestCOSI()

	hwAddr := network.NewHardwareAddr(network.NamespaceName, network.FirstHardwareAddr)
	hwAddr.TypedSpec().Name = testNICName
	hwAddr.TypedSpec().HardwareAddr = nethelpers.HardwareAddr{0, 0, 0, 0, 0, 0}
	require.NoError(t, cosi.Create(t.Context(), hwAddr))

	result := DiscoveryResult{}
	discoverIdentity(t.Context(), cosi, &result)

	assert.Nil(t, result.Identity, "all-zero MAC must not be stored as a valid identity")
}
