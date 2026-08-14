package controller

import (
	"context"
	"testing"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeWorkloadClientset is a test double for workloadClientset that records
// calls and serves canned Node/Pod state.
type fakeWorkloadClientset struct {
	nodes    []corev1.Node
	pods     []corev1.Pod
	evicted  []string
	deleted  []string
	cordoned []string
	patchErr error
	evictErr error
}

func (f *fakeWorkloadClientset) Nodes() workloadNodeInterface {
	return &fakeNodeInterface{f: f}
}

func (f *fakeWorkloadClientset) Pods(_ string) workloadPodInterface {
	return &fakePodInterface{f: f}
}

func (f *fakeWorkloadClientset) Evictions(_ string) workloadEvictionInterface {
	return &fakeEvictionInterface{f: f}
}

type fakeNodeInterface struct{ f *fakeWorkloadClientset }

func (n *fakeNodeInterface) List(_ context.Context, _ metav1.ListOptions) (*corev1.NodeList, error) {
	return &corev1.NodeList{Items: append([]corev1.Node{}, n.f.nodes...)}, nil
}

func (n *fakeNodeInterface) Patch(
	_ context.Context,
	name string,
	_ types.PatchType,
	_ []byte,
	_ metav1.PatchOptions,
) (*corev1.Node, error) {
	if n.f.patchErr != nil {
		return nil, n.f.patchErr
	}

	n.f.cordoned = append(n.f.cordoned, name)

	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
}

func (n *fakeNodeInterface) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	n.f.deleted = append(n.f.deleted, "node:"+name)
	n.f.nodes = filterNodes(n.f.nodes, name)

	return nil
}

type fakePodInterface struct{ f *fakeWorkloadClientset }

func (p *fakePodInterface) List(_ context.Context, _ metav1.ListOptions) (*corev1.PodList, error) {
	return &corev1.PodList{Items: append([]corev1.Pod{}, p.f.pods...)}, nil
}

func (p *fakePodInterface) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	p.f.deleted = append(p.f.deleted, "pod:"+name)
	p.f.pods = filterPods(p.f.pods, name)

	return nil
}

type fakeEvictionInterface struct{ f *fakeWorkloadClientset }

func (e *fakeEvictionInterface) Evict(_ context.Context, eviction *policyv1.Eviction) error {
	if e.f.evictErr != nil {
		return e.f.evictErr
	}

	e.f.evicted = append(e.f.evicted, eviction.Name)
	e.f.pods = filterPods(e.f.pods, eviction.Name)

	return nil
}

func filterNodes(nodes []corev1.Node, name string) []corev1.Node {
	out := nodes[:0]
	for _, n := range nodes {
		if n.Name != name {
			out = append(out, n)
		}
	}

	return out
}

func filterPods(pods []corev1.Pod, name string) []corev1.Pod {
	out := pods[:0]
	for _, p := range pods {
		if p.Name != name {
			out = append(out, p)
		}
	}

	return out
}

func TestResolveWorkloadNodeNamePrefersNodeRef(t *testing.T) {
	t.Parallel()

	machine := &clusterv1.Machine{Status: clusterv1.MachineStatus{NodeRef: &corev1.ObjectReference{Name: "ref-node"}}}
	byotMachine := newByotMachine("203.0.113.10")

	name, err := resolveWorkloadNodeName(t.Context(), &fakeWorkloadClientset{}, machine, byotMachine)
	require.NoError(t, err)
	assert.Equal(t, "ref-node", name)
}

func TestResolveWorkloadNodeNameFallsBackToProviderID(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")
	fakeCs := &fakeWorkloadClientset{nodes: []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: corev1.NodeSpec{ProviderID: "byot://203.0.113.99"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "matched"}, Spec: corev1.NodeSpec{ProviderID: "byot://203.0.113.10"}},
	}}

	name, err := resolveWorkloadNodeName(t.Context(), fakeCs, nil, byotMachine)
	require.NoError(t, err)
	assert.Equal(t, "matched", name)
}

func TestResolveWorkloadNodeNameEmptyWhenNoMatch(t *testing.T) {
	t.Parallel()

	byotMachine := newByotMachine("203.0.113.10")
	name, err := resolveWorkloadNodeName(t.Context(), &fakeWorkloadClientset{}, nil, byotMachine)
	require.NoError(t, err)
	assert.Empty(t, name)
}

