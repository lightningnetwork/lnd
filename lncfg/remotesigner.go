package lncfg

import (
	"fmt"
	"time"
)

const (
	// DefaultRemoteSignerRPCTimeout is the default connection timeout
	// that is used when connecting to the remote signer or watch-only node
	// through RPC.
	DefaultRemoteSignerRPCTimeout = 5 * time.Second

	// DefaultRequestTimeout is the default timeout used for requests to and
	// from the remote signer.
	DefaultRequestTimeout = 5 * time.Second
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

// WatchOnlyNode holds the configuration options for how to connect to a watch
// only node. Only a signer node specifies this config.
//
//nolint:ll
type WatchOnlyNode struct {
	// Enable signals if this node a signer node and is expected to connect
	// to a watch-only node.
	Enable bool `long:"enable" description:"Signals that this node functions as a remote signer that will to connect with a watch-only node."`

	// ConnectionCfg holds the connection configuration options that the
	// remote signer node will use when setting up the connection to the
	// watch-only node.
	ConnectionCfg
}

// DefaultWatchOnlyNodeCfg returns the default WatchOnlyNode config.
func DefaultWatchOnlyNodeCfg() *WatchOnlyNode {
	return &WatchOnlyNode{
		Enable:        false,
		ConnectionCfg: defaultConnectionCfg(),
	}
}

// Validate checks the values set in the WatchOnlyNode config are valid.
func (w *WatchOnlyNode) Validate() error {
	if !w.Enable {
		return nil
	}

	err := w.ConnectionCfg.Validate()
	if err != nil {
		return fmt.Errorf("watchonlynode.%w", err)
	}

	return nil
}

// ConnectionCfg holds the configuration options required when setting up a
// connection to either a remote signer or watch-only node, depending on which
// side makes the outbound connection.
//
//nolint:ll
type ConnectionCfg struct {
	RPCHost        string        `long:"rpchost" description:"The RPC host:port of the remote signer or watch-only node. For watch-only nodes, this should be set to the remote signer's RPC host:port. For remote signer nodes connecting to a watch-only node, this should be set to the watch-only node's RPC host:port."`
	MacaroonPath   string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer or the watch-only node. For watch-only nodes, this should be set to the remote signer's macaroon. For remote signer nodes connecting to a watch-only node, this should be set to the watch-only node's macaroon."`
	TLSCertPath    string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's or watch-only node's identity. For watch-only nodes, this should be set to the remote signer's TLS certificate. For remote signer nodes connecting to a watch-only node, this should be set to the watch-only node's TLS certificate."`
	Timeout        time.Duration `long:"timeout" description:"The timeout for making the connection to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. Valid time units are {s, m, h}."`
	RequestTimeout time.Duration `long:"requesttimeout" description:"The time we will wait when making requests to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. Valid time units are {s, m, h}."`
}

// defaultConnectionCfg returns the default ConnectionCfg config.
func defaultConnectionCfg() ConnectionCfg {
	return ConnectionCfg{
		Timeout:        DefaultRemoteSignerRPCTimeout,
		RequestTimeout: DefaultRequestTimeout,
	}
}

// Validate checks the values set in the ConnectionCfg config are valid.
func (c *ConnectionCfg) Validate() error {
	if c.Timeout < time.Millisecond {
		return fmt.Errorf("timeout of %v is invalid, cannot be "+
			"smaller than %v", c.Timeout, time.Millisecond)
	}

	if c.RequestTimeout < time.Second {
		return fmt.Errorf("requesttimeout of %v is invalid, cannot "+
			"be smaller than %v", c.RequestTimeout, time.Second)
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
