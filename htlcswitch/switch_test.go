package htlcswitch

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/ticker"
)

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}

// TestSwitchAddDuplicateLink tests that the switch will reject duplicate links
// for both pending and live links. It also tests that we can successfully
// add a link after having removed it.
func TestSwitchAddDuplicateLink(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	pendingChanID := lnwire.ShortChannelID{}

	aliceChannelLink := newMockChannelLink(
		s, chanID1, pendingChanID, alicePeer, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	// Alice should have a pending link, adding again should fail.
	if err := s.AddLink(aliceChannelLink); err == nil {
		t.Fatalf("adding duplicate link should have failed")
	}

	// Update the short chan id of the channel, so that the link goes live.
	aliceChannelLink.setLiveShortChanID(aliceChanID)
	err = s.UpdateShortChanID(chanID1)
	if err != nil {
		t.Fatalf("unable to update alice short_chan_id: %v", err)
	}

	// Alice should have a live link, adding again should fail.
	if err := s.AddLink(aliceChannelLink); err == nil {
		t.Fatalf("adding duplicate link should have failed")
	}

	// Remove the live link to ensure the indexes are cleared.
	s.RemoveLink(chanID1)

	// Alice has no links, adding should succeed.
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
}

// TestSwitchHasActiveLink tests the behavior of HasActiveLink, and asserts that
// it only returns true if a link's short channel id has confirmed (meaning the
// channel is no longer pending) and it's EligibleToForward method returns true,
// i.e. it has received FundingLocked from the remote peer.
func TestSwitchHasActiveLink(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	pendingChanID := lnwire.ShortChannelID{}

	aliceChannelLink := newMockChannelLink(
		s, chanID1, pendingChanID, alicePeer, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	// The link has been added, but it's still pending. HasActiveLink should
	// return false since the link has not been added to the linkIndex
	// containing live links.
	if s.HasActiveLink(chanID1) {
		t.Fatalf("link should not be active yet, still pending")
	}

	// Update the short chan id of the channel, so that the link goes live.
	aliceChannelLink.setLiveShortChanID(aliceChanID)
	err = s.UpdateShortChanID(chanID1)
	if err != nil {
		t.Fatalf("unable to update alice short_chan_id: %v", err)
	}

	// UpdateShortChanID will cause the mock link to become eligible to
	// forward. However, we can simulate the event where the short chan id
	// is confirmed, but funding locked has yet to be received by resetting
	// the mock link's eligibility to false.
	aliceChannelLink.eligible = false

	// Now, even though the link has been added to the linkIndex because the
	// short channel id has confirmed, we should still see HasActiveLink
	// fail because EligibleToForward should return false.
	if s.HasActiveLink(chanID1) {
		t.Fatalf("link should not be active yet, still ineligible")
	}

	// Finally, simulate the link receiving funding locked by setting its
	// eligibility to true.
	aliceChannelLink.eligible = true

	// The link should now be reported as active, since EligibleToForward
	// returns true and the link is in the linkIndex.
	if !s.HasActiveLink(chanID1) {
		t.Fatalf("link should not be active now")
	}
}

// TestSwitchSendPending checks the inability of htlc switch to forward adds
// over pending links, and the UpdateShortChanID makes a pending link live.
func TestSwitchSendPending(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, bobChanID := genIDs()

	pendingChanID := lnwire.ShortChannelID{}

	aliceChannelLink := newMockChannelLink(
		s, chanID1, pendingChanID, alicePeer, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	// Create request which should is being forwarded from Bob channel
	// link to Alice channel link.
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := fastsha256.Sum256(preimage[:])
	packet := &htlcPacket{
		incomingChanID: bobChanID,
		incomingHTLCID: 0,
		outgoingChanID: aliceChanID,
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}

	// Send the ADD packet, this should not be forwarded out to the link
	// since there are no eligible links.
	err = s.forward(packet)
	expErr := fmt.Sprintf("unable to find link with destination %v",
		aliceChanID)
	if err != nil && err.Error() != expErr {
		t.Fatalf("expected forward failure: %v", err)
	}

	// No message should be sent, since the packet was failed.
	select {
	case <-aliceChannelLink.packets:
		t.Fatal("expected not to receive message")
	case <-time.After(time.Second):
	}

	// Since the packet should have been failed, there should be no active
	// circuits.
	if s.circuits.NumOpen() != 0 {
		t.Fatal("wrong amount of circuits")
	}

	// Now, update Alice's link with her final short channel id. This should
	// move the link to the live state.
	aliceChannelLink.setLiveShortChanID(aliceChanID)
	err = s.UpdateShortChanID(chanID1)
	if err != nil {
		t.Fatalf("unable to update alice short_chan_id: %v", err)
	}

	// Increment the packet's HTLC index, so that it does not collide with
	// the prior attempt.
	packet.incomingHTLCID++

	// Handle the request and checks that bob channel link received it.
	if err := s.forward(packet); err != nil {
		t.Fatalf("unexpected forward failure: %v", err)
	}

	// Since Alice's link is now active, this packet should succeed.
	select {
	case <-aliceChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to alice")
	}
}

// TestSwitchForward checks the ability of htlc switch to forward add/settle
// requests.
func TestSwitchForward(t *testing.T) {
	t.Parallel()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

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
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
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
		if err := aliceChannelLink.deleteCircuit(pkt); err != nil {
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

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
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

	s, err := initSwitchWithDB(testStartingHeight, cdb)
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

	if err := cdb.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
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

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
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

	s, err := initSwitchWithDB(testStartingHeight, cdb)
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

	if err := cdb.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
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
		htlc: &lnwire.UpdateFulfillHTLC{
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
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete circuit with in key=%s: %v",
				packet.inKey(), err)
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

	// Send the settle packet again, which should fail.
	if err := s2.forward(settle); err == nil {
		t.Fatalf("expected failure when sending duplicate settle " +
			"with no pending circuit")
	}
}

func TestSwitchForwardDropAfterFullAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
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

	s, err := initSwitchWithDB(testStartingHeight, cdb)
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
	case packet := <-bobChannelLink.packets:
		// Complete the payment circuit and assign the outgoing htlc id
		// before restarting.
		if err := bobChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}

	// Now we will restart bob, leaving the forwarding decision for this
	// htlc is in the half-added state.
	if err := s.Stop(); err != nil {
		t.Fatalf(err.Error())
	}

	if err := cdb.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
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
		t.Fatalf("wrong amount of half circuits")
	}

	// Resend the failed htlc, it should be returned to alice since the
	// switch will detect that it has been half added previously.
	err = s2.forward(ogPacket)
	if err != ErrDuplicateAdd {
		t.Fatal("unexpected error when reforwarding a "+
			"failed packet", err)
	}

	// After detecting an incomplete forward, the fail packet should have
	// been returned to the sender.
	select {
	case <-aliceChannelLink.packets:
		t.Fatal("request should not have returned to source")
	case <-bobChannelLink.packets:
		t.Fatal("request should not have forwarded to destination")
	case <-time.After(time.Second):
	}
}

func TestSwitchForwardFailAfterHalfAdd(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
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

	s, err := initSwitchWithDB(testStartingHeight, cdb)
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

	if err := cdb.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
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
	err = s2.forward(ogPacket)
	if err != ErrIncompleteForward {
		t.Fatal("unexpected error when reforwarding a "+
			"failed packet", err)
	}

	// After detecting an incomplete forward, the fail packet should have
	// been returned to the sender.
	select {
	case <-aliceChannelLink.packets:
	case <-time.After(time.Second):
		t.Fatal("request was not propagated to destination")
	}
}

// TestSwitchForwardCircuitPersistence checks the ability of htlc switch to
// maintain the proper entries in the circuit map in the face of restarts.
func TestSwitchForwardCircuitPersistence(t *testing.T) {
	t.Parallel()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
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

	s, err := initSwitchWithDB(testStartingHeight, cdb)
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

	if err := cdb.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb2, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s2, err := initSwitchWithDB(testStartingHeight, cdb2)
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
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}

	// Handle the request and checks that payment circuit works properly.
	if err := s2.forward(ogPacket); err != nil {
		t.Fatal(err)
	}

	select {
	case packet = <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete circuit with in key=%s: %v",
				packet.inKey(), err)
		}
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

	if err := cdb2.Close(); err != nil {
		t.Fatalf(err.Error())
	}

	cdb3, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to reopen channeldb: %v", err)
	}

	s3, err := initSwitchWithDB(testStartingHeight, cdb3)
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

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

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
	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, false,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := fastsha256.Sum256(preimage[:])
	addMsg := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	// We'll attempt to send out a new HTLC that has Alice as the first
	// outgoing link. This should fail as Alice isn't yet able to forward
	// any active HTLC's.
	_, err = s.SendHTLC(aliceChannelLink.ShortChanID(), addMsg, nil)
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

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

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
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
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

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(t, "bob", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
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
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
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

	alicePeer, err := newMockServer(t, "alice", testStartingHeight, nil, 6)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}

	s, err := initSwitchWithDB(testStartingHeight, nil)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	defer s.Stop()

	chanID1, _, aliceChanID, _ := genIDs()

	aliceChannelLink := newMockChannelLink(
		s, chanID1, aliceChanID, alicePeer, true,
	)
	if err := s.AddLink(aliceChannelLink); err != nil {
		t.Fatalf("unable to add link: %v", err)
	}

	// Create request which should be forwarder from alice channel link
	// to bob channel link.
	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := fastsha256.Sum256(preimage[:])
	update := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	// Handle the request and checks that bob channel link received it.
	errChan := make(chan error)
	go func() {
		_, err := s.SendHTLC(
			aliceChannelLink.ShortChanID(), update,
			newMockDeobfuscator())
		errChan <- err
	}()

	go func() {
		// Send the payment with the same payment hash and same
		// amount and check that it will be propagated successfully
		_, err := s.SendHTLC(
			aliceChannelLink.ShortChanID(), update,
			newMockDeobfuscator(),
		)
		errChan <- err
	}()

	select {
	case packet := <-aliceChannelLink.packets:
		if err := aliceChannelLink.completeCircuit(packet); err != nil {
			t.Fatalf("unable to complete payment circuit: %v", err)
		}

	case err := <-errChan:
		if err != ErrPaymentInFlight {
			t.Fatalf("unable to send payment: %v", err)
		}
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

	if s.numPendingPayments() != 1 {
		t.Fatal("wrong amount of pending payments")
	}

	if s.circuits.NumOpen() != 1 {
		t.Fatal("wrong amount of circuits")
	}

	// Create fail request pretending that bob channel link handled
	// the add htlc request with error and sent the htlc fail request
	// back. This request should be forwarded back to alice channel link.
	obfuscator := NewMockObfuscator()
	failure := lnwire.NewFailUnknownPaymentHash(update.Amount)
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
		if !strings.Contains(err.Error(), lnwire.CodeUnknownPaymentHash.String()) {
			t.Fatalf("expected %v got %v", err,
				lnwire.CodeUnknownPaymentHash)
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
	firstHop := n.firstBobChannelLink.ShortChanID()
	_, err = makePayment(
		n.aliceServer, receiver, firstHop, hops, amount, htlcAmt,
		totalTimelock,
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
	firstHop := n.firstBobChannelLink.ShortChanID()
	for i := 0; i < numPayments/2; i++ {
		_, err := makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, finalAmt,
			htlcAmt, totalTimelock,
		).Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	bobLog, ok := n.bobServer.htlcSwitch.cfg.FwdingLog.(*mockForwardingLog)
	if !ok {
		t.Fatalf("mockForwardingLog assertion failed")
	}

	// After sending 5 of the payments, trigger the forwarding ticker, to
	// make sure the events are properly flushed.
	bobTicker, ok := n.bobServer.htlcSwitch.cfg.FwdEventTicker.(*ticker.Mock)
	if !ok {
		t.Fatalf("mockTicker assertion failed")
	}

	// We'll trigger the ticker, and wait for the events to appear in Bob's
	// forwarding log.
	timeout := time.After(15 * time.Second)
	for {
		select {
		case bobTicker.Force <- time.Now():
		case <-time.After(1 * time.Second):
			t.Fatalf("unable to force tick")
		}

		// If all 5 events is found in Bob's log, we can break out and
		// continue the test.
		bobLog.Lock()
		if len(bobLog.events) == 5 {
			bobLog.Unlock()
			break
		}
		bobLog.Unlock()

		// Otherwise wait a little bit before checking again.
		select {
		case <-time.After(50 * time.Millisecond):
		case <-timeout:
			bobLog.Lock()
			defer bobLog.Unlock()
			t.Fatalf("expected 5 events in event log, instead "+
				"found: %v", spew.Sdump(bobLog.events))
		}
	}

	// Send the remaining payments.
	for i := numPayments / 2; i < numPayments; i++ {
		_, err := makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, finalAmt,
			htlcAmt, totalTimelock,
		).Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

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
