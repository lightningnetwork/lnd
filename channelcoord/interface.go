package channelcoord

import (
	"net"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanstate"
)

// Coordinator is the aggregate channel/link-node coordination contract for
// operations that span multiple storage domains.
type Coordinator interface {
	// LinkNodeMaintainer owns link-node maintenance derived from channel
	// state.
	LinkNodeMaintainer

	// ChannelLifecycle owns operations that keep channel state and
	// link-node metadata in sync.
	ChannelLifecycle
}

// LinkNodeMaintainer owns link-node maintenance derived from channel state.
type LinkNodeMaintainer interface {
	// PruneLinkNodes attempts to prune all link nodes found within the
	// database with whom we no longer have any open channels with.
	PruneLinkNodes() error

	// RepairLinkNodes scans all channels in the database and ensures
	// that a link node exists for each remote peer. This should be
	// called on startup to ensure that our database is consistent.
	RepairLinkNodes(network wire.BitcoinNet) error
}

// ChannelLifecycle owns operations that keep channel state and link-node
// metadata in sync across a channel's lifetime.
type ChannelLifecycle interface {
	// SyncPendingChannel writes a pending channel to the store, records the
	// funding broadcast height, and creates the related link node metadata.
	SyncPendingChannel(channel *chanstate.OpenChannel, addr net.Addr,
		pendingHeight uint32) error

	// RestoreChannelShells reconstructs the state of an OpenChannel from
	// the ChannelShell. It writes the channel state and recreates link-node
	// metadata for the restored channels.
	RestoreChannelShells(channelShells ...*chanstate.ChannelShell) error

	// MarkChanFullyClosed marks a channel as fully closed and prunes the
	// related link node when no other open channels remain for the peer.
	MarkChanFullyClosed(chanPoint *wire.OutPoint) error
}
