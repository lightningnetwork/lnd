package channeldb

import (
	"bytes"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	cstate "github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// HTLCBlindingPointTLV is the tlv type used for storing blinding
	// points with HTLCs.
	HTLCBlindingPointTLV tlv.Type = 0
)

const (
	// AbsoluteThawHeightThreshold is the threshold at which a thaw height
	// begins to be interpreted as an absolute block height, rather than a
	// relative one.
	AbsoluteThawHeightThreshold = cstate.AbsoluteThawHeightThreshold
)

var (
	closedChannelBucket     = cstate.ClosedChannelBucketKey()
	openChannelBucket       = cstate.OpenChannelBucketKey()
	outpointBucket          = cstate.OutpointBucketKey()
	chanIDBucket            = cstate.ChanIDBucketKey()
	historicalChannelBucket = cstate.HistoricalChannelBucketKey()
)

var (
	// ErrNoCommitmentsFound is returned when a channel has not set
	// commitment states.
	ErrNoCommitmentsFound = cstate.ErrNoCommitmentsFound

	// ErrNoChanInfoFound is returned when a particular channel does not
	// have any channels state.
	ErrNoChanInfoFound = cstate.ErrNoChanInfoFound

	// ErrNoRevocationsFound is returned when revocation state for a
	// particular channel cannot be found.
	ErrNoRevocationsFound = cstate.ErrNoRevocationsFound

	// ErrNoPendingCommit is returned when there is not a pending
	// commitment for a remote party. A new commitment is written to disk
	// each time we write a new state in order to be properly fault
	// tolerant.
	ErrNoPendingCommit = cstate.ErrNoPendingCommit

	// ErrNoCommitPoint is returned when no data loss commit point is found
	// in the database.
	ErrNoCommitPoint = cstate.ErrNoCommitPoint

	// ErrNoCloseTx is returned when no closing tx is found for a channel
	// in the state CommitBroadcasted.
	ErrNoCloseTx = cstate.ErrNoCloseTx

	// ErrNoShutdownInfo is returned when no shutdown info has been
	// persisted for a channel.
	ErrNoShutdownInfo = cstate.ErrNoShutdownInfo

	// ErrNoRestoredChannelMutation is returned when a caller attempts to
	// mutate a channel that's been recovered.
	ErrNoRestoredChannelMutation = cstate.ErrNoRestoredChannelMutation

	// ErrChanBorked is returned when a caller attempts to mutate a borked
	// channel.
	ErrChanBorked = cstate.ErrChanBorked

	// ErrMissingIndexEntry is returned when a caller attempts to close a
	// channel and the outpoint is missing from the index.
	ErrMissingIndexEntry = cstate.ErrMissingIndexEntry

	// ErrOnionBlobLength is returned is an onion blob with incorrect
	// length is read from disk.
	ErrOnionBlobLength = cstate.ErrOnionBlobLength
)

type (
	indexStatus = cstate.IndexStatus

	// OpenChannel encapsulates the persistent and dynamic state of an open
	// channel with a remote node.
	OpenChannel = cstate.OpenChannel

	// ChannelCommitment is a snapshot of the commitment state at a
	// particular point in the commitment chain.
	ChannelCommitment = cstate.ChannelCommitment

	// HTLC is the on-disk representation of a hash time-locked contract.
	HTLC = cstate.HTLC

	// LogUpdate represents a pending update to the remote commitment
	// chain.
	LogUpdate = cstate.LogUpdate

	// CommitDiff represents the delta needed to apply the state
	// transition between two subsequent commitment states.
	CommitDiff = cstate.CommitDiff
)

const (
	indexStatusType = cstate.IndexStatusType
	outpointOpen    = cstate.OutpointOpen
	outpointClosed  = cstate.OutpointClosed
)

// isOutpointClosed reports whether the supplied chanKey has been flipped to
// outpointClosed in the supplied outpointBucket. The flip is performed in the
// same transaction as the rest of CloseChannel (sync and tombstone paths
// alike), so a true result is the authoritative "this channel went through
// CloseChannel" signal. On tombstone-enabled backends the chanBucket may still
// exist on disk; readers consult this helper to skip those entries. Callers
// fetch outpointBucket once and pass it in, which lets loop-style readers
// hoist the bucket lookup out of the inner loop.
func isOutpointClosed(opBucket kvdb.RBucket, chanKey []byte) (bool, error) {
	return cstate.IsOutpointClosed(opBucket, chanKey)
}

// ChannelType is an enum-like type that describes one of several possible
// channel types.
type ChannelType = cstate.ChannelType

