package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func objectKey(name string, namespace string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: namespace}
}

const (
	testClusterName = "test-cluster"
	testMachineName = "test-machine"
	testNamespace   = "default"
	testMachineUID  = "machine-uid"
	testHostName    = "test-host"
)

func clusterKey(obj metav1.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

// newByotMachine builds a ByotMachine that has already claimed test-host
// (resolvedHost/resolvedPublicIP set) so adoption-target tests can proceed
// past the claim step. publicIP is the resolved host IP.
func newByotMachine(publicIP string) *infrav1.ByotMachine {
	return &infrav1.ByotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMachineName,
			Namespace: testNamespace,
			UID:       testMachineUID,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: testClusterName,
			},
		},
		Spec: infrav1.ByotMachineSpec{
			HostRef: &infrav1.LocalObjectReference{Name: testHostName},
		},
		Status: infrav1.ByotMachineStatus{
			ResolvedHost:     testHostName,
			ResolvedPublicIP: publicIP,
		},
	}
}

// newClaimedByotHost builds the ByotHost claimed by newByotMachine: phase
// Claimed, claimRef pointing at the test ByotMachine, spec.publicIP set.
func newClaimedByotHost(publicIP string) *infrav1.ByotHost {
	return &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testHostName,
			Namespace:  testNamespace,
			Finalizers: []string{byotHostFinalizer},
		},
		Spec: infrav1.ByotHostSpec{PublicIP: publicIP},
		Status: infrav1.ByotHostStatus{
			Phase: infrav1.HostPhaseClaimed,
			ClaimRef: &infrav1.HostClaimRef{
				Kind:      "ByotMachine",
				Name:      testMachineName,
				Namespace: testNamespace,
				UID:       testMachineUID,
			},
		},
	}
}
func TestByotMachineReconcileAddsFinalizer(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("203.0.113.10")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(byotMachine).
		Build()

	reconciler := NewByotMachineReconciler(client)

	// No Machine owner yet: the reconciler adds the finalizer and requeues.
	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}

	err = client.Get(t.Context(), clusterKey(byotMachine), updated)
	require.NoError(t, err)
	assert.Contains(t, updated.Finalizers, byotMachineFinalizer)
	assert.False(t, updated.Status.Ready)
	assert.Nil(t, updated.Spec.ProviderID)
}

// newAdoptedByotMachine builds a ByotMachine that is already adopted (ready,
// providerID set, owner Machine reference attached) for tests that start past
// the adoption phase. publicIP and configHash are kept as parameters for
// readability even though current callers share defaults.
//
//nolint:unparam // test builder: parameters document intent for future cases
func newAdoptedByotMachine(publicIP string, configHash string) *infrav1.ByotMachine {
	machine := newByotMachine(publicIP)
	providerID := infrav1.ProviderIDPrefix + publicIP
	machine.Spec.ProviderID = &providerID
	machine.Status.Ready = true
	machine.Status.LastAppliedConfigSHA = configHash
	machine.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       testMachineName,
			UID:        testMachineUID,
		},
	}

	return machine
}

// newOwningMachine builds the CAPI Machine owning a ByotMachine, with its
// bootstrap data secret reference set. dataSecretName is kept as a parameter
// for readability even though current callers share a default.
//
//nolint:unparam // test builder: parameter documents intent for future cases
func newOwningMachine(dataSecretName string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMachineName,
			Namespace: testNamespace,
			UID:       testMachineUID,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: testClusterName,
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: testClusterName,
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: &dataSecretName,
			},
		},
	}
}

func newBootstrapSecret(name string, namespace string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			bootstrapSecretDataKey: data,
		},
	}
}

func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func TestByotMachineReconcileNoOpWhenConfigUnchanged(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	configData := []byte("cluster:\n  controlPlane: {}\n")
	currentHash := sha256HexOf(configData)

	byotMachine := newAdoptedByotMachine("203.0.113.10", currentHash)
	host := newClaimedByotHost("203.0.113.10")
	machine := newOwningMachine("test-bootstrap")
	machine.Status.NodeRef = &corev1.ObjectReference{Name: "test-node"} // node already linked
	secret := newBootstrapSecret("test-bootstrap", "default", configData)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, host, machine, secret).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, result.Requeue)
}

