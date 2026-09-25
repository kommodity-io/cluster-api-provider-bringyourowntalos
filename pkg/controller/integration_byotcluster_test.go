package controller

import (
	"context"
	"testing"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
)

// TestIntegrationByotClusterReady exercises the full controller manager
// loop: it starts a real envtest API server, registers the ByotCluster
// reconciler, creates a ByotCluster CR, and asserts the reconciler marks
// it Ready asynchronously through the watch/cache.
func TestIntegrationByotClusterReady(t *testing.T) {
	if envtestTestEnv == nil {
		t.Skip("envtest not available")
	}

	t.Parallel()

	namespace := newNamespace(t, "itest-byotcluster-"+randomSuffix())

	_, stop := startManager(t, func(mgr ctrl.Manager) error {
		return NewByotClusterReconciler(mgr.GetClient()).SetupWithManager(mgr, ctrlcontroller.Options{})
	})
	defer stop()

	byotCluster := &infrav1.ByotCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: namespace,
		},
	}

	require.NoError(t, envtestK8sClient.Create(context.Background(), byotCluster))

	key := types.NamespacedName{Name: "test-cluster", Namespace: namespace}

	updated := &infrav1.ByotCluster{}

	require.Eventually(t, func() bool {
		err := envtestK8sClient.Get(context.Background(), key, updated)

		return err == nil && updated.Status.Ready
	}, 10*time.Second, 200*time.Millisecond, "ByotCluster should be marked Ready by reconciler")
}
