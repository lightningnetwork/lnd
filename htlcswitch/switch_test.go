package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	hash1, _ = chainhash.NewHash(bytes.Repeat([]byte("a"), 32))
	hash2, _ = chainhash.NewHash(bytes.Repeat([]byte("b"), 32))

	chanPoint1 = wire.NewOutPoint(hash1, 0)
	chanPoint2 = wire.NewOutPoint(hash2, 0)

	chanID1 = lnwire.NewChanIDFromOutPoint(chanPoint1)
	chanID2 = lnwire.NewChanIDFromOutPoint(chanPoint2)

	aliceChanID = lnwire.NewShortChanIDFromInt(1)
	bobChanID   = lnwire.NewShortChanIDFromInt(2)
)

// TestSwitchForward checks the ability of htlc switch to forward add/settle
// requests.
func TestSwitchForward(t *testing.T) {
	t.Parallel()

	alicePeer := newMockServer(t, "alice")
	bobPeer := newMockServer(t, "bob")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, bobPeer, true,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarded from Alice channel link to
	// bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	packet := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     newMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(packet); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.pending() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob link handled the add htlc
	// request and sent the htlc settle request back. This request should
	// be forwarder back to Alice link.
	packet = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.forward(packet); err != nil {
		t.Fatal(err)
	}

	select {
	case <-aliceChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.pending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSkipIneligibleLinksMultiHopForward tests that if a multi-hop HTLC comes
// along, then we won't attempt to froward it down al ink that isn't yet able
// to forward any HTLC's.
func TestSkipIneligibleLinksMultiHopForward(t *testing.T) {
	t.Parallel()

	var packet *htlcPacket

	alicePeer := newMockServer(t, "alice")
	bobPeer := newMockServer(t, "bob")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)

	// We'll create a link for Bob, but mark the link as unable to forward
	// any new outgoing HTLC's.
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, bobPeer, false,
	)

	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create a new packet that's destined for Bob as an incoming HTLC from
	// Alice.
	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	packet = &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
		obfuscator: newMockObfuscator(),
	}

	// The request to forward should fail as
	err := s.forward(packet)
	if err == nil {
		t.Fatalf("forwarding should have failed due to inactive link")
	}

	if s.circuits.pending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSkipIneligibleLinksLocalForward ensures that the switch will not attempt
