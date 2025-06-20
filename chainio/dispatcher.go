package chainio

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnutils"
	"golang.org/x/sync/errgroup"
)

// DefaultProcessBlockTimeout is the timeout value used when waiting for one
// consumer to finish processing the new block epoch.
var DefaultProcessBlockTimeout = 60 * time.Second

// ErrProcessBlockTimeout is the error returned when a consumer takes too long
// to process the block.
var ErrProcessBlockTimeout = errors.New("process block timeout")

// BlockbeatDispatcher is a service that handles dispatching new blocks to
// `lnd`'s subsystems. During startup, subsystems that are block-driven should
// implement the `Consumer` interface and register themselves via
// `RegisterQueue`. When two subsystems are independent of each other, they
// should be registered in different queues so blocks are notified concurrently.
// Otherwise, when living in the same queue, the subsystems are notified of the
// new blocks sequentially, which means it's critical to understand the
// relationship of these systems to properly handle the order.
type BlockbeatDispatcher struct {
	wg sync.WaitGroup

	// notifier is used to receive new block epochs.
	notifier chainntnfs.ChainNotifier

	// beat is the latest blockbeat received.
	beat Blockbeat

	// consumerQueues is a map of consumers that will receive blocks. Its
	// key is a unique counter and its value is a queue of consumers. Each
	// queue is notified concurrently, and consumers in the same queue is
	// notified sequentially.
	consumerQueues map[uint32][]Consumer

	// counter is used to assign a unique id to each queue.
	counter atomic.Uint32

	// quit is used to signal the BlockbeatDispatcher to stop.
	quit chan struct{}

	// queryHeightChan is used to receive queries on the current height of
	// the dispatcher.
	queryHeightChan chan *query
}

// query is used to fetch the internal state of the dispatcher.
type query struct {
	// respChan is used to send back the current height back to the caller.
	//
	// NOTE: This channel must be buffered.
	respChan chan int32
}

// newQuery creates a query to be used to fetch the internal state of the
// dispatcher.
func newQuery() *query {
	return &query{
		respChan: make(chan int32, 1),
	}
}

// NewBlockbeatDispatcher returns a new blockbeat dispatcher instance.
func NewBlockbeatDispatcher(n chainntnfs.ChainNotifier) *BlockbeatDispatcher {
	return &BlockbeatDispatcher{
		notifier:        n,
		quit:            make(chan struct{}),
		consumerQueues:  make(map[uint32][]Consumer),
		queryHeightChan: make(chan *query, 1),
	}
}

// RegisterQueue takes a list of consumers and registers them in the same
// queue.
//
// NOTE: these consumers are notified sequentially.
func (b *BlockbeatDispatcher) RegisterQueue(consumers []Consumer) {
	qid := b.counter.Add(1)

	b.consumerQueues[qid] = append(b.consumerQueues[qid], consumers...)
	clog.Infof("Registered queue=%d with %d blockbeat consumers", qid,
		len(consumers))

	for _, c := range consumers {
		clog.Debugf("Consumer [%s] registered in queue %d", c.Name(),
			qid)
	}
}

// Start starts the blockbeat dispatcher - it registers a block notification
// and monitors and dispatches new blocks in a goroutine. It will refuse to
// start if there are no registered consumers.
func (b *BlockbeatDispatcher) Start() error {
	// Make sure consumers are registered.
	if len(b.consumerQueues) == 0 {
		return fmt.Errorf("no consumers registered")
	}

	// Start listening to new block epochs. We should get a notification
	// with the current best block immediately.
	blockEpochs, err := b.notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %w", err)
	}

	clog.Infof("BlockbeatDispatcher is starting with %d consumer queues",
		len(b.consumerQueues))
	defer clog.Debug("BlockbeatDispatcher started")

	b.wg.Add(1)
	go b.dispatchBlocks(blockEpochs)

	return nil
}

// Stop shuts down the blockbeat dispatcher.
func (b *BlockbeatDispatcher) Stop() {
	clog.Info("BlockbeatDispatcher is stopping")
	defer clog.Debug("BlockbeatDispatcher stopped")

	// Signal the dispatchBlocks goroutine to stop.
	close(b.quit)
	b.wg.Wait()
}

func (b *BlockbeatDispatcher) log() btclog.Logger {
	// There's no guarantee that the `b.beat` is initialized when the
	// dispatcher shuts down, especially in the case where the node is
	// running as a remote signer, which doesn't have a chainbackend. In
	// that case we will use the package logger.
	if b.beat == nil {
		return clog
	}

	return b.beat.logger()
}

