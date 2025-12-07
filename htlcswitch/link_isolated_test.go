package htlcswitch

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

type linkTestContext struct {
	t *testing.T

	aliceSwitch *Switch
	aliceLink   ChannelLink
	bobChannel  *lnwallet.LightningChannel
	aliceMsgs   <-chan lnwire.Message
}

// sendHtlcBobToAlice sends an HTLC from Bob to Alice, that pays to a preimage
// already in Alice's registry.
func (l *linkTestContext) sendHtlcBobToAlice(htlc *lnwire.UpdateAddHTLC) {
	l.t.Helper()

	_, err := l.bobChannel.AddHTLC(htlc, nil)
	if err != nil {
		l.t.Fatalf("bob failed adding htlc: %v", err)
	}

	l.aliceLink.HandleChannelUpdate(htlc)
}

// sendHtlcAliceToBob sends an HTLC from Alice to Bob, by first committing the
// HTLC in the circuit map, then delivering the outgoing packet to Alice's link.
// The HTLC will be sent to Bob via Alice's message stream.
func (l *linkTestContext) sendHtlcAliceToBob(htlcID int,
	htlc *lnwire.UpdateAddHTLC) {

	l.t.Helper()

	circuitMap := l.aliceSwitch.circuits
	fwdActions, err := circuitMap.CommitCircuits(
		&PaymentCircuit{
			Incoming: CircuitKey{
				HtlcID: uint64(htlcID),
			},
			PaymentHash: htlc.PaymentHash,
		},
	)
	if err != nil {
		l.t.Fatalf("unable to commit circuit: %v", err)
	}

	if len(fwdActions.Adds) != 1 {
		l.t.Fatalf("expected 1 adds, found %d", len(fwdActions.Adds))
	}

	err = l.aliceLink.handleSwitchPacket(&htlcPacket{
		incomingHTLCID: uint64(htlcID),
		htlc:           htlc,
	})
	if err != nil {
		l.t.Fatal(err)
	}
}

// receiveHtlcAliceToBob pulls the next message from Alice's message stream,
// asserts that it is an UpdateAddHTLC, then applies it to Bob's state machine.
func (l *linkTestContext) receiveHtlcAliceToBob() {
	l.t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-l.aliceMsgs:
	case <-time.After(15 * time.Second):
		l.t.Fatalf("did not received htlc from alice")
	}

	htlcAdd, ok := msg.(*lnwire.UpdateAddHTLC)
	if !ok {
		l.t.Fatalf("expected UpdateAddHTLC, got %T", msg)
	}

	_, err := l.bobChannel.ReceiveHTLC(htlcAdd)
	if err != nil {
		l.t.Fatalf("bob failed receiving htlc: %v", err)
	}
}

// sendCommitSigBobToAlice makes Bob sign a new commitment and send it to
// Alice, asserting that it signs expHtlcs number of HTLCs.
func (l *linkTestContext) sendCommitSigBobToAlice(expHtlcs int) {
	l.t.Helper()

	testQuit, testQuitFunc := context.WithCancel(l.t.Context())
	defer testQuitFunc()
	sigs, err := l.bobChannel.SignNextCommitment(testQuit)
	if err != nil {
		l.t.Fatalf("error signing commitment: %v", err)
	}

	commitSig := &lnwire.CommitSig{
		CommitSig: sigs.CommitSig,
		HtlcSigs:  sigs.HtlcSigs,
	}

	if len(commitSig.HtlcSigs) != expHtlcs {
		l.t.Fatalf("Expected %d htlc sigs, got %d", expHtlcs,
			len(commitSig.HtlcSigs))
	}

	l.aliceLink.HandleChannelUpdate(commitSig)
}

// receiveRevAndAckAliceToBob waits for Alice to send a RevAndAck to Bob, then
// hands this to Bob.
func (l *linkTestContext) receiveRevAndAckAliceToBob() {
	l.t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-l.aliceMsgs:
	case <-time.After(15 * time.Second):
		l.t.Fatalf("did not receive message")
	}

	rev, ok := msg.(*lnwire.RevokeAndAck)
	if !ok {
		l.t.Fatalf("expected RevokeAndAck, got %T", msg)
	}

	_, _, err := l.bobChannel.ReceiveRevocation(rev)
	if err != nil {
		l.t.Fatalf("bob failed receiving revocation: %v", err)
	}
}

