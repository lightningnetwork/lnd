package channeldb

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/walletdb"
	cstate "github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// AbsoluteThawHeightThreshold is the threshold at which a thaw height
	// begins to be interpreted as an absolute block height, rather than a
	// relative one.
	AbsoluteThawHeightThreshold uint32 = 500000

	// HTLCBlindingPointTLV is the tlv type used for storing blinding
	// points with HTLCs.
	HTLCBlindingPointTLV tlv.Type = 0
)

var (
	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("closed-chan-bucket")

	// openChannelBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	//
	// openChan -> nodeID -> chanPoint
	//
	// TODO(roasbeef): flesh out comment
	openChannelBucket = []byte("open-chan-bucket")

	// outpointBucket stores all of our channel outpoints and a tlv
	// stream containing channel data.
	//
	// outpoint -> tlv stream.
	//
	outpointBucket = []byte("outpoint-bucket")

	// chanIDBucket stores all of the 32-byte channel ID's we know about.
	// These could be derived from outpointBucket, but it is more
	// convenient to have these in their own bucket.
	//
	// chanID -> tlv stream.
	//
	chanIDBucket = []byte("chan-id-bucket")

	// historicalChannelBucket stores all channels that have seen their
	// commitment tx confirm. All information from their previous open state
	// is retained.
	historicalChannelBucket = []byte("historical-chan-bucket")

	// chanInfoKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores all the static
	// information for a channel which is decided at the end of  the
	// funding flow.
	chanInfoKey = []byte("chan-info-key")

	// localUpfrontShutdownKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores an optional upfront
	// shutdown script for the local peer.
	localUpfrontShutdownKey = []byte("local-upfront-shutdown-key")

	// remoteUpfrontShutdownKey can be accessed within the bucket for a channel
	// (identified by its chanPoint). This key stores an optional upfront
	// shutdown script for the remote peer.
	remoteUpfrontShutdownKey = []byte("remote-upfront-shutdown-key")

	// chanCommitmentKey can be accessed within the sub-bucket for a
	// particular channel. This key stores the up to date commitment state
	// for a particular channel party. Appending a 0 to the end of this key
	// indicates it's the commitment for the local party, and appending a 1
	// to the end of this key indicates it's the commitment for the remote
	// party.
	chanCommitmentKey = []byte("chan-commitment-key")

	// unsignedAckedUpdatesKey is an entry in the channel bucket that
	// contains the remote updates that we have acked, but not yet signed
	// for in one of our remote commits.
	unsignedAckedUpdatesKey = []byte("unsigned-acked-updates-key")

	// remoteUnsignedLocalUpdatesKey is an entry in the channel bucket that
	// contains the local updates that the remote party has acked, but
	// has not yet signed for in one of their local commits.
	remoteUnsignedLocalUpdatesKey = []byte("remote-unsigned-local-updates-key")

	// revocationStateKey stores their current revocation hash, our
	// preimage producer and their preimage store.
	revocationStateKey = []byte("revocation-state-key")

	// dataLossCommitPointKey stores the commitment point received from the
	// remote peer during a channel sync in case we have lost channel state.
	dataLossCommitPointKey = []byte("data-loss-commit-point-key")

	// forceCloseTxKey points to a the unilateral closing tx that we
	// broadcasted when moving the channel to state CommitBroadcasted.
	forceCloseTxKey = []byte("closing-tx-key")

	// coopCloseTxKey points to a the cooperative closing tx that we
	// broadcasted when moving the channel to state CoopBroadcasted.
	coopCloseTxKey = []byte("coop-closing-tx-key")

	// shutdownInfoKey points to the serialised shutdown info that has been
	// persisted for a channel. The existence of this info means that we
	// have sent the Shutdown message before and so should re-initiate the
	// shutdown on re-establish.
	shutdownInfoKey = []byte("shutdown-info-key")

	// commitDiffKey stores the current pending commitment state we've
	// extended to the remote party (if any). Each time we propose a new
	// state, we store the information necessary to reconstruct this state
	// from the prior commitment. This allows us to resync the remote party
	// to their expected state in the case of message loss.
	//
	// TODO(roasbeef): rename to commit chain?
	commitDiffKey = []byte("commit-diff-key")

	// frozenChanKey is the key where we store the information for any
	// active "frozen" channels. This key is present only in the leaf
	// bucket for a given channel.
	frozenChanKey = []byte("frozen-chans")

	// lastWasRevokeKey is a key that stores true when the last update we
	// sent was a revocation and false when it was a commitment signature.
	// This is nil in the case of new channels with no updates exchanged.
	lastWasRevokeKey = []byte("last-was-revoke")

	// finalHtlcsBucket contains the htlcs that have been resolved
	// definitively. Within this bucket, there is a sub-bucket for each
	// channel. In each channel bucket, the htlc indices are stored along
	// with final outcome.
	//
	// final-htlcs -> chanID -> htlcIndex -> outcome
	//
	// 'outcome' is a byte value that encodes:
	//
	//       | true      false
	// ------+------------------
	// bit 0 | settled   failed
	// bit 1 | offchain  onchain
	//
	// This bucket is positioned at the root level, because its contents
	// will be kept independent of the channel lifecycle. This is to avoid
	// the situation where a channel force-closes autonomously and the user
	// not being able to query for htlc outcomes anymore.
	finalHtlcsBucket = []byte("final-htlcs")
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

const (
	// A tlv type definition used to serialize an outpoint's indexStatus
	// for use in the outpoint index.
	indexStatusType tlv.Type = 0
)

type (
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

// openChannelTlvData houses the new data fields that are stored for each
// channel in a TLV stream within the root bucket. This is stored as a TLV
// stream appended to the existing hard-coded fields in the channel's root
// bucket. New fields being added to the channel state should be added here.
//
// NOTE: This struct is used for serialization purposes only and its fields
// should be accessed via the OpenChannel struct while in memory.
type openChannelTlvData struct {
	// revokeKeyLoc is the key locator for the revocation key.
	revokeKeyLoc tlv.RecordT[tlv.TlvType1, keyLocRecord]

	// initialLocalBalance is the initial local balance of the channel.
	initialLocalBalance tlv.RecordT[tlv.TlvType2, uint64]

	// initialRemoteBalance is the initial remote balance of the channel.
	initialRemoteBalance tlv.RecordT[tlv.TlvType3, uint64]

	// realScid is the real short channel ID of the channel corresponding to
	// the on-chain outpoint.
	realScid tlv.RecordT[tlv.TlvType4, lnwire.ShortChannelID]

	// memo is an optional text field that gives context to the user about
	// the channel.
	memo tlv.OptionalRecordT[tlv.TlvType5, []byte]

	// tapscriptRoot is the optional Tapscript root the channel funding
	// output commits to.
	tapscriptRoot tlv.OptionalRecordT[tlv.TlvType6, [32]byte]

	// customBlob is an optional TLV encoded blob of data representing
	// custom channel funding information.
	customBlob tlv.OptionalRecordT[tlv.TlvType7, tlv.Blob]

	// confirmationHeight records the block height at which the funding
	// transaction was first confirmed.
	confirmationHeight tlv.RecordT[tlv.TlvType8, uint32]

	// closeConfirmationHeight records the block height at which the closing
	// transaction was first confirmed. This is used to calculate the
	// remaining confirmations until the channel is considered fully closed.
	// Note: if not set, it means either the channel has not been
	// closed yet, or it was closed before this field was introduced.
	closeConfirmationHeight tlv.OptionalRecordT[tlv.TlvType9, uint32]
}

// encode serializes the openChannelTlvData to the given io.Writer.
func (c *openChannelTlvData) encode(w io.Writer) error {
	tlvRecords := []tlv.Record{
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		c.confirmationHeight.Record(),
	}
	c.memo.WhenSome(func(memo tlv.RecordT[tlv.TlvType5, []byte]) {
		tlvRecords = append(tlvRecords, memo.Record())
	})
	c.tapscriptRoot.WhenSome(
		func(root tlv.RecordT[tlv.TlvType6, [32]byte]) {
			tlvRecords = append(tlvRecords, root.Record())
		},
	)
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType7, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})
	c.closeConfirmationHeight.WhenSome(
		func(h tlv.RecordT[tlv.TlvType9, uint32]) {
			tlvRecords = append(tlvRecords, h.Record())
		},
	)

	tlv.SortRecords(tlvRecords)

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode deserializes the openChannelTlvData from the given io.Reader.
func (c *openChannelTlvData) decode(r io.Reader) error {
	memo := c.memo.Zero()
	tapscriptRoot := c.tapscriptRoot.Zero()
	blob := c.customBlob.Zero()
	closeConfHeight := c.closeConfirmationHeight.Zero()

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		memo.Record(),
		tapscriptRoot.Record(),
		blob.Record(),
		c.confirmationHeight.Record(),
		closeConfHeight.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[memo.TlvType()]; ok {
		c.memo = tlv.SomeRecordT(memo)
	}
	if _, ok := tlvs[tapscriptRoot.TlvType()]; ok {
		c.tapscriptRoot = tlv.SomeRecordT(tapscriptRoot)
	}
	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}
	if _, ok := tlvs[closeConfHeight.TlvType()]; ok {
		c.closeConfirmationHeight = tlv.SomeRecordT(closeConfHeight)
	}

	return nil
}

// indexStatus is an enum-like type that describes what state the
// outpoint is in. Currently only two possible values.
type indexStatus uint8

const (
	// outpointOpen represents an outpoint that is open in the outpoint index.
	outpointOpen indexStatus = 0

	// outpointClosed represents an outpoint that is closed in the outpoint
	// index.
	outpointClosed indexStatus = 1
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
	if opBucket == nil {
		return false, nil
	}
	raw := opBucket.Get(chanKey)
	if raw == nil {
		return false, nil
	}

	var status uint8
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &status)
	stream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return false, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return false, fmt.Errorf("decode outpoint status for "+
			"chan_key=%x: %w", chanKey, err)
	}

	return indexStatus(status) == outpointClosed, nil
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

// commitTlvData stores all the optional data that may be stored as a TLV stream
// at the _end_ of the normal serialized commit on disk.
type commitTlvData struct {
	// customBlob is a custom blob that may store extra data for custom
	// channels.
	customBlob tlv.OptionalRecordT[tlv.TlvType1, tlv.Blob]
}

// encode encodes the aux data into the passed io.Writer.
func (c *commitTlvData) encode(w io.Writer) error {
	var tlvRecords []tlv.Record
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType1, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode attempts to decode the aux data from the passed io.Reader.
func (c *commitTlvData) decode(r io.Reader) error {
	blob := c.customBlob.Zero()

	tlvStream, err := tlv.NewStream(
		blob.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}

	return nil
}

// amendCommitTlvData updates the commitment with the given auxiliary TLV data.
func amendCommitTlvData(c *ChannelCommitment, auxData commitTlvData) {
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		c.CustomBlob = fn.Some(blob)
	})
}

// extractCommitTlvData creates a new commitTlvData from the given commitment.
func extractCommitTlvData(c *ChannelCommitment) commitTlvData {
	var auxData commitTlvData

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](blob),
		)
	})

	return auxData
}

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

// FinalHtlcByte defines a byte type that encodes information about the final
// htlc resolution.
type FinalHtlcByte byte

const (
	// FinalHtlcSettledBit is the bit that encodes whether the htlc was
	// settled or failed.
	FinalHtlcSettledBit FinalHtlcByte = 1 << 0

	// FinalHtlcOffchainBit is the bit that encodes whether the htlc was
	// resolved offchain or onchain.
	FinalHtlcOffchainBit FinalHtlcByte = 1 << 1
)

// OpenChannel encapsulates the persistent and dynamic state of an open channel
// with a remote node. An open channel supports several options for on-disk
// serialization depending on the exact context. Full (upon channel creation)
// state commitments, and partial (due to a commitment update) writes are
// supported. Each partial write due to a state update appends the new update
// to an on-disk log, which can then subsequently be queried in order to
// "time-travel" to a prior state.
type OpenChannel struct {
	// ChanType denotes which type of channel this is.
	ChanType ChannelType

	// ChainHash is a hash which represents the blockchain that this
	// channel will be opened within. This value is typically the genesis
	// hash. In the case that the original chain went through a contentious
	// hard-fork, then this value will be tweaked using the unique fork
	// point on each branch.
	ChainHash chainhash.Hash

	// FundingOutpoint is the outpoint of the final funding transaction.
	// This value uniquely and globally identifies the channel within the
	// target blockchain as specified by the chain hash parameter.
	FundingOutpoint wire.OutPoint

	// ShortChannelID encodes the exact location in the chain in which the
	// channel was initially confirmed. This includes: the block height,
	// transaction index, and the output within the target transaction.
	//
	// If IsZeroConf(), then this will the "base" (very first) ALIAS scid
	// and the confirmed SCID will be stored in ConfirmedScid.
	ShortChannelID lnwire.ShortChannelID

	// IsPending indicates whether a channel's funding transaction has been
	// confirmed.
	IsPending bool

	// IsInitiator is a bool which indicates if we were the original
	// initiator for the channel. This value may affect how higher levels
	// negotiate fees, or close the channel.
	IsInitiator bool

	// chanStatus is the current status of this channel. If it is not in
	// the state Default, it should not be used for forwarding payments.
	chanStatus ChannelStatus

	// FundingBroadcastHeight is the height in which the funding
	// transaction was broadcast. This value can be used by higher level
	// sub-systems to determine if a channel is stale and/or should have
	// been confirmed before a certain height.
	FundingBroadcastHeight uint32

	// ConfirmationHeight records the block height at which the funding
	// transaction was first confirmed.
	ConfirmationHeight uint32

	// CloseConfirmationHeight records the block height at which the closing
	// transaction was first confirmed. This is used to track remaining
	// confirmations until the channel is considered fully closed. It is
	// None if the closing transaction has not yet been confirmed, or if
	// this data was not available (e.g. channels closed before this
	// field was introduced).
	CloseConfirmationHeight fn.Option[uint32]

	// NumConfsRequired is the number of confirmations a channel's funding
	// transaction must have received in order to be considered available
	// for normal transactional use.
	NumConfsRequired uint16

	// ChannelFlags holds the flags that were sent as part of the
	// open_channel message.
	ChannelFlags lnwire.FundingFlag

	// IdentityPub is the identity public key of the remote node this
	// channel has been established with.
	IdentityPub *btcec.PublicKey

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// TotalMSatSent is the total number of milli-satoshis we've sent
	// within this channel.
	TotalMSatSent lnwire.MilliSatoshi

	// TotalMSatReceived is the total number of milli-satoshis we've
	// received within this channel.
	TotalMSatReceived lnwire.MilliSatoshi

	// InitialLocalBalance is the balance we have during the channel
	// opening. When we are not the initiator, this value represents the
	// push amount.
	InitialLocalBalance lnwire.MilliSatoshi

	// InitialRemoteBalance is the balance they have during the channel
	// opening.
	InitialRemoteBalance lnwire.MilliSatoshi

	// LocalChanCfg is the channel configuration for the local node.
	LocalChanCfg ChannelConfig

	// RemoteChanCfg is the channel configuration for the remote node.
	RemoteChanCfg ChannelConfig

	// LocalCommitment is the current local commitment state for the local
	// party. This is stored distinct from the state of the remote party
	// as there are certain asymmetric parameters which affect the
	// structure of each commitment.
	LocalCommitment ChannelCommitment

	// RemoteCommitment is the current remote commitment state for the
	// remote party. This is stored distinct from the state of the local
	// party as there are certain asymmetric parameters which affect the
	// structure of each commitment.
	RemoteCommitment ChannelCommitment

	// RemoteCurrentRevocation is the current revocation for their
	// commitment transaction. However, since this the derived public key,
	// we don't yet have the private key so we aren't yet able to verify
	// that it's actually in the hash chain.
	RemoteCurrentRevocation *btcec.PublicKey

	// RemoteNextRevocation is the revocation key to be used for the *next*
	// commitment transaction we create for the local node. Within the
	// specification, this value is referred to as the
	// per-commitment-point.
	RemoteNextRevocation *btcec.PublicKey

	// RevocationProducer is used to generate the revocation in such a way
	// that remote side might store it efficiently and have the ability to
	// restore the revocation by index if needed. Current implementation of
	// secret producer is shachain producer.
	RevocationProducer shachain.Producer

	// RevocationStore is used to efficiently store the revocations for
	// previous channels states sent to us by remote side. Current
	// implementation of secret store is shachain store.
	RevocationStore shachain.Store

	// Packager is used to create and update forwarding packages for this
	// channel, which encodes all necessary information to recover from
	// failures and reforward HTLCs that were not fully processed.
	Packager FwdPackager

	// FundingTxn is the transaction containing this channel's funding
	// outpoint. Upon restarts, this txn will be rebroadcast if the channel
	// is found to be pending.
	//
	// NOTE: This value will only be populated for single-funder channels
	// for which we are the initiator, and that we also have the funding
	// transaction for. One can check this by using the HasFundingTx()
	// method on the ChanType field.
	FundingTxn *wire.MsgTx

	// LocalShutdownScript is set to a pre-set script if the channel was opened
	// by the local node with option_upfront_shutdown_script set. If the option
	// was not set, the field is empty.
	LocalShutdownScript lnwire.DeliveryAddress

	// RemoteShutdownScript is set to a pre-set script if the channel was opened
	// by the remote node with option_upfront_shutdown_script set. If the option
	// was not set, the field is empty.
	RemoteShutdownScript lnwire.DeliveryAddress

	// ThawHeight is the height when a frozen channel once again becomes a
	// normal channel. If this is zero, then there're no restrictions on
	// this channel. If the value is lower than 500,000, then it's
	// interpreted as a relative height, or an absolute height otherwise.
	ThawHeight uint32

	// LastWasRevoke is a boolean that determines if the last update we sent
	// was a revocation (true) or a commitment signature (false).
	LastWasRevoke bool

	// RevocationKeyLocator stores the KeyLocator information that we will
	// need to derive the shachain root for this channel. This allows us to
	// have private key isolation from lnd.
	RevocationKeyLocator keychain.KeyLocator

	// confirmedScid is the confirmed ShortChannelID for a zero-conf
	// channel. If the channel is unconfirmed, then this will be the
	// default ShortChannelID. This is only set for zero-conf channels.
	confirmedScid lnwire.ShortChannelID

	// Memo is any arbitrary information we wish to store locally about the
	// channel that will be useful to our future selves.
	Memo []byte

	// TapscriptRoot is an optional tapscript root used to derive the MuSig2
	// funding output.
	TapscriptRoot fn.Option[chainhash.Hash]

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This information is only created
	// at channel funding time, and after wards is to be considered
	// immutable.
	CustomBlob fn.Option[tlv.Blob]

	// Db persists channel state through the chanstate Store contract. This
	// field intentionally keeps the existing name while the code moves from
	// channeldb toward chanstate so call sites can become backend
	// independent before the OpenChannel type itself is moved.
	Db cstate.Store[*OpenChannel]

	// TODO(roasbeef): just need to store local and remote HTLC's?

	sync.RWMutex
}

