package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
)

const (
	// talosAPIPort is the secure Talos machine API port. In maintenance mode
	// it accepts ApplyConfiguration with unverified TLS.
	talosAPIPort = "50000"

	// applyTimeout bounds a single ApplyConfiguration attempt.
	applyTimeout = 2 * time.Minute
)

// applyMachineConfig applies the given Talos machine configuration to a
// machine running in maintenance mode at publicIP. Maintenance mode performs
// no client authentication, so an unverified TLS client is used; this is
// intentional for adoption only.
func applyMachineConfig(ctx context.Context, publicIP string, machineConfig []byte) error {
	//nolint:gosec // Maintenance mode has no PKI material; apply must skip verification.
	tlsConfig := &tls.Config{InsecureSkipVerify: true}

	endpoint := net.JoinHostPort(publicIP, talosAPIPort)

	client, err := talosclient.New(ctx, talosclient.WithTLSConfig(tlsConfig), talosclient.WithEndpoints(endpoint))
	if err != nil {
		return fmt.Errorf("failed to create Talos maintenance client for %s: %w", endpoint, err)
	}

	defer client.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(talosclient.WithNode(ctx, publicIP), applyTimeout)
	defer cancel()

	_, err = client.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: machineConfig,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	})
	if err != nil {
		return fmt.Errorf("failed to apply machine configuration on %s: %w", endpoint, err)
	}

	return nil
}
