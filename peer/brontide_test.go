package peer

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

var (
	// p2SHAddress is a valid pay to script hash address.
	p2SHAddress = "2NBFNJTktNa7GZusGbDbGKRZTxdK9VVez3n"

	// p2wshAddress is a valid pay to witness script hash address.
	p2wshAddress = "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"
)

// TestPeerChannelClosureShutdownResponseLinkRemoved tests the shutdown
// response we get if the link for the channel can't be found in the
// switch. This test was added due to a regression.
func TestPeerChannelClosureShutdownResponseLinkRemoved(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		alicePeer = harness.peer
		bobChan   = harness.channel
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer
	// this shutdown request with a Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	require.NotEqualValues(t, shutdownMsg.Address, dummyDeliveryScript)
}

// TestPeerChannelClosureAcceptFeeResponder tests the shutdown responder's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeResponder(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		alicePeer       = harness.peer
		bobChan         = harness.channel
		mockSwitch      = harness.mockSwitch
		broadcastTxChan = harness.publishTx
		notifier        = harness.notifier
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer
	// this shutdown request with a Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	respDeliveryScript := shutdownMsg.Address
	require.NotEqualValues(t, respDeliveryScript, dummyDeliveryScript)

	// Alice will then send a ClosingSigned message, indicating her proposed
	// closing transaction fee. Alice sends the ClosingSigned message as she is
	// the initiator of the channel.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}

	respClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// We accept the fee, and send a ClosingSigned with the same fee back,
	// so she knows we agreed.
	aliceFee := respClosingSigned.FeeSatoshis
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, respDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")
	closingSigned := lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice should now see that we agreed on the fee, and should broadcast the
	// closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Need to pull the remaining message off of Alice's outgoing queue.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Alice should be waiting in a goroutine for a confirmation.
	notifier.ConfChan <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFlushDrivesNegotiation checks that a legacy cooperative
// close holds off on fee negotiation until the link reports that the channel
// has drained, and that the report is what carries the negotiation forward. The
// link notices the flush on its own goroutine, so it hands the channel to the
// channelManager rather than advancing the closer itself.
func TestPeerChannelClosureFlushDrivesNegotiation(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		alicePeer       = harness.peer
		bobChan         = harness.channel
		mockSwitch      = harness.mockSwitch
		broadcastTxChan = harness.publishTx
		notifier        = harness.notifier
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	// The link holds on to the flush hook rather than running it inline, so
	// we get to say when the channel looks drained.
	mockLink := newDeferredFlushUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We send a shutdown request to Alice, and expect her own Shutdown in
	// response.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	require.True(t, ok, "expected Shutdown message, got %T", msg)

	respDeliveryScript := shutdownMsg.Address

	// The channel hasn't drained yet, so Alice shouldn't have opened fee
	// negotiation, even though she's the one that funded the channel.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		t.Fatalf("negotiation started before the channel flushed: %T",
			outMsg.msg)

	case <-time.After(shortTimeout):
	}

	// A flush report for a channel we have no closer for should be dropped
	// on the floor rather than start anything.
	var unknownChanID lnwire.ChannelID
	select {
	case alicePeer.chanCloseFlushed <- unknownChanID:
	case <-time.After(timeout):
		t.Fatalf("channelManager not reading flush reports")
	}

	// Now we let the link report the flush, which is what should carry the
	// negotiation into its fee phase.
	select {
	case hook := <-mockLink.flushHooks:
		go hook()
	case <-time.After(timeout):
		t.Fatalf("no flush hook was registered")
	}

	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}

	respClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	require.True(t, ok, "expected ClosingSigned message, got %T", msg)

	// We accept the fee, and send a ClosingSigned with the same fee back so
	// she knows we agreed.
	aliceFee := respClosingSigned.FeeSatoshis
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, respDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")

	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewClosingSigned(chanID, aliceFee, parsedSig),
	}

	// Alice should now see that we agreed on the fee, and broadcast the
	// closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Need to pull the remaining message off of Alice's outgoing queue.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive ClosingSigned message")
	}
	_, ok = msg.(*lnwire.ClosingSigned)
	require.True(t, ok, "expected ClosingSigned message, got %T", msg)

	// Alice should be waiting in a goroutine for a confirmation.
	notifier.ConfChan <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureAcceptFeeInitiator tests the shutdown initiator's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeInitiator(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		bobChan         = harness.channel
		alicePeer       = harness.peer
		mockSwitch      = harness.mockSwitch
		broadcastTxChan = harness.publishTx
		notifier        = harness.notifier
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We make Alice send a shutdown request.
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      &chanPoint,
		Updates:        updateChan,
		TargetFeePerKw: 12500,
		Err:            errChan,
	}
	alicePeer.localCloseChanReqs <- closeCommand

	// We can now pull a Shutdown message off of Alice's outgoingQueue.
	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address
	require.NotEqualValues(t, aliceDeliveryScript, dummyDeliveryScript)

	// Bob will respond with his own Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	// Alice will reply with a ClosingSigned here.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Bob should reply with the exact same fee in his next ClosingSigned
	// message.
	bobFee := closingSignedMsg.FeeSatoshis
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		bobFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "unable to create close proposal")
	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "unable to parse signature")

	closingSigned := lnwire.NewClosingSigned(shutdownMsg.ChannelID,
		bobFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice should accept Bob's fee, broadcast the cooperative close tx, and
	// send a ClosingSigned message back to Bob.

	// Alice should now broadcast the closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	if closingSignedMsg.FeeSatoshis != bobFee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			bobFee, closingSignedMsg.FeeSatoshis)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.ConfChan <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsResponder tests the shutdown
// responder's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsResponder(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		bobChan         = harness.channel
		alicePeer       = harness.peer
		mockSwitch      = harness.mockSwitch
		broadcastTxChan = harness.publishTx
		notifier        = harness.notifier
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	// Bob sends a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer this
	// Shutdown request with a Shutdown message.
	dummyDeliveryScript := genScript(t, p2wshAddress)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address
	require.NotEqualValues(t, aliceDeliveryScript, dummyDeliveryScript)

	// As Alice is the channel initiator, she will send her ClosingSigned
	// message.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Bob doesn't agree with the fee and will send one back that's 2.5x.
	preferredRespFee := aliceClosingSigned.FeeSatoshis
	increasedFee := btcutil.Amount(float64(preferredRespFee) * 2.5)
	bobSig, _, _, err := bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")
	closingSigned := lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice will now see the new fee we propose, but with current settings it
	// won't accept it immediately as it differs too much by its ideal fee. We
	// should get a new proposal back, which should have the average fee rate
	// proposed.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// The fee sent by Alice should be less than the fee Bob just sent as Alice
	// should attempt to compromise.
	aliceFee := aliceClosingSigned.FeeSatoshis
	if aliceFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", aliceFee, increasedFee)
	}
	lastFeeResponder := aliceFee

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = btcutil.Amount(float64(preferredRespFee) * 2.1)
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")
	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Bob's latest proposal still won't be accepted and Alice should send over
	// a new ClosingSigned message. It should be the average of what Bob and
	// Alice each proposed last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}

	aliceClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// Alice should inch towards Bob's fee, in order to compromise.
	// Additionally, this fee should be less than the fee Bob sent before.
	aliceFee = aliceClosingSigned.FeeSatoshis
	if aliceFee < lastFeeResponder {
		t.Fatalf("new fee should be greater than prior: new=%v, "+
			"prior=%v", aliceFee, lastFeeResponder)
	}
	if aliceFee > increasedFee {
		t.Fatalf("new fee should be less than Bob's fee: new=%v, "+
			"prior=%v", aliceFee, increasedFee)
	}

	// Finally, Bob will accept the fee by echoing back the same fee that Alice
	// just sent over.
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")
	closingSigned = lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice will now see that Bob agreed on the fee, and broadcast the coop
	// close transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.ConfChan <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsInitiator tests the shutdown
// initiator's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsInitiator(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channels")

	var (
		alicePeer       = harness.peer
		bobChan         = harness.channel
		mockSwitch      = harness.mockSwitch
		broadcastTxChan = harness.publishTx
		notifier        = harness.notifier
	)

	chanPoint := bobChan.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	// We make the initiator send a shutdown request.
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      &chanPoint,
		Updates:        updateChan,
		TargetFeePerKw: 12500,
		Err:            errChan,
	}

	alicePeer.localCloseChanReqs <- closeCommand

	// Alice should now send a Shutdown request to Bob.
	var msg lnwire.Message
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	aliceDeliveryScript := shutdownMsg.Address

	// Bob will answer the Shutdown message with his own Shutdown.
	dummyDeliveryScript := genScript(t, p2wshAddress)
	respShutdown := lnwire.NewShutdown(chanID, dummyDeliveryScript)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: respShutdown,
	}

	// Alice should now respond with a ClosingSigned message with her ideal
	// fee rate.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	idealFeeRate := closingSignedMsg.FeeSatoshis
	lastReceivedFee := idealFeeRate

	increasedFee := btcutil.Amount(float64(idealFeeRate) * 2.1)
	lastSentFee := increasedFee

	bobSig, _, _, err := bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "unable to parse signature")

	closingSigned := lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// It still won't be accepted, and we should get a new proposal, the
	// average of what we proposed, and what they proposed last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	aliceFee := closingSignedMsg.FeeSatoshis
	if aliceFee < lastReceivedFee {
		t.Fatalf("new fee should be greater than prior: new=%v, old=%v",
			aliceFee, lastReceivedFee)
	}
	if aliceFee > lastSentFee {
		t.Fatalf("new fee should be less than our fee: new=%v, old=%v",
			aliceFee, lastSentFee)
	}

	lastReceivedFee = aliceFee

	// We'll try negotiating a 1.5x fee, which should also be rejected.
	increasedFee = btcutil.Amount(float64(idealFeeRate) * 1.5)
	lastSentFee = increasedFee

	bobSig, _, _, err = bobChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")

	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Alice won't accept Bob's new proposal, and Bob should receive a new
	// proposal which is the average of what Bob proposed and Alice proposed
	// last time.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	aliceFee = closingSignedMsg.FeeSatoshis
	if aliceFee < lastReceivedFee {
		t.Fatalf("new fee should be greater than prior: new=%v, old=%v",
			aliceFee, lastReceivedFee)
	}
	if aliceFee > lastSentFee {
		t.Fatalf("new fee should be less than Bob's fee: new=%v, old=%v",
			aliceFee, lastSentFee)
	}

	// Bob will now accept their fee by sending back a ClosingSigned message
	// with an identical fee.
	bobSig, _, _, err = bobChan.CreateCloseProposal(
		aliceFee, dummyDeliveryScript, aliceDeliveryScript,
	)
	require.NoError(t, err, "error creating close proposal")

	parsedSig, err = lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "error parsing signature")
	closingSigned = lnwire.NewClosingSigned(chanID, aliceFee, parsedSig)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Wait for closing tx to be broadcasted.
	select {
	case <-broadcastTxChan:
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}

	// Alice should respond with the ClosingSigned they both agreed upon.
	select {
	case outMsg := <-alicePeer.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
		t.Fatalf("did not receive closing signed message")
	}
	if _, ok := msg.(*lnwire.ClosingSigned); !ok {
		t.Fatalf("expected to receive closing signed message, got %T", msg)
	}

	// Alice should be waiting on a single confirmation for the coop close tx.
	notifier.ConfChan <- &chainntnfs.TxConfirmation{}
}

