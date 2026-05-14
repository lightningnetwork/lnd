package chanstate

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// ChannelCommitment is a snapshot of the commitment state at a particular
// point in the commitment chain. With each state transition, a snapshot of the
// current state along with all non-settled HTLCs are recorded. These snapshots
// detail the state of the _remote_ party's commitment at a particular state
// number.  For ourselves (the local node) we ONLY store our most recent
// (unrevoked) state for safety purposes.
type ChannelCommitment struct {
	// CommitHeight is the update number that this ChannelDelta represents
	// the total number of commitment updates to this point. This can be
	// viewed as sort of a "commitment height" as this number is
	// monotonically increasing.
	CommitHeight uint64

	// LocalLogIndex is the cumulative log index of the local node at this
	// point in the commitment chain. This value will be incremented for
	// each _update_ added to the local update log.
	LocalLogIndex uint64

	// LocalHtlcIndex is the current local running HTLC index. This value
	// will be incremented for each outgoing HTLC the local node offers.
	LocalHtlcIndex uint64

	// RemoteLogIndex is the cumulative log index of the remote node at
	// this point in the commitment chain. This value will be incremented
	// for each _update_ added to the remote update log.
	RemoteLogIndex uint64

	// RemoteHtlcIndex is the current remote running HTLC index. This value
	// will be incremented for each outgoing HTLC the remote node offers.
	RemoteHtlcIndex uint64

	// LocalBalance is the current available settled balance within the
	// channel directly spendable by us.
	//
	// NOTE: This is the balance *after* subtracting any commitment fee,
	// AND anchor output values.
	LocalBalance lnwire.MilliSatoshi

	// RemoteBalance is the current available settled balance within the
	// channel directly spendable by the remote node.
	//
	// NOTE: This is the balance *after* subtracting any commitment fee,
	// AND anchor output values.
	RemoteBalance lnwire.MilliSatoshi

	// CommitFee is the amount calculated to be paid in fees for the
	// current set of commitment transactions. The fee amount is persisted
	// with the channel in order to allow the fee amount to be removed and
	// recalculated with each channel state update, including updates that
	// happen after a system restart.
	CommitFee btcutil.Amount

	// FeePerKw is the min satoshis/kilo-weight that should be paid within
	// the commitment transaction for the entire duration of the channel's
	// lifetime. This field may be updated during normal operation of the
	// channel as on-chain conditions change.
	//
	// TODO(halseth): make this SatPerKWeight. Cannot be done atm because
	// this will cause the import cycle lnwallet<->channeldb. Fee
	// estimation stuff should be in its own package.
	FeePerKw btcutil.Amount

	// CommitTx is the latest version of the commitment state, broadcast
	// able by us.
	CommitTx *wire.MsgTx

	// CustomBlob is an optional blob that can be used to store information
	// specific to a custom channel type. This may track some custom
	// specific state for this given commitment.
	CustomBlob fn.Option[tlv.Blob]

	// CommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above. This is the
	// signature signed by the remote party for our version of the
	// commitment transactions.
	CommitSig []byte

	// Htlcs is the set of HTLC's that are pending at this particular
	// commitment height.
	Htlcs []HTLC
}

// Copy returns a deep copy of the channel commitment.
func (c *ChannelCommitment) Copy() ChannelCommitment {
	c2 := *c
	if c.CommitTx != nil {
		c2.CommitTx = c.CommitTx.Copy()
	}
	if len(c.CommitSig) > 0 {
		c2.CommitSig = make([]byte, len(c.CommitSig))
		copy(c2.CommitSig, c.CommitSig)
	}

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		blobCopy := make([]byte, len(blob))
		copy(blobCopy, blob)
		c2.CustomBlob = fn.Some(blobCopy)
	})

	if len(c.Htlcs) > 0 {
		c2.Htlcs = make([]HTLC, len(c.Htlcs))
		for i, h := range c.Htlcs {
			c2.Htlcs[i] = h.Copy()
		}
	}

	return c2
}