// String returns a string representation of the channel.
func (c *OpenChannel) String() string {
	indexStr := "height=%v, local_htlc_index=%v, local_log_index=%v, " +
		"remote_htlc_index=%v, remote_log_index=%v"

	commit := c.LocalCommitment
	local := fmt.Sprintf(indexStr, commit.CommitHeight,
		commit.LocalHtlcIndex, commit.LocalLogIndex,
		commit.RemoteHtlcIndex, commit.RemoteLogIndex,
	)

	commit = c.RemoteCommitment
	remote := fmt.Sprintf(indexStr, commit.CommitHeight,
		commit.LocalHtlcIndex, commit.LocalLogIndex,
		commit.RemoteHtlcIndex, commit.RemoteLogIndex,
	)

	return fmt.Sprintf("SCID=%v, status=%v, initiator=%v, pending=%v, "+
		"local commitment has %s, remote commitment has %s",
		c.ShortChannelID, c.chanStatus, c.IsInitiator, c.IsPending,
		local, remote,
	)
}

// Initiator returns the ChannelParty that originally opened this channel.
func (c *OpenChannel) Initiator() lntypes.ChannelParty {
	c.RLock()
	defer c.RUnlock()

	if c.IsInitiator {
		return lntypes.Local
	}

	return lntypes.Remote
}

// ShortChanID returns the current ShortChannelID of this channel.
func (c *OpenChannel) ShortChanID() lnwire.ShortChannelID {
	c.RLock()
	defer c.RUnlock()

	return c.ShortChannelID
}

// ZeroConfRealScid returns the zero-conf channel's confirmed scid. This should
// only be called if IsZeroConf returns true.
func (c *OpenChannel) ZeroConfRealScid() lnwire.ShortChannelID {
	c.RLock()
	defer c.RUnlock()

	return c.confirmedScid
}

// ZeroConfConfirmed returns whether the zero-conf channel has confirmed. This
// should only be called if IsZeroConf returns true.
func (c *OpenChannel) ZeroConfConfirmed() bool {
	c.RLock()
	defer c.RUnlock()

	return c.confirmedScid != hop.Source
}

// IsZeroConf returns whether the option_zeroconf channel type was negotiated.
func (c *OpenChannel) IsZeroConf() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasZeroConf()
}

// IsOptionScidAlias returns whether the option_scid_alias channel type was
// negotiated.
func (c *OpenChannel) IsOptionScidAlias() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasScidAliasChan()
}

// NegotiatedAliasFeature returns whether the option-scid-alias feature bit was
// negotiated.
func (c *OpenChannel) NegotiatedAliasFeature() bool {
	c.RLock()
	defer c.RUnlock()

	return c.ChanType.HasScidAliasFeature()
}

// ChanStatus returns the current ChannelStatus of this channel.
func (c *OpenChannel) ChanStatus() ChannelStatus {
	c.RLock()
	defer c.RUnlock()

	return c.chanStatus
}

// ChannelStatusForStore returns the in-memory channel status without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code that
// still lives in channeldb while OpenChannel moves toward chanstate. Callers
// are responsible for synchronization. Normal callers should use ChanStatus.
func (c *OpenChannel) ChannelStatusForStore() ChannelStatus {
	return c.chanStatus
}

// SetChannelStatusForStore updates the in-memory channel status without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code that
// still lives in channeldb while OpenChannel moves toward chanstate. Callers
// are responsible for synchronization. Normal callers should use
// ApplyChanStatus or ClearChanStatus when the status change must be persisted.
func (c *OpenChannel) SetChannelStatusForStore(status ChannelStatus) {
	c.chanStatus = status
}

// ApplyChanStatus allows the caller to modify the internal channel state in a
// thead-safe manner.
func (c *OpenChannel) ApplyChanStatus(status ChannelStatus) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.ApplyChannelStatus(c, status)
}

// ClearChanStatus allows the caller to clear a particular channel status from
// the primary channel status bit field. After this method returns, a call to
// HasChanStatus(status) should return false.
func (c *OpenChannel) ClearChanStatus(status ChannelStatus) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.ClearChannelStatus(c, status)
}

// HasChanStatus returns true if the internal bitfield channel status of the
// target channel has the specified status bit set.
func (c *OpenChannel) HasChanStatus(status ChannelStatus) bool {
	c.RLock()
	defer c.RUnlock()

	return c.hasChanStatus(status)
}

func (c *OpenChannel) hasChanStatus(status ChannelStatus) bool {
	// Special case ChanStatusDefualt since it isn't actually flag, but a
	// particular combination (or lack-there-of) of flags.
	if status == ChanStatusDefault {
		return c.chanStatus == ChanStatusDefault
	}

	return c.chanStatus&status == status
}

// ConfirmedScidForStore returns the in-memory confirmed SCID without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code that
// still lives in channeldb while OpenChannel moves toward chanstate. Callers
// are responsible for synchronization. Normal callers should use
// ZeroConfRealScid.
func (c *OpenChannel) ConfirmedScidForStore() lnwire.ShortChannelID {
	return c.confirmedScid
}

// SetConfirmedScidForStore updates the in-memory confirmed SCID without taking
// the channel mutex.
//
// NOTE: This is a preliminary migration hook for KV-backed store code that
// still lives in channeldb while OpenChannel moves toward chanstate. Callers
// are responsible for synchronization.
func (c *OpenChannel) SetConfirmedScidForStore(scid lnwire.ShortChannelID) {
	c.confirmedScid = scid
}

// BroadcastHeight returns the height at which the funding tx was broadcast.
func (c *OpenChannel) BroadcastHeight() uint32 {
	c.RLock()
	defer c.RUnlock()

	return c.FundingBroadcastHeight
}

// SetBroadcastHeight sets the FundingBroadcastHeight.
func (c *OpenChannel) SetBroadcastHeight(height uint32) {
	c.Lock()
	defer c.Unlock()

	c.FundingBroadcastHeight = height
}

// amendOpenChannelTlvData updates the channel with the given auxiliary TLV
// data.
func amendOpenChannelTlvData(channel *OpenChannel, auxData openChannelTlvData) {
	channel.RevocationKeyLocator = auxData.revokeKeyLoc.Val.KeyLocator
	channel.InitialLocalBalance = lnwire.MilliSatoshi(
		auxData.initialLocalBalance.Val,
	)
	channel.InitialRemoteBalance = lnwire.MilliSatoshi(
		auxData.initialRemoteBalance.Val,
	)
	channel.SetConfirmedScidForStore(auxData.realScid.Val)
	channel.ConfirmationHeight = auxData.confirmationHeight.Val

	auxData.memo.WhenSomeV(func(memo []byte) {
		channel.Memo = memo
	})
	auxData.tapscriptRoot.WhenSomeV(func(h [32]byte) {
		channel.TapscriptRoot = fn.Some[chainhash.Hash](h)
	})
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		channel.CustomBlob = fn.Some(blob)
	})
	auxData.closeConfirmationHeight.WhenSomeV(func(h uint32) {
		channel.CloseConfirmationHeight = fn.Some(h)
	})
}

// extractOpenChannelTlvData creates a new openChannelTlvData from the given
// channel.
func extractOpenChannelTlvData(channel *OpenChannel) openChannelTlvData {
	auxData := openChannelTlvData{
		revokeKeyLoc: tlv.NewRecordT[tlv.TlvType1](
			keyLocRecord{channel.RevocationKeyLocator},
		),
		initialLocalBalance: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint64(channel.InitialLocalBalance),
		),
		initialRemoteBalance: tlv.NewPrimitiveRecord[tlv.TlvType3](
			uint64(channel.InitialRemoteBalance),
		),
		realScid: tlv.NewRecordT[tlv.TlvType4](
			channel.ConfirmedScidForStore(),
		),
		confirmationHeight: tlv.NewPrimitiveRecord[tlv.TlvType8](
			channel.ConfirmationHeight,
		),
	}

	if len(channel.Memo) != 0 {
		auxData.memo = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5](channel.Memo),
		)
	}
	channel.TapscriptRoot.WhenSome(func(h chainhash.Hash) {
		auxData.tapscriptRoot = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6, [32]byte](h),
		)
	})
	channel.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType7](blob),
		)
	})
	channel.CloseConfirmationHeight.WhenSome(func(h uint32) {
		auxData.closeConfirmationHeight = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType9](h),
		)
	})

	return auxData
}

// Refresh updates the in-memory channel state using the latest state observed
// on disk.
func (c *OpenChannel) Refresh() error {
	c.Lock()
	defer c.Unlock()

	return c.Db.RefreshChannel(c)
}

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
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (kvdb.RBucket, error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := isOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

// fetchChanBucketRw is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on. This differs from
// fetchChanBucket in that it returns a writeable bucket.
func fetchChanBucketRw(tx kvdb.RwTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (kvdb.RwBucket,
	error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadWriteBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := isOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadWriteBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

func fetchFinalHtlcsBucketRw(tx kvdb.RwTx,
	chanID lnwire.ShortChannelID) (kvdb.RwBucket, error) {

	finalHtlcsBucket, err := tx.CreateTopLevelBucket(finalHtlcsBucket)
	if err != nil {
		return nil, err
	}

	var chanIDBytes [8]byte
	byteOrder.PutUint64(chanIDBytes[:], chanID.ToUint64())
	chanBucket, err := finalHtlcsBucket.CreateBucketIfNotExists(
		chanIDBytes[:],
	)
	if err != nil {
		return nil, err
	}

	return chanBucket, nil
}

// fullSyncOpenChannel syncs the contents of an OpenChannel while re-using an
// existing database transaction.
func fullSyncOpenChannel(tx kvdb.RwTx, c *OpenChannel) error {
	// Fetch the outpoint bucket and check if the outpoint already exists.
	opBucket := tx.ReadWriteBucket(outpointBucket)
	if opBucket == nil {
		return ErrNoChanDBExists
	}
	cidBucket := tx.ReadWriteBucket(chanIDBucket)
	if cidBucket == nil {
		return ErrNoChanDBExists
	}

	var chanPointBuf bytes.Buffer
	err := graphdb.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return err
	}

	// Now, check if the outpoint exists in our index.
	if opBucket.Get(chanPointBuf.Bytes()) != nil {
		return ErrChanAlreadyExists
	}

	cid := lnwire.NewChanIDFromOutPoint(c.FundingOutpoint)
	if cidBucket.Get(cid[:]) != nil {
		return ErrChanAlreadyExists
	}

	status := uint8(outpointOpen)

	// Write the status of this outpoint as the first entry in a tlv
	// stream.
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &status)
	opStream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := opStream.Encode(&b); err != nil {
		return err
	}

	// Add the outpoint to our outpoint index with the tlv stream.
	if err := opBucket.Put(chanPointBuf.Bytes(), b.Bytes()); err != nil {
		return err
	}

	if err := cidBucket.Put(cid[:], []byte{}); err != nil {
		return err
	}

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket, err := tx.CreateTopLevelBucket(openChannelBucket)
	if err != nil {
		return err
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := c.IdentityPub.SerializeCompressed()
	nodeChanBucket, err := openChanBucket.CreateBucketIfNotExists(nodePub)
	if err != nil {
		return err
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket, err := nodeChanBucket.CreateBucketIfNotExists(c.ChainHash[:])
	if err != nil {
		return err
	}

	// With the bucket for the node fetched, we can now go down another
	// level, creating the bucket for this channel itself.
	chanBucket, err := chainBucket.CreateBucket(
		chanPointBuf.Bytes(),
	)
	switch {
	case err == kvdb.ErrBucketExists:
		// If this channel already exists, then in order to avoid
		// overriding it, we'll return an error back up to the caller.
		return ErrChanAlreadyExists
	case err != nil:
		return err
	}

	return putOpenChannel(chanBucket, c)
}

// MarkConfirmationHeight updates the channel's confirmation height once the
// channel opening transaction receives one confirmation.
func (c *OpenChannel) MarkConfirmationHeight(height uint32) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelConfirmationHeight(c, height); err != nil {
		return err
	}

	c.ConfirmationHeight = height

	return nil
}

// MarkChannelConfirmationHeight updates the channel's confirmation height once
// the channel opening transaction receives one confirmation.
func (c *ChannelStateDB) MarkChannelConfirmationHeight(channel *OpenChannel,
	height uint32) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		diskChannel.ConfirmationHeight = height

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {})
}

