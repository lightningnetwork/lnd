package chanfsm

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Channel is the part of a LightningChannel the state machine drives. The
// machine decides whether and in what order these run; the channel builds,
// signs, verifies and persists the commitments.
//
// It is the channel's own commitment protocol surface, so it has one method
// per operation rather than a few small interfaces.
//
//nolint:interfacebloat
type Channel interface {
	// AddHTLC adds an HTLC to our log and returns its ID.
	AddHTLC(htlc *lnwire.UpdateAddHTLC,
		openKey *models.CircuitKey) (uint64, error)

	// ReceiveHTLC adds the peer's HTLC to its log and returns its ID.
	ReceiveHTLC(htlc *lnwire.UpdateAddHTLC) (uint64, error)

	// SettleHTLC settles one of the peer's HTLCs.
	SettleHTLC(preimage [32]byte, htlcIndex uint64,
		sourceRef *channeldb.AddRef, destRef *channeldb.SettleFailRef,
		closeKey *models.CircuitKey) error

	// ReceiveHTLCSettle records the peer settling one of our HTLCs.
	ReceiveHTLCSettle(preimage [32]byte, htlcIndex uint64) error

	// FailHTLC fails one of the peer's HTLCs.
	FailHTLC(htlcIndex uint64, reason []byte,
		sourceRef *channeldb.AddRef, destRef *channeldb.SettleFailRef,
		closeKey *models.CircuitKey) error

	// MalformedFailHTLC fails one of the peer's HTLCs as malformed.
	MalformedFailHTLC(htlcIndex uint64, failCode lnwire.FailCode,
		shaOnionBlob [sha256.Size]byte,
		sourceRef *channeldb.AddRef) error

	// ReceiveFailHTLC records the peer failing one of our HTLCs.
	ReceiveFailHTLC(htlcIndex uint64, reason []byte) error

	// UpdateFee proposes a new fee rate.
	UpdateFee(feePerKw chainfee.SatPerKWeight) error

	// ReceiveUpdateFee records the peer's new fee rate.
	ReceiveUpdateFee(feePerKw chainfee.SatPerKWeight) error

	// SignNextCommitment signs a new commitment for the peer.
	SignNextCommitment(ctx context.Context) (*lnwallet.NewCommitState,
		error)

	// ReceiveNewCommitment verifies and records a new commitment for us.
	ReceiveNewCommitment(commitSigs *lnwallet.CommitSigs) error

	// RevokeCurrentCommitment revokes our current commitment.
	RevokeCurrentCommitment() (*lnwire.RevokeAndAck, []channeldb.HTLC,
		map[uint64]bool, error)

	// ReceiveRevocation records the peer revoking its current
	// commitment.
	ReceiveRevocation(revMsg *lnwire.RevokeAndAck) (*channeldb.FwdPkg,
		[]channeldb.HTLC, error)

	// ChanSyncMsg returns our channel_reestablish.
	ChanSyncMsg() (*lnwire.ChannelReestablish, error)

	// ProcessChanSyncMsg answers the peer's channel_reestablish with the
	// messages to retransmit.
	ProcessChanSyncMsg(ctx context.Context,
		msg *lnwire.ChannelReestablish) ([]lnwire.Message,
		[]models.CircuitKey, []models.CircuitKey, error)

	// ProtocolSnapshot returns the channel's commitment protocol state.
	ProtocolSnapshot() *lnwallet.ProtocolSnapshot

	// ChannelID returns the channel's ID.
	ChannelID() lnwire.ChannelID

	// IsInitiator reports whether we opened the channel.
	IsInitiator() bool
}

// A compile-time check that LightningChannel is a Channel.
var _ Channel = (*lnwallet.LightningChannel)(nil)

// Op is an operation on the channel that a transition authorizes. Only the
// actor runs it, and it feeds the outcome back as an OpDone.
type Op interface {
	// run applies the operation to the channel.
	run(ctx context.Context, ch Channel) (any, error)

	// String names the operation.
	String() string
}

