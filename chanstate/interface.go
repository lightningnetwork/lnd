package chanstate

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Store is the full persistence contract for the channel-state subsystem.
// Consumers depend on this interface rather than the concrete
// channeldb.ChannelStateDB so the underlying storage can be swapped without
// touching call sites.
//
// NOTE: This is named Store instead of DB to avoid confusion with the existing
// concrete channeldb.ChannelStateDB type during the migration. Once the channel
// state implementation moves into this package and the old concrete type is no
// longer part of consumer-facing code, this name can be revisited.
type Store[Channel any] interface {
	// OpenChannelStore owns open-channel records.
	OpenChannelStore[Channel]

	// HistoricalChannelStore owns the post-close historical channel view.
	HistoricalChannelStore[Channel]

	// OpenChannelLifecycleStore owns persisted lifecycle state for open
	// channel records.
	OpenChannelLifecycleStore[Channel]

	// ClosedChannelStore owns closed-channel summaries and lifecycle
	// mutations.
	ClosedChannelStore[Channel]

	// FinalHTLCStore owns final HTLC outcome data.
	FinalHTLCStore

	// ChannelSetupStore owns temporary state used while setting up a
	// channel.
	ChannelSetupStore

	// LinkNodeMaintainer owns link-node maintenance derived from channel
	// state.
	LinkNodeMaintainer
}

// OpenChannelStore owns open-channel records.
type OpenChannelStore[Channel any] interface {
	// FetchOpenChannels starts a new database transaction and returns
	// all stored currently active/open channels associated with the
	// target nodeID. In the case that no active channels are known to
	// have been created with this node, then a zero-length slice is
	// returned.
	FetchOpenChannels(nodeID *btcec.PublicKey) ([]Channel, error)

	// FetchChannel attempts to locate a channel specified by the passed
	// channel point. If the channel cannot be found, then an error will
	// be returned.
	FetchChannel(chanPoint wire.OutPoint) (Channel, error)

	// FetchChannelByID attempts to locate a channel specified by the
	// passed channel ID. If the channel cannot be found, then an error
	// will be returned.
	FetchChannelByID(id lnwire.ChannelID) (Channel, error)

	// FetchAllChannels attempts to retrieve all open channels currently
	// stored within the database, including pending open, fully open and
	// channels waiting for a closing transaction to confirm.
	FetchAllChannels() ([]Channel, error)

	// FetchAllOpenChannels will return all channels that have the
	// funding transaction confirmed, and is not waiting for a closing
	// transaction to be confirmed.
	FetchAllOpenChannels() ([]Channel, error)

	// FetchPendingChannels will return channels that have completed the
	// process of generating and broadcasting funding transactions, but
	// whose funding transactions have yet to be confirmed on the
	// blockchain.
	FetchPendingChannels() ([]Channel, error)

	// FetchWaitingCloseChannels will return all channels that have been
	// opened, but are now waiting for a closing transaction to be
	// confirmed.
	//
	// NOTE: This includes channels that are also pending to be opened.
	FetchWaitingCloseChannels() ([]Channel, error)

	// FetchPermAndTempPeers returns a map where the key is the remote
	// node's public key and the value is a struct that has a tally of
	// the pending-open channels and whether the peer has an open or
	// closed channel with us.
	FetchPermAndTempPeers(chainHash []byte) (map[string]ChanCount, error)

	// RestoreChannelShells reconstructs the state of an OpenChannel from
	// the ChannelShell. We'll attempt to write the new channel to disk,
	// create a LinkNode instance with the passed node addresses, and
	// finally create an edge within the graph for the channel as well.
	// This method is idempotent, so repeated calls with the same set of
	// channel shells won't modify the database after the initial call.
	RestoreChannelShells(channelShells ...*ChannelShell[Channel]) error
}

// HistoricalChannelStore owns the post-close historical channel view.
type HistoricalChannelStore[Channel any] interface {
	// FetchHistoricalChannel fetches open channel data from the
	// historical channel bucket.
	FetchHistoricalChannel(outPoint *wire.OutPoint) (Channel, error)
}

// OpenChannelLifecycleStore owns persisted lifecycle state for open channel
// records.
type OpenChannelLifecycleStore[Channel any] interface {
	// RefreshChannel updates the in-memory channel state using the latest
	// state observed on disk.
	RefreshChannel(channel Channel) error

	// MarkChannelConfirmationHeight updates the channel's confirmation
	// height once the channel opening transaction receives one
	// confirmation.
	MarkChannelConfirmationHeight(channel Channel, height uint32) error

	// MarkChannelCloseConfirmationHeight updates the channel's close
	// confirmation height when the closing transaction is first detected
	// in a block.
	MarkChannelCloseConfirmationHeight(channel Channel,
		height fn.Option[uint32]) error

	// MarkChannelOpen marks a channel as fully open given a locator that
	// uniquely describes its location within the chain.
	MarkChannelOpen(channel Channel, openLoc lnwire.ShortChannelID) error

	// MarkChannelRealScid marks the zero-conf channel's confirmed
	// ShortChannelID.
	MarkChannelRealScid(channel Channel,
		realScid lnwire.ShortChannelID) error

	// MarkChannelScidAliasNegotiated marks that the scid-alias feature
	// bit was negotiated during the lifetime of the channel.
	MarkChannelScidAliasNegotiated(channel Channel) error
}

