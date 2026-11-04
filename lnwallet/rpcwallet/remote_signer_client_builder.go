package rpcwallet

import (
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

type rscBuilder = RemoteSignerClientBuilder

// RemoteSignerClientBuilder creates instances of the RemoteSignerClient
// interface, based on the provided configuration.
type RemoteSignerClientBuilder struct {
	cfg *lncfg.WatchOnlyNode
}

// NewRemoteSignerClientBuilder creates a new instance of the
// RemoteSignerClientBuilder.
func NewRemoteSignerClientBuilder(cfg *lncfg.WatchOnlyNode) *rscBuilder {
	return &rscBuilder{cfg}
}

// Build creates a new RemoteSignerClient instance. If this node is configured
// to act as a remote signer that dials the watch-only node, an OutboundClient
// is returned. Otherwise a NoOpClient is returned.
//
// NOTE: "outbound" here is from this node's point of view, as it is this node
// that dials out. The watch-only node calls the resulting connection inbound.
func (b *rscBuilder) Build(subServers []lnrpc.SubServer) (
	RemoteSignerClient, error) {

	var (
		walletServer walletrpc.WalletKitServer
		signerServer signrpc.SignerServer
	)

	for _, subServer := range subServers {
		if server, ok := subServer.(walletrpc.WalletKitServer); ok {
			walletServer = server
		}

		if server, ok := subServer.(signrpc.SignerServer); ok {
			signerServer = server
		}
	}

	// Check if we have all servers and if this node is configured to act as
	// a remote signer that dials the watch-only node. If not, return a
	// NoOpClient.
	if walletServer == nil || signerServer == nil {
		log.Debugf("Using a No Op remote signer client due to " +
			"current sub-server support")

		return &NoOpClient{}, nil
	}

	if !b.cfg.ExperimentalEnable {
		log.Debugf("Using a No Op remote signer client due to the " +
			"current watchonly config")

		return &NoOpClient{}, nil
	}

	// This node is configured to dial the watch-only node and serve signing
	// requests over that connection, so create the client that does it.
	log.Debugf("Using an outbound remote signer client")

	streamFeeder := NewStreamFeeder(b.cfg.ConnectionCfg)

	return NewOutboundClient(
		walletServer, signerServer, streamFeeder,
		b.cfg.ExperimentalRequestTimeout,
	)
}