func TestByotMachineReconcileRequeuesWhenClusterTalosConfigMissing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newAdoptedByotMachine("203.0.113.10", "stale-hash")
	host := newClaimedByotHost("203.0.113.10")
	machine := newOwningMachine("test-bootstrap")
	secret := newBootstrapSecret("test-bootstrap", "default", []byte("new-config"))

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, host, machine, secret).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	// Config changed; machine is already configured, so it needs the cluster
	// talosconfig, which does not exist yet: expect a requeue.
	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)
}

func TestByotMachineReconcileDeleteBlocksUntilResetSucceeds(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	// 127.0.0.1 refuses the Talos API connection immediately: the reset
	// fails fast and deletion must stay blocked with the finalizer retained.
	byotMachine := newByotMachine("127.0.0.1")
	host := newClaimedByotHost("127.0.0.1")
	byotMachine.Finalizers = []string{byotMachineFinalizer}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, host).
		WithStatusSubresource(byotMachine, host).
		Build()

	reconciler := NewByotMachineReconciler(client)

	err = client.Delete(t.Context(), byotMachine)
	require.NoError(t, err)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Equal(t, requeueAfterResetIssued, result.RequeueAfter)

	// Deletion is blocked: the object still carries the finalizer.
	preserved := &infrav1.ByotMachine{}
	err = client.Get(t.Context(), clusterKey(byotMachine), preserved)
	require.NoError(t, err)
	assert.Contains(t, preserved.Finalizers, byotMachineFinalizer)
}

func TestByotMachineReconcileDeleteReleasesWithoutReset(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	// A ByotMachine that never claimed a host is removed immediately on
	// delete (no host to reset).
	byotMachine := newByotMachine("127.0.0.1")
	byotMachine.Status.ResolvedHost = ""
	byotMachine.Status.ResolvedPublicIP = ""
	byotMachine.Finalizers = []string{byotMachineFinalizer}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(byotMachine).
		Build()

	reconciler := NewByotMachineReconciler(client)

	err = client.Delete(t.Context(), byotMachine)
	require.NoError(t, err)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	// Finalizer removed: the object is gone.
	deleted := &infrav1.ByotMachine{}
	err = client.Get(t.Context(), clusterKey(byotMachine), deleted)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestByotMachineReconcileJoinPreflightFailsWithoutCredentials(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	// 127.0.0.1 refuses every Talos API connection: the machine is neither
	// in maintenance mode nor verifiable against the cluster talosconfig,
	// and no foreign talosconfig reference exists. The join preflight must
	// fail with NoCredentials instead of blindly applying configuration.
	byotMachine := newByotMachine("127.0.0.1")
	host := newClaimedByotHost("127.0.0.1")
	byotMachine.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
			Name:       "test-machine",
			UID:        testMachineUID,
		},
	}
	machine := newOwningMachine("test-bootstrap")
	secret := newBootstrapSecret("test-bootstrap", "default", []byte("new-config"))
	cluster := newTalosConfigSecret("test-cluster-talosconfig", "default", []byte("cluster"))

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, host, machine, secret, cluster).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	_, err = reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotMachine),
	})
	require.ErrorIs(t, err, ErrJoinNoCredentials)

	updated := &infrav1.ByotMachine{}
	err = client.Get(t.Context(), clusterKey(byotMachine), updated)
	require.NoError(t, err)
	assert.False(t, updated.Status.Ready)

	condition := conditions.Get(updated, JoinPreflightCondition)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, "NoCredentials", condition.Reason)
}

func TestResetCredentialCandidatesOrder(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("203.0.113.10")
	byotMachine.Spec.TalosConfigSecretRef = &infrav1.LocalObjectReference{Name: "foreign-creds"}

	foreign := newTalosConfigSecret("foreign-creds", "default", []byte("foreign"))
	cluster := newTalosConfigSecret("test-cluster-talosconfig", "default", []byte("cluster"))

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, foreign, cluster).
		Build()

	reconciler := NewByotMachineReconciler(client)

	candidates, err := reconciler.resetCredentialCandidates(t.Context(), byotMachine)
	require.NoError(t, err)
	require.Len(t, candidates, 3)
	assert.Equal(t, []byte("foreign"), candidates[0])
	assert.Equal(t, []byte("cluster"), candidates[1])
	assert.Nil(t, candidates[2])
}

