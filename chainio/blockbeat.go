package chainio

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
)

// DefaultProcessBlockTimeout is the timeout value used when waiting for one
// consumer to finish processing the new block epoch.
var DefaultProcessBlockTimeout = 30 * time.Second

// Consumer defines a blockbeat consumer interface. Subsystems that need block
// info should implement it.
type Consumer interface {
	// Name returns a human-readable string for this subsystem.
	Name() string

	// ProcessBlock takes a beat and processes it. A receive-only error
	// chan must be returned.
	//
	// NOTE: When implementing this, it's very important to send back the
	// error or nil to the channel immediately, otherwise BlockBeat will
	// timeout and lnd will shutdown.
	ProcessBlock(b Beat) <-chan error
}

// Beat contains the block epoch and a buffer error chan.
//
// TODO(yy): extend this to check for confirmation status - which serves as the
// single source of truth, to avoid the potential race between receiving blocks
// and `GetTransactionDetails/RegisterSpendNtfn/RegisterConfirmationsNtfn`.
type Beat struct {
	// Epoch is the current block epoch the blockbeat is aware of.
	Epoch chainntnfs.BlockEpoch

	// Err is a buffered chan that receives an error or nil from
	// ProcessBlock.
	Err chan error
}

// NewBeat creates a new beat with the specified block epoch and a buffered
// error chan.
func NewBeat(epoch chainntnfs.BlockEpoch) Beat {
	return Beat{
		Epoch: epoch,
		Err:   make(chan error, 1),
	}
}

// NotifySequential takes a list of consumers and notify them about the new
// epoch sequentially.
func (b *Beat) NotifySequential(consumers []Consumer) error {
	for _, c := range consumers {
		// Construct a new beat with a buffered error chan.
		beat := NewBeat(b.Epoch)

		// Record the time it takes the consumer to process this block.
		start := time.Now()

		log.Tracef("Sending block %v to consumer: %v", b.Epoch.Height,
			c.Name())

		// We expect the consumer to finish processing this block under
		// 30s, otherwise a timeout error is returned.
		err, timeout := fn.RecvOrTimeout(
			c.ProcessBlock(beat), DefaultProcessBlockTimeout,
		)
		if err != nil {
			return fmt.Errorf("%s: ProcessBlock got: %w", c.Name(),
				err)
		}
		if timeout != nil {
			return fmt.Errorf("%s timed out while processing block",
				c.Name())
		}

		log.Debugf("Consumer [%s] processed block %d in %v", c.Name(),
			b.Epoch.Height, time.Since(start))
	}

	return nil
}

// NotifyConcurrent notifies each queue concurrently about the latest block
// epoch.
func (b *Beat) NotifyConcurrent(consumers []Consumer, quit chan struct{}) {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[string]chan error, len(consumers))

	// Notify each queue in goroutines.
	for _, c := range consumers {
		log.Tracef("Sending block %v to consumer: %v", b.Epoch.Height,
			c.Name())

		// Create a signal chan.
		errChan := make(chan error)
		errChans[c.Name()] = errChan

		// Notify each consumer concurrently.
		go func(c Consumer, epoch chainntnfs.BlockEpoch) {
			// Construct a new beat with a buffered error chan.
			beat := NewBeat(epoch)

			// Notify each consumer in this queue sequentially.
			errChan <- beat.NotifySequential([]Consumer{c})
		}(c, b.Epoch)
	}

	// Wait for all consumers in each queue to finish.
	for name, errChan := range errChans {
		select {
		case err := <-errChan:
			// It's critical that the subsystems can process blocks
			// correctly and timely, if an error returns, we'd
			// gracefully shutdown lnd to bring attentions.
			if err != nil {
				log.Criticalf("Consumer=%v failed to process "+
					"block: %v", name, err)

				return
			}

			log.Debugf("Notified consumer=%v on block %d", name,
				b.Epoch.Height)

		case <-quit:
		}
	}
}

// BlockBeat is a service that handles dispatching new blocks to `lnd`'s
// subsystems. During startup, subsystems that are block-driven should
// implement the `Consumer` interface and register themselves via
// `RegisterQueue`. When two subsystems are independent of each other, they
// should be registered in differet queues so blocks are notified concurrently.
// Otherwise, when living in the same queue, the subsystems are notified of the
// new blocks sequentially, which means it's critical to understand the
// relationship of these systems to properly handle the order.
type BlockBeat struct {
	wg sync.WaitGroup

	// notifier is used to receive new block epochs.
	notifier chainntnfs.ChainNotifier

	// blockEpoch is the latest block epoch received .
	blockEpoch chainntnfs.BlockEpoch

	// consumerQueues is a map of consumers that will receive blocks. Each
	// queue is notified concurrently, and consumers in the same queue is
	// notified sequentially.
	consumerQueues map[uint32][]Consumer

	// counter is used to assign a unique id to each queue.
	counter atomic.Uint32

	// quit is used to signal the BlockBeat to stop.
	quit chan struct{}
}

// NewBlockBeat returns a new blockbeat instance.
func NewBlockBeat(notifier chainntnfs.ChainNotifier) *BlockBeat {
	return &BlockBeat{
		notifier:       notifier,
		quit:           make(chan struct{}),
		consumerQueues: make(map[uint32][]Consumer),
	}
}

