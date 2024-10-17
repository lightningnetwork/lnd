package htlcswitch

import (
	prand "math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const testExpiry = time.Minute

// TestMailBoxCouriers tests that both aspects of the mailBox struct works
// properly. Both packets and messages should be able to added to each
// respective mailbox concurrently, and also messages/packets should also be
// able to be received concurrently.
func TestMailBoxCouriers(t *testing.T) {
	t.Parallel()

	// First, we'll create new instance of the current default mailbox
	// type.
	ctx := newMailboxContext(t, time.Now(), testExpiry)

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

		err := ctx.mailbox.AddPacket(pkt)
		if err != nil {
			t.Fatalf("unable to add packet: %v", err)
		}
	}

	// Next, we'll do the same, but this time adding wire messages.
	sentMessages := make([]lnwire.Message, numPackets)
	for i := 0; i < numPackets; i++ {
		msg := &lnwire.UpdateAddHTLC{
			ID:     uint64(prand.Int63()),
			Amount: lnwire.MilliSatoshi(prand.Int63()),
		}
		sentMessages[i] = msg

		err := ctx.mailbox.AddMessage(msg)
		if err != nil {
			t.Fatalf("unable to add message: %v", err)
		}
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
			case pkt := <-ctx.mailbox.PacketOutBox():
				recvdPackets = append(recvdPackets, pkt)
			}
		} else {
			select {
			case <-timeout:
				t.Fatalf("didn't recv message after timeout")
			case msg := <-ctx.mailbox.MessageOutBox():
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
	if !reflect.DeepEqual(sentMessages, recvdMessages) {
		t.Fatalf("recvd messages mismatched: expected %v, got %v",
			spew.Sdump(sentMessages), spew.Sdump(recvdMessages))
	}

	// Now that we've received all of the intended msgs/pkts, ack back half
	// of the packets.
	for _, recvdPkt := range recvdPackets[:halfPackets] {
		ctx.mailbox.AckPacket(recvdPkt.inKey())
	}

	// With the packets drained and partially acked,  we reset the mailbox,
	// simulating a link shutting down and then coming back up.
	err := ctx.mailbox.ResetMessages()
	require.NoError(t, err, "unable to reset messages")
	err = ctx.mailbox.ResetPackets()
	require.NoError(t, err, "unable to reset packets")

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
			case pkt := <-ctx.mailbox.PacketOutBox():
				recvdPackets2 = append(recvdPackets2, pkt)
			}
		} else {
			select {
			case <-ctx.mailbox.MessageOutBox():
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

	ctx := newMailboxContext(t, time.Now(), time.Second)

	// Stop the mailbox, then try to reset the message and packet couriers.
	ctx.mailbox.Stop()

	err := ctx.mailbox.ResetMessages()
	if err != ErrMailBoxShuttingDown {
		t.Fatalf("expected ErrMailBoxShuttingDown, got: %v", err)
	}

	err = ctx.mailbox.ResetPackets()
	if err != ErrMailBoxShuttingDown {
		t.Fatalf("expected ErrMailBoxShuttingDown, got: %v", err)
	}
}

type mailboxContext struct {
	t        *testing.T
	mailbox  MailBox
	clock    *clock.TestClock
	forwards chan *htlcPacket
}

// newMailboxContextWithClock creates a new mailbox context with the given
// mocked clock.
//
// TODO(yy): replace all usage of `newMailboxContext` with this method.
func newMailboxContextWithClock(t *testing.T,
	clock clock.Clock) *mailboxContext {

	ctx := &mailboxContext{
		t:        t,
		forwards: make(chan *htlcPacket, 1),
	}

	failMailboxUpdate := func(outScid,
		mboxScid lnwire.ShortChannelID) lnwire.FailureMessage {

		return &lnwire.FailTemporaryNodeFailure{}
	}

	ctx.mailbox = newMemoryMailBox(&mailBoxConfig{
		failMailboxUpdate: failMailboxUpdate,
		forwardPackets:    ctx.forward,
		clock:             clock,
	})
	ctx.mailbox.Start()
	t.Cleanup(ctx.mailbox.Stop)

	return ctx
}

func newMailboxContext(t *testing.T, startTime time.Time,
	expiry time.Duration) *mailboxContext {

	ctx := &mailboxContext{
		t:        t,
		clock:    clock.NewTestClock(startTime),
		forwards: make(chan *htlcPacket, 1),
	}

	failMailboxUpdate := func(outScid,
		mboxScid lnwire.ShortChannelID) lnwire.FailureMessage {

		return &lnwire.FailTemporaryNodeFailure{}
	}

	ctx.mailbox = newMemoryMailBox(&mailBoxConfig{
		failMailboxUpdate: failMailboxUpdate,
		forwardPackets:    ctx.forward,
		clock:             ctx.clock,
		expiry:            expiry,
	})
	ctx.mailbox.Start()
	t.Cleanup(ctx.mailbox.Stop)

	return ctx
}

func (c *mailboxContext) forward(_ <-chan struct{},
	pkts ...*htlcPacket) error {

	for _, pkt := range pkts {
		c.forwards <- pkt
	}

	return nil
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
		c.t.Fatalf("unexpected forward: %v", pkt.keystone())
	case <-time.After(50 * time.Millisecond):
	}
}

