// Package controller contains the reconcilers for the byot infrastructure provider.
package controller

import (
	"context"
	"fmt"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ByotClusterReconciler reconciles a ByotCluster object. Adopted clusters need
// no infrastructure provisioning, so the reconciler only reports readiness.
type ByotClusterReconciler struct {
	Client ctrlclient.Client
	Scheme *runtime.Scheme
}

// NewByotClusterReconciler creates a new ByotClusterReconciler.
func NewByotClusterReconciler(client ctrlclient.Client) *ByotClusterReconciler {
	return &ByotClusterReconciler{
		Client: client,
		Scheme: client.Scheme(),
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ByotClusterReconciler) SetupWithManager(
	manager ctrl.Manager,
	options ctrlcontroller.Options,
) error {
	err := ctrl.NewControllerManagedBy(manager).
		For(&infrav1.ByotCluster{}).
		WithOptions(options).
		Complete(r)
	if err != nil {
		return fmt.Errorf("failed to build ByotCluster controller: %w", err)
	}

	return nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotclusters/status,verbs=get;update;patch

// Reconcile marks the ByotCluster ready.
func (r *ByotClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	byotCluster := &infrav1.ByotCluster{}

	err := r.Client.Get(ctx, req.NamespacedName, byotCluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get ByotCluster %s: %w", req.NamespacedName, err)
	}

	if !byotCluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if byotCluster.Status.Ready {
		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(byotCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create patch helper for ByotCluster %s: %w", req.NamespacedName, err)
	}

	byotCluster.Status.Ready = true

	err = patchHelper.Patch(ctx, byotCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch ByotCluster %s: %w", req.NamespacedName, err)
	}

	logger.Info("ByotCluster is ready", "byotCluster", req.NamespacedName)

	return ctrl.Result{}, nil
}
