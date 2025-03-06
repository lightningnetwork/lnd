package peer

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
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
		test := test

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
	setShutdown := func(a, b *channeldb.OpenChannel) {
		a.LocalShutdownScript = script
		b.RemoteShutdownScript = script
	}

	tests := []struct {
		name string

		// update is a function used to set values on the channel set up for the
		// test. It is used to set values for upfront shutdown addresses.
		update func(a, b *channeldb.OpenChannel)

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
		test := test

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
		test := test

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
func genScript(t *testing.T, address string) lnwire.DeliveryAddress {
	// Generate an address which can be used for testing.
	deliveryAddr, err := btcutil.DecodeAddress(
		address,
		&chaincfg.TestNet3Params,
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

	var channel *channeldb.OpenChannel
	channelIntercept := func(a, b *channeldb.OpenChannel) {
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
		tc := tc

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
		tc := tc

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
	var channel *channeldb.OpenChannel
	getChannels := func(a, b *channeldb.OpenChannel) {
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
