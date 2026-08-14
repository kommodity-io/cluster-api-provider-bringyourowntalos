package controller

import "errors"

var (
	// ErrBootstrapDataNotReady indicates the owner Machine has no bootstrap
	// data secret yet; reconciliation should be retried later.
	ErrBootstrapDataNotReady = errors.New("bootstrap data not ready")

	// ErrBootstrapDataEmpty indicates the bootstrap data secret exists but
	// carries no machine configuration payload.
	ErrBootstrapDataEmpty = errors.New("bootstrap data secret is empty")

	// ErrClusterTalosConfigNotReady indicates the talosconfig secret needed
	// for authenticated access is missing or incomplete; reconciliation
	// should be retried later.
	ErrClusterTalosConfigNotReady = errors.New("talosconfig secret not ready")

	// ErrNoResetCredentials indicates no usable talosconfig exists to
	// authenticate a machine reset.
	ErrNoResetCredentials = errors.New("no credential candidates available to reset machine")

	// ErrJoinBundleMismatch indicates the machine being adopted is already
	// configured and answers the Talos API with a different PKI bundle than
	// the cluster's; it must be reset before it can join.
	ErrJoinBundleMismatch = errors.New("machine carries a different cluster bundle: set " +
		"spec.joinPolicy=Reset to wipe it before adoption, or pre-clean it manually")

	// ErrJoinNoCredentials indicates the machine being adopted is already
	// configured but no talosconfig exists that authenticates against it, so
	// its bundle cannot be verified. Provide a talosconfig (spec.talosConfigSecretRef)
	// to authenticate and verify the machine, or manually return it to
	// maintenance mode.
	ErrJoinNoCredentials = errors.New("machine is already configured and no usable talosconfig " +
		"is available: provide a talosconfig via spec.talosConfigSecretRef to " +
		"authenticate the machine, or manually put it back in maintenance mode")

	// ErrClusterNameUnresolved indicates the ByotMachine being deleted has no
	// resolvable owning cluster name, so its workload Node cannot be cleaned up.
	ErrClusterNameUnresolved = errors.New("cannot resolve cluster name for ByotMachine")

	// ErrDrainTimeout indicates the best-effort drain of a workload Node
	// exceeded its timeout during ByotMachine deletion.
	ErrDrainTimeout = errors.New("timed out draining workload node")
)
