package htlcswitch

import (
	"reflect"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// TestWaitingQueueThreadSafety test the thread safety properties of the
// waiting queue, by executing methods in separate goroutines which operates
// with the same data.
func TestWaitingQueueThreadSafety(t *testing.T) {
	t.Parallel()

	const numPkts = 1000

	q := newPacketQueue(numPkts)
	q.Start()
	defer q.Stop()

	a := make([]lnwire.MilliSatoshi, numPkts)
	for i := 0; i < numPkts; i++ {
		a[i] = lnwire.MilliSatoshi(i)
		q.AddPkt(&htlcPacket{
			amount: lnwire.MilliSatoshi(i),
			htlc:   &lnwire.UpdateAddHTLC{},
		})
	}

	// The reported length of the queue should be the exact number of
	// packets we added above.
	queueLength := q.Length()
	if queueLength != numPkts {
		t.Fatalf("queue has wrong length: expected %v, got %v", numPkts,
			queueLength)
	}

	var b []lnwire.MilliSatoshi
	for i := 0; i < numPkts; i++ {
		q.SignalFreeSlot()

		select {
		case packet := <-q.outgoingPkts:
			b = append(b, packet.amount)

		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}

	// The length of the queue should be zero at this point.
	time.Sleep(time.Millisecond * 50)
	queueLength = q.Length()
	if queueLength != 0 {
		t.Fatalf("queue has wrong length: expected %v, got %v", 0,
			queueLength)
	}

	if !reflect.DeepEqual(b, a) {
		t.Fatal("wrong order of the objects")
	}
}