// TestChooseDeliveryScript tests that chooseDeliveryScript correctly errors
// when upfront and user set scripts that do not match are provided, allows
// matching values and returns appropriate values in the case where one or none
// are set.
func TestChooseDeliveryScript(t *testing.T) {
	// generate non-zero scripts for testing.
	script1 := genScript(t, p2SHAddress)
	script2 := genScript(t, p2wshAddress)

	tests := []struct {
		name           string
		userScript     lnwire.DeliveryAddress
		shutdownScript lnwire.DeliveryAddress
		expectedScript lnwire.DeliveryAddress
		newAddr        func() ([]byte, error)
		expectedError  error
	}{
		{
			name:           "Both set and equal",
			userScript:     script1,
			shutdownScript: script1,
			expectedScript: script1,
			expectedError:  nil,
		},
		{
			name:           "Both set and not equal",
			userScript:     script1,
			shutdownScript: script2,
			expectedScript: nil,
			expectedError:  chancloser.ErrUpfrontShutdownScriptMismatch,
		},
		{
			name:           "Only upfront script",
			userScript:     nil,
			shutdownScript: script1,
			expectedScript: script1,
			expectedError:  nil,
		},
		{
			name:           "Only user script",
			userScript:     script2,
			shutdownScript: nil,
			expectedScript: script2,
			expectedError:  nil,
		},
		{
			name:           "no script generate new one",
			userScript:     nil,
			shutdownScript: nil,
			expectedScript: script2,
			newAddr: func() ([]byte, error) {
				return script2, nil
			},
			expectedError: nil,
		},
	}

	for _, test := range tests {

		t.Run(test.name, func(t *testing.T) {
			script, err := chooseDeliveryScript(
				test.shutdownScript, test.userScript,
				test.newAddr,
			)
			if err != test.expectedError {
				t.Fatalf("Expected: %v, got: %v",
					test.expectedError, err)
			}

			if !bytes.Equal(script, test.expectedScript) {
				t.Fatalf("Expected: %x, got: %x",
					test.expectedScript, script)
			}
		})
	}
}

// TestCustomShutdownScript tests that the delivery script of a shutdown
// message can be set to a specified address. It checks that setting a close
// script fails for channels which have an upfront shutdown script already set.
func TestCustomShutdownScript(t *testing.T) {
	script := genScript(t, p2SHAddress)

	// setShutdown is a function which sets the upfront shutdown address for
	// the local channel.
	setShutdown := func(a, b *chanstate.OpenChannel) {
		a.LocalShutdownScript = script
		b.RemoteShutdownScript = script
	}

	tests := []struct {
		name string

		// update is a function used to set values on the channel set up for the
		// test. It is used to set values for upfront shutdown addresses.
		update func(a, b *chanstate.OpenChannel)

		// userCloseScript is the address specified by the user.
		userCloseScript lnwire.DeliveryAddress

		// expectedScript is the address we expect to be set on the shutdown
		// message.
		expectedScript lnwire.DeliveryAddress

		// expectedError is the error we expect, if any.
		expectedError error
	}{
		{
			name:            "User set script",
			update:          noUpdate,
			userCloseScript: script,
			expectedScript:  script,
		},
		{
			name:   "No user set script",
			update: noUpdate,
		},
		{
			name:           "Shutdown set, no user script",
			update:         setShutdown,
			expectedScript: script,
		},
		{
			name:            "Shutdown set, user script matches",
			update:          setShutdown,
			userCloseScript: script,
			expectedScript:  script,
		},
		{
			name:            "Shutdown set, user script different",
			update:          setShutdown,
			userCloseScript: []byte("different addr"),
			expectedError:   chancloser.ErrUpfrontShutdownScriptMismatch,
		},
	}

	for _, test := range tests {

		t.Run(test.name, func(t *testing.T) {
			// Open a channel.
			harness, err := createTestPeerWithChannel(
				t, test.update,
			)
			if err != nil {
				t.Fatalf("unable to create test channels: %v", err)
			}

			var (
				alicePeer  = harness.peer
				bobChan    = harness.channel
				mockSwitch = harness.mockSwitch
			)

			chanPoint := bobChan.ChannelPoint()
			chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
			mockLink := newMockUpdateHandler(chanID)
			mockSwitch.links = append(mockSwitch.links, mockLink)

			// Request initiator to cooperatively close the channel,
			// with a specified delivery address.
			updateChan := make(chan interface{}, 1)
			errChan := make(chan error, 1)
			closeCommand := htlcswitch.ChanClose{
				CloseType:      contractcourt.CloseRegular,
				ChanPoint:      &chanPoint,
				Updates:        updateChan,
				TargetFeePerKw: 12500,
				DeliveryScript: test.userCloseScript,
				Err:            errChan,
			}

			// Send the close command for the correct channel and check that a
			// shutdown message is sent.
			alicePeer.localCloseChanReqs <- &closeCommand

			var msg lnwire.Message
			select {
			case outMsg := <-alicePeer.outgoingQueue:
				msg = outMsg.msg
			case <-time.After(timeout):
				t.Fatalf("did not receive shutdown message")
			case err := <-errChan:
				// Fail if we do not expect an error.
				if test.expectedError != nil {
					require.ErrorIs(
						t, err, test.expectedError,
					)
				}

				// Terminate the test early if have received an error, no
				// further action is expected.
				return
			}

			// Check that we have received a shutdown message.
			shutdownMsg, ok := msg.(*lnwire.Shutdown)
			if !ok {
				t.Fatalf("expected shutdown message, got %T", msg)
			}

			// If the test has not specified an expected address, do not check
			// whether the shutdown address matches. This covers the case where
			// we expect shutdown to a random address and cannot match it.
			if len(test.expectedScript) == 0 {
				return
			}

			// Check that the Shutdown message includes the expected delivery
			// script.
			if !bytes.Equal(test.expectedScript, shutdownMsg.Address) {
				t.Fatalf("expected delivery script: %x, got: %x",
					test.expectedScript, shutdownMsg.Address)
			}
		})
	}
}

// TestStaticRemoteDowngrade tests that we downgrade our static remote feature
// bit to optional if we have legacy channels with a peer. This ensures that
// we can stay connected to peers that don't support the feature bit that we
// have channels with.
func TestStaticRemoteDowngrade(t *testing.T) {
	t.Parallel()

	var (
		// We set the same legacy feature bits for all tests, since
		// these are not relevant to our test scenario
		rawLegacy = lnwire.NewRawFeatureVector(
			lnwire.UpfrontShutdownScriptOptional,
		)
		legacy = lnwire.NewFeatureVector(rawLegacy, nil)

		legacyCombinedOptional = lnwire.NewRawFeatureVector(
			lnwire.UpfrontShutdownScriptOptional,
			lnwire.StaticRemoteKeyOptional,
		)

		rawFeatureOptional = lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyOptional,
		)

		featureOptional = lnwire.NewFeatureVector(
			rawFeatureOptional, nil,
		)

		rawFeatureRequired = lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
		)

		featureRequired = lnwire.NewFeatureVector(
			rawFeatureRequired, nil,
		)
	)

	tests := []struct {
		name         string
		legacy       bool
		features     *lnwire.FeatureVector
		expectedInit *lnwire.Init
	}{
		{
			name:     "no legacy channel, static optional",
			legacy:   false,
			features: featureOptional,
			expectedInit: &lnwire.Init{
				GlobalFeatures: rawLegacy,
				Features:       rawFeatureOptional,
			},
		},
		{
			name:     "legacy channel, static optional",
			legacy:   true,
			features: featureOptional,
			expectedInit: &lnwire.Init{
				GlobalFeatures: rawLegacy,
				Features:       rawFeatureOptional,
			},
		},
		{
			name:     "no legacy channel, static required",
			legacy:   false,
			features: featureRequired,
			expectedInit: &lnwire.Init{
				GlobalFeatures: rawLegacy,
				Features:       rawFeatureRequired,
			},
		},

		// In this case we need to flip our required bit to optional,
		// this should also propagate to the legacy set of feature bits
		// so we have proper consistency: a bit isn't set to optional
		// in one field and required in the other.
		{
			name:     "legacy channel, static required",
			legacy:   true,
			features: featureRequired,
			expectedInit: &lnwire.Init{
				GlobalFeatures: legacyCombinedOptional,
				Features:       rawFeatureOptional,
			},
		},
	}

	for _, test := range tests {

		t.Run(test.name, func(t *testing.T) {
			params := createTestPeer(t)

			var (
				p         = params.peer
				mockConn  = params.mockConn
				writePool = p.cfg.WritePool
			)
			// Set feature bits.
			p.cfg.LegacyFeatures = legacy
			p.cfg.Features = test.features

			var b bytes.Buffer
			_, err := lnwire.WriteMessage(&b, test.expectedInit, 0)
			require.NoError(t, err)

			// Send our init message, assert that we write our
			// expected message and shutdown our write pool.
			require.NoError(t, p.sendInitMsg(test.legacy))
			mockConn.assertWrite(b.Bytes())
			require.NoError(t, writePool.Stop())
		})
	}
}

// genScript creates a script paying out to the address provided, which must
// be a valid address.
func genScript(t *testing.T, addr string) lnwire.DeliveryAddress {
	// Generate an address which can be used for testing.
	deliveryAddr, err := address.DecodeAddress(
		addr, &chaincfg.TestNet3Params,
	)
	require.NoError(t, err, "invalid delivery address")

	script, err := txscript.PayToAddrScript(deliveryAddr)
	require.NoError(t, err, "cannot create script")

	return script
}