// ResetCloseConfirmationHeight clears the channel's close confirmation height
// when the spending transaction is reorged out.
func (c *OpenChannel) ResetCloseConfirmationHeight() error {
	return c.MarkCloseConfirmationHeight(fn.None[uint32]())
}

// MarkCloseConfirmationHeight updates the channel's close confirmation height
// when the closing transaction is first detected in a block (spend height).
func (c *OpenChannel) MarkCloseConfirmationHeight(
	height fn.Option[uint32]) error {

	c.Lock()
	defer c.Unlock()

	err := c.Db.MarkChannelCloseConfirmationHeight(c, height)
	if err != nil {
		return err
	}

	c.CloseConfirmationHeight = height

	return nil
}

// MarkChannelCloseConfirmationHeight updates the channel's close confirmation
// height when the closing transaction is first detected in a block.
func (c *ChannelStateDB) MarkChannelCloseConfirmationHeight(
	channel *OpenChannel, height fn.Option[uint32]) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		diskChannel.CloseConfirmationHeight = height

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {})
}

// MarkAsOpen marks a channel as fully open given a locator that uniquely
// describes its location within the chain.
func (c *OpenChannel) MarkAsOpen(openLoc lnwire.ShortChannelID) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelOpen(c, openLoc); err != nil {
		return err
	}

	c.IsPending = false
	c.ShortChannelID = openLoc
	c.Packager = NewChannelPackager(openLoc)

	return nil
}

// MarkChannelOpen marks a channel as fully open given a locator that uniquely
// describes its location within the chain.
func (c *ChannelStateDB) MarkChannelOpen(channel *OpenChannel,
	openLoc lnwire.ShortChannelID) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		diskChannel.IsPending = false
		diskChannel.ShortChannelID = openLoc

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {})
}

// MarkRealScid marks the zero-conf channel's confirmed ShortChannelID. This
// should only be done if IsZeroConf returns true.
func (c *OpenChannel) MarkRealScid(realScid lnwire.ShortChannelID) error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelRealScid(c, realScid); err != nil {
		return err
	}

	c.confirmedScid = realScid

	return nil
}

// MarkChannelRealScid marks the zero-conf channel's confirmed ShortChannelID.
func (c *ChannelStateDB) MarkChannelRealScid(channel *OpenChannel,
	realScid lnwire.ShortChannelID) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		diskChannel.SetConfirmedScidForStore(realScid)

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {})
}

// MarkScidAliasNegotiated adds ScidAliasFeatureBit to ChanType in-memory and
// in the database.
func (c *OpenChannel) MarkScidAliasNegotiated() error {
	c.Lock()
	defer c.Unlock()

	if err := c.Db.MarkChannelScidAliasNegotiated(c); err != nil {
		return err
	}

	c.ChanType |= ScidAliasFeatureBit

	return nil
}

// MarkChannelScidAliasNegotiated adds ScidAliasFeatureBit to ChanType in the
// database.
func (c *ChannelStateDB) MarkChannelScidAliasNegotiated(
	channel *OpenChannel) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		diskChannel.ChanType |= ScidAliasFeatureBit

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {})
}

// MarkDataLoss marks sets the channel status to LocalDataLoss and stores the
// passed commitPoint for use to retrieve funds in case the remote force closes
// the channel.
func (c *OpenChannel) MarkDataLoss(commitPoint *btcec.PublicKey) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.MarkChannelDataLoss(c, commitPoint)
}

// MarkChannelDataLoss marks the channel as local-data-loss and stores the
// commit point needed if the remote force closes.
func (c *ChannelStateDB) MarkChannelDataLoss(channel *OpenChannel,
	commitPoint *btcec.PublicKey) error {

	var b bytes.Buffer
	if err := WriteElement(&b, commitPoint); err != nil {
		return err
	}

	putCommitPoint := func(chanBucket kvdb.RwBucket) error {
		return chanBucket.Put(dataLossCommitPointKey, b.Bytes())
	}

	return c.putChanStatus(channel, ChanStatusLocalDataLoss, putCommitPoint)
}

// DataLossCommitPoint retrieves the stored commit point set during
// MarkDataLoss. If not found ErrNoCommitPoint is returned.
func (c *OpenChannel) DataLossCommitPoint() (*btcec.PublicKey, error) {
	return c.Db.FetchChannelDataLossCommitPoint(c)
}

// FetchChannelDataLossCommitPoint retrieves the commit point stored when the
// channel was marked as local-data-loss.
func (c *ChannelStateDB) FetchChannelDataLossCommitPoint(
	channel *OpenChannel) (*btcec.PublicKey, error) {

	var commitPoint *btcec.PublicKey

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch err {
		case nil:
		case ErrNoChanDBExists, ErrNoActiveChannels, ErrChannelNotFound:
			return ErrNoCommitPoint
		default:
			return err
		}

		bs := chanBucket.Get(dataLossCommitPointKey)
		if bs == nil {
			return ErrNoCommitPoint
		}
		r := bytes.NewReader(bs)
		if err := ReadElements(r, &commitPoint); err != nil {
			return err
		}

		return nil
	}, func() {
		commitPoint = nil
	})
	if err != nil {
		return nil, err
	}

	return commitPoint, nil
}

// MarkBorked marks the event when the channel as reached an irreconcilable
// state, such as a channel breach or state desynchronization. Borked channels
// should never be added to the switch.
func (c *OpenChannel) MarkBorked() error {
	c.Lock()
	defer c.Unlock()

	return c.Db.MarkChannelBorked(c)
}

// MarkChannelBorked marks the channel as irreconcilable.
func (c *ChannelStateDB) MarkChannelBorked(channel *OpenChannel) error {
	return c.ApplyChannelStatus(channel, ChanStatusBorked)
}

// SecondCommitmentPoint returns the second per-commitment-point for use in the
// channel_ready message.
func (c *OpenChannel) SecondCommitmentPoint() (*btcec.PublicKey, error) {
	c.RLock()
	defer c.RUnlock()

	// Since we start at commitment height = 0, the second per commitment
	// point is actually at the 1st index.
	revocation, err := c.RevocationProducer.AtIndex(1)
	if err != nil {
		return nil, err
	}

	return input.ComputeCommitmentPoint(revocation[:]), nil
}

var (
	// taprootRevRootKey is the key used to derive the revocation root for
	// the taproot nonces. This is done via HMAC of the existing revocation
	// root.
	taprootRevRootKey = []byte("taproot-rev-root")
)

// DeriveMusig2Shachain derives a shachain producer for the taproot channel
// from normal shachain revocation root.
func DeriveMusig2Shachain(revRoot shachain.Producer) (shachain.Producer, error) { //nolint:ll
	// In order to obtain the revocation root hash to create the taproot
	// revocation, we'll encode the producer into a buffer, then use that
	// to derive the shachain root needed.
	var rootHashBuf bytes.Buffer
	if err := revRoot.Encode(&rootHashBuf); err != nil {
		return nil, fmt.Errorf("unable to encode producer: %w", err)
	}

	revRootHash := chainhash.HashH(rootHashBuf.Bytes())

	// For taproot channel types, we'll also generate a distinct shachain
	// root using the same seed information. We'll use this to generate
	// verification nonces for the channel. We'll bind with this a simple
	// hmac.
	taprootRevHmac := hmac.New(sha256.New, taprootRevRootKey)
	if _, err := taprootRevHmac.Write(revRootHash[:]); err != nil {
		return nil, err
	}

	taprootRevRoot := taprootRevHmac.Sum(nil)

	// Once we have the root, we can then generate our shachain producer
	// and from that generate the per-commitment point.
	return shachain.NewRevocationProducerFromBytes(
		taprootRevRoot,
	)
}

// NewMusigVerificationNonce generates the local or verification nonce for
// another musig2 session. In order to permit our implementation to not have to
// write any secret nonce state to disk, we'll use the _next_ shachain
// pre-image as our primary randomness source. When used to generate the nonce
// again to broadcast our commitment hte current height will be used.
func NewMusigVerificationNonce(pubKey *btcec.PublicKey, targetHeight uint64,
	shaGen shachain.Producer) (*musig2.Nonces, error) {

	// Now that we know what height we need, we'll grab the shachain
	// pre-image at the target destination.
	nextPreimage, err := shaGen.AtIndex(targetHeight)
	if err != nil {
		return nil, err
	}

	shaChainRand := musig2.WithCustomRand(bytes.NewBuffer(nextPreimage[:]))
	pubKeyOpt := musig2.WithPublicKey(pubKey)

	return musig2.GenNonces(pubKeyOpt, shaChainRand)
}

// ChanSyncMsg returns the ChannelReestablish message that should be sent upon
// reconnection with the remote peer that we're maintaining this channel with.
// The information contained within this message is necessary to re-sync our
// commitment chains in the case of a last or only partially processed message.
// When the remote party receives this message one of three things may happen:
//
//  1. We're fully synced and no messages need to be sent.
//  2. We didn't get the last CommitSig message they sent, so they'll re-send
//     it.
//  3. We didn't get the last RevokeAndAck message they sent, so they'll
//     re-send it.
//
// If this is a restored channel, having status ChanStatusRestored, then we'll
// modify our typical chan sync message to ensure they force close even if
// we're on the very first state.
func (c *OpenChannel) ChanSyncMsg() (*lnwire.ChannelReestablish, error) {

	c.Lock()
	defer c.Unlock()

	// The remote commitment height that we'll send in the
	// ChannelReestablish message is our current commitment height plus
	// one. If the receiver thinks that our commitment height is actually
	// *equal* to this value, then they'll re-send the last commitment that
	// they sent but we never fully processed.
	localHeight := c.LocalCommitment.CommitHeight
	nextLocalCommitHeight := localHeight + 1

	// The second value we'll send is the height of the remote commitment
	// from our PoV. If the receiver thinks that their height is actually
	// *one plus* this value, then they'll re-send their last revocation.
	remoteChainTipHeight := c.RemoteCommitment.CommitHeight

	// If this channel has undergone a commitment update, then in order to
	// prove to the remote party our knowledge of their prior commitment
	// state, we'll also send over the last commitment secret that the
	// remote party sent.
	var lastCommitSecret [32]byte
	if remoteChainTipHeight != 0 {
		remoteSecret, err := c.RevocationStore.LookUp(
			remoteChainTipHeight - 1,
		)
		if err != nil {
			return nil, err
		}
		lastCommitSecret = [32]byte(*remoteSecret)
	}

	// Additionally, we'll send over the current unrevoked commitment on
	// our local commitment transaction.
	currentCommitSecret, err := c.RevocationProducer.AtIndex(
		localHeight,
	)
	if err != nil {
		return nil, err
	}

	// If we've restored this channel, then we'll purposefully give them an
	// invalid LocalUnrevokedCommitPoint so they'll force close the channel
	// allowing us to sweep our funds.
	if c.hasChanStatus(ChanStatusRestored) {
		currentCommitSecret[0] ^= 1

		// If this is a tweakless channel, then we'll purposefully send
		// a next local height taht's invalid to trigger a force close
		// on their end. We do this as tweakless channels don't require
		// that the commitment point is valid, only that it's present.
		if c.ChanType.IsTweakless() {
			nextLocalCommitHeight = 0
		}
	}

	// If this is a taproot channel, then we'll need to generate our next
	// verification nonce to send to the remote party. They'll use this to
	// sign the next update to our commitment transaction.
	var (
		nextTaprootNonce lnwire.OptMusig2NonceTLV
		nextLocalNonces  lnwire.OptLocalNonces
	)
	if c.ChanType.IsTaproot() {
		taprootRevProducer, err := DeriveMusig2Shachain(
			c.RevocationProducer,
		)
		if err != nil {
			return nil, err
		}

		nextNonce, err := NewMusigVerificationNonce(
			c.LocalChanCfg.MultiSigKey.PubKey,
			nextLocalCommitHeight, taprootRevProducer,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to gen next "+
				"nonce: %w", err)
		}

		fundingTxid := c.FundingOutpoint.Hash
		nonce := nextNonce.PubNonce

		// Final taproot channels use the map-based LocalNonces
		// field keyed by funding TXID. Staging channels use the
		// legacy single LocalNonce field.
		if c.ChanType.IsTaprootFinal() {
			noncesMap := make(map[chainhash.Hash]lnwire.Musig2Nonce)
			noncesMap[fundingTxid] = nonce
			nextLocalNonces = lnwire.SomeLocalNonces(
				lnwire.LocalNoncesData{NoncesMap: noncesMap},
			)
		} else {
			nextTaprootNonce = lnwire.SomeMusig2Nonce(nonce)
		}
	}

	return &lnwire.ChannelReestablish{
		ChanID: lnwire.NewChanIDFromOutPoint(
			c.FundingOutpoint,
		),
		NextLocalCommitHeight:  nextLocalCommitHeight,
		RemoteCommitTailHeight: remoteChainTipHeight,
		LastRemoteCommitSecret: lastCommitSecret,
		LocalUnrevokedCommitPoint: input.ComputeCommitmentPoint(
			currentCommitSecret[:],
		),
		LocalNonce:  nextTaprootNonce,
		LocalNonces: nextLocalNonces,
	}, nil
}

// MarkShutdownSent serialises and persist the given ShutdownInfo for this
// channel. Persisting this info represents the fact that we have sent the
// Shutdown message to the remote side and hence that we should re-transmit the
// same Shutdown message on re-establish.
func (c *OpenChannel) MarkShutdownSent(info *ShutdownInfo) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.StoreChannelShutdownInfo(c, info)
}

// StoreChannelShutdownInfo persists the ShutdownInfo for the target channel.
func (c *ChannelStateDB) StoreChannelShutdownInfo(channel *OpenChannel,
	info *ShutdownInfo) error {

	var b bytes.Buffer
	err := encodeShutdownInfo(info, &b)
	if err != nil {
		return err
	}

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return chanBucket.Put(shutdownInfoKey, b.Bytes())
	}, func() {})
}

// ShutdownInfo decodes the shutdown info stored for this channel and returns
// the result. If no shutdown info has been persisted for this channel then the
// ErrNoShutdownInfo error is returned.
func (c *OpenChannel) ShutdownInfo() (fn.Option[ShutdownInfo], error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.FetchChannelShutdownInfo(c)
}

// FetchChannelShutdownInfo fetches the persisted ShutdownInfo for the target
// channel.
func (c *ChannelStateDB) FetchChannelShutdownInfo(
	channel *OpenChannel) (fn.Option[ShutdownInfo], error) {

	var shutdownInfo *ShutdownInfo
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoChanDBExists),
			errors.Is(err, ErrNoActiveChannels),
			errors.Is(err, ErrChannelNotFound):

			return ErrNoShutdownInfo
		default:
			return err
		}

		shutdownInfoBytes := chanBucket.Get(shutdownInfoKey)
		if shutdownInfoBytes == nil {
			return ErrNoShutdownInfo
		}

		shutdownInfo, err = decodeShutdownInfo(shutdownInfoBytes)

		return err
	}, func() {
		shutdownInfo = nil
	})
	if err != nil {
		return fn.None[ShutdownInfo](), err
	}

	return fn.Some[ShutdownInfo](*shutdownInfo), nil
}