const (
	// SingleFunderBit represents a channel wherein one party solely funds
	// the entire capacity of the channel.
	SingleFunderBit = cstate.SingleFunderBit

	// DualFunderBit represents a channel wherein both parties contribute
	// funds towards the total capacity of the channel.
	DualFunderBit = cstate.DualFunderBit

	// SingleFunderTweaklessBit is similar to the basic SingleFunder channel
	// type, but it omits the tweak for one's key.
	SingleFunderTweaklessBit = cstate.SingleFunderTweaklessBit

	// NoFundingTxBit denotes if we have the funding transaction locally on
	// disk.
	NoFundingTxBit = cstate.NoFundingTxBit

	// AnchorOutputsBit indicates that the channel makes use of anchor
	// outputs to bump the commitment transaction's effective feerate.
	AnchorOutputsBit = cstate.AnchorOutputsBit

	// FrozenBit indicates that the channel is a frozen channel, meaning
	// that only the responder can decide to cooperatively close the
	// channel.
	FrozenBit = cstate.FrozenBit

	// ZeroHtlcTxFeeBit indicates that the channel should use zero-fee
	// second-level HTLC transactions.
	ZeroHtlcTxFeeBit = cstate.ZeroHtlcTxFeeBit

	// LeaseExpirationBit indicates that the channel has been leased for a
	// period of time.
	LeaseExpirationBit = cstate.LeaseExpirationBit

	// ZeroConfBit indicates that the channel is a zero-conf channel.
	ZeroConfBit = cstate.ZeroConfBit

	// ScidAliasChanBit indicates that the channel has negotiated the
	// scid-alias channel type.
	ScidAliasChanBit = cstate.ScidAliasChanBit

	// ScidAliasFeatureBit indicates that the scid-alias feature bit was
	// negotiated during the lifetime of this channel.
	ScidAliasFeatureBit = cstate.ScidAliasFeatureBit

	// SimpleTaprootFeatureBit indicates that the simple-taproot-chans
	// feature bit was negotiated during the lifetime of the channel.
	SimpleTaprootFeatureBit = cstate.SimpleTaprootFeatureBit

	// TapscriptRootBit indicates that this is a MuSig2 channel with a top
	// level tapscript commitment.
	TapscriptRootBit = cstate.TapscriptRootBit

	// TaprootFinalBit indicates that this is a MuSig2 channel using the
	// final/production taproot scripts and feature bits 80/81.
	TaprootFinalBit = cstate.TaprootFinalBit
)

// ChannelStateBounds are the parameters from OpenChannel and AcceptChannel
// that bound the abstract channel state.
type ChannelStateBounds = cstate.ChannelStateBounds

// CommitmentParams are the parameters from OpenChannel and AcceptChannel that
// are required to render an abstract channel state to a concrete commitment
// transaction.
type CommitmentParams = cstate.CommitmentParams

// ChannelConfig houses the channel configuration for one side of a channel.
type ChannelConfig = cstate.ChannelConfig

// ChannelStatus is a bit vector used to indicate whether an OpenChannel is in
// the default usable state, or a state where it shouldn't be used.
type ChannelStatus = cstate.ChannelStatus

var (
	// ChanStatusDefault is the normal state of an open channel.
	ChanStatusDefault = cstate.ChanStatusDefault

	// ChanStatusBorked indicates that the channel has entered an
	// irreconcilable state.
	ChanStatusBorked = cstate.ChanStatusBorked

	// ChanStatusCommitBroadcasted indicates that a commitment for this
	// channel has been broadcasted.
	ChanStatusCommitBroadcasted = cstate.ChanStatusCommitBroadcasted

	// ChanStatusLocalDataLoss indicates that we have lost channel state
	// for this channel.
	ChanStatusLocalDataLoss = cstate.ChanStatusLocalDataLoss

	// ChanStatusRestored signals that the channel has been restored and
	// doesn't have all fields a typical channel will have.
	ChanStatusRestored = cstate.ChanStatusRestored

	// ChanStatusCoopBroadcasted indicates that a cooperative close for this
	// channel has been broadcasted.
	ChanStatusCoopBroadcasted = cstate.ChanStatusCoopBroadcasted

	// ChanStatusLocalCloseInitiator indicates that we initiated closing the
	// channel.
	ChanStatusLocalCloseInitiator = cstate.ChanStatusLocalCloseInitiator

	// ChanStatusRemoteCloseInitiator indicates that the remote node
	// initiated closing the channel.
	ChanStatusRemoteCloseInitiator = cstate.ChanStatusRemoteCloseInitiator
)

// FinalHtlcByte is a type alias for a byte that encodes information about the
// final htlc resolution.
type FinalHtlcByte = cstate.FinalHtlcByte

