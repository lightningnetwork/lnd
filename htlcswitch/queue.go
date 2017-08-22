package htlcswitch

import (
	"container/list"
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
)

// packetQueue represent the wrapper around the original queue plus the
// functionality for releasing the queue objects in object channel. Such
// structures allows storing of all pending object in queue before the moment
// of actual releasing.
//
// TODO(andrew.shvv) structure not preserve the order if object failed second
// time.
type packetQueue struct {
	*list.List
	sync.Mutex

	// pending channel is needed in order to send the object which should
	// be re-proceed.
	pending chan *htlcPacket

	// grab channel represents the channel-lock which is needed in order to
	// make "release" goroutines block during other release goroutine
	// processing.
	grab chan struct{}
}

func newWaitingQueue() *packetQueue {
	// Initialize grab channel and release one slot, otherwise release
	// function will block.
	done := make(chan struct{}, 1)
	done <- struct{}{}

	return &packetQueue{
		pending: make(chan *htlcPacket),
		grab:    done,
		List:    list.New(),
	}
}

// consume function take the given packet and store it in queue till release
// function will be executed.
func (q *packetQueue) consume(packet *htlcPacket) {
	q.Lock()
	defer q.Unlock()

	q.PushBack(packet)
}

// release function spawn the goroutine which grab the object from pending
// queue and pass it in processing channel.
func (q *packetQueue) release() {
	q.Lock()
	defer q.Unlock()

	if q.Len() == 0 {
		return
	}

	go func() {
		// Grab the pending mutex so that other goroutines waits before
		// grabbing the object, otherwise the objects will be send in
		// the pending channel in random sequence.
		<-q.grab

		defer func() {
			// Release the channel-lock and give other goroutines
			// the ability to
			q.grab <- struct{}{}
		}()

		// Fetch object after releasing the pending mutex in order to
		// preserver the order of the stored objects.
		q.Lock()
		e := q.Front()
		q.Unlock()

		if e != nil {
			// Send the object in object queue and wait it to be
			// processed by other side.
			q.pending <- e.Value.(*htlcPacket)

			// After object have been preprocessed remove it from
			// the queue.
			q.Lock()
			q.Remove(e)
			q.Unlock()
			return
		}
	}()
}

// length returns the number of pending htlc packets which is stored in queue.
func (q *packetQueue) length() int {
	q.Lock()
	defer q.Unlock()

	return q.Len()
}

// pendingAmount returns the amount of money which is stored in pending queue.
func (q *packetQueue) pendingAmount() lnwire.MilliSatoshi {
	q.Lock()
	defer q.Unlock()

	var amount lnwire.MilliSatoshi
	for e := q.Front(); e != nil; e = e.Next() {
		packet := e.Value.(*htlcPacket)
		htlc := packet.htlc.(*lnwire.UpdateAddHTLC)
		amount += htlc.Amount
	}

	return amount
}
