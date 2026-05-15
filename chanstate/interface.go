package chanstate

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
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
type Store interface {
	// OpenChannelStore owns open-channel records.
	OpenChannelStore

	// HistoricalChannelStore owns the post-close historical channel view.
	HistoricalChannelStore

	// OpenChannelLifecycleStore owns persisted lifecycle state for open
	// channel records.
	OpenChannelLifecycleStore

	// OpenChannelStatusStore owns persisted status flags for open channel
	// records.
	OpenChannelStatusStore

	// OpenChannelShutdownStore owns persisted shutdown state.
	OpenChannelShutdownStore

	// OpenChannelCloseTxStore owns persisted closing transaction state.
	OpenChannelCloseTxStore

	// OpenChannelCommitmentStore owns persisted commitment state for open
	// channel records.
	OpenChannelCommitmentStore

	// OpenChannelFwdPkgStore owns forwarding packages tied to open
	// channel records.
	OpenChannelFwdPkgStore

	// ClosedChannelStore owns closed-channel summaries and lifecycle
	// mutations.
	ClosedChannelStore

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
type OpenChannelStore interface {
	// FetchOpenChannels starts a new database transaction and returns
	// all stored currently active/open channels associated with the
	// target nodeID. In the case that no active channels are known to
	// have been created with this node, then a zero-length slice is
	// returned.
	FetchOpenChannels(nodeID *btcec.PublicKey) ([]*OpenChannel, error)

	// FetchChannel attempts to locate a channel specified by the passed
	// channel point. If the channel cannot be found, then an error will
	// be returned.
	FetchChannel(chanPoint wire.OutPoint) (*OpenChannel, error)

	// FetchChannelByID attempts to locate a channel specified by the
	// passed channel ID. If the channel cannot be found, then an error
	// will be returned.
	FetchChannelByID(id lnwire.ChannelID) (*OpenChannel, error)

	// FetchAllChannels attempts to retrieve all open channels currently
	// stored within the database, including pending open, fully open and
	// channels waiting for a closing transaction to confirm.
	FetchAllChannels() ([]*OpenChannel, error)

	// FetchAllOpenChannels will return all channels that have the
	// funding transaction confirmed, and is not waiting for a closing
	// transaction to be confirmed.
	FetchAllOpenChannels() ([]*OpenChannel, error)

	// FetchPendingChannels will return channels that have completed the
	// process of generating and broadcasting funding transactions, but
	// whose funding transactions have yet to be confirmed on the
	// blockchain.
	FetchPendingChannels() ([]*OpenChannel, error)

	// FetchWaitingCloseChannels will return all channels that have been
	// opened, but are now waiting for a closing transaction to be
	// confirmed.
	//
	// NOTE: This includes channels that are also pending to be opened.
	FetchWaitingCloseChannels() ([]*OpenChannel, error)

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
	RestoreChannelShells(channelShells ...*ChannelShell) error
}

// HistoricalChannelStore owns the post-close historical channel view.
type HistoricalChannelStore interface {
	// FetchHistoricalChannel fetches open channel data from the
	// historical channel bucket.
	FetchHistoricalChannel(outPoint *wire.OutPoint) (*OpenChannel, error)
}

// OpenChannelLifecycleStore owns persisted lifecycle state for open channel
// records.
type OpenChannelLifecycleStore interface {
	// SyncPendingChannel writes a pending channel to the store and records
	// the funding broadcast height.
	SyncPendingChannel(channel *OpenChannel, addr net.Addr,
		pendingHeight uint32) error

	// RefreshChannel updates the in-memory channel state using the latest
	// state observed on disk.
	RefreshChannel(channel *OpenChannel) error

	// MarkChannelConfirmationHeight updates the channel's confirmation
	// height once the channel opening transaction receives one
	// confirmation.
	MarkChannelConfirmationHeight(channel *OpenChannel, height uint32) error

	// MarkChannelCloseConfirmationHeight updates the channel's close
	// confirmation height when the closing transaction is first detected
	// in a block.
	MarkChannelCloseConfirmationHeight(channel *OpenChannel,
		height fn.Option[uint32]) error

	// MarkChannelOpen marks a channel as fully open given a locator that
	// uniquely describes its location within the chain.
	MarkChannelOpen(channel *OpenChannel,
		openLoc lnwire.ShortChannelID) error

	// MarkChannelRealScid marks the zero-conf channel's confirmed
	// ShortChannelID.
	MarkChannelRealScid(channel *OpenChannel,
		realScid lnwire.ShortChannelID) error

	// MarkChannelScidAliasNegotiated marks that the scid-alias feature
	// bit was negotiated during the lifetime of the channel.
	MarkChannelScidAliasNegotiated(channel *OpenChannel) error
}

// OpenChannelStatusStore owns persisted status flags for open channel records.
type OpenChannelStatusStore interface {
	// ApplyChannelStatus adds the target status to the channel's
	// persisted status bit field.
	ApplyChannelStatus(channel *OpenChannel, status ChannelStatus) error

	// ClearChannelStatus clears the target status from the channel's
	// persisted status bit field.
	ClearChannelStatus(channel *OpenChannel, status ChannelStatus) error

	// MarkChannelDataLoss marks the channel as local-data-loss and stores
	// the commit point needed if the remote force closes.
	MarkChannelDataLoss(channel *OpenChannel,
		commitPoint *btcec.PublicKey) error

	// FetchChannelDataLossCommitPoint retrieves the commit point stored
	// when the channel was marked as local-data-loss.
	FetchChannelDataLossCommitPoint(channel *OpenChannel) (
		*btcec.PublicKey, error)

	// MarkChannelBorked marks the channel as irreconcilable.
	MarkChannelBorked(channel *OpenChannel) error
}

// OpenChannelShutdownStore owns persisted shutdown state.
type OpenChannelShutdownStore interface {
	// StoreChannelShutdownInfo persists the ShutdownInfo for the target
	// channel.
	StoreChannelShutdownInfo(channel *OpenChannel, info *ShutdownInfo) error

	// FetchChannelShutdownInfo fetches the persisted ShutdownInfo for the
	// target channel.
	FetchChannelShutdownInfo(channel *OpenChannel) (
		fn.Option[ShutdownInfo], error)
}

// OpenChannelCloseTxStore owns persisted closing transaction state.
type OpenChannelCloseTxStore interface {
	// MarkChannelCommitmentBroadcasted marks the channel as having a
	// commitment transaction broadcast.
	MarkChannelCommitmentBroadcasted(channel *OpenChannel,
		closeTx *wire.MsgTx, closer lntypes.ChannelParty) error

	// MarkChannelCoopBroadcasted marks the channel as having a
	// cooperative close transaction broadcast.
	MarkChannelCoopBroadcasted(channel *OpenChannel, closeTx *wire.MsgTx,
		closer lntypes.ChannelParty) error

	// FetchChannelBroadcastedCommitment fetches the stored unilateral
	// closing transaction.
	FetchChannelBroadcastedCommitment(channel *OpenChannel) (*wire.MsgTx,
		error)

	// FetchChannelBroadcastedCooperative fetches the stored cooperative
	// closing transaction.
	FetchChannelBroadcastedCooperative(channel *OpenChannel) (*wire.MsgTx,
		error)
}

// OpenChannelCommitmentStore owns persisted commitment state for open channel
// records.
type OpenChannelCommitmentStore interface {
	OpenChannelCommitmentMutationStore
	OpenChannelCommitmentQueryStore
}

// OpenChannelCommitmentMutationStore owns persisted commitment mutations for
// open channel records.
type OpenChannelCommitmentMutationStore interface {
	// UpdateChannelCommitment updates the local commitment state. It
	// locks in pending local updates received from the remote party and
	// persists remote log updates that have been acked, but not signed
	// for yet. The returned map contains all HTLC resolutions locked into
	// this commitment, keyed by HTLC index.
	UpdateChannelCommitment(channel *OpenChannel,
		newCommitment *ChannelCommitment,
		unsignedAckedUpdates []LogUpdate) (map[uint64]bool, error)

	// AppendRemoteCommitChain appends a new CommitDiff to the remote
	// party's commitment chain. This is used after preparing a new remote
	// commitment state, before transmitting it to the remote party.
	AppendRemoteCommitChain(channel *OpenChannel, diff *CommitDiff) error

	// RemoteCommitChainTip returns the "tip" of the current remote
	// commitment chain.
	RemoteCommitChainTip(channel *OpenChannel) (*CommitDiff, error)

	// UnsignedAckedUpdates retrieves the persisted unsigned acked remote
	// log updates that still need to be signed for.
	UnsignedAckedUpdates(channel *OpenChannel) ([]LogUpdate, error)

	// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local
	// log updates that the remote still needs to sign for.
	RemoteUnsignedLocalUpdates(channel *OpenChannel) ([]LogUpdate, error)

	// InsertNextRevocation inserts the next commitment point into the
	// persisted channel state.
	InsertNextRevocation(channel *OpenChannel,
		revKey *btcec.PublicKey) error

	// AdvanceCommitChainTail records the new state transition within the
	// revocation log and promotes the pending remote commitment to the
	// current remote commitment.
	AdvanceCommitChainTail(channel *OpenChannel, fwdPkg *FwdPkg,
		updates []LogUpdate, ourOutputIndex,
		theirOutputIndex uint32) error
}

// OpenChannelCommitmentQueryStore owns persisted commitment queries for open
// channel records.
type OpenChannelCommitmentQueryStore interface {
	// CommitmentHeight returns the current persisted commitment height.
	CommitmentHeight(channel *OpenChannel) (uint64, error)

	// LatestCommitments returns the two latest commitments for both the
	// local and remote party.
	LatestCommitments(channel *OpenChannel) (*ChannelCommitment,
		*ChannelCommitment, error)

	// RemoteRevocationStore returns the most up to date commitment version
	// of the revocation storage tree for the remote party.
	RemoteRevocationStore(channel *OpenChannel) (shachain.Store, error)

	// FindPreviousState scans through the append-only log in an attempt to
	// recover the previous channel state indicated by the update number.
	FindPreviousState(channel *OpenChannel, updateNum uint64) (
		*RevocationLog, *ChannelCommitment, error)
}

// OpenChannelFwdPkgStore owns forwarding packages tied to open channel
// records.
type OpenChannelFwdPkgStore interface {
	// LoadFwdPkgs loads forwarding packages that have not been processed.
	LoadFwdPkgs(channel *OpenChannel) ([]*FwdPkg, error)

	// AckAddHtlcs marks add HTLCs in forwarding packages as resolved.
	AckAddHtlcs(channel *OpenChannel, addRefs ...AddRef) error

	// AckSettleFails marks settles or fails as delivered to the incoming
	// link.
	AckSettleFails(channel *OpenChannel,
		settleFailRefs ...SettleFailRef) error

	// SetFwdFilter writes the forwarding filter for the forwarding package
	// identified by height.
	SetFwdFilter(channel *OpenChannel, height uint64,
		fwdFilter *PkgFilter) error

	// RemoveFwdPkgs removes forwarding packages by remote commitment
	// height.
	RemoveFwdPkgs(channel *OpenChannel, heights ...uint64) error
}

// ClosedChannelStore owns closed-channel summaries and lifecycle mutations.
type ClosedChannelStore interface {
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
	CloseChannel(channel *OpenChannel, summary *ChannelCloseSummary,
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
