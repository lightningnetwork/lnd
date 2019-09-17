package htlcswitch

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// sendHtlcBobToAlice sends an HTLC from Bob to Alice, that pays to a preimage
// already in Alice's registry.
func sendHtlcBobToAlice(t *testing.T, aliceLink ChannelLink,
	bobChannel *lnwallet.LightningChannel, htlc *lnwire.UpdateAddHTLC) {

	t.Helper()

	_, err := bobChannel.AddHTLC(htlc, nil)
	if err != nil {
		t.Fatalf("bob failed adding htlc: %v", err)
	}

	aliceLink.HandleChannelUpdate(htlc)
}

// sendHtlcAliceToBob sends an HTLC from Alice to Bob, by first committing the
// HTLC in the circuit map, then delivering the outgoing packet to Alice's link.
// The HTLC will be sent to Bob via Alice's message stream.
func sendHtlcAliceToBob(t *testing.T, aliceLink ChannelLink, htlcID int,
	htlc *lnwire.UpdateAddHTLC) {

	t.Helper()

	circuitMap := aliceLink.(*channelLink).cfg.Switch.circuits
	fwdActions, err := circuitMap.CommitCircuits(
		&PaymentCircuit{
			Incoming: CircuitKey{
				HtlcID: uint64(htlcID),
			},
			PaymentHash: htlc.PaymentHash,
		},
	)
	if err != nil {
		t.Fatalf("unable to commit circuit: %v", err)
	}

	if len(fwdActions.Adds) != 1 {
		t.Fatalf("expected 1 adds, found %d", len(fwdActions.Adds))
	}

	aliceLink.HandleSwitchPacket(&htlcPacket{
		incomingHTLCID: uint64(htlcID),
		htlc:           htlc,
	})

}

// receiveHtlcAliceToBob pulls the next message from Alice's message stream,
// asserts that it is an UpdateAddHTLC, then applies it to Bob's state machine.
func receiveHtlcAliceToBob(t *testing.T, aliceMsgs <-chan lnwire.Message,
	bobChannel *lnwallet.LightningChannel) {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not received htlc from alice")
	}

	htlcAdd, ok := msg.(*lnwire.UpdateAddHTLC)
	if !ok {
		t.Fatalf("expected UpdateAddHTLC, got %T", msg)
	}

	_, err := bobChannel.ReceiveHTLC(htlcAdd)
	if err != nil {
		t.Fatalf("bob failed receiving htlc: %v", err)
	}
}

// sendCommitSigBobToAlice makes Bob sign a new commitment and send it to
// Alice, asserting that it signs expHtlcs number of HTLCs.
func sendCommitSigBobToAlice(t *testing.T, aliceLink ChannelLink,
	bobChannel *lnwallet.LightningChannel, expHtlcs int) {

	t.Helper()

	sig, htlcSigs, _, err := bobChannel.SignNextCommitment()
	if err != nil {
		t.Fatalf("error signing commitment: %v", err)
	}

	commitSig := &lnwire.CommitSig{
		CommitSig: sig,
		HtlcSigs:  htlcSigs,
	}

	if len(commitSig.HtlcSigs) != expHtlcs {
		t.Fatalf("Expected %d htlc sigs, got %d", expHtlcs,
			len(commitSig.HtlcSigs))
	}

	aliceLink.HandleChannelUpdate(commitSig)
}

// receiveRevAndAckAliceToBob waits for Alice to send a RevAndAck to Bob, then
// hands this to Bob.
func receiveRevAndAckAliceToBob(t *testing.T, aliceMsgs chan lnwire.Message,
	aliceLink ChannelLink,
	bobChannel *lnwallet.LightningChannel) {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive message")
	}

	rev, ok := msg.(*lnwire.RevokeAndAck)
	if !ok {
		t.Fatalf("expected RevokeAndAck, got %T", msg)
	}

	_, _, _, _, err := bobChannel.ReceiveRevocation(rev)
	if err != nil {
		t.Fatalf("bob failed receiving revocation: %v", err)
	}
}

// receiveCommitSigAliceToBob waits for Alice to send a CommitSig to Bob,
// signing expHtlcs numbers of HTLCs, then hands this to Bob.
func receiveCommitSigAliceToBob(t *testing.T, aliceMsgs chan lnwire.Message,
	aliceLink ChannelLink, bobChannel *lnwallet.LightningChannel,
	expHtlcs int) {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive message")
	}

	comSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		t.Fatalf("expected CommitSig, got %T", msg)
	}

	if len(comSig.HtlcSigs) != expHtlcs {
		t.Fatalf("expected %d htlc sigs, got %d", expHtlcs,
			len(comSig.HtlcSigs))
	}
	err := bobChannel.ReceiveNewCommitment(comSig.CommitSig,
		comSig.HtlcSigs)
	if err != nil {
		t.Fatalf("bob failed receiving commitment: %v", err)
	}
}

// sendRevAndAckBobToAlice make Bob revoke his current commitment, then hand
// the RevokeAndAck to Alice.
func sendRevAndAckBobToAlice(t *testing.T, aliceLink ChannelLink,
	bobChannel *lnwallet.LightningChannel) {

	t.Helper()

	rev, _, err := bobChannel.RevokeCurrentCommitment()
	if err != nil {
		t.Fatalf("unable to revoke commitment: %v", err)
	}

	aliceLink.HandleChannelUpdate(rev)
}

// receiveSettleAliceToBob waits for Alice to send a HTLC settle message to
// Bob, then hands this to Bob.
func receiveSettleAliceToBob(t *testing.T, aliceMsgs chan lnwire.Message,
	aliceLink ChannelLink, bobChannel *lnwallet.LightningChannel) {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive message")
	}

	settleMsg, ok := msg.(*lnwire.UpdateFulfillHTLC)
	if !ok {
		t.Fatalf("expected UpdateFulfillHTLC, got %T", msg)
	}

	err := bobChannel.ReceiveHTLCSettle(settleMsg.PaymentPreimage,
		settleMsg.ID)
	if err != nil {
		t.Fatalf("failed settling htlc: %v", err)
	}
}

// sendSettleBobToAlice settles an HTLC on Bob's state machine, then sends an
// UpdateFulfillHTLC message to Alice's upstream inbox.
func sendSettleBobToAlice(t *testing.T, aliceLink ChannelLink,
	bobChannel *lnwallet.LightningChannel, htlcID uint64,
	preimage lntypes.Preimage) {

	t.Helper()

	err := bobChannel.SettleHTLC(preimage, htlcID, nil, nil, nil)
	if err != nil {
		t.Fatalf("alice failed settling htlc id=%d hash=%x",
			htlcID, sha256.Sum256(preimage[:]))
	}

	settle := &lnwire.UpdateFulfillHTLC{
		ID:              htlcID,
		PaymentPreimage: preimage,
	}

	aliceLink.HandleChannelUpdate(settle)
}

// receiveSettleAliceToBob waits for Alice to send a HTLC settle message to
// Bob, then hands this to Bob.
func receiveFailAliceToBob(t *testing.T, aliceMsgs chan lnwire.Message,
	aliceLink ChannelLink, bobChannel *lnwallet.LightningChannel) {

	t.Helper()

	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(15 * time.Second):
		t.Fatalf("did not receive message")
	}

	failMsg, ok := msg.(*lnwire.UpdateFailHTLC)
	if !ok {
		t.Fatalf("expected UpdateFailHTLC, got %T", msg)
	}

	err := bobChannel.ReceiveFailHTLC(failMsg.ID, failMsg.Reason)
	if err != nil {
		t.Fatalf("unable to apply received fail htlc: %v", err)
	}
}