// isChannelBorked returns true if the channel has been marked as borked in the
// database. This requires an existing database transaction to already be
// active.
//
// NOTE: The primary mutex should already be held before this method is called.
func isChannelBorked(channel *OpenChannel, chanBucket kvdb.RBucket) (
	bool, error) {

	diskChannel, err := fetchOpenChannel(
		chanBucket, &channel.FundingOutpoint,
	)
	if err != nil {
		return false, err
	}

	return diskChannel.ChannelStatusForStore() != ChanStatusDefault, nil
}

// MarkCommitmentBroadcasted marks the channel as a commitment transaction has
// been broadcast, either our own or the remote, and we should watch the chain
// for it to confirm before taking any further action. It takes as argument the
// closing tx _we believe_ will appear in the chain. This is only used to
// republish this tx at startup to ensure propagation, and we should still
// handle the case where a different tx actually hits the chain.
func (c *OpenChannel) MarkCommitmentBroadcasted(closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return c.Db.MarkChannelCommitmentBroadcasted(c, closeTx, closer)
}

// MarkChannelCommitmentBroadcasted marks the channel as having a commitment
// transaction broadcast.
func (c *ChannelStateDB) MarkChannelCommitmentBroadcasted(
	channel *OpenChannel, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return c.markBroadcasted(
		channel, ChanStatusCommitBroadcasted, forceCloseTxKey,
		closeTx, closer,
	)
}

// MarkCoopBroadcasted marks the channel to indicate that a cooperative close
// transaction has been broadcast, either our own or the remote, and that we
// should watch the chain for it to confirm before taking further action. It
// takes as argument a cooperative close tx that could appear on chain, and
// should be rebroadcast upon startup. This is only used to republish and
// ensure propagation, and we should still handle the case where a different tx
// actually hits the chain.
func (c *OpenChannel) MarkCoopBroadcasted(closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	return c.Db.MarkChannelCoopBroadcasted(c, closeTx, closer)
}

// MarkChannelCoopBroadcasted marks the channel as having a cooperative close
// transaction broadcast.
func (c *ChannelStateDB) MarkChannelCoopBroadcasted(channel *OpenChannel,
	closeTx *wire.MsgTx, closer lntypes.ChannelParty) error {

	return c.markBroadcasted(
		channel, ChanStatusCoopBroadcasted, coopCloseTxKey,
		closeTx, closer,
	)
}

// markBroadcasted modifies the channel status and inserts a close transaction
// under the requested key, which should specify either a coop or force close.
// It adds a status which indicates the party that initiated the channel close.
func (c *ChannelStateDB) markBroadcasted(channel *OpenChannel,
	status ChannelStatus, key []byte, closeTx *wire.MsgTx,
	closer lntypes.ChannelParty) error {

	if closeTx == nil {
		return fmt.Errorf("closeTx must be non-nil")
	}

	channel.Lock()
	defer channel.Unlock()

	var b bytes.Buffer
	if err := WriteElement(&b, closeTx); err != nil {
		return err
	}

	putClosingTx := func(chanBucket kvdb.RwBucket) error {
		return chanBucket.Put(key, b.Bytes())
	}

	// Add the initiator status to the status provided. These statuses are
	// set in addition to the broadcast status so that we do not need to
	// migrate the original logic which does not store initiator.
	if closer.IsLocal() {
		status |= ChanStatusLocalCloseInitiator
	} else {
		status |= ChanStatusRemoteCloseInitiator
	}

	return c.putChanStatus(channel, status, putClosingTx)
}

// BroadcastedCommitment retrieves the stored unilateral closing tx set during
// MarkCommitmentBroadcasted. If not found ErrNoCloseTx is returned.
func (c *OpenChannel) BroadcastedCommitment() (*wire.MsgTx, error) {
	return c.Db.FetchChannelBroadcastedCommitment(c)
}

// FetchChannelBroadcastedCommitment fetches the stored unilateral closing
// transaction.
func (c *ChannelStateDB) FetchChannelBroadcastedCommitment(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return c.getClosingTx(channel, forceCloseTxKey)
}

// BroadcastedCooperative retrieves the stored cooperative closing tx set during
// MarkCoopBroadcasted. If not found ErrNoCloseTx is returned.
func (c *OpenChannel) BroadcastedCooperative() (*wire.MsgTx, error) {
	return c.Db.FetchChannelBroadcastedCooperative(c)
}

// FetchChannelBroadcastedCooperative fetches the stored cooperative closing
// transaction.
func (c *ChannelStateDB) FetchChannelBroadcastedCooperative(
	channel *OpenChannel) (*wire.MsgTx, error) {

	return c.getClosingTx(channel, coopCloseTxKey)
}

// getClosingTx returns the stored closing transaction for key. The caller
// should use either the force or coop closing keys.
func (c *ChannelStateDB) getClosingTx(channel *OpenChannel,
	key []byte) (*wire.MsgTx, error) {

	var closeTx *wire.MsgTx

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch err {
		case nil:
		case ErrNoChanDBExists, ErrNoActiveChannels, ErrChannelNotFound:
			return ErrNoCloseTx
		default:
			return err
		}

		bs := chanBucket.Get(key)
		if bs == nil {
			return ErrNoCloseTx
		}
		r := bytes.NewReader(bs)
		return ReadElement(r, &closeTx)
	}, func() {
		closeTx = nil
	})
	if err != nil {
		return nil, err
	}

	return closeTx, nil
}

// ApplyChannelStatus adds the target status to the channel's persisted status
// bit field.
func (c *ChannelStateDB) ApplyChannelStatus(channel *OpenChannel,
	status ChannelStatus) error {

	return c.putChanStatus(channel, status)
}

// putChanStatus appends the given status to the channel. fs is an optional list
// of closures that are given the chanBucket in order to atomically add extra
// information together with the new status.
func (c *ChannelStateDB) putChanStatus(channel *OpenChannel,
	status ChannelStatus, fs ...func(kvdb.RwBucket) error) error {

	if err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		// Add this status to the existing bitvector found in the DB.
		status = diskChannel.ChannelStatusForStore() | status
		diskChannel.SetChannelStatusForStore(status)

		if err := putOpenChannel(chanBucket, diskChannel); err != nil {
			return err
		}

		for _, f := range fs {
			// Skip execution of nil closures.
			if f == nil {
				continue
			}

			if err := f(chanBucket); err != nil {
				return err
			}
		}

		return nil
	}, func() {}); err != nil {
		return err
	}

	// Update the in-memory representation to keep it in sync with the DB.
	channel.SetChannelStatusForStore(status)

	return nil
}

// ClearChannelStatus clears the target status from the channel's persisted
// status bit field.
func (c *ChannelStateDB) ClearChannelStatus(channel *OpenChannel,
	status ChannelStatus) error {

	if err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		diskChannel, err := fetchOpenChannel(
			chanBucket, &channel.FundingOutpoint,
		)
		if err != nil {
			return err
		}

		// Unset this bit in the bitvector on disk.
		status = diskChannel.ChannelStatusForStore() & ^status
		diskChannel.SetChannelStatusForStore(status)

		return putOpenChannel(chanBucket, diskChannel)
	}, func() {}); err != nil {
		return err
	}

	// Update the in-memory representation to keep it in sync with the DB.
	channel.SetChannelStatusForStore(status)

	return nil
}

// putOpenChannel serializes, and stores the current state of the channel in its
// entirety.
func putOpenChannel(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	// First, we'll write out all the relatively static fields, that are
	// decided upon initial channel creation.
	if err := putChanInfo(chanBucket, channel); err != nil {
		return fmt.Errorf("unable to store chan info: %w", err)
	}

	// With the static channel info written out, we'll now write out the
	// current commitment state for both parties.
	if err := putChanCommitments(chanBucket, channel); err != nil {
		return fmt.Errorf("unable to store chan commitments: %w", err)
	}

	// Next, if this is a frozen channel, we'll add in the axillary
	// information we need to store.
	if channel.ChanType.IsFrozen() || channel.ChanType.HasLeaseExpiration() {
		err := storeThawHeight(
			chanBucket, channel.ThawHeight,
		)
		if err != nil {
			return fmt.Errorf("unable to store thaw height: %w",
				err)
		}
	}

	// Finally, we'll write out the revocation state for both parties
	// within a distinct key space.
	if err := putChanRevocationState(chanBucket, channel); err != nil {
		return fmt.Errorf("unable to store chan revocations: %w", err)
	}

	return nil
}

// fetchOpenChannel retrieves, and deserializes (including decrypting
// sensitive) the complete channel currently active with the passed nodeID.
func fetchOpenChannel(chanBucket kvdb.RBucket,
	chanPoint *wire.OutPoint) (*OpenChannel, error) {

	channel := &OpenChannel{
		FundingOutpoint: *chanPoint,
	}

	// First, we'll read all the static information that changes less
	// frequently from disk.
	if err := fetchChanInfo(chanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to fetch chan info: %w", err)
	}

	// With the static information read, we'll now read the current
	// commitment state for both sides of the channel.
	if err := fetchChanCommitments(chanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to fetch chan commitments: %w",
			err)
	}

	// Next, if this is a frozen channel, we'll add in the axillary
	// information we need to store.
	if channel.ChanType.IsFrozen() || channel.ChanType.HasLeaseExpiration() {
		thawHeight, err := fetchThawHeight(chanBucket)
		if err != nil {
			return nil, fmt.Errorf("unable to store thaw "+
				"height: %v", err)
		}

		channel.ThawHeight = thawHeight
	}

	// Finally, we'll retrieve the current revocation state so we can
	// properly
	if err := fetchChanRevocationState(chanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to fetch chan revocations: %w",
			err)
	}

	channel.Packager = NewChannelPackager(channel.ShortChannelID)

	return channel, nil
}

// SyncPending writes the contents of the channel to the database while it's in
// the pending (waiting for funding confirmation) state. The IsPending flag
// will be set to true. When the channel's funding transaction is confirmed,
// the channel should be marked as "open" and the IsPending flag set to false.
// Note that this function also creates a LinkNode relationship between this
// newly created channel and a new LinkNode instance. This allows listing all
// channels in the database globally, or according to the LinkNode they were
// created with.
//
// TODO(roasbeef): addr param should eventually be an lnwire.NetAddress type
// that includes service bits.
func (c *OpenChannel) SyncPending(addr net.Addr, pendingHeight uint32) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.SyncPendingChannel(c, addr, pendingHeight)
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

// UpdateCommitment updates the local commitment state. It locks in the pending
// local updates that were received by us from the remote party. The commitment
// state completely describes the balance state at this point in the commitment
// chain. In addition to that, it persists all the remote log updates that we
// have acked, but not signed a remote commitment for yet. These need to be
// persisted to be able to produce a valid commit signature if a restart would
// occur. This method its to be called when we revoke our prior commitment
// state.
//
// A map is returned of all the htlc resolutions that were locked in this
// commitment. Keys correspond to htlc indices and values indicate whether the
// htlc was settled or failed.
func (c *OpenChannel) UpdateCommitment(newCommitment *ChannelCommitment,
	unsignedAckedUpdates []LogUpdate) (map[uint64]bool, error) {

	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state as all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return nil, ErrNoRestoredChannelMutation
	}

	finalHtlcs, err := c.Db.UpdateChannelCommitment(
		c, newCommitment, unsignedAckedUpdates,
	)
	if err != nil {
		return nil, err
	}

	c.LocalCommitment = *newCommitment

	return finalHtlcs, nil
}

// UpdateChannelCommitment updates the local commitment state.
func (c *ChannelStateDB) UpdateChannelCommitment(channel *OpenChannel,
	newCommitment *ChannelCommitment,
	unsignedAckedUpdates []LogUpdate) (map[uint64]bool, error) {

	var finalHtlcs = make(map[uint64]bool)

	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := isChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		if err = putChanInfo(chanBucket, channel); err != nil {
			return fmt.Errorf("unable to store chan info: %w", err)
		}

		// With the proper bucket fetched, we'll now write the latest
		// commitment state to disk for the target party.
		err = putChanCommitment(
			chanBucket, newCommitment, true,
		)
		if err != nil {
			return fmt.Errorf("unable to store chan "+
				"revocations: %v", err)
		}

		// Persist unsigned but acked remote updates that need to be
		// restored after a restart.
		var b bytes.Buffer
		err = serializeLogUpdates(&b, unsignedAckedUpdates)
		if err != nil {
			return err
		}

		err = chanBucket.Put(unsignedAckedUpdatesKey, b.Bytes())
		if err != nil {
			return fmt.Errorf("unable to store dangline remote "+
				"updates: %v", err)
		}

		// Since we have just sent the counterparty a revocation, store true
		// under lastWasRevokeKey.
		var b2 bytes.Buffer
		if err := WriteElements(&b2, true); err != nil {
			return err
		}

		if err := chanBucket.Put(lastWasRevokeKey, b2.Bytes()); err != nil {
			return err
		}

		// Persist the remote unsigned local updates that are not included
		// in our new commitment.
		updateBytes := chanBucket.Get(remoteUnsignedLocalUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err := deserializeLogUpdates(r)
		if err != nil {
			return err
		}

		// Get the bucket where settled htlcs are recorded if the user
		// opted in to storing this information.
		var finalHtlcsBucket kvdb.RwBucket
		if c.parent.storeFinalHtlcResolutions {
			bucket, err := fetchFinalHtlcsBucketRw(
				tx, channel.ShortChannelID,
			)
			if err != nil {
				return err
			}

			finalHtlcsBucket = bucket
		}

		var unsignedUpdates []LogUpdate
		for _, upd := range updates {
			// Gather updates that are not on our local commitment.
			if upd.LogIndex >= newCommitment.LocalLogIndex {
				unsignedUpdates = append(unsignedUpdates, upd)

				continue
			}

			// The update was locked in. If the update was a
			// resolution, then store it in the database.
			err := processFinalHtlc(
				finalHtlcsBucket, upd, finalHtlcs,
			)
			if err != nil {
				return err
			}
		}

		var b3 bytes.Buffer
		err = serializeLogUpdates(&b3, unsignedUpdates)
		if err != nil {
			return fmt.Errorf("unable to serialize log updates: %w",
				err)
		}

		err = chanBucket.Put(remoteUnsignedLocalUpdatesKey, b3.Bytes())
		if err != nil {
			return fmt.Errorf("unable to restore chanbucket: %w",
				err)
		}

		return nil
	}, func() {
		finalHtlcs = make(map[uint64]bool)
	})
	if err != nil {
		return nil, err
	}

	return finalHtlcs, nil
}

// processFinalHtlc stores a final htlc outcome in the database if signaled via
// the supplied log update. An in-memory htlcs map is updated too.
func processFinalHtlc(finalHtlcsBucket walletdb.ReadWriteBucket, upd LogUpdate,
	finalHtlcs map[uint64]bool) error {

	var (
		settled bool
		id      uint64
	)

	switch msg := upd.UpdateMsg.(type) {
	case *lnwire.UpdateFulfillHTLC:
		settled = true
		id = msg.ID

	case *lnwire.UpdateFailHTLC:
		settled = false
		id = msg.ID

	case *lnwire.UpdateFailMalformedHTLC:
		settled = false
		id = msg.ID

	default:
		return nil
	}

	// Store the final resolution in the database if a bucket is provided.
	if finalHtlcsBucket != nil {
		err := putFinalHtlc(
			finalHtlcsBucket, id,
			FinalHtlcInfo{
				Settled:  settled,
				Offchain: true,
			},
		)
		if err != nil {
			return err
		}
	}

	finalHtlcs[id] = settled

	return nil
}

