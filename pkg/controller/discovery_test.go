package controller

import (
	"strings"
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
		{3 * 1024 * 1024, "4G"},     // ~3Gi -> 4G
		{7 * 1024 * 1024, "8G"},     // ~7Gi -> 8G
		{15 * 1024 * 1024, "16G"},   // ~15Gi -> 16G
		{30 * 1024 * 1024, "32G"},   // ~30Gi -> 32G
		{60 * 1024 * 1024, "64G"},   // ~60Gi -> 64G
		{120 * 1024 * 1024, "128G"}, // ~120Gi -> 128G
		{400 * 1024 * 1024, "128G"}, // oversized -> clamp
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