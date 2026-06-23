package lncfg

import (
	"fmt"
	"time"
)

const (
	// DefaultRemoteSignerListenPort is the default port for the dedicated
	// inbound remote signer RPC listeners when no port is specified.
	DefaultRemoteSignerListenPort = 10019

	// DefaultRemoteSignerRPCTimeout is the default connection timeout
	// that is used when connecting to the remote signer or watch-only node
	// through RPC.
	DefaultRemoteSignerRPCTimeout = 5 * time.Second

	// DefaultRemoteSignerRequestTimeout is the default timeout used for
	// requests to and from the remote signer.
	DefaultRemoteSignerRequestTimeout = 5 * time.Second

	// DefaultStartupTimeout is the default startup timeout used when a
	// watch-only node with
	// 'remotesigner.experimentalallowinboundconnection' set to true waits
	// for the remote signer to connect.
	DefaultStartupTimeout = 5 * time.Minute
)

// RemoteSigner holds the configuration options for how to connect to a remote
// signer. Only a watch-only node specifies this config.
//
//nolint:ll
type RemoteSigner struct {
	// Enable signals if this node is a watch-only node in a remote signer
	// setup.
	Enable bool `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys."`

	// ExperimentalAllowInboundConnection is true if the remote signer node
	// will connect to this node.
	ExperimentalAllowInboundConnection bool `long:"experimentalallowinboundconnection" description:"EXPERIMENTAL: Signals that we allow an inbound connection from a remote signer to this node."`

	// MigrateWatchOnly migrates the wallet to a watch-only wallet by
	// purging all private keys from the wallet after first unlock with this
	// flag.
	MigrateWatchOnly bool `long:"migrate-wallet-to-watch-only" description:"If a wallet with private key material already exists, migrate it into a watch-only wallet on first startup. WARNING: This cannot be undone! Make sure you have backed up your seed before you use this flag! All private keys will be purged from the wallet after first unlock with this flag!"`

	// ConnectionCfg holds the connection configuration options that the
	// watch-only node will use when setting up the connection to the remote
	// signer.
	ConnectionCfg

	// InboundWatchOnlyCfg holds the configuration options specifically
	// used when the watch-only node expects an inbound connection from
	// the remote signer.
	InboundWatchOnlyCfg
}

// DefaultRemoteSignerCfg returns the default RemoteSigner config.
func DefaultRemoteSignerCfg() *RemoteSigner {
	return &RemoteSigner{
		Enable:                             false,
		ExperimentalAllowInboundConnection: false,
		ConnectionCfg:                      defaultConnectionCfg(),
		InboundWatchOnlyCfg: InboundWatchOnlyCfg{
			ExperimentalStartupTimeout: DefaultStartupTimeout,
		},
	}
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteSigner) Validate() error {
	if !r.Enable {
		if r.MigrateWatchOnly {
			return fmt.Errorf("remote signer: cannot turn on " +
				"wallet migration to watch-only if remote " +
				"signing is not enabled")
		}

		if r.ExperimentalAllowInboundConnection {
			return fmt.Errorf("remote signer: cannot enable " +
				"'experimentalallowinboundconnection' if " +
				"remote signing is not enabled")
		}

		return nil
	}

	if r.ExperimentalAllowInboundConnection {
		if len(r.ExperimentalRPCListeners) == 0 {
			//nolint:ll
			return fmt.Errorf("remotesigner.experimentalrpclisten " +
				"must be set when " +
				"experimentalallowinboundconnection is enabled")
		}

		if r.ExperimentalStartupTimeout < 0 {
			return fmt.Errorf("remotesigner."+
				"experimentalstartuptimeout of %v is "+
				"invalid, cannot be smaller than %v",
				r.ExperimentalStartupTimeout, 0)
		}
	}

	// Validate the shared timeout values in both inbound and outbound mode.
	err := r.ConnectionCfg.validateTimeouts()
	if err != nil {
		return fmt.Errorf("remotesigner.%w", err)
	}

	// The host and credential settings are required only when the
	// watch-only node initiates the outbound connection to the remote
	// signer.
	if r.ExperimentalAllowInboundConnection {
		return nil
	}

	err = r.ConnectionCfg.validateRemoteHostCredentials()
	if err != nil {
		return fmt.Errorf("remotesigner.%w", err)
	}

	return nil
}

