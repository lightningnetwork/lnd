package htlcswitch

import (
	"testing"
	"time"
	"container/heap"

	"github.com/lightningnetwork/lnd/lnwire"
)

// Generate a packet with the given packet id
func makePacket(pkt_id uint64) *htlcPacket {
	return &htlcPacket{
		incomingHTLCID : pkt_id,
		htlc           : &lnwire.UpdateAddHTLC{},
	}
}

// Test priority of packets
func TestHtlcPriority(t *testing.T) {
	q := make(priorityQueue, 2)
	q[0] = makeNode(makePacket(100))
	q[1] = makeNode(makePacket(200))

	if !q.Less(0, 1) {
		t.Fatalf("wrong priority order p0=%v, p1=%v", q[0].priority, q[1].priority)
	}
}

// Test input/output of priority queue
func TestPriorityQueue(t *testing.T) {
	q := make(priorityQueue, 0)
	
	for i := 0; i < 100; i++ {
		pkt := makePacket(uint64(i))
		heap.Push(&q, makeNode(pkt))
	}

	for i := 0; i < 100; i++ {
		head_node := q[0]
		pkt_id := head_node.packet.incomingHTLCID
		if pkt_id != uint64(i) {
			t.Fatalf("wrong incomingHTLCID in pop: expected %v got %v", i, pkt_id)
		}
		heap.Pop(&q)
	}
}

// TestWaitingQueueThreadSafety test the thread safety properties of the
// waiting queue, by executing methods in separate goroutines which operates
// with the same data.
func TestWaitingQueueThreadSafety(t *testing.T) {
	t.Parallel()

	const numPkts = 1000
	const maxQueueLen = 500

	q := newPacketQueue(numPkts, maxQueueLen)
	q.Start()
	defer q.Stop()

	a := make([]uint64, numPkts)
	for i := 0; i < numPkts; i++ {
		a[i] = uint64(i)
		q.AddPkt(makePacket(a[i]))
	}

	// The reported length of the queue should be maxQueueLen.
	queueLength := q.Length()
	if queueLength != maxQueueLen {
		t.Fatalf("queue has wrong length: expected %v, got %v", maxQueueLen,
			queueLength)
	}

	for i := 0; i < maxQueueLen; i++ {
		q.SignalFreeSlot()

		select {
		case packet := <-q.outgoingPkts:
			if packet.incomingHTLCID != uint64(i) {
				t.Fatalf("wrong output element: expected packet %v, got %v", i, 
					packet.incomingHTLCID)
			}

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
}
