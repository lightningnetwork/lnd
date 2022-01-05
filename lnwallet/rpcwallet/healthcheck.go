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
		_, err := connectRPC(
			cfg.RPCHost, cfg.TLSCertPath, cfg.MacaroonPath, timeout,
		)
		if err != nil {
			return fmt.Errorf("error connecting to the remote "+
				"signing node through RPC: %v", err)
		}

		return nil
	}
}
