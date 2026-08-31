package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	infrav1 "github.com/kommodity-io/cluster-api-provider-bringyourowntalos/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ErrNoHostAvailable indicates no ByotHost currently matches the claim
// criteria (none exists, or none is Available). The ByotMachine requeues and
// retries until a host becomes available.
var ErrNoHostAvailable = errors.New("no available ByotHost matches the claim criteria")

// claimHost ensures the ByotMachine has claimed an Available ByotHost. It is
// idempotent: a ByotMachine that already holds a host keeps it. On a successful
// claim it sets status.resolvedHost and status.resolvedPublicIP. handled is
// true when the caller must return the given result (requeue waiting for a
// host, or a CAS conflict) instead of proceeding to adoption.
//
func (r *ByotMachineReconciler) claimHost(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
	patchHelper *patch.Helper,
) (ctrl.Result, bool, error) {
	if kept, err := r.verifyExistingClaim(ctx, byotMachine, patchHelper); err != nil {
		return ctrl.Result{}, true, err
	} else if kept {
		return ctrl.Result{}, false, nil
	}

	host, err := r.resolveClaimCandidate(ctx, byotMachine)
	if err != nil {
		if errors.Is(err, ErrNoHostAvailable) {
			log.FromContext(ctx).Info("Waiting for an available ByotHost",
				"byotMachine", byotMachine.Name)

			return ctrl.Result{RequeueAfter: requeueAfterBootstrap}, true, nil
		}

		return ctrl.Result{}, true, err
	}

	if err := r.claimCAS(ctx, host, byotMachine); err != nil {
		if apierrors.IsConflict(err) {
			// Lost the race to claim: retry on the next reconcile.
			return ctrl.Result{Requeue: true}, true, nil
		}

		return ctrl.Result{}, true, fmt.Errorf("failed to claim ByotHost %s: %w", host.Name, err)
	}

	byotMachine.Status.ResolvedHost = host.Name
	byotMachine.Status.ResolvedPublicIP = host.Spec.PublicIP

	if err := patchHelper.Patch(ctx, byotMachine); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("failed to patch ByotMachine after claim: %w", err)
	}

	log.FromContext(ctx).Info("Claimed ByotHost",
		"byotMachine", byotMachine.Name, "byotHost", host.Name, "publicIP", host.Spec.PublicIP)

	return ctrl.Result{}, false, nil
}

// verifyExistingClaim returns kept=true when the ByotMachine already holds a
// valid claim on its resolved host. A lost claim (host gone or re-claimed
// elsewhere) is cleared so the caller re-claims.
func (r *ByotMachineReconciler) verifyExistingClaim(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
	patchHelper *patch.Helper,
) (bool, error) {
	if byotMachine.Status.ResolvedHost == "" {
		return false, nil
	}

	host := &infrav1.ByotHost{}

	err := r.Client.Get(ctx, ctrlclient.ObjectKey{
		Namespace: byotMachine.Namespace,
		Name:      byotMachine.Status.ResolvedHost,
	}, host)

	if err == nil && host.Status.ClaimRef != nil && host.Status.ClaimRef.UID == string(byotMachine.UID) {
		// Keep the resolved IP in sync (the host IP is immutable, so this
		// only heals a stale status).
		if byotMachine.Status.ResolvedPublicIP != host.Spec.PublicIP {
			byotMachine.Status.ResolvedPublicIP = host.Spec.PublicIP

			if err := patchHelper.Patch(ctx, byotMachine); err != nil {
				return false, fmt.Errorf("failed to patch ByotMachine resolved IP: %w", err)
			}
		}

		return true, nil
	}

	// Lost the claim (host released/deleted/re-claimed elsewhere): clear
	// and fall through to re-claim.
	byotMachine.Status.ResolvedHost = ""
	byotMachine.Status.ResolvedPublicIP = ""

	if err := patchHelper.Patch(ctx, byotMachine); err != nil {
		return false, fmt.Errorf("failed to patch ByotMachine after lost claim: %w", err)
	}

	return false, nil
}

// resolveClaimCandidate picks the ByotHost to claim: the host named by
// hostRef, or the first Available host matching hostSelector (+ failureDomain).
func (r *ByotMachineReconciler) resolveClaimCandidate(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
) (*infrav1.ByotHost, error) {
	if byotMachine.Spec.HostRef != nil {
		return r.resolveHostRef(ctx, byotMachine)
	}

	return r.selectClaimCandidate(ctx, byotMachine)
}

// resolveHostRef fetches the explicitly named ByotHost and admits it only when
// it is Available with maintenance liveness confirmed and unclaimed (or
// self-claimed).
func (r *ByotMachineReconciler) resolveHostRef(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
) (*infrav1.ByotHost, error) {
	host := &infrav1.ByotHost{}

	err := r.Client.Get(ctx, ctrlclient.ObjectKey{
		Namespace: byotMachine.Namespace,
		Name:      byotMachine.Spec.HostRef.Name,
	}, host)
	if apierrors.IsNotFound(err) {
		return nil, ErrNoHostAvailable
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get ByotHost %s: %w", byotMachine.Spec.HostRef.Name, err)
	}

	if host.Status.Phase != infrav1.HostPhaseAvailable || !hostClaimable(host) {
		return nil, ErrNoHostAvailable
	}

	if host.Status.ClaimRef != nil && host.Status.ClaimRef.UID != string(byotMachine.UID) {
		return nil, ErrNoHostAvailable
	}

	return host, nil
}

