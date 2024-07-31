package rpcwallet

import (
	"time"
)

// HealthCheck returns a health check function for the given remote signing
// configuration.
func HealthCheck(rs RemoteSigner, timeout time.Duration) func() error {
	return func() error {
		err := rs.Ping(timeout)
		if err != nil {
			log.Errorf("Remote signer health check failed: %v", err)

			return err
		}

		return nil
	}
}