// to forward any HTLC's down a link that isn't yet eligible for forwarding.
func TestSkipIneligibleLinksLocalForward(t *testing.T) {
	t.Parallel()

	// We'll create a single link for this test, marking it as being unable
	// to forward form the get go.
	alicePeer := newMockServer(t, "alice")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	addMsg := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	// We'll attempt to send out a new HTLC that has Alice as the first
	// outgoing link. This should fail as Alice isn't yet able to forward
	// any active HTLC's.
	alicePub := aliceChannelLink.Peer().PubKey()
	_, err := s.SendHTLC(alicePub, addMsg, nil)
	if err == nil {
		t.Fatalf("local forward should fail due to inactive link")
	}

	if s.circuits.pending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchCancel checks that if htlc was rejected we remove unused
// circuits.
func TestSwitchCancel(t *testing.T) {
	t.Parallel()

	alicePeer := newMockServer(t, "alice")
	bobPeer := newMockServer(t, "bob")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, bobPeer, true,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	request := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     newMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.pending() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob channel link handled
	// the add htlc request and sent the htlc settle request back. This
	// request should be forwarder back to alice channel link.
	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	select {
	case <-aliceChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.pending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchAddSamePayment tests that we send the payment with the same
// payment hash.
func TestSwitchAddSamePayment(t *testing.T) {
	t.Parallel()

	alicePeer := newMockServer(t, "alice")
	bobPeer := newMockServer(t, "bob")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		s, chanID2, bobChanID, bobPeer, true,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	request := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     newMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.pending() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	request = &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 1,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     newMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	if s.circuits.pending() != 2 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob channel link handled
	// the add htlc request and sent the htlc settle request back. This
	// request should be forwarder back to alice channel link.
	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	select {
	case <-aliceChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.pending() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	request = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 1,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s.forward(request); err != nil {
		t.Fatal(err)
	}

	select {
	case <-aliceChannelLink.packets:
		break
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.pending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchSendPayment tests ability of htlc switch to respond to the
// users when response is came back from channel link.
func TestSwitchSendPayment(t *testing.T) {
	t.Parallel()

	alicePeer := newMockServer(t, "alice")

	s := New(Config{
		FwdingLog: &mockForwardingLog{
			events: make(map[time.Time]channeldb.ForwardingEvent),
		},
	})
	s.Start()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage := [sha256.Size]byte{1}
	rhash := fastsha256.Sum256(preimage[:])
	update := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	// Handle the request and checks that bob channel link received it.
	errChan := make(chan error)
	go func() {
		_, err := s.SendHTLC(aliceChannelLink.Peer().PubKey(), update,
			newMockDeobfuscator())
		errChan <- err
	}()

	go func() {
		// Send the payment with the same payment hash and same
		// amount and check that it will be propagated successfully
		_, err := s.SendHTLC(aliceChannelLink.Peer().PubKey(), update,
			newMockDeobfuscator())
		errChan <- err
	}()

	select {
	case <-aliceChannelLink.packets:
		break
	case err := <-errChan:
		t.Fatalf("unable to send payment: %v", err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	select {
	case <-aliceChannelLink.packets:
		break
	case err := <-errChan:
		t.Fatalf("unable to send payment: %v", err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.numPendingPayments() != 2 {
		t.Fatal("wrong amount of pending payments")
	}

	if s.circuits.pending() != 2 {
		t.Fatal("wrong amount of circuits")
	}

	// Create fail request pretending that bob channel link handled
	// the add htlc request with error and sent the htlc fail request
	// back. This request should be forwarded back to alice channel link.
	obfuscator := newMockObfuscator()
	failure := lnwire.FailIncorrectPaymentAmount{}
	reason, err := obfuscator.EncryptFirstHop(failure)
	if err != nil {
		t.Fatalf("unable obfuscate failure: %v", err)
	}

	packet := &htlcPacket{
		outgoingChanID: aliceChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}

	if err := s.forward(packet); err != nil {
		t.Fatalf("can't forward htlc packet: %v", err)
	}

	select {
	case err := <-errChan:
		if err.Error() != errors.New(lnwire.CodeIncorrectPaymentAmount).Error() {
			t.Fatal("err wasn't received")
		}
	case <-time.After(time.Second):
		t.Fatal("err wasn't received")
	}

	packet = &htlcPacket{
		outgoingChanID: aliceChannelLink.ShortChanID(),
		outgoingHTLCID: 1,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}

	// Send second failure response and check that user were able to
	// receive the error.
	if err := s.forward(packet); err != nil {
		t.Fatalf("can't forward htlc packet: %v", err)
	}

	select {
	case err := <-errChan:
		if err.Error() != errors.New(lnwire.CodeIncorrectPaymentAmount).Error() {
			t.Fatal("err wasn't received")
		}
	case <-time.After(time.Second):
		t.Fatal("err wasn't received")
	}

	if s.numPendingPayments() != 0 {
		t.Fatal("wrong amount of pending payments")
	}
}

// TestLocalPaymentNoForwardingEvents tests that if we send a series of locally
// initiated payments, then they aren't reflected in the forwarding log.
func TestLocalPaymentNoForwardingEvents(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network. We'll only be
	// interacting with and asserting the state of the first end point for
	// this test.
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}

	// We'll now craft and send a payment from Alice to Bob.
	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.firstBobChannelLink,
	)

	// With the payment crafted, we'll send it from Alice to Bob. We'll
	// wait for Alice to receive the preimage for the payment before
	// proceeding.
	receiver := n.bobServer
	_, err = n.makePayment(
		n.aliceServer, receiver, n.bobServer.PubKey(), hops, amount,
		htlcAmt, totalTimelock,
	).Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}

	// At this point, we'll forcibly stop the three hop network. Doing
	// this will cause any pending forwarding events to be flushed by the
	// various switches in the network.
	n.stop()

	// With all the switches stopped, we'll fetch Alice's mock forwarding
	// event log.
	log, ok := n.aliceServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	log.Lock()
	defer log.Unlock()

	// If we examine the memory of the forwarding log, then it should be
	// blank.
	if len(log.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(log.events))
	}
}

// TestMultiHopPaymentForwardingEvents tests that if we send a series of
// multi-hop payments via Alice->Bob->Carol. Then Bob properly logs forwarding
// events, while Alice and Carol don't.
func TestMultiHopPaymentForwardingEvents(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network.
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}

	// We'll make now 10 payments, of 100k satoshis each from Alice to
	// Carol via Bob.
	const numPayments = 10
	finalAmt := lnwire.NewMSatFromSatoshis(100000)
	htlcAmt, totalTimelock, hops := generateHops(
		finalAmt, testStartingHeight, n.firstBobChannelLink,
		n.carolChannelLink,
	)
	for i := 0; i < numPayments; i++ {
		_, err := n.makePayment(
			n.aliceServer, n.carolServer, n.bobServer.PubKey(),
			hops, finalAmt, htlcAmt, totalTimelock,
		).Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	time.Sleep(time.Millisecond * 200)

	// With all 10 payments sent. We'll now manually stop each of the
	// switches so we can examine their end state.
	n.stop()

	// Alice and Carol shouldn't have any recorded forwarding events, as
	// they were the source and the sink for these payment flows.
	aliceLog, ok := n.aliceServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	aliceLog.Lock()
	defer aliceLog.Unlock()
	if len(aliceLog.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(aliceLog.events))
	}

	carolLog, ok := n.carolServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	carolLog.Lock()
	defer carolLog.Unlock()
	if len(carolLog.events) != 0 {
		t.Fatalf("log should have no events, instead has: %v",
			spew.Sdump(carolLog.events))
	}

	// Bob on the other hand, should have 10 events.
	bobLog, ok := n.bobServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}
	bobLog.Lock()
	defer bobLog.Unlock()
	if len(bobLog.events) != 10 {
		t.Fatalf("log should have 10 events, instead has: %v",
			spew.Sdump(bobLog.events))
	}

	// Each of the 10 events should have had all fields set properly.
	for _, event := range bobLog.events {
		// The incoming and outgoing channels should properly be set for
		// the event.
		if event.IncomingChanID != n.aliceChannelLink.ShortChanID() {
			t.Fatalf("chan id mismatch: expected %v, got %v",
				event.IncomingChanID,
				n.aliceChannelLink.ShortChanID())
		}
		if event.OutgoingChanID != n.carolChannelLink.ShortChanID() {
			t.Fatalf("chan id mismatch: expected %v, got %v",
				event.OutgoingChanID,
				n.carolChannelLink.ShortChanID())
		}

		// Additionally, the incoming and outgoing amounts should also
		// be properly set.
		if event.AmtIn != htlcAmt {
			t.Fatalf("incoming amt mismatch: expected %v, got %v",
				event.AmtIn, htlcAmt)
		}
		if event.AmtOut != finalAmt {
			t.Fatalf("outgoing amt mismatch: expected %v, got %v",
				event.AmtOut, finalAmt)
		}
	}
}