func TestResetCredentialCandidatesInsecureOnly(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("203.0.113.10")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		Build()

	reconciler := NewByotMachineReconciler(client)

	candidates, err := reconciler.resetCredentialCandidates(t.Context(), byotMachine)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Nil(t, candidates[0])
}

func TestAttemptResetWithoutCredentials(t *testing.T) {
	t.Parallel()

	err := attemptReset(t.Context(), [][]byte{}, "203.0.113.10")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoResetCredentials)
}

func newTalosConfigSecret(name string, namespace string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			talosConfigSecretDataKey: data,
		},
	}
}

var errTestKubeletNotDefined = errors.New(`service "kubelet" not defined`)

var (
	errTestWorkloadGet   = errors.New("workload api down")
	errTestWorkloadPatch = errors.New("patch forbidden")
	errTestMustNotCall   = errors.New("workload client must not be called")
)

func TestNudgeKubeletAfterSplitReadoptFailsNonFatal(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// Genuine split-re-adopt: bundleMatch on a not-yet-ready ByotMachine
	// whose kubelet is already Running, with a failing restart (machine
	// rebooting, kubelet service not yet defined). The nudge records a
	// warning but never reverts adoption.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"),
		true, false,
		func(context.Context, string, []byte, string) (bool, error) { return true, nil },
		func(context.Context, string, []byte, string) error {
			return errTestKubeletNotDefined
		})

	condition := conditions.Get(byotMachine, KubeletRestartNudgeCondition)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, "RestartFailed", condition.Reason)
	assert.Equal(t, clusterv1.ConditionSeverityWarning, condition.Severity)
	assert.Contains(t, condition.Message, `service "kubelet" not defined`)
}

func TestNudgeKubeletAfterSplitReadoptSucceeds(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"),
		true, false,
		func(context.Context, string, []byte, string) (bool, error) { return true, nil },
		func(context.Context, string, []byte, string) error { return nil })

	condition := conditions.Get(byotMachine, KubeletRestartNudgeCondition)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
}

func TestNudgeKubeletAfterSplitReadoptSkipsWhenNotBundleMatch(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// Fresh maintenance-mode adoption (bundleMatch=false): no kubelet restart
	// nudge is issued, the kubelet starts on boot.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, nil, false, false,
		func(context.Context, string, []byte, string) (bool, error) {
			t.Fatal("running probe must not be called when bundleMatch is false")

			return false, nil
		},
		func(context.Context, string, []byte, string) error {
			t.Fatal("restart must not be called when bundleMatch is false")

			return nil
		})

	assert.Nil(t, conditions.Get(byotMachine, KubeletRestartNudgeCondition))
}

func TestNudgeKubeletAfterSplitReadoptSkipsWhenAlreadyReady(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// Re-apply on an already-adopted machine (wasReady=true): no nudge, the
	// kubelet picks up the updated config without a restart.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"), true, true,
		func(context.Context, string, []byte, string) (bool, error) {
			t.Fatal("running probe must not be called when the machine was already ready")

			return false, nil
		},
		func(context.Context, string, []byte, string) error {
			t.Fatal("restart must not be called when the machine was already ready")

			return nil
		})

	assert.Nil(t, conditions.Get(byotMachine, KubeletRestartNudgeCondition))
}

func TestNudgeKubeletAfterSplitReadoptSkipsFreshAdoption(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// Fresh maintenance-mode adoption: the first apply reboots the machine,
	// and a stale-cache reconcile probes bundleMatch=true with wasReady=false
	// before the kubelet is back up. The running probe reports not-Running,
	// so the nudge must not fire: the kubelet registers on boot. This is the
	// race that previously recorded a spurious
	// KubeletRestartNudge=False/RestartFailed right after a first adoption.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"), true, false,
		func(context.Context, string, []byte, string) (bool, error) { return false, nil },
		func(context.Context, string, []byte, string) error {
			t.Fatal("restart must not be called when the kubelet is not yet running")

			return nil
		})

	assert.Nil(t, conditions.Get(byotMachine, KubeletRestartNudgeCondition))
}

