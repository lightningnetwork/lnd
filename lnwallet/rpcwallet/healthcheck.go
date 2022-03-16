package rpcwallet

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
)

// HealthCheck returns a health check function for the given remote signing
// configuration.
func HealthCheck(cfg *lncfg.RemoteSigner, timeout time.Duration) func() error {
	return func() error {
		conn, err := connectRPC(
			cfg.RPCHost, cfg.TLSCertPath, cfg.MacaroonPath, timeout,
		)
		if err != nil {
			return fmt.Errorf("error connecting to the remote "+
				"signing node through RPC: %v", err)
		}

		defer func() {
			err = conn.Close()
			if err != nil {
				log.Warnf("Failed to close health check "+
					"connection to remote signing node: %v",
					err)
			}
		}()

		return nil
	}
}
