package controller

import (
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