func TestNudgeKubeletAfterSplitReadoptSkipsOnRunningProbeError(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// The running probe fails (machine rebooting, Talos API unreachable): the
	// nudge is skipped without recording a condition. The kubelet registers
	// on boot regardless.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"), true, false,
		func(context.Context, string, []byte, string) (bool, error) { return false, errTestKubeletNotDefined },
		func(context.Context, string, []byte, string) error {
			t.Fatal("restart must not be called when the running probe fails")

			return nil
		})

	assert.Nil(t, conditions.Get(byotMachine, KubeletRestartNudgeCondition))
}

func TestNudgeKubeletAfterSplitReadoptFiresOnRoundTrip(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")

	// splitPolicy=None round-trip: the ByotMachine is deleted by Cluster API
	// and recreated by Helm with no prior config hash, but the host still
	// carries the bundle (bundleMatch=true). Its kubelet is Running with a
	// Node deleted by Cluster API, so the nudge must restart it to
	// re-register. The previouslyAdopted (config hash) guard could not
	// distinguish this from a fresh adoption; the running probe can.
	nudgeKubeletAfterSplitReadopt(t.Context(), byotMachine, []byte("talosconfig"), true, false,
		func(context.Context, string, []byte, string) (bool, error) { return true, nil },
		func(context.Context, string, []byte, string) error { return nil })

	condition := conditions.Get(byotMachine, KubeletRestartNudgeCondition)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
}

func TestEnsureNodeLinkedRetriggersWhenNoNodeRef(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap") // no NodeRef

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(byotMachine, client)
	require.NoError(t, err)

	result, err := reconciler.ensureNodeLinked(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Equal(t, requeueAfterNodeLink, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}
	err = client.Get(t.Context(), clusterKey(byotMachine), updated)
	require.NoError(t, err)
	assert.Contains(t, updated.Annotations, nodeLinkRetriggerAnnotation)
}

func TestEnsureNodeLinkedClearsAnnotationWhenLinked(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	byotMachine.Annotations = map[string]string{nodeLinkRetriggerAnnotation: "12345"}
	machine := newOwningMachine("test-bootstrap")
	machine.Status.NodeRef = &corev1.ObjectReference{Name: "test-node"} // node now linked

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(byotMachine, client)
	require.NoError(t, err)

	result, err := reconciler.ensureNodeLinked(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}
	err = client.Get(t.Context(), clusterKey(byotMachine), updated)
	require.NoError(t, err)
	assert.NotContains(t, updated.Annotations, nodeLinkRetriggerAnnotation)
}

func TestEnsureNodeLinkedNoopWhenNotReady(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	// Not yet adopted: nothing to link, no requeue.
	byotMachine := newByotMachine("203.0.113.10")
	machine := newOwningMachine("test-bootstrap")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(byotMachine, client)
	require.NoError(t, err)

	result, err := reconciler.ensureNodeLinked(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func TestWithoutNodeLinkRetriggerStripsAnnotation(t *testing.T) {
	t.Parallel()

	const otherKey = "other"

	t.Run("nil map returns empty map", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, map[string]string{}, withoutNodeLinkRetrigger(nil))
	})

	t.Run("strips only the node-link annotation", func(t *testing.T) {
		t.Parallel()

		got := withoutNodeLinkRetrigger(map[string]string{
			nodeLinkRetriggerAnnotation: "1700000000000000000",
			otherKey:                    "kept",
		})
		assert.Equal(t, map[string]string{otherKey: "kept"}, got)
	})
}

func TestNodeLinkRetriggerSelfFilterUpdate(t *testing.T) {
	t.Parallel()

	makeMachine := func(annotations map[string]string, generation int64) *infrav1.ByotMachine {
		m := newByotMachine("203.0.113.10")
		m.Annotations = annotations
		m.Generation = generation

		return m
	}

	t.Run("drops annotation-only bump", func(t *testing.T) {
		t.Parallel()

		oldObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "1"}, 1)
		newObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "2"}, 1)
		assert.False(t, nodeLinkRetriggerSelfFilter{}.Update(event.UpdateEvent{
			ObjectOld: oldObj, ObjectNew: newObj,
		}))
	})

	t.Run("drops annotation removal", func(t *testing.T) {
		t.Parallel()

		oldObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "1"}, 1)
		newObj := makeMachine(map[string]string{}, 1)
		assert.False(t, nodeLinkRetriggerSelfFilter{}.Update(event.UpdateEvent{
			ObjectOld: oldObj, ObjectNew: newObj,
		}))
	})

	t.Run("keeps spec change despite annotation bump", func(t *testing.T) {
		t.Parallel()

		oldObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "1"}, 1)
		newObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "2"}, 2)
		assert.True(t, nodeLinkRetriggerSelfFilter{}.Update(event.UpdateEvent{
			ObjectOld: oldObj, ObjectNew: newObj,
		}))
	})

	t.Run("keeps other annotation change", func(t *testing.T) {
		t.Parallel()

		oldObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "1"}, 1)
		newObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "2", "other": "added"}, 1)
		assert.True(t, nodeLinkRetriggerSelfFilter{}.Update(event.UpdateEvent{
			ObjectOld: oldObj, ObjectNew: newObj,
		}))
	})

	t.Run("keeps when old is nil", func(t *testing.T) {
		t.Parallel()

		newObj := makeMachine(map[string]string{nodeLinkRetriggerAnnotation: "1"}, 1)
		assert.True(t, nodeLinkRetriggerSelfFilter{}.Update(event.UpdateEvent{
			ObjectNew: newObj,
		}))
	})
}

