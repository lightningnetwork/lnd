package rpcwallet

import (
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

const (
	// signerBuildTag and walletBuildTag are the names of the build tags
	// that gate the sub-servers required to service remote signing
	// requests. They are spelled out here rather than referenced from the
	// sub-server packages, as those packages only export their identifiers
	// when the corresponding tag is enabled, which is exactly the case we
	// need to produce an error message for.
	signerBuildTag = "signrpc"
	walletBuildTag = "walletrpc"
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

// Build creates a new RemoteSignerClient instance. If the configuration enables
// an outbound remote signer, a new OutboundRemoteSignerClient will be returned.
// Else, a NoOpClient will be returned.
//
// If the node has been configured to act as a remote signer but the sub-servers
// required to service signing requests are unavailable, an error is returned
// rather than a NoOpClient. Degrading to a no-op in that case would leave the
// signer node running and reporting healthy while never connecting to the
// watch-only node, which in turn leaves the watch-only node waiting for a
// signer connection that is never coming, with nothing in either node's logs at
// the default log level to explain why.
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

	// Collect the build tags of any required sub-server that isn't
	// available, so that we can name them specifically if the node did ask
	// to act as a remote signer.
	var missingTags []string
	if walletServer == nil {
		missingTags = append(missingTags, walletBuildTag)
	}
	if signerServer == nil {
		missingTags = append(missingTags, signerBuildTag)
	}

	// Check if we have all servers and if the configuration enables an
	// outbound remote signer. If not, return a NoOpClient.
	if len(missingTags) > 0 {
		// We cannot service any signing requests without these
		// sub-servers, so if the node was configured to act as a remote
		// signer, this is a misconfiguration we must surface instead of
		// silently ignoring it.
		if b.cfg.IsSignerNode() {
			return nil, fmt.Errorf("unable to act as a remote "+
				"signer node: lnd was built without the %s "+
				"sub-server(s), which are required to service "+
				"signing requests. Rebuild lnd with the %q "+
				"and %q build tags enabled (e.g. 'make build "+
				"rpc=1'), or use an official release build",
				strings.Join(missingTags, " and "),
				signerBuildTag, walletBuildTag)
		}

		log.Debugf("Using a No Op remote signer client due to " +
			"current sub-server support")

		return &NoOpClient{}, nil
	}

	if !b.cfg.IsSignerNode() {
		log.Debugf("Using a No Op remote signer client due to the " +
			"current watchonly config")

		return &NoOpClient{}, nil
	}

	// An outbound remote signer client is enabled, therefore we create one.
	log.Debugf("Using an outbound remote signer client")

	streamFeeder := NewStreamFeeder(b.cfg.ConnectionCfg)

	return NewOutboundClient(
		walletServer, signerServer, streamFeeder,
		b.cfg.ExperimentalRequestTimeout,
	)
}
