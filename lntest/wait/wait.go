package wait

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/fn"
)

// PollInterval is a constant specifying a 200 ms interval.
const PollInterval = 200 * time.Millisecond

// predicateOptions holds the options for the Predicate function.
type predicateOptions struct {
	// MaxCallAttempts is the maximum number of target function call
	// attempts possible before returning an error.
	MaxCallAttempts fn.Option[uint64]
}

// defaultPredicateOptions returns the default options for the Predicate
// function.
func defaultPredicateOptions() *predicateOptions {
	return &predicateOptions{
		MaxCallAttempts: fn.None[uint64](),
	}
}

// PredicateOpt is a functional option that can be passed to the Predicate
// function.
type PredicateOpt func(*predicateOptions)

// WithPredicateMaxCallAttempts is a functional option that can be passed to the
// Predicate function to set the maximum number of target function call
// attempts.
func WithPredicateMaxCallAttempts(attempts uint64) PredicateOpt {
	return func(o *predicateOptions) {
		o.MaxCallAttempts = fn.Some(attempts)
	}
}

// Predicate is a helper test function that will wait for a timeout period of
// time until the passed predicate returns true. This function is helpful as
// timing doesn't always line up well when running integration tests with
// several running lnd nodes. This function gives callers a way to assert that
// some property is upheld within a particular time frame.
//
// TODO(yy): build a counter here so we know how many times we've tried the
// `pred`.
func Predicate(pred func() bool, timeout time.Duration,
	opts ...PredicateOpt) error {

	options := defaultPredicateOptions()
	for _, opt := range opts {
		opt(options)
	}

	exitTimer := time.After(timeout)
	result := make(chan bool, 1)

	// We'll keep track of the number of times we've called the predicate.
	callAttemptsCounter := uint64(0)

	for {
		<-time.After(PollInterval)

		// We'll increment the call attempts counter each time we call
		// the predicate.
		callAttemptsCounter += 1

		go func() {
			result <- pred()
		}()

		// Each time we call the pred(), we expect a result to be
		// returned otherwise it will time out.
		select {
		case <-exitTimer:
			return fmt.Errorf("predicate not satisfied after " +
				"time out")

		case succeed := <-result:
			if succeed {
				return nil
			}

			// If we have a max call attempts set, we'll check if
			// we've exceeded it and return an error if so.
			maxCallAttempts := options.MaxCallAttempts.UnwrapOr(
				callAttemptsCounter + 1,
			)
			if callAttemptsCounter >= maxCallAttempts {
				return fmt.Errorf("predicate not satisfied "+
					"after max call attempts: %d",
					maxCallAttempts)
			}
		}
	}
}

// noErrorOptions holds the options for the NoError function.
type noErrorOptions struct {
	// maxCallAttempts is the maximum number of target function call
	// attempts possible before returning an error.
	maxCallAttempts fn.Option[uint64]
}

// defaultNoErrorOptions returns the default options for the NoError function.
func defaultNoErrorOptions() *noErrorOptions {
	return &noErrorOptions{
		maxCallAttempts: fn.None[uint64](),
	}
}

// NoErrorOpt is a functional option that can be passed to the NoError function.
type NoErrorOpt func(*noErrorOptions)

// WithNoErrorMaxCallAttempts is a functional option that can be passed to the
// NoError function to set the maximum number of target function call attempts.
func WithNoErrorMaxCallAttempts(attempts uint64) NoErrorOpt {
	return func(o *noErrorOptions) {
		o.maxCallAttempts = fn.Some(attempts)
	}
}

// NoError is a wrapper around Predicate that waits for the passed method f to
// execute without error, and returns the last error encountered if this doesn't
// happen within the timeout.
func NoError(f func() error, timeout time.Duration,
	opts ...NoErrorOpt) error {

	options := defaultNoErrorOptions()
	for _, opt := range opts {
		opt(options)
	}

	// Formulate the options for the predicate function.
	withPredicateMaxAttempts := func(*predicateOptions) {}
	options.maxCallAttempts.WhenSome(func(attempts uint64) {
		withPredicateMaxAttempts = WithPredicateMaxCallAttempts(
			attempts,
		)
	})

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
	err := Predicate(pred, timeout, withPredicateMaxAttempts)
	if err != nil {
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