func TestNodeLinkRetriggerSelfFilterCreateDeleteGeneric(t *testing.T) {
	t.Parallel()

	filter := nodeLinkRetriggerSelfFilter{}
	assert.True(t, filter.Create(event.CreateEvent{}))
	assert.True(t, filter.Delete(event.DeleteEvent{}))
	assert.True(t, filter.Generic(event.GenericEvent{}))
}

// newWorkloadNode builds a workload-cluster Node named after the owning
// Machine, with an empty providerID (the host-registry default: the kubelet
// self-reports none because the IP is resolved at claim time).
//
//nolint:unparam // test builder: name always the owning Machine name by design
func newWorkloadNode(name string, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

// getInterceptor returns interceptor.Funcs whose Get fails with err.
func getInterceptor(err error) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(
			context.Context,
			ctrlclient.WithWatch,
			ctrlclient.ObjectKey,
			ctrlclient.Object,
			...ctrlclient.GetOption,
		) error {
			return err
		},
	}
}

// patchInterceptor returns interceptor.Funcs whose Patch fails with err, or
// aborts the test with msg when fatal is set.
func patchInterceptor(t *testing.T, err error, fatal bool, msg string) interceptor.Funcs {
	t.Helper()

	return interceptor.Funcs{
		Patch: func(
			context.Context,
			ctrlclient.WithWatch,
			ctrlclient.Object,
			ctrlclient.Patch,
			...ctrlclient.PatchOption,
		) error {
			if fatal {
				t.Fatal(msg)
			}

			return err
		},
	}
}

// workloadClientBuilder returns a fake client builder with the schemes needed
// to host workload-cluster Nodes (core v1 only).
func workloadClientBuilder(t *testing.T) *fake.ClientBuilder {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme)
}

func TestUpdateNodeProviderIDPatchesNodeAndSetsFlag(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")
	node := newWorkloadNode(testMachineName, "") // kubelet reported no providerID

	workload := workloadClientBuilder(t).WithObjects(node).Build()

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}
	require.NoError(t, mgmtClient.Get(t.Context(), clusterKey(byotMachine), updated))
	assert.True(t, updated.Status.NodeUpdated)

	patched := &corev1.Node{}
	require.NoError(t, workload.Get(t.Context(), ctrlclient.ObjectKey{Name: testMachineName}, patched))
	assert.Equal(t, infrav1.ProviderIDPrefix+"203.0.113.10", patched.Spec.ProviderID)
}