// ClosedChannelStore owns closed-channel summaries and lifecycle mutations.
type ClosedChannelStore[Channel any] interface {
	// FetchClosedChannels attempts to fetch all closed channels from the
	// database. The pendingOnly bool toggles if channels that aren't yet
	// fully closed should be returned in the response or not. When a
	// channel was cooperatively closed, it becomes fully closed after a
	// single confirmation. When a channel was forcibly closed, it will
	// become fully closed after _all_ the pending funds (if any) have
	// been swept.
	FetchClosedChannels(pendingOnly bool) (
		[]*ChannelCloseSummary, error)

	// FetchClosedChannel queries for a channel close summary using the
	// channel point of the channel in question.
	FetchClosedChannel(chanID *wire.OutPoint) (
		*ChannelCloseSummary, error)

	// FetchClosedChannelForID queries for a channel close summary using
	// the channel ID of the channel in question.
	FetchClosedChannelForID(cid lnwire.ChannelID) (
		*ChannelCloseSummary, error)

	// MarkChanFullyClosed marks a channel as fully closed within the
	// database. A channel should be marked as fully closed if the
	// channel was initially cooperatively closed and it's reached a
	// single confirmation, or after all the pending funds in a channel
	// that has been forcibly closed have been swept.
	MarkChanFullyClosed(chanPoint *wire.OutPoint) error

	// CloseChannel marks the given channel as closed: the open-channel
	// record is removed and the supplied ChannelCloseSummary is
	// archived so the channel becomes retrievable via
	// FetchClosedChannel and FetchClosedChannelForID. Any ChannelStatus
	// values are merged into the archived summary. Returns
	// ErrChannelCloseSummaryNil if summary is nil.
	CloseChannel(channel Channel, summary *ChannelCloseSummary,
		statuses ...ChannelStatus) error

	// AbandonChannel attempts to remove the target channel from the open
	// channel database. If the channel was already removed (has a closed
	// channel entry), then we'll return a nil error. Otherwise, we'll
	// insert a new close summary into the database.
	AbandonChannel(chanPoint *wire.OutPoint, bestHeight uint32) error
}

// FinalHTLCStore owns final HTLC outcome data.
type FinalHTLCStore interface {
	// LookupFinalHtlc retrieves a final htlc resolution from the
	// database. If the htlc has no final resolution yet, ErrHtlcUnknown
	// is returned.
	LookupFinalHtlc(chanID lnwire.ShortChannelID,
		htlcIndex uint64) (*FinalHtlcInfo, error)

	// PutOnchainFinalHtlcOutcome stores the final on-chain outcome of an
	// htlc in the database.
	PutOnchainFinalHtlcOutcome(chanID lnwire.ShortChannelID,
		htlcID uint64, settled bool) error
}

// ChannelSetupStore owns temporary state used while setting up a channel. This
// state should be deleted once the link comes up.
type ChannelSetupStore interface {
	// SaveChannelOpeningState saves the serialized channel state for the
	// provided chanPoint to the channelOpeningStateBucket.
	SaveChannelOpeningState(outPoint, serializedState []byte) error

	// GetChannelOpeningState fetches the serialized channel state for
	// the provided outPoint from the database, or returns
	// ErrChannelNotFound if the channel is not found.
	GetChannelOpeningState(outPoint []byte) ([]byte, error)

	// DeleteChannelOpeningState removes any state for outPoint from the
	// database.
	DeleteChannelOpeningState(outPoint []byte) error

	// SaveInitialForwardingPolicy saves the serialized forwarding policy
	// for the provided permanent channel id.
	SaveInitialForwardingPolicy(chanID lnwire.ChannelID,
		forwardingPolicy *models.ForwardingPolicy) error

	// GetInitialForwardingPolicy fetches the serialized forwarding policy
	// for the provided channel id from the database, or returns
	// ErrChannelNotFound if a forwarding policy for this channel id is not
	// found.
	GetInitialForwardingPolicy(chanID lnwire.ChannelID) (
		*models.ForwardingPolicy, error)

	// DeleteInitialForwardingPolicy removes the forwarding policy for a
	// given channel from the database.
	DeleteInitialForwardingPolicy(chanID lnwire.ChannelID) error
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