// RegisterQueue takes a list of consumers and register them in the same queue.
//
// NOTE: these consumers are notified sequentially.
func (b *BlockBeat) RegisterQueue(consumers []Consumer) {
	qid := b.counter.Add(1)

	b.consumerQueues[qid] = append(b.consumerQueues[qid], consumers...)
	log.Infof("Registered queue=%d with %d blockbeat consumers", qid,
		len(consumers))

	for _, c := range consumers {
		log.Debugf("Consumer [%s] registered in queue %d", c.Name(),
			qid)
	}
}

// Start starts the blockbeat - it registers a block notification and monitors
// and dispatches new blocks in a goroutine. It will refuse to start if there
// are no registered consumers.
func (b *BlockBeat) Start() error {
	// Make sure consumers are registered.
	if len(b.consumerQueues) == 0 {
		return fmt.Errorf("no consumers registered")
	}

	// Start listening to new block epochs.
	blockEpochs, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %w", err)
	}

	log.Infof("BlockBeat is starting with %d consumer queues",
		len(b.consumerQueues))
	defer log.Debug("BlockBeat started")

	b.wg.Add(1)
	go b.dispatchBlocks(blockEpochs)

	return nil
}

// Stop shuts down the blockbeat.
func (b *BlockBeat) Stop() {
	log.Info("BlockBeat is stopping")
	defer log.Debug("BlockBeat stopped")

	// Signal the dispatchBlocks goroutine to stop.
	close(b.quit)
	b.wg.Wait()
}

// dispatchBlocks listens to new block epoch and dispatches it to all the
// consumers. Each queue in BlockBeat is notified concurrently, and the
// consumers in the same queue are notified sequentially.
func (b *BlockBeat) dispatchBlocks(blockEpochs *chainntnfs.BlockEpochEvent) {
	defer b.wg.Done()
	defer blockEpochs.Cancel()

	for {
		select {
		case blockEpoch, ok := <-blockEpochs.Epochs:
			if !ok {
				log.Debugf("Block epoch channel closed")
				return
			}

			log.Infof("Received new block %v at height %d, "+
				"notifying consumers...", blockEpoch.Hash,
				blockEpoch.Height)

			// Update the current block epoch.
			b.blockEpoch = *blockEpoch

			// Notify all consumers.
			b.notifyQueues()

			log.Infof("Notified all consumers on block %v at "+
				"height %d", blockEpoch.Hash, blockEpoch.Height)

		case <-b.quit:
			log.Debugf("BlockBeat quit signal received")
			return
		}
	}
}

// notifyQueues notifies each queue concurrently about the latest block epoch.
func (b *BlockBeat) notifyQueues() {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[uint32]chan error, len(b.consumerQueues))

	// Notify each queue in goroutines.
	for qid, consumers := range b.consumerQueues {
		log.Debugf("Notifying queue=%d on block %d", qid,
			b.blockEpoch.Height)

		// Create a signal chan.
		errChan := make(chan error)
		errChans[qid] = errChan

		// Notify each queue concurrently.
		b.wg.Add(1)
		go func(qid uint32, c []Consumer,
			epoch chainntnfs.BlockEpoch) {

			defer b.wg.Done()

			// Construct a new beat with a buffered error chan.
			beat := NewBeat(epoch)

			// Notify each consumer in this queue sequentially.
			errChan <- beat.NotifySequential(c)
		}(qid, consumers, b.blockEpoch)
	}

	// Wait for all consumers in each queue to finish.
	for qid, errChan := range errChans {
		select {
		case err := <-errChan:
			// It's critical that the subsystems can process blocks
			// correctly and timely, if an error returns, we'd
			// gracefully shutdown lnd to bring attentions.
			if err != nil {
				log.Criticalf("Queue=%d failed to process "+
					"block: %v", qid, err)

				return
			}

			log.Debugf("Notified queue=%d on block %d", qid,
				b.blockEpoch.Height)

		case <-b.quit:
		}
	}
}

// // notifyQueue takes a list of consumers and notify them about the new epoch
// // sequentially.
// func (b *BlockBeat) notifyQueue(queue []Consumer,
// 	epoch chainntnfs.BlockEpoch) error {

// 	for _, c := range queue {
// 		log.Debugf("Notifying consumer [%s] on block %d", c.Name(),
// 			epoch.Height)

// 		// Construct a new beat with a buffered error chan.
// 		beat := NewBeat(epoch)

// 		// Record the time it takes the consumer to process this block.
// 		start := time.Now()

// 		// We expect the consumer to finish processing this block under
// 		// 30s, otherwise a timeout error is returned.
// 		err, timeout := fn.RecvOrTimeout(
// 			c.ProcessBlock(beat), DefaultProcessBlockTimeout,
// 		)
// 		if err != nil {
// 			return fmt.Errorf("%s: ProcessBlock got: %w", c.Name(),
// 				err)
// 		}
// 		if timeout != nil {
// 			return fmt.Errorf("%s timed out while processing block",
// 				c.Name())
// 		}

// 		log.Debugf("Consumer [%s] processed block %d in %v", c.Name(),
// 			epoch.Height, time.Since(start))
// 	}

// 	return nil
// }