func TestUpdateNodeProviderIDNoopWhenNodeUpdated(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	byotMachine.Status.NodeUpdated = true // already patched once
	machine := newOwningMachine("test-bootstrap")

	calls := 0
	workload := workloadClientBuilder(t).WithObjects(newWorkloadNode(testMachineName, "")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				context.Context,
				ctrlclient.WithWatch,
				ctrlclient.ObjectKey,
				ctrlclient.Object,
				...ctrlclient.GetOption,
			) error {
				calls++

				return nil
			},
			Patch: patchInterceptor(t, nil, true, "must not patch when already NodeUpdated").Patch,
		}).Build()

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Zero(t, calls)
}

func TestUpdateNodeProviderIDNoopWhenNotAdopted(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10") // no providerID, not ready
	machine := newOwningMachine("test-bootstrap")

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return nil, errTestMustNotCall
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func TestUpdateNodeProviderIDNoopWhenNoClusterCache(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient) // getWorkloadClient nil
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}
	require.NoError(t, mgmtClient.Get(t.Context(), clusterKey(byotMachine), updated))
	assert.False(t, updated.Status.NodeUpdated)
}

func TestUpdateNodeProviderIDRequeuesWhenClusterNotConnected(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return nil, clustercache.ErrClusterNotConnected
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Equal(t, requeueAfterNodeLink, result.RequeueAfter)

	updated := &infrav1.ByotMachine{}
	require.NoError(t, mgmtClient.Get(t.Context(), clusterKey(byotMachine), updated))
	assert.False(t, updated.Status.NodeUpdated)
}

func TestUpdateNodeProviderIDRequeuesWhenNodeNotFound(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")

	workload := workloadClientBuilder(t).Build() // no Node

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Equal(t, requeueAfterNodeLink, result.RequeueAfter)
}

func TestUpdateNodeProviderIDMarksUpdatedWhenAlreadyMatching(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")
	// A prior run already patched the Node.
	node := newWorkloadNode(testMachineName, infrav1.ProviderIDPrefix+"203.0.113.10")

	patched := false
	workload := workloadClientBuilder(t).WithObjects(node).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				context.Context,
				ctrlclient.WithWatch,
				ctrlclient.Object,
				ctrlclient.Patch,
				...ctrlclient.PatchOption,
			) error {
				patched = true

				return nil
			},
		}).Build()

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	result, err := reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.False(t, patched)

	updated := &infrav1.ByotMachine{}
	require.NoError(t, mgmtClient.Get(t.Context(), clusterKey(byotMachine), updated))
	assert.True(t, updated.Status.NodeUpdated)
}

func TestUpdateNodeProviderIDReturnsErrorOnGetFailure(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")

	errGet := errTestWorkloadGet
	workload := workloadClientBuilder(t).
		WithInterceptorFuncs(getInterceptor(errGet)).Build()

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	_, err = reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.ErrorIs(t, err, errGet)
}

func TestUpdateNodeProviderIDReturnsErrorOnPatchFailure(t *testing.T) {
	t.Parallel()

	byotMachine := newAdoptedByotMachine("203.0.113.10", "hash")
	machine := newOwningMachine("test-bootstrap")
	node := newWorkloadNode(testMachineName, "")

	errPatch := errTestWorkloadPatch
	workload := workloadClientBuilder(t).WithObjects(node).
		WithInterceptorFuncs(patchInterceptor(t, errPatch, false, "")).Build()

	mgmtClient := fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(mgmtClient)
	reconciler.getWorkloadClient = func(context.Context, types.NamespacedName) (ctrlclient.Client, error) {
		return workload, nil
	}
	patchHelper, err := patch.NewHelper(byotMachine, mgmtClient)
	require.NoError(t, err)

	_, err = reconciler.updateNodeProviderID(t.Context(), patchHelper, byotMachine, machine)
	require.ErrorIs(t, err, errPatch)

	updated := &infrav1.ByotMachine{}
	require.NoError(t, mgmtClient.Get(t.Context(), clusterKey(byotMachine), updated))
	assert.False(t, updated.Status.NodeUpdated)
}