// ActiveHtlcs returns a slice of HTLC's which are currently active on *both*
// commitment transactions.
func (c *OpenChannel) ActiveHtlcs() []HTLC {
	c.RLock()
	defer c.RUnlock()

	// We'll only return HTLC's that are locked into *both* commitment
	// transactions. So we'll iterate through their set of HTLC's to note
	// which ones are present on their commitment.
	remoteHtlcs := make(map[[32]byte]struct{})
	for _, htlc := range c.RemoteCommitment.Htlcs {
		log.Tracef("RemoteCommitment has htlc: id=%v, update=%v "+
			"incoming=%v", htlc.HtlcIndex, htlc.LogIndex,
			htlc.Incoming)

		onionHash := sha256.Sum256(htlc.OnionBlob[:])
		remoteHtlcs[onionHash] = struct{}{}
	}

	// Now that we know which HTLC's they have, we'll only mark the HTLC's
	// as active if *we* know them as well.
	activeHtlcs := make([]HTLC, 0, len(remoteHtlcs))
	for _, htlc := range c.LocalCommitment.Htlcs {
		log.Tracef("LocalCommitment has htlc: id=%v, update=%v "+
			"incoming=%v", htlc.HtlcIndex, htlc.LogIndex,
			htlc.Incoming)

		onionHash := sha256.Sum256(htlc.OnionBlob[:])
		if _, ok := remoteHtlcs[onionHash]; !ok {
			log.Tracef("Skipped htlc due to onion mismatched: "+
				"id=%v, update=%v incoming=%v",
				htlc.HtlcIndex, htlc.LogIndex, htlc.Incoming)

			continue
		}

		activeHtlcs = append(activeHtlcs, htlc)
	}

	return activeHtlcs
}

// serializeHtlcExtraData encodes a TLV stream of extra data to be stored with a
// HTLC. It uses the update_add_htlc TLV types, because this is where extra
// data is passed with a HTLC. At present blinding points are the only extra
// data that we will store, and the function is a no-op if a nil blinding
// point is provided.
//
// This function MUST be called to persist all HTLC values when they are
// serialized.
func serializeHtlcExtraData(h *HTLC) error {
	var records []tlv.RecordProducer
	h.BlindingPoint.WhenSome(func(b tlv.RecordT[lnwire.BlindingPointTlvType,
		*btcec.PublicKey]) {

		records = append(records, &b)
	})

	records, err := h.CustomRecords.ExtendRecordProducers(records)
	if err != nil {
		return err
	}

	return h.ExtraData.PackRecords(records...)
}

// deserializeHtlcExtraData extracts TLVs from the extra data persisted for the
// HTLC and populates values in the struct accordingly.
//
// This function MUST be called to populate the struct properly when HTLCs
// are deserialized.
func deserializeHtlcExtraData(h *HTLC) error {
	if len(h.ExtraData) == 0 {
		return nil
	}

	blindingPoint := h.BlindingPoint.Zero()
	tlvMap, err := h.ExtraData.ExtractRecords(&blindingPoint)
	if err != nil {
		return err
	}

	if val, ok := tlvMap[h.BlindingPoint.TlvType()]; ok && val == nil {
		h.BlindingPoint = tlv.SomeRecordT(blindingPoint)

		// Remove the entry from the TLV map. Anything left in the map
		// will be included in the custom records field.
		delete(tlvMap, h.BlindingPoint.TlvType())
	}

	// Set the custom records field to the remaining TLV records.
	customRecords, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return err
	}
	h.CustomRecords = customRecords

	return nil
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
	numHtlcs := uint16(len(htlcs))
	if err := WriteElement(b, numHtlcs); err != nil {
		return err
	}

	for _, htlc := range htlcs {
		// Populate TLV stream for any additional fields contained
		// in the TLV.
		if err := serializeHtlcExtraData(&htlc); err != nil {
			return err
		}

		// The onion blob and hltc data are stored as a single var
		// bytes blob.
		onionAndExtraData := make(
			[]byte, lnwire.OnionPacketSize+len(htlc.ExtraData),
		)
		copy(onionAndExtraData, htlc.OnionBlob[:])
		copy(onionAndExtraData[lnwire.OnionPacketSize:], htlc.ExtraData)

		if err := WriteElements(b,
			htlc.Signature, htlc.RHash, htlc.Amt, htlc.RefundTimeout,
			htlc.OutputIndex, htlc.Incoming, onionAndExtraData,
			htlc.HtlcIndex, htlc.LogIndex,
		); err != nil {
			return err
		}
	}

	return nil
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
	var numHtlcs uint16
	if err := ReadElement(r, &numHtlcs); err != nil {
		return nil, err
	}

	var htlcs []HTLC
	if numHtlcs == 0 {
		return htlcs, nil
	}

	htlcs = make([]HTLC, numHtlcs)
	for i := uint16(0); i < numHtlcs; i++ {
		var onionAndExtraData []byte
		if err := ReadElements(r,
			&htlcs[i].Signature, &htlcs[i].RHash, &htlcs[i].Amt,
			&htlcs[i].RefundTimeout, &htlcs[i].OutputIndex,
			&htlcs[i].Incoming, &onionAndExtraData,
			&htlcs[i].HtlcIndex, &htlcs[i].LogIndex,
		); err != nil {
			return htlcs, err
		}

		// Sanity check that we have at least the onion blob size we
		// expect.
		if len(onionAndExtraData) < lnwire.OnionPacketSize {
			return nil, ErrOnionBlobLength
		}

		// First OnionPacketSize bytes are our fixed length onion
		// packet.
		copy(
			htlcs[i].OnionBlob[:],
			onionAndExtraData[0:lnwire.OnionPacketSize],
		)

		// Any additional bytes belong to extra data. ExtraDataLen
		// will be >= 0, because we know that we always have a fixed
		// length onion packet.
		extraDataLen := len(onionAndExtraData) - lnwire.OnionPacketSize
		if extraDataLen > 0 {
			htlcs[i].ExtraData = make([]byte, extraDataLen)

			copy(
				htlcs[i].ExtraData,
				onionAndExtraData[lnwire.OnionPacketSize:],
			)
		}

		// Finally, deserialize any TLVs contained in that extra data
		// if they are present.
		if err := deserializeHtlcExtraData(&htlcs[i]); err != nil {
			return nil, err
		}
	}

	return htlcs, nil
}

// serializeLogUpdate writes a log update to the provided io.Writer.
func serializeLogUpdate(w io.Writer, l *LogUpdate) error {
	return WriteElements(w, l.LogIndex, l.UpdateMsg)
}

// deserializeLogUpdate reads a log update from the provided io.Reader.
func deserializeLogUpdate(r io.Reader) (*LogUpdate, error) {
	l := &LogUpdate{}
	if err := ReadElements(r, &l.LogIndex, &l.UpdateMsg); err != nil {
		return nil, err
	}

	return l, nil
}

// serializeLogUpdates serializes provided list of updates to a stream.
func serializeLogUpdates(w io.Writer, logUpdates []LogUpdate) error {
	numUpdates := uint16(len(logUpdates))
	if err := binary.Write(w, byteOrder, numUpdates); err != nil {
		return err
	}

	for _, diff := range logUpdates {
		err := WriteElements(w, diff.LogIndex, diff.UpdateMsg)
		if err != nil {
			return err
		}
	}

	return nil
}

// deserializeLogUpdates deserializes a list of updates from a stream.
func deserializeLogUpdates(r io.Reader) ([]LogUpdate, error) {
	var numUpdates uint16
	if err := binary.Read(r, byteOrder, &numUpdates); err != nil {
		return nil, err
	}

	logUpdates := make([]LogUpdate, numUpdates)
	for i := 0; i < int(numUpdates); i++ {
		err := ReadElements(r,
			&logUpdates[i].LogIndex, &logUpdates[i].UpdateMsg,
		)
		if err != nil {
			return nil, err
		}
	}
	return logUpdates, nil
}

func serializeCommitDiff(w io.Writer, diff *CommitDiff) error { // nolint: dupl
	if err := serializeChanCommit(w, &diff.Commitment); err != nil {
		return err
	}

	if err := WriteElements(w, diff.CommitSig); err != nil {
		return err
	}

	if err := serializeLogUpdates(w, diff.LogUpdates); err != nil {
		return err
	}

	numOpenRefs := uint16(len(diff.OpenedCircuitKeys))
	if err := binary.Write(w, byteOrder, numOpenRefs); err != nil {
		return err
	}

	for _, openRef := range diff.OpenedCircuitKeys {
		err := WriteElements(w, openRef.ChanID, openRef.HtlcID)
		if err != nil {
			return err
		}
	}

	numClosedRefs := uint16(len(diff.ClosedCircuitKeys))
	if err := binary.Write(w, byteOrder, numClosedRefs); err != nil {
		return err
	}

	for _, closedRef := range diff.ClosedCircuitKeys {
		err := WriteElements(w, closedRef.ChanID, closedRef.HtlcID)
		if err != nil {
			return err
		}
	}

	// We'll also encode the commit aux data stream here. We do this here
	// rather than above (at the call to serializeChanCommit), to ensure
	// backwards compat for reads to existing non-custom channels.
	auxData := extractCommitTlvData(&diff.Commitment)
	if err := auxData.encode(w); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return nil
}

func deserializeCommitDiff(r io.Reader) (*CommitDiff, error) {
	var (
		d   CommitDiff
		err error
	)

	d.Commitment, err = deserializeChanCommit(r)
	if err != nil {
		return nil, err
	}

	var msg lnwire.Message
	if err := ReadElements(r, &msg); err != nil {
		return nil, err
	}
	commitSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		return nil, fmt.Errorf("expected lnwire.CommitSig, instead "+
			"read: %T", msg)
	}
	d.CommitSig = commitSig

	d.LogUpdates, err = deserializeLogUpdates(r)
	if err != nil {
		return nil, err
	}

	var numOpenRefs uint16
	if err := binary.Read(r, byteOrder, &numOpenRefs); err != nil {
		return nil, err
	}

	d.OpenedCircuitKeys = make([]models.CircuitKey, numOpenRefs)
	for i := 0; i < int(numOpenRefs); i++ {
		err := ReadElements(r,
			&d.OpenedCircuitKeys[i].ChanID,
			&d.OpenedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	var numClosedRefs uint16
	if err := binary.Read(r, byteOrder, &numClosedRefs); err != nil {
		return nil, err
	}

	d.ClosedCircuitKeys = make([]models.CircuitKey, numClosedRefs)
	for i := 0; i < int(numClosedRefs); i++ {
		err := ReadElements(r,
			&d.ClosedCircuitKeys[i].ChanID,
			&d.ClosedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	// As a final step, we'll read out any aux commit data that we have at
	// the end of this byte stream. We do this here to ensure backward
	// compatibility, as otherwise we risk erroneously reading into the
	// wrong field.
	var auxData commitTlvData
	if err := auxData.decode(r); err != nil {
		return nil, fmt.Errorf("unable to decode aux data: %w", err)
	}

	amendCommitTlvData(&d.Commitment, auxData)

	return &d, nil
}

// AppendRemoteCommitChain appends a new CommitDiff to the end of the
// commitment chain for the remote party. This method is to be used once we
// have prepared a new commitment state for the remote party, but before we
// transmit it to the remote party. The contents of the argument should be
// sufficient to retransmit the updates and signature needed to reconstruct the
// state in full, in the case that we need to retransmit.
func (c *OpenChannel) AppendRemoteCommitChain(diff *CommitDiff) error {
	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state at all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return ErrNoRestoredChannelMutation
	}

	return c.Db.AppendRemoteCommitChain(c, diff)
}

// AppendRemoteCommitChain appends a new CommitDiff to the remote party's
// commitment chain.
func (c *ChannelStateDB) AppendRemoteCommitChain(channel *OpenChannel,
	diff *CommitDiff) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		// First, we'll grab the writable bucket where this channel's
		// data resides.
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := isChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		// Any outgoing settles and fails necessarily have a
		// corresponding adds in this channel's forwarding packages.
		// Mark all of these as being fully processed in our forwarding
		// package, which prevents us from reprocessing them after
		// startup.
		err = channel.Packager.AckAddHtlcs(tx, diff.AddAcks...)
		if err != nil {
			return err
		}

		// Additionally, we ack from any fails or settles that are
		// persisted in another channel's forwarding package. This
		// prevents the same fails and settles from being retransmitted
		// after restarts. The actual fail or settle we need to
		// propagate to the remote party is now in the commit diff.
		err = channel.Packager.AckSettleFails(
			tx, diff.SettleFailAcks...,
		)
		if err != nil {
			return err
		}

		// We are sending a commitment signature so lastWasRevokeKey should
		// store false.
		var b bytes.Buffer
		if err := WriteElements(&b, false); err != nil {
			return err
		}
		if err := chanBucket.Put(lastWasRevokeKey, b.Bytes()); err != nil {
			return err
		}

		// TODO(roasbeef): use seqno to derive key for later LCP

		// With the bucket retrieved, we'll now serialize the commit
		// diff itself, and write it to disk.
		var b2 bytes.Buffer
		if err := serializeCommitDiff(&b2, diff); err != nil {
			return err
		}
		return chanBucket.Put(commitDiffKey, b2.Bytes())
	}, func() {})
}

// RemoteCommitChainTip returns the "tip" of the current remote commitment
// chain. This value will be non-nil iff, we've created a new commitment for
// the remote party that they haven't yet ACK'd. In this case, their commitment
// chain will have a length of two: their current unrevoked commitment, and
// this new pending commitment. Once they revoked their prior state, we'll swap
// these pointers, causing the tip and the tail to point to the same entry.
func (c *OpenChannel) RemoteCommitChainTip() (*CommitDiff, error) {
	return c.Db.RemoteCommitChainTip(c)
}

// RemoteCommitChainTip returns the "tip" of the current remote commitment
// chain.
func (c *ChannelStateDB) RemoteCommitChainTip(channel *OpenChannel) (
	*CommitDiff, error) {

	var cd *CommitDiff
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch err {
		case nil:
		case ErrNoChanDBExists, ErrNoActiveChannels, ErrChannelNotFound:
			return ErrNoPendingCommit
		default:
			return err
		}

		tipBytes := chanBucket.Get(commitDiffKey)
		if tipBytes == nil {
			return ErrNoPendingCommit
		}

		tipReader := bytes.NewReader(tipBytes)
		dcd, err := deserializeCommitDiff(tipReader)
		if err != nil {
			return err
		}

		cd = dcd
		return nil
	}, func() {
		cd = nil
	})
	if err != nil {
		return nil, err
	}

	return cd, nil
}

// UnsignedAckedUpdates retrieves the persisted unsigned acked remote log
// updates that still need to be signed for.
func (c *OpenChannel) UnsignedAckedUpdates() ([]LogUpdate, error) {
	return c.Db.UnsignedAckedUpdates(c)
}

