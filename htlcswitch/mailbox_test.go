package htlcswitch

import (
	prand "math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

// TestMailBoxCouriers tests that both aspects of the mailBox struct works
// properly. Both packets and messages should be able to added to each
// respective mailbox concurrently, and also messages/packets should also be
// able to be received concurrently.
func TestMailBoxCouriers(t *testing.T) {
	t.Parallel()

	// First, we'll create new instance of the current default mailbox
	// type.
	mailBox := newMemoryMailBox(&mailBoxConfig{
		clock:  clock.NewDefaultClock(),
		expiry: time.Minute,
	})
	mailBox.Start()
	defer mailBox.Stop()

	// We'll be adding 10 message of both types to the mailbox.
	const numPackets = 10
	const halfPackets = numPackets / 2

	// We'll add a set of random packets to the mailbox.
	sentPackets := make([]*htlcPacket, numPackets)
	for i := 0; i < numPackets; i++ {
		pkt := &htlcPacket{
			outgoingChanID: lnwire.NewShortChanIDFromInt(uint64(prand.Int63())),
			incomingChanID: lnwire.NewShortChanIDFromInt(uint64(prand.Int63())),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
			htlc: &lnwire.UpdateAddHTLC{
				ID: uint64(i),
			},
		}
		sentPackets[i] = pkt

		mailBox.AddPacket(pkt)
	}

	// Next, we'll do the same, but this time adding wire messages.
	sentMessages := make([]lnwire.Message, numPackets)
	for i := 0; i < numPackets; i++ {
		msg := &lnwire.UpdateAddHTLC{
			ID:     uint64(prand.Int63()),
			Amount: lnwire.MilliSatoshi(prand.Int63()),
		}
		sentMessages[i] = msg

		mailBox.AddMessage(msg)
	}

	// Now we'll attempt to read back the packets/messages we added to the
	// mailbox. We'll alternative reading from the message outbox vs the
	// packet outbox to ensure that they work concurrently properly.
	recvdPackets := make([]*htlcPacket, 0, numPackets)
	recvdMessages := make([]lnwire.Message, 0, numPackets)
	for i := 0; i < numPackets*2; i++ {
		timeout := time.After(time.Second * 5)
		if i%2 == 0 {
			select {
			case <-timeout:
				t.Fatalf("didn't recv pkt after timeout")
			case pkt := <-mailBox.PacketOutBox():
				recvdPackets = append(recvdPackets, pkt)
			}
		} else {
			select {
			case <-timeout:
				t.Fatalf("didn't recv message after timeout")
			case msg := <-mailBox.MessageOutBox():
				recvdMessages = append(recvdMessages, msg)
			}
		}
	}

	// The number of messages/packets we sent, and the number we received
	// should match exactly.
	if len(sentPackets) != len(recvdPackets) {
		t.Fatalf("expected %v packets instead got %v", len(sentPackets),
			len(recvdPackets))
	}
	if len(sentMessages) != len(recvdMessages) {
		t.Fatalf("expected %v messages instead got %v", len(sentMessages),
			len(recvdMessages))
	}

	// Additionally, the set of packets should match exactly, as we should
	// have received the packets in the exact same ordering that we added.
	if !reflect.DeepEqual(sentPackets, recvdPackets) {
		t.Fatalf("recvd packets mismatched: expected %v, got %v",
			spew.Sdump(sentPackets), spew.Sdump(recvdPackets))
	}
	if !reflect.DeepEqual(recvdMessages, recvdMessages) {
		t.Fatalf("recvd messages mismatched: expected %v, got %v",
			spew.Sdump(sentMessages), spew.Sdump(recvdMessages))
	}

	// Now that we've received all of the intended msgs/pkts, ack back half
	// of the packets.
	for _, recvdPkt := range recvdPackets[:halfPackets] {
		mailBox.AckPacket(recvdPkt.inKey())
	}

	// With the packets drained and partially acked,  we reset the mailbox,
	// simulating a link shutting down and then coming back up.
	mailBox.ResetMessages()
	mailBox.ResetPackets()

	// Now, we'll use the same alternating strategy to read from our
	// mailbox. All wire messages are dropped on startup, but any unacked
	// packets will be replayed in the same order they were delivered
	// initially.
	recvdPackets2 := make([]*htlcPacket, 0, halfPackets)
	for i := 0; i < 2*halfPackets; i++ {
		timeout := time.After(time.Second * 5)
		if i%2 == 0 {
			select {
			case <-timeout:
				t.Fatalf("didn't recv pkt after timeout")
			case pkt := <-mailBox.PacketOutBox():
				recvdPackets2 = append(recvdPackets2, pkt)
			}
		} else {
			select {
			case <-mailBox.MessageOutBox():
				t.Fatalf("should not receive wire msg after reset")
			default:
			}
		}
	}

	// The number of packets we received should match the number of unacked
	// packets left in the mailbox.
	if halfPackets != len(recvdPackets2) {
		t.Fatalf("expected %v packets instead got %v", halfPackets,
			len(recvdPackets))
	}

	// Additionally, the set of packets should match exactly with the
	// unacked packets, and we should have received the packets in the exact
	// same ordering that we added.
	if !reflect.DeepEqual(recvdPackets[halfPackets:], recvdPackets2) {
		t.Fatalf("recvd packets mismatched: expected %v, got %v",
			spew.Sdump(sentPackets), spew.Sdump(recvdPackets))
	}
}

// TestMailBoxResetAfterShutdown tests that ResetMessages and ResetPackets
// return ErrMailBoxShuttingDown after the mailbox has been stopped.
func TestMailBoxResetAfterShutdown(t *testing.T) {
	t.Parallel()

	m := newMemoryMailBox(&mailBoxConfig{})
	m.Start()

	// Stop the mailbox, then try to reset the message and packet couriers.
	m.Stop()

	err := m.ResetMessages()
	if err != ErrMailBoxShuttingDown {
		t.Fatalf("expected ErrMailBoxShuttingDown, got: %v", err)
	}

	err = m.ResetPackets()
	if err != ErrMailBoxShuttingDown {
		t.Fatalf("expected ErrMailBoxShuttingDown, got: %v", err)
	}
}

type mailboxContext struct {
	t        *testing.T
	clock    *clock.TestClock
	mailbox  MailBox
	forwards chan *htlcPacket
}

func newMailboxContext(t *testing.T, startTime time.Time,
	expiry time.Duration) *mailboxContext {

	ctx := &mailboxContext{
		t:        t,
		clock:    clock.NewTestClock(startTime),
		forwards: make(chan *htlcPacket, 1),
	}
	ctx.mailbox = newMemoryMailBox(&mailBoxConfig{
		fetchUpdate: func(sid lnwire.ShortChannelID) (
			*lnwire.ChannelUpdate, error) {
			return &lnwire.ChannelUpdate{
				ShortChannelID: sid,
			}, nil
		},
		forwardPackets: ctx.forward,
		clock:          ctx.clock,
		expiry:         expiry,
	})
	ctx.mailbox.Start()

	return ctx
}

func (c *mailboxContext) forward(_ chan struct{},
	pkts ...*htlcPacket) chan error {

	for _, pkt := range pkts {
		c.forwards <- pkt
	}

	errChan := make(chan error)
	close(errChan)

	return errChan
}

func (c *mailboxContext) sendAdds(start, num int) []*htlcPacket {
	c.t.Helper()

	sentPackets := make([]*htlcPacket, num)
	for i := 0; i < num; i++ {
		pkt := &htlcPacket{
			outgoingChanID: lnwire.NewShortChanIDFromInt(
				uint64(prand.Int63())),
			incomingChanID: lnwire.NewShortChanIDFromInt(
				uint64(prand.Int63())),
			incomingHTLCID: uint64(start + i),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
			htlc: &lnwire.UpdateAddHTLC{
				ID: uint64(start + i),
			},
		}
		sentPackets[i] = pkt

		err := c.mailbox.AddPacket(pkt)
		if err != nil {
			c.t.Fatalf("unable to add packet: %v", err)
		}
	}

	return sentPackets
}

func (c *mailboxContext) receivePkts(pkts []*htlcPacket) {
	c.t.Helper()

	for i, expPkt := range pkts {
		select {
		case pkt := <-c.mailbox.PacketOutBox():
			if reflect.DeepEqual(expPkt, pkt) {
				continue
			}

			c.t.Fatalf("inkey mismatch #%d, want: %v vs "+
				"got: %v", i, expPkt.inKey(), pkt.inKey())

		case <-time.After(50 * time.Millisecond):
			c.t.Fatalf("did not receive fail for index %d", i)
		}
	}
}

func (c *mailboxContext) checkFails(adds []*htlcPacket) {
	c.t.Helper()

	for i, add := range adds {
		select {
		case fail := <-c.forwards:
			if add.inKey() == fail.inKey() {
				continue
			}
			c.t.Fatalf("inkey mismatch #%d, add: %v vs fail: %v",
				i, add.inKey(), fail.inKey())

		case <-time.After(50 * time.Millisecond):
			c.t.Fatalf("did not receive fail for index %d", i)
		}
	}

	select {
	case pkt := <-c.forwards:
		c.t.Fatalf("unexpected forward: %v", pkt)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestMailBoxFailAdd asserts that FailAdd returns a response to the switch
// under various interleavings with other operations on the mailbox.
func TestMailBoxFailAdd(t *testing.T) {
	ctx := newMailboxContext(t, time.Now(), time.Minute)
	defer ctx.mailbox.Stop()

	failAdds := func(adds []*htlcPacket) {
		for _, add := range adds {
			ctx.mailbox.FailAdd(add)
		}
	}

	const numBatchPackets = 5

	// Send  10 adds, and pull them from the mailbox.
	adds := ctx.sendAdds(0, numBatchPackets)
	ctx.receivePkts(adds)

	// Fail all of these adds, simulating an error adding the HTLCs to the
	// commitment. We should see a failure message for each.
	go failAdds(adds)
	ctx.checkFails(adds)

	// As a sanity check, Fail all of them again and assert that no
	// duplicate fails are sent.
	go failAdds(adds)
	ctx.checkFails(nil)

}

// TestMailBoxPacketPrioritization asserts that the mailbox will prioritize
// delivering Settle and Fail packets over Adds if both are available for
// delivery at the same time.
func TestMailBoxPacketPrioritization(t *testing.T) {
	t.Parallel()

	// First, we'll create new instance of the current default mailbox
	// type.
	mailBox := newMemoryMailBox(&mailBoxConfig{
		clock:  clock.NewDefaultClock(),
		expiry: time.Minute,
	})
	mailBox.Start()
	defer mailBox.Stop()

	const numPackets = 5

	_, _, aliceChanID, bobChanID := genIDs()

	// Next we'll send the following sequence of packets:
	//  - Settle1
	//  - Add1
	//  - Add2
	//  - Fail
	//  - Settle2
	sentPackets := make([]*htlcPacket, numPackets)
	for i := 0; i < numPackets; i++ {
		pkt := &htlcPacket{
			outgoingChanID: aliceChanID,
			outgoingHTLCID: uint64(i),
			incomingChanID: bobChanID,
			incomingHTLCID: uint64(i),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
		}

		switch i {
		case 0, 4:
			// First and last packets are a Settle. A non-Add is
			// sent first to make the test deterministic w/o needing
			// to sleep.
			pkt.htlc = &lnwire.UpdateFulfillHTLC{ID: uint64(i)}
		case 1, 2:
			// Next two packets are Adds.
			pkt.htlc = &lnwire.UpdateAddHTLC{ID: uint64(i)}
		case 3:
			// Last packet is a Fail.
			pkt.htlc = &lnwire.UpdateFailHTLC{ID: uint64(i)}
		}

		sentPackets[i] = pkt

		err := mailBox.AddPacket(pkt)
		if err != nil {
			t.Fatalf("failed to add packet: %v", err)
		}
	}

	// When dequeueing the packets, we expect the following sequence:
	//  - Settle1
	//  - Fail
	//  - Settle2
	//  - Add1
	//  - Add2
	//
	// We expect to see Fail and Settle2 to be delivered before either Add1
	// or Add2 due to the prioritization between the split queue.
	for i := 0; i < numPackets; i++ {
		select {
		case pkt := <-mailBox.PacketOutBox():
			var expPkt *htlcPacket
			switch i {
			case 0:
				// First packet should be Settle1.
				expPkt = sentPackets[0]
			case 1:
				// Second packet should be Fail.
				expPkt = sentPackets[3]
			case 2:
				// Third packet should be Settle2.
				expPkt = sentPackets[4]
			case 3:
				// Fourth packet should be Add1.
				expPkt = sentPackets[1]
			case 4:
				// Last packet should be Add2.
				expPkt = sentPackets[2]
			}

			if !reflect.DeepEqual(expPkt, pkt) {
				t.Fatalf("recvd packet mismatch %d, want: %v, got: %v",
					i, spew.Sdump(expPkt), spew.Sdump(pkt))
			}

		case <-time.After(50 * time.Millisecond):
			t.Fatalf("didn't receive packet %d before timeout", i)
		}
	}
}

// TestMailOrchestrator asserts that the orchestrator properly buffers packets
// for channels that haven't been made live, such that they are delivered
// immediately after BindLiveShortChanID. It also tests that packets are delivered
// readily to mailboxes for channels that are already in the live state.
func TestMailOrchestrator(t *testing.T) {
	t.Parallel()

	// First, we'll create a new instance of our orchestrator.
	mo := newMailOrchestrator(&mailOrchConfig{
		clock:  clock.NewDefaultClock(),
		expiry: time.Minute,
	})
	defer mo.Stop()

	// We'll be delivering 10 htlc packets via the orchestrator.
	const numPackets = 10
	const halfPackets = numPackets / 2

	// Before any mailbox is created or made live, we will deliver half of
	// the htlcs via the orchestrator.
	chanID1, chanID2, aliceChanID, bobChanID := genIDs()
	sentPackets := make([]*htlcPacket, halfPackets)
	for i := 0; i < halfPackets; i++ {
		pkt := &htlcPacket{
			outgoingChanID: aliceChanID,
			outgoingHTLCID: uint64(i),
			incomingChanID: bobChanID,
			incomingHTLCID: uint64(i),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
			htlc: &lnwire.UpdateAddHTLC{
				ID: uint64(i),
			},
		}
		sentPackets[i] = pkt

		mo.Deliver(pkt.outgoingChanID, pkt)
	}

	// Now, initialize a new mailbox for Alice's chanid.
	mailbox := mo.GetOrCreateMailBox(chanID1, aliceChanID)

	// Verify that no messages are received, since Alice's mailbox has not
	// been made live.
	for i := 0; i < halfPackets; i++ {
		timeout := time.After(50 * time.Millisecond)
		select {
		case <-mailbox.MessageOutBox():
			t.Fatalf("should not receive wire msg after reset")
		case <-timeout:
		}
	}

	// Assign a short chan id to the existing mailbox, make it available for
	// capturing incoming HTLCs. The HTLCs added above should be delivered
	// immediately.
	mo.BindLiveShortChanID(mailbox, chanID1, aliceChanID)

	// Verify that all of the packets are queued and delivered to Alice's
	// mailbox.
	recvdPackets := make([]*htlcPacket, 0, len(sentPackets))
	for i := 0; i < halfPackets; i++ {
		timeout := time.After(5 * time.Second)
		select {
		case <-timeout:
			t.Fatalf("didn't recv pkt %d after timeout", i)
		case pkt := <-mailbox.PacketOutBox():
			recvdPackets = append(recvdPackets, pkt)
		}
	}

	// We should have received half of the total number of packets.
	if len(recvdPackets) != halfPackets {
		t.Fatalf("expected %v packets instead got %v",
			halfPackets, len(recvdPackets))
	}

	// Check that the received packets are equal to the sent packets.
	if !reflect.DeepEqual(recvdPackets, sentPackets) {
		t.Fatalf("recvd packets mismatched: expected %v, got %v",
			spew.Sdump(sentPackets), spew.Sdump(recvdPackets))
	}

	// For the second half of the test, create a new mailbox for Bob and
	// immediately make it live with an assigned short chan id.
	mailbox = mo.GetOrCreateMailBox(chanID2, bobChanID)
	mo.BindLiveShortChanID(mailbox, chanID2, bobChanID)

	// Create the second half of our htlcs, and deliver them via the
	// orchestrator. We should be able to receive each of these in order.
	recvdPackets = make([]*htlcPacket, 0, len(sentPackets))
	for i := 0; i < halfPackets; i++ {
		pkt := &htlcPacket{
			outgoingChanID: aliceChanID,
			outgoingHTLCID: uint64(halfPackets + i),
			incomingChanID: bobChanID,
			incomingHTLCID: uint64(halfPackets + i),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
			htlc: &lnwire.UpdateAddHTLC{
				ID: uint64(halfPackets + i),
			},
		}
		sentPackets[i] = pkt

		mo.Deliver(pkt.incomingChanID, pkt)

		timeout := time.After(50 * time.Millisecond)
		select {
		case <-timeout:
			t.Fatalf("didn't recv pkt %d after timeout", halfPackets+i)
		case pkt := <-mailbox.PacketOutBox():
			recvdPackets = append(recvdPackets, pkt)
		}
	}

	// Again, we should have received half of the total number of packets.
	if len(recvdPackets) != halfPackets {
		t.Fatalf("expected %v packets instead got %v",
			halfPackets, len(recvdPackets))
	}

	// Check that the received packets are equal to the sent packets.
	if !reflect.DeepEqual(recvdPackets, sentPackets) {
		t.Fatalf("recvd packets mismatched: expected %v, got %v",
			spew.Sdump(sentPackets), spew.Sdump(recvdPackets))
	}
}
