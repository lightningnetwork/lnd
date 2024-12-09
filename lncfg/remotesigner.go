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

// RemoteSigner holds the configuration options for how to connect to a remote
// signer. Only a watch-only node specifies this config.
//
//nolint:ll
type RemoteSigner struct {
	// Enable signals if this node is a watch-only node in a remote signer
	// setup.
	Enable bool `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys."`

	// MigrateWatchOnly migrates the wallet to a watch-only wallet by
	// purging all private keys from the wallet after first unlock with this
	// flag.
	MigrateWatchOnly bool `long:"migrate-wallet-to-watch-only" description:"If a wallet with private key material already exists, migrate it into a watch-only wallet on first startup. WARNING: This cannot be undone! Make sure you have backed up your seed before you use this flag! All private keys will be purged from the wallet after first unlock with this flag!"`

	// ConnectionCfg holds the connection configuration options that the
	// watch-only node will use when setting up the connection to the remote
	// signer.
	ConnectionCfg
}

// DefaultRemoteSignerCfg returns the default RemoteSigner config.
func DefaultRemoteSignerCfg() *RemoteSigner {
	return &RemoteSigner{
		Enable:        false,
		ConnectionCfg: defaultConnectionCfg(),
	}
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteSigner) Validate() error {
	if r.MigrateWatchOnly && !r.Enable {
		return fmt.Errorf("remote signer: cannot turn on wallet " +
			"migration to watch-only if remote signing is not " +
			"enabled")
	}

	if !r.Enable {
		return nil
	}

	// Else, we are in outbound mode, so we verify the connection config.
	err := r.ConnectionCfg.Validate()
	if err != nil {
		return fmt.Errorf("remotesigner.%w", err)
	}

	return nil
}

// ConnectionCfg holds the configuration options required when setting up a
// connection to either a remote signer or watch-only node, depending on which
// side makes the outbound connection.
//
//nolint:ll
type ConnectionCfg struct {
	RPCHost      string        `long:"rpchost" description:"The RPC host:port of the remote signer. For watch-only nodes, this should be set to the remote signer's RPC host:port."`
	MacaroonPath string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer. For watch-only nodes, this should be set to the remote signer's macaroon."`
	TLSCertPath  string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's identity. For watch-only nodes, this should be set to the remote signer's TLS certificate."`
	Timeout      time.Duration `long:"timeout" description:"The timeout for making the connection to the remote signer. Valid time units are {s, m, h}."`
}

// defaultConnectionCfg returns the default ConnectionCfg config.
func defaultConnectionCfg() ConnectionCfg {
	return ConnectionCfg{
		Timeout: DefaultRemoteSignerRPCTimeout,
	}
}

// Validate checks the values set in the ConnectionCfg config are valid.
func (c *ConnectionCfg) Validate() error {
	if c.Timeout < time.Millisecond {
		return fmt.Errorf("timeout of %v is invalid, cannot be "+
			"smaller than %v", c.Timeout, time.Millisecond)
	}

	if c.RPCHost == "" {
		return fmt.Errorf("rpchost must be set")
	}

	if c.MacaroonPath == "" {
		return fmt.Errorf("macaroonpath must be set")
	}

	if c.TLSCertPath == "" {
		return fmt.Errorf("tlscertpath must be set")
	}

	return nil
}
