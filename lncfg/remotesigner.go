package lncfg

import (
	"fmt"
	"time"
)

const (
	// DefaultRemoteSignerRPCTimeout is the default timeout that is used
	// when forwarding a request to the remote signer through RPC.
	DefaultRemoteSignerRPCTimeout = 5 * time.Second
)

// RemoteSigner holds the configuration options for a remote RPC signer.
//
//nolint:ll
type RemoteSigner struct {
	Enable           bool          `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys."`
	RPCHost          string        `long:"rpchost" description:"The remote signer's RPC host:port"`
	MacaroonPath     string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer"`
	TLSCertPath      string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's identity"`
	Timeout          time.Duration `long:"timeout" description:"The timeout for connecting to and signing requests with the remote signer. Valid time units are {s, m, h}."`
	MigrateWatchOnly bool          `long:"migrate-wallet-to-watch-only" description:"If a wallet with private key material already exists, migrate it into a watch-only wallet on first startup. WARNING: This cannot be undone! Make sure you have backed up your seed before you use this flag! All private keys will be purged from the wallet after first unlock with this flag!"`
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteSigner) Validate() error {
	if !r.Enable {
		return nil
	}

	if r.Timeout < time.Millisecond {
		return fmt.Errorf("remote signer: timeout of %v is invalid, "+
			"cannot be smaller than %v", r.Timeout,
			time.Millisecond)
	}

	if r.MigrateWatchOnly && !r.Enable {
		return fmt.Errorf("remote signer: cannot turn on wallet " +
			"migration to watch-only if remote signing is not " +
			"enabled")
	}

	return nil
}
