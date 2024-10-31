package chainio

import (
	"errors"
	"fmt"
	"time"
)

// DefaultProcessBlockTimeout is the timeout value used when waiting for one
// consumer to finish processing the new block epoch.
var DefaultProcessBlockTimeout = 60 * time.Second

// ErrProcessBlockTimeout is the error returned when a consumer takes too long
// to process the block.
var ErrProcessBlockTimeout = errors.New("process block timeout")

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
	// errChans is a map of channels that will be used to receive errors
	// returned from notifying the consumers.
	errChans := make(map[string]chan error, len(consumers))

	// Notify each queue in goroutines.
	for _, c := range consumers {
		// Create a signal chan.
		errChan := make(chan error, 1)
		errChans[c.Name()] = errChan

		// Notify each consumer concurrently.
		go func(c Consumer, beat Blockbeat) {
			// Send the beat to the consumer.
			errChan <- notifyAndWait(
				b, c, DefaultProcessBlockTimeout,
			)
		}(c, b)
	}

	// Wait for all consumers in each queue to finish.
	for name, errChan := range errChans {
		err := <-errChan
		if err != nil {
			b.logger().Errorf("Consumer=%v failed to process "+
				"block: %v", name, err)

			return err
		}
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
