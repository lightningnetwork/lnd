package common

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/keychain"
)

// CircuitKey is used by a channel to uniquely identify the HTLCs it receives
// from the switch, and is used to purge our in-memory state of HTLCs that have
// already been processed by a link. Two list of CircuitKeys are included in
// each CommitDiff to allow a link to determine which in-memory htlcs directed
// the opening and closing of circuits in the switch's circuit map.
type CircuitKey struct {
	// ChanID is the short chanid indicating the HTLC's origin.
	//
	// NOTE: It is fine for this value to be blank, as this indicates a
	// locally-sourced payment.
	ChanID lnwire.ShortChannelID

	// HtlcID is the unique htlc index predominately assigned by links,
	// though can also be assigned by switch in the case of locally-sourced
	// payments.
	HtlcID uint64
}

// HTLC is the on-disk representation of a hash time-locked contract. HTLCs are
// contained within ChannelDeltas which encode the current state of the
// commitment between state updates.
//
// TODO(roasbeef): save space by using smaller ints at tail end?
type HTLC struct {
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
	OnionBlob []byte

	// HtlcIndex is the HTLC counter index of this active, outstanding
	// HTLC. This differs from the LogIndex, as the HtlcIndex is only
	// incremented for each offered HTLC, while they LogIndex is
	// incremented for each update (includes settle+fail).
	HtlcIndex uint64

	// LogIndex is the cumulative log index of this HTLC. This differs
	// from the HtlcIndex as this will be incremented for each new log
	// update added.
	LogIndex uint64
}

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

	// LocalLogIndex is the cumulative log index index of the local node at
	// this point in the commitment chain. This value will be incremented
	// for each _update_ added to the local update log.
	LocalLogIndex uint64

	// LocalHtlcIndex is the current local running HTLC index. This value
	// will be incremented for each outgoing HTLC the local node offers.
	LocalHtlcIndex uint64

	// RemoteLogIndex is the cumulative log index index of the remote node
	// at this point in the commitment chain. This value will be
	// incremented for each _update_ added to the remote update log.
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

	// CommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above. This is the
	// signature signed by the remote party for our version of the
	// commitment transactions.
	CommitSig []byte

	// Htlcs is the set of HTLC's that are pending at this particular
	// commitment height.
	Htlcs []HTLC

	// TODO(roasbeef): pending commit pointer?
	//  * lets just walk through
}

// LogUpdate represents a pending update to the remote commitment chain. The
// log update may be an add, fail, or settle entry. We maintain this data in
// order to be able to properly retransmit our proposed
// state if necessary.
type LogUpdate struct {
	// LogIndex is the log index of this proposed commitment update entry.
	LogIndex uint64

	// UpdateMsg is the update message that was included within the our
	// local update log. The LogIndex value denotes the log index of this
	// update which will be used when restoring our local update log if
	// we're left with a dangling update on restart.
	UpdateMsg lnwire.Message
}

// AddRef is used to identify a particular Add in a FwdPkg. The short channel ID
// is assumed to be that of the packager.
type AddRef struct {
	// Height is the remote commitment height that locked in the Add.
	Height uint64

	// Index is the index of the Add within the fwd pkg's Adds.
	//
	// NOTE: This index is static over the lifetime of a forwarding package.
	Index uint16
}

