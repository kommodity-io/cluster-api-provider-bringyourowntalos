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
)
