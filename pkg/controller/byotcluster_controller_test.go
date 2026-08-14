package controller

import (
	"testing"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()

	err := infrav1.AddToScheme(scheme)
	require.NoError(t, err)

	err = corev1.AddToScheme(scheme)
	require.NoError(t, err)

	return scheme
}

func TestByotClusterReconcileSetsReady(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	byotCluster := &infrav1.ByotCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(byotCluster).
		WithStatusSubresource(byotCluster).
		Build()

	reconciler := NewByotClusterReconciler(client)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: clusterKey(byotCluster),
	})
	require.NoError(t, err)
	require.False(t, result.Requeue)
	require.Zero(t, result.RequeueAfter)

	updated := &infrav1.ByotCluster{}

	err = client.Get(t.Context(), clusterKey(byotCluster), updated)
	require.NoError(t, err)
	require.True(t, updated.Status.Ready)
}

func TestByotClusterReconcileNotFound(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	reconciler := NewByotClusterReconciler(client)

	result, err := reconciler.Reconcile(t.Context(), reconcile.Request{
		NamespacedName: objectKey("missing", "default"),
	})
	require.NoError(t, err)
	require.False(t, result.Requeue)
}
