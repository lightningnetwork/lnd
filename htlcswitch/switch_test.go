package htlcswitch

import (
	"bytes"
	"crypto/sha256"
	"io/ioutil"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
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

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
		obfuscator:     NewMockObfuscator(),
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
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 1 {
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
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

func TestSwitchForwardFailAfterFullAdd(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	tempPath, err := ioutil.TempDir("", "circuitdb")
	if err != nil {
		t.Fatalf("unable to temporary path: %v", err)
	}

	cdb, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}

	s, err := initSwitchWithDB(cdb)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

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
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(cdb2)
	if err != nil {
		t.Fatalf("unable reinit switch: %v", err)
	}
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, bobPeer, true,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Craft a failure message from the remote peer.
	fail := &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}

	// Send the fail packet from the remote peer through the switch.
	if err := s2.forward(fail); err != nil {
		t.Fatalf(err.Error())
	}

	// Pull packet from alice's link, as it should have gone through
	// successfully.
	select {
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Circuit map should be empty now.
	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Send the fail packet from the remote peer through the switch.
	if err := s2.forward(fail); err == nil {
		t.Fatalf("expected failure when sending duplicate fail " +
			"with no pending circuit")
	}
}

func TestSwitchForwardSettleAfterFullAdd(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	tempPath, err := ioutil.TempDir("", "circuitdb")
	if err != nil {
		t.Fatalf("unable to temporary path: %v", err)
	}

	cdb, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}

	s, err := initSwitchWithDB(cdb)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

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
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(cdb2)
	if err != nil {
		t.Fatalf("unable reinit switch: %v", err)
	}
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, bobPeer, true,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatalf("wrong amount of circuits")
	}

	// Craft a settle message from the remote peer.
	settle := &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFufillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Send the settle packet from the remote peer through the switch.
	if err := s2.forward(settle); err != nil {
		t.Fatalf(err.Error())
	}

	// Pull packet from alice's link, as it should have gone through
	// successfully.
	select {
	case <-aliceChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Circuit map should be empty now.
	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Send the settle packet again, which should fail.
	if err := s2.forward(settle); err == nil {
		t.Fatalf("expected failure when sending duplicate settle " +
			"with no pending circuit")
	}
}

func TestSwitchForwardFailAfterHalfAdd(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	tempPath, err := ioutil.TempDir("", "circuitdb")
	if err != nil {
		t.Fatalf("unable to temporary path: %v", err)
	}

	cdb, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}

	s, err := initSwitchWithDB(cdb)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

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
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Pull packet from bob's link, but do not perform a full add.
	select {
	case <-bobChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(cdb2)
	if err != nil {
		t.Fatalf("unable reinit switch: %v", err)
	}
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, bobPeer, true,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Resend the failed htlc, it should be returned to alice since the
	// switch will detect that it has been half added previously.
	if err := s2.forward(ogPacket); err != ErrDuplicateAdd {
		t.Fatal("unexpected error when reforwarding a "+
			"failed packet", err)
	}
}

