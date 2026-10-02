package chanfsm

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Event is an input to the channel state machine: a command from the local
// node, a message from the peer, or the result of an operation the machine
// asked the channel to run.
type Event interface {
	chanEvent()
}

// AddHTLC is a command to offer an HTLC to the peer. The add's ID is ignored:
// the HTLC takes the next ID of our log.
type AddHTLC struct {
	// Htlc is the HTLC to offer.
	Htlc *lnwire.UpdateAddHTLC

	// OpenKey is the incoming circuit the HTLC forwards, if any.
	OpenKey *models.CircuitKey
}

// SettleHTLC is a command to settle one of the peer's HTLCs.
type SettleHTLC struct {
	// Msg is the update_fulfill_htlc to send. Its ID names the HTLC.
	Msg *lnwire.UpdateFulfillHTLC

	// SourceRef locates the add in its forwarding package.
	SourceRef *channeldb.AddRef

	// DestRef locates the settle in the outgoing channel's forwarding
	// package, if it came from there.
	DestRef *channeldb.SettleFailRef

	// CloseKey is the circuit the settle closes, if any.
	CloseKey *models.CircuitKey
}

// FailHTLC is a command to fail one of the peer's HTLCs.
type FailHTLC struct {
	// Msg is the update_fail_htlc to send. Its ID names the HTLC.
	Msg *lnwire.UpdateFailHTLC

	// SourceRef locates the add in its forwarding package.
	SourceRef *channeldb.AddRef

	// DestRef locates the fail in the outgoing channel's forwarding
	// package, if it came from there.
	DestRef *channeldb.SettleFailRef

	// CloseKey is the circuit the fail closes, if any.
	CloseKey *models.CircuitKey
}

// MalformedFailHTLC is a command to fail one of the peer's HTLCs because its
// onion was malformed.
type MalformedFailHTLC struct {
	// Msg is the update_fail_malformed_htlc to send. Its ID names the
	// HTLC.
	Msg *lnwire.UpdateFailMalformedHTLC

	// SourceRef locates the add in its forwarding package.
	SourceRef *channeldb.AddRef
}

// UpdateFee is a command to propose a new commitment fee rate. Only the
// channel initiator may.
type UpdateFee struct {
	// FeePerKw is the new fee rate.
	FeePerKw chainfee.SatPerKWeight
}

// SignCommitment is a command to sign a new commitment for the peer if we
// owe one and the peer has revoked its previous one.
type SignCommitment struct{}

// Connect tells a channel just loaded from disk that the connection to the
// peer is up, so it sends its channel_reestablish. The actor sends it to
// itself before anything else.
type Connect struct{}

// PeerReestablish is a channel_reestablish from the peer.
type PeerReestablish struct {
	// Msg is the message.
	Msg *lnwire.ChannelReestablish
}

// PeerAdd is an update_add_htlc from the peer.
type PeerAdd struct {
	// Msg is the message.
	Msg *lnwire.UpdateAddHTLC
}

// PeerFulfill is an update_fulfill_htlc from the peer.
type PeerFulfill struct {
	// Msg is the message.
	Msg *lnwire.UpdateFulfillHTLC
}

// PeerFail is an update_fail_htlc from the peer.
type PeerFail struct {
	// Msg is the message.
	Msg *lnwire.UpdateFailHTLC
}

// PeerFailMalformed is an update_fail_malformed_htlc from the peer.
type PeerFailMalformed struct {
	// Msg is the message.
	Msg *lnwire.UpdateFailMalformedHTLC
}

// PeerUpdateFee is an update_fee from the peer.
type PeerUpdateFee struct {
	// Msg is the message.
	Msg *lnwire.UpdateFee
}

// PeerCommitSig is a commitment_signed from the peer.
type PeerCommitSig struct {
	// Msg is the message.
	Msg *lnwire.CommitSig
}

// PeerRevokeAndAck is a revoke_and_ack from the peer.
type PeerRevokeAndAck struct {
	// Msg is the message.
	Msg *lnwire.RevokeAndAck
}