// SettleFailRef is used to locate a Settle/Fail in another channel's FwdPkg. A
// channel does not remove its own Settle/Fail htlcs, so the source is provided
// to locate a db bucket belonging to another channel.
type SettleFailRef struct {
	// Source identifies the outgoing link that locked in the settle or
	// fail. This is then used by the *incoming* link to find the settle
	// fail in another link's forwarding packages.
	Source lnwire.ShortChannelID

	// Height is the remote commitment height that locked in this
	// Settle/Fail.
	Height uint64

	// Index is the index of the Add with the fwd pkg's SettleFails.
	//
	// NOTE: This index is static over the lifetime of a forwarding package.
	Index uint16
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
	OpenedCircuitKeys []CircuitKey

	// ClosedCircuitKeys records the unique identifiers for any settle/fail
	// packets that were resolved by this commitment txn. After a restart,
	// this is used to ensure those circuits are removed from the circuit
	// map, and the downstream packets in the link's mailbox are removed.
	ClosedCircuitKeys []CircuitKey

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

// NetworkResult is the raw result received from the network after a payment
// attempt has been made. Since the switch doesn't always have the necessary
// data to decode the raw message, we store it together with some meta data,
// and decode it when the router query for the final result.
type NetworkResult struct {
	// Msg is the received result. This should be of type UpdateFulfillHTLC
	// or UpdateFailHTLC.
	Msg lnwire.Message

	// unencrypted indicates whether the failure encoded in the message is
	// unencrypted, and hence doesn't need to be decrypted.
	Unencrypted bool

	// IsResolution indicates whether this is a resolution message, in
	// which the failure reason might not be included.
	IsResolution bool
}

// ClosureType is an enum like structure that details exactly _how_ a channel
// was closed. Three closure types are currently possible: none, cooperative,
// local force close, remote force close, and (remote) breach.
type ClosureType uint8

// ChannelConstraints represents a set of constraints meant to allow a node to
// limit their exposure, enact flow control and ensure that all HTLCs are
// economically relevant. This struct will be mirrored for both sides of the
// channel, as each side will enforce various constraints that MUST be adhered
// to for the life time of the channel. The parameters for each of these
// constraints are static for the duration of the channel, meaning the channel
// must be torn down for them to change.
type ChannelConstraints struct {
	// DustLimit is the threshold (in satoshis) below which any outputs
	// should be trimmed. When an output is trimmed, it isn't materialized
	// as an actual output, but is instead burned to miner's fees.
	DustLimit btcutil.Amount

	// ChanReserve is an absolute reservation on the channel for the
	// owner of this set of constraints. This means that the current
	// settled balance for this node CANNOT dip below the reservation
	// amount. This acts as a defense against costless attacks when
	// either side no longer has any skin in the game.
	ChanReserve btcutil.Amount

	// MaxPendingAmount is the maximum pending HTLC value that the
	// owner of these constraints can offer the remote node at a
	// particular time.
	MaxPendingAmount lnwire.MilliSatoshi

	// MinHTLC is the minimum HTLC value that the owner of these
	// constraints can offer the remote node. If any HTLCs below this
	// amount are offered, then the HTLC will be rejected. This, in
	// tandem with the dust limit allows a node to regulate the
	// smallest HTLC that it deems economically relevant.
	MinHTLC lnwire.MilliSatoshi

	// MaxAcceptedHtlcs is the maximum number of HTLCs that the owner of
	// this set of constraints can offer the remote node. This allows each
	// node to limit their over all exposure to HTLCs that may need to be
	// acted upon in the case of a unilateral channel closure or a contract
	// breach.
	MaxAcceptedHtlcs uint16

	// CsvDelay is the relative time lock delay expressed in blocks. Any
	// settled outputs that pay to the owner of this channel configuration
	// MUST ensure that the delay branch uses this value as the relative
	// time lock. Similarly, any HTLC's offered by this node should use
	// this value as well.
	CsvDelay uint16
}

// ChannelConfig is a struct that houses the various configuration opens for
// channels. Each side maintains an instance of this configuration file as it
// governs: how the funding and commitment transaction to be created, the
// nature of HTLC's allotted, the keys to be used for delivery, and relative
// time lock parameters.
type ChannelConfig struct {
	// ChannelConstraints is the set of constraints that must be upheld for
	// the duration of the channel for the owner of this channel
	// configuration. Constraints govern a number of flow control related
	// parameters, also including the smallest HTLC that will be accepted
	// by a participant.
	ChannelConstraints

	// MultiSigKey is the key to be used within the 2-of-2 output script
	// for the owner of this channel config.
	MultiSigKey keychain.KeyDescriptor

	// RevocationBasePoint is the base public key to be used when deriving
	// revocation keys for the remote node's commitment transaction. This
	// will be combined along with a per commitment secret to derive a
	// unique revocation key for each state.
	RevocationBasePoint keychain.KeyDescriptor

	// PaymentBasePoint is the base public key to be used when deriving
	// the key used within the non-delayed pay-to-self output on the
	// commitment transaction for a node. This will be combined with a
	// tweak derived from the per-commitment point to ensure unique keys
	// for each commitment transaction.
	PaymentBasePoint keychain.KeyDescriptor

	// DelayBasePoint is the base public key to be used when deriving the
	// key used within the delayed pay-to-self output on the commitment
	// transaction for a node. This will be combined with a tweak derived
	// from the per-commitment point to ensure unique keys for each
	// commitment transaction.
	DelayBasePoint keychain.KeyDescriptor

	// HtlcBasePoint is the base public key to be used when deriving the
	// local HTLC key. The derived key (combined with the tweak derived
	// from the per-commitment point) is used within the "to self" clause
	// within any HTLC output scripts.
	HtlcBasePoint keychain.KeyDescriptor
}

// ChannelCloseSummary contains the final state of a channel at the point it
// was closed. Once a channel is closed, all the information pertaining to that
// channel within the openChannelBucket is deleted, and a compact summary is
// put in place instead.
type ChannelCloseSummary struct {
	// ChanPoint is the outpoint for this channel's funding transaction,
	// and is used as a unique identifier for the channel.
	ChanPoint wire.OutPoint

	// ShortChanID encodes the exact location in the chain in which the
	// channel was initially confirmed. This includes: the block height,
	// transaction index, and the output within the target transaction.
	ShortChanID lnwire.ShortChannelID

	// ChainHash is the hash of the genesis block that this channel resides
	// within.
	ChainHash chainhash.Hash

	// ClosingTXID is the txid of the transaction which ultimately closed
	// this channel.
	ClosingTXID chainhash.Hash

	// RemotePub is the public key of the remote peer that we formerly had
	// a channel with.
	RemotePub *btcec.PublicKey

	// Capacity was the total capacity of the channel.
	Capacity btcutil.Amount

	// CloseHeight is the height at which the funding transaction was
	// spent.
	CloseHeight uint32

	// SettledBalance is our total balance settled balance at the time of
	// channel closure. This _does not_ include the sum of any outputs that
	// have been time-locked as a result of the unilateral channel closure.
	SettledBalance btcutil.Amount

	// TimeLockedBalance is the sum of all the time-locked outputs at the
	// time of channel closure. If we triggered the force closure of this
	// channel, then this value will be non-zero if our settled output is
	// above the dust limit. If we were on the receiving side of a channel
	// force closure, then this value will be non-zero if we had any
	// outstanding outgoing HTLC's at the time of channel closure.
	TimeLockedBalance btcutil.Amount

	// CloseType details exactly _how_ the channel was closed. Five closure
	// types are possible: cooperative, local force, remote force, breach
	// and funding canceled.
	CloseType ClosureType

	// IsPending indicates whether this channel is in the 'pending close'
	// state, which means the channel closing transaction has been
	// confirmed, but not yet been fully resolved. In the case of a channel
	// that has been cooperatively closed, it will go straight into the
	// fully resolved state as soon as the closing transaction has been
	// confirmed. However, for channels that have been force closed, they'll
	// stay marked as "pending" until _all_ the pending funds have been
	// swept.
	IsPending bool

	// RemoteCurrentRevocation is the current revocation for their
	// commitment transaction. However, since this is the derived public key,
	// we don't yet have the private key so we aren't yet able to verify
	// that it's actually in the hash chain.
	RemoteCurrentRevocation *btcec.PublicKey

	// RemoteNextRevocation is the revocation key to be used for the *next*
	// commitment transaction we create for the local node. Within the
	// specification, this value is referred to as the
	// per-commitment-point.
	RemoteNextRevocation *btcec.PublicKey

	// LocalChanCfg is the channel configuration for the local node.
	LocalChanConfig ChannelConfig

	// LastChanSyncMsg is the ChannelReestablish message for this channel
	// for the state at the point where it was closed.
	LastChanSyncMsg *lnwire.ChannelReestablish
}

// FwdState is an enum used to describe the lifecycle of a FwdPkg.
type FwdState byte

const (
	// FwdStateLockedIn is the starting state for all forwarding packages.
	// Packages in this state have not yet committed to the exact set of
	// Adds to forward to the switch.
	FwdStateLockedIn FwdState = iota

	// FwdStateProcessed marks the state in which all Adds have been
	// locally processed and the forwarding decision to the switch has been
	// persisted.
	FwdStateProcessed

	// FwdStateCompleted signals that all Adds have been acked, and that all
	// settles and fails have been delivered to their sources. Packages in
	// this state can be removed permanently.
	FwdStateCompleted
)

// PkgFilter is used to compactly represent a particular subset of the Adds in a
// forwarding package. Each filter is represented as a simple, statically-sized
// bitvector, where the elements are intended to be the indices of the Adds as
// they are written in the FwdPkg.
type PkgFilter struct {
	count  uint16
	filter []byte
}

// NewPkgFilter initializes an empty PkgFilter supporting `count` elements.
func NewPkgFilter(count uint16) *PkgFilter {
	// We add 7 to ensure that the integer division yields properly rounded
	// values.
	filterLen := (count + 7) / 8

	return &PkgFilter{
		count:  count,
		filter: make([]byte, filterLen),
	}
}

// Count returns the number of elements represented by this PkgFilter.
func (f *PkgFilter) Count() uint16 {
	return f.count
}

// Set marks the `i`-th element as included by this filter.
// NOTE: It is assumed that i is always less than count.
func (f *PkgFilter) Set(i uint16) {
	byt := i / 8
	bit := i % 8

	// Set the i-th bit in the filter.
	// TODO(conner): ignore if > count to prevent panic?
	f.filter[byt] |= byte(1 << (7 - bit))
}

// Contains queries the filter for membership of index `i`.
// NOTE: It is assumed that i is always less than count.
func (f *PkgFilter) Contains(i uint16) bool {
	byt := i / 8
	bit := i % 8

	// Read the i-th bit in the filter.
	// TODO(conner): ignore if > count to prevent panic?
	return f.filter[byt]&(1<<(7-bit)) != 0
}

// Equal checks two PkgFilters for equality.
func (f *PkgFilter) Equal(f2 *PkgFilter) bool {
	if f == f2 {
		return true
	}
	if f.count != f2.count {
		return false
	}

	return bytes.Equal(f.filter, f2.filter)
}

// IsFull returns true if every element in the filter has been Set, and false
// otherwise.
func (f *PkgFilter) IsFull() bool {
	// Batch validate bytes that are fully used.
	for i := uint16(0); i < f.count/8; i++ {
		if f.filter[i] != 0xFF {
			return false
		}
	}

	// If the count is not a multiple of 8, check that the filter contains
	// all remaining bits.
	rem := f.count % 8
	for idx := f.count - rem; idx < f.count; idx++ {
		if !f.Contains(idx) {
			return false
		}
	}

	return true
}

// Size returns number of bytes produced when the PkgFilter is serialized.
func (f *PkgFilter) Size() uint16 {
	// 2 bytes for uint16 `count`, then round up number of bytes required to
	// represent `count` bits.
	return 2 + (f.count+7)/8
}

// Encode writes the filter to the provided io.Writer.
func (f *PkgFilter) Encode(w io.Writer) error {
	if err := binary.Write(w, binary.BigEndian, f.count); err != nil {
		return err
	}

	_, err := w.Write(f.filter)

	return err
}

// Decode reads the filter from the provided io.Reader.
func (f *PkgFilter) Decode(r io.Reader) error {
	if err := binary.Read(r, binary.BigEndian, &f.count); err != nil {
		return err
	}

	f.filter = make([]byte, f.Size()-2)
	_, err := io.ReadFull(r, f.filter)

	return err
}

// FwdPkg records all adds, settles, and fails that were locked in as a result
// of the remote peer sending us a revocation. Each package is identified by
// the short chanid and remote commitment height corresponding to the revocation
// that locked in the HTLCs. For everything except a locally initiated payment,
// settles and fails in a forwarding package must have a corresponding Add in
// another package, and can be removed individually once the source link has
// received the fail/settle.
//
// Adds cannot be removed, as we need to present the same batch of Adds to
// properly handle replay protection. Instead, we use a PkgFilter to mark that
// we have finished processing a particular Add. A FwdPkg should only be deleted
// after the AckFilter is full and all settles and fails have been persistently
// removed.
type FwdPkg struct {
	// Source identifies the channel that wrote this forwarding package.
	Source lnwire.ShortChannelID

	// Height is the height of the remote commitment chain that locked in
	// this forwarding package.
	Height uint64

	// State signals the persistent condition of the package and directs how
	// to reprocess the package in the event of failures.
	State FwdState

	// Adds contains all add messages which need to be processed and
	// forwarded to the switch. Adds does not change over the life of a
	// forwarding package.
	Adds []LogUpdate

	// FwdFilter is a filter containing the indices of all Adds that were
	// forwarded to the switch.
	FwdFilter *PkgFilter

	// AckFilter is a filter containing the indices of all Adds for which
	// the source has received a settle or fail and is reflected in the next
	// commitment txn. A package should not be removed until IsFull()
	// returns true.
	AckFilter *PkgFilter

	// SettleFails contains all settle and fail messages that should be
	// forwarded to the switch.
	SettleFails []LogUpdate

	// SettleFailFilter is a filter containing the indices of all Settle or
	// Fails originating in this package that have been received and locked
	// into the incoming link's commitment state.
	SettleFailFilter *PkgFilter
}

// NewFwdPkg initializes a new forwarding package in FwdStateLockedIn. This
// should be used to create a package at the time we receive a revocation.
func NewFwdPkg(source lnwire.ShortChannelID, height uint64,
	addUpdates, settleFailUpdates []LogUpdate) *FwdPkg {

	nAddUpdates := uint16(len(addUpdates))
	nSettleFailUpdates := uint16(len(settleFailUpdates))

	return &FwdPkg{
		Source:           source,
		Height:           height,
		State:            FwdStateLockedIn,
		Adds:             addUpdates,
		FwdFilter:        NewPkgFilter(nAddUpdates),
		AckFilter:        NewPkgFilter(nAddUpdates),
		SettleFails:      settleFailUpdates,
		SettleFailFilter: NewPkgFilter(nSettleFailUpdates),
	}
}
