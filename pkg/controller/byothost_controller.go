package controller

import (
	"context"
	"fmt"
	"time"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
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
	// byotHostFinalizer blocks ByotHost deletion while a ByotMachine has
	// claimed it (status.claimRef set). Delete the owning ByotMachine first.
	byotHostFinalizer = "byot-host-protection"

	// hostLivenessInterval is the default requeue delay between liveness
	// probes for an Available host.
	hostLivenessInterval = 60 * time.Second

	// hostProbeRequeue is the requeue delay while probing, recovering, or
	// failing (shorter than the steady-state interval).
	hostProbeRequeue = 15 * time.Second

	// hostClaimedRequeue is the requeue delay while a host is Claimed (probing
	// paused); just long enough to notice a release.
	hostClaimedRequeue = 5 * time.Minute

	// hostFailureThreshold is the default consecutive probe failures before a
	// host is marked Unavailable.
	hostFailureThreshold = 3
)

// ByotHostReconciler reconciles a ByotHost: it discovers hardware features
// from the Talos maintenance API, probes liveness periodically, promotes
// curated bucketed labels, and exposes a claimRef the ByotMachine controller
// CAS-claims. It drives the phase state machine Probing → Available ↔
// Unavailable and Releasing → Available; the ByotMachine controller drives
// Available → Claimed and Claimed → Releasing.
type ByotHostReconciler struct {
	Client ctrlclient.Client
	Scheme *runtime.Scheme

	// Discover runs the maintenance discovery surface. Defaults to
	// discoverHost; injectable for tests.
	Discover func(context.Context, string) (DiscoveryResult, error)

	// ProbeMaintenance reports maintenance liveness. Defaults to
	// probeMaintenance; injectable for tests.
	ProbeMaintenance func(context.Context, string) bool

	// LivenessInterval overrides the steady-state probe requeue delay.
	LivenessInterval time.Duration

	// FailureThreshold overrides the consecutive-failure count that flips a
	// host to Unavailable.
	FailureThreshold int32
}

// NewByotHostReconciler creates a new ByotHostReconciler with default probes.
func NewByotHostReconciler(client ctrlclient.Client) *ByotHostReconciler {
	return &ByotHostReconciler{
		Client:           client,
		Scheme:           client.Scheme(),
		Discover:         discoverHost,
		ProbeMaintenance: probeMaintenance,
		LivenessInterval: hostLivenessInterval,
		FailureThreshold: hostFailureThreshold,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ByotHostReconciler) SetupWithManager(
	manager ctrl.Manager,
	options ctrlcontroller.Options,
) error {
	err := ctrl.NewControllerManagedBy(manager).
		For(&infrav1.ByotHost{}).
		WithOptions(options).
		Complete(r)
	if err != nil {
		return fmt.Errorf("failed to build ByotHost controller: %w", err)
	}

	return nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byothosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byothosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byothosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=byotmachines,verbs=get;list;watch

// Reconcile drives the ByotHost discovery + liveness state machine.
func (r *ByotHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &infrav1.ByotHost{}

	err := r.Client.Get(ctx, req.NamespacedName, host)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get ByotHost %s: %w", req.NamespacedName, err)
	}

	if !host.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, host)
	}

	if _, err := finalizers.EnsureFinalizer(ctx, r.Client, host, byotHostFinalizer); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to add finalizer to ByotHost %s: %w", req.NamespacedName, err)
	}

	patchHelper, err := patch.NewHelper(host, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create patch helper for ByotHost %s: %w", req.NamespacedName, err)
	}

	return r.reconcileHost(ctx, patchHelper, host)
}

// reconcileDelete blocks deletion while a host is claimed; once the claim is
// cleared (by deleting the owning ByotMachine, which releases + clears
// claimRef), the finalizer is removed.
func (r *ByotHostReconciler) reconcileDelete(ctx context.Context, host *infrav1.ByotHost) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(host, byotHostFinalizer) {
		return ctrl.Result{}, nil
	}

	if host.Status.ClaimRef != nil {
		// Still claimed: keep the host until the owning ByotMachine is
		// deleted and releases it.
		return ctrl.Result{RequeueAfter: hostProbeRequeue}, nil
	}

	if controllerutil.RemoveFinalizer(host, byotHostFinalizer) {
		if err := r.Client.Update(ctx, host); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from ByotHost %s: %w", host.Name, err)
		}
	}

	return ctrl.Result{}, nil
}

