package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// byotMachineFinalizer protects ByotMachine resources during deletion.
	byotMachineFinalizer = "byotmachine.infrastructure.cluster.x-k8s.io"

	// bootstrapSecretDataKey is the key holding the Talos machine
	// configuration in the bootstrap data secret.
	bootstrapSecretDataKey = "value"

	// requeueAfterBootstrap is the delay used while waiting for bootstrap data.
	requeueAfterBootstrap = 10 * time.Second
)

// MachineAdoptedCondition reports whether the machine configuration has been
// applied to the adopted machine.
const MachineAdoptedCondition clusterv1.ConditionType = "MachineAdopted"

// ByotMachineReconciler reconciles a ByotMachine object.
type ByotMachineReconciler struct {
	Client ctrlclient.Client
	Scheme *runtime.Scheme
}

// NewByotMachineReconciler creates a new ByotMachineReconciler.
func NewByotMachineReconciler(client ctrlclient.Client) *ByotMachineReconciler {
	return &ByotMachineReconciler{
		Client: client,
		Scheme: client.Scheme(),
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ByotMachineReconciler) SetupWithManager(
	manager ctrl.Manager,
	options ctrlcontroller.Options,
) error {
	err := ctrl.NewControllerManagedBy(manager).
		For(&infrav1.ByotMachine{}).
		WithOptions(options).
		Complete(r)
	if err != nil {
		return fmt.Errorf("failed to build ByotMachine controller: %w", err)
	}

	return nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotmachines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotmachines,verbs=create;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile adopts the Talos machine referenced by the ByotMachine.
func (r *ByotMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	byotMachine := &infrav1.ByotMachine{}

	err := r.Client.Get(ctx, req.NamespacedName, byotMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get ByotMachine %s: %w", req.NamespacedName, err)
	}

	if !byotMachine.DeletionTimestamp.IsZero() {
		// Adoption is a one-way operation in v1: nothing to undo on the host.
		// Authenticated reset back to maintenance mode is a planned follow-up.
		if controllerutil.RemoveFinalizer(byotMachine, byotMachineFinalizer) {
			err = r.Client.Update(ctx, byotMachine)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from ByotMachine %s: %w", req.NamespacedName, err)
			}
		}

		return ctrl.Result{}, nil
	}

	_, err = finalizers.EnsureFinalizer(ctx, r.Client, byotMachine, byotMachineFinalizer)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to add finalizer to ByotMachine %s: %w", req.NamespacedName, err)
	}

	if byotMachine.Status.Ready && byotMachine.Spec.ProviderID != nil {
		return ctrl.Result{}, nil
	}

	result, err := r.adopt(ctx, byotMachine)
	if err != nil {
		return result, fmt.Errorf("failed to adopt ByotMachine %s: %w", req.NamespacedName, err)
	}

	return result, nil
}

func (r *ByotMachineReconciler) adopt(ctx context.Context, byotMachine *infrav1.ByotMachine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	patchHelper, err := patch.NewHelper(byotMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create patch helper: %w", err)
	}

	machine, err := util.GetOwnerMachine(ctx, r.Client, byotMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get owner Machine: %w", err)
	}

	if machine == nil {
		logger.Info("Waiting for Machine controller to set OwnerReference", "byotMachine", byotMachine.Name)

		return ctrl.Result{RequeueAfter: requeueAfterBootstrap}, nil
	}

	machineConfig, err := r.bootstrapData(ctx, machine)
	if errors.Is(err, ErrBootstrapDataNotReady) {
		logger.Info("Waiting for bootstrap data", "byotMachine", byotMachine.Name, "machine", machine.Name)

		return ctrl.Result{RequeueAfter: requeueAfterBootstrap}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	err = applyMachineConfig(ctx, byotMachine.Spec.PublicIP, machineConfig)
	if err != nil {
		conditions.MarkFalse(
			byotMachine,
			MachineAdoptedCondition,
			"ApplyFailed",
			clusterv1.ConditionSeverityWarning,
			"%s",
			err.Error(),
		)

		patchErr := patchHelper.Patch(ctx, byotMachine)
		if patchErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch ByotMachine after apply failure: %w", patchErr)
		}

		return ctrl.Result{}, err
	}

	markAdopted(byotMachine)

	err = patchHelper.Patch(ctx, byotMachine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch ByotMachine: %w", err)
	}

	logger.Info("Machine adopted", "byotMachine", byotMachine.Name, "publicIP", byotMachine.Spec.PublicIP)

	return ctrl.Result{}, nil
}

// markAdopted updates the ByotMachine spec and status after a successful
// machine configuration apply.
func markAdopted(byotMachine *infrav1.ByotMachine) {
	providerID := infrav1.ProviderIDPrefix + byotMachine.Spec.PublicIP

	byotMachine.Spec.ProviderID = &providerID
	byotMachine.Status.Ready = true
	byotMachine.Status.Addresses = []clusterv1.MachineAddress{
		{
			Type:    clusterv1.MachineExternalIP,
			Address: byotMachine.Spec.PublicIP,
		},
	}

	conditions.MarkTrue(byotMachine, MachineAdoptedCondition)
}

// bootstrapData reads the Talos machine configuration produced by the
// bootstrap provider for the given Machine.
func (r *ByotMachineReconciler) bootstrapData(
	ctx context.Context,
	machine *clusterv1.Machine,
) ([]byte, error) {
	if machine.Spec.Bootstrap.DataSecretName == nil {
		return nil, ErrBootstrapDataNotReady
	}

	secret := &corev1.Secret{}
	secretKey := ctrlclient.ObjectKey{
		Namespace: machine.Namespace,
		Name:      *machine.Spec.Bootstrap.DataSecretName,
	}

	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrBootstrapDataNotReady
		}

		return nil, fmt.Errorf("failed to get bootstrap secret %s: %w", secretKey, err)
	}

	data, ok := secret.Data[bootstrapSecretDataKey]
	if !ok || len(data) == 0 {
		return nil, ErrBootstrapDataEmpty
	}

	return data, nil
}