const (
	// FinalHtlcSettledBit is the bit that encodes whether the htlc was
	// settled or failed.
	FinalHtlcSettledBit = cstate.FinalHtlcSettledBit

	// FinalHtlcOffchainBit is the bit that encodes whether the htlc was
	// resolved offchain or onchain.
	FinalHtlcOffchainBit = cstate.FinalHtlcOffchainBit
)

// RefreshChannel updates the in-memory channel state using the latest state
// observed on disk.
func (c *ChannelStateDB) RefreshChannel(channel *OpenChannel) error {
	return kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// We'll re-populating the in-memory channel with the info
		// fetched from disk.
		if err := fetchChanInfo(chanBucket, channel); err != nil {
			return fmt.Errorf("unable to fetch chan info: %w", err)
		}

		// Also populate the channel's commitment states for both sides
		// of the channel.
		err = fetchChanCommitments(chanBucket, channel)
		if err != nil {
			return fmt.Errorf("unable to fetch chan commitments: "+
				"%v", err)
		}

		// Also retrieve the current revocation state.
		err = fetchChanRevocationState(chanBucket, channel)
		if err != nil {
			return fmt.Errorf("unable to fetch chan revocations: "+
				"%v", err)
		}

		return nil
	}, func() {})
}

// fetchChanBucket is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on.
func fetchChanBucket(tx kvdb.RTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (
	kvdb.RBucket, error) {

	return cstate.FetchChanBucket(tx, nodeKey, outPoint, chainHash)
}

func fetchFinalHtlcsBucketRw(tx kvdb.RwTx,
	chanID lnwire.ShortChannelID) (kvdb.RwBucket, error) {

	return cstate.FetchFinalHtlcsBucketRw(tx, chanID)
}

// fullSyncOpenChannel syncs the contents of an OpenChannel while re-using an
// existing database transaction.
func fullSyncOpenChannel(tx kvdb.RwTx, c *OpenChannel) error {
	return cstate.FullSyncOpenChannel(tx, c)
}

// MarkChannelConfirmationHeight updates the channel's confirmation height once
// the channel opening transaction receives one confirmation.
func (c *ChannelStateDB) MarkChannelConfirmationHeight(channel *OpenChannel,
	height uint32) error {

	return cstate.MarkChannelConfirmationHeight(c.backend, channel, height)
}

// MarkChannelCloseConfirmationHeight updates the channel's close confirmation
// height when the closing transaction is first detected in a block.
func (c *ChannelStateDB) MarkChannelCloseConfirmationHeight(
	channel *OpenChannel, height fn.Option[uint32]) error {

	return cstate.MarkChannelCloseConfirmationHeight(
		c.backend, channel, height,
	)
}

// MarkChannelOpen marks a channel as fully open given a locator that uniquely
// describes its location within the chain.
func (c *ChannelStateDB) MarkChannelOpen(channel *OpenChannel,
	openLoc lnwire.ShortChannelID) error {

	return cstate.MarkChannelOpen(c.backend, channel, openLoc)
}

// MarkChannelRealScid marks the zero-conf channel's confirmed ShortChannelID.
func (c *ChannelStateDB) MarkChannelRealScid(channel *OpenChannel,
	realScid lnwire.ShortChannelID) error {

	return cstate.MarkChannelRealScid(c.backend, channel, realScid)
}

// MarkChannelScidAliasNegotiated adds ScidAliasFeatureBit to ChanType in the
// database.
func (c *ChannelStateDB) MarkChannelScidAliasNegotiated(
	channel *OpenChannel) error {

	return cstate.MarkChannelScidAliasNegotiated(c.backend, channel)
}

// MarkChannelDataLoss marks the channel as local-data-loss and stores the
// commit point needed if the remote force closes.
func (c *ChannelStateDB) MarkChannelDataLoss(channel *OpenChannel,
	commitPoint *btcec.PublicKey) error {

	return cstate.MarkChannelDataLoss(c.backend, channel, commitPoint)
}

// FetchChannelDataLossCommitPoint retrieves the commit point stored when the
// channel was marked as local-data-loss.
func (c *ChannelStateDB) FetchChannelDataLossCommitPoint(
	channel *OpenChannel) (*btcec.PublicKey, error) {

	return cstate.FetchDataLossCommitPoint(c.backend, channel)
}

// MarkChannelBorked marks the channel as irreconcilable.
func (c *ChannelStateDB) MarkChannelBorked(channel *OpenChannel) error {
	return c.ApplyChannelStatus(channel, ChanStatusBorked)
}

var (
	// DeriveMusig2Shachain derives a shachain producer for the taproot
	// channel from normal shachain revocation root.
	DeriveMusig2Shachain = cstate.DeriveMusig2Shachain

	// NewMusigVerificationNonce generates the local or verification nonce
	// for another musig2 session.
	NewMusigVerificationNonce = cstate.NewMusigVerificationNonce
)

// StoreChannelShutdownInfo persists the ShutdownInfo for the target channel.
func (c *ChannelStateDB) StoreChannelShutdownInfo(channel *OpenChannel,
	info *ShutdownInfo) error {

	return cstate.StoreChannelShutdownInfo(c.backend, channel, info)
}

// FetchChannelShutdownInfo fetches the persisted ShutdownInfo for the target
// channel.
func (c *ChannelStateDB) FetchChannelShutdownInfo(
	channel *OpenChannel) (fn.Option[ShutdownInfo], error) {

	return cstate.FetchShutdownInfo(c.backend, channel)
}

// MarkChannelCommitmentBroadcasted marks the channel as having a commitment
// transaction broadcast.
func (c *ChannelStateDB) MarkChannelCommitmentBroadcasted(
	channel *OpenChannel, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return cstate.MarkChannelCommitmentBroadcasted(
		c.backend, channel, closeTx, closer,
	)
}

// MarkChannelCoopBroadcasted marks the channel as having a cooperative close
// transaction broadcast.
func (c *ChannelStateDB) MarkChannelCoopBroadcasted(channel *OpenChannel,
	closeTx *wire.MsgTx, closer lntypes.ChannelParty) error {

	return cstate.MarkChannelCoopBroadcasted(
		c.backend, channel, closeTx, closer,
	)
}

// FetchChannelBroadcastedCommitment fetches the stored unilateral closing
// transaction.
func (c *ChannelStateDB) FetchChannelBroadcastedCommitment(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return c.getClosingTx(channel, cstate.ForceCloseTxKey())
}

// FetchChannelBroadcastedCooperative fetches the stored cooperative closing
// transaction.
func (c *ChannelStateDB) FetchChannelBroadcastedCooperative(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return c.getClosingTx(channel, cstate.CoopCloseTxKey())
}

// getClosingTx returns the stored closing transaction for key. The caller
// should use either the force or coop closing keys.
func (c *ChannelStateDB) getClosingTx(channel *OpenChannel,
	key []byte) (*wire.MsgTx, error) {

	return cstate.FetchClosingTx(c.backend, channel, key)
}

// ApplyChannelStatus adds the target status to the channel's persisted status
// bit field.
func (c *ChannelStateDB) ApplyChannelStatus(channel *OpenChannel,
	status ChannelStatus) error {

	return cstate.ApplyChannelStatus(c.backend, channel, status)
}

// ClearChannelStatus clears the target status from the channel's persisted
// status bit field.
func (c *ChannelStateDB) ClearChannelStatus(channel *OpenChannel,
	status ChannelStatus) error {

	return cstate.ClearChannelStatus(c.backend, channel, status)
}

// putOpenChannel serializes, and stores the current state of the channel in its
// entirety.
func putOpenChannel(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	return cstate.PutOpenChannel(chanBucket, channel)
}

// fetchOpenChannel retrieves, and deserializes (including decrypting
// sensitive) the complete channel currently active with the passed nodeID.
func fetchOpenChannel(chanBucket kvdb.RBucket,
	chanPoint *wire.OutPoint) (*OpenChannel, error) {

	return cstate.FetchOpenChannel(chanBucket, chanPoint)
}

// SyncPendingChannel writes a pending channel to the store and records the
// funding broadcast height.
func (c *ChannelStateDB) SyncPendingChannel(channel *OpenChannel,
	addr net.Addr, pendingHeight uint32) error {

	channel.FundingBroadcastHeight = pendingHeight

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		return syncNewChannel(tx, channel, []net.Addr{addr}, c.backend)
	}, func() {})
}