// OpDone reports the outcome of the operation the machine is applying.
type OpDone struct {
	// Op is the operation, which must be the one the machine is
	// applying.
	Op Op

	// Value is the operation's result, whose type depends on the
	// operation.
	Value any

	// Err is set if the channel refused the operation.
	Err error

	// Snapshot, if set, is the channel's protocol state after the
	// operation, which the machine checks its ledger against.
	Snapshot fn.Option[*lnwallet.ProtocolSnapshot]
}

func (*AddHTLC) chanEvent()           {}
func (*SettleHTLC) chanEvent()        {}
func (*FailHTLC) chanEvent()          {}
func (*MalformedFailHTLC) chanEvent() {}
func (*UpdateFee) chanEvent()         {}
func (*SignCommitment) chanEvent()    {}
func (*Connect) chanEvent()           {}
func (*PeerReestablish) chanEvent()   {}
func (*PeerAdd) chanEvent()           {}
func (*PeerFulfill) chanEvent()       {}
func (*PeerFail) chanEvent()          {}
func (*PeerFailMalformed) chanEvent() {}
func (*PeerUpdateFee) chanEvent()     {}
func (*PeerCommitSig) chanEvent()     {}
func (*PeerRevokeAndAck) chanEvent()  {}
func (*OpDone) chanEvent()            {}

// isCommand reports whether an event is a command from the local node, whose
// caller waits for a Reply.
func isCommand(e Event) bool {
	switch e.(type) {
	case *AddHTLC, *SettleHTLC, *FailHTLC, *MalformedFailHTLC, *UpdateFee,
		*SignCommitment:

		return true

	default:
		return false
	}
}

// Outbox is a side effect a transition asks the actor to carry out.
type Outbox interface {
	chanOutbox()
}

// ApplyOp asks the actor to run an operation on the channel and feed back
// its outcome as an OpDone before handling anything else. A transition
// emits at most one, as its last outbox event.
type ApplyOp struct {
	// Op is the operation.
	Op Op
}

// SendToPeer asks the actor to send messages to the peer, in order.
type SendToPeer struct {
	// Msgs are the messages.
	Msgs []lnwire.Message
}

// ForwardPackage hands the switch the updates a revocation locked in.
type ForwardPackage struct {
	// Pkg is the forwarding package.
	Pkg *channeldb.FwdPkg
}

// HtlcSet names one of the commitments whose HTLC set the contract court
// tracks.
type HtlcSet uint8

const (
	// LocalHtlcSet is our current commitment.
	LocalHtlcSet HtlcSet = iota

	// RemoteHtlcSet is the peer's current commitment.
	RemoteHtlcSet

	// RemotePendingHtlcSet is the peer's unrevoked next commitment.
	RemotePendingHtlcSet
)

// ContractUpdate tells the contract court the HTLCs of a commitment changed.
type ContractUpdate struct {
	// Set is the commitment.
	Set HtlcSet

	// Htlcs are its HTLCs.
	Htlcs []channeldb.HTLC
}

// FinalHtlcs reports the incoming HTLCs whose resolution our new commitment
// locked in, keyed by HTLC ID, with true for a settle.
type FinalHtlcs struct {
	// Resolved maps each HTLC to whether it was settled.
	Resolved map[uint64]bool
}

// FailChannel reports that the channel failed. The actor stops accepting
// updates, and whoever drives it should tear the connection down.
type FailChannel struct {
	// Err is the reason.
	Err error
}

// Reply answers the local command being handled.
type Reply struct {
	// Value is the command's result: the HTLC ID for an AddHTLC, and
	// whether a commitment was signed for a SignCommitment.
	Value any

	// Err is set if the command was refused.
	Err error
}

func (*ApplyOp) chanOutbox()        {}
func (*SendToPeer) chanOutbox()     {}
func (*ForwardPackage) chanOutbox() {}
func (*ContractUpdate) chanOutbox() {}
func (*FinalHtlcs) chanOutbox()     {}
func (*FailChannel) chanOutbox()    {}
func (*Reply) chanOutbox()          {}
