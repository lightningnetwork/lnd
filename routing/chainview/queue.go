package chainview

import "sync"

// blockEventType is the possible types of a blockEvent.
type blockEventType uint8

const (
	// connected is the type of a blockEvent representing a block
	// that was connected to our current chain.
	connected blockEventType = iota

	// disconnected is the type of a blockEvent representing a
	// block that is stale/disconnected from our current chain.
	disconnected
)

// blockEvent represent a block that was either connected
// or disconnected from the current chain.
type blockEvent struct {
	eventType blockEventType
	block     *FilteredBlock
}

// blockEventQueue is an ordered queue for block events sent from a
// FilteredChainView. The two types of possible block events are
// connected/new blocks, and disconnected/stale blocks. The
// blockEventQueue keeps the order of these events intact, while
// still being non-blocking. This is important in order for the
// chainView's call to onBlockConnected/onBlockDisconnected to not
// get blocked, and for the consumer of the block events to always
// get the events in the correct order.
type blockEventQueue struct {
	queueCond *sync.Cond
	queueMtx  sync.Mutex
	queue     []*blockEvent

	// newBlocks is the channel where the consumer of the queue
	// will receive connected/new blocks from the FilteredChainView.
	newBlocks chan *FilteredBlock

	// staleBlocks is the channel where the consumer of the queue will
	// receive disconnected/stale blocks from the FilteredChainView.
	staleBlocks chan *FilteredBlock

	wg   sync.WaitGroup
	quit chan struct{}
}

// newBlockEventQueue creates a new blockEventQueue.
func newBlockEventQueue() *blockEventQueue {
	b := &blockEventQueue{
		newBlocks:   make(chan *FilteredBlock),
		staleBlocks: make(chan *FilteredBlock),
		quit:        make(chan struct{}),
	}
	b.queueCond = sync.NewCond(&b.queueMtx)

	return b
}

// Start starts the blockEventQueue coordinator such that it can start handling
// events.
func (b *blockEventQueue) Start() {
	b.wg.Add(1)
	go b.queueCoordinator()
}

// Stop signals the queue coordinator to stop, such that the queue can be
// shut down.
func (b *blockEventQueue) Stop() {
	close(b.quit)

	b.queueCond.Signal()
}

// queueCoordinator is the queue's main loop, handling incoming block events
// and handing them off to the correct output channel.
//
// NB: MUST be run as a goroutine from the Start() method.
func (b *blockEventQueue) queueCoordinator() {
	defer b.wg.Done()

	for {
		// First, we'll check our condition. If the queue of events is
		// empty, then we'll wait until a new item is added.
		b.queueCond.L.Lock()
		for len(b.queue) == 0 {
			b.queueCond.Wait()

			// If we were woke up in order to exit, then we'll do
			// so. Otherwise, we'll check the queue for any new
			// items.
			select {
			case <-b.quit:
				b.queueCond.L.Unlock()
				return
			default:
			}
		}

		// Grab the first element in the queue, and nil the index to
		// avoid gc leak.
		event := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		b.queueCond.L.Unlock()

		// In the case this is a connected block, we'll send it on the
		// newBlocks channel. In case it is a disconnected block, we'll
		// send it on the staleBlocks channel. This send will block
		// until it is received by the consumer on the other end, making
		// sure we won't try to send any other block event before the
		// consumer is aware of this one.
		switch event.eventType {
		case connected:
			select {
			case b.newBlocks <- event.block:
			case <-b.quit:
				return
			}
		case disconnected:
			select {
			case b.staleBlocks <- event.block:
			case <-b.quit:
				return
			}
		}
	}
}

// Add puts the provided blockEvent at the end of the event queue, making sure
// it will first be received after all previous events. This method is
// non-blocking, in the sense that it will never wait for the consumer of the
// queue to read form the other end, making it safe to call from the
// FilteredChainView's onBlockConnected/onBlockDisconnected.
func (b *blockEventQueue) Add(event *blockEvent) {

	// Lock the condition, and add the event to the end of queue.
	b.queueCond.L.Lock()
	b.queue = append(b.queue, event)
	b.queueCond.L.Unlock()

	// With the event added, we signal to the queueCoordinator that
	// there are new events to handle.
	b.queueCond.Signal()
}