// TestSwitchForwardCircuitPersistence checks the ability of htlc switch to
// maintain the proper entries in the circuit map in the face of restarts.
func TestSwitchForwardCircuitPersistence(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	tempPath, err := ioutil.TempDir("", "circuitdb")
	if err != nil {
		t.Fatalf("unable to temporary path: %v", err)
	}

	cdb, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}

	s, err := initSwitchWithDB(cdb)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}

	// Even though we intend to Stop s later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s.Stop()

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
	ogPacket := &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	if s.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}

	// Retrieve packet from outgoing link and cache until after restart.
	var packet *htlcPacket
	select {
	case packet = <-bobChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if err := s.Stop(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(cdb2)
	if err != nil {
		t.Fatalf("unable reinit switch: %v", err)
	}
	if err := s2.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}

	// Even though we intend to Stop s2 later in the test, it is safe to
	// defer this Stop since its execution it is protected by an atomic
	// guard, guaranteeing it executes at most once.
	defer s2.Stop()

	aliceChannelLink = newMockChannelLink(
		s2, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink = newMockChannelLink(
		s2, chanID2, bobChanID, bobPeer, true,
	)
	if err := s2.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s2.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}

	// Now that the switch has restarted, complete the payment circuit.
	if err := bobChannelLink.completeCircuit(packet); err != nil {
		t.Fatalf("unable to complete payment circuit: %v", err)
	}

	if s2.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s2.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create settle request pretending that bob link handled the add htlc
	// request and sent the htlc settle request back. This request should
	// be forwarder back to Alice link.
	ogPacket = &htlcPacket{
		outgoingChanID: bobChannelLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFufillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s2.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	select {
	case packet = <-aliceChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s2.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits, want 1, got %d",
			s2.circuits.NumPending())
	}
	if s2.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}

	if err := s2.Stop(); err != nil {
		t.Fatal(err)
	}

	cdb3, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s3, err := initSwitchWithDB(cdb3)
	if err != nil {
		t.Fatalf("unable reinit switch: %v", err)
	}
	if err := s3.Start(); err != nil {
		t.Fatalf("unable to restart switch: %v", err)
	}
	defer s3.Stop()

	aliceChannelLink = newMockChannelLink(
		s3, chanID1, aliceChanID, alicePeer, true,
	)
	bobChannelLink = newMockChannelLink(
		s3, chanID2, bobChanID, bobPeer, true,
	)
	if err := s3.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s3.AddLink(bobChannelLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	if s3.circuits.NumPending() != 0 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s3.circuits.NumOpen() != 0 {
		t.Fatalf("wrong amount of circuits")
	}
}

// TestSkipIneligibleLinksMultiHopForward tests that if a multi-hop HTLC comes
// along, then we won't attempt to froward it down al ink that isn't yet able
// to forward any HTLC's.
func TestSkipIneligibleLinksMultiHopForward(t *testing.T) {
	t.Parallel()

	var packet *htlcPacket

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
		obfuscator: NewMockObfuscator(),
	}

	// The request to forward should fail as
	err = s.forward(packet)
	if err == nil {
		t.Fatalf("forwarding should have failed due to inactive link")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSkipIneligibleLinksLocalForward ensures that the switch will not attempt
// to forward any HTLC's down a link that isn't yet eligible for forwarding.
func TestSkipIneligibleLinksLocalForward(t *testing.T) {
	t.Parallel()

	// We'll create a single link for this test, marking it as being unable
	// to forward form the get go.
	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
	_, err = s.SendHTLC(alicePub, addMsg, nil)
	if err == nil {
		t.Fatalf("local forward should fail due to inactive link")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchCancel checks that if htlc was rejected we remove unused
// circuits.
func TestSwitchCancel(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
		obfuscator:     NewMockObfuscator(),
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
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumPending() != 1 {
		t.Fatalf("wrong amount of half circuits")
	}
	if s.circuits.NumOpen() != 1 {
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
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumPending() != 0 {
		t.Fatal("wrong amount of circuits")
	}
	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchAddSamePayment tests that we send the payment with the same
// payment hash.
func TestSwitchAddSamePayment(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", nil)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
		obfuscator:     NewMockObfuscator(),
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
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	request = &htlcPacket{
		incomingChanID: aliceChannelLink.ShortChanID(),
		incomingHTLCID: 1,
		outgoingChanID: bobChannelLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
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
	case packet := <-bobChannelLink.packets:
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.circuits.NumOpen() != 2 {
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
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 1 {
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
	case pkt := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}

	case <-time.After(time.Second):
		t.Fatal("request was not propagated to channelPoint")
	}

	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}
}

// TestSwitchSendPayment tests ability of htlc switch to respond to the
// users when response is came back from channel link.
func TestSwitchSendPayment(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", nil)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

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
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case err := <-errChan:
		t.Fatalf("unable to send payment: %v", err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	select {
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case err := <-errChan:
		t.Fatalf("unable to send payment: %v", err)
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	if s.numPendingPayments() != 2 {
		t.Fatal("wrong amount of pending payments")
	}

	if s.circuits.NumOpen() != 2 {
		t.Fatal("wrong amount of circuits")
	}

	// Create fail request pretending that bob channel link handled
	// the add htlc request with error and sent the htlc fail request
	// back. This request should be forwarded back to alice channel link.
	obfuscator := NewMockObfuscator()
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