// reconcileHost runs the phase state machine.
func (r *ByotHostReconciler) reconcileHost(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	switch host.Status.Phase {
	case "", infrav1.HostPhaseProbing:
		return r.reconcileProbing(ctx, patchHelper, host)
	case infrav1.HostPhaseAvailable:
		return r.reconcileAvailable(ctx, patchHelper, host)
	case infrav1.HostPhaseUnavailable:
		return r.reconcileUnavailable(ctx, patchHelper, host)
	case infrav1.HostPhaseClaimed:
		// Probing paused while the host is being adopted. The ByotMachine
		// controller sets phase=Releasing and clears claimRef on release.
		return ctrl.Result{RequeueAfter: hostClaimedRequeue}, nil
	case infrav1.HostPhaseReleasing:
		return r.reconcileReleasing(ctx, patchHelper, host)
	default:
		return r.reconcileProbing(ctx, patchHelper, host)
	}
}

// reconcileProbing runs initial discovery. On success the host becomes
// Available; on failure it stays Probing and requeues.
func (r *ByotHostReconciler) reconcileProbing(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	result, err := r.Discover(ctx, host.Spec.PublicIP)
	if err != nil {
		return r.recordDiscoveryFailure(ctx, patchHelper, host, err)
	}

	r.populateFromDiscovery(host, result)
	host.Status.Phase = infrav1.HostPhaseAvailable
	host.Status.MaintenanceMode = true
	host.Status.ProbeFailureCount = 0
	conditions.MarkTrue(host, infrav1.HostMaintenanceProbeCondition)
	applyDiscoveryLabels(host)

	if err := r.patchHost(ctx, patchHelper, host); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("ByotHost discovered and available",
		"byotHost", host.Name, "publicIP", host.Spec.PublicIP,
		"talosVersion", result.TalosVersion, "arch", result.Arch)

	return ctrl.Result{RequeueAfter: r.livenessInterval()}, nil
}

// reconcileAvailable probes maintenance liveness. On consecutive failures it
// flips to Unavailable; on success it stays Available.
func (r *ByotHostReconciler) reconcileAvailable(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	if r.ProbeMaintenance(ctx, host.Spec.PublicIP) {
		host.Status.MaintenanceMode = true
		host.Status.LastProbedAt = nowPtr()
		host.Status.ProbeFailureCount = 0
		conditions.MarkTrue(host, infrav1.HostMaintenanceProbeCondition)
		applyDiscoveryLabels(host)

		if err := r.patchHost(ctx, patchHelper, host); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: r.livenessInterval()}, nil
	}

	return r.recordProbeFailure(ctx, patchHelper, host)
}

// reconcileUnavailable probes recovery. On success it re-discovers and flips
// to Available; on failure it stays Unavailable.
func (r *ByotHostReconciler) reconcileUnavailable(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	if !r.ProbeMaintenance(ctx, host.Spec.PublicIP) {
		host.Status.LastProbedAt = nowPtr()
		applyDiscoveryLabels(host)

		if err := r.patchHost(ctx, patchHelper, host); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: hostProbeRequeue}, nil
	}

	// Recovered: re-discover features (they may have changed after an outage).
	result, err := r.Discover(ctx, host.Spec.PublicIP)
	if err != nil {
		return r.recordDiscoveryFailure(ctx, patchHelper, host, err)
	}

	r.populateFromDiscovery(host, result)
	host.Status.Phase = infrav1.HostPhaseAvailable
	host.Status.MaintenanceMode = true
	host.Status.ProbeFailureCount = 0
	conditions.MarkTrue(host, infrav1.HostMaintenanceProbeCondition)
	applyDiscoveryLabels(host)

	if err := r.patchHost(ctx, patchHelper, host); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("ByotHost recovered and available",
		"byotHost", host.Name, "publicIP", host.Spec.PublicIP)

	return ctrl.Result{RequeueAfter: r.livenessInterval()}, nil
}

// reconcileReleasing waits for the host to come back in maintenance after a
// release reset, then re-discovers and flips to Available.
func (r *ByotHostReconciler) reconcileReleasing(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	if !r.ProbeMaintenance(ctx, host.Spec.PublicIP) {
		host.Status.LastProbedAt = nowPtr()
		applyDiscoveryLabels(host)

		if err := r.patchHost(ctx, patchHelper, host); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: hostProbeRequeue}, nil
	}

	result, err := r.Discover(ctx, host.Spec.PublicIP)
	if err != nil {
		return r.recordDiscoveryFailure(ctx, patchHelper, host, err)
	}

	r.populateFromDiscovery(host, result)
	host.Status.Phase = infrav1.HostPhaseAvailable
	host.Status.ClaimRef = nil
	host.Status.MaintenanceMode = true
	host.Status.ProbeFailureCount = 0
	conditions.MarkTrue(host, infrav1.HostMaintenanceProbeCondition)
	applyDiscoveryLabels(host)

	if err := r.patchHost(ctx, patchHelper, host); err != nil {
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("ByotHost released and available again",
		"byotHost", host.Name, "publicIP", host.Spec.PublicIP)

	return ctrl.Result{RequeueAfter: r.livenessInterval()}, nil
}