// syncNewChannel will write the passed channel to disk, and also create a
// LinkNode (if needed) for the channel peer.
func syncNewChannel(tx kvdb.RwTx, c *OpenChannel, addrs []net.Addr,
	backend kvdb.Backend) error {

	// First, sync all the persistent channel state to disk.
	if err := fullSyncOpenChannel(tx, c); err != nil {
		return err
	}

	nodeInfoBucket, err := tx.CreateTopLevelBucket(nodeInfoBucket)
	if err != nil {
		return err
	}

	// If a LinkNode for this identity public key already exists,
	// then we can exit early.
	nodePub := c.IdentityPub.SerializeCompressed()
	if nodeInfoBucket.Get(nodePub) != nil {
		return nil
	}

	// Next, we need to establish a (possibly) new LinkNode relationship
	// for this channel. The LinkNode metadata contains reachability,
	// up-time, and service bits related information.
	linkNode := NewLinkNode(
		&LinkNodeDB{backend: backend}, wire.MainNet, c.IdentityPub,
		addrs...,
	)

	// TODO(roasbeef): do away with link node all together?

	return putLinkNode(nodeInfoBucket, linkNode)
}

// UpdateChannelCommitment updates the local commitment state.
func (c *ChannelStateDB) UpdateChannelCommitment(channel *OpenChannel,
	newCommitment *ChannelCommitment,
	unsignedAckedUpdates []LogUpdate) (map[uint64]bool, error) {

	return cstate.UpdateChannelCommitment(
		c.backend, channel, newCommitment, unsignedAckedUpdates,
		c.parent.storeFinalHtlcResolutions,
	)
}

