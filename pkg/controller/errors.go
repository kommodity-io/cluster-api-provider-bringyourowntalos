package controller

import "errors"

var (
	// ErrBootstrapDataNotReady indicates the owner Machine has no bootstrap
	// data secret yet; reconciliation should be retried later.
	ErrBootstrapDataNotReady = errors.New("bootstrap data not ready")

	// ErrBootstrapDataEmpty indicates the bootstrap data secret exists but
	// carries no machine configuration payload.
	ErrBootstrapDataEmpty = errors.New("bootstrap data secret is empty")
)