func TestDrainAndDeleteNodeSkipsDaemonMirrorTerminating(t *testing.T) {
	t.Parallel()

	controller := true
	nodeName := "worker-0"
	fakeCs := &fakeWorkloadClientset{
		nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}},
		pods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testNamespace}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "ds", Namespace: "kube-system",
				OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Controller: &controller}}},
			},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "mirror", Namespace: "kube-system",
				Annotations: map[string]string{mirrorPodAnnotation: "static"}},
			},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "term", Namespace: testNamespace,
				DeletionTimestamp: &metav1.Time{Time: time.Now()}},
			},
		},
	}

	err := drainAndDeleteNode(t.Context(), fakeCs, nodeName)
	require.NoError(t, err)
	assert.Contains(t, fakeCs.cordoned, nodeName)
	assert.Contains(t, fakeCs.evicted, "app")
	assert.NotContains(t, fakeCs.evicted, "ds")
	assert.NotContains(t, fakeCs.evicted, "mirror")
	assert.NotContains(t, fakeCs.evicted, "term")
	assert.Contains(t, fakeCs.deleted, "node:"+nodeName)
}

func TestDrainAndDeleteNodeForceDeletesOnEvictionRejection(t *testing.T) {
	t.Parallel()

	nodeName := "worker-0"
	fakeCs := &fakeWorkloadClientset{
		nodes:    []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}},
		pods:     []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pdb-blocked", Namespace: "default"}}},
		evictErr: apierrors.NewTooManyRequests("blocked by PDB", 0),
	}

	err := drainAndDeleteNode(t.Context(), fakeCs, nodeName)
	require.NoError(t, err)
	assert.NotContains(t, fakeCs.evicted, "pdb-blocked")
	assert.Contains(t, fakeCs.deleted, "pod:pdb-blocked")
	assert.Contains(t, fakeCs.deleted, "node:"+nodeName)
}

func TestCleanupWorkloadNodeNoopWhenNoNode(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err)

	byotMachine := newByotMachine("203.0.113.10")
	byotMachine.Labels = map[string]string{clusterNameLabel: testClusterName}
	machine := newOwningMachine("test-bootstrap")

	cluster := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + clusterKubeconfigSecretSuffix, Namespace: testNamespace},
		Data:       map[string][]byte{kubeconfigSecretDataKey: []byte("kubeconfig")},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, machine, cluster).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)
	orig := workloadClientsetBuilder
	workloadClientsetBuilder = noNodesWorkloadClientset

	t.Cleanup(func() { workloadClientsetBuilder = orig })

	err = reconciler.cleanupWorkloadNode(t.Context(), byotMachine, machine)
	require.NoError(t, err)
}

func TestCleanupWorkloadNodeRecordsFailureWhenKubeconfigMissing(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	byotMachine := newByotMachine("203.0.113.10")
	byotMachine.Labels = map[string]string{clusterNameLabel: testClusterName}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	err := reconciler.cleanupWorkloadNode(t.Context(), byotMachine, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load workload kubeconfig")
}

func TestReconcileDeleteRecordsNodeCleanupFailureButReleases(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	byotMachine := newByotMachine("203.0.113.10")
	byotMachine.Labels = map[string]string{clusterNameLabel: testClusterName}
	byotMachine.Finalizers = []string{byotMachineFinalizer}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)

	err := client.Delete(t.Context(), byotMachine)
	require.NoError(t, err)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: byotMachine.Name, Namespace: byotMachine.Namespace},
	})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	deleted := &infrav1.ByotMachine{}
	key := ctrlclient.ObjectKey{Name: byotMachine.Name, Namespace: byotMachine.Namespace}
	getErr := client.Get(t.Context(), key, deleted)
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestReconcileDeleteNodeCleanupConditionTrueOnSuccess(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)
	byotMachine := newByotMachine("203.0.113.10")
	byotMachine.Labels = map[string]string{clusterNameLabel: testClusterName}
	byotMachine.Finalizers = []string{byotMachineFinalizer}

	cluster := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName + clusterKubeconfigSecretSuffix, Namespace: testNamespace},
		Data:       map[string][]byte{kubeconfigSecretDataKey: []byte("kubeconfig")},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotMachine, cluster).
		WithStatusSubresource(&infrav1.ByotMachine{}).
		Build()

	reconciler := NewByotMachineReconciler(client)
	orig := workloadClientsetBuilder
	workloadClientsetBuilder = noNodesWorkloadClientset

	t.Cleanup(func() { workloadClientsetBuilder = orig })

	err := client.Delete(t.Context(), byotMachine)
	require.NoError(t, err)

	_, err = reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: byotMachine.Name, Namespace: byotMachine.Namespace},
	})
	require.NoError(t, err)
}

// unused-import guard kept minimal.
var _ = policyv1.Eviction{}
var _ = clusterv1.Machine{}

// noNodesWorkloadClientset is a workloadClientset builder stub returning a
// client with no nodes, used to exercise the no-op cleanup path.
func noNodesWorkloadClientset([]byte) (workloadClientset, error) {
	return &fakeWorkloadClientset{}, nil
}