// SerializeHtlcs writes out the passed set of HTLC's into the passed writer
// using the current default on-disk serialization format.
//
// This inline serialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) is serialized as var bytes in
//     WriteElements (ie, the length 1366 was written, followed by the 1366
//     onion bytes).
//   - To include extra data, we append any extra data present to this one
//     variable length of data. Since we know that the onion is strictly 1366
//     bytes, any length after that should be considered to be extra data.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func SerializeHtlcs(b io.Writer, htlcs ...HTLC) error {
	return cstate.SerializeHtlcs(b, htlcs...)
}

// DeserializeHtlcs attempts to read out a slice of HTLC's from the passed
// io.Reader. The bytes within the passed reader MUST have been previously
// written to using the SerializeHtlcs function.
//
// This inline deserialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) and any additional data present
//     are read out as a single blob of variable byte data.
//   - They are stored like this to take advantage of the variable space
//     available for extension without migration (see SerializeHtlcs).
//   - The first 1366 bytes are interpreted as the onion blob, and any remaining
//     bytes as extra HTLC data.
//   - This extra HTLC data is expected to be serialized as a TLV stream, and
//     its parsing is left to higher layers.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func DeserializeHtlcs(r io.Reader) ([]HTLC, error) {
	return cstate.DeserializeHtlcs(r)
}

func newChannelPackager(channel *OpenChannel) *ChannelPackager {
	return NewChannelPackager(channel.ShortChannelID)
}

// AppendRemoteCommitChain appends a new CommitDiff to the remote party's
// commitment chain.
func (c *ChannelStateDB) AppendRemoteCommitChain(channel *OpenChannel,
	diff *CommitDiff) error {

	return cstate.AppendRemoteCommitChain(c.backend, channel, diff)
}

// RemoteCommitChainTip returns the "tip" of the current remote commitment
// chain.
func (c *ChannelStateDB) RemoteCommitChainTip(channel *OpenChannel) (
	*CommitDiff, error) {

	return cstate.RemoteCommitChainTip(c.backend, channel)
}

// UnsignedAckedUpdates retrieves the persisted unsigned acked remote log
// updates that still need to be signed for.
func (c *ChannelStateDB) UnsignedAckedUpdates(channel *OpenChannel) (
	[]LogUpdate, error) {

	return cstate.UnsignedAckedUpdates(c.backend, channel)
}

// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local log
// updates that the remote still needs to sign for.
func (c *ChannelStateDB) RemoteUnsignedLocalUpdates(channel *OpenChannel) (
	[]LogUpdate, error) {

	return cstate.RemoteUnsignedLocalUpdates(c.backend, channel)
}

// InsertNextRevocation inserts the next commitment point into the persisted
// channel state.
func (c *ChannelStateDB) InsertNextRevocation(channel *OpenChannel,
	revKey *btcec.PublicKey) error {

	return cstate.InsertNextRevocation(c.backend, channel, revKey)
}

// AdvanceCommitChainTail records the new state transition within the
// revocation log and promotes the pending remote commitment to the current
// remote commitment.
func (c *ChannelStateDB) AdvanceCommitChainTail(channel *OpenChannel,
	fwdPkg *FwdPkg, updates []LogUpdate, ourOutputIndex,
	theirOutputIndex uint32) error {

	return cstate.AdvanceCommitChainTail(
		c.backend, channel, fwdPkg, updates, ourOutputIndex,
		theirOutputIndex, c.parent.noRevLogAmtData,
	)
}

// FinalHtlcInfo contains information about the final outcome of an htlc.
type FinalHtlcInfo = cstate.FinalHtlcInfo

