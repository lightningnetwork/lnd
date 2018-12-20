// +build !rpctest

package main

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestPeerChannelClosureAcceptFeeResponder tests the shutdown responder's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeResponder(t *testing.T) {
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
	// node in this shutdown procedure. We first expect Alice to answer
	// this shutdown request with a Shutdown message.
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

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
	initiatorSig, _, _, err := initiatorChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := lnwire.NewSigFromRawSignature(initiatorSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, peerFee, parsedSig)
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

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
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      htlcswitch.CloseRegular,
		ChanPoint:      initiatorChan.ChannelPoint(),
		Updates:        updateChan,
		TargetFeePerKw: 12500,
		Err:            errChan,
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
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	estimator := lnwallet.NewStaticFeeEstimator(12500, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		t.Fatalf("unable to query fee estimator: %v", err)
	}
	fee := responderChan.CalcFee(feePerKw)
	closeSig, _, _, err := responderChan.CreateCloseProposal(fee,
		dummyDeliveryScript, initiatorDeliveryScript)
	if err != nil {
		t.Fatalf("unable to create close proposal: %v", err)
	}
	parsedSig, err := lnwire.NewSigFromRawSignature(closeSig)
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(shutdownMsg.ChannelID,
		fee, parsedSig)
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

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

	if closingSignedMsg.FeeSatoshis != fee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			fee, closingSignedMsg.FeeSatoshis)
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

// TestPeerChannelClosureFeeNegotiationsResponder tests the shutdown
// responder's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsResponder(t *testing.T) {
	t.Parallel()

	notifier := &mockNotfier{
		confChannel: make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	responder, responderChan, initiatorChan, cleanUp, err := createTestPeer(
		notifier, broadcastTxChan,
	)
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	chanID := lnwire.NewChanIDFromOutPoint(responderChan.ChannelPoint())

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure. We first expect Alice to answer
	// this shutdown request with a Shutdown message.
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

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
	increasedFee := btcutil.Amount(float64(preferredRespFee) * 2.5)
	initiatorSig, _, _, err := initiatorChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err := lnwire.NewSigFromRawSignature(initiatorSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned := lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// The responder will see the new fee we propose, but with current
	// settings it won't accept it immediately as it differs too much by
	// its ideal fee. We should get a new proposal back, which should have
	// the average fee rate proposed.
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

	// The fee sent by the responder should be less than the fee we just
	// sent as it should attempt to compromise.
	peerFee := responderClosingSigned.FeeSatoshis
	if peerFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", peerFee, increasedFee)
	}
	lastFeeResponder := peerFee

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = btcutil.Amount(float64(preferredRespFee) * 2.1)
	initiatorSig, _, _, err = initiatorChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromRawSignature(initiatorSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

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

	// The peer should inch towards our fee, in order to compromise.
	// Additionally, this fee should be less than the fee we sent prior.
	peerFee = responderClosingSigned.FeeSatoshis
	if peerFee < lastFeeResponder {
		t.Fatalf("new fee should be greater than prior: new=%v, "+
			"prior=%v", peerFee, lastFeeResponder)
	}
	if peerFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", peerFee, increasedFee)
	}

	// Finally, we'll accept the fee by echoing back the same fee that they
	// sent to us.
	initiatorSig, _, _, err = initiatorChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, respDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromRawSignature(initiatorSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, peerFee, parsedSig)
	responder.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

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

// TestPeerChannelClosureFeeNegotiationsInitiator tests the shutdown
// initiator's behavior in the case where we must do several rounds of fee
// negotiation before we agree on a fee.
func TestPeerChannelClosureFeeNegotiationsInitiator(t *testing.T) {
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
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      htlcswitch.CloseRegular,
		ChanPoint:      initiatorChan.ChannelPoint(),
		Updates:        updateChan,
		TargetFeePerKw: 12500,
		Err:            errChan,
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
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: respShutdown,
	}

	estimator := lnwallet.NewStaticFeeEstimator(12500, 0)
	initiatorIdealFeeRate, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		t.Fatalf("unable to query fee estimator: %v", err)
	}
	initiatorIdealFee := responderChan.CalcFee(initiatorIdealFeeRate)
	increasedFee := btcutil.Amount(float64(initiatorIdealFee) * 2.5)
	closeSig, _, _, err := responderChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("unable to create close proposal: %v", err)
	}
	parsedSig, err := lnwire.NewSigFromRawSignature(closeSig)
	if err != nil {
		t.Fatalf("unable to parse signature: %v", err)
	}

	closingSigned := lnwire.NewClosingSigned(
		shutdownMsg.ChannelID, increasedFee, parsedSig,
	)
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// We should get two closing signed messages, the first will be the
	// ideal fee sent by the initiator in response to our shutdown request.
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
	if closingSignedMsg.FeeSatoshis != initiatorIdealFee {
		t.Fatalf("expected ClosingSigned fee to be %v, instead got %v",
			initiatorIdealFee, closingSignedMsg.FeeSatoshis)
	}
	lastFeeSent := closingSignedMsg.FeeSatoshis

	// The second message should be the compromise fee sent in response to
	// them receiving our fee proposal.
	select {
	case outMsg := <-initiator.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(time.Second * 5):
		t.Fatalf("did not receive closing signed")
	}
	closingSignedMsg, ok = msg.(*lnwire.ClosingSigned)
	if !ok {
		t.Fatalf("expected ClosingSigned message, got %T", msg)
	}

	// The peer should inch towards our fee, in order to compromise.
	// Additionally, this fee should be less than the fee we sent prior.
	peerFee := closingSignedMsg.FeeSatoshis
	if peerFee < lastFeeSent {
		t.Fatalf("new fee should be greater than prior: new=%v, "+
			"prior=%v", peerFee, lastFeeSent)
	}
	if peerFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", peerFee, increasedFee)
	}
	lastFeeSent = closingSignedMsg.FeeSatoshis

	// We try negotiating a 2.1x fee, which should also be rejected.
	increasedFee = btcutil.Amount(float64(initiatorIdealFee) * 2.1)
	responderSig, _, _, err := responderChan.CreateCloseProposal(
		increasedFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromRawSignature(responderSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}

	closingSigned = lnwire.NewClosingSigned(chanID, increasedFee, parsedSig)
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

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

	// Once again, the fee sent by the initiator should be greater than the
	// last fee they sent, but less than the last fee we sent.
	peerFee = initiatorClosingSigned.FeeSatoshis
	if peerFee < lastFeeSent {
		t.Fatalf("new fee should be greater than prior: new=%v, "+
			"prior=%v", peerFee, lastFeeSent)
	}
	if peerFee > increasedFee {
		t.Fatalf("new fee should be less than our fee: new=%v, "+
			"prior=%v", peerFee, increasedFee)
	}

	// At this point, we'll accept their fee by sending back a CloseSigned
	// message with an identical fee.
	responderSig, _, _, err = responderChan.CreateCloseProposal(
		peerFee, dummyDeliveryScript, initiatorDeliveryScript,
	)
	if err != nil {
		t.Fatalf("error creating close proposal: %v", err)
	}

	parsedSig, err = lnwire.NewSigFromRawSignature(responderSig)
	if err != nil {
		t.Fatalf("error parsing signature: %v", err)
	}
	closingSigned = lnwire.NewClosingSigned(chanID, peerFee, parsedSig)
	initiator.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: closingSigned,
	}

	// Wait for closing tx to be broadcasted.
	select {
	case <-broadcastTxChan:
	case <-time.After(time.Second * 5):
		t.Fatalf("closing tx not broadcast")
	}
}