// TestPeerCustomMessage tests custom message exchange between peers.
func TestPeerCustomMessage(t *testing.T) {
	t.Parallel()

	params := createTestPeer(t)

	var (
		mockConn           = params.mockConn
		alicePeer          = params.peer
		receivedCustomChan = params.customChan
		remoteKey          = alicePeer.PubKey()
	)

	// Start peer.
	startPeerDone := startPeer(t, mockConn, alicePeer)
	_, err := fn.RecvOrTimeout(startPeerDone, 2*timeout)
	require.NoError(t, err)

	// Send a custom message.
	customMsg, err := lnwire.NewCustom(
		lnwire.MessageType(40000), []byte{1, 2, 3},
	)
	require.NoError(t, err)

	require.NoError(t, alicePeer.SendMessageLazy(false, customMsg))

	// Verify that it is passed down to the noise layer correctly.
	writtenMsg := <-mockConn.writtenMessages
	require.Equal(t, []byte{0x9c, 0x40, 0x1, 0x2, 0x3}, writtenMsg)

	// Receive a custom message.
	receivedCustomMsg, err := lnwire.NewCustom(
		lnwire.MessageType(40001), []byte{4, 5, 6},
	)
	require.NoError(t, err)

	receivedData := []byte{0x9c, 0x41, 0x4, 0x5, 0x6}
	mockConn.readMessages <- receivedData

	// Verify that it is propagated up to the custom message handler.
	receivedCustom := <-receivedCustomChan
	require.Equal(t, remoteKey, receivedCustom.peer)
	require.Equal(t, receivedCustomMsg, &receivedCustom.msg)
}

// TestPeerIgnoresPingWithoutPongReply ensures we keep the connection alive for
// pings using the BOLT 1 no-reply sentinel range.
func TestPeerIgnoresPingWithoutPongReply(t *testing.T) {
	t.Parallel()

	// Arrange: Start a peer using the mock connection so we can
	// inject incoming pings and observe any outgoing responses.
	params := createTestPeer(t)

	var (
		mockConn  = params.mockConn
		alicePeer = params.peer
	)

	startPeerDone := startPeer(t, mockConn, alicePeer)
	_, err := fn.RecvOrTimeout(startPeerDone, 2*timeout)
	require.NoError(t, err)

	// writePing serializes each boundary request and injects it through the
	// normal reader path so the assertions cover decoding and dispatch.
	writePing := func(msg *lnwire.Ping) {
		t.Helper()

		var b bytes.Buffer
		_, err := lnwire.WriteMessage(&b, msg, 0)
		require.NoError(t, err)

		select {
		case mockConn.readMessages <- b.Bytes():
		case <-time.After(timeout):
			t.Fatal("timeout sending ping to peer")
		}
	}

	// Act: Send the largest Ping BOLT 1 still requires us to answer,
	// then read its response before exercising the adjacent no-reply value.
	writePing(&lnwire.Ping{NumPongBytes: lnwire.MaxPongBytes})
	rawMsg, err := fn.RecvOrTimeout(mockConn.writtenMessages, timeout)
	require.NoError(t, err)

	msg, err := lnwire.ReadMessage(bytes.NewReader(rawMsg), 0)
	require.NoError(t, err)

	// Assert: The inclusive boundary receives exactly the requested
	// bytes, proving the implementation does not suppress one value early.
	pong, ok := msg.(*lnwire.Pong)
	require.True(t, ok)
	require.Len(t, pong.PongBytes, int(lnwire.MaxPongBytes))

	// Act: Send the first BOLT 1 no-reply value and retain a
	// payload that shows when the read loop has processed it.
	ignoredPayload := []byte{1, 2, 3}
	writePing(&lnwire.Ping{
		NumPongBytes: lnwire.MaxPongBytes + 1,
		PaddingBytes: ignoredPayload,
	})

	// Assert: The peer records the latest ping payload for observability.
	require.Eventually(t, func() bool {
		return bytes.Equal(
			alicePeer.LastRemotePingPayload(), ignoredPayload,
		)
	}, timeout, 10*time.Millisecond)

	// Assert: No pong is sent for the no-reply sentinel range.
	select {
	case rawMsg := <-mockConn.writtenMessages:
		t.Fatalf("expected no pong reply, got %x", rawMsg)
	case <-time.After(100 * time.Millisecond):
	}

	// Act: Send a normal ping afterward to prove the peer
	// stayed connected and still handles standard ping/pong
	// traffic.
	writePing(&lnwire.Ping{NumPongBytes: 1})

	rawMsg, err = fn.RecvOrTimeout(mockConn.writtenMessages, timeout)
	require.NoError(t, err)

	msg, err = lnwire.ReadMessage(bytes.NewReader(rawMsg), 0)
	require.NoError(t, err)

	// Assert: The follow-up ping receives the requested pong reply.
	pong, ok = msg.(*lnwire.Pong)
	require.True(t, ok)
	require.Len(t, pong.PongBytes, 1)
}

// TestPeerPingLimitsProductionBoundaries verifies the exact burst and refill
// thresholds used by both production Ping policies.
func TestPeerPingLimitsProductionBoundaries(t *testing.T) {
	t.Parallel()

	// Arrange: Use fresh production limiters and expected values
	// so each subtest starts with a full, independent token bucket.
	limits := defaultPingLimits()
	tests := []struct {
		name    string
		limiter *rate.Limiter
		limit   rate.Limit
		burst   int
	}{
		{
			name:    "Pong replies",
			limiter: limits.pongLimiter,
			limit:   pongReplyRate,
			burst:   pongReplyBurst,
		},
		{
			name:    "Ping floods",
			limiter: limits.pingLimiter,
			limit:   pingFloodRate,
			burst:   pingFloodBurst,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: Derive the one-token interval from the rate
			// constant under test, then fix a synthetic timestamp.
			// This makes both sides of the boundary deterministic.
			// Two nanoseconds keep the pre-boundary deficit above
			// rate's duration-truncation quantum.
			now := time.Now()
			refillTime := time.Duration(
				float64(time.Second) / float64(test.limit),
			)
			const boundaryEpsilon = 2 * time.Nanosecond
			require.Equal(t, test.limit, test.limiter.Limit())
			require.Equal(t, test.burst, test.limiter.Burst())

			// Act: Consume the burst, probe one token past it, and
			// test just before and at the derived replacement time.
			atBoundary := test.limiter.AllowN(now, test.burst)
			pastBoundary := test.limiter.AllowN(now, 1)
			beforeRefill := test.limiter.AllowN(
				now.Add(refillTime-boundaryEpsilon), 1,
			)
			atRefill := test.limiter.AllowN(
				now.Add(refillTime), 1,
			)

			// Assert: The burst boundary is inclusive, both probes
			// before refill are rejected, and the derived boundary
			// restores exactly one token without scheduler timing.
			require.True(t, atBoundary)
			require.False(t, pastBoundary)
			require.False(t, beforeRefill)
			require.True(t, atRefill)
		})
	}
}

// TestPeerPingLimitsAllowHonestCadence verifies that both inbound Ping
// limiters admit realistic keepalive cadences for long-lived connections.
func TestPeerPingLimitsAllowHonestCadence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cadence time.Duration
	}{
		{name: "lnd cadence", cadence: time.Minute},
		{name: "aggressive cadence", cadence: 10 * time.Second},
		{name: "five second cadence", cadence: 5 * time.Second},
		{name: "pathological cadence", cadence: 2 * time.Second},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: Construct the production Ping policy
			// separately so token history cannot cross test cases.
			limits := defaultPingLimits()
			start := time.Now()

			// Act: Advance a synthetic clock at the selected
			// cadence, avoiding scheduler and wall-clock noise.
			for i := 0; i < 5000; i++ {
				elapsed := time.Duration(i) * test.cadence
				now := start.Add(elapsed)

				// Assert: Both budgets admit each ping, so this
				// cadence reaches neither protection tier.
				require.True(
					t, limits.pongLimiter.AllowN(now, 1),
				)
				require.True(
					t, limits.pingLimiter.AllowN(now, 1),
				)
			}
		})
	}
}

// TestPeerValidPingsReceivePongs verifies that every Ping admitted by the
// flood limiter receives the Pong response mandated by BOLT 1.
func TestPeerValidPingsReceivePongs(t *testing.T) {
	t.Parallel()

	// Arrange: Start a peer with the production flood policy and prepare to
	// retain one response for every token in its initial burst. Exercising
	// the full burst crosses the former reply-only limit while remaining
	// within the request limit that keeps the connection active.
	params := createTestPeer(t)
	peer := params.peer
	startDone := startPeer(t, params.mockConn, peer)
	_, err := fn.RecvOrTimeout(startDone, 2*timeout)
	require.NoError(t, err)
	responses := make([][]byte, 0, pingFloodBurst)

	// Act: Deliver exactly the admitted burst of valid Pings through the
	// normal read path. Drain one wire response after each request to avoid
	// mock backpressure while observing the protocol behavior.
	for i := 0; i < pingFloodBurst; i++ {
		var b bytes.Buffer
		ping := lnwire.NewPing(1)
		ping.PaddingBytes = []byte{byte(i)}
		_, err := lnwire.WriteMessage(&b, ping, 0)
		require.NoError(t, err)

		select {
		case params.mockConn.readMessages <- b.Bytes():
		case <-peer.cg.Done():
			t.Fatal("peer disconnected before Ping was delivered")
		}

		response, err := fn.RecvOrTimeout(
			params.mockConn.writtenMessages, timeout,
		)
		require.NoError(t, err)
		responses = append(responses, response)
	}

	// Assert: Every admitted request produced a one-byte Pong and the peer
	// remained connected at the flood boundary. The response count and wire
	// decoding prevent silent suppression from regressing.
	require.Len(t, responses, pingFloodBurst)
	for _, response := range responses {
		msg, err := lnwire.ReadMessage(bytes.NewReader(response), 0)
		require.NoError(t, err)

		pong, ok := msg.(*lnwire.Pong)
		require.True(t, ok)
		require.Len(t, pong.PongBytes, 1)
	}
	require.Zero(t, atomic.LoadInt32(&peer.disconnect))
}

