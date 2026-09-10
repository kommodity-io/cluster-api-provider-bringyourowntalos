package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectInstallDiskPreservesHostnameConfigDoc(t *testing.T) {
	t.Parallel()

	// CABPT bootstrap data: v1alpha1 config + HostnameConfig as a second doc.
	config := []byte("version: v1alpha1\nmachine:\n  install:\n    wipe: false\n---\napiVersion: v1alpha1\nkind: HostnameConfig\nauto: \"off\"\nhostname: paul-test-worker-default-ch2z9-frgnj\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	s := string(out)
	assert.Contains(t, s, "disk: /dev/sda")
	assert.Contains(t, s, "wipe: false")
	assert.Contains(t, s, "version: v1alpha1")
	// HostnameConfig doc must survive the round-trip.
	assert.Contains(t, s, "HostnameConfig")
	assert.Contains(t, s, "auto: \"off\"")
	assert.Contains(t, s, "paul-test-worker-default-ch2z9-frgnj")
}

func TestInjectInstallDiskPreservesMultipleV1alpha1Docs(t *testing.T) {
	t.Parallel()

	// Three docs, only the v1alpha1 one has machine.install.
	config := []byte("---\napiVersion: v1alpha1\nkind: ExtensionServiceConfig\n---\nversion: v1alpha1\nmachine:\n  install:\n    wipe: false\n---\napiVersion: v1alpha1\nkind: KmsgLogConfig\n")

	out, err := injectInstallDisk(config, "/dev/sda")
	require.NoError(t, err)

	s := string(out)
	assert.Contains(t, s, "disk: /dev/sda")
	assert.Contains(t, s, "ExtensionServiceConfig")
	assert.Contains(t, s, "KmsgLogConfig")
}
