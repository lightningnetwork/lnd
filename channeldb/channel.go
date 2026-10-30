package channeldb

import (
	"io"

	cstate "github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/lnwire"
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

const (
	// FinalHtlcSettledBit is the bit that encodes whether the htlc was
	// settled or failed.
	FinalHtlcSettledBit = cstate.FinalHtlcSettledBit

	// FinalHtlcOffchainBit is the bit that encodes whether the htlc was
	// resolved offchain or onchain.
	FinalHtlcOffchainBit = cstate.FinalHtlcOffchainBit
)

var (
	// DeriveMusig2Shachain derives a shachain producer for the taproot
	// channel from normal shachain revocation root.
	DeriveMusig2Shachain = cstate.DeriveMusig2Shachain

	// NewMusigVerificationNonce generates the local or verification nonce
	// for another musig2 session.
	NewMusigVerificationNonce = cstate.NewMusigVerificationNonce
)

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
func SerializeHtlcs(b io.Writer, htlcs ...cstate.HTLC) error {
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
func DeserializeHtlcs(r io.Reader) ([]cstate.HTLC, error) {
	return cstate.DeserializeHtlcs(r)
}

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

// makeLogKey converts a uint64 into an 8 byte array.
//
// TODO(chanstate): remove together with the deprecated revocation log
// bucket. chanstate owns the forwarding and revocation key helpers now,
// and fetchOldRevocationLog is the only caller left here.
func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}

// EKeyLocator is an encoder for keychain.KeyLocator.
func EKeyLocator(w io.Writer, val interface{}, buf *[8]byte) error {
	return cstate.EKeyLocator(w, val, buf)
}

// DKeyLocator is a decoder for keychain.KeyLocator.
func DKeyLocator(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	return cstate.DKeyLocator(r, val, buf, l)
}

// NewShutdownInfo constructs a new ShutdownInfo object.
func NewShutdownInfo(deliveryScript lnwire.DeliveryAddress,
	locallyInitiated bool) *cstate.ShutdownInfo {

	return cstate.NewShutdownInfo(deliveryScript, locallyInitiated)
}