// TestPeerPongReplyUsesPriorityQueue verifies that the read path classifies a
// generated Pong as high priority before the generic queue handler sees it.
func TestPeerPongReplyUsesPriorityQueue(t *testing.T) {
	t.Parallel()

	// Arrange: Isolate readHandler with a buffered outgoing boundary. This
	// preserves the response before queueHandler can consume it.
	// Empty remote features avoid unrelated gossip initialization. The mock
	// router rejects one message, letting the normal Ping switch handle it
	// without starting the generic router's independent event loop.
	params := createTestPeer(t)
	peer := params.peer
	peer.remoteFeatures = lnwire.EmptyFeatureVector()
	peer.outgoingQueue = make(chan outgoingMsg, 1)
	router := &mockMsgRouter{}
	router.On("RouteMsg", mock.Anything).Return(
		msgmux.ErrUnableToRouteMsg,
	).Once()
	peer.msgRouter = fn.Some[msgmux.Router](router)
	peer.globalMsgRouter = true

	const requestedPongBytes = 3
	var pingBytes bytes.Buffer
	_, err := lnwire.WriteMessage(
		&pingBytes, lnwire.NewPing(requestedPongBytes), 0,
	)
	require.NoError(t, err)

	peer.cg.WgAdd(1)
	go peer.readHandler()

	// Act: Deliver the valid Ping through wire decoding, then capture the
	// envelope created by queueMsg. Closing the mock input after capture
	// gives the focused reader a deterministic shutdown path.
	select {
	case params.mockConn.readMessages <- pingBytes.Bytes():
	case <-peer.cg.Done():
		t.Fatal("peer disconnected before Ping was delivered")
	}

	queuedMsg, err := fn.RecvOrTimeout(peer.outgoingQueue, timeout)
	require.NoError(t, err)
	close(params.mockConn.readMessages)
	peer.cg.WgWait()

	// Assert: Verify the requested Pong and queueMsg's priority marker. The
	// marker proves the response cannot enter the lazy-message class.
	require.True(t, queuedMsg.priority)
	pong, ok := queuedMsg.msg.(*lnwire.Pong)
	require.True(t, ok)
	require.Len(t, pong.PongBytes, requestedPongBytes)
	router.AssertExpectations(t)
}

// mockMsgRouter records message-router calls while letting a test choose
// whether a message would be consumed. Embedding mock.Mock keeps every
// interface interaction explicit and independently assertable.
type mockMsgRouter struct {
	mock.Mock
}

// RegisterEndpoint returns the result configured for one endpoint so tests
// can exercise router registration without adding a second fake.
func (m *mockMsgRouter) RegisterEndpoint(endpoint msgmux.Endpoint) error {
	args := m.Called(endpoint)

	return args.Error(0)
}

// UnregisterEndpoint returns the configured removal result for the supplied
// endpoint name.
func (m *mockMsgRouter) UnregisterEndpoint(name msgmux.EndpointName) error {
	args := m.Called(name)

	return args.Error(0)
}

// RouteMsg returns the configured routing result while recording the complete
// peer message that reached the generic routing boundary.
func (m *mockMsgRouter) RouteMsg(msg msgmux.PeerMsg) error {
	args := m.Called(msg)

	return args.Error(0)
}

// Start records the lifecycle context so any test that starts the mock router
// must declare that interaction explicitly.
func (m *mockMsgRouter) Start(ctx context.Context) {
	m.Called(ctx)
}

// Stop records shutdown so tests cannot accidentally rely on an unobserved
// router lifecycle transition.
func (m *mockMsgRouter) Stop() {
	m.Called()
}

// Compile-time verification keeps the focused mock synchronized with the
// production router interface used by Brontide.
var _ msgmux.Router = (*mockMsgRouter)(nil)

// TestPeerPingFloodDisconnects verifies flood accounting precedes a generic
// router that would consume an oversized Ping.
func TestPeerPingFloodDisconnects(t *testing.T) {
	t.Parallel()

	// Arrange: Empty the flood budget and retain errors through an active
	// channel. Install a mock router prepared to consume any message;
	// marking it global avoids unrelated lifecycle calls.
	params := createTestPeer(t)
	peer := params.peer
	peer.pingLimits.pingLimiter = rate.NewLimiter(0, 0)
	peer.remoteFeatures = lnwire.EmptyFeatureVector()
	peer.activeChannels.Store(
		lnwire.ChannelID{1}, &lnwallet.LightningChannel{},
	)

	router := &mockMsgRouter{}
	router.On("RouteMsg", mock.Anything).Return(nil).Maybe()
	peer.msgRouter = fn.Some[msgmux.Router](router)
	peer.globalMsgRouter = true

	// Arrange: Encode the first oversized Pong request and register the
	// focused reader with the control group so shutdown remains joinable.
	var b bytes.Buffer
	_, err := lnwire.WriteMessage(&b, &lnwire.Ping{
		NumPongBytes: lnwire.MaxPongBytes + 1,
	}, 0)
	require.NoError(t, err)

	peer.cg.WgAdd(1)
	go peer.readHandler()

	// Act: Send the oversized Ping through normal decoding, then wait for
	// the empty flood budget to cancel and fully stop the focused reader.
	select {
	case params.mockConn.readMessages <- b.Bytes():
	case <-peer.cg.Done():
		t.Fatal("peer disconnected before Ping was delivered")
	}

	_, err = fn.RecvOrTimeout(peer.cg.Done(), timeout)
	require.NoError(t, err)
	peer.cg.WgWait()

	// Assert: Teardown precedes generic routing, and the retained error
	// matches the stable sentinel without depending on its display text.
	require.EqualValues(t, 1, atomic.LoadInt32(&peer.disconnect))
	router.AssertNotCalled(t, "RouteMsg", mock.Anything)

	storedErrors := peer.ErrorBuffer().List()
	require.NotEmpty(t, storedErrors)
	storedErr, ok := storedErrors[0].(*TimestampedError)
	require.True(t, ok)
	require.ErrorIs(t, storedErr.Error, errPingFlood)
}

// startTestQueueHandler isolates queue ownership from unrelated peer loops so
// focused tests can drive outgoingQueue directly; callers own cancellation
// and join the registered goroutine before returning.
func startTestQueueHandler(peer *Brontide) {
	peer.cg.WgAdd(1)
	go peer.queueHandler()
}

// mockQueueWriteConn adds testify-controlled write behavior to the shared
// connection fixture while inheriting its safe address and close methods.
type mockQueueWriteConn struct {
	mock.Mock
	*mockMessageConn
}

// Compile-time conformance keeps the focused mock aligned with MessageConn.
var _ MessageConn = (*mockQueueWriteConn)(nil)

// SetWriteDeadline records each pre-flush deadline for exact call assertions.
func (m *mockQueueWriteConn) SetWriteDeadline(deadline time.Time) error {
	return m.Called(deadline).Error(0)
}

// WriteMessage lets each test control when a serialized message is accepted.
func (m *mockQueueWriteConn) WriteMessage(msg []byte) error {
	return m.Called(msg).Error(0)
}

// Flush records the final wire flush and returns its configured outcome.
func (m *mockQueueWriteConn) Flush() (int, error) {
	args := m.Called()
	return args.Int(0), args.Error(1)
}

// TestMsgQueueRejectsOverflowWithoutRetention verifies that prospective limit
// checks leave both priority lists and their accounting at the accepted cap.
func TestMsgQueueRejectsOverflowWithoutRetention(t *testing.T) {
	// Arrange: Fill a one-message, one-byte queue with a priority item so a
	// lazy item would cross both limits and expose either insertion path.
	queue := newMsgQueue(queueLimits{maxMsgs: 1, maxBytes: 1})
	require.True(t, queue.push(outgoingMsg{
		priority: true, queueCost: 1,
	}))

	// Act: Attempt to append one excess lazy item through the normal push.
	accepted := queue.push(outgoingMsg{queueCost: 1})

	// Assert: Rejection preserves the exact accepted totals and leaves the
	// lazy list empty, proving the excess object is no longer retained.
	require.False(t, accepted)
	require.Equal(t, 1, queue.numMsgs)
	require.Equal(t, 1, queue.numBytes)
	require.Equal(t, 1, queue.priorityMsgs.Len())
	require.Zero(t, queue.lazyMsgs.Len())
}

// TestMsgQueueOrdersAndReleasesCapacity verifies strict priority selection,
// FIFO ordering within each class, and exact accounting release on removal.
func TestMsgQueueOrdersAndReleasesCapacity(t *testing.T) {
	// Arrange: Interleave lazy and priority messages with distinct costs.
	// The queue starts below both limits so every item is admitted and the
	// expected removal totals can prove which element left at each step.
	queue := newMsgQueue(queueLimits{maxMsgs: 4, maxBytes: 40})
	lazyFirst := lnwire.NewPing(1)
	priorityFirst := lnwire.NewPing(2)
	lazySecond := lnwire.NewPing(3)
	prioritySecond := lnwire.NewPing(4)

	require.True(t, queue.push(outgoingMsg{
		msg: lazyFirst, queueCost: 11,
	}))
	require.True(t, queue.push(outgoingMsg{
		priority: true, msg: priorityFirst, queueCost: 7,
	}))
	require.True(t, queue.push(outgoingMsg{
		msg: lazySecond, queueCost: 13,
	}))
	require.True(t, queue.push(outgoingMsg{
		priority: true, msg: prioritySecond, queueCost: 5,
	}))

	// Act: Repeatedly select and remove the front item, recording both the
	// chosen message and the shadow totals after each exact cost is
	// released.
	var (
		orderedMsgs    []lnwire.Message
		remainingMsgs  []int
		remainingBytes []int
	)
	for {
		elem, msg := queue.front()
		if elem == nil {
			break
		}

		orderedMsgs = append(orderedMsgs, msg.msg)
		queue.pop(elem)
		remainingMsgs = append(remainingMsgs, queue.numMsgs)
		remainingBytes = append(remainingBytes, queue.numBytes)
	}

	acceptedAfterDrain := queue.push(outgoingMsg{queueCost: 40})

	// Assert: Priority items lead in FIFO order, lazy items follow in FIFO
	// order, each pop releases its own count and bytes, and the empty queue
	// can admit an item that consumes the complete byte budget again.
	require.Equal(t, []lnwire.Message{
		priorityFirst, prioritySecond, lazyFirst, lazySecond,
	}, orderedMsgs)
	require.Equal(t, []int{3, 2, 1, 0}, remainingMsgs)
	require.Equal(t, []int{29, 24, 13, 0}, remainingBytes)
	require.True(t, acceptedAfterDrain)
	require.Equal(t, 1, queue.numMsgs)
	require.Equal(t, 40, queue.numBytes)
}