// UnsignedAckedUpdates retrieves the persisted unsigned acked remote log
// updates that still need to be signed for.
func (c *ChannelStateDB) UnsignedAckedUpdates(channel *OpenChannel) (
	[]LogUpdate, error) {

	var updates []LogUpdate
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch err {
		case nil:
		case ErrNoChanDBExists, ErrNoActiveChannels, ErrChannelNotFound:
			return nil
		default:
			return err
		}

		updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err = deserializeLogUpdates(r)
		return err
	}, func() {
		updates = nil
	})
	if err != nil {
		return nil, err
	}

	return updates, nil
}

// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local log
// updates that the remote still needs to sign for.
func (c *OpenChannel) RemoteUnsignedLocalUpdates() ([]LogUpdate, error) {
	return c.Db.RemoteUnsignedLocalUpdates(c)
}

// RemoteUnsignedLocalUpdates retrieves the persisted, unsigned local log
// updates that the remote still needs to sign for.
func (c *ChannelStateDB) RemoteUnsignedLocalUpdates(channel *OpenChannel) (
	[]LogUpdate, error) {

	var updates []LogUpdate
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		switch err {
		case nil:
			break
		case ErrNoChanDBExists, ErrNoActiveChannels, ErrChannelNotFound:
			return nil
		default:
			return err
		}

		updateBytes := chanBucket.Get(remoteUnsignedLocalUpdatesKey)
		if updateBytes == nil {
			return nil
		}

		r := bytes.NewReader(updateBytes)
		updates, err = deserializeLogUpdates(r)
		return err
	}, func() {
		updates = nil
	})
	if err != nil {
		return nil, err
	}

	return updates, nil
}

// InsertNextRevocation inserts the _next_ commitment point (revocation) into
// the database, and also modifies the internal RemoteNextRevocation attribute
// to point to the passed key. This method is to be using during final channel
// set up, _after_ the channel has been fully confirmed.
//
// NOTE: If this method isn't called, then the target channel won't be able to
// propose new states for the commitment state of the remote party.
func (c *OpenChannel) InsertNextRevocation(revKey *btcec.PublicKey) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.InsertNextRevocation(c, revKey)
}

// InsertNextRevocation inserts the next commitment point into the persisted
// channel state.
func (c *ChannelStateDB) InsertNextRevocation(channel *OpenChannel,
	revKey *btcec.PublicKey) error {

	channel.RemoteNextRevocation = revKey

	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return putChanRevocationState(chanBucket, channel)
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// AdvanceCommitChainTail records the new state transition within an on-disk
// append-only log which records all state transitions by the remote peer. In
// the case of an uncooperative broadcast of a prior state by the remote peer,
// this log can be consulted in order to reconstruct the state needed to
// rectify the situation. This method will add the current commitment for the
// remote party to the revocation log, and promote the current pending
// commitment to the current remote commitment. The updates parameter is the
// set of local updates that the peer still needs to send us a signature for.
// We store this set of updates in case we go down.
func (c *OpenChannel) AdvanceCommitChainTail(fwdPkg *FwdPkg,
	updates []LogUpdate, ourOutputIndex, theirOutputIndex uint32) error {

	c.Lock()
	defer c.Unlock()

	// If this is a restored channel, then we want to avoid mutating the
	// state at all, as it's impossible to do so in a protocol compliant
	// manner.
	if c.hasChanStatus(ChanStatusRestored) {
		return ErrNoRestoredChannelMutation
	}

	return c.Db.AdvanceCommitChainTail(
		c, fwdPkg, updates, ourOutputIndex, theirOutputIndex,
	)
}

// AdvanceCommitChainTail records the new state transition within the
// revocation log and promotes the pending remote commitment to the current
// remote commitment.
func (c *ChannelStateDB) AdvanceCommitChainTail(channel *OpenChannel,
	fwdPkg *FwdPkg, updates []LogUpdate, ourOutputIndex,
	theirOutputIndex uint32) error {

	var newRemoteCommit *ChannelCommitment

	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		chanBucket, err := fetchChanBucketRw(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// If the channel is marked as borked, then for safety reasons,
		// we shouldn't attempt any further updates.
		isBorked, err := isChannelBorked(channel, chanBucket)
		if err != nil {
			return err
		}
		if isBorked {
			return ErrChanBorked
		}

		// Persist the latest preimage state to disk as the remote peer
		// has just added to our local preimage store, and given us a
		// new pending revocation key.
		err = putChanRevocationState(chanBucket, channel)
		if err != nil {
			return err
		}

		// With the current preimage producer/store state updated,
		// append a new log entry recording this the delta of this
		// state transition.
		//
		// TODO(roasbeef): could make the deltas relative, would save
		// space, but then tradeoff for more disk-seeks to recover the
		// full state.
		logKey := revocationLogBucket
		logBucket, err := chanBucket.CreateBucketIfNotExists(logKey)
		if err != nil {
			return err
		}

		// Before we append this revoked state to the revocation log,
		// we'll swap out what's currently the tail of the commit tip,
		// with the current locked-in commitment for the remote party.
		tipBytes := chanBucket.Get(commitDiffKey)
		tipReader := bytes.NewReader(tipBytes)
		newCommit, err := deserializeCommitDiff(tipReader)
		if err != nil {
			return err
		}
		err = putChanCommitment(
			chanBucket, &newCommit.Commitment, false,
		)
		if err != nil {
			return err
		}
		if err := chanBucket.Delete(commitDiffKey); err != nil {
			return err
		}

		// With the commitment pointer swapped, we can now add the
		// revoked (prior) state to the revocation log.
		err = putRevocationLog(
			logBucket, &channel.RemoteCommitment, ourOutputIndex,
			theirOutputIndex, c.parent.noRevLogAmtData,
		)
		if err != nil {
			return err
		}

		// Lastly, we write the forwarding package to disk so that we
		// can properly recover from failures and reforward HTLCs that
		// have not received a corresponding settle/fail.
		if err := channel.Packager.AddFwdPkg(tx, fwdPkg); err != nil {
			return err
		}

		// Persist the unsigned acked updates that are not included
		// in their new commitment.
		updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
		if updateBytes == nil {
			// This shouldn't normally happen as we always store
			// the number of updates, but could still be
			// encountered by nodes that are upgrading.
			newRemoteCommit = &newCommit.Commitment
			return nil
		}

		r := bytes.NewReader(updateBytes)
		unsignedUpdates, err := deserializeLogUpdates(r)
		if err != nil {
			return err
		}

		var validUpdates []LogUpdate
		for _, upd := range unsignedUpdates {
			lIdx := upd.LogIndex

			// Filter for updates that are not on the remote
			// commitment.
			if lIdx >= newCommit.Commitment.RemoteLogIndex {
				validUpdates = append(validUpdates, upd)
			}
		}

		var b bytes.Buffer
		err = serializeLogUpdates(&b, validUpdates)
		if err != nil {
			return fmt.Errorf("unable to serialize log updates: %w",
				err)
		}

		err = chanBucket.Put(unsignedAckedUpdatesKey, b.Bytes())
		if err != nil {
			return fmt.Errorf("unable to store under "+
				"unsignedAckedUpdatesKey: %w", err)
		}

		// Persist the local updates the peer hasn't yet signed so they
		// can be restored after restart.
		var b2 bytes.Buffer
		err = serializeLogUpdates(&b2, updates)
		if err != nil {
			return err
		}

		err = chanBucket.Put(remoteUnsignedLocalUpdatesKey, b2.Bytes())
		if err != nil {
			return fmt.Errorf("unable to restore remote unsigned "+
				"local updates: %v", err)
		}

		newRemoteCommit = &newCommit.Commitment

		return nil
	}, func() {
		newRemoteCommit = nil
	})
	if err != nil {
		return err
	}

	// With the db transaction complete, we'll swap over the in-memory
	// pointer of the new remote commitment, which was previously the tip
	// of the commit chain.
	channel.RemoteCommitment = *newRemoteCommit

	return nil
}

// FinalHtlcInfo contains information about the final outcome of an htlc.
type FinalHtlcInfo = cstate.FinalHtlcInfo

// putFinalHtlc writes the final htlc outcome to the database. Additionally it
// records whether the htlc was resolved off-chain or on-chain.
func putFinalHtlc(finalHtlcsBucket kvdb.RwBucket, id uint64,
	info FinalHtlcInfo) error {

	var key [8]byte
	byteOrder.PutUint64(key[:], id)

	var finalHtlcByte FinalHtlcByte
	if info.Settled {
		finalHtlcByte |= FinalHtlcSettledBit
	}
	if info.Offchain {
		finalHtlcByte |= FinalHtlcOffchainBit
	}

	return finalHtlcsBucket.Put(key[:], []byte{byte(finalHtlcByte)})
}

// NextLocalHtlcIndex returns the next unallocated local htlc index. To ensure
// this always returns the next index that has been not been allocated, this
// will first try to examine any pending commitments, before falling back to the
// last locked-in remote commitment.
func (c *OpenChannel) NextLocalHtlcIndex() (uint64, error) {
	// First, load the most recent commit diff that we initiated for the
	// remote party. If no pending commit is found, this is not treated as
	// a critical error, since we can always fall back.
	pendingRemoteCommit, err := c.RemoteCommitChainTip()
	if err != nil && err != ErrNoPendingCommit {
		return 0, err
	}

	// If a pending commit was found, its local htlc index will be at least
	// as large as the one on our local commitment.
	if pendingRemoteCommit != nil {
		return pendingRemoteCommit.Commitment.LocalHtlcIndex, nil
	}

	// Otherwise, fallback to using the local htlc index of their commitment.
	return c.RemoteCommitment.LocalHtlcIndex, nil
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in map indexed by the
// remote commitment height at which the updates were locked in.
func (c *OpenChannel) LoadFwdPkgs() ([]*FwdPkg, error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.LoadFwdPkgs(c)
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in map indexed by the
// remote commitment height at which the updates were locked in.
func (c *ChannelStateDB) LoadFwdPkgs(channel *OpenChannel) ([]*FwdPkg,
	error) {

	var fwdPkgs []*FwdPkg
	if err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		var err error
		fwdPkgs, err = channel.Packager.LoadFwdPkgs(tx)
		return err
	}, func() {
		fwdPkgs = nil
	}); err != nil {
		return nil, err
	}

	return fwdPkgs, nil
}

// AckAddHtlcs updates the AckAddFilter containing any of the provided AddRefs
// indicating that a response to this Add has been committed to the remote party.
// Doing so will prevent these Add HTLCs from being reforwarded internally.
func (c *OpenChannel) AckAddHtlcs(addRefs ...AddRef) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.AckAddHtlcs(c, addRefs...)
}

// AckAddHtlcs updates the AckAddFilter containing any of the provided AddRefs
// indicating that a response to this Add has been committed to the remote party.
// Doing so will prevent these Add HTLCs from being reforwarded internally.
func (c *ChannelStateDB) AckAddHtlcs(channel *OpenChannel,
	addRefs ...AddRef) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		return channel.Packager.AckAddHtlcs(tx, addRefs...)
	}, func() {})
}

// AckSettleFails updates the SettleFailFilter containing any of the provided
// SettleFailRefs, indicating that the response has been delivered to the
// incoming link, corresponding to a particular AddRef. Doing so will prevent
// the responses from being retransmitted internally.
func (c *OpenChannel) AckSettleFails(settleFailRefs ...SettleFailRef) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.AckSettleFails(c, settleFailRefs...)
}

// AckSettleFails updates the SettleFailFilter containing any of the provided
// SettleFailRefs, indicating that the response has been delivered to the
// incoming link, corresponding to a particular AddRef. Doing so will prevent
// the responses from being retransmitted internally.
func (c *ChannelStateDB) AckSettleFails(channel *OpenChannel,
	settleFailRefs ...SettleFailRef) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		return channel.Packager.AckSettleFails(tx, settleFailRefs...)
	}, func() {})
}

// SetFwdFilter atomically sets the forwarding filter for the forwarding package
// identified by `height`.
func (c *OpenChannel) SetFwdFilter(height uint64, fwdFilter *PkgFilter) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.SetFwdFilter(c, height, fwdFilter)
}

// SetFwdFilter atomically sets the forwarding filter for the forwarding package
// identified by `height`.
func (c *ChannelStateDB) SetFwdFilter(channel *OpenChannel, height uint64,
	fwdFilter *PkgFilter) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		return channel.Packager.SetFwdFilter(tx, height, fwdFilter)
	}, func() {})
}

// RemoveFwdPkgs atomically removes forwarding packages specified by the remote
// commitment heights. If one of the intermediate RemovePkg calls fails, then the
// later packages won't be removed.
//
// NOTE: This method should only be called on packages marked FwdStateCompleted.
func (c *OpenChannel) RemoveFwdPkgs(heights ...uint64) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.RemoveFwdPkgs(c, heights...)
}

// RemoveFwdPkgs atomically removes forwarding packages specified by the remote
// commitment heights. If one of the intermediate RemovePkg calls fails, then the
// later packages won't be removed.
//
// NOTE: This method should only be called on packages marked FwdStateCompleted.
func (c *ChannelStateDB) RemoveFwdPkgs(channel *OpenChannel,
	heights ...uint64) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		for _, height := range heights {
			err := channel.Packager.RemovePkg(tx, height)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// revocationLogTailCommitHeight returns the commit height at the end of the
// revocation log.
func (c *ChannelStateDB) revocationLogTailCommitHeight(
	channel *OpenChannel) (uint64, error) {

	var height uint64

	// If we haven't created any state updates yet, then we'll exit early as
	// there's nothing to be found on disk in the revocation bucket.
	if channel.RemoteCommitment.CommitHeight == 0 {
		return height, nil
	}

	if err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		logBucket, err := fetchLogBucket(chanBucket)
		if err != nil {
			return err
		}

		// Once we have the bucket that stores the revocation log from
		// this channel, we'll jump to the _last_ key in bucket. Since
		// the key is the commit height, we'll decode the bytes and
		// return it.
		cursor := logBucket.ReadCursor()
		rawHeight, _ := cursor.Last()
		height = byteOrder.Uint64(rawHeight)

		return nil
	}, func() {}); err != nil {
		return height, err
	}

	return height, nil
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to date.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to date.
func (c *OpenChannel) CommitmentHeight() (uint64, error) {
	c.RLock()
	defer c.RUnlock()

	return c.Db.CommitmentHeight(c)
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to date.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to date.
func (c *ChannelStateDB) CommitmentHeight(channel *OpenChannel) (
	uint64, error) {

	var height uint64
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		commit, err := fetchChanCommitment(chanBucket, true)
		if err != nil {
			return err
		}

		height = commit.CommitHeight
		return nil
	}, func() {
		height = 0
	})
	if err != nil {
		return 0, err
	}

	return height, nil
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *OpenChannel) FindPreviousState(
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	c.RLock()
	defer c.RUnlock()

	return c.Db.FindPreviousState(c, updateNum)
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *ChannelStateDB) FindPreviousState(channel *OpenChannel,
	updateNum uint64) (*RevocationLog, *ChannelCommitment, error) {

	commit := &ChannelCommitment{}
	rl := &RevocationLog{}

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		// Find the revocation log from both the new and the old
		// bucket.
		r, c, err := fetchRevocationLogCompatible(chanBucket, updateNum)
		if err != nil {
			return err
		}

		rl = r
		commit = c
		return nil
	}, func() {})
	if err != nil {
		return nil, nil, err
	}

	// Either the `rl` or the `commit` is nil here. We return them as-is
	// and leave it to the caller to decide its following action.
	return rl, commit, nil
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

