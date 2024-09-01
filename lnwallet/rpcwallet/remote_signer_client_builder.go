package rpcwallet

import (
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

type rscBuilder = RemoteSignerClientBuilder

// RemoteSignerClientBuilder is creates instances of the RemoteSignerClient
// interface, based on the provided configuration.
type RemoteSignerClientBuilder struct {
	cfg *lncfg.RemoteSigner
}

// NewRemoteSignerClientBuilder creates a new instance of the
// RemoteSignerClientBuilder.
func NewRemoteSignerClientBuilder(cfg *lncfg.RemoteSigner) *rscBuilder {
	return &rscBuilder{cfg}
}

// Build creates a new RemoteSignerClient instance. If the configuration enables
// an outbound remote signer, a new OutboundRemoteSignerClient will be returned.
// Else, a NoOpClient will be returned.
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

	// Check if we have all servers and if the configuration enables an
	// outbound remote signer. If not, return a NoOpClient.
	if walletServer == nil || signerServer == nil {
		log.Debugf("Using a No Op remote signer client due to " +
			"current sub-server support")

		return &NoOpClient{}, nil
	}

	if b.cfg == nil || b.cfg.SignerType != lncfg.SignerClientType ||
		b.cfg.RPCHost == "" || b.cfg.MacaroonPath == "" ||
		b.cfg.TLSCertPath == "" || b.cfg.RequestTimeout <= 0 {

		log.Debugf("Using a No Op remote signer client due to " +
			"current remote signer config")

		return &NoOpClient{}, nil
	}

	// An outbound remote signer client is enabled, therefore we create one.
	log.Debugf("Using an outbound remote signer client")

	streamFeeder := NewStreamFeeder(
		b.cfg.RPCHost, b.cfg.MacaroonPath, b.cfg.TLSCertPath,
		b.cfg.Timeout,
	)

	rsClient, err := NewOutboundClient(
		walletServer, signerServer, streamFeeder, b.cfg.RequestTimeout,
	)
	if err != nil {
		return nil, err
	}

	return rsClient, nil
}
