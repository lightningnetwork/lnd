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

	// DefaultInboundRemoteSignerType is the default type of remote signer
	// used when enabling a remote signer. It indicates that the remote
	// signer node allows inbound connections from the watch-only node.
	DefaultInboundRemoteSignerType = "inbound"

	// OutboundRemoteSignerType is a type of remote signer used when
	// enabling a remote signer. It indicates that the remote signer node
	// will make an outbound connection to the watch-only node to connect
	// the nodes.
	OutboundRemoteSignerType = "outbound"

	// SignerClientType indicates that the lnd instance will act as an
	// outbound remote signer, connecting to a watch-only node that has the
	// 'outbound' signer type set.
	SignerClientType = "signer"
)

// RemoteSigner holds the configuration options for a remote RPC signer.
//
//nolint:lll
type RemoteSigner struct {
	Enable           bool          `long:"enable" description:"Use a remote signer for signing any on-chain related transactions or messages. Only recommended if local wallet is initialized as watch-only. Remote signer must use the same seed/root key as the local watch-only wallet but must have private keys. This param should not be set to true when signertype is set to 'signer'"`
	SignerType       string        `long:"signertype" description:"Sets the type of remote signer to use, or signals that the node will act as a remote signer. Can be set to either 'inbound' (default), 'outbound' or 'signer'. 'inbound' means that a remote signer that allows inbound connections from the watch-only node is used. 'outbound' means that a remote signer node that makes an outbound connection to the watch-only node is used. 'signer' means the lnd instance will act as a remote signer, making an outbound connection to a watch-only node with the 'outbound' signertype set" choice:"inbound" choice:"outbound" choice:"signer"`
	RPCHost          string        `long:"rpchost" description:"The remote signer's or watch-only node's RPC host:port. For nodes which have the signertype set to 'inbound', this should be set to the remote signer node's RPC host:port. For nodes which have the signertype set to 'signer', this should be set to the watch-only node's RPC host:port. This param should not be set when signertype is set to 'outbound'"`
	MacaroonPath     string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote signer or the watch-only node. For nodes which have the signertype set to 'inbound', this should be set to the remote signer node's macaroon. For nodes which have the signertype set to 'signer', this should be set to the watch-only node's macaroon. This param should not be set when signertype is set to 'outbound'"`
	TLSCertPath      string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote signer's or watch-only node's identity. For nodes which have the signertype set to 'inbound', this should be set to the remote signer node's TLS certificate. For nodes which have the signertype set to 'signer', this should be set to the watch-only node's TLS certificate. This param should not be set when signertype is set to 'outbound'"`
	Timeout          time.Duration `long:"timeout" description:"The timeout for making the connection to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. Valid time units are {s, m, h}"`
	RequestTimeout   time.Duration `long:"requesttimeout" description:"The time we will wait when making requests to the remote signer or watch-only node, depending on whether the node acts as a watch-only node or a signer. This parameter will have no effect if signertype is set to 'inbound'. Valid time units are {s, m, h}."`
	MigrateWatchOnly bool          `long:"migrate-wallet-to-watch-only" description:"If a wallet with private key material already exists, migrate it into a watch-only wallet on first startup. WARNING: This cannot be undone! Make sure you have backed up your seed before you use this flag! All private keys will be purged from the wallet after first unlock with this flag!"`
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteSigner) Validate() error {
	if r.SignerType == OutboundRemoteSignerType {
		return fmt.Errorf("remote signer: the set signertype \"%v\" "+
			"is not yet supported", r.SignerType)
	}

	if r.Timeout < time.Millisecond {
		return fmt.Errorf("remote signer: timeout of %v is invalid, "+
			"cannot be smaller than %v", r.Timeout,
			time.Millisecond)
	}

	if r.RequestTimeout < time.Millisecond {
		return fmt.Errorf("remote signer: requesttimeout of %v is "+
			"invalid, cannot be smaller than %v",
			r.Timeout, time.Millisecond)
	}

	if r.MigrateWatchOnly && !r.Enable {
		return fmt.Errorf("remote signer: cannot turn on wallet " +
			"migration to watch-only if remote signing is not " +
			"enabled")
	}

	if r.SignerType == SignerClientType && r.Enable {
		return fmt.Errorf("remote signer: do not set " +
			"remotesigner.enable when signertype is set to " +
			"'signer'")
	}

	if r.SignerType == SignerClientType && r.RPCHost == "" {
		return fmt.Errorf("remote signer: the rpchost for the " +
			"watch-only node should be set when the signertype " +
			"is set to signer")
	}

	if r.SignerType == SignerClientType && r.MacaroonPath == "" {
		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"watch-only node should be set when the signertype " +
			"is set to signer")
	}

	if r.SignerType == SignerClientType && r.TLSCertPath == "" {
		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"watch-only node should be set when the signertype " +
			"is set to signer")
	}

	if !r.Enable {
		return nil
	}

	if r.SignerType == DefaultInboundRemoteSignerType && r.RPCHost == "" {
		return fmt.Errorf("remote signer: the rpchost for the remote " +
			"signer should be set when using an inbound remote " +
			"signer")
	}

	if r.SignerType == DefaultInboundRemoteSignerType &&
		r.MacaroonPath == "" {

		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"remote signer should be set when using an inbound " +
			"remote signer")
	}

	if r.SignerType == DefaultInboundRemoteSignerType &&
		r.TLSCertPath == "" {

		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"remote signer should be set when using an inbound " +
			"remote signer")
	}

	if r.SignerType == OutboundRemoteSignerType && r.RPCHost != "" {
		return fmt.Errorf("remote signer: the rpchost for the remote " +
			"signer should not be set if the signertype is set " +
			"to outbound")
	}

	if r.SignerType == OutboundRemoteSignerType && r.MacaroonPath != "" {
		return fmt.Errorf("remote signer: the macaroonpath for the " +
			"remote signer should not be set if the signertype " +
			"is set to outbound")
	}

	if r.SignerType == OutboundRemoteSignerType && r.TLSCertPath != "" {
		return fmt.Errorf("remote signer: the tlscertpath for the " +
			"remote signer not be set if the signertype " +
			"is set to outbound")
	}

	return nil
}