// TestMailBoxFailAdd asserts that FailAdd returns a response to the switch
// under various interleavings with other operations on the mailbox.
func TestMailBoxFailAdd(t *testing.T) {
	var (
		batchDelay       = time.Second
		expiry           = time.Minute
		firstBatchStart  = time.Now()
		secondBatchStart = time.Now().Add(batchDelay)
		thirdBatchStart  = time.Now().Add(2 * batchDelay)
		thirdBatchExpiry = thirdBatchStart.Add(expiry)
	)
	ctx := newMailboxContext(t, firstBatchStart, expiry)

	failAdds := func(adds []*htlcPacket) {
		for _, add := range adds {
			ctx.mailbox.FailAdd(add)
		}
	}

	const numBatchPackets = 5

	// Send  10 adds, and pull them from the mailbox.
	firstBatch := ctx.sendAdds(0, numBatchPackets)
	ctx.receivePkts(firstBatch)

	// Fail all of these adds, simulating an error adding the HTLCs to the
	// commitment. We should see a failure message for each.
	go failAdds(firstBatch)
	ctx.checkFails(firstBatch)

	// As a sanity check, Fail all of them again and assert that no
	// duplicate fails are sent.
	go failAdds(firstBatch)
	ctx.checkFails(nil)

	// Now, send a second batch of adds after a short delay and deliver them
	// to the link.
	ctx.clock.SetTime(secondBatchStart)
	secondBatch := ctx.sendAdds(numBatchPackets, numBatchPackets)
	ctx.receivePkts(secondBatch)

	// Reset the packet queue w/o changing the current time. This simulates
	// the link flapping and coming back up before the second batch's
	// expiries have elapsed. We should see no failures sent back.
	err := ctx.mailbox.ResetPackets()
	require.NoError(t, err, "unable to reset packets")
	ctx.checkFails(nil)

	// Redeliver the second batch to the link and hold them there.
	ctx.receivePkts(secondBatch)

	// Send a third batch of adds shortly after the second batch.
	ctx.clock.SetTime(thirdBatchStart)
	thirdBatch := ctx.sendAdds(2*numBatchPackets, numBatchPackets)

	// Advance the clock so that the third batch expires. We expect to only
	// see fails for the third batch, since the second batch is still being
	// held by the link.
	ctx.clock.SetTime(thirdBatchExpiry)
	ctx.checkFails(thirdBatch)

	// Finally, reset the link which should cause the second batch to be
	// cancelled immediately.
	err = ctx.mailbox.ResetPackets()
	require.NoError(t, err, "unable to reset packets")
	ctx.checkFails(secondBatch)
}