// dispatchBlocks listens to new block epoch and dispatches it to all the
// consumers. Each queue is notified concurrently, and the consumers in the
// same queue are notified sequentially.
//
// NOTE: Must be run as a goroutine.
func (b *BlockbeatDispatcher) dispatchBlocks(
	blockEpochs *chainntnfs.BlockEpochEvent) {

	defer b.wg.Done()
	defer blockEpochs.Cancel()

	for {
		select {
		case blockEpoch, ok := <-blockEpochs.Epochs:
			if !ok {
				clog.Debugf("Block epoch channel closed")

				return
			}

			// Log a separator so it's easier to identify when a
			// new block arrives for subsystems.
			clog.Debugf("%v", lnutils.NewSeparatorClosure())

			clog.Debugf("Received new block %v at height %d, "+
				"notifying consumers...", blockEpoch.Hash,
				blockEpoch.Height)

			// Record the time it takes the consumer to process
			// this block.
			start := time.Now()

			// Update the current block epoch.
			b.beat = NewBeat(*blockEpoch)

			// Notify all consumers.
			err := b.notifyQueues()
			if err != nil {
				b.log().Errorf("Notify block failed: %v", err)
			}

			b.log().Debugf("Notified all consumers on new block "+
				"in %v", time.Since(start))

		// A query has been made to fetch the current height, we now
		// send the height from its current beat.
		case query := <-b.queryHeightChan:
			// The beat may not be set yet, e.g., during the startup
			// the query is made before the block epoch being sent.
			height := int32(0)
			if b.beat != nil {
				height = b.beat.Height()
			}

			query.respChan <- height

		case <-b.quit:
			b.log().Debugf("BlockbeatDispatcher quit signal " +
				"received")

			return
		}
	}
}

// CurrentHeight returns the current best height known to the dispatcher. 0 is
// returned if the dispatcher is shutting down.
func (b *BlockbeatDispatcher) CurrentHeight() int32 {
	query := newQuery()

	select {
	case b.queryHeightChan <- query:

	case <-b.quit:
		clog.Debugf("BlockbeatDispatcher quit before query")
		return 0
	}

	select {
	case height := <-query.respChan:
		clog.Debugf("Responded current height: %v", height)
		return height

	case <-b.quit:
		clog.Debugf("BlockbeatDispatcher quit before response")
		return 0
	}
}

// notifyQueues notifies each queue concurrently about the latest block epoch.
func (b *BlockbeatDispatcher) notifyQueues() error {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[uint32]chan error, len(b.consumerQueues))

	// Notify each queue in goroutines.
	for qid, consumers := range b.consumerQueues {
		b.log().Debugf("Notifying queue=%d with %d consumers", qid,
			len(consumers))

		// Create a signal chan.
		errChan := make(chan error, 1)
		errChans[qid] = errChan

		// Notify each queue concurrently.
		go func(qid uint32, c []Consumer, beat Blockbeat) {
			// Notify each consumer in this queue sequentially.
			errChan <- DispatchSequential(beat, c)
		}(qid, consumers, b.beat)
	}

	// Wait for all consumers in each queue to finish.
	for qid, errChan := range errChans {
		select {
		case err := <-errChan:
			if err != nil {
				return fmt.Errorf("queue=%d got err: %w", qid,
					err)
			}

			b.log().Debugf("Notified queue=%d", qid)

		case <-b.quit:
			b.log().Debugf("BlockbeatDispatcher quit signal " +
				"received, exit notifyQueues")

			return nil
		}
	}

	return nil
}

// DispatchSequential takes a list of consumers and notify them about the new
// epoch sequentially. It requires the consumer to finish processing the block
// within the specified time, otherwise a timeout error is returned.
func DispatchSequential(b Blockbeat, consumers []Consumer) error {
	for _, c := range consumers {
		// Send the beat to the consumer.
		err := notifyAndWait(b, c, DefaultProcessBlockTimeout)
		if err != nil {
			b.logger().Errorf("Failed to process block: %v", err)

			return err
		}
	}

	return nil
}

// DispatchConcurrent notifies each consumer concurrently about the blockbeat.
// It requires the consumer to finish processing the block within the specified
// time, otherwise a timeout error is returned.
func DispatchConcurrent(b Blockbeat, consumers []Consumer) error {
	eg := &errgroup.Group{}

	// Notify each queue in goroutines.
	for _, c := range consumers {
		// Notify each consumer concurrently.
		eg.Go(func() error {
			// Send the beat to the consumer.
			err := notifyAndWait(b, c, DefaultProcessBlockTimeout)

			// Exit early if there's no error.
			if err == nil {
				return nil
			}

			b.logger().Errorf("Consumer=%v failed to process "+
				"block: %v", c.Name(), err)

			return err
		})
	}

	// Wait for all consumers in each queue to finish.
	if err := eg.Wait(); err != nil {
		return err
	}

	return nil
}

// notifyAndWait sends the blockbeat to the specified consumer. It requires the
// consumer to finish processing the block within the specified time, otherwise
// a timeout error is returned.
func notifyAndWait(b Blockbeat, c Consumer, timeout time.Duration) error {
	b.logger().Debugf("Waiting for consumer[%s] to process it", c.Name())

	// Record the time it takes the consumer to process this block.
	start := time.Now()

	errChan := make(chan error, 1)
	go func() {
		errChan <- c.ProcessBlock(b)
	}()

	// We expect the consumer to finish processing this block under 30s,
	// otherwise a timeout error is returned.
	select {
	case err := <-errChan:
		if err == nil {
			break
		}

		return fmt.Errorf("%s got err in ProcessBlock: %w", c.Name(),
			err)

	case <-time.After(timeout):
		return fmt.Errorf("consumer %s: %w", c.Name(),
			ErrProcessBlockTimeout)
	}

	b.logger().Debugf("Consumer[%s] processed block in %v", c.Name(),
		time.Since(start))

	return nil
}