// Every operation the machine can authorize. The local command operations
// leave the channel unchanged when they fail. The others fail the channel.
type (
	// opAdd runs AddHTLC and returns the new HTLC's ID.
	opAdd struct{ ev *AddHTLC }

	// opSettle runs SettleHTLC.
	opSettle struct{ ev *SettleHTLC }

	// opFail runs FailHTLC.
	opFail struct{ ev *FailHTLC }

	// opMalformed runs MalformedFailHTLC.
	opMalformed struct{ ev *MalformedFailHTLC }

	// opFee runs UpdateFee.
	opFee struct{ ev *UpdateFee }

	// opReceiveAdd runs ReceiveHTLC and returns the HTLC's ID.
	opReceiveAdd struct{ msg *lnwire.UpdateAddHTLC }

	// opReceiveSettle runs ReceiveHTLCSettle.
	opReceiveSettle struct{ msg *lnwire.UpdateFulfillHTLC }

	// opReceiveFail runs ReceiveFailHTLC. A malformed fail is recorded
	// as a fail with the failure it stands for as its reason, as the
	// link does.
	opReceiveFail struct {
		id     uint64
		reason []byte
		mal    *lnwire.UpdateFailMalformedHTLC
	}

	// opReceiveFee runs ReceiveUpdateFee.
	opReceiveFee struct{ msg *lnwire.UpdateFee }

	// opSign runs SignNextCommitment and returns the new state.
	opSign struct{}

	// opReceiveCommit runs ReceiveNewCommitment.
	opReceiveCommit struct{ msg *lnwire.CommitSig }

	// opRevoke runs RevokeCurrentCommitment and returns a revokeResult.
	opRevoke struct{}

	// opReceiveRevocation runs ReceiveRevocation and returns a
	// revocationResult.
	opReceiveRevocation struct{ msg *lnwire.RevokeAndAck }

	// opChanSync builds our channel_reestablish, for the channel whose
	// restored ledger it carries.
	opChanSync struct{ restored RestoredLedger }

	// opProcessSync runs ProcessChanSyncMsg, which must retransmit what
	// the plan says, and returns the messages.
	opProcessSync struct {
		msg  *lnwire.ChannelReestablish
		plan SyncPlan
	}

	// opConfirmDataLoss runs ProcessChanSyncMsg on a channel_reestablish
	// that says we lost state, which only the channel can confirm, by
	// checking the secret the peer sends with it. Either way the channel
	// can't be updated again.
	opConfirmDataLoss struct{ msg *lnwire.ChannelReestablish }
)

// revokeResult is what RevokeCurrentCommitment returns.
type revokeResult struct {
	msg   *lnwire.RevokeAndAck
	htlcs []channeldb.HTLC
	final map[uint64]bool
}

// revocationResult is what ReceiveRevocation returns.
type revocationResult struct {
	pkg   *channeldb.FwdPkg
	htlcs []channeldb.HTLC
}

func (o *opAdd) run(_ context.Context, ch Channel) (any, error) {
	return ch.AddHTLC(o.ev.Htlc, o.ev.OpenKey)
}

func (o *opSettle) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.SettleHTLC(
		o.ev.Msg.PaymentPreimage, o.ev.Msg.ID, o.ev.SourceRef,
		o.ev.DestRef, o.ev.CloseKey,
	)
}

func (o *opFail) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.FailHTLC(
		o.ev.Msg.ID, o.ev.Msg.Reason, o.ev.SourceRef, o.ev.DestRef,
		o.ev.CloseKey,
	)
}

func (o *opMalformed) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.MalformedFailHTLC(
		o.ev.Msg.ID, o.ev.Msg.FailureCode, o.ev.Msg.ShaOnionBlob,
		o.ev.SourceRef,
	)
}

func (o *opFee) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.UpdateFee(o.ev.FeePerKw)
}

func (o *opReceiveAdd) run(_ context.Context, ch Channel) (any, error) {
	return ch.ReceiveHTLC(o.msg)
}

