package peer

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/stretchr/testify/require"
)

var (
	// p2wshAddress is a valid pay to witness script hash address.
	p2wshAddress = "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"
)

// TestPeerChannelClosureAcceptFeeResponder tests the shutdown responder's
// behavior if we can agree on the fee immediately.
func TestPeerChannelClosureAcceptFeeResponder(t *testing.T) {
	t.Parallel()

	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	mockSwitch := &mockMessageSwitch{}

	alicePeer, bobChan, err := createTestPeer(
		t, notifier, broadcastTxChan, noUpdate, mockSwitch,
	)
	require.NoError(t, err, "unable to create test channels")

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())

	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We send a shutdown request to Alice. She will now be the responding
	// node in this shutdown procedure.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID, dummyDeliveryScript),
	}

	// Alice calls HandleLocalCloseChanReqs to notify us that the link has
	// sent Shutdown.
	aliceDelivery := genScript(t, p2wshAddress)
	aliceClose := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		TargetFeePerKw: 300,
		DeliveryScript: aliceDelivery,
		Updates:        make(chan interface{}, 2),
		Err:            make(chan error, 1),
	}
	aliceQuit := make(chan struct{})
	alicePeer.HandleLocalCloseChanReqs(aliceClose, aliceQuit)

	// Alice calls HandleCoopReady which marks the coop close flow as
	// ready.
	alicePeer.HandleCoopReady(bobChan.ChannelPoint(), aliceQuit)

	// Alice will then send a ClosingSigned message, indicating her proposed
	// closing transaction fee. Alice sends the ClosingSigned message as she is
	// the initiator of the channel.
	var msg lnwire.Message
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
		aliceFee, dummyDeliveryScript, aliceDelivery,
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

	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	mockSwitch := &mockMessageSwitch{}

	alicePeer, bobChan, err := createTestPeer(
		t, notifier, broadcastTxChan, noUpdate, mockSwitch,
	)
	require.NoError(t, err, "unable to create test channels")

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())
	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	dummyDeliveryScript := genScript(t, p2wshAddress)

	// We make Alice send a shutdown request.
	aliceDelivery := genScript(t, p2wshAddress)
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		Updates:        updateChan,
		DeliveryScript: aliceDelivery,
		TargetFeePerKw: 12500,
		Err:            errChan,
	}
	alicePeer.localCloseChanReqs <- closeCommand

	// Bob will respond with his own Shutdown message.
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: lnwire.NewShutdown(chanID,
			dummyDeliveryScript),
	}

	// Alice calls HandleCoopReady and will send ClosingSigned.
	aliceQuit := make(chan struct{})
	alicePeer.HandleCoopReady(bobChan.ChannelPoint(), aliceQuit)

	// Alice will reply with a ClosingSigned here.
	var msg lnwire.Message
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
		bobFee, dummyDeliveryScript, aliceDelivery,
	)
	require.NoError(t, err, "unable to create close proposal")
	parsedSig, err := lnwire.NewSigFromSignature(bobSig)
	require.NoError(t, err, "unable to parse signature")

	closingSigned := lnwire.NewClosingSigned(chanID,
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

	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	mockSwitch := &mockMessageSwitch{}

	alicePeer, bobChan, err := createTestPeer(
		t, notifier, broadcastTxChan, noUpdate, mockSwitch,
	)
	require.NoError(t, err, "unable to create test channels")

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())

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

	// Alice calls HandleLocalCloseChanReqs to notify that she sent
	// Shutdown.
	aliceDelivery := genScript(t, p2wshAddress)
	aliceClose := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		TargetFeePerKw: 300,
		DeliveryScript: aliceDelivery,
		Updates:        make(chan interface{}, 2),
		Err:            make(chan error, 1),
	}
	aliceQuit := make(chan struct{})
	alicePeer.HandleLocalCloseChanReqs(aliceClose, aliceQuit)

	// Alice will now call HandleCoopReady and send ClosingSigned.
	alicePeer.HandleCoopReady(bobChan.ChannelPoint(), aliceQuit)

	var msg lnwire.Message
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
		increasedFee, dummyDeliveryScript, aliceDelivery,
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
		increasedFee, dummyDeliveryScript, aliceDelivery,
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
		aliceFee, dummyDeliveryScript, aliceDelivery,
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

	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	broadcastTxChan := make(chan *wire.MsgTx)

	mockSwitch := &mockMessageSwitch{}

	alicePeer, bobChan, err := createTestPeer(
		t, notifier, broadcastTxChan, noUpdate, mockSwitch,
	)
	require.NoError(t, err, "unable to create test channels")

	chanID := lnwire.NewChanIDFromOutPoint(bobChan.ChannelPoint())
	mockLink := newMockUpdateHandler(chanID)
	mockSwitch.links = append(mockSwitch.links, mockLink)

	// We make the initiator send a shutdown request.
	updateChan := make(chan interface{}, 1)
	errChan := make(chan error, 1)
	aliceDelivery := genScript(t, p2wshAddress)
	closeCommand := &htlcswitch.ChanClose{
		CloseType:      contractcourt.CloseRegular,
		ChanPoint:      bobChan.ChannelPoint(),
		Updates:        updateChan,
		DeliveryScript: aliceDelivery,
		TargetFeePerKw: 12500,
		Err:            errChan,
	}

	alicePeer.localCloseChanReqs <- closeCommand

	// Bob will answer the Shutdown message with his own Shutdown.
	dummyDeliveryScript := genScript(t, p2wshAddress)
	respShutdown := lnwire.NewShutdown(chanID, dummyDeliveryScript)
	alicePeer.chanCloseMsgs <- &closeMsg{
		cid: chanID,
		msg: respShutdown,
	}

	// Alice will now mark the channel clean and send ClosingSigned.
	aliceQuit := make(chan struct{})
	alicePeer.HandleCoopReady(bobChan.ChannelPoint(), aliceQuit)

	// Alice should now respond with a ClosingSigned message with her ideal
	// fee rate.
	var msg lnwire.Message
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
		increasedFee, dummyDeliveryScript, aliceDelivery,
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
		increasedFee, dummyDeliveryScript, aliceDelivery,
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
		aliceFee, dummyDeliveryScript, aliceDelivery,
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
			writeBufferPool := pool.NewWriteBuffer(
				pool.DefaultWriteBufferGCInterval,
				pool.DefaultWriteBufferExpiryInterval,
			)

			writePool := pool.NewWrite(
				writeBufferPool, 1, timeout,
			)
			require.NoError(t, writePool.Start())

			mockConn := newMockConn(t, 1)

			p := Brontide{
				cfg: Config{
					LegacyFeatures: legacy,
					Features:       test.features,
					Conn:           mockConn,
					WritePool:      writePool,
					PongBuf:        make([]byte, lnwire.MaxPongBytes),
				},
				log: peerLog,
			}

			var b bytes.Buffer
			_, err := lnwire.WriteMessage(&b, test.expectedInit, 0)
			require.NoError(t, err)

			// Send our init message, assert that we write our expected message
			// and shutdown our write pool.
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

	// Set up node Alice.
	dbAlice, err := channeldb.Open(t.TempDir())
	require.NoError(t, err)

	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	writeBufferPool := pool.NewWriteBuffer(
		pool.DefaultWriteBufferGCInterval,
		pool.DefaultWriteBufferExpiryInterval,
	)

	writePool := pool.NewWrite(
		writeBufferPool, 1, timeout,
	)
	require.NoError(t, writePool.Start())

	readBufferPool := pool.NewReadBuffer(
		pool.DefaultReadBufferGCInterval,
		pool.DefaultReadBufferExpiryInterval,
	)

	readPool := pool.NewRead(
		readBufferPool, 1, timeout,
	)
	require.NoError(t, readPool.Start())

	mockConn := newMockConn(t, 1)

	receivedCustomChan := make(chan *customMsg)

	remoteKey := [33]byte{8}

	notifier := &mock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}

	alicePeer := NewBrontide(Config{
		PubKeyBytes: remoteKey,
		ChannelDB:   dbAlice.ChannelStateDB(),
		Addr: &lnwire.NetAddress{
			IdentityKey: aliceKey.PubKey(),
		},
		PrunePersistentPeerConnection: func([33]byte) {},
		Features:                      lnwire.EmptyFeatureVector(),
		LegacyFeatures:                lnwire.EmptyFeatureVector(),
		WritePool:                     writePool,
		ReadPool:                      readPool,
		Conn:                          mockConn,
		ChainNotifier:                 notifier,
		HandleCustomMessage: func(
			peer [33]byte, msg *lnwire.Custom) error {

			receivedCustomChan <- &customMsg{
				peer: peer,
				msg:  *msg,
			}
			return nil
		},
		PongBuf: make([]byte, lnwire.MaxPongBytes),
	})

	// Set up the init sequence.
	go func() {
		// Read init message.
		<-mockConn.writtenMessages

		// Write the init reply message.
		initReplyMsg := lnwire.NewInitMessage(
			lnwire.NewRawFeatureVector(
				lnwire.DataLossProtectRequired,
			),
			lnwire.NewRawFeatureVector(),
		)
		var b bytes.Buffer
		_, err = lnwire.WriteMessage(&b, initReplyMsg, 0)
		require.NoError(t, err)

		mockConn.readMessages <- b.Bytes()
	}()

	// Start the peer.
	require.NoError(t, alicePeer.Start())

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
