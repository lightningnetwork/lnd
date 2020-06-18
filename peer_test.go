// +build !rpctest

package lnd

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// p2SHAddress is a valid pay to script hash address.
	p2SHAddress = "2NBFNJTktNa7GZusGbDbGKRZTxdK9VVez3n"

	// p2wshAddress is a valid pay to witness script hash address.
	p2wshAddress = "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"

	// timeout is a timeout value to use for tests which need ot wait for
	// a return value on a channel.
	timeout = time.Second * 5
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
		notifier, broadcastTxChan, noUpdate,
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
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	var msg lnwire.Message
	select {
	case outMsg := <-responder.outgoingQueue:
		msg = outMsg.msg
	case <-time.After(timeout):
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
	case <-time.After(timeout):
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

	parsedSig, err := lnwire.NewSigFromSignature(initiatorSig)
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
	case <-time.After(timeout):
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
		notifier, broadcastTxChan, noUpdate,
	)
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
	case <-time.After(timeout):
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

	estimator := chainfee.NewStaticEstimator(12500, 0)
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
	parsedSig, err := lnwire.NewSigFromSignature(closeSig)
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
	case <-time.After(timeout):
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
	case <-time.After(timeout):
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
		notifier, broadcastTxChan, noUpdate,
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
	case <-time.After(timeout):
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
	case <-time.After(timeout):
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

	parsedSig, err := lnwire.NewSigFromSignature(initiatorSig)
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
	case <-time.After(timeout):
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

	parsedSig, err = lnwire.NewSigFromSignature(initiatorSig)
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
	case <-time.After(timeout):
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

	parsedSig, err = lnwire.NewSigFromSignature(initiatorSig)
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
	case <-time.After(timeout):
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
		notifier, broadcastTxChan, noUpdate,
	)
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
	case <-time.After(timeout):
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

	estimator := chainfee.NewStaticEstimator(12500, 0)
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
	parsedSig, err := lnwire.NewSigFromSignature(closeSig)
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
	case <-time.After(timeout):
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
	case <-time.After(timeout):
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

	parsedSig, err = lnwire.NewSigFromSignature(responderSig)
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
	case <-time.After(timeout):
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

	parsedSig, err = lnwire.NewSigFromSignature(responderSig)
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
	case <-time.After(timeout):
		t.Fatalf("closing tx not broadcast")
	}
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
		expectedError  error
	}{
		{
			name:           "Neither set",
			userScript:     nil,
			shutdownScript: nil,
			expectedScript: nil,
			expectedError:  nil,
		},
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
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			script, err := chooseDeliveryScript(
				test.shutdownScript, test.userScript,
			)
			if err != test.expectedError {
				t.Fatalf("Expected: %v, got: %v", test.expectedError, err)
			}

			if !bytes.Equal(script, test.expectedScript) {
				t.Fatalf("Expected: %x, got: %x", test.expectedScript, script)
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
			notifier := &mockNotfier{
				confChannel: make(chan *chainntnfs.TxConfirmation),
			}
			broadcastTxChan := make(chan *wire.MsgTx)

			// Open a channel.
			initiator, initiatorChan, _, cleanUp, err := createTestPeer(
				notifier, broadcastTxChan, test.update,
			)
			if err != nil {
				t.Fatalf("unable to create test channels: %v", err)
			}
			defer cleanUp()

			// Request initiator to cooperatively close the channel, with
			// a specified delivery address.
			updateChan := make(chan interface{}, 1)
			errChan := make(chan error, 1)
			chanPoint := initiatorChan.ChannelPoint()
			closeCommand := htlcswitch.ChanClose{
				CloseType:      htlcswitch.CloseRegular,
				ChanPoint:      chanPoint,
				Updates:        updateChan,
				TargetFeePerKw: 12500,
				DeliveryScript: test.userCloseScript,
				Err:            errChan,
			}

			// Send the close command for the correct channel and check that a
			// shutdown message is sent.
			initiator.localCloseChanReqs <- &closeCommand

			var msg lnwire.Message
			select {
			case outMsg := <-initiator.outgoingQueue:
				msg = outMsg.msg
			case <-time.After(timeout):
				t.Fatalf("did not receive shutdown message")
			case err := <-errChan:
				// Fail if we do not expect an error.
				if err != test.expectedError {
					t.Fatalf("error closing channel: %v", err)
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
			// we epect shutdown to a random address and cannot match it.
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

// genScript creates a script paying out to the address provided, which must
// be a valid address.
func genScript(t *testing.T, address string) lnwire.DeliveryAddress {
	// Generate an address which can be used for testing.
	deliveryAddr, err := btcutil.DecodeAddress(
		address,
		activeNetParams.Params,
	)
	if err != nil {
		t.Fatalf("invalid delivery address: %v", err)
	}

	script, err := txscript.PayToAddrScript(deliveryAddr)
	if err != nil {
		t.Fatalf("cannot create script: %v", err)
	}

	return script
}
