package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectInstallDiskAddsWhenAbsent(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  install:\n    wipe: false\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	assert.Contains(t, string(out), "disk: /dev/sda")
	assert.Contains(t, string(out), "wipe: false")
}

func TestInjectInstallDiskPreservesExistingDisk(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  install:\n    disk: /dev/nvme0n1\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	assert.Contains(t, string(out), "/dev/nvme0n1")
	assert.NotContains(t, string(out), "/dev/sda")
}

func TestInjectInstallDiskPreservesExistingDiskSelector(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  install:\n    diskSelector:\n      size: 20GB\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	assert.Contains(t, string(out), "size: 20GB")
	assert.NotContains(t, string(out), "/dev/sda")
}

func TestInjectInstallDiskPreservesIntScalars(t *testing.T) {
	t.Parallel()

	// A YAML round-trip via JSON would corrupt 6443 to 6443.0; yaml.v3 keeps it int.
	config := []byte("cluster:\n  controlPlane:\n    localPort: 6443\nmachine:\n  install:\n    wipe: false\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	assert.Contains(t, string(out), "localPort: 6443")
	assert.NotContains(t, string(out), "6443.0")
	assert.Contains(t, string(out), "disk: /dev/sda")
}

func TestInjectInstallDiskCreatesInstallWhenMissing(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  kubelet:\n    image: kubelet:v1.35.0\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	assert.Contains(t, string(out), "disk: /dev/sda")
	assert.Contains(t, string(out), "kubelet:v1.35.0")
}

func TestInjectInstallDiskWithRAIDOverride(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  install:\n    wipe: false\n")

	out, err := injectInstallDisk(config, "/dev/md127")
	require.NoError(t, err)

	assert.Contains(t, string(out), "disk: /dev/md127")
	assert.Contains(t, string(out), "wipe: false")
}

func TestInjectCertSANsAddsWhenAbsent(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  kubelet:\n    image: kubelet:v1.35.0\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	assert.Contains(t, string(out), "130.226.165.252")
	assert.Contains(t, string(out), "kubelet:v1.35.0")
}

func TestInjectCertSANsPreservesExistingSANs(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  certSANs:\n    - 10.10.53.50\n    - 127.0.0.1\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	assert.Contains(t, string(out), "10.10.53.50")
	assert.Contains(t, string(out), "127.0.0.1")
	assert.Contains(t, string(out), "130.226.165.252")
}

func TestInjectCertSANsIdempotent(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  certSANs:\n    - 10.10.53.50\n    - 130.226.165.252\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	assert.Contains(t, string(out), "10.10.53.50")
	assert.Contains(t, string(out), "130.226.165.252")
	// Count occurrences of the IP — should be exactly 1 (not duplicated).
	assert.Equal(t, 1, strings.Count(string(out), "130.226.165.252"))
}

func TestInjectCertSANsPreservesEmptyList(t *testing.T) {
	t.Parallel()

	config := []byte("machine:\n  certSANs: []\n  kubelet:\n    image: kubelet:v1.35.0\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	assert.Contains(t, string(out), "130.226.165.252")
	assert.Contains(t, string(out), "kubelet:v1.35.0")
}

func TestInjectCertSANsPreservesMultiDoc(t *testing.T) {
	t.Parallel()

	config := []byte("version: v1alpha1\nmachine:\n  certSANs: []\n" +
		"---\napiVersion: v1alpha1\nkind: HostnameConfig\n" +
		"auto: \"off\"\nhostname: prd-cph04-controlplane-112ba5-wvrqq\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	rendered := string(out)
	assert.Contains(t, rendered, "130.226.165.252")
	assert.Contains(t, rendered, "HostnameConfig")
	assert.Contains(t, rendered, "prd-cph04-controlplane-112ba5-wvrqq")
}

func TestInjectCertSANsSkipsNonMachineDocs(t *testing.T) {
	t.Parallel()

	config := []byte("---\napiVersion: v1alpha1\nkind: VolumeConfig\nname: STATE\n" +
		"---\nversion: v1alpha1\nmachine:\n  certSANs: []\n")

	out, err := injectCertSANs(config, "130.226.165.252")
	require.NoError(t, err)

	rendered := string(out)
	assert.Contains(t, rendered, "130.226.165.252")
	assert.Contains(t, rendered, "VolumeConfig")
}

func TestInjectClusterAPIServerCertSANsAddsWhenAbsent(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  network:\n    podSubnets:\n      - 10.244.0.0/16\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252", "172.16.16.2"})
	require.NoError(t, err)

	assert.Contains(t, string(out), "130.226.165.252")
	assert.Contains(t, string(out), "172.16.16.2")
	assert.Contains(t, string(out), "10.244.0.0/16")
}

func TestInjectClusterAPIServerCertSANsPreservesExistingSANs(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  apiServer:\n    certSANs:\n      - 10.10.53.50\n      - 127.0.0.1\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	assert.Contains(t, string(out), "10.10.53.50")
	assert.Contains(t, string(out), "127.0.0.1")
	assert.Contains(t, string(out), "130.226.165.252")
}

func TestInjectClusterAPIServerCertSANsIdempotent(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  apiServer:\n    certSANs:\n      - 10.10.53.50\n      - 130.226.165.252\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	assert.Contains(t, string(out), "10.10.53.50")
	assert.Equal(t, 1, strings.Count(string(out), "130.226.165.252"))
}

func TestInjectClusterAPIServerCertSANsPreservesEmptyList(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  apiServer:\n    certSANs: []\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	assert.Contains(t, string(out), "130.226.165.252")
}

func TestInjectClusterAPIServerCertSANsCreatesApiServerWhenMissing(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  network:\n    podSubnets:\n      - 10.244.0.0/16\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	assert.Contains(t, string(out), "certSANs:")
	assert.Contains(t, string(out), "130.226.165.252")
	assert.Contains(t, string(out), "10.244.0.0/16")
}

func TestInjectClusterAPIServerCertSANsPreservesMultiDoc(t *testing.T) {
	t.Parallel()

	config := []byte("version: v1alpha1\ncluster:\n  apiServer:\n    certSANs: []\n" +
		"---\napiVersion: v1alpha1\nkind: HostnameConfig\n" +
		"auto: \"off\"\nhostname: prd-cph04-controlplane-112ba5-wvrqq\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	rendered := string(out)
	assert.Contains(t, rendered, "130.226.165.252")
	assert.Contains(t, rendered, "HostnameConfig")
	assert.Contains(t, rendered, "prd-cph04-controlplane-112ba5-wvrqq")
}

func TestInjectClusterAPIServerCertSANsSkipsNonClusterDocs(t *testing.T) {
	t.Parallel()

	config := []byte("---\napiVersion: v1alpha1\nkind: VolumeConfig\nname: STATE\n" +
		"---\nversion: v1alpha1\ncluster:\n  apiServer:\n    certSANs: []\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252"})
	require.NoError(t, err)

	rendered := string(out)
	assert.Contains(t, rendered, "130.226.165.252")
	assert.Contains(t, rendered, "VolumeConfig")
}

func TestInjectClusterAPIServerCertSANsMultipleIPs(t *testing.T) {
	t.Parallel()

	config := []byte("cluster:\n  apiServer:\n    certSANs: []\n")

	out, err := injectClusterAPIServerCertSANs(config, []string{"130.226.165.252", "172.16.16.2"})
	require.NoError(t, err)

	rendered := string(out)
	assert.Contains(t, rendered, "130.226.165.252")
	assert.Contains(t, rendered, "172.16.16.2")
	assert.Equal(t, 1, strings.Count(rendered, "130.226.165.252"))
	assert.Equal(t, 1, strings.Count(rendered, "172.16.16.2"))
}