// TestPeerSendMessageQueueBounds verifies that the public sending API accepts
// an exact queue boundary and returns a stable error for its first excess.
func TestPeerSendMessageQueueBounds(t *testing.T) {
	t.Parallel()

	// Arrange: Derive exact data-only boundaries from the production
	// limits.
	// The onion size makes each charged message 64 KiB, so 256 distinct
	// blobs fill the 16 MiB byte budget without approaching the count cap.
	limits := defaultQueueLimits()
	onionBlobSize := (1 << 16) - limits.msgOverhead
	require.Zero(t, limits.maxBytes%(limits.msgOverhead+onionBlobSize))
	tests := []struct {
		name        string
		msgType     lnwire.MessageType
		payloadSize int
		numAtLimit  int
	}{
		{
			name:       "message count",
			msgType:    lnwire.MsgPong,
			numAtLimit: limits.maxMsgs,
		},
		{
			name:        "message bytes",
			msgType:     lnwire.MsgOnionMessage,
			payloadSize: onionBlobSize,
			numAtLimit: limits.maxBytes /
				(limits.msgOverhead + onionBlobSize),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: Start only the queue handler so no writer
			// drains it. The row selects a builder that allocates a
			// fresh peer-controlled blob for every message.
			params := createTestPeer(t)
			peer := params.peer
			startTestQueueHandler(peer)
			t.Cleanup(func() {
				peer.cg.Quit()
				peer.cg.WgWait()
			})

			// newMsg builds fresh values while keeping rows
			// declarative, including distinct blobs for every send.
			newMsg := func() lnwire.Message {
				switch test.msgType {
				case lnwire.MsgPong:
					return lnwire.NewPong(nil)

				case lnwire.MsgOnionMessage:
					blob := make([]byte, test.payloadSize)
					return &lnwire.OnionMessage{
						OnionBlob: blob,
					}

				default:
					t.Fatalf(
						"unsupported queue message: %v",
						test.msgType,
					)
				}

				return nil
			}

			// Act: Queue the exact boundary through SendMessage.
			// Its async path waits for queueHandler to
			// receive its message without requiring a writer.
			for i := 0; i < test.numAtLimit; i++ {
				err := peer.SendMessage(false, newMsg())
				require.NoError(t, err)
			}

			// Assert: The inclusive boundary remains connected. It
			// proves the cap describes accepted traffic rather than
			// the message that merely reaches it.
			select {
			case <-peer.cg.Done():
				t.Fatal("peer disconnected at the inclusive " +
					"queue limit")
			default:
			}

			// Act: Send one additional message synchronously so its
			// rejected caller receives the precise overflow cause.
			err := peer.SendMessage(true, newMsg())

			// Assert: The first excess send returns the wrapped
			// sentinel. Cancellation and joining prove teardown.
			require.ErrorIs(t, err, errQueueOverflow)
			_, err = fn.RecvOrTimeout(peer.cg.Done(), timeout)
			require.NoError(t, err)
			peer.cg.WgWait()
		})
	}
}

