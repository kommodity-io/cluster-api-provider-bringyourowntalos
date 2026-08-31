package controller

import (
	"context"
	"errors"
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testHostNamespace = "default"

// errTestUnreachable is a static discovery failure used to simulate an
// unreachable host in tests.
var errTestUnreachable = errors.New("unreachable")

//nolint:unparam // test builder: IP fixed for readability, documents intent
func newByotHost(publicIP string) *infrav1.ByotHost {
	return &infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testHostName,
			Namespace: testHostNamespace,
		},
		Spec: infrav1.ByotHostSpec{PublicIP: publicIP},
	}
}

type discoverFunc func(context.Context, string) (DiscoveryResult, error)
type probeFunc func(context.Context, string) bool

func newHostReconciler(
	t *testing.T,
	clientBuilder *fake.ClientBuilder,
	discover discoverFunc,
	probe probeFunc,
) *ByotHostReconciler {
	t.Helper()

	client := clientBuilder.
		WithStatusSubresource(&infrav1.ByotHost{}).
		Build()

	reconciler := &ByotHostReconciler{
		Client:           client,
		Scheme:           client.Scheme(),
		Discover:         discover,
		ProbeMaintenance: probe,
		LivenessInterval: hostLivenessInterval,
		FailureThreshold: hostFailureThreshold,
	}

	return reconciler
}

func fakeDiscovery() DiscoveryResult {
	return DiscoveryResult{
		TalosVersion: "v1.13.8",
		Arch:         "amd64",
		Platform:     "scaleway",
		CPU:          infrav1.HostCPU{Cores: 4, Packages: 1, NumaNodes: 1},
		Memory:       resource.MustParse("8Gi"),
		Disks: []infrav1.HostDisk{
			{Name: "/dev/sda", Size: resource.MustParse("100Gi"), Type: "SSD", SystemDisk: true},
		},
		NetworkInterfaces: []string{"eth0"},
	}
}

func TestByotHostReconcileDiscoveryPopulatesStatusAndLabels(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return true },
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostLivenessInterval, result.RequeueAfter)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))

	assert.Equal(t, infrav1.HostPhaseAvailable, updated.Status.Phase)
	assert.Equal(t, "v1.13.8", updated.Status.TalosVersion)
	assert.Equal(t, "amd64", updated.Status.Arch)
	assert.Equal(t, "scaleway", updated.Status.Platform)
	require.NotNil(t, updated.Status.Hardware)
	assert.Equal(t, int32(4), updated.Status.Hardware.CPU.Cores)
	assert.True(t, conditions.IsTrue(updated, infrav1.HostDiscoveredCondition))

	// Curated labels promoted.
	assert.Equal(t, "true", updated.Labels[labelAvailable])
	assert.Equal(t, "4", updated.Labels[labelCPUCores])
	assert.Equal(t, "amd64", updated.Labels[labelCPUArch])
	assert.Equal(t, "8G", updated.Labels[labelMemoryClass])
	assert.Equal(t, "ssd", updated.Labels[labelDiskType])
	assert.Equal(t, "100G", updated.Labels[labelDiskClass])
	assert.Contains(t, updated.Finalizers, byotHostFinalizer)
}

func TestByotHostReconcileDiscoveryFailureStaysProbing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) {
			return DiscoveryResult{}, errTestUnreachable
		},
		func(context.Context, string) bool { return false },
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostProbeRequeue, result.RequeueAfter)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseProbing, updated.Status.Phase)
	assert.False(t, conditions.IsTrue(updated, infrav1.HostMaintenanceProbeCondition))
}

func TestByotHostReconcileAvailableFlipsToUnavailableAfterThreshold(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseAvailable

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return false },
	)

	// First failure: below threshold -> ProbeFailed info, stays Available.
	_, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseAvailable, updated.Status.Phase)
	assert.Equal(t, int32(1), updated.Status.ProbeFailureCount)

	// Two more failures -> threshold reached -> Unavailable.
	_, err = reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)

	_, err = reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)

	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseUnavailable, updated.Status.Phase)
	assert.False(t, conditions.IsTrue(updated, infrav1.HostMaintenanceProbeCondition))
	assert.Equal(t, "MaintenanceProbeFailed", conditions.Get(updated, infrav1.HostMaintenanceProbeCondition).Reason)
}

func TestByotHostReconcileUnavailableRecoversToAvailableWithRediscovery(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseUnavailable
	host.Status.MaintenanceMode = false

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return true },
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostLivenessInterval, result.RequeueAfter)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseAvailable, updated.Status.Phase)
	assert.True(t, conditions.IsTrue(updated, infrav1.HostDiscoveredCondition))
	assert.Equal(t, "v1.13.8", updated.Status.TalosVersion) // re-discovered
}

func TestByotHostReconcileReleasingFlipsToAvailableWhenMaintenanceAnswers(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseReleasing

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return true },
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostLivenessInterval, result.RequeueAfter)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseAvailable, updated.Status.Phase)
	assert.Nil(t, updated.Status.ClaimRef)
}