func (o *opReceiveSettle) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.ReceiveHTLCSettle(o.msg.PaymentPreimage, o.msg.ID)
}

func (o *opReceiveFail) run(_ context.Context, ch Channel) (any, error) {
	reason := o.reason
	if o.mal != nil {
		var err error
		reason, err = malformedReason(o.mal)
		if err != nil {
			return nil, err
		}
	}

	return nil, ch.ReceiveFailHTLC(o.id, reason)
}

func (o *opReceiveFee) run(_ context.Context, ch Channel) (any, error) {
	return nil, ch.ReceiveUpdateFee(chainfee.SatPerKWeight(o.msg.FeePerKw))
}

func (o *opSign) run(ctx context.Context, ch Channel) (any, error) {
	return ch.SignNextCommitment(ctx)
}

func (o *opReceiveCommit) run(_ context.Context, ch Channel) (any, error) {
	auxSigBlob, err := o.msg.CustomRecords.Serialize()
	if err != nil {
		return nil, fmt.Errorf("unable to serialize custom records: "+
			"%w", err)
	}

	return nil, ch.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig:  o.msg.CommitSig,
		HtlcSigs:   o.msg.HtlcSigs,
		PartialSig: o.msg.PartialSig,
		AuxSigBlob: auxSigBlob,
	})
}

func (o *opRevoke) run(_ context.Context, ch Channel) (any, error) {
	msg, htlcs, final, err := ch.RevokeCurrentCommitment()
	if err != nil {
		return nil, err
	}

	return &revokeResult{msg: msg, htlcs: htlcs, final: final}, nil
}

func (o *opReceiveRevocation) run(_ context.Context,
	ch Channel) (any, error) {

	pkg, htlcs, err := ch.ReceiveRevocation(o.msg)
	if err != nil {
		return nil, err
	}

	return &revocationResult{pkg: pkg, htlcs: htlcs}, nil
}

func (o *opChanSync) run(_ context.Context, ch Channel) (any, error) {
	return ch.ChanSyncMsg()
}

func (o *opProcessSync) run(ctx context.Context, ch Channel) (any, error) {
	msgs, _, _, err := ch.ProcessChanSyncMsg(ctx, o.msg)
	return msgs, err
}

func (o *opConfirmDataLoss) run(ctx context.Context,
	ch Channel) (any, error) {

	msgs, _, _, err := ch.ProcessChanSyncMsg(ctx, o.msg)
	return msgs, err
}

func (o *opChanSync) String() string        { return "ChanSyncMsg" }
func (o *opProcessSync) String() string     { return "ProcessChanSyncMsg" }
func (o *opConfirmDataLoss) String() string { return "ConfirmDataLoss" }

func (o *opAdd) String() string           { return "AddHTLC" }
func (o *opSettle) String() string        { return "SettleHTLC" }
func (o *opFail) String() string          { return "FailHTLC" }
func (o *opMalformed) String() string     { return "MalformedFailHTLC" }
func (o *opFee) String() string           { return "UpdateFee" }
func (o *opReceiveAdd) String() string    { return "ReceiveHTLC" }
func (o *opReceiveSettle) String() string { return "ReceiveHTLCSettle" }
func (o *opReceiveFail) String() string   { return "ReceiveFailHTLC" }
func (o *opReceiveFee) String() string    { return "ReceiveUpdateFee" }
func (o *opSign) String() string          { return "SignNextCommitment" }
func (o *opReceiveCommit) String() string { return "ReceiveNewCommitment" }
func (o *opRevoke) String() string {
	return "RevokeCurrentCommitment"
}
func (o *opReceiveRevocation) String() string { return "ReceiveRevocation" }

// isLocalCommand reports whether an operation carries out a local command,
// which leaves the channel unchanged if it fails.
func isLocalCommand(op Op) bool {
	switch op.(type) {
	case *opAdd, *opSettle, *opFail, *opMalformed, *opFee:
		return true

	default:
		return false
	}
}