// CloseChannel closes a previously active Lightning channel. Closing a
// channel entails persisting a record of the close while either purging the
// nested per-channel state inline (synchronous backends like bbolt and etcd)
// or skipping the cascading delete on tombstone-enabled backends, where the
// outpoint-index flip to outpointClosed is the authoritative marker. The
// compact summary written to closedChannelBucket and the historical record
// under historicalChannelBucket are populated identically across both paths,
// so historical reads remain uniform regardless of backend. The optional set
// of channel statuses is OR'd into the chanStatus written to the historical
// bucket and is used to record close initiators.
func (c *OpenChannel) CloseChannel(summary *ChannelCloseSummary,
	statuses ...ChannelStatus) error {

	c.Lock()
	defer c.Unlock()

	return c.Db.CloseChannel(c, summary, statuses...)
}

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
	opBucket := tx.ReadWriteBucket(outpointBucket)
	if opBucket == nil {
		return ErrNoChanDBExists
	}
	if opBucket.Get(chanKey) == nil {
		return ErrMissingIndexEntry
	}

	status := uint8(outpointClosed)
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &status)
	opStream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := opStream.Encode(&b); err != nil {
		return err
	}

	return opBucket.Put(chanKey, b.Bytes())
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

		if err = chanState.Packager.Wipe(tx); err != nil {
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

// ChannelSnapshot is a frozen snapshot of the current channel state. A
// snapshot is detached from the original channel that generated it, providing
// read-only access to the current or prior state of an active channel.
//
// TODO(roasbeef): remove all together? pretty much just commitment
type ChannelSnapshot struct {
	// RemoteIdentity is the identity public key of the remote node that we
	// are maintaining the open channel with.
	RemoteIdentity btcec.PublicKey

	// ChanPoint is the outpoint that created the channel. This output is
	// found within the funding transaction and uniquely identified the
	// channel on the resident chain.
	ChannelPoint wire.OutPoint

	// ChainHash is the genesis hash of the chain that the channel resides
	// within.
	ChainHash chainhash.Hash

	// Capacity is the total capacity of the channel.
	Capacity btcutil.Amount

	// TotalMSatSent is the total number of milli-satoshis we've sent
	// within this channel.
	TotalMSatSent lnwire.MilliSatoshi

	// TotalMSatReceived is the total number of milli-satoshis we've
	// received within this channel.
	TotalMSatReceived lnwire.MilliSatoshi

	// ChannelCommitment is the current up-to-date commitment for the
	// target channel.
	ChannelCommitment
}

// Snapshot returns a read-only snapshot of the current channel state. This
// snapshot includes information concerning the current settled balance within
// the channel, metadata detailing total flows, and any outstanding HTLCs.
func (c *OpenChannel) Snapshot() *ChannelSnapshot {
	c.RLock()
	defer c.RUnlock()

	localCommit := c.LocalCommitment
	snapshot := &ChannelSnapshot{
		RemoteIdentity:    *c.IdentityPub,
		ChannelPoint:      c.FundingOutpoint,
		Capacity:          c.Capacity,
		TotalMSatSent:     c.TotalMSatSent,
		TotalMSatReceived: c.TotalMSatReceived,
		ChainHash:         c.ChainHash,
		ChannelCommitment: ChannelCommitment{
			LocalBalance:  localCommit.LocalBalance,
			RemoteBalance: localCommit.RemoteBalance,
			CommitHeight:  localCommit.CommitHeight,
			CommitFee:     localCommit.CommitFee,
		},
	}

	localCommit.CustomBlob.WhenSome(func(blob tlv.Blob) {
		blobCopy := make([]byte, len(blob))
		copy(blobCopy, blob)

		snapshot.ChannelCommitment.CustomBlob = fn.Some(blobCopy)
	})

	// Copy over the current set of HTLCs to ensure the caller can't mutate
	// our internal state.
	snapshot.Htlcs = make([]HTLC, len(localCommit.Htlcs))
	for i, h := range localCommit.Htlcs {
		snapshot.Htlcs[i] = h.Copy()
	}

	return snapshot
}

// Copy returns a deep copy of the channel state.
func (c *OpenChannel) Copy() *OpenChannel {
	c.RLock()
	defer c.RUnlock()

	clone := &OpenChannel{
		ChanType:                c.ChanType,
		ChainHash:               c.ChainHash,
		FundingOutpoint:         c.FundingOutpoint,
		ShortChannelID:          c.ShortChannelID,
		IsPending:               c.IsPending,
		IsInitiator:             c.IsInitiator,
		chanStatus:              c.chanStatus,
		FundingBroadcastHeight:  c.FundingBroadcastHeight,
		ConfirmationHeight:      c.ConfirmationHeight,
		NumConfsRequired:        c.NumConfsRequired,
		ChannelFlags:            c.ChannelFlags,
		IdentityPub:             c.IdentityPub,
		Capacity:                c.Capacity,
		TotalMSatSent:           c.TotalMSatSent,
		TotalMSatReceived:       c.TotalMSatReceived,
		InitialLocalBalance:     c.InitialLocalBalance,
		InitialRemoteBalance:    c.InitialRemoteBalance,
		LocalChanCfg:            c.LocalChanCfg,
		RemoteChanCfg:           c.RemoteChanCfg,
		LocalCommitment:         c.LocalCommitment.Copy(),
		RemoteCommitment:        c.RemoteCommitment.Copy(),
		RemoteCurrentRevocation: c.RemoteCurrentRevocation,
		RemoteNextRevocation:    c.RemoteNextRevocation,
		RevocationProducer:      c.RevocationProducer,
		RevocationStore:         c.RevocationStore,
		Packager:                c.Packager,
		ThawHeight:              c.ThawHeight,
		LastWasRevoke:           c.LastWasRevoke,
		RevocationKeyLocator:    c.RevocationKeyLocator,
		confirmedScid:           c.confirmedScid,
		TapscriptRoot:           c.TapscriptRoot,
	}

	if c.FundingTxn != nil {
		clone.FundingTxn = c.FundingTxn.Copy()
	}

	if len(c.LocalShutdownScript) > 0 {
		clone.LocalShutdownScript = make(
			lnwire.DeliveryAddress,
			len(c.LocalShutdownScript),
		)
		copy(clone.LocalShutdownScript, c.LocalShutdownScript)
	}
	if len(c.RemoteShutdownScript) > 0 {
		clone.RemoteShutdownScript = make(
			lnwire.DeliveryAddress,
			len(c.RemoteShutdownScript),
		)
		copy(clone.RemoteShutdownScript, c.RemoteShutdownScript)
	}

	if len(c.Memo) > 0 {
		clone.Memo = make([]byte, len(c.Memo))
		copy(clone.Memo, c.Memo)
	}

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		blobCopy := make([]byte, len(blob))
		copy(blobCopy, blob)
		clone.CustomBlob = fn.Some(blobCopy)
	})

	return clone
}

// LatestCommitments returns the two latest commitments for both the local and
// remote party. These commitments are read from disk to ensure that only the
// latest fully committed state is returned. The first commitment returned is
// the local commitment, and the second returned is the remote commitment.
func (c *OpenChannel) LatestCommitments() (*ChannelCommitment, *ChannelCommitment, error) {
	return c.Db.LatestCommitments(c)
}

// LatestCommitments returns the two latest commitments for both the local and
// remote party. These commitments are read from disk to ensure that only the
// latest fully committed state is returned. The first commitment returned is
// the local commitment, and the second returned is the remote commitment.
func (c *ChannelStateDB) LatestCommitments(channel *OpenChannel) (
	*ChannelCommitment, *ChannelCommitment, error) {

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return fetchChanCommitments(chanBucket, channel)
	}, func() {})
	if err != nil {
		return nil, nil, err
	}

	return &channel.LocalCommitment, &channel.RemoteCommitment, nil
}

// RemoteRevocationStore returns the most up to date commitment version of the
// revocation storage tree for the remote party. This method can be used when
// acting on a possible contract breach to ensure, that the caller has the most
// up to date information required to deliver justice.
func (c *OpenChannel) RemoteRevocationStore() (shachain.Store, error) {
	return c.Db.RemoteRevocationStore(c)
}

// RemoteRevocationStore returns the most up to date commitment version of the
// revocation storage tree for the remote party. This method can be used when
// acting on a possible contract breach to ensure, that the caller has the most
// up to date information required to deliver justice.
func (c *ChannelStateDB) RemoteRevocationStore(channel *OpenChannel) (
	shachain.Store, error) {

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchChanBucket(
			tx, channel.IdentityPub, &channel.FundingOutpoint,
			channel.ChainHash,
		)
		if err != nil {
			return err
		}

		return fetchChanRevocationState(chanBucket, channel)
	}, func() {})
	if err != nil {
		return nil, err
	}

	return channel.RevocationStore, nil
}

// AbsoluteThawHeight determines a frozen channel's absolute thaw height. If the
// channel is not frozen, then 0 is returned.
func (c *OpenChannel) AbsoluteThawHeight() (uint32, error) {
	// Only frozen channels have a thaw height.
	if !c.ChanType.IsFrozen() && !c.ChanType.HasLeaseExpiration() {
		return 0, nil
	}

	// If the channel has the frozen bit set and it's thaw height is below
	// the absolute threshold, then it's interpreted as a relative height to
	// the chain's current height.
	if c.ChanType.IsFrozen() && c.ThawHeight < AbsoluteThawHeightThreshold {
		// We'll only known of the channel's short ID once it's
		// confirmed.
		if c.IsPending {
			return 0, errors.New("cannot use relative thaw " +
				"height for unconfirmed channel")
		}

		// For non-zero-conf channels, this is the base height to use.
		blockHeightBase := c.ShortChannelID.BlockHeight

		// If this is a zero-conf channel, the ShortChannelID will be
		// an alias.
		if c.IsZeroConf() {
			if !c.ZeroConfConfirmed() {
				return 0, errors.New("cannot use relative " +
					"height for unconfirmed zero-conf " +
					"channel")
			}

			// Use the confirmed SCID's BlockHeight.
			blockHeightBase = c.confirmedScid.BlockHeight
		}

		return blockHeightBase + c.ThawHeight, nil
	}

	return c.ThawHeight, nil
}

// DeriveHeightHint derives the block height for the channel opening.
func (c *OpenChannel) DeriveHeightHint() uint32 {
	// As a height hint, we'll try to use the opening height, but if the
	// channel isn't yet open, then we'll use the height it was broadcast
	// at. This may be an unconfirmed zero-conf channel.
	heightHint := c.ShortChanID().BlockHeight
	if heightHint == 0 {
		heightHint = c.BroadcastHeight()
	}

	// Since no zero-conf state is stored in a channel backup, the below
	// logic will not be triggered for restored, zero-conf channels. Set
	// the height hint for zero-conf channels.
	if c.IsZeroConf() {
		if c.ZeroConfConfirmed() {
			// If the zero-conf channel is confirmed, we'll use the
			// confirmed SCID's block height.
			heightHint = c.ZeroConfRealScid().BlockHeight
		} else {
			// The zero-conf channel is unconfirmed. We'll need to
			// use the FundingBroadcastHeight.
			heightHint = c.BroadcastHeight()
		}
	}

	return heightHint
}

func putChannelCloseSummary(tx kvdb.RwTx, chanID []byte,
	summary *ChannelCloseSummary, lastChanState *OpenChannel) error {

	closedChanBucket, err := tx.CreateTopLevelBucket(closedChannelBucket)
	if err != nil {
		return err
	}

	summary.RemoteCurrentRevocation = lastChanState.RemoteCurrentRevocation
	summary.RemoteNextRevocation = lastChanState.RemoteNextRevocation
	summary.LocalChanConfig = lastChanState.LocalChanCfg

	var b bytes.Buffer
	if err := serializeChannelCloseSummary(&b, summary); err != nil {
		return err
	}

	return closedChanBucket.Put(chanID, b.Bytes())
}

func serializeChannelCloseSummary(w io.Writer, cs *ChannelCloseSummary) error {
	err := WriteElements(w,
		cs.ChanPoint, cs.ShortChanID, cs.ChainHash, cs.ClosingTXID,
		cs.CloseHeight, cs.RemotePub, cs.Capacity, cs.SettledBalance,
		cs.TimeLockedBalance, cs.CloseType, cs.IsPending,
	)
	if err != nil {
		return err
	}

	// If this is a close channel summary created before the addition of
	// the new fields, then we can exit here.
	if cs.RemoteCurrentRevocation == nil {
		return WriteElements(w, false)
	}

	// If fields are present, write boolean to indicate this, and continue.
	if err := WriteElements(w, true); err != nil {
		return err
	}

	if err := WriteElements(w, cs.RemoteCurrentRevocation); err != nil {
		return err
	}

	if err := writeChanConfig(w, &cs.LocalChanConfig); err != nil {
		return err
	}

	// The RemoteNextRevocation field is optional, as it's possible for a
	// channel to be closed before we learn of the next unrevoked
	// revocation point for the remote party. Write a boolean indicating
	// whether this field is present or not.
	if err := WriteElements(w, cs.RemoteNextRevocation != nil); err != nil {
		return err
	}

	// Write the field, if present.
	if cs.RemoteNextRevocation != nil {
		if err = WriteElements(w, cs.RemoteNextRevocation); err != nil {
			return err
		}
	}

	// Write whether the channel sync message is present.
	if err := WriteElements(w, cs.LastChanSyncMsg != nil); err != nil {
		return err
	}

	// Write the channel sync message, if present.
	if cs.LastChanSyncMsg != nil {
		if err := WriteElements(w, cs.LastChanSyncMsg); err != nil {
			return err
		}
	}

	return nil
}

