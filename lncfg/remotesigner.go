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

	// DefaultStartupTimeout is the default startup timeout used when the
	// watch-only node with signerrole 'watchonly-outbound' waits for the
	// remote signer to connect.
	DefaultStartupTimeout = 5 * time.Minute

	// DefaultInboundWatchOnlyRole is the default signer role used when
	// enabling a remote signer on the watch-only node. It indicates that
	// the remote signer node allows inbound connections from the watch-only
	// node.
	DefaultInboundWatchOnlyRole = "watchonly-inbound"

	// OutboundWatchOnlyRole is a type of signer role used when enabling a
	// remote signer on the watch-only node. It indicates that the remote
	// signer node will make an outbound connection to the watch-only node
	// to connect the nodes.
	OutboundWatchOnlyRole = "watchonly-outbound"

	// OutboundSignerRole indicates that the lnd instance will act as an
	// outbound remote signer, connecting to a watch-only node that has the
	// 'watchonly-outbound' signer role set.
	OutboundSignerRole = "signer-outbound"

	// InboundSignerRole indicates that the lnd instance will act as an
	// inbound remote signer, which allows a watch-only node to connect
	// which has the 'watchonly-inbound' signer role set.
	InboundSignerRole = "signer-inbound"
)

// RemoteSigner holds the configuration options for a remote RPC signer.
//
//nolint:lll
type RemoteSigner struct {
	Enable           bool          `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys. This param should not be set to true when signerrole is set to either 'signer-outbound' or 'signer-inbound'"`
	SignerRole       string        `long:"signerrole" description:"Sets the type of remote signer to use, or signals that the node will act as a remote signer. Can be set to either 'watchonly-inbound' (default), 'watchonly-outbound', 'signer-outbound' or 'signer-inbound'. 'watchonly-inbound' means that a remote signer that allows inbound connections from the watch-only node is used. 'watchonly-outbound' means that a remote signer node that makes an outbound connection to the watch-only node is used. 'signer-outbound' means the lnd instance will act as a remote signer, making an outbound connection to a watch-only node with the 'watchonly-outbound' signerrole set. 'signer-inbound' means that the lnd instance will act as an inbound remote signer, which allows a watch-only node to connect which has the 'watchonly-inbound' signer role set." choice:"watchonly-inbound" choice:"watchonly-outbound" choice:"signer-outbound" choice:"signer-inbound"`
	RPCHost          string        `long:"rpchost" description:"The remote signer's or watch-only node's RPC host:port. For nodes which have the signerrole set to 'watchonly-inbound', this should be set to the remote signer node's RPC host:port. For nodes which have the signerrole set to 'signer-outbound', this should be set to the watch-only node's RPC host:port. This param should not be set when signerrole is set to either 'watchonly-outbound' or 'signer-inbound'."`
	MacaroonPath     string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer or the watch-only node. For nodes which have the signerrole set to 'watchonly-inbound', this should be set to the remote signer node's macaroon. For nodes which have the signerrole set to 'signer-outbound', this should be set to the watch-only node's macaroon. This param should not be set when signerrole is set to either 'watchonly-outbound' or 'signer-inbound'."`
	TLSCertPath      string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's or watch-only node's identity. For nodes which have the signerrole set to 'watchonly-inbound', this should be set to the remote signer node's TLS certificate. For nodes which have the signerrole set to 'signer-outbound', this should be set to the watch-only node's TLS certificate. This param should not be set when signerrole is set to either 'watchonly-outbound' or 'signer-inbound'."`
	Timeout          time.Duration `long:"timeout" description:"The timeout for making the connection to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. Valid time units are {s, m, h}"`
	RequestTimeout   time.Duration `long:"requesttimeout" description:"The time we will wait when making requests to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. This parameter will have no effect if signerrole is set to 'watchonly-inbound'. Valid time units are {s, m, h}."`
	StartupTimeout   time.Duration `long:"startuptimeout" description:"The time a watch-only node (with signerrole set to 'watchonly-outbound') will wait for the remote signer to connect during startup. If the timeout expires before the remote signer connects, the watch-only node will shut down. This parameter has no effect if 'signerrole' is not set to 'watchonly-outbound'. Valid time units are {s, m, h}."`
	MigrateWatchOnly bool          `long:"migrate-wallet-to-watch-only" description:"If a wallet with private key material already exists, migrate it into a watch-only wallet on first startup. WARNING: This cannot be undone! Make sure you have backed up your seed before you use this flag! All private keys will be purged from the wallet after first unlock with this flag!"`
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteSigner) Validate() error {
	if r.Timeout < time.Millisecond {
		return fmt.Errorf("remote signer: timeout of %v is invalid, "+
			"cannot be smaller than %v", r.Timeout,
			time.Millisecond)
	}

	if r.RequestTimeout < time.Second {
		return fmt.Errorf("remote signer: requesttimeout of %v is "+
			"invalid, cannot be smaller than %v",
			r.Timeout, time.Second)
	}

	if r.StartupTimeout < time.Second {
		return fmt.Errorf("remote signer: startuptimeout of %v is "+
			"invalid, cannot be smaller than %v",
			r.Timeout, time.Second)
	}

	if r.MigrateWatchOnly && !r.Enable {
		return fmt.Errorf("remote signer: cannot turn on wallet " +
			"migration to watch-only if remote signing is not " +
			"enabled")
	}

	if r.SignerRole == OutboundSignerRole && r.Enable {
		return fmt.Errorf("remote signer: do not set " +
			"remotesigner.enable when signerrole is set to " +
			"'signer-outbound'")
	}

	if r.SignerRole == OutboundSignerRole && r.RPCHost == "" {
		return fmt.Errorf("remote signer: the rpchost for the " +
			"watch-only node must be set when the node acts as " +
			"an outbound remote signer")
	}

	if r.SignerRole == OutboundSignerRole && r.MacaroonPath == "" {
		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"watch-only node must be set when the node acts as " +
			"an outbound remote signer")
	}

	if r.SignerRole == OutboundSignerRole && r.TLSCertPath == "" {
		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"watch-only node must be set when the node acts as " +
			"an outbound remote signer")
	}

	if r.SignerRole == InboundSignerRole && r.Enable {
		return fmt.Errorf("remote signer: do not set " +
			"remotesigner.enable when signerrole is set to " +
			"'signer-inbound'")
	}

	if r.SignerRole == InboundSignerRole && r.RPCHost != "" {
		return fmt.Errorf("remote signer: the rpchost for the " +
			"watch-only node should not be set when the node " +
			"acts as an inbound remote signer")
	}

	if r.SignerRole == InboundSignerRole && r.MacaroonPath != "" {
		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"watch-only node should not be set when the node " +
			"acts as an inbound remote signer")
	}

	if r.SignerRole == InboundSignerRole && r.TLSCertPath != "" {
		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"watch-only node should not be set when the node " +
			"acts as an inbound remote signer")
	}

	if !r.Enable {
		return nil
	}

	if r.SignerRole == DefaultInboundWatchOnlyRole && r.RPCHost == "" {
		return fmt.Errorf("remote signer: the rpchost for the remote " +
			"signer should be set when using an inbound remote " +
			"signer")
	}

	if r.SignerRole == DefaultInboundWatchOnlyRole &&
		r.MacaroonPath == "" {

		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"remote signer should be set when using an inbound " +
			"remote signer")
	}

	if r.SignerRole == DefaultInboundWatchOnlyRole &&
		r.TLSCertPath == "" {

		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"remote signer should be set when using an inbound " +
			"remote signer")
	}

	if r.SignerRole == OutboundWatchOnlyRole && r.RPCHost != "" {
		return fmt.Errorf("remote signer: the rpchost for the remote " +
			"signer should not be set if the signerrole is set " +
			"to 'watchonly-outbound'")
	}

	if r.SignerRole == OutboundWatchOnlyRole && r.MacaroonPath != "" {
		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"remote signer should not be set if the signerrole " +
			"is set to 'watchonly-outbound'")
	}

	if r.SignerRole == OutboundWatchOnlyRole && r.TLSCertPath != "" {
		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"remote signer not be set if the signerrole " +
			"is set to 'watchonly-outbound'")
	}

	return nil
}
