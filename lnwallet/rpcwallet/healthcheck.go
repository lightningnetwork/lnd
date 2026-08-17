package rpcwallet

import (
	"context"
	"time"
)

// HealthCheck returns a health check function for the given remote signing
// configuration.
func HealthCheck(ctx context.Context, timeout time.Duration, ping func(
	ctx context.Context, duration time.Duration) error) func() error {

	return func() error {
		ctxt, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		err := ping(ctxt, timeout)
		if err != nil {
			log.Errorf("Remote signer health check failed: %v", err)

			return err
		}

		return nil
	}
}
