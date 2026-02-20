package wait

import (
	"fmt"
	"time"
)

// PollInterval is a constant specifying a 200 ms interval.
const PollInterval = 200 * time.Millisecond

// Predicate is a helper function that waits for a specified timeout period
// until the provided predicate returns true. This is useful in scenarios where
// timing is uncertain, allowing callers to assert that a condition becomes true
// within a given time frame.
func Predicate(pred func() bool, timeout time.Duration) error {
	var (
		// exitTimer is a channel that signals when the timeout period
		// has elapsed. All predicate calls must complete before this
		// signal.
		exitTimer = time.After(timeout)

		// predCallCount tracks how many times the predicate function
		// has been called.
		predCallCount int
	)

	for {
		// Wait for the polling interval before subsequent predicate
		// calls.
		if predCallCount > 0 {
			<-time.After(PollInterval)
		}

		// exitPredGoroutine is closed to signal termination of the
		// goroutine running the predicate, preventing leaks if a
		// timeout occurs.
		exitPredGoroutine := make(chan struct{})

		// predResult receives the boolean result from the predicate
		// function.
		predResult := make(chan bool, 1)

		go func() {
			select {
			case predResult <- pred():
			case <-exitPredGoroutine:
			}
		}()

		predCallCount++

		// Wait for either the predicate to return or the timeout to
		// occur.
		select {
		case <-exitTimer:
			close(exitPredGoroutine)
			return fmt.Errorf("predicate not satisfied before "+
				"timeout (pred_call_count=%d)", predCallCount)

		case succeed := <-predResult:
			if succeed {
				return nil
			}
		}
	}
}

// NoError is a wrapper around Predicate that waits for the passed method f to
// execute without error, and returns the last error encountered if this doesn't
// happen within the timeout.
func NoError(f func() error, timeout time.Duration) error {
	var predErr error
	pred := func() bool {
		if err := f(); err != nil {
			predErr = err
			return false
		}
		return true
	}

	// If f() doesn't succeed within the timeout, return the last
	// encountered error.
	if err := Predicate(pred, timeout); err != nil {
		// Handle the case where the passed in method, f, hangs for the
		// full timeout
		if predErr == nil {
			return fmt.Errorf("method did not return within the " +
				"timeout")
		}

		return predErr
	}

	return nil
}

// Invariant is a helper test function that will wait for a timeout period of
// time, verifying that a statement remains true for the entire duration.  This
// function is helpful as timing doesn't always line up well when running
// integration tests with several running lnd nodes. This function gives callers
// a way to assert that some property is maintained over a particular time
// frame.
func Invariant(statement func() bool, timeout time.Duration) error {
	const pollInterval = 20 * time.Millisecond

	exitTimer := time.After(timeout)
	for {
		<-time.After(pollInterval)

		// Fail if the invariant is broken while polling.
		if !statement() {
			return fmt.Errorf("invariant broken before time out")
		}

		select {
		case <-exitTimer:
			return nil
		default:
		}
	}
}

// InvariantNoError is a wrapper around Invariant that waits out the duration
// specified by timeout. It fails if the predicate ever returns an error during
// that time.
func InvariantNoError(f func() error, timeout time.Duration) error {
	var predErr error
	pred := func() bool {
		if err := f(); err != nil {
			predErr = err
			return false
		}
		return true
	}

	if err := Invariant(pred, timeout); err != nil {
		return predErr
	}

	return nil
}