// putFinalHtlc writes the final htlc outcome to the database. Additionally it
// records whether the htlc was resolved off-chain or on-chain.
func putFinalHtlc(finalHtlcsBucket kvdb.RwBucket, id uint64,
	info FinalHtlcInfo) error {

	return cstate.PutFinalHtlc(finalHtlcsBucket, id, info)
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in map indexed by the
// remote commitment height at which the updates were locked in.
func (c *ChannelStateDB) LoadFwdPkgs(channel *OpenChannel) ([]*FwdPkg,
	error) {

	return cstate.LoadFwdPkgs(c.backend, channel)
}

// AckAddHtlcs updates the AckAddFilter containing any of the provided AddRefs
// indicating that a response to this Add has been committed to the remote party.
// Doing so will prevent these Add HTLCs from being reforwarded internally.
func (c *ChannelStateDB) AckAddHtlcs(channel *OpenChannel,
	addRefs ...AddRef) error {

	return cstate.AckAddHtlcs(c.backend, channel, addRefs...)
}

// AckSettleFails updates the SettleFailFilter containing any of the provided
// SettleFailRefs, indicating that the response has been delivered to the
// incoming link, corresponding to a particular AddRef. Doing so will prevent
// the responses from being retransmitted internally.
func (c *ChannelStateDB) AckSettleFails(channel *OpenChannel,
	settleFailRefs ...SettleFailRef) error {

	return cstate.AckSettleFails(c.backend, channel, settleFailRefs...)
}

// SetFwdFilter atomically sets the forwarding filter for the forwarding package
// identified by `height`.
func (c *ChannelStateDB) SetFwdFilter(channel *OpenChannel, height uint64,
	fwdFilter *PkgFilter) error {

	return cstate.SetFwdFilter(c.backend, channel, height, fwdFilter)
}

// RemoveFwdPkgs atomically removes forwarding packages specified by the remote
// commitment heights. If one of the intermediate RemovePkg calls fails, then the
// later packages won't be removed.
//
// NOTE: This method should only be called on packages marked FwdStateCompleted.
func (c *ChannelStateDB) RemoveFwdPkgs(channel *OpenChannel,
	heights ...uint64) error {

	return cstate.RemoveFwdPkgs(c.backend, channel, heights...)
}

// revocationLogTailCommitHeight returns the commit height at the end of the
// revocation log.
func (c *ChannelStateDB) revocationLogTailCommitHeight(
	channel *OpenChannel) (uint64, error) {

	return cstate.RevocationLogTailCommitHeight(c.backend, channel)
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to date.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to date.
func (c *ChannelStateDB) CommitmentHeight(channel *OpenChannel) (
	uint64, error) {

	return cstate.CommitmentHeight(c.backend, channel)
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *ChannelStateDB) FindPreviousState(channel *OpenChannel,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	return cstate.FindPreviousState(c.backend, channel, updateNum)
}

// ClosureType is an enum like structure that details exactly how a channel was
// closed.
type ClosureType = cstate.ClosureType

const (
	// CooperativeClose indicates that a channel has been closed
	// cooperatively.
	CooperativeClose = cstate.CooperativeClose

	// LocalForceClose indicates that we have unilaterally broadcast our
	// current commitment state on-chain.
	LocalForceClose = cstate.LocalForceClose

	// RemoteForceClose indicates that the remote peer has unilaterally
	// broadcast their current commitment state on-chain.
	RemoteForceClose = cstate.RemoteForceClose

	// BreachClose indicates that the remote peer attempted to broadcast a
	// prior revoked channel state.
	BreachClose = cstate.BreachClose

	// FundingCanceled indicates that the channel never was fully opened
	// before it was marked as closed in the database.
	FundingCanceled = cstate.FundingCanceled

	// Abandoned indicates that the channel state was removed without any
	// further actions.
	Abandoned = cstate.Abandoned
)

// ChannelCloseSummary contains the final state of a channel at the point it
// was closed.
type ChannelCloseSummary = cstate.ChannelCloseSummary

// CloseChannel closes the supplied channel via the strategy selected at DB
// construction. On synchronous backends the channel's nested state — the
// revocation log, the per-channel forwarding-package bucket, and the
// chanBucket itself — is deleted inline. On tombstone-enabled backends none
// of the bulk state is touched; the outpointBucket flip to outpointClosed
// signals that the channel is logically closed.
func (c *ChannelStateDB) CloseChannel(channel *OpenChannel,
	summary *ChannelCloseSummary, statuses ...ChannelStatus) error {

	if c.tombstoneClosedChannels {
		return c.closeChannelTombstone(channel, summary, statuses...)
	}

	return c.closeChannelSync(channel, summary, statuses...)
}

// locateOpenChannel performs the open-channel-bucket descent for a
// CloseChannel transaction: it returns the chain bucket, the channel bucket,
// and the serialized chanKey for the supplied OpenChannel. A chanKey already
// flipped to outpointClosed surfaces ErrChannelNotFound so a redundant
// CloseChannel does not re-archive or re-flip the index.
func locateOpenChannel(tx kvdb.RwTx, channel *OpenChannel) (kvdb.RwBucket,
	kvdb.RwBucket, []byte, error) {

	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, nil, nil, ErrNoChanDBExists
	}

	nodePub := channel.IdentityPub.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	chainBucket := nodeChanBucket.NestedReadWriteBucket(
		channel.ChainHash[:],
	)
	if chainBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(
		&chanPointBuf, &channel.FundingOutpoint,
	); err != nil {
		return nil, nil, nil, err
	}
	chanKey := chanPointBuf.Bytes()

	chanBucket := chainBucket.NestedReadWriteBucket(chanKey)
	if chanBucket == nil {
		return nil, nil, nil, ErrNoActiveChannels
	}

	// A channel whose outpoint is already flipped to outpointClosed must
	// not be re-closed: on tombstone backends the chanBucket survives a
	// previous close, but the index flip is the authoritative record that
	// the channel is gone from the open-channel view.
	closed, err := isOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, nil, nil, err
	}
	if closed {
		return nil, nil, nil, ErrChannelNotFound
	}

	return chainBucket, chanBucket, chanKey, nil
}

