package rpcwallet

import (
	"context"
	"errors"

	"github.com/lightningnetwork/lnd/lncfg"
)

// BuildRemoteSignerConnection creates the watch-only node's side of the
// connection to the remote signer. Which implementation is returned depends on
// which of the two nodes dials the other:
//
//   - The watch-only node dials the signer (the default). The connection is
//     outbound as seen from here, so an OutboundConnection is returned. Such a
//     signer is configured as an "inbound" remote signer, because the same
//     connection is inbound as seen from the signer.
//
//   - The signer dials the watch-only node, which is what
//     remotesigner.experimentalallowinboundconnection selects. The connection
//     is inbound as seen from here, so an InboundConnection is returned, and
//     such a signer is configured as an "outbound" remote signer.
//
// The labels look inverted only because each one describes the direction from
// the point of view of the node using it, never from a single fixed vantage
// point.
func BuildRemoteSignerConnection(ctx context.Context,
	cfg *lncfg.RemoteSigner) (RemoteSignerConnection, error) {

	if !cfg.Enable {
		// This should be unreachable, but this is an extra sanity check
		return nil, errors.New("remote signer not enabled in " +
			"config")
	}

	// We dial the signer ourselves, so the connection is outbound from
	// here.
	if !cfg.ExperimentalAllowInboundConnection {
		return NewOutboundConnection(ctx, cfg.ConnectionCfg)
	}

	// Otherwise the signer dials us, so we set up the receiving side and
	// wait for it to connect.
	inboundConnection := NewInboundConnection(
		cfg.ConnectionCfg.ExperimentalRequestTimeout,
		cfg.ExperimentalStartupTimeout,
	)

	return inboundConnection, nil
}