// selectClaimCandidate lists Available ByotHosts matching the label selector
// and failureDomain, and returns the first by name.
func (r *ByotMachineReconciler) selectClaimCandidate(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
) (*infrav1.ByotHost, error) {
	selector, err := metav1.LabelSelectorAsSelector(byotMachine.Spec.HostSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid hostSelector: %w", err)
	}

	hostList := &infrav1.ByotHostList{}

	err = r.Client.List(ctx, hostList,
		ctrlclient.InNamespace(byotMachine.Namespace),
		ctrlclient.MatchingLabelsSelector{Selector: selector},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list ByotHosts: %w", err)
	}

	candidates := filterClaimCandidates(hostList.Items, byotMachine)
	if len(candidates) == 0 {
		return nil, ErrNoHostAvailable
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })

	return &candidates[0], nil
}

// hostClaimable reports whether a ByotHost is claimable: it must be in the
// Available phase with maintenance liveness confirmed at the last probe. A
// host whose last probe failed (even once, before the failure threshold flips
// its phase to Unavailable) is not claimable, so a transient liveness flap does
// not let a claim grab a host mid-failure (Decision 5: Available requires
// liveness confirmed).
func hostClaimable(host *infrav1.ByotHost) bool {
	return conditions.IsTrue(host, infrav1.HostMaintenanceProbeCondition)
}

// filterClaimCandidates keeps only Available, unclaimed (or self-claimed)
// hosts matching the failureDomain, when set.
func filterClaimCandidates(hosts []infrav1.ByotHost, byotMachine *infrav1.ByotMachine) []infrav1.ByotHost {
	out := make([]infrav1.ByotHost, 0, len(hosts))

	for i := range hosts {
		host := &hosts[i]

		if host.Status.Phase != infrav1.HostPhaseAvailable {
			continue
		}

		if !hostClaimable(host) {
			continue
		}

		if host.Status.ClaimRef != nil && host.Status.ClaimRef.UID != string(byotMachine.UID) {
			continue
		}

		if byotMachine.Spec.FailureDomain != nil && *byotMachine.Spec.FailureDomain != "" {
			if host.Spec.FailureDomain == nil || *host.Spec.FailureDomain != *byotMachine.Spec.FailureDomain {
				continue
			}
		}

		out = append(out, *host)
	}

	return out
}

// claimCAS optimistically sets the host's claimRef to this ByotMachine and
// moves it to Claimed, using a status update for compare-and-swap. A
// concurrent writer that claimed first causes a conflict (first writer wins).
func (r *ByotMachineReconciler) claimCAS(
	ctx context.Context,
	host *infrav1.ByotHost,
	byotMachine *infrav1.ByotMachine,
) error {
	// Re-fetch the latest state right before claiming to tighten the race.
	latest := &infrav1.ByotHost{}

	if err := r.Client.Get(ctx, ctrlclient.ObjectKeyFromObject(host), latest); err != nil {
		return err
	}

	if latest.Status.ClaimRef != nil && latest.Status.ClaimRef.UID != string(byotMachine.UID) {
		return ErrNoHostAvailable
	}

	latest.Status.ClaimRef = &infrav1.HostClaimRef{
		Kind:      "ByotMachine",
		Name:      byotMachine.Name,
		Namespace: byotMachine.Namespace,
		UID:       string(byotMachine.UID),
	}
	latest.Status.Phase = infrav1.HostPhaseClaimed

	if err := r.Client.Status().Update(ctx, latest); err != nil {
		return err
	}

	// Ensure the protection finalizer so a claimed host cannot be deleted
	// out from under the ByotMachine.
	if controllerutil.AddFinalizer(latest, byotHostFinalizer) {
		if err := r.Client.Update(ctx, latest); err != nil {
			return fmt.Errorf("failed to add finalizer to ByotHost %s: %w", latest.Name, err)
		}
	}

	return nil
}

// releaseHost resets the claimed host to maintenance and moves it to Releasing
// so the ByotHost liveness loop returns it to Available. The claimRef is
// cleared once the reset is issued. It is a no-op when no host is claimed or
// the host is already gone.
func (r *ByotMachineReconciler) releaseHost(
	ctx context.Context,
	byotMachine *infrav1.ByotMachine,
) error {
	if byotMachine.Status.ResolvedHost == "" {
		return nil
	}

	host := &infrav1.ByotHost{}

	err := r.Client.Get(ctx, ctrlclient.ObjectKey{
		Namespace: byotMachine.Namespace,
		Name:      byotMachine.Status.ResolvedHost,
	}, host)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to get ByotHost %s: %w", byotMachine.Status.ResolvedHost, err)
	}

	candidates, err := r.resetCredentialCandidates(ctx, byotMachine)
	if err != nil {
		return err
	}

	publicIP := byotMachine.Status.ResolvedPublicIP

	if err := attemptReset(ctx, candidates, publicIP); err != nil {
		return fmt.Errorf("failed to reset host %s (%s): %w", host.Name, publicIP, err)
	}

	// Reset issued: clear the claim and let the liveness loop flip
	// Releasing → Available once maintenance answers.
	host.Status.Phase = infrav1.HostPhaseReleasing
	host.Status.ClaimRef = nil

	if err := r.Client.Status().Update(ctx, host); err != nil {
		return fmt.Errorf("failed to update ByotHost %s status on release: %w", host.Name, err)
	}

	return nil
}

