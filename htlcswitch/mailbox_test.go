package htlcswitch

import (
	prand "math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
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
	mailBox := newMemoryMailBox()
	mailBox.Start()
	defer mailBox.Stop()

	// We'll be adding 10 message of both types to the mailbox.
	const numPackets = 10

	// We'll add a set of random packets to the mailbox.
	sentPackets := make([]*htlcPacket, numPackets)
	for i := 0; i < numPackets; i++ {
		pkt := &htlcPacket{
			outgoingChanID: lnwire.NewShortChanIDFromInt(uint64(prand.Int63())),
			incomingChanID: lnwire.NewShortChanIDFromInt(uint64(prand.Int63())),
			amount:         lnwire.MilliSatoshi(prand.Int63()),
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
}