func TestByotHostReconcileReleasingRequeuesWhenStillDown(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseReleasing

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return false },
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostProbeRequeue, result.RequeueAfter)

	updated := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), updated))
	assert.Equal(t, infrav1.HostPhaseReleasing, updated.Status.Phase)
}

func TestByotHostReconcileClaimedRequeuesWithoutProbing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Status.Phase = infrav1.HostPhaseClaimed

	probeCalled := false
	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) {
			t.Fatal("discover must not run while Claimed")

			return DiscoveryResult{}, nil
		},
		func(context.Context, string) bool {
			probeCalled = true

			return true
		},
	)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostClaimedRequeue, result.RequeueAfter)
	assert.False(t, probeCalled)
}

func TestByotHostReconcileDeleteBlockedWhileClaimed(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Finalizers = []string{byotHostFinalizer}
	host.Status.ClaimRef = &infrav1.HostClaimRef{Name: "some-machine", Namespace: "default", UID: "claim-uid"}

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return true },
	)

	require.NoError(t, reconciler.Client.Delete(t.Context(), host))

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Equal(t, hostProbeRequeue, result.RequeueAfter)

	// Object still exists (deletion blocked), finalizer retained.
	preserved := &infrav1.ByotHost{}
	require.NoError(t, reconciler.Client.Get(t.Context(), clusterKey(host), preserved))
	assert.Contains(t, preserved.Finalizers, byotHostFinalizer)
}

func TestByotHostReconcileDeleteCompletesWhenUnclaimed(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	require.NoError(t, infrav1.AddToScheme(scheme))

	host := newByotHost("203.0.113.10")
	host.Finalizers = []string{byotHostFinalizer}

	reconciler := newHostReconciler(t,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(host),
		func(context.Context, string) (DiscoveryResult, error) { return fakeDiscovery(), nil },
		func(context.Context, string) bool { return true },
	)

	require.NoError(t, reconciler.Client.Delete(t.Context(), host))

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: clusterKey(host)})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	// Finalizer removed, object gone.
	updated := &infrav1.ByotHost{}
	err = reconciler.Client.Get(t.Context(), clusterKey(host), updated)
	require.Error(t, err) // not found
}

func TestFilterClaimCandidatesKeepsAvailableUnclaimedMatchingFailureDomain(t *testing.T) {
	t.Parallel()

	failureDomain := "par01"
	otherDomain := "par02"

	available := infrav1.HostPhaseAvailable
	claimed := infrav1.HostPhaseClaimed

	hosts := []infrav1.ByotHost{
		hostCandidate("a", &failureDomain, available, ""),
		hostCandidate("b", &otherDomain, available, ""),
		hostCandidate("c", &failureDomain, claimed, ""),
		hostCandidate("d", &failureDomain, available, "other"),
	}

	byotMachine := &infrav1.ByotMachine{Spec: infrav1.ByotMachineSpec{FailureDomain: &failureDomain}}
	byotMachine.UID = "self"

	got := filterClaimCandidates(hosts, byotMachine)
	require.Len(t, got, 1)
	assert.Equal(t, "a", got[0].Name)
}

func TestFilterClaimCandidatesNoFailureDomainMatchesAllAvailable(t *testing.T) {
	t.Parallel()

	available := infrav1.HostPhaseAvailable
	claimed := infrav1.HostPhaseClaimed

	hosts := []infrav1.ByotHost{
		hostCandidate("a", nil, available, ""),
		hostCandidate("b", nil, available, ""),
		hostCandidate("c", nil, claimed, ""),
	}

	byotMachine := &infrav1.ByotMachine{}

	got := filterClaimCandidates(hosts, byotMachine)
	assert.Len(t, got, 2)
}

func TestFilterClaimCandidatesExcludesAvailableHostMidProbeFailure(t *testing.T) {
	t.Parallel()

	// An Available host whose last liveness probe failed (MaintenanceProbe=False,
	// below the failure threshold so phase has not flipped to Unavailable) is not
	// claimable: Decision 5 requires liveness confirmed.
	available := infrav1.HostPhaseAvailable
	host := hostCandidate("a", nil, available, "")
	conditions.MarkFalse(&host, infrav1.HostMaintenanceProbeCondition,
		"ProbeFailed", clusterv1.ConditionSeverityInfo, "transient")

	got := filterClaimCandidates([]infrav1.ByotHost{host}, &infrav1.ByotMachine{})
	assert.Empty(t, got)
}

// hostCandidate builds a ByotHost for filterClaimCandidates tests. An
// Available host is marked maintenance-probe-true so it is claimable.
func hostCandidate(name string, fd *string, phase infrav1.HostPhase, claimUID string) infrav1.ByotHost {
	host := infrav1.ByotHost{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       infrav1.ByotHostSpec{FailureDomain: fd},
		Status:     infrav1.ByotHostStatus{Phase: phase},
	}

	if phase == infrav1.HostPhaseAvailable {
		conditions.MarkTrue(&host, infrav1.HostMaintenanceProbeCondition)
	}

	if claimUID != "" {
		host.Status.ClaimRef = &infrav1.HostClaimRef{UID: claimUID}
	}

	return host
}