// TestPeerSendMessageMixedWriteAndOverflow verifies that a live writer can
// complete earlier public sends before a later message crosses the queue cap.
func TestPeerSendMessageMixedWriteAndOverflow(t *testing.T) {
	// Arrange: Allow one retained message, make the third wire write block,
	// and run both queue stages. First two writes must fully acknowledge
	// before the blocked write lets one message fill the local queue.
	params := createTestPeer(t)
	peer := params.peer
	peer.queueLimits = defaultQueueLimits()
	peer.queueLimits.maxMsgs = 1
	releaseWrite := make(chan struct{})
	thirdWriteStarted := make(chan struct{})
	var writeCount atomic.Int32
	conn := &mockQueueWriteConn{mockMessageConn: params.mockConn}
	conn.On("WriteMessage", mock.Anything).Run(func(mock.Arguments) {
		if writeCount.Add(1) == 3 {
			close(thirdWriteStarted)
			<-releaseWrite
		}
	}).Return(nil).Times(3)
	conn.On("SetWriteDeadline", mock.Anything).Return(nil).Times(3)
	conn.On("Flush").Return(0, nil).Times(3)
	peer.cfg.Conn = conn
	peer.cg.WgAdd(2)
	go peer.queueHandler()
	go peer.writeHandler()
	released := false
	t.Cleanup(func() {
		if !released {
			close(releaseWrite)
		}
		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Complete two synchronous sends, block a third in the writer,
	// admit a fourth asynchronously, then submit the fifth message that
	// exceeds the one-message local backlog.
	firstErr := peer.SendMessage(true, lnwire.NewPing(0))
	secondErr := peer.SendMessage(true, lnwire.NewPing(0))
	thirdResult := make(chan error, 1)
	go func() {
		thirdResult <- peer.SendMessage(true, lnwire.NewPing(0))
	}()
	_, err := fn.RecvOrTimeout(thirdWriteStarted, timeout)
	require.NoError(t, err)
	queuedErr := peer.SendMessage(false, lnwire.NewPing(0))
	overflowErr := peer.SendMessage(true, lnwire.NewPing(0))

	// Assert: The live writes and exact-limit admission succeed, while only
	// the first excess public send receives the typed overflow. Releasing
	// the in-flight write proves all participating goroutines terminate.
	require.NoError(t, firstErr)
	require.NoError(t, secondErr)
	require.NoError(t, queuedErr)
	require.ErrorIs(t, overflowErr, errQueueOverflow)
	close(releaseWrite)
	released = true
	_, err = fn.RecvOrTimeout(thirdResult, timeout)
	require.NoError(t, err)
	peer.cg.WgWait()
	conn.AssertExpectations(t)
}

// TestPeerConcurrentSenders verifies that synchronized public callers share
// the bounded queue and single writer without racing or losing acknowledgments.
func TestPeerConcurrentSenders(t *testing.T) {
	// Arrange: Give each sender one queue slot and configure a testify mock
	// for the exact write lifecycle, so the race run observes queue and
	// writer coordination instead of stopping at outgoingQueue admission.
	const numSenders = 16
	params := createTestPeer(t)
	peer := params.peer
	peer.queueLimits = defaultQueueLimits()
	peer.queueLimits.maxMsgs = numSenders
	conn := &mockQueueWriteConn{mockMessageConn: params.mockConn}
	conn.On("WriteMessage", mock.Anything).Return(nil).Times(numSenders)
	conn.On("SetWriteDeadline", mock.Anything).Return(nil).Times(numSenders)
	conn.On("Flush").Return(0, nil).Times(numSenders)
	peer.cfg.Conn = conn
	peer.cg.WgAdd(2)
	go peer.queueHandler()
	go peer.writeHandler()
	t.Cleanup(func() {
		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Launch all synchronous senders concurrently and collect each
	// public result through a buffered channel that cannot serialize them.
	results := make(chan error, numSenders)
	for i := 0; i < numSenders; i++ {
		go func() {
			results <- peer.SendMessage(true, lnwire.NewPing(0))
		}()
	}

	// Assert: Every sender receives its successful writer acknowledgment,
	// all expected wire operations occur, and both handlers join cleanly.
	for i := 0; i < numSenders; i++ {
		err, recvErr := fn.RecvOrTimeout(results, timeout)
		require.NoError(t, recvErr)
		require.NoError(t, err)
	}
	peer.cg.Quit()
	peer.cg.WgWait()
	conn.AssertExpectations(t)
}

// TestPeerSendMessageBatchReturnsQueueOverflow verifies that a synchronous
// variadic send reports a later queue rejection even while its first message
// remains accepted and pending.
func TestPeerSendMessageBatchReturnsQueueOverflow(t *testing.T) {
	t.Parallel()

	// Arrange: Restrict the peer to one retained message and start only the
	// queue handler. With no writer, the first batch item cannot
	// acknowledge before the second item crosses the count limit.
	params := createTestPeer(t)
	peer := params.peer
	peer.queueLimits = defaultQueueLimits()
	peer.queueLimits.maxMsgs = 1
	startTestQueueHandler(peer)
	t.Cleanup(func() {
		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Submit both messages through the public synchronous API so they
	// share one batch result path and the later rejection initiates
	// teardown.
	err := peer.SendMessage(
		true, lnwire.NewPing(0), lnwire.NewPing(0),
	)

	// Assert: The overflow sentinel wins over the first message's missing
	// acknowledgement and generic cancellation, then teardown completes.
	require.ErrorIs(t, err, errQueueOverflow)
	_, err = fn.RecvOrTimeout(peer.cg.Done(), timeout)
	require.NoError(t, err)
	peer.cg.WgWait()
}

// TestPeerMessageQueueCost verifies the non-serializing cost rules used by
// the outgoing queue byte budget.
func TestPeerMessageQueueCost(t *testing.T) {
	t.Parallel()

	// Arrange: Load the production fixed overhead and describe each
	// retained payload declaratively so expectations follow the policy
	// without serialization hiding whether the original slice is charged.
	limits := defaultQueueLimits()
	channelFeatures := lnwire.NewRawFeatureVector(1, 17)
	nodeFeatures := lnwire.NewRawFeatureVector(3, 9)
	// The highest defined feature produces a large, mostly empty serialized
	// span, guarding against multiplication as if every bit were set.
	sparseFeatures := lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootOverlayChansRequired,
	)
	// A dense vector retains one map entry for every possible FeatureBit.
	// Building the complete key space proves queue charging follows decoded
	// population instead of only the much smaller serialized bit span.
	denseFeatureBits := make([]lnwire.FeatureBit, 1<<16)
	for i := range denseFeatureBits {
		denseFeatureBits[i] = lnwire.FeatureBit(i)
	}
	denseFeatures := lnwire.NewRawFeatureVector(denseFeatureBits...)
	tcpAddr := &net.TCPAddr{
		IP:   net.IPv4(192, 0, 2, 1),
		Port: 9735,
		Zone: "test-zone",
	}
	onionAddr := &tor.OnionAddr{
		OnionService: "abcdefghijklmnop.onion",
		Port:         9735,
	}
	dnsAddr := &lnwire.DNSAddress{
		Hostname: "node.example.com",
		Port:     9735,
	}
	opaqueAddr := &lnwire.OpaqueAddrs{
		Payload: make([]byte, 11),
	}
	nodeAddrs := []net.Addr{tcpAddr, onionAddr, dnsAddr, opaqueAddr}
	addressCost := len(nodeAddrs)*queuedAddrOverhead + len(tcpAddr.IP) +
		len(tcpAddr.Zone) + len(onionAddr.OnionService) +
		len(dnsAddr.Hostname) + len(opaqueAddr.Payload)
	tests := []struct {
		name     string
		msg      lnwire.Message
		expected int
	}{
		{
			name:     "fixed message",
			msg:      lnwire.NewPing(0),
			expected: limits.msgOverhead,
		},
		{
			name:     "shared Pong payload",
			msg:      lnwire.NewPong(make([]byte, 1000)),
			expected: limits.msgOverhead,
		},
		{
			name: "failure reason",
			msg: &lnwire.UpdateFailHTLC{
				Reason: make([]byte, 5),
			},
			expected: limits.msgOverhead + 5,
		},
		{
			name: "add onion and extra data",
			msg: &lnwire.UpdateAddHTLC{
				ExtraData: make([]byte, 7),
			},
			expected: limits.msgOverhead +
				lnwire.OnionPacketSize + 7,
		},
		{
			name: "error data",
			msg: &lnwire.Error{
				Data: make([]byte, 3),
			},
			expected: limits.msgOverhead + 3,
		},
		{
			name: "warning data",
			msg: &lnwire.Warning{
				Data: make([]byte, 4),
			},
			expected: limits.msgOverhead + 4,
		},
		{
			name: "onion message blob",
			msg: &lnwire.OnionMessage{
				OnionBlob: make([]byte, 6),
			},
			expected: limits.msgOverhead + 6,
		},
		{
			name: "channel announcement retained data",
			msg: &lnwire.ChannelAnnouncement1{
				Features:        channelFeatures,
				ExtraOpaqueData: make([]byte, 8),
			},
			// Literal 107 pins 8 opaque bytes, 64 bytes of feature
			// overhead, two map entries, and the three-byte span.
			expected: limits.msgOverhead + 107,
		},
		{
			name: "node announcement retained data",
			msg: &lnwire.NodeAnnouncement1{
				Features:        nodeFeatures,
				Addresses:       nodeAddrs,
				ExtraOpaqueData: make([]byte, 9),
			},
			// Literal 107 pins 9 opaque bytes, 64 bytes of feature
			// overhead, two map entries, and the two-byte span.
			expected: limits.msgOverhead + 107 + addressCost,
		},
		{
			name: "sparse high feature",
			msg: &lnwire.ChannelAnnouncement1{
				Features: sparseFeatures,
			},
			// Literal 334 covers fixed, sparse-span, and one-entry
			// costs without charging unset intervening bits.
			expected: limits.msgOverhead + 334,
		},
		{
			name: "dense feature entries",
			msg: &lnwire.ChannelAnnouncement1{
				Features: denseFeatures,
			},
			// The expected cost independently derives the retained
			// entry charge from the full input. This catches a
			// regression to serialized-span-only accounting.
			expected: limits.msgOverhead + queuedFeatureOverhead +
				denseFeatures.SerializeSize() +
				len(denseFeatureBits)*
					queuedFeatureEntryOverhead,
		},
		{
			name: "channel update opaque data",
			msg: &lnwire.ChannelUpdate1{
				ExtraOpaqueData: make([]byte, 10),
			},
			expected: limits.msgOverhead + 10,
		},
		{
			name: "channel range query retained data",
			msg: &lnwire.QueryChannelRange{
				QueryOptions: lnwire.NewTimestampQueryOption(),
				ExtraData:    make([]byte, 11),
			},
			// The single query bit retains its vector object, map
			// entry, and one-byte span in addition to opaque data.
			expected: limits.msgOverhead + 11 +
				queuedFeatureOverhead +
				queuedFeatureEntryOverhead + 1,
		},
		{
			name: "short channel ID query retained data",
			msg: &lnwire.QueryShortChanIDs{
				ShortChanIDs: make([]lnwire.ShortChannelID, 2),
				ExtraData:    make([]byte, 7),
			},
			// Decoded SCIDs occupy their padded in-memory width,
			// while the unknown TLV backing bytes remain retained.
			expected: limits.msgOverhead +
				2*queuedShortChanIDSize + 7,
		},
		{
			name: "channel range reply retained data",
			msg: &lnwire.ReplyChannelRange{
				ShortChanIDs: make([]lnwire.ShortChannelID, 3),
				Timestamps:   make(lnwire.Timestamps, 3),
				ExtraData:    make([]byte, 5),
			},
			// Each decoded reply row retains one padded SCID and
			// one pair of uint32 update timestamps.
			expected: limits.msgOverhead +
				3*queuedShortChanIDSize +
				3*queuedTimestampPairSize + 5,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: Select the row's concrete message and exact
			// fixed-plus-dynamic expectation without an encoder.

			// Act: Evaluate the immutable charge stored with the
			// message when it enters the outgoing queue.
			actual := limits.msgCost(test.msg)

			// Assert: Exact equality proves the dynamic bytes are
			// neither omitted nor counted more than once.
			require.Equal(t, test.expected, actual)
		})
	}
}

// TestPeerQueueHandlerDrainsBacklog verifies that messages leaving the queue
// decrement its shadow count for a long-lived peer.
func TestPeerQueueHandlerDrainsBacklog(t *testing.T) {
	t.Parallel()

	// Arrange: Start the isolated handler and register cleanup that
	// cancels and joins it so no goroutine survives this test.
	params := createTestPeer(t)
	peer := params.peer
	startTestQueueHandler(peer)
	t.Cleanup(func() {
		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Exceed the lifetime count cap while draining each message
	// immediately, forcing the shadow count back to zero each time.
	for i := 0; i <= peer.queueLimits.maxMsgs; i++ {
		peer.queueMsg(lnwire.NewPong(nil), nil)

		select {
		case <-peer.sendQueue:
		case <-peer.cg.Done():
			t.Fatal("healthy drained queue exceeded message bound")
		case <-time.After(timeout):
			t.Fatal("queued message was not drained")
		}
	}

	// Assert: Every item drained and the peer remains live, proving
	// only the concurrent backlog contributes to the queue bound.
	select {
	case <-peer.cg.Done():
		t.Fatal("healthy drained queue disconnected")
	default:
	}
}

// TestPeerQueueHandlerServicesQueueDuringTeardown verifies that queue
// producers are failed while Disconnect waits for peer startup to finish.
func TestPeerQueueHandlerServicesQueueDuringTeardown(t *testing.T) {
	t.Parallel()

	// Arrange: Mark the peer started but hold startReady open, so
	// overflow enters Disconnect without finishing; cleanup later
	// releases that gate, cancels, and joins the queue goroutine.
	params := createTestPeer(t)
	peer := params.peer
	atomic.StoreInt32(&peer.started, 1)
	startTestQueueHandler(peer)
	t.Cleanup(func() {
		select {
		case <-peer.startReady:
		default:
			close(peer.startReady)
		}

		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Cross the count cap, wait for Disconnect to block, then
	// invoke a synchronous sender in a goroutine so the queue handler
	// must return its result while teardown remains pending.
	for i := 0; i <= peer.queueLimits.maxMsgs; i++ {
		peer.queueMsg(lnwire.NewPong(nil), nil)
	}

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&peer.disconnect) == 1
	}, timeout, 10*time.Millisecond)

	errChan := make(chan error, 1)
	go func() {
		errChan <- peer.SendMessage(true, lnwire.NewPing(0))
	}()

	// Assert: The sender gets ErrPeerExiting while cg remains live,
	// proving producers are serviced until startup teardown can finish.
	err, recvErr := fn.RecvOrTimeout(errChan, timeout)
	require.NoError(t, recvErr)
	require.ErrorIs(t, err, lnpeer.ErrPeerExiting)

	select {
	case <-peer.cg.Done():
		t.Fatal("Disconnect completed before startReady was signaled")
	default:
	}
}

// TestPeerPriorityMessageSharesQueueBudget verifies that lazy traffic can fill
// the shared queue budget and cause the first later priority send to tear down
// the peer rather than bypassing or displacing an already accepted message.
func TestPeerPriorityMessageSharesQueueBudget(t *testing.T) {
	t.Parallel()

	// Arrange: Restrict the peer to one fixed-cost message and start only
	// queue ownership, leaving no writer to drain the lazy message. Cleanup
	// cancels and joins the handler even if an assertion stops the test
	// early.
	params := createTestPeer(t)
	peer := params.peer
	peer.queueLimits = queueLimits{
		maxMsgs:     1,
		maxBytes:    queuedMsgOverhead,
		msgOverhead: queuedMsgOverhead,
	}
	startTestQueueHandler(peer)
	t.Cleanup(func() {
		peer.cg.Quit()
		peer.cg.WgWait()
	})

	// Act: Fill the exact shared budget through the public lazy API, then
	// send one synchronous priority message so its rejection is observable.
	err := peer.SendMessageLazy(false, lnwire.NewPong(nil))
	require.NoError(t, err)
	err = peer.SendMessage(true, lnwire.NewPing(0))

	// Assert: Priority has no reserved capacity: the first excess send gets
	// the typed overflow cause, disconnects the peer, and finishes
	// teardown.
	require.ErrorIs(t, err, errQueueOverflow)
	_, err = fn.RecvOrTimeout(peer.cg.Done(), timeout)
	require.NoError(t, err)
	peer.cg.WgWait()
}

// TestMessageSummaryPingIncludesNumPongBytes ensures the debug summary for a
// ping exposes the requested pong size, which makes ignored no-reply pings
// visible without requiring trace-level logging.
func TestMessageSummaryPingIncludesNumPongBytes(t *testing.T) {
	t.Parallel()

	// Arrange: Build a ping that uses the BOLT 1 no-reply sentinel range.
	msg := &lnwire.Ping{
		NumPongBytes: 65535,
		PaddingBytes: []byte{1, 2, 3},
	}

	// Act: Generate the human-readable message summary.
	summary := messageSummary(msg)

	// Assert: The summary includes both the requested pong size and payload
	// length so debug logs can explain why no pong was sent.
	require.Equal(t, "num_pong_bytes=65535, len(ping_bytes)=3", summary)
}

// TestUpdateNextRevocation checks that the method `updateNextRevocation` is
// behave as expected.
func TestUpdateNextRevocation(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(err, "unable to create test channels")

	bobChan := harness.channel
	alicePeer := harness.peer

	// testChannel is used to test the updateNextRevocation function.
	testChannel := bobChan.State()

	// Update the next revocation for a known channel should give us no
	// error.
	err = alicePeer.updateNextRevocation(testChannel)
	require.NoError(err, "expected no error")

	// Test an error is returned when the chanID cannot be found in
	// `activeChannels` map.
	testChannel.FundingOutpoint = wire.OutPoint{Index: 0}
	err = alicePeer.updateNextRevocation(testChannel)
	require.Error(err, "expected an error")

	// Test an error is returned when the chanID's corresponding channel is
	// nil.
	testChannel.FundingOutpoint = wire.OutPoint{Index: 1}
	chanID := lnwire.NewChanIDFromOutPoint(testChannel.FundingOutpoint)
	alicePeer.activeChannels.Store(chanID, nil)

	err = alicePeer.updateNextRevocation(testChannel)
	require.Error(err, "expected an error")

	// TODO(yy): should also test `InitNextRevocation` is called on
	// `lnwallet.LightningWallet` once it's interfaced.
}

func assertMsgSent(t *testing.T, conn *mockMessageConn,
	msgType lnwire.MessageType) {

	t.Helper()

	require := require.New(t)

	rawMsg, err := fn.RecvOrTimeout(conn.writtenMessages, timeout)
	require.NoError(err)

	msgReader := bytes.NewReader(rawMsg)
	msg, err := lnwire.ReadMessage(msgReader, 0)
	require.NoError(err)

	require.Equal(msgType, msg.MsgType())
}

// TestAlwaysSendChannelUpdate tests that each time we connect to the peer if
// an active channel, we always send the latest channel update.
func TestAlwaysSendChannelUpdate(t *testing.T) {
	require := require.New(t)

	var channel *chanstate.OpenChannel
	channelIntercept := func(a, b *chanstate.OpenChannel) {
		channel = a
	}

	harness, err := createTestPeerWithChannel(t, channelIntercept)
	require.NoError(err, "unable to create test channels")

	// Avoid the need to mock the channel graph by marking the channel
	// borked. Borked channels still get a reestablish message sent on
	// reconnect, while skipping channel graph checks and link creation.
	require.NoError(channel.MarkBorked())

	// Start the peer, which'll trigger the normal init and start up logic.
	startPeerDone := startPeer(t, harness.mockConn, harness.peer)
	_, err = fn.RecvOrTimeout(startPeerDone, 2*timeout)
	require.NoError(err)

	// Assert that we eventually send a channel update.
	assertMsgSent(t, harness.mockConn, lnwire.MsgChannelReestablish)
	assertMsgSent(t, harness.mockConn, lnwire.MsgChannelUpdate)
}

// TODO(yy): add test for `addActiveChannel` and `handleNewActiveChannel` once
// we have interfaced `lnwallet.LightningChannel` and
// `*contractcourt.ChainArbitrator`.

// TestHandleNewPendingChannel checks the method `handleNewPendingChannel`
// behaves as expected.
func TestHandleNewPendingChannel(t *testing.T) {
	t.Parallel()

	// Create three channel IDs for testing.
	chanIDActive := lnwire.ChannelID{0}
	chanIDNotExist := lnwire.ChannelID{1}
	chanIDPending := lnwire.ChannelID{2}

	testCases := []struct {
		name   string
		chanID lnwire.ChannelID

		// expectChanAdded specifies whether this chanID will be added
		// to the peer's state.
		expectChanAdded bool
	}{
		{
			name:            "noop on active channel",
			chanID:          chanIDActive,
			expectChanAdded: false,
		},
		{
			name:            "noop on pending channel",
			chanID:          chanIDPending,
			expectChanAdded: false,
		},
		{
			name:            "new channel should be added",
			chanID:          chanIDNotExist,
			expectChanAdded: true,
		},
	}

	for _, tc := range testCases {

		// Create a request for testing.
		errChan := make(chan error, 1)
		req := &newChannelMsg{
			channelID: tc.chanID,
			err:       errChan,
		}

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)

			// Create a test brontide.
			dummyConfig := Config{}
			peer := NewBrontide(dummyConfig)

			// Create the test state.
			peer.activeChannels.Store(
				chanIDActive, &lnwallet.LightningChannel{},
			)
			peer.activeChannels.Store(chanIDPending, nil)

			// Assert test state, we should have two channels
			// store, one active and one pending.
			numChans := 2
			require.EqualValues(
				numChans, peer.activeChannels.Len(),
			)

			// Call the method.
			peer.handleNewPendingChannel(req)

			// Add one if we expect this channel to be added.
			if tc.expectChanAdded {
				numChans++
			}

			// Assert the number of channels is correct.
			require.Equal(numChans, peer.activeChannels.Len())

			// Assert the request's error chan is closed.
			err, ok := <-req.err
			require.False(ok, "expect err chan to be closed")
			require.NoError(err, "expect no error")
		})
	}
}

// TestHandleRemovePendingChannel checks the method
// `handleRemovePendingChannel` behaves as expected.
func TestHandleRemovePendingChannel(t *testing.T) {
	t.Parallel()

	// Create three channel IDs for testing.
	chanIDActive := lnwire.ChannelID{0}
	chanIDNotExist := lnwire.ChannelID{1}
	chanIDPending := lnwire.ChannelID{2}

	testCases := []struct {
		name   string
		chanID lnwire.ChannelID

		// expectDeleted specifies whether this chanID will be removed
		// from the peer's state.
		expectDeleted bool
	}{
		{
			name:          "noop on active channel",
			chanID:        chanIDActive,
			expectDeleted: false,
		},
		{
			name:          "pending channel should be removed",
			chanID:        chanIDPending,
			expectDeleted: true,
		},
		{
			name:          "noop on non-exist channel",
			chanID:        chanIDNotExist,
			expectDeleted: false,
		},
	}

	for _, tc := range testCases {

		// Create a request for testing.
		errChan := make(chan error, 1)
		req := &newChannelMsg{
			channelID: tc.chanID,
			err:       errChan,
		}

		// Create a test brontide.
		dummyConfig := Config{}
		peer := NewBrontide(dummyConfig)

		// Create the test state.
		peer.activeChannels.Store(
			chanIDActive, &lnwallet.LightningChannel{},
		)
		peer.activeChannels.Store(chanIDPending, nil)

		// Assert test state, we should have two channels store, one
		// active and one pending.
		require.Equal(t, 2, peer.activeChannels.Len())

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require := require.New(t)

			// Get the number of channels before mutating the
			// state.
			numChans := peer.activeChannels.Len()

			// Call the method.
			peer.handleRemovePendingChannel(req)

			// Minus one if we expect this channel to be removed.
			if tc.expectDeleted {
				numChans--
			}

			// Assert the number of channels is correct.
			require.Equal(numChans, peer.activeChannels.Len())

			// Assert the request's error chan is closed.
			err, ok := <-req.err
			require.False(ok, "expect err chan to be closed")
			require.NoError(err, "expect no error")
		})
	}
}

