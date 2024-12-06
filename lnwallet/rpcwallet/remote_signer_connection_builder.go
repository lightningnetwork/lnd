package rpcwallet

import (
	"context"
	"errors"

	"github.com/lightningnetwork/lnd/lncfg"
)

// RemoteSignerConnectionBuilder creates instances of the RemoteSignerConnection
// interface, based on the provided configuration.
type RemoteSignerConnectionBuilder struct {
	cfg *lncfg.RemoteSigner
}

// NewRemoteSignerConnectionBuilder creates a new instance of the
// RemoteSignerBuilder.
func NewRemoteSignerConnectionBuilder(
	cfg *lncfg.RemoteSigner) *RemoteSignerConnectionBuilder {

	return &RemoteSignerConnectionBuilder{cfg}
}

// Build creates a new RemoteSignerConnection instance. If the configuration
// specifies that an inbound remote signer should be used, a new
// OutboundConnection is created. If the configuration specifies that an
// outbound remote signer should be used, a new InboundConnection is created.
// The function returns the created RemoteSignerConnection instance, and a
// cleanup function that should be called when the RemoteSignerConnection is no
// longer needed.
func (b *RemoteSignerConnectionBuilder) Build(
	ctx context.Context) (RemoteSignerConnection, error) {

	if !b.cfg.Enable {
		// This should be unreachable, but this is an extra sanity check
		return nil, errors.New("remote signer not enabled in " +
			"config")
	}

	// Create the remote signer based on the configuration.
	if !b.cfg.AllowInboundConnection {
		return NewOutboundConnection(ctx, b.cfg.ConnectionCfg)
	}

	inboundConnection := NewInboundConnection(
		b.cfg.RequestTimeout, b.cfg.Timeout,
	)

	return inboundConnection, nil
}
