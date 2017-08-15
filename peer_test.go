package main

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

func disablePeerLogger(t *testing.T) {
	peerLog = btclog.Disabled
	srvrLog = btclog.Disabled
	lnwallet.UseLogger(btclog.Disabled)
	htlcswitch.UseLogger(btclog.Disabled)
	channeldb.UseLogger(btclog.Disabled)
}

// TestPeerChannelClosureAcceptFeeResponder tests the shutdown responder's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeResponder(t *testing.T) {
	disablePeerLogger(t)
	t.Parallel()

	notifier := &mockNotfier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	responder, responderChan, initiatorChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(responderChan.ChannelPoint())

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer this
	// shutdown request with a Shutdown message.
	responder.shutdownChanReqs <- lnwire.NewShutdown(chanID, dummyDeliveryScript)

	var msg lnwire.Message
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	respDeliveryScript := shutdownMsg.Address

	// Alice will thereafter send a ClosingSigned message, indicating her
	// proposed closing transaction fee.
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive ClosingSigned message")
	}

	responderClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// We accept the fee, and send a ClosingSigned with the same fee back,
	// so she knows we agreed.
	peerFee := responderClosingSigned.FeeSatoshis
	initiatorSig, proposedFee, err := initiatorChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, respDeliveryScript)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	initSig := append(initiatorSig, byte(txscript.SigHashAll))
	parsedSig, err := btcec.ParseSignature(initSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	responder.closingSignedChanReqs <- closingSigned

	// The responder will now see that we agreed on the fee, and broadcast
	// the closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("closing tx not broadcast")
	}

	// And the initiator should be waiting for a confirmation notification.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureAcceptFeeInitiator tests the shutdown initiator's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeInitiator(t *testing.T) {
	disablePeerLogger(t)
	t.Parallel()

	notifier := &mockNotfier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	initiator, initiatorChan, responderChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We make the initiator send a shutdown request.
	updateChan := make(chan *lnrpc.CloseStatusUpdate, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType: htlcswitch.CloseRegular,
		ChanPoint: initiatorChan.ChannelPoint(),
		Updates:   updateChan,
		Err:       errChan,
	}
	initiator.localCloseChanReqs <- closeCommand

	// We should now be getting the shutdown request.
	var msg lnwire.Message
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	initiatorDeliveryScript := shutdownMsg.Address

	// We'll answer the shutdown message with our own Shutdown, and then a
	// ClosingSigned message.
	chanID := shutdownMsg.ChannelID
	initiator.shutdownChanReqs <- lnwire.NewShutdown(chanID,
		dummyDeliveryScript)

	estimator := lnwallet.StaticFeeEstimator{FeeRate: 50}
	feeRate := estimator.EstimateFeePerWeight(1) * 1000
	fee := responderChan.CalcFee(feeRate)
	closeSig, proposedFee, err := responderChan.CreateCloseProposal(fee,
		dummyDeliveryScript, initiatorDeliveryScript)
	if err != nil {
		t.Fatalf("unable to create close proposal: %v", err)
	}
	parsedSig, err := btcec.ParseSignature(closeSig, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(shutdownMsg.ChannelID,
		proposedFee, parsedSig)
	initiator.closingSignedChanReqs <- closingSigned

	// And we expect the initiator to accept the fee, and broadcast the
	// closing transaction.
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed message")
	}

	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	if closingSignedMsg.FeeSatoshis != proposedFee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			proposedFee, closingSignedMsg.FeeSatoshis)
	}

	// The initiator will now see that we agreed on the fee, and broadcast
	// the closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("closing tx not broadcast")
	}

	// And the initiator should be waiting for a confirmation notification.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsResponder tests the shutdown responder's
