package lnwallet

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

// updateType is the exact type of an entry within the shared HTLC log.
type updateType uint8

const (
	// Add is an update type that adds a new HTLC entry into the log.
	// Either side can add a new pending HTLC by adding a new Add entry
	// into their update log.
	Add updateType = iota

	// Fail is an update type which removes a prior HTLC entry from the
	// log. Adding a Fail entry to one's log will modify the _remote_
	// party's update log once a new commitment view has been evaluated
	// which contains the Fail entry.
	Fail

	// MalformedFail is an update type which removes a prior HTLC entry
	// from the log. Adding a MalformedFail entry to one's log will modify
	// the _remote_ party's update log once a new commitment view has been
	// evaluated which contains the MalformedFail entry. The difference
	// from Fail type lie in the different data we have to store.
	MalformedFail

	// Settle is an update type which settles a prior HTLC crediting the
	// balance of the receiving node. Adding a Settle entry to a log will
	// result in the settle entry being removed on the log as well as the
	// original add entry from the remote party's log after the next state
	// transition.
	Settle

	// FeeUpdate is an update type sent by the channel initiator that
	// updates the fee rate used when signing the commitment transaction.
	FeeUpdate
)

// String returns a human readable string that uniquely identifies the target
// update type.
func (u updateType) String() string {
	switch u {
	case Add:
		return "Add"
	case Fail:
		return "Fail"
	case MalformedFail:
		return "MalformedFail"
	case Settle:
		return "Settle"
	case FeeUpdate:
		return "FeeUpdate"
	default:
		return "<unknown type>"
	}
}

// PaymentDescriptor represents a commitment state update which either adds,
// settles, or removes an HTLC. PaymentDescriptors encapsulate all necessary
// metadata w.r.t to an HTLC, and additional data pairing a settle message to
// the original added HTLC.
//
// TODO(roasbeef): LogEntry interface??
//   - need to separate attrs for cancel/add/settle/feeupdate
type PaymentDescriptor struct {
	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash PaymentHash

	// RPreimage is the preimage that settles the HTLC pointed to within the
	// log by the ParentIndex.
	RPreimage PaymentHash

	// Timeout is the absolute timeout in blocks, after which this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in milli-satoshis.
	Amount lnwire.MilliSatoshi

	// LogIndex is the log entry number that his HTLC update has within the
	// log. Depending on if IsIncoming is true, this is either an entry the
	// remote party added, or one that we added locally.
	LogIndex uint64

	// HtlcIndex is the index within the main update log for this HTLC.
	// Entries within the log of type Add will have this field populated,
	// as other entries will point to the entry via this counter.
	//
	// NOTE: This field will only be populate if EntryType is Add.
	HtlcIndex uint64

	// ParentIndex is the HTLC index of the entry that this update settles
	// or times out.
	//
	// NOTE: This field will only be populate if EntryType is Fail or
	// Settle.
	ParentIndex uint64

	// SourceRef points to an Add update in a forwarding package owned by
	// this channel.
	//
	// NOTE: This field will only be populated if EntryType is Fail or
	// Settle.
	SourceRef *channeldb.AddRef

	// DestRef points to a Fail/Settle update in another link's forwarding
	// package.
	//
	// NOTE: This field will only be populated if EntryType is Fail or
	// Settle, and the forwarded Add successfully included in an outgoing
	// link's commitment txn.
	DestRef *channeldb.SettleFailRef

	// OpenCircuitKey references the incoming Chan/HTLC ID of an Add HTLC
	// packet delivered by the switch.
	//
	// NOTE: This field is only populated for payment descriptors in the
	// *local* update log, and if the Add packet was delivered by the
	// switch.
	OpenCircuitKey *models.CircuitKey

	// ClosedCircuitKey references the incoming Chan/HTLC ID of the Add HTLC
	// that opened the circuit.
	//
	// NOTE: This field is only populated for payment descriptors in the
	// *local* update log, and if settle/fails have a committed circuit in
	// the circuit map.
	ClosedCircuitKey *models.CircuitKey

	// localOutputIndex is the output index of this HTLc output in the
	// commitment transaction of the local node.
	//
	// NOTE: If the output is dust from the PoV of the local commitment
	// chain, then this value will be -1.
	localOutputIndex int32

	// remoteOutputIndex is the output index of this HTLC output in the
	// commitment transaction of the remote node.
	//
	// NOTE: If the output is dust from the PoV of the remote commitment
	// chain, then this value will be -1.
	remoteOutputIndex int32

	// sig is the signature for the second-level HTLC transaction that
	// spends the version of this HTLC on the commitment transaction of the
	// local node. This signature is generated by the remote node and
	// stored by the local node in the case that local node needs to
	// broadcast their commitment transaction.
	sig input.Signature

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	addCommitHeightRemote uint64
	addCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	// commitment which removed the parent pointer of this
	// PaymentDescriptor either due to a timeout or a settle. Once both
	// these heights are below the tail of both chains, the log entries can
	// safely be removed.
	removeCommitHeightRemote uint64
	removeCommitHeightLocal  uint64

	// OnionBlob is an opaque blob which is used to complete multi-hop
	// routing.
	//
	// NOTE: Populated only on add payment descriptor entry types.
	OnionBlob []byte

	// ShaOnionBlob is a sha of the onion blob.
	//
	// NOTE: Populated only in payment descriptor with MalformedFail type.
	ShaOnionBlob [sha256.Size]byte

	// FailReason stores the reason why a particular payment was canceled.
	//
	// NOTE: Populate only in fail payment descriptor entry types.
	FailReason []byte

	// FailCode stores the code why a particular payment was canceled.
	//
	// NOTE: Populated only in payment descriptor with MalformedFail type.
	FailCode lnwire.FailCode

	// [our|their|]PkScript are the raw public key scripts that encodes the
	// redemption rules for this particular HTLC. These fields will only be
	// populated iff the EntryType of this PaymentDescriptor is Add.
	// ourPkScript is the ourPkScript from the context of our local
	// commitment chain. theirPkScript is the latest pkScript from the
	// context of the remote commitment chain.
	//
	// NOTE: These values may change within the logs themselves, however,
	// they'll stay consistent within the commitment chain entries
	// themselves.
	ourPkScript        []byte
	ourWitnessScript   []byte
	theirPkScript      []byte
	theirWitnessScript []byte

	// EntryType denotes the exact type of the PaymentDescriptor. In the
	// case of a Timeout, or Settle type, then the Parent field will point
	// into the log to the HTLC being modified.
	EntryType updateType

	// isForwarded denotes if an incoming HTLC has been forwarded to any
	// possible upstream peers in the route.
	isForwarded bool

	// BlindingPoint is an optional ephemeral key used in route blinding.
	// This value is set for nodes that are relaying payments inside of a
	// blinded route (ie, not the introduction node) from update_add_htlc's
	// TLVs.
	BlindingPoint lnwire.BlindingPointRecord
}