// TestMailBoxPacketPrioritization asserts that the mailbox will prioritize
// delivering Settle and Fail packets over Adds if both are available for
// delivery at the same time.
func TestMailBoxPacketPrioritization(t *testing.T) {
	t.Parallel()

	// First, we'll create new instance of the current default mailbox
	// type.
	ctx := newMailboxContext(t, time.Now(), testExpiry)

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

		err := ctx.mailbox.AddPacket(pkt)
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
		case pkt := <-ctx.mailbox.PacketOutBox():
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

// TestMailBoxAddExpiry asserts that the mailbox will cancel back Adds that
// have reached their expiry time.
func TestMailBoxAddExpiry(t *testing.T) {
	// Each batch will consist of 10 messages.
	const numBatchPackets = 10

	// deadline is the returned value from the `pktWithExpiry.deadline`.
	deadline := make(chan time.Time, numBatchPackets*2)

	// Create a mock clock and mock the methods.
	mockClock := &lnmock.MockClock{}
	mockClock.On("Now").Return(time.Now())

	// Mock TickAfter, which mounts the above `deadline` channel to the
	// returned value from `pktWithExpiry.deadline`.
	mockClock.On("TickAfter", mock.Anything).Return(deadline)

	// Create a test mailbox context.
	ctx := newMailboxContextWithClock(t, mockClock)

	// Send 10 packets and assert no failures are sent back.
	firstBatch := ctx.sendAdds(0, numBatchPackets)
	ctx.checkFails(nil)

	// Send another 10 packets and assert no failures are sent back.
	secondBatch := ctx.sendAdds(numBatchPackets, numBatchPackets)
	ctx.checkFails(nil)

	// Tick 10 times and we should see the first batch expired.
	for i := 0; i < numBatchPackets; i++ {
		deadline <- time.Now()
	}
	ctx.checkFails(firstBatch)

	// Tick another 10 times and we should see the second batch expired.
	for i := 0; i < numBatchPackets; i++ {
		deadline <- time.Now()
	}
	ctx.checkFails(secondBatch)
}

// TestMailBoxDuplicateAddPacket asserts that the mailbox returns an
// ErrPacketAlreadyExists failure when two htlcPackets are added with identical
// incoming circuit keys.
func TestMailBoxDuplicateAddPacket(t *testing.T) {
	t.Parallel()

	ctx := newMailboxContext(t, time.Now(), testExpiry)
	ctx.mailbox.Start()

	addTwice := func(t *testing.T, pkt *htlcPacket) {
		// The first add should succeed.
		err := ctx.mailbox.AddPacket(pkt)
		if err != nil {
			t.Fatalf("unable to add packet: %v", err)
		}

		// Adding again with the same incoming circuit key should fail.
		err = ctx.mailbox.AddPacket(pkt)
		if err != ErrPacketAlreadyExists {
			t.Fatalf("expected ErrPacketAlreadyExists, got: %v", err)
		}
	}

	// Assert duplicate AddPacket calls fail for all types of HTLCs.
	addTwice(t, &htlcPacket{
		incomingHTLCID: 0,
		htlc:           &lnwire.UpdateAddHTLC{},
	})
	addTwice(t, &htlcPacket{
		incomingHTLCID: 1,
		htlc:           &lnwire.UpdateFulfillHTLC{},
	})
	addTwice(t, &htlcPacket{
		incomingHTLCID: 2,
		htlc:           &lnwire.UpdateFailHTLC{},
	})
}

// TestMailBoxDustHandling tests that DustPackets returns the expected values
// for the local and remote dust sum after calling SetFeeRate and
// SetDustClosure.
func TestMailBoxDustHandling(t *testing.T) {
	t.Run("tweakless mailbox dust", func(t *testing.T) {
		testMailBoxDust(t, channeldb.SingleFunderTweaklessBit)
	})
	t.Run("zero htlc fee anchors mailbox dust", func(t *testing.T) {
		testMailBoxDust(t, channeldb.SingleFunderTweaklessBit|
			channeldb.AnchorOutputsBit|
			channeldb.ZeroHtlcTxFeeBit,
		)
	})
}

func testMailBoxDust(t *testing.T, chantype channeldb.ChannelType) {
	t.Parallel()

	ctx := newMailboxContext(t, time.Now(), testExpiry)

	_, _, aliceID, bobID := genIDs()

	// It should not be the case that the MailBox has packets before the
	// feeRate or dustClosure is set. This is because the mailbox is always
	// created *with* its associated link and attached via AttachMailbox,
	// where these parameters will be set. Even though the lifetime is
	// longer than the link, the setting will persist across multiple link
	// creations.
	ctx.mailbox.SetFeeRate(chainfee.SatPerKWeight(253))

	localDustLimit := btcutil.Amount(400)
	remoteDustLimit := btcutil.Amount(500)
	isDust := dustHelper(chantype, localDustLimit, remoteDustLimit)
	ctx.mailbox.SetDustClosure(isDust)

	// The first packet will be dust according to the remote dust limit,
	// but not the local. We set a different amount if this is a zero fee
	// htlc channel type.
	firstAmt := lnwire.MilliSatoshi(600_000)

	if chantype.ZeroHtlcTxFee() {
		firstAmt = lnwire.MilliSatoshi(450_000)
	}

	firstPkt := &htlcPacket{
		outgoingChanID: aliceID,
		outgoingHTLCID: 0,
		incomingChanID: bobID,
		incomingHTLCID: 0,
		amount:         firstAmt,
		htlc: &lnwire.UpdateAddHTLC{
			ID: uint64(0),
		},
	}

	err := ctx.mailbox.AddPacket(firstPkt)
	require.NoError(t, err)

	// Assert that the local sum is 0, and the remote sum accounts for this
	// added packet.
	localSum, remoteSum := ctx.mailbox.DustPackets()
	require.Equal(t, lnwire.MilliSatoshi(0), localSum)
	require.Equal(t, firstAmt, remoteSum)

	// The next packet will be dust according to both limits.
	secondAmt := lnwire.MilliSatoshi(300_000)
	secondPkt := &htlcPacket{
		outgoingChanID: aliceID,
		outgoingHTLCID: 1,
		incomingChanID: bobID,
		incomingHTLCID: 1,
		amount:         secondAmt,
		htlc: &lnwire.UpdateAddHTLC{
			ID: uint64(1),
		},
	}

	err = ctx.mailbox.AddPacket(secondPkt)
	require.NoError(t, err)

	// Assert that both the local and remote sums have increased by the
	// second amount.
	localSum, remoteSum = ctx.mailbox.DustPackets()
	require.Equal(t, secondAmt, localSum)
	require.Equal(t, firstAmt+secondAmt, remoteSum)

	// Now we pull both packets off of the queue.
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.mailbox.PacketOutBox():
		case <-time.After(50 * time.Millisecond):
			ctx.t.Fatalf("did not receive packet in time")
		}
	}

	// Assert that the sums haven't changed.
	localSum, remoteSum = ctx.mailbox.DustPackets()
	require.Equal(t, secondAmt, localSum)
	require.Equal(t, firstAmt+secondAmt, remoteSum)

	// Remove the first packet from the mailbox.
	removed := ctx.mailbox.AckPacket(firstPkt.inKey())
	require.True(t, removed)

	// Assert that the remote sum does not include the firstAmt.
	localSum, remoteSum = ctx.mailbox.DustPackets()
	require.Equal(t, secondAmt, localSum)
	require.Equal(t, secondAmt, remoteSum)

	// Remove the second packet from the mailbox.
	removed = ctx.mailbox.AckPacket(secondPkt.inKey())
	require.True(t, removed)

	// Assert that both sums are equal to 0.
	localSum, remoteSum = ctx.mailbox.DustPackets()
	require.Equal(t, lnwire.MilliSatoshi(0), localSum)
	require.Equal(t, lnwire.MilliSatoshi(0), remoteSum)
}

// TestMailOrchestrator asserts that the orchestrator properly buffers packets
// for channels that haven't been made live, such that they are delivered
// immediately after BindLiveShortChanID. It also tests that packets are delivered
// readily to mailboxes for channels that are already in the live state.
func TestMailOrchestrator(t *testing.T) {
	t.Parallel()

	failMailboxUpdate := func(outScid,
		mboxScid lnwire.ShortChannelID) lnwire.FailureMessage {

		return &lnwire.FailTemporaryNodeFailure{}
	}

	// First, we'll create a new instance of our orchestrator.
	mo := newMailOrchestrator(&mailOrchConfig{
		failMailboxUpdate: failMailboxUpdate,
		forwardPackets: func(_ <-chan struct{},
			pkts ...*htlcPacket) error {

			return nil
		},
		clock:  clock.NewTestClock(time.Now()),
		expiry: testExpiry,
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
