package chainio

import (
	"fmt"
	"time"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
)

// DefaultProcessBlockTimeout is the timeout value used when waiting for one
// consumer to finish processing the new block epoch.
var DefaultProcessBlockTimeout = 30 * time.Second

// Beat implements the Blockbeat interface. It contains the block epoch and a
// buffer error chan.
//
// TODO(yy): extend this to check for confirmation status - which serves as the
// single source of truth, to avoid the potential race between receiving blocks
// and `GetTransactionDetails/RegisterSpendNtfn/RegisterConfirmationsNtfn`.
type Beat struct {
	// epoch is the current block epoch the blockbeat is aware of.
	epoch chainntnfs.BlockEpoch

	// errChan is a buffered chan that receives an error returned from
	// processing this block. This serves as the communication channel
	// between the BlockbeatDispatcher and the Consumer. The flow is,
	// - BlockbeatDispatcher sends a `Beat` to the Consumer and waits an
	//   error to be read from `Beat.errChan` or time out.
	// - Consumer receives this blockbeat in `ProcessBlock`.
	// - Consumer processes the block and calls `NotifyBlockProcessed`.
	// - BlockbeatDispatcher unblocks the current dispatch by reading the
	//   error from `Beat.errChan`.
	errChan chan error

	// log is the customized logger for the blockbeat which prints the
	// block height.
	log btclog.Logger
}

// Compile-time check to ensure Beat satisfies the Blockbeat interface.
var _ Blockbeat = (*Beat)(nil)

// NewBeat creates a new beat with the specified block epoch and a buffered
// error chan.
func NewBeat(epoch chainntnfs.BlockEpoch) Beat {
	b := Beat{
		epoch:   epoch,
		errChan: make(chan error, 1),
	}

	// Create a customized logger for the blockbeat.
	logPrefix := fmt.Sprintf("Height[%6d]:", b.Height())
	b.log = build.NewPrefixLog(logPrefix, clog)

	return b
}

// NewBeatFromHeight creates a new beat with the specified height and a
// buffered error chan. This method should only be used in test.
func NewBeatFromHeight(height int32) Beat {
	return NewBeat(chainntnfs.BlockEpoch{Height: height})
}

// Height returns the height of the block epoch.
//
// NOTE: Part of the Blockbeat interface.
func (b Beat) Height() int32 {
	return b.epoch.Height
}

// NOTE: Part of the Blockbeat interface.
func (b Beat) NotifyBlockProcessed(err error, quitChan chan struct{}) {
	fn.SendOrQuit(b.errChan, err, quitChan)
}

// DispatchSequential takes a list of consumers and notify them about the new
// epoch sequentially.
//
// NOTE: Part of the Blockbeat interface.
func (b Beat) DispatchSequential(consumers []Consumer) error {
	for _, c := range consumers {
		// Send the copy of the beat to the consumer.
		if err := b.notifyAndWait(c); err != nil {
			b.log.Errorf("Consumer=%v failed to process "+
				"block: %v", c.Name(), err)

			return err
		}
	}

	return nil
}

// DispatchConcurrent notifies each consumer concurrently about the blockbeat.
//
// NOTE: Part of the Blockbeat interface.
func (b Beat) DispatchConcurrent(consumers []Consumer) error {
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[string]chan error, len(consumers))

	// Notify each queue in goroutines.
	for _, c := range consumers {
		// Create a signal chan.
		errChan := make(chan error, 1)
		errChans[c.Name()] = errChan

		// Notify each consumer concurrently.
		go func(c Consumer, b Beat) {
			// Send the copy of the beat to the consumer.
			errChan <- b.notifyAndWait(c)
		}(c, b)
	}

	// Wait for all consumers in each queue to finish.
	for name, errChan := range errChans {
		err := <-errChan
		if err != nil {
			b.log.Errorf("Consumer=%v failed to process block: %v",
				name, err)

			return err
		}
	}

	return nil
}

// notifyAndWait sends the blockbeat to the specified consumer. It requires the
// consumer to finish processing the block under 30s, otherwise a timeout error
// is returned.
func (b Beat) notifyAndWait(c Consumer) error {
	// Construct a new beat with a buffered error chan.
	beatCopy := NewBeat(b.epoch)

	b.log.Debugf("Waiting for consumer[%v] to process it", c.Name())

	// Record the time it takes the consumer to process this block.
	start := time.Now()

	// We expect the consumer to finish processing this block under 30s,
	// otherwise a timeout error is returned.
	err, timeout := fn.RecvOrTimeout(
		c.ProcessBlock(beatCopy), DefaultProcessBlockTimeout,
	)
	if err != nil {
		return fmt.Errorf("%s: ProcessBlock got: %w", c.Name(), err)
	}
	if timeout != nil {
		return fmt.Errorf("%s timed out while processing block",
			c.Name())
	}

	b.log.Debugf("Consumer [%s] processed block in %v", c.Name(),
		time.Since(start))

	return nil
}