func deserializeCloseChannelSummary(r io.Reader) (*ChannelCloseSummary, error) {
	c := &ChannelCloseSummary{}

	err := ReadElements(r,
		&c.ChanPoint, &c.ShortChanID, &c.ChainHash, &c.ClosingTXID,
		&c.CloseHeight, &c.RemotePub, &c.Capacity, &c.SettledBalance,
		&c.TimeLockedBalance, &c.CloseType, &c.IsPending,
	)
	if err != nil {
		return nil, err
	}

	// We'll now check to see if the channel close summary was encoded with
	// any of the additional optional fields.
	var hasNewFields bool
	err = ReadElements(r, &hasNewFields)
	if err != nil {
		return nil, err
	}

	// If fields are not present, we can return.
	if !hasNewFields {
		return c, nil
	}

	// Otherwise read the new fields.
	if err := ReadElements(r, &c.RemoteCurrentRevocation); err != nil {
		return nil, err
	}

	if err := readChanConfig(r, &c.LocalChanConfig); err != nil {
		return nil, err
	}

	// Finally, we'll attempt to read the next unrevoked commitment point
	// for the remote party. If we closed the channel before receiving a
	// channel_ready message then this might not be present. A boolean
	// indicating whether the field is present will come first.
	var hasRemoteNextRevocation bool
	err = ReadElements(r, &hasRemoteNextRevocation)
	if err != nil {
		return nil, err
	}

	// If this field was written, read it.
	if hasRemoteNextRevocation {
		err = ReadElements(r, &c.RemoteNextRevocation)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have a channel sync message to read.
	var hasChanSyncMsg bool
	err = ReadElements(r, &hasChanSyncMsg)
	if err == io.EOF {
		return c, nil
	} else if err != nil {
		return nil, err
	}

	// If a chan sync message is present, read it.
	if hasChanSyncMsg {
		// We must pass in reference to a lnwire.Message for the codec
		// to support it.
		var msg lnwire.Message
		if err := ReadElements(r, &msg); err != nil {
			return nil, err
		}

		chanSync, ok := msg.(*lnwire.ChannelReestablish)
		if !ok {
			return nil, errors.New("unable cast db Message to " +
				"ChannelReestablish")
		}
		c.LastChanSyncMsg = chanSync
	}

	return c, nil
}

func writeChanConfig(b io.Writer, c *ChannelConfig) error {
	return WriteElements(b,
		c.DustLimit, c.MaxPendingAmount, c.ChanReserve, c.MinHTLC,
		c.MaxAcceptedHtlcs, c.CsvDelay, c.MultiSigKey,
		c.RevocationBasePoint, c.PaymentBasePoint, c.DelayBasePoint,
		c.HtlcBasePoint,
	)
}

// fundingTxPresent returns true if expect the funding transcation to be found
// on disk or already populated within the passed open channel struct.
func fundingTxPresent(channel *OpenChannel) bool {
	chanType := channel.ChanType

	return chanType.IsSingleFunder() && chanType.HasFundingTx() &&
		channel.IsInitiator &&
		!channel.hasChanStatus(ChanStatusRestored)
}

func putChanInfo(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	var w bytes.Buffer
	if err := WriteElements(&w,
		channel.ChanType, channel.ChainHash, channel.FundingOutpoint,
		channel.ShortChannelID, channel.IsPending, channel.IsInitiator,
		channel.ChannelStatusForStore(), channel.FundingBroadcastHeight,
		channel.NumConfsRequired, channel.ChannelFlags,
		channel.IdentityPub, channel.Capacity, channel.TotalMSatSent,
		channel.TotalMSatReceived,
	); err != nil {
		return err
	}

	// For single funder channels that we initiated, and we have the
	// funding transaction, then write the funding txn.
	if fundingTxPresent(channel) {
		if err := WriteElement(&w, channel.FundingTxn); err != nil {
			return err
		}
	}

	if err := writeChanConfig(&w, &channel.LocalChanCfg); err != nil {
		return err
	}
	if err := writeChanConfig(&w, &channel.RemoteChanCfg); err != nil {
		return err
	}

	auxData := extractOpenChannelTlvData(channel)
	if err := auxData.encode(&w); err != nil {
		return fmt.Errorf("unable to encode aux data: %w", err)
	}

	if err := chanBucket.Put(chanInfoKey, w.Bytes()); err != nil {
		return err
	}

	// Finally, add optional shutdown scripts for the local and remote peer if
	// they are present.
	if err := putOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, channel.LocalShutdownScript,
	); err != nil {
		return err
	}

	return putOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, channel.RemoteShutdownScript,
	)
}

// putOptionalUpfrontShutdownScript adds a shutdown script under the key
// provided if it has a non-zero length.
func putOptionalUpfrontShutdownScript(chanBucket kvdb.RwBucket, key []byte,
	script []byte) error {
	// If the script is empty, we do not need to add anything.
	if len(script) == 0 {
		return nil
	}

	var w bytes.Buffer
	if err := WriteElement(&w, script); err != nil {
		return err
	}

	return chanBucket.Put(key, w.Bytes())
}

// getOptionalUpfrontShutdownScript reads the shutdown script stored under the
// key provided if it is present. Upfront shutdown scripts are optional, so the
// function returns with no error if the key is not present.
func getOptionalUpfrontShutdownScript(chanBucket kvdb.RBucket, key []byte,
	script *lnwire.DeliveryAddress) error {

	// Return early if the bucket does not exit, a shutdown script was not set.
	bs := chanBucket.Get(key)
	if bs == nil {
		return nil
	}

	var tempScript []byte
	r := bytes.NewReader(bs)
	if err := ReadElement(r, &tempScript); err != nil {
		return err
	}
	*script = tempScript

	return nil
}

func serializeChanCommit(w io.Writer, c *ChannelCommitment) error {
	if err := WriteElements(w,
		c.CommitHeight, c.LocalLogIndex, c.LocalHtlcIndex,
		c.RemoteLogIndex, c.RemoteHtlcIndex, c.LocalBalance,
		c.RemoteBalance, c.CommitFee, c.FeePerKw, c.CommitTx,
		c.CommitSig,
	); err != nil {
		return err
	}

	return SerializeHtlcs(w, c.Htlcs...)
}

func putChanCommitment(chanBucket kvdb.RwBucket, c *ChannelCommitment,
	local bool) error {

	var commitKey []byte
	if local {
		commitKey = append(chanCommitmentKey, byte(0x00))
	} else {
		commitKey = append(chanCommitmentKey, byte(0x01))
	}

	var b bytes.Buffer
	if err := serializeChanCommit(&b, c); err != nil {
		return err
	}

	// Before we write to disk, we'll also write our aux data as well.
	auxData := extractCommitTlvData(c)
	if err := auxData.encode(&b); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return chanBucket.Put(commitKey, b.Bytes())
}

func putChanCommitments(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	// If this is a restored channel, then we don't have any commitments to
	// write.
	if channel.hasChanStatus(ChanStatusRestored) {
		return nil
	}

	err := putChanCommitment(
		chanBucket, &channel.LocalCommitment, true,
	)
	if err != nil {
		return err
	}

	return putChanCommitment(
		chanBucket, &channel.RemoteCommitment, false,
	)
}

func putChanRevocationState(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	var b bytes.Buffer
	err := WriteElements(
		&b, channel.RemoteCurrentRevocation, channel.RevocationProducer,
		channel.RevocationStore,
	)
	if err != nil {
		return err
	}

	// If the next revocation is present, which is only the case after the
	// ChannelReady message has been sent, then we'll write it to disk.
	if channel.RemoteNextRevocation != nil {
		err = WriteElements(&b, channel.RemoteNextRevocation)
		if err != nil {
			return err
		}
	}

	return chanBucket.Put(revocationStateKey, b.Bytes())
}

func readChanConfig(b io.Reader, c *ChannelConfig) error {
	return ReadElements(b,
		&c.DustLimit, &c.MaxPendingAmount, &c.ChanReserve,
		&c.MinHTLC, &c.MaxAcceptedHtlcs, &c.CsvDelay,
		&c.MultiSigKey, &c.RevocationBasePoint,
		&c.PaymentBasePoint, &c.DelayBasePoint,
		&c.HtlcBasePoint,
	)
}

func fetchChanInfo(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	infoBytes := chanBucket.Get(chanInfoKey)
	if infoBytes == nil {
		return ErrNoChanInfoFound
	}
	r := bytes.NewReader(infoBytes)

	var chanStatus ChannelStatus
	if err := ReadElements(r,
		&channel.ChanType, &channel.ChainHash, &channel.FundingOutpoint,
		&channel.ShortChannelID, &channel.IsPending, &channel.IsInitiator,
		&chanStatus, &channel.FundingBroadcastHeight,
		&channel.NumConfsRequired, &channel.ChannelFlags,
		&channel.IdentityPub, &channel.Capacity, &channel.TotalMSatSent,
		&channel.TotalMSatReceived,
	); err != nil {
		return err
	}
	channel.SetChannelStatusForStore(chanStatus)

	// For single funder channels that we initiated and have the funding
	// transaction to, read the funding txn.
	if fundingTxPresent(channel) {
		if err := ReadElement(r, &channel.FundingTxn); err != nil {
			return err
		}
	}

	if err := readChanConfig(r, &channel.LocalChanCfg); err != nil {
		return err
	}
	if err := readChanConfig(r, &channel.RemoteChanCfg); err != nil {
		return err
	}

	// Retrieve the boolean stored under lastWasRevokeKey.
	lastWasRevokeBytes := chanBucket.Get(lastWasRevokeKey)
	if lastWasRevokeBytes == nil {
		// If nothing has been stored under this key, we store false in the
		// OpenChannel struct.
		channel.LastWasRevoke = false
	} else {
		// Otherwise, read the value into the LastWasRevoke field.
		revokeReader := bytes.NewReader(lastWasRevokeBytes)
		err := ReadElements(revokeReader, &channel.LastWasRevoke)
		if err != nil {
			return err
		}
	}

	var auxData openChannelTlvData
	if err := auxData.decode(r); err != nil {
		return fmt.Errorf("unable to decode aux data: %w", err)
	}

	// Assign all the relevant fields from the aux data into the actual
	// open channel.
	amendOpenChannelTlvData(channel, auxData)

	channel.Packager = NewChannelPackager(channel.ShortChannelID)

	// Finally, read the optional shutdown scripts.
	if err := getOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, &channel.LocalShutdownScript,
	); err != nil {
		return err
	}

	return getOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, &channel.RemoteShutdownScript,
	)
}

func deserializeChanCommit(r io.Reader) (ChannelCommitment, error) {
	var c ChannelCommitment

	err := ReadElements(r,
		&c.CommitHeight, &c.LocalLogIndex, &c.LocalHtlcIndex, &c.RemoteLogIndex,
		&c.RemoteHtlcIndex, &c.LocalBalance, &c.RemoteBalance,
		&c.CommitFee, &c.FeePerKw, &c.CommitTx, &c.CommitSig,
	)
	if err != nil {
		return c, err
	}

	c.Htlcs, err = DeserializeHtlcs(r)
	if err != nil {
		return c, err
	}

	return c, nil
}

func fetchChanCommitment(chanBucket kvdb.RBucket,
	local bool) (ChannelCommitment, error) {

	var commitKey []byte
	if local {
		commitKey = append(chanCommitmentKey, byte(0x00))
	} else {
		commitKey = append(chanCommitmentKey, byte(0x01))
	}

	commitBytes := chanBucket.Get(commitKey)
	if commitBytes == nil {
		return ChannelCommitment{}, ErrNoCommitmentsFound
	}

	r := bytes.NewReader(commitBytes)
	chanCommit, err := deserializeChanCommit(r)
	if err != nil {
		return ChannelCommitment{}, fmt.Errorf("unable to decode "+
			"chan commit: %w", err)
	}

	// We'll also check to see if we have any aux data stored as the end of
	// the stream.
	var auxData commitTlvData
	if err := auxData.decode(r); err != nil {
		return ChannelCommitment{}, fmt.Errorf("unable to decode "+
			"chan aux data: %w", err)
	}

	amendCommitTlvData(&chanCommit, auxData)

	return chanCommit, nil
}

func fetchChanCommitments(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	var err error

	// If this is a restored channel, then we don't have any commitments to
	// read.
	if channel.hasChanStatus(ChanStatusRestored) {
		return nil
	}

	channel.LocalCommitment, err = fetchChanCommitment(chanBucket, true)
	if err != nil {
		return err
	}
	channel.RemoteCommitment, err = fetchChanCommitment(chanBucket, false)
	if err != nil {
		return err
	}

	return nil
}

func fetchChanRevocationState(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	revBytes := chanBucket.Get(revocationStateKey)
	if revBytes == nil {
		return ErrNoRevocationsFound
	}
	r := bytes.NewReader(revBytes)

	err := ReadElements(
		r, &channel.RemoteCurrentRevocation, &channel.RevocationProducer,
		&channel.RevocationStore,
	)
	if err != nil {
		return err
	}

	// If there aren't any bytes left in the buffer, then we don't yet have
	// the next remote revocation, so we can exit early here.
	if r.Len() == 0 {
		return nil
	}

	// Otherwise we'll read the next revocation for the remote party which
	// is always the last item within the buffer.
	return ReadElements(r, &channel.RemoteNextRevocation)
}

func deleteOpenChannel(chanBucket kvdb.RwBucket) error {
	if err := chanBucket.Delete(chanInfoKey); err != nil {
		return err
	}

	err := chanBucket.Delete(append(chanCommitmentKey, byte(0x00)))
	if err != nil {
		return err
	}
	err = chanBucket.Delete(append(chanCommitmentKey, byte(0x01)))
	if err != nil {
		return err
	}

	if err := chanBucket.Delete(revocationStateKey); err != nil {
		return err
	}

	if diff := chanBucket.Get(commitDiffKey); diff != nil {
		return chanBucket.Delete(commitDiffKey)
	}

	return nil
}

// makeLogKey converts a uint64 into an 8 byte array.
func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}

func fetchThawHeight(chanBucket kvdb.RBucket) (uint32, error) {
	var height uint32

	heightBytes := chanBucket.Get(frozenChanKey)
	heightReader := bytes.NewReader(heightBytes)

	if err := ReadElements(heightReader, &height); err != nil {
		return 0, err
	}

	return height, nil
}

func storeThawHeight(chanBucket kvdb.RwBucket, height uint32) error {
	var heightBuf bytes.Buffer
	if err := WriteElements(&heightBuf, height); err != nil {
		return err
	}

	return chanBucket.Put(frozenChanKey, heightBuf.Bytes())
}

func deleteThawHeight(chanBucket kvdb.RwBucket) error {
	return chanBucket.Delete(frozenChanKey)
}

// keyLocRecord is a wrapper struct around keychain.KeyLocator to implement the
// tlv.RecordProducer interface.
type keyLocRecord struct {
	keychain.KeyLocator
}

// Record creates a Record out of a KeyLocator using the passed Type and the
// EKeyLocator and DKeyLocator functions. The size will always be 8 as
// KeyFamily is uint32 and the Index is uint32.
//
// NOTE: This is part of the tlv.RecordProducer interface.
func (k *keyLocRecord) Record() tlv.Record {
	// Note that we set the type here as zero, as when used with a
	// tlv.RecordT, the type param will be used as the type.
	return tlv.MakeStaticRecord(
		0, &k.KeyLocator, 8, EKeyLocator, DKeyLocator,
	)
}

// EKeyLocator is an encoder for keychain.KeyLocator.
func EKeyLocator(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*keychain.KeyLocator); ok {
		err := tlv.EUint32T(w, uint32(v.Family), buf)
		if err != nil {
			return err
		}

		return tlv.EUint32T(w, v.Index, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "keychain.KeyLocator")
}

// DKeyLocator is a decoder for keychain.KeyLocator.
func DKeyLocator(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*keychain.KeyLocator); ok {
		var family uint32
		err := tlv.DUint32(r, &family, buf, 4)
		if err != nil {
			return err
		}
		v.Family = keychain.KeyFamily(family)

		return tlv.DUint32(r, &v.Index, buf, 4)
	}
	return tlv.NewTypeForDecodingErr(val, "keychain.KeyLocator", l, 8)
}

// ShutdownInfo contains various info about the shutdown initiation of a
// channel.
type ShutdownInfo = cstate.ShutdownInfo

// NewShutdownInfo constructs a new ShutdownInfo object.
var NewShutdownInfo = cstate.NewShutdownInfo

// encodeShutdownInfo serialises the ShutdownInfo to the given io.Writer.
func encodeShutdownInfo(s *ShutdownInfo, w io.Writer) error {
	records := []tlv.Record{
		s.DeliveryScript.Record(),
		s.LocalInitiator.Record(),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// decodeShutdownInfo constructs a ShutdownInfo struct by decoding the given
// byte slice.
func decodeShutdownInfo(b []byte) (*ShutdownInfo, error) {
	tlvStream := lnwire.ExtraOpaqueData(b)

	var info ShutdownInfo
	records := []tlv.RecordProducer{
		&info.DeliveryScript,
		&info.LocalInitiator,
	}

	_, err := tlvStream.ExtractRecords(records...)

	return &info, err
}