// receiveCommitSigAliceToBob waits for Alice to send a CommitSig to Bob,
// signing expHtlcs numbers of HTLCs, then hands this to Bob.
func (l *linkTestContext) receiveCommitSigAliceToBob(expHtlcs int) {
	l.t.Helper()

	comSig := l.receiveCommitSigAlice(expHtlcs)

	err := l.bobChannel.ReceiveNewCommitment(&lnwallet.CommitSigs{
		CommitSig: comSig.CommitSig,
		HtlcSigs:  comSig.HtlcSigs,
	})
	if err != nil {
		l.t.Fatalf("bob failed receiving commitment: %v", err)
	}
}

// receiveCommitSigAlice waits for Alice to send a CommitSig, signing expHtlcs
// numbers of HTLCs.
func (l *linkTestContext) receiveCommitSigAlice(expHtlcs int) *lnwire.CommitSig {
	l.t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-l.aliceMsgs:
	case <-time.After(15 * time.Second):
		l.t.Fatalf("did not receive message")
	}

	comSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		l.t.Fatalf("expected CommitSig, got %T", msg)
	}

	if len(comSig.HtlcSigs) != expHtlcs {
		l.t.Fatalf("expected %d htlc sigs, got %d", expHtlcs,
			len(comSig.HtlcSigs))
	}

	return comSig
}

// sendRevAndAckBobToAlice make Bob revoke his current commitment, then hand
// the RevokeAndAck to Alice.
func (l *linkTestContext) sendRevAndAckBobToAlice() {
	l.t.Helper()

	rev, _, _, err := l.bobChannel.RevokeCurrentCommitment()
	if err != nil {
		l.t.Fatalf("unable to revoke commitment: %v", err)
	}

	l.aliceLink.HandleChannelUpdate(rev)
}

// receiveSettleAliceToBob waits for Alice to send a HTLC settle message to
// Bob, then hands this to Bob.
func (l *linkTestContext) receiveSettleAliceToBob() {
	l.t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-l.aliceMsgs:
	case <-time.After(15 * time.Second):
		l.t.Fatalf("did not receive message")
	}

	settleMsg, ok := msg.(*lnwire.UpdateFulfillHTLC)
	if !ok {
		l.t.Fatalf("expected UpdateFulfillHTLC, got %T", msg)
	}

	err := l.bobChannel.ReceiveHTLCSettle(settleMsg.PaymentPreimage,
		settleMsg.ID)
	if err != nil {
		l.t.Fatalf("failed settling htlc: %v", err)
	}
}

// sendSettleBobToAlice settles an HTLC on Bob's state machine, then sends an
// UpdateFulfillHTLC message to Alice's upstream inbox.
func (l *linkTestContext) sendSettleBobToAlice(htlcID uint64,
	preimage lntypes.Preimage) {

	l.t.Helper()

	err := l.bobChannel.SettleHTLC(preimage, htlcID, nil, nil, nil)
	if err != nil {
		l.t.Fatalf("alice failed settling htlc id=%d hash=%x",
			htlcID, sha256.Sum256(preimage[:]))
	}

	settle := &lnwire.UpdateFulfillHTLC{
		ID:              htlcID,
		PaymentPreimage: preimage,
	}

	l.aliceLink.HandleChannelUpdate(settle)
}

// receiveSettleAliceToBob waits for Alice to send a HTLC settle message to
// Bob, then hands this to Bob.
func (l *linkTestContext) receiveFailAliceToBob() {
	l.t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-l.aliceMsgs:
	case <-time.After(15 * time.Second):
		l.t.Fatalf("did not receive message")
	}

	failMsg, ok := msg.(*lnwire.UpdateFailHTLC)
	if !ok {
		l.t.Fatalf("expected UpdateFailHTLC, got %T", msg)
	}

	err := l.bobChannel.ReceiveFailHTLC(failMsg.ID, failMsg.Reason)
	if err != nil {
		l.t.Fatalf("unable to apply received fail htlc: %v", err)
	}
}

// assertNoMsgFromAlice asserts that Alice hasn't sent a message. Before
// calling, make sure that Alice has had the opportunity to send the message.
func (l *linkTestContext) assertNoMsgFromAlice(timeout time.Duration) {
	l.t.Helper()

	select {
	case msg := <-l.aliceMsgs:
		l.t.Fatalf("unexpected message from Alice: %v", msg)
	case <-time.After(timeout):
	}
}
