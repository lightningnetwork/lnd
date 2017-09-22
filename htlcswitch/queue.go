package htlcswitch

import (
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
)

// packetQueue is an goroutine-safe queue of htlc packets which over flow the
// current commitment transaction. An HTLC will overflow the current commitment
// transaction if it is attempted to be added to a state machine which already
// has the max number of pending HTLC's present on the commitment transaction.
// Packets are removed from the queue by the channelLink itself as additional
// slots become available on the commitment transaction itself.
type packetQueue struct {
	queueCond *sync.Cond
	queueMtx  sync.Mutex
	queue     []*htlcPacket

	// outgoingPkts is a channel that the channelLink will receive on in
	// order to drain the packetQueue as new slots become available on the
	// commitment transaction.
	outgoingPkts chan *htlcPacket

	queries chan interface{}

	wg   sync.WaitGroup
	quit chan struct{}
}

// newPacketQueue returns a new instance of the packetQueue.
func newPacketQueue() *packetQueue {
	p := &packetQueue{
		outgoingPkts: make(chan *htlcPacket),

		queries: make(chan interface{}),

		quit: make(chan struct{}),
	}
	p.queueCond = sync.NewCond(&p.queueMtx)

	return p
}

// Start starts all goroutines that packetQueue needs to perform its normal
// duties.
func (p *packetQueue) Start() {
	p.wg.Add(2)
	go p.packetCoordinator()
	go p.queryHandler()
}

// Stop signals the packetQueue for a graceful shutdown, and waits for all
// goroutines to exit.
func (p *packetQueue) Stop() {
	close(p.quit)

	p.queueCond.Signal()

	p.wg.Wait()
}

// packetCoordinator is a goroutine that handles the packet overflow queue.
// Using a synchronized queue, outside callers are able to append to the end of
// the queue, waking up the coordinator when the queue transitions from empty
// to non-empty. The packetCoordinator will then aggressively try to empty out
// the queue, passing new htlcPackets to the channelLink as slots within the
// commitment transaction become available.
//
// Future iterations of the packetCoordinator will implement congestion
// avoidance logic in the face of persistent htlcPacket back-pressure.
//
// TODO(roasbeef): later will need to add back pressure handling heuristics
// like reg congestion avoidance:
//   * random dropping, RED, etc
func (p *packetQueue) packetCoordinator() {
	defer p.wg.Done()

	for {
		// First, we'll check our condition. If the queue of packets is
		// empty, then we'll wait until a new item is added.
		p.queueCond.L.Lock()
		for len(p.queue) == 0 {
			p.queueCond.Wait()

			// If we were woke up in order to exit, then we'll do
			// so. Otherwise, we'll check the message queue for any
			// new items.
			select {
			case <-p.quit:
				p.queueCond.L.Unlock()
				return
			default:
			}
		}

		nextPkt := p.queue[0]

		// If there aren't any further messages to sent (or the link
		// didn't immediately read our message), then we'll block and
		// wait for a new message to be sent into the overflow queue,
		// or for the link's htlcForwarder to wake up.
		select {
		case p.outgoingPkts <- nextPkt:
			// Pop the item off the front of the queue and slide
			// down the reference one to re-position the head
			// pointer. This will set us up for the next iteration.
			// If the queue is empty at this point, then we'll
			// block at the top.
			p.queue[0] = nil
			p.queue = p.queue[1:]

		case <-p.quit:
			p.queueCond.L.Unlock()
			return

		default:
		}

		p.queueCond.L.Unlock()
	}
}

// queueLenReq is a request sent to the queryHandler to query for the length of
// the current pending packet queue.
type queueLenReq struct {
	resp chan int
}

// totalPendingReq is a request sent to the queryHandler to query for the total
// amount of satoshis pending in the queue at a given instant.
type totalPendingReq struct {
	resp chan lnwire.MilliSatoshi
}

// queryHandler is a dedicated goroutine for handling queries from outside
// sub-systems to the packetQueue itself.
func (p *packetQueue) queryHandler() {
	defer p.wg.Done()

	for {
		select {
		case query := <-p.queries:
			switch req := query.(type) {
			case *queueLenReq:
				p.queueCond.L.Lock()

				select {
				case req.resp <- len(p.queue):
				case <-p.quit:
					p.queueCond.L.Unlock()
					return
				}

				p.queueCond.L.Unlock()
			case *totalPendingReq:
				p.queueCond.L.Lock()

				var amount lnwire.MilliSatoshi
				for _, pkt := range p.queue {
					amount += pkt.amount
				}

				select {
				case req.resp <- amount:
				case <-p.quit:
					p.queueCond.L.Unlock()
					return
				}

				p.queueCond.L.Unlock()
			}

		case <-p.quit:
			return
		}
	}
}

// AddPkt adds the referenced packet to the overflow queue, preserving ordering
// of the existing items.
func (p *packetQueue) AddPkt(pkt *htlcPacket) {
	// First, we'll lock the condition, and add the message to the end of
	// the message queue.
	p.queueCond.L.Lock()
	p.queue = append(p.queue, pkt)
	p.queueCond.L.Unlock()

	// With the message added, we signal to the msgConsumer that there are
	// additional messages to consume.
	p.queueCond.Signal()
}

// Length returns the number of pending htlc packets present within the over
// flow queue.
func (p *packetQueue) Length() int {
	lenReq := &queueLenReq{
		resp: make(chan int, 1),
	}

	select {
	case p.queries <- lenReq:
	case <-p.quit:
		return 0
	}

	select {
	case len := <-lenReq.resp:
		return len
	case <-p.quit:
		return 0
	}
}

// PendingAmount returns the total sum in satoshis of all the pending
// htlcPackets within the queue.
func (p *packetQueue) PendingAmount() lnwire.MilliSatoshi {
	amtReq := &totalPendingReq{
		resp: make(chan lnwire.MilliSatoshi, 1),
	}

	select {
	case p.queries <- amtReq:
	case <-p.quit:
		return 0
	}

	select {
	case amt := <-amtReq.resp:
		return amt
	case <-p.quit:
		return 0
	}
}
