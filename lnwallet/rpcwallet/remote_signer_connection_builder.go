package rpcwallet

import (
	"context"
	"errors"

	"github.com/lightningnetwork/lnd/lncfg"
)

// BuildRemoteSignerConnection creates a new RemoteSignerConnection instance.
// If the configuration specifies that an inbound remote signer should be used,
// a new OutboundConnection is created. If the configuration specifies that an
// outbound remote signer should be used, a new InboundConnection is created.
// The function returns the created RemoteSignerConnection instance, and a
// cleanup function that should be called when the RemoteSignerConnection is no
// longer needed.
func BuildRemoteSignerConnection(ctx context.Context,
	cfg *lncfg.RemoteSigner) (RemoteSignerConnection, error) {

	if !cfg.Enable {
		// This should be unreachable, but this is an extra sanity check
		return nil, errors.New("remote signer not enabled in " +
			"config")
	}

	return NewOutboundConnection(ctx, cfg.ConnectionCfg)
}