// InboundWatchOnlyCfg holds the configuration options specific for watch-only
// nodes with the `experimentalallowinboundconnection` option set.
//
//nolint:ll
type InboundWatchOnlyCfg struct {
	ExperimentalStartupTimeout time.Duration `long:"experimentalstartuptimeout" description:"EXPERIMENTAL: The time the watch-only node will wait for the remote signer to connect during startup. If the timeout expires before the remote signer connects, the watch-only node will shut down. If set to 0, no timeout will not expire. Valid time units are {s, m, h}."`

	// RPCListeners is the set of dedicated gRPC listener addresses that
	// serve only the SignCoordinatorStreams RPC for inbound remote signer
	// connections. This must be set when
	// experimentalallowinboundconnection is enabled.
	// If a listener omits a port, the default remote signer RPC port is
	// used.
	ExperimentalRPCListeners []string `long:"experimentalrpclisten" description:"EXPERIMENTAL: Dedicated RPC listen address(es) for inbound remote signer connections. When experimentalallowinboundconnection is enabled, lnd starts a separate gRPC server on these listeners that serves only the SignCoordinatorStreams RPC. If no port is specified, the default remote signer RPC port 10019 is used."`
}

// WatchOnlyNode holds the configuration options for how to connect to a watch
// only node. Only a signer node specifies this config.
//
//nolint:ll
type WatchOnlyNode struct {
	// Enable signals if this node a signer node and is expected to connect
	// to a watch-only node.
	ExperimentalEnable bool `long:"experimentalenable" description:"EXPERIMENTAL: Signals that this node functions as a remote signer that will to connect with a watch-only node."`

	// ConnectionCfg holds the connection configuration options that the
	// remote signer node will use when setting up the connection to the
	// watch-only node.
	ConnectionCfg
}

// DefaultWatchOnlyNodeCfg returns the default WatchOnlyNode config.
func DefaultWatchOnlyNodeCfg() *WatchOnlyNode {
	return &WatchOnlyNode{
		ExperimentalEnable: false,
		ConnectionCfg:      defaultConnectionCfg(),
	}
}

// Validate checks the values set in the WatchOnlyNode config are valid.
func (w *WatchOnlyNode) Validate() error {
	if !w.ExperimentalEnable {
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
	RPCHost                    string        `long:"rpchost" description:"The RPC host:port of the remote signer or watch-only node. For watch-only nodes with 'remotesigner.experimentalallowinboundconnection' set to false (the default value if not specifically set), this should be set to the remote signer's RPC host:port. For remote signer nodes connecting to a watch-only node with 'remotesigner.experimentalallowinboundconnection' set to true, this should be set to the watch-only node's RPC host:port."`
	MacaroonPath               string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer or the watch-only node. For watch-only nodes with 'remotesigner.experimentalallowinboundconnection' set to false (the default value if not specifically set), this should be set to the remote signer's macaroon. For remote signer nodes connecting to a watch-only node with 'remotesigner.experimentalallowinboundconnection' set to true, this should be set to the watch-only node's macaroon."`
	TLSCertPath                string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's or watch-only node's identity. For watch-only nodes with 'remotesigner.experimentalallowinboundconnection' set to false (the default value if not specifically set), this should be set to the remote signer's TLS certificate. For remote signer nodes connecting to a watch-only node with 'remotesigner.experimentalallowinboundconnection' set to true, this should be set to the watch-only node's TLS certificate."`
	Timeout                    time.Duration `long:"timeout" description:"The timeout for making the connection to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. For watch-only nodes with 'remotesigner.experimentalallowinboundconnection' set to true, this timeout value has no effect. Valid time units are {s, m, h}."`
	ExperimentalRequestTimeout time.Duration `long:"experimentalrequesttimeout" description:"EXPERIMENTAL: The time we will wait when making requests to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. Valid time units are {s, m, h}."`
}

// defaultConnectionCfg returns the default ConnectionCfg config.
func defaultConnectionCfg() ConnectionCfg {
	return ConnectionCfg{
		Timeout:                    DefaultRemoteSignerRPCTimeout,
		ExperimentalRequestTimeout: DefaultRemoteSignerRequestTimeout,
	}
}

// Validate checks the values set in the ConnectionCfg config are valid.
func (c *ConnectionCfg) Validate() error {
	err := c.validateTimeouts()
	if err != nil {
		return err
	}

	return c.validateRemoteHostCredentials()
}

// validateTimeouts checks the timeout values that apply in both inbound and
// outbound remote signer modes.
func (c *ConnectionCfg) validateTimeouts() error {
	if c.Timeout < time.Millisecond {
		return fmt.Errorf("timeout of %v is invalid, cannot be "+
			"smaller than %v", c.Timeout, time.Millisecond)
	}

	if c.ExperimentalRequestTimeout < time.Second {
		return fmt.Errorf("experimentalrequesttimeout of %v is "+
			"invalid, cannot be smaller than %v",
			c.ExperimentalRequestTimeout, time.Second)
	}

	return nil
}

// validateRemoteHostCredentials checks the host and credential settings needed
// when this node initiates the outbound RPC connection.
func (c *ConnectionCfg) validateRemoteHostCredentials() error {
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