// TestStartupWriteMessageRace checks that no data race occurs when starting up
// a peer with an existing channel, while an outgoing message is queuing. Such
// a race occurred in https://github.com/lightningnetwork/lnd/issues/8184, where
// a channel reestablish message raced with another outgoing message.
//
// Note that races will only be detected with the Go race detector enabled.
func TestStartupWriteMessageRace(t *testing.T) {
	t.Parallel()

	// Use a callback to extract the channel created by
	// createTestPeerWithChannel, so we can mark it borked below.
	// We can't mark it borked within the callback, since the channel hasn't
	// been saved to the DB yet when the callback executes.
	var channel *chanstate.OpenChannel
	getChannels := func(a, b *chanstate.OpenChannel) {
		channel = a
	}

	// createTestPeerWithChannel creates a peer and a channel with that
	// peer.
	harness, err := createTestPeerWithChannel(t, getChannels)
	require.NoError(t, err, "unable to create test channel")

	peer := harness.peer

	// Avoid the need to mock the channel graph by marking the channel
	// borked. Borked channels still get a reestablish message sent on
	// reconnect, while skipping channel graph checks and link creation.
	require.NoError(t, channel.MarkBorked())

	// Use a mock conn to detect read/write races on the conn.
	mockConn := newMockConn(t, 2)
	peer.cfg.Conn = mockConn

	// Send a message while starting the peer. As the peer starts up, it
	// should not trigger a data race between the sending of this message
	// and the sending of the channel reestablish message.
	var sendPingDone = make(chan struct{})
	go func() {
		require.NoError(t, peer.SendMessage(true, lnwire.NewPing(0)))
		close(sendPingDone)
	}()

	// Start the peer. No data race should occur.
	startPeerDone := startPeer(t, mockConn, peer)

	// Ensure startup is complete.
	_, err = fn.RecvOrTimeout(startPeerDone, 2*timeout)
	require.NoError(t, err)

	// Ensure messages were sent during startup.
	<-sendPingDone
	for i := 0; i < 2; i++ {
		select {
		case <-mockConn.writtenMessages:
		default:
			t.Fatalf("Failed to send all messages during startup")
		}
	}
}

// TestRemovePendingChannel checks that we are able to remove a pending channel
// successfully from the peers channel map. This also makes sure the
// removePendingChannel is initialized so we don't send to a nil channel and
// get stuck.
func TestRemovePendingChannel(t *testing.T) {
	t.Parallel()

	// createTestPeerWithChannel creates a peer and a channel.
	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err, "unable to create test channel")

	peer := harness.peer

	// Add a pending channel to the peer Alice.
	errChan := make(chan error, 1)
	pendingChanID := lnwire.ChannelID{1}
	req := &newChannelMsg{
		channelID: pendingChanID,
		err:       errChan,
	}

	select {
	case peer.newPendingChannel <- req:
		// Operation completed successfully
	case <-time.After(timeout):
		t.Fatalf("not able to remove pending channel")
	}

	// Make sure the channel was added as a pending channel.
	// The peer was already created with one active channel therefore the
	// `activeChannels` had already one channel prior to adding the new one.
	// The `addedChannels` map only tracks new channels in the current life
	// cycle therefore the initial channel is not part of it.
	err = wait.NoError(func() error {
		if peer.activeChannels.Len() == 2 &&
			peer.addedChannels.Len() == 1 {

			return nil
		}

		return fmt.Errorf("pending channel not successfully added")
	}, wait.DefaultTimeout)

	require.NoError(t, err)

	// Now try to remove it, the errChan needs to be reopened because it was
	// closed during the pending channel registration above.
	errChan = make(chan error, 1)
	req = &newChannelMsg{
		channelID: pendingChanID,
		err:       errChan,
	}

	select {
	case peer.removePendingChannel <- req:
		// Operation completed successfully
	case <-time.After(timeout):
		t.Fatalf("not able to remove pending channel")
	}

	// Make sure the pending channel is successfully removed from both
	// channel maps.
	// The initial channel between the peer is still active at this point.
	err = wait.NoError(func() error {
		if peer.activeChannels.Len() == 1 &&
			peer.addedChannels.Len() == 0 {

			return nil
		}

		return fmt.Errorf("pending channel not successfully removed")
	}, wait.DefaultTimeout)

	require.NoError(t, err)
}

// mockAuxTrafficShaper is a mock implementation of htlcswitch.AuxTrafficShaper
// for testing the createHtlcValidator function.
type mockAuxTrafficShaper struct {
	mock.Mock
}

// ShouldHandleTraffic returns the configured mock values.
func (m *mockAuxTrafficShaper) ShouldHandleTraffic(
	cid lnwire.ShortChannelID,
	fundingBlob, htlcBlob fn.Option[tlv.Blob]) (bool, error) {

	args := m.Called(cid, fundingBlob, htlcBlob)
	return args.Bool(0), args.Error(1)
}

// PaymentBandwidth returns the configured mock values.
func (m *mockAuxTrafficShaper) PaymentBandwidth(fundingBlob, htlcBlob,
	commitmentBlob fn.Option[tlv.Blob], linkBandwidth,
	htlcAmt lnwire.MilliSatoshi, htlcView lnwallet.AuxHtlcView,
	peer route.Vertex) (lnwire.MilliSatoshi, error) {

	args := m.Called(
		fundingBlob, htlcBlob, commitmentBlob, linkBandwidth,
		htlcAmt, htlcView, peer,
	)

	bw, _ := args.Get(0).(lnwire.MilliSatoshi)

	return bw, args.Error(1)
}

