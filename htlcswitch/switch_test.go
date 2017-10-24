package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
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

	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		chanID2, bobChanID, bobPeer, true,
	)

	s := New(Config{})
	s.Start()
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
		src:        aliceChannelLink.ShortChanID(),
		dest:       bobChannelLink.ShortChanID(),
		obfuscator: newMockObfuscator(),
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
		src:     bobChannelLink.ShortChanID(),
		payHash: rhash,
		amount:  1,
		htlc: &lnwire.UpdateFufillHTLC{
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

	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, true,
	)

	// We'll create a link for Bob, but mark the link as unable to forward
	// any new outgoing HTLC's.
	bobChannelLink := newMockChannelLink(
		chanID2, bobChanID, bobPeer, false,
	)

	s := New(Config{})
	s.Start()
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
		src:  aliceChannelLink.ShortChanID(),
		dest: bobChannelLink.ShortChanID(),
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
	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, false,
	)

	s := New(Config{})
	s.Start()
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

	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		chanID2, bobChanID, bobPeer, true,
	)

	s := New(Config{})
	s.Start()
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
		src:        aliceChannelLink.ShortChanID(),
		dest:       bobChannelLink.ShortChanID(),
		obfuscator: newMockObfuscator(),
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
		src:          bobChannelLink.ShortChanID(),
		payHash:      rhash,
		amount:       1,
		isObfuscated: true,
		htlc:         &lnwire.UpdateFailHTLC{},
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

	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink := newMockChannelLink(
		chanID2, bobChanID, bobPeer, true,
	)

	s := New(Config{})
	s.Start()
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
		src:        aliceChannelLink.ShortChanID(),
		dest:       bobChannelLink.ShortChanID(),
		obfuscator: newMockObfuscator(),
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
		src:          bobChannelLink.ShortChanID(),
		payHash:      rhash,
		amount:       1,
		isObfuscated: true,
		htlc:         &lnwire.UpdateFailHTLC{},
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
	aliceChannelLink := newMockChannelLink(
		chanID1, aliceChanID, alicePeer, true,
	)

	s := New(Config{})
	s.Start()
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

	if s.circuits.pending() != 0 {
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
		src:          aliceChannelLink.ShortChanID(),
		payHash:      rhash,
		amount:       1,
		isObfuscated: true,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
			ID:     1,
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