// HTLC is the on-disk representation of a hash time-locked contract. HTLCs are
// contained within ChannelDeltas which encode the current state of the
// commitment between state updates.
//
// TODO(roasbeef): save space by using smaller ints at tail end?
type HTLC struct {
	// TODO(yy): can embed an HTLCEntry here.

	// Signature is the signature for the second level covenant transaction
	// for this HTLC. The second level transaction is a timeout tx in the
	// case that this is an outgoing HTLC, and a success tx in the case
	// that this is an incoming HTLC.
	//
	// TODO(roasbeef): make [64]byte instead?
	Signature []byte

	// RHash is the payment hash of the HTLC.
	RHash [32]byte

	// Amt is the amount of milli-satoshis this HTLC escrows.
	Amt lnwire.MilliSatoshi

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout uint32

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	OutputIndex int32

	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming bool

	// OnionBlob is an opaque blob which is used to complete multi-hop
	// routing.
	OnionBlob [lnwire.OnionPacketSize]byte

	// HtlcIndex is the HTLC counter index of this active, outstanding
	// HTLC. This differs from the LogIndex, as the HtlcIndex is only
	// incremented for each offered HTLC, while they LogIndex is
	// incremented for each update (includes settle+fail).
	HtlcIndex uint64

	// LogIndex is the cumulative log index of this HTLC. This differs
	// from the HtlcIndex as this will be incremented for each new log
	// update added.
	LogIndex uint64

	// ExtraData contains any additional information that was transmitted
	// with the HTLC via TLVs. This data *must* already be encoded as a
	// TLV stream, and may be empty. The length of this data is naturally
	// limited by the space available to TLVs in update_add_htlc:
	// = 65535 bytes (bolt 8 maximum message size):
	// - 2 bytes (bolt 1 message_type)
	// - 32 bytes (channel_id)
	// - 8 bytes (id)
	// - 8 bytes (amount_msat)
	// - 32 bytes (payment_hash)
	// - 4 bytes (cltv_expiry)
	// - 1366 bytes (onion_routing_packet)
	// = 64083 bytes maximum possible TLV stream
	//
	// Note that this extra data is stored inline with the OnionBlob for
	// legacy reasons, see serialization/deserialization functions for
	// detail.
	ExtraData lnwire.ExtraOpaqueData

	// BlindingPoint is an optional blinding point included with the HTLC.
	//
	// Note: this field is not a part of on-disk representation of the
	// HTLC. It is stored in the ExtraData field, which is used to store
	// a TLV stream of additional information associated with the HTLC.
	BlindingPoint lnwire.BlindingPointRecord

	// CustomRecords is a set of custom TLV records that are associated with
	// this HTLC. These records are used to store additional information
	// about the HTLC that is not part of the standard HTLC fields. This
	// field is encoded within the ExtraData field.
	CustomRecords lnwire.CustomRecords
}

// Copy returns a full copy of the target HTLC.
func (h *HTLC) Copy() HTLC {
	clone := HTLC{
		Incoming:      h.Incoming,
		Amt:           h.Amt,
		RefundTimeout: h.RefundTimeout,
		OutputIndex:   h.OutputIndex,
	}
	copy(clone.Signature, h.Signature)
	copy(clone.RHash[:], h.RHash[:])
	copy(clone.ExtraData, h.ExtraData)
	clone.BlindingPoint = h.BlindingPoint
	clone.CustomRecords = h.CustomRecords.Copy()

	return clone
}

// LogUpdate represents a pending update to the remote commitment chain. The
// log update may be an add, fail, or settle entry. We maintain this data in
// order to be able to properly retransmit our proposed state if necessary.
type LogUpdate struct {
	// LogIndex is the log index of this proposed commitment update entry.
	LogIndex uint64

	// UpdateMsg is the update message that was included within our
	// local update log. The LogIndex value denotes the log index of this
	// update which will be used when restoring our local update log if
	// we're left with a dangling update on restart.
	UpdateMsg lnwire.Message
}

// CommitDiff represents the delta needed to apply the state transition between
// two subsequent commitment states. Given state N and state N+1, one is able
// to apply the set of messages contained within the CommitDiff to N to arrive
// at state N+1. Each time a new commitment is extended, we'll write a new
// commitment (along with the full commitment state) to disk so we can
// re-transmit the state in the case of a connection loss or message drop.
type CommitDiff struct {
	// ChannelCommitment is the full commitment state that one would arrive
	// at by applying the set of messages contained in the UpdateDiff to
	// the prior accepted commitment.
	Commitment ChannelCommitment

	// LogUpdates is the set of messages sent prior to the commitment state
	// transition in question. Upon reconnection, if we detect that they
	// don't have the commitment, then we re-send this along with the
	// proper signature.
	LogUpdates []LogUpdate

	// CommitSig is the exact CommitSig message that should be sent after
	// the set of LogUpdates above has been retransmitted. The signatures
	// within this message should properly cover the new commitment state
	// and also the HTLC's within the new commitment state.
	CommitSig *lnwire.CommitSig

	// OpenedCircuitKeys is a set of unique identifiers for any downstream
	// Add packets included in this commitment txn. After a restart, this
	// set of htlcs is acked from the link's incoming mailbox to ensure
	// there isn't an attempt to re-add them to this commitment txn.
	OpenedCircuitKeys []models.CircuitKey

	// ClosedCircuitKeys records the unique identifiers for any settle/fail
	// packets that were resolved by this commitment txn. After a restart,
	// this is used to ensure those circuits are removed from the circuit
	// map, and the downstream packets in the link's mailbox are removed.
	ClosedCircuitKeys []models.CircuitKey

	// AddAcks specifies the locations (commit height, pkg index) of any
	// Adds that were failed/settled in this commit diff. This will ack
	// entries in *this* channel's forwarding packages.
	//
	// NOTE: This value is not serialized, it is used to atomically mark the
	// resolution of adds, such that they will not be reprocessed after a
	// restart.
	AddAcks []AddRef

	// SettleFailAcks specifies the locations (chan id, commit height, pkg
	// index) of any Settles or Fails that were locked into this commit
	// diff, and originate from *another* channel, i.e. the outgoing link.
	//
	// NOTE: This value is not serialized, it is used to atomically acks
	// settles and fails from the forwarding packages of other channels,
	// such that they will not be reforwarded internally after a restart.
	SettleFailAcks []SettleFailRef
}