// recordDiscoveryFailure marks discovery/probe failed and keeps the host
// Probing (initial) or Unavailable (recovery), requeueing shortly.
func (r *ByotHostReconciler) recordDiscoveryFailure(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
	discoverErr error,
) (ctrl.Result, error) {
	host.Status.MaintenanceMode = false
	host.Status.LastProbedAt = nowPtr()
	host.Status.Phase = infrav1.HostPhaseProbing

	conditions.MarkFalse(
		host,
		infrav1.HostMaintenanceProbeCondition,
		"MaintenanceProbeFailed",
		clusterv1.ConditionSeverityWarning,
		"%s",
		discoverErr.Error(),
	)
	conditions.MarkFalse(
		host,
		infrav1.HostDiscoveredCondition,
		"DiscoveryFailed",
		clusterv1.ConditionSeverityWarning,
		"%s",
		discoverErr.Error(),
	)

	applyDiscoveryLabels(host)

	if err := r.patchHost(ctx, patchHelper, host); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: hostProbeRequeue}, nil
}

// recordProbeFailure increments the consecutive failure count and flips the
// host to Unavailable once the threshold is reached.
func (r *ByotHostReconciler) recordProbeFailure(
	ctx context.Context,
	patchHelper *patch.Helper,
	host *infrav1.ByotHost,
) (ctrl.Result, error) {
	host.Status.MaintenanceMode = false
	host.Status.LastProbedAt = nowPtr()
	host.Status.ProbeFailureCount++

	if host.Status.ProbeFailureCount >= r.failureThreshold() {
		host.Status.Phase = infrav1.HostPhaseUnavailable
		conditions.MarkFalse(
			host,
			infrav1.HostMaintenanceProbeCondition,
			"MaintenanceProbeFailed",
			clusterv1.ConditionSeverityWarning,
			"maintenance probe failed %d consecutive times",
			host.Status.ProbeFailureCount,
		)
	} else {
		conditions.MarkFalse(
			host,
			infrav1.HostMaintenanceProbeCondition,
			"ProbeFailed",
			clusterv1.ConditionSeverityInfo,
			"maintenance probe failed %d consecutive times",
			host.Status.ProbeFailureCount,
		)
	}

	applyDiscoveryLabels(host)

	if err := r.patchHost(ctx, patchHelper, host); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: hostProbeRequeue}, nil
}

// populateFromDiscovery copies a discovery result into the host status and
// sets the Discovered condition: True on success, False with
// MixedGPUModels when the host has GPUs of more than one vendor:device pair
// (a mixed node is not claimable by a single-model selector).
func (r *ByotHostReconciler) populateFromDiscovery(host *infrav1.ByotHost, result DiscoveryResult) {
	host.Status.TalosVersion = result.TalosVersion
	host.Status.Arch = result.Arch
	host.Status.Platform = result.Platform
	host.Status.Hardware = &infrav1.HostHardware{
		CPU:               result.CPU,
		Memory:            result.Memory,
		Disks:             result.Disks,
		NetworkInterfaces: result.NetworkInterfaces,
		GPUs:              result.GPUs,
	}
	host.Status.LastProbedAt = nowPtr()

	if result.GPUs != nil && result.GPUs.Mixed {
		conditions.MarkFalse(
			host,
			infrav1.HostDiscoveredCondition,
			infrav1.HostDiscoveredReasonMixedGPUModels,
			clusterv1.ConditionSeverityWarning,
			"host has GPUs of more than one vendor:device pair",
		)

		return
	}

	conditions.MarkTrue(host, infrav1.HostDiscoveredCondition)
}

// patchHost persists metadata (labels) and status.
func (r *ByotHostReconciler) patchHost(ctx context.Context, patchHelper *patch.Helper, host *infrav1.ByotHost) error {
	if err := patchHelper.Patch(ctx, host); err != nil {
		return fmt.Errorf("failed to patch ByotHost %s: %w", host.Name, err)
	}

	return nil
}

func (r *ByotHostReconciler) livenessInterval() time.Duration {
	if r.LivenessInterval > 0 {
		return r.LivenessInterval
	}

	return hostLivenessInterval
}

func (r *ByotHostReconciler) failureThreshold() int32 {
	if r.FailureThreshold > 0 {
		return r.FailureThreshold
	}

	return hostFailureThreshold
}

func nowPtr() *metav1.Time {
	now := metav1.Now()

	return &now
}
