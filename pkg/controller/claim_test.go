package controller

import (
	"context"
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func newClaimingByotMachine(hostRef string) *infrav1.ByotMachine {
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
			HostRef: &infrav1.LocalObjectReference{Name: hostRef},
		},
	}
}

func newAvailableByotHost(name string, publicIP string) *infrav1.ByotHost {
	host := &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec:   infrav1.ByotHostSpec{PublicIP: publicIP},
		Status: infrav1.ByotHostStatus{Phase: infrav1.HostPhaseAvailable},
	}
	conditions.MarkTrue(host, infrav1.HostMaintenanceProbeCondition)

	return host
}

func TestClaimHostRequeuesWhenHostUnavailable(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := newClaimingByotMachine("missing-host")
	host := newAvailableByotHost("missing-host", "203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseUnavailable

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, host).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)

	assert.Empty(t, machine.Status.ResolvedHost)
}

func TestClaimHostRequeuesWhenHostMissing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := newClaimingByotMachine("no-such-host")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)
}

func TestClaimHostClaimsAvailableHost(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := newClaimingByotMachine(testHostName)
	host := newAvailableByotHost(testHostName, "203.0.113.10")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, host).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, result.RequeueAfter)

	assert.Equal(t, testHostName, machine.Status.ResolvedHost)
	assert.Equal(t, "203.0.113.10", machine.Status.ResolvedPublicIP)

	// Host is now Claimed with claimRef pointing at the ByotMachine.
	updated := &infrav1.ByotHost{}
	require.NoError(t, client.Get(t.Context(),
		reconcile.Request{NamespacedName: clusterKey(host)}.NamespacedName, updated))
	assert.Equal(t, infrav1.HostPhaseClaimed, updated.Status.Phase)
	require.NotNil(t, updated.Status.ClaimRef)
	assert.Equal(t, testMachineName, updated.Status.ClaimRef.Name)
	assert.Equal(t, testMachineUID, updated.Status.ClaimRef.UID)
	assert.Contains(t, updated.Finalizers, byotHostFinalizer)
}

func TestClaimHostIdempotentWhenAlreadyClaimed(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := newClaimingByotMachine(testHostName)
	machine.Status.ResolvedHost = testHostName
	machine.Status.ResolvedPublicIP = "203.0.113.10"

	host := newAvailableByotHost(testHostName, "203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseClaimed
	host.Status.ClaimRef = &infrav1.HostClaimRef{
		Kind: "ByotMachine", Name: testMachineName, Namespace: testNamespace, UID: testMachineUID,
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, host).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, result.RequeueAfter)
}

func TestClaimHostSelectorPicksFirstAvailableByLabel(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := &infrav1.ByotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: testMachineName, Namespace: testNamespace, UID: testMachineUID,
			Labels: map[string]string{clusterv1.ClusterNameLabel: testClusterName},
		},
		Spec: infrav1.ByotMachineSpec{
			HostSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelAvailable: "true"},
			},
		},
	}

	hostA := newAvailableByotHost("host-a", "203.0.113.1")
	hostA.Labels = map[string]string{labelAvailable: "true"}
	hostB := newAvailableByotHost("host-b", "203.0.113.2")
	hostB.Labels = map[string]string{labelAvailable: "true"}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, hostA, hostB).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, result.RequeueAfter)

	// First by name = host-a.
	assert.Equal(t, "host-a", machine.Status.ResolvedHost)
	assert.Equal(t, "203.0.113.1", machine.Status.ResolvedPublicIP)
}

func TestClaimHostSelectorRequeuesWhenNoneAvailable(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	machine := &infrav1.ByotMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: testMachineName, Namespace: testNamespace, UID: testMachineUID,
		},
		Spec: infrav1.ByotMachineSpec{
			HostSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelAvailable: "true"},
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, requeueAfterBootstrap, result.RequeueAfter)
}

func TestClaimHostLostClaimReclaims(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	// ByotMachine thinks it claimed a host, but the host has been released
	// (claimRef cleared, phase Available). claimHost must re-claim it.
	machine := newClaimingByotMachine(testHostName)
	machine.Status.ResolvedHost = testHostName
	machine.Status.ResolvedPublicIP = "203.0.113.10"

	host := newAvailableByotHost(testHostName, "203.0.113.10") // Available, no claimRef

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, host).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Zero(t, result.RequeueAfter)

	assert.Equal(t, testHostName, machine.Status.ResolvedHost)
}

func TestClaimHostLostRaceRequeuesOnConflict(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, clusterv1.AddToScheme(scheme))

	// Two ByotMachines race one Available host; this one loses the
	// compare-and-swap (Status().Update conflicts because another writer set
	// claimRef first). claimHost must swallow the conflict and requeue for a
	// retry rather than erroring.
	machine := newClaimingByotMachine(testHostName)
	host := newAvailableByotHost(testHostName, "203.0.113.10")

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, host).
		WithStatusSubresource(&infrav1.ByotMachine{}, &infrav1.ByotHost{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				_ context.Context,
				_ ctrlclient.Client,
				_ string,
				_ ctrlclient.Object,
				_ ...ctrlclient.SubResourceUpdateOption,
			) error {
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "byothosts"},
					host.Name,
					nil,
				)
			},
		}).
		Build()

	r := NewByotMachineReconciler(client)
	patchHelper, err := patch.NewHelper(machine, client)
	require.NoError(t, err)

	result, handled, err := r.claimHost(t.Context(), machine, patchHelper)
	require.NoError(t, err) // conflict is swallowed
	assert.True(t, handled)
	assert.True(t, result.Requeue)
	assert.Zero(t, result.RequeueAfter)
	assert.Empty(t, machine.Status.ResolvedHost) // claim not finalized
}