// behavior in the case where we must do several rounds of fee negotiation
// before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsResponder(t *testing.T) {
	disablePeerLogger(t)
	t.Parallel()

	notifier := &mockNotfier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	responder, responderChan, initiatorChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(responderChan.ChannelPoint())

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer this
	// shutdown request with a Shutdown message.
	responder.shutdownChanReqs <- lnwire.NewShutdown(chanID,
		dummyDeliveryScript)

	var msg lnwire.Message
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive shutdown message")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	respDeliveryScript := shutdownMsg.Address

	// Alice will thereafter send a ClosingSigned message, indicating her
	// proposed closing transaction fee.
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed message")
	}

	responderClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// We don't agree with the fee, and will send back one that's 2.5x.
	preferredRespFee := responderClosingSigned.FeeSatoshis
	increasedFee := uint64(float64(preferredRespFee) * 2.5)
	initiatorSig, proposedFee, err := initiatorChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := btcec.ParseSignature(initiatorSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	responder.closingSignedChanReqs <- closingSigned

	// The responder will see the new fee we propose, but with current
	// settings wont't accept anything over 2*FeeRate. We should get a new
	// proposal back, which should have the average fee rate proposed.
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed message")
	}

	responderClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	avgFee := (preferredRespFee + increasedFee) / 2
	peerFee := responderClosingSigned.FeeSatoshis
	if peerFee != avgFee {
		t.Fatalf("expected ClosingSigned with fee %v, got %v",
			proposedFee, responderClosingSigned.FeeSatoshis)
	}

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = uint64(float64(preferredRespFee) * 2.1)
	initiatorSig, proposedFee, err = initiatorChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = btcec.ParseSignature(initiatorSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	responder.closingSignedChanReqs <- closingSigned

	// It still won't be accepted, and we should get a new proposal, the
	// average of what we proposed, and what they proposed last time.
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed message")
	}

	responderClosingSigned, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	avgFee = (peerFee + increasedFee) / 2
	peerFee = responderClosingSigned.FeeSatoshis
	if peerFee != avgFee {
		t.Fatalf("expected ClosingSigned with fee %v, got %v",
			proposedFee, responderClosingSigned.FeeSatoshis)
	}

	// Accept fee.
	initiatorSig, proposedFee, err = initiatorChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	initSig := append(initiatorSig, byte(txscript.SigHashAll))
	parsedSig, err = btcec.ParseSignature(initSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	responder.closingSignedChanReqs <- closingSigned

	// The responder will now see that we agreed on the fee, and broadcast
	// the closing transaction.
	select {
	case <-broadcastTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("closing tx not broadcast")
	}

	// And the responder should be waiting for a confirmation notification.
	notifier.confChannel <- &chainntnfs.TxConfirmation{}
}

// TestPeerChannelClosureFeeNegotiationsInitiator tests the shutdown initiator's
// behavior in the case where we must do several rounds of fee negotiation
// before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsInitiator(t *testing.T) {
	disablePeerLogger(t)
	t.Parallel()

	notifier := &mockNotfier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	initiator, initiatorChan, responderChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// We make the initiator send a shutdown request.
	updateChan := make(chan *lnrpc.CloseStatusUpdate, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType: htlcswitch.CloseRegular,
		ChanPoint: initiatorChan.ChannelPoint(),
		Updates:   updateChan,
		Err:       errChan,
	}

	initiator.localCloseChanReqs <- closeCommand

	// We should now be getting the shutdown request.
	var msg lnwire.Message
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive shutdown request")
	}

	shutdownMsg, ok := msg.(*lnwire.Shutdown)
	if !ok {
		t.Fatalf("expected Shutdown message, got %T", msg)
	}

	initiatorDeliveryScript := shutdownMsg.Address

	// We'll answer the shutdown message with our own Shutdown, and then a
	// ClosingSigned message.
	chanID := lnwire.NewChanIDFromOutPoint(initiatorChan.ChannelPoint())
	respShutdown := lnwire.NewShutdown(chanID, dummyDeliveryScript)
	initiator.shutdownChanReqs <- respShutdown

	estimator := lnwallet.StaticFeeEstimator{FeeRate: 50}
	initiatorIdealFeeRate := estimator.EstimateFeePerWeight(1) * 1000
	initiatorIdealFee := responderChan.CalcFee(initiatorIdealFeeRate)
	increasedFee := uint64(float64(initiatorIdealFee) * 2.5)
	closeSig, proposedFee, err := responderChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to create close proposal: %v", err)
	}
	parsedSig, err := btcec.ParseSignature(closeSig, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(shutdownMsg.ChannelID,
		proposedFee, parsedSig)
	initiator.closingSignedChanReqs <- closingSigned

	// And we expect the initiator to reject the fee, and suggest a lower
	// one.
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed")
	}

	closingSignedMsg, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}
	avgFee := (initiatorIdealFee + increasedFee) / 2
	peerFee := closingSignedMsg.FeeSatoshis
	if peerFee != avgFee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			avgFee, peerFee)
	}

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = uint64(float64(initiatorIdealFee) * 2.1)
	responderSig, proposedFee, err := responderChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = btcec.ParseSignature(responderSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}

	closingSigned = lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	initiator.closingSignedChanReqs <- closingSigned

	// It still won't be accepted, and we should get a new proposal, the
	// average of what we proposed, and what they proposed last time.
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed")
	}

	initiatorClosingSigned, ok := msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	avgFee = (peerFee + increasedFee) / 2
	peerFee = initiatorClosingSigned.FeeSatoshis
	if peerFee != avgFee {
		t.Fatalf("expected ClosingSigned with fee %v, got %v",
			proposedFee, initiatorClosingSigned.FeeSatoshis)
	}

	// Accept fee.
	responderSig, proposedFee, err = responderChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	respSig := append(responderSig, byte(txscript.SigHashAll))
	parsedSig, err = btcec.ParseSignature(respSig, btcec.S256())
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, proposedFee, parsedSig)
	initiator.closingSignedChanReqs <- closingSigned

	// Wait for closing tx to be broadcasted.
	select {
	case <-broadcastTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("closing tx not broadcast")
	}
}