// updateClosedOutpointIndex flips the outpoint index entry for chanKey from
// open to closed. The index entry must already exist; it was placed there
// when the channel was opened.
func updateClosedOutpointIndex(tx kvdb.RwTx, chanKey []byte) error {
	return cstate.UpdateClosedOutpointIndex(tx, chanKey)
}

// archiveClosedChannel writes the immutable close-time records of the
// channel: a copy of the open-channel state under historicalChannelBucket
// (with the supplied close statuses OR'd into chanStatus) and the close
// summary under closeSummaryBucket.
func archiveClosedChannel(tx kvdb.RwTx, chanKey []byte,
	chanState *OpenChannel, summary *ChannelCloseSummary,
	statuses ...ChannelStatus) error {

	historicalBucket, err := tx.CreateTopLevelBucket(
		historicalChannelBucket,
	)
	if err != nil {
		return err
	}
	historicalChanBucket, err := historicalBucket.CreateBucketIfNotExists(
		chanKey,
	)
	if err != nil {
		return err
	}

	for _, s := range statuses {
		chanState.SetChannelStatusForStore(
			chanState.ChannelStatusForStore() | s,
		)
	}

	if err := putOpenChannel(historicalChanBucket, chanState); err != nil {
		return err
	}

	return putChannelCloseSummary(tx, chanKey, summary, chanState)
}

// closeChannelSync performs the historical synchronous close path: in a
// single write transaction it wipes the forwarding-package state, deletes
// the channel bucket and its nested revocation log entries, updates the
// outpoint index, and archives the close summary. It is used by backends
// where nested-bucket deletion is cheap (bbolt, etcd).
func (c *ChannelStateDB) closeChannelSync(channel *OpenChannel,
	summary *ChannelCloseSummary, statuses ...ChannelStatus) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chainBucket, chanBucket, chanKey, err := locateOpenChannel(
			tx, channel,
		)
		if err != nil {
			return err
		}

		chanState, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		if err = newChannelPackager(chanState).Wipe(tx); err != nil {
			return err
		}

		if err := deleteOpenChannel(chanBucket); err != nil {
			return err
		}

		if channel.ChanType.IsFrozen() ||
			channel.ChanType.HasLeaseExpiration() {

			if err := deleteThawHeight(chanBucket); err != nil {
				return err
			}
		}

		if err := deleteLogBucket(chanBucket); err != nil {
			return err
		}

		if err := chainBucket.DeleteNestedBucket(chanKey); err != nil {
			return err
		}

		if err := updateClosedOutpointIndex(tx, chanKey); err != nil {
			return err
		}

		return archiveClosedChannel(
			tx, chanKey, chanState, summary, statuses...,
		)
	}, func() {})
}

// closeChannelTombstone performs the tombstone close path used by
// KV-over-SQL backends. The channel's per-channel state is left intact —
// touching it would trigger the cascading nested-bucket delete this path
// exists to avoid — and the outpointBucket flip from outpointOpen to
// outpointClosed serves as the authoritative closed-channel marker. The
// disk space is reclaimed wholesale by the upcoming native-SQL
// channel-state migration.
func (c *ChannelStateDB) closeChannelTombstone(channel *OpenChannel,
	summary *ChannelCloseSummary, statuses ...ChannelStatus) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		_, chanBucket, chanKey, err := locateOpenChannel(tx, channel)
		if err != nil {
			return err
		}

		chanState, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		if err := updateClosedOutpointIndex(tx, chanKey); err != nil {
			return err
		}

		return archiveClosedChannel(
			tx, chanKey, chanState, summary, statuses...,
		)
	}, func() {})
}

