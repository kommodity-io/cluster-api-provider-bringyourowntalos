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
	// its bundle cannot be verified; it must be reset before it can join.
	ErrJoinNoCredentials = errors.New("machine is already configured and no usable talosconfig " +
		"is available: set spec.joinPolicy=Reset to wipe it before adoption, or pre-clean it manually")
)
