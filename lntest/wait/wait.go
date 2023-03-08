package wait

import (
	"fmt"
	"time"
)

// PollInterval is a constant specifying a 200 ms interval.
const PollInterval = 200 * time.Millisecond

// Predicate is a helper test function that will wait for a timeout period of
// time until the passed predicate returns true. This function is helpful as
// timing doesn't always line up well when running integration tests with
// several running lnd nodes. This function gives callers a way to assert that
// some property is upheld within a particular time frame.
//
// TODO(yy): build a counter here so we know how many times we've tried the
// `pred`.
func Predicate(pred func() bool, timeout time.Duration) error {
	exitTimer := time.After(timeout)
	result := make(chan bool, 1)

	for {
		<-time.After(PollInterval)

		go func() {
			result <- pred()
		}()

		// Each time we call the pred(), we expect a result to be
		// returned otherwise it will timeout.
		select {
		case <-exitTimer:
			return fmt.Errorf("predicate not satisfied after " +
				"time out")

		case succeed := <-result:
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