// ProduceHtlcExtraData is part of the AuxTrafficShaper interface.
func (m *mockAuxTrafficShaper) ProduceHtlcExtraData(
	totalAmount lnwire.MilliSatoshi,
	htlcCustomRecords lnwire.CustomRecords,
	peer route.Vertex) (lnwire.MilliSatoshi, lnwire.CustomRecords,
	error) {

	args := m.Called(totalAmount, htlcCustomRecords, peer)

	amt, _ := args.Get(0).(lnwire.MilliSatoshi)
	records, _ := args.Get(1).(lnwire.CustomRecords)

	return amt, records, args.Error(2)
}

// IsCustomHTLC is part of the AuxTrafficShaper interface.
func (m *mockAuxTrafficShaper) IsCustomHTLC(
	htlcRecords lnwire.CustomRecords) bool {

	args := m.Called(htlcRecords)
	return args.Bool(0)
}

// Compile-time check that mockAuxTrafficShaper implements AuxTrafficShaper.
var _ htlcswitch.AuxTrafficShaper = (*mockAuxTrafficShaper)(nil)

// TestCreateHtlcValidator tests that the HTLC validator created by
// createHtlcValidator respects the ShouldHandleTraffic check. When
// ShouldHandleTraffic returns false, the validator should return nil without
// calling PaymentBandwidth.
func TestCreateHtlcValidator(t *testing.T) {
	t.Parallel()

	// Create a minimal Brontide with just the identity key set.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	peer := &Brontide{
		cfg: Config{
			Addr: &lnwire.NetAddress{
				IdentityKey: privKey.PubKey(),
			},
		},
	}

	// Create a mock channel with minimal required fields.
	dbChan := &chanstate.OpenChannel{
		ShortChannelID: lnwire.NewShortChanIDFromInt(123),
	}

	anyArg := mock.Anything

	testCases := []struct {
		name        string
		setupMock   func(*mockAuxTrafficShaper)
		htlcAmount  lnwire.MilliSatoshi
		linkBw      lnwire.MilliSatoshi
		expectError bool
	}{
		{
			name: "non-custom channel skips check",
			setupMock: func(m *mockAuxTrafficShaper) {
				m.On(
					"ShouldHandleTraffic",
					anyArg, anyArg, anyArg,
				).Return(false, nil)
			},
			htlcAmount:  1000,
			linkBw:      5000,
			expectError: false,
		},
		{
			name: "sufficient bandwidth",
			setupMock: func(m *mockAuxTrafficShaper) {
				m.On(
					"ShouldHandleTraffic",
					anyArg, anyArg, anyArg,
				).Return(true, nil)
				m.On(
					"PaymentBandwidth",
					anyArg, anyArg, anyArg,
					anyArg, anyArg, anyArg,
					anyArg,
				).Return(
					lnwire.MilliSatoshi(10000),
					nil,
				)
			},
			htlcAmount:  1000,
			linkBw:      5000,
			expectError: false,
		},
		{
			name: "insufficient bandwidth",
			setupMock: func(m *mockAuxTrafficShaper) {
				m.On(
					"ShouldHandleTraffic",
					anyArg, anyArg, anyArg,
				).Return(true, nil)
				m.On(
					"PaymentBandwidth",
					anyArg, anyArg, anyArg,
					anyArg, anyArg, anyArg,
					anyArg,
				).Return(
					lnwire.MilliSatoshi(500),
					nil,
				)
			},
			htlcAmount:  1000,
			linkBw:      5000,
			expectError: true,
		},
		{
			name: "ShouldHandleTraffic error",
			setupMock: func(m *mockAuxTrafficShaper) {
				m.On(
					"ShouldHandleTraffic",
					anyArg, anyArg, anyArg,
				).Return(
					false,
					fmt.Errorf("shaper error"),
				)
			},
			htlcAmount:  1000,
			linkBw:      5000,
			expectError: true,
		},
		{
			name: "PaymentBandwidth error",
			setupMock: func(m *mockAuxTrafficShaper) {
				m.On(
					"ShouldHandleTraffic",
					anyArg, anyArg, anyArg,
				).Return(true, nil)
				m.On(
					"PaymentBandwidth",
					anyArg, anyArg, anyArg,
					anyArg, anyArg, anyArg,
					anyArg,
				).Return(
					lnwire.MilliSatoshi(0),
					fmt.Errorf("bandwidth error"),
				)
			},
			htlcAmount:  1000,
			linkBw:      5000,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockAuxTrafficShaper{}
			tc.setupMock(m)

			validator := peer.createHtlcValidator(
				dbChan, m,
			)

			err := validator.ValidateHtlc(
				tc.htlcAmount, tc.linkBw,
				nil, lnwallet.AuxHtlcView{},
			)

			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			m.AssertExpectations(t)
		})
	}
}

// TestHasActiveChannels exercises the atomic active-channel counter that
// backs hasActiveChannels(). hasActiveChannels is on the hot path for
// every incoming onion message — the onion message ingress gate calls
// it per packet — so a correct, O(1) shadow of activeChannels is a
// load-bearing invariant. This test walks the three state transitions
// that have to keep numActiveChans in lockstep with activeChannels:
// initial emptiness, pending entries (nil values) that must not count,
// and pending-delete paths that must not decrement.
func TestHasActiveChannels(t *testing.T) {
	t.Parallel()

	peer := NewBrontide(Config{})

	// Initial state: no channels, counter is zero, gate is closed.
	require.False(t, peer.hasActiveChannels())
	require.Equal(t, int32(0), peer.numActiveChans.Load())

	// Simulate the loadActiveChannels active-channel path: the entry
	// is stored and the counter is incremented in lockstep. After
	// this, hasActiveChannels must flip to true because the peer now
	// holds a non-pending channel.
	activeID := lnwire.ChannelID{0x01}
	peer.activeChannels.Store(activeID, &lnwallet.LightningChannel{})
	peer.numActiveChans.Add(1)
	require.True(t, peer.hasActiveChannels())
	require.Equal(t, int32(1), peer.numActiveChans.Load())

	// Simulate the loadActiveChannels pending path: the entry is
	// stored as nil and the counter must NOT move. This is the
	// invariant the onion message gate relies on — pending channels
	// are cheap to open and get stuck, so they must not satisfy the
	// Sybil-resistance gate on their own.
	pendingID := lnwire.ChannelID{0x02}
	peer.activeChannels.Store(pendingID, nil)
	require.Equal(t, int32(1), peer.numActiveChans.Load())
	require.True(t, peer.hasActiveChannels())

	// handleRemovePendingChannel walks the pending-delete path. It
	// uses LoadAndDelete and must skip the counter decrement when
	// the previous value was nil (pending). If this invariant ever
	// broke, the counter would underflow every time a pending
	// channel was cancelled and hasActiveChannels would return the
	// wrong answer until the next reconnect.
	errChan := make(chan error, 1)
	peer.handleRemovePendingChannel(&newChannelMsg{
		channelID: pendingID,
		err:       errChan,
	})
	require.Equal(t, int32(1), peer.numActiveChans.Load())
	require.True(t, peer.hasActiveChannels())

	// The pending entry must have been removed from the map.
	_, found := peer.activeChannels.Load(pendingID)
	require.False(t, found)

	// Drain the request error channel so the test leaves no loose
	// ends. handleRemovePendingChannel closes the err chan via
	// defer, so we expect a closed-channel receive here.
	_, reqOk := <-errChan
	require.False(t, reqOk)

	// Finally, simulate WipeChannel's decrement path directly via
	// LoadAndDelete. We cannot call WipeChannel in this
	// dummy-config harness because it also calls
	// p.cfg.Switch.RemoveLink, but the counter-maintenance half of
	// WipeChannel is exactly the LoadAndDelete + conditional Add(-1)
	// we exercise here.
	prev, loaded := peer.activeChannels.LoadAndDelete(activeID)
	require.True(t, loaded)
	require.NotNil(t, prev)
	peer.numActiveChans.Add(-1)

	require.False(t, peer.hasActiveChannels())
	require.Equal(t, int32(0), peer.numActiveChans.Load())
}

// TestRbfCoopCloseAllowed asserts that the per-channel RBF coop close
// predicate excludes aux channels (channel types carrying a tapscript root)
// even when both peers have negotiated the RBF coop close feature, while
// permitting it for all other channel types.
func TestRbfCoopCloseAllowed(t *testing.T) {
	t.Parallel()

	newPeer := func(local, remote *lnwire.RawFeatureVector) *Brontide {
		return &Brontide{
			cfg: Config{
				Features: lnwire.NewFeatureVector(
					local, lnwire.Features,
				),
			},
			remoteFeatures: lnwire.NewFeatureVector(
				remote, lnwire.Features,
			),
		}
	}

	var (
		noBits = lnwire.NewRawFeatureVector()
		rbfBit = lnwire.NewRawFeatureVector(
			lnwire.RbfCoopCloseOptional,
		)
		stagingBit = lnwire.NewRawFeatureVector(
			lnwire.RbfCoopCloseOptionalStaging,
		)

		overlayChan = chanstate.SimpleTaprootFeatureBit |
			chanstate.TapscriptRootBit
	)

	tests := []struct {
		name     string
		peer     *Brontide
		chanType chanstate.ChannelType
		allowed  bool
	}{
		{
			name:     "both signal, plain channel",
			peer:     newPeer(rbfBit, rbfBit),
			chanType: chanstate.SingleFunderTweaklessBit,
			allowed:  true,
		},
		{
			name:     "both signal staging, plain channel",
			peer:     newPeer(stagingBit, stagingBit),
			chanType: chanstate.SingleFunderTweaklessBit,
			allowed:  true,
		},
		{
			name:     "both signal, simple taproot channel",
			peer:     newPeer(rbfBit, rbfBit),
			chanType: chanstate.SimpleTaprootFeatureBit,
			allowed:  true,
		},
		{
			name:     "both signal, aux (overlay) channel",
			peer:     newPeer(rbfBit, rbfBit),
			chanType: overlayChan,
			allowed:  false,
		},
		{
			name:     "both signal staging, aux (overlay) channel",
			peer:     newPeer(stagingBit, stagingBit),
			chanType: overlayChan,
			allowed:  false,
		},
		{
			name:     "only local signals, plain channel",
			peer:     newPeer(rbfBit, noBits),
			chanType: chanstate.SingleFunderTweaklessBit,
			allowed:  false,
		},
		{
			name:     "neither signals, aux (overlay) channel",
			peer:     newPeer(noBits, noBits),
			chanType: overlayChan,
			allowed:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.allowed,
				test.peer.rbfCoopCloseAllowed(
					test.chanType,
				),
			)
		})
	}
}