// ChannelSnapshot is a frozen snapshot of the current channel state.
type ChannelSnapshot = cstate.ChannelSnapshot

// LatestCommitments returns the two latest commitments for both the local and
// remote party. These commitments are read from disk to ensure that only the
// latest fully committed state is returned. The first commitment returned is
// the local commitment, and the second returned is the remote commitment.
func (c *ChannelStateDB) LatestCommitments(channel *OpenChannel) (
	*ChannelCommitment, *ChannelCommitment, error) {

	return cstate.LatestCommitments(c.backend, channel)
}

// RemoteRevocationStore returns the most up to date commitment version of the
// revocation storage tree for the remote party. This method can be used when
// acting on a possible contract breach to ensure, that the caller has the most
// up to date information required to deliver justice.
func (c *ChannelStateDB) RemoteRevocationStore(channel *OpenChannel) (
	shachain.Store, error) {

	return cstate.RemoteRevocationStore(c.backend, channel)
}

func putChannelCloseSummary(tx kvdb.RwTx, chanID []byte,
	summary *ChannelCloseSummary, lastChanState *OpenChannel) error {

	return cstate.PutChannelCloseSummary(
		tx, chanID, summary, lastChanState,
	)
}

func serializeChannelCloseSummary(w io.Writer, cs *ChannelCloseSummary) error {
	return cstate.SerializeChannelCloseSummary(w, cs)
}

func deserializeCloseChannelSummary(r io.Reader) (*ChannelCloseSummary, error) {
	return cstate.DeserializeCloseChannelSummary(r)
}

func serializeChanCommit(w io.Writer, c *ChannelCommitment) error {
	return cstate.SerializeChanCommit(w, c)
}

func putChanCommitment(chanBucket kvdb.RwBucket, c *ChannelCommitment,
	local bool) error {

	return cstate.PutChanCommitment(chanBucket, c, local)
}

func putChanCommitments(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	return cstate.PutChanCommitments(chanBucket, channel)
}

func putChanRevocationState(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	return cstate.PutChanRevocationState(chanBucket, channel)
}

func readChanConfig(b io.Reader, c *ChannelConfig) error {
	return cstate.ReadChanConfig(b, c)
}

func fetchChanInfo(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	return cstate.FetchChanInfo(chanBucket, channel)
}

func deserializeChanCommit(r io.Reader) (ChannelCommitment, error) {
	return cstate.DeserializeChanCommit(r)
}

func fetchChanCommitment(chanBucket kvdb.RBucket,
	local bool) (ChannelCommitment, error) {

	return cstate.FetchChanCommitment(chanBucket, local)
}

func fetchChanCommitments(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	return cstate.FetchChanCommitments(chanBucket, channel)
}

func fetchChanRevocationState(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	return cstate.FetchChanRevocationState(chanBucket, channel)
}

func deleteOpenChannel(chanBucket kvdb.RwBucket) error {
	return cstate.DeleteOpenChannel(chanBucket)
}

// makeLogKey converts a uint64 into an 8 byte array.
func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}

func fetchThawHeight(chanBucket kvdb.RBucket) (uint32, error) {
	return cstate.FetchThawHeight(chanBucket)
}

func storeThawHeight(chanBucket kvdb.RwBucket, height uint32) error {
	return cstate.StoreThawHeight(chanBucket, height)
}

func deleteThawHeight(chanBucket kvdb.RwBucket) error {
	return cstate.DeleteThawHeight(chanBucket)
}

// EKeyLocator is an encoder for keychain.KeyLocator.
func EKeyLocator(w io.Writer, val interface{}, buf *[8]byte) error {
	return cstate.EKeyLocator(w, val, buf)
}

// DKeyLocator is a decoder for keychain.KeyLocator.
func DKeyLocator(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	return cstate.DKeyLocator(r, val, buf, l)
}

// ShutdownInfo contains various info about the shutdown initiation of a
// channel.
type ShutdownInfo = cstate.ShutdownInfo

// NewShutdownInfo constructs a new ShutdownInfo object.
func NewShutdownInfo(deliveryScript lnwire.DeliveryAddress,
	locallyInitiated bool) *ShutdownInfo {

	return cstate.NewShutdownInfo(deliveryScript, locallyInitiated)
}
