package actor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestFutureAwaitContextCancellation tests that Await respects context
// cancellation if the context is cancelled before the future resolves.
func TestFutureAwaitContextCancellation(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// Test cancellation when the Await context is cancelled via
		// context.Cancel. The underlying future will not be completed, allowing
		// us to test the cancellation path of Await.
		prom1 := NewPromise[int]()
		fut1 := prom1.Future()
		ctx1, cancel1 := context.WithCancel(context.Background())

		// We'll cancel the future immediately after creating it.
		cancel1()

		result1 := fut1.Await(ctx1)
		if !result1.IsErr() ||
			!errors.Is(result1.Err(), context.Canceled) {
			t.Fatalf("await with immediate cancel: expected "+
				"context.Canceled, got %v", result1.Err())
		}

		// Test cancellation when the Await context times out. The
		// underlying future will also not be completed.
		prom2 := NewPromise[int]()
		fut2 := prom2.Future()

		// Use a very short timeout that will trigger.
		ctx2, cancel2 := context.WithTimeout(
			context.Background(), 1*time.Nanosecond,
		)
		defer cancel2()

		// Await the future; it should fall through to the timeout
		// because the future itself is not completed.
		result2 := fut2.Await(ctx2)
		if !result2.IsErr() ||
			!errors.Is(result2.Err(), context.DeadlineExceeded) {

			t.Fatalf("await with timeout: expected "+
				"context.DeadlineExceeded, got %v",
				result2.Err())
		}
	})
}

// TestFutureAwaitFutureCompletes tests that Await returns the future's
// result if the context is not cancelled before the future resolves.
func TestFutureAwaitFutureCompletes(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		valToSet := rapid.Int().Draw(t, "valToSet")

		// With a 50% chance, configure the test to complete the future
		// with an error instead of a successful value.
		var errToSet error
		if rapid.Bool().Draw(t, "have_error") {
			errToSet = fmt.Errorf("err")
		}

		promise := NewPromise[int]()
		fut := promise.Future()

		// Use a background context for Await, as we expect the future
		// to complete normally.
		ctx := context.Background()

		// Complete the future in a separate goroutine to simulate an
		// asynchronous operation.
		go func() {
			if errToSet != nil {
				promise.Complete(fn.Err[int](errToSet))
			} else {
				promise.Complete(fn.Ok(valToSet))
			}
		}()

		// Now we'll wait for the future to complete, then verify below
		// that the result (value or error) is as expected.
		result := fut.Await(ctx)

		if errToSet != nil {
			// If an error was set, verify that Await returns that
			// specific error.
			if !result.IsErr() ||
				!errors.Is(result.Err(), errToSet) {

				t.Fatalf("await with error: expected "+
					"error %v, got %v", errToSet,
					result.Err())
			}
		} else {
			// If no error was set, verify that Await returns the
			// correct value.
			if result.IsErr() {
				t.Fatalf("await with value: expected success, "+
					"got error %v", result.Err())
			}
			result.WhenOk(func(val int) {
				if val != valToSet {
					t.Fatalf("await with value: "+
						"expected %v, got %v",
						valToSet, val)
				}
			})
		}
	})
}

// TestFutureThenApplyContextCancellation tests that ThenApply respects its
// context, yielding a context error if cancelled before the original future
// completes.
func TestFutureThenApplyContextCancellation(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// The original future will not be completed in this test case,
		// allowing us to specifically test the cancellation behavior of
		// the context passed to ThenApply.
		originalPromise := NewPromise[int]()
		originalFut := originalPromise.Future()

		// Create a context for ThenApply and cancel it immediately.
		ctxApply, cancelApply := context.WithCancel(
			context.Background(),
		)
		cancelApply()

		var transformCalled atomic.Bool
		transform := func(i int) int {
			transformCalled.Store(true)
			return i * 2
		}

		// Register the transformation. The ThenApply operation itself
		// will start a goroutine to await the originalFut.
		newFut := originalFut.ThenApply(ctxApply, transform)

		// Await the new (transformed) future. Use a background context
		// for this Await to isolate the test to the cancellation of
		// ctxApply.
		result := newFut.Await(context.Background())

		if !result.IsErr() ||
			!errors.Is(result.Err(), context.Canceled) {

			t.Fatalf("ThenApply with cancelled context: expected "+
				"context.Canceled, got %v", result.Err())
		}
		if transformCalled.Load() {
			t.Fatal("ThenApply transform function called " +
				"despite context cancellation")
		}
	})
}

// TestFutureThenApplyOriginalFutureCompletes tests ThenApply's behavior when
// the original future completes (with a value or error) before ThenApply's
// context is cancelled.
func TestFutureThenApplyOriginalFutureCompletes(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		initialVal := rapid.Int().Draw(t, "initialVal")

		// Configure whether the original future completes with an error
		// or a successful value.
		var originalErr error
		if rapid.Bool().Draw(t, "have_error") {
			originalErr = fmt.Errorf("original error")
		}

		originalPromise := NewPromise[int]()
		originalFut := originalPromise.Future()

		// Create a context for ThenApply that should not cancel before
		// the original future completes.
		ctxApply, cancelApply := context.WithTimeout(
			context.Background(), 50*time.Millisecond,
		)
		defer cancelApply()

		var transformCalled atomic.Bool
		transform := func(i int) int {
			transformCalled.Store(true)
			return i * 2
		}

		newFut := originalFut.ThenApply(ctxApply, transform)

		// Complete the original future in a separate goroutine to
		// simulate asynchrony.
		go func() {
			if originalErr != nil {
				originalPromise.Complete(
					fn.Err[int](originalErr),
				)
			} else {
				originalPromise.Complete(fn.Ok(initialVal))
			}
		}()

		// Await our new future which transforms the original future's
		// result. Use a background context for this Await.
		result := newFut.Await(context.Background())

		if originalErr != nil {
			// If the original future had an error, the transformed
			// future should also yield that same error.
			if !result.IsErr() ||
				!errors.Is(result.Err(), originalErr) {

				t.Fatalf("ThenApply with original error: "+
					"expected error %v, got %v",
					originalErr, result.Err())
			}
			if transformCalled.Load() {
				t.Fatal("ThenApply transform function called " +
					"despite original future having " +
					"an error")
			}
		} else {
			// If the original future completed successfully, the
			// transformed future should contain the transformed value.
			if result.IsErr() {
				t.Fatalf("ThenApply with original value: "+
					"expected success, got "+
					"error %v", result.Err())
			}

			if !transformCalled.Load() {
				t.Fatal("ThenApply transform function not " +
					"called for successful original future")
			}

			result.WhenOk(func(val int) {
				expectedTransformedVal := initialVal * 2
				if val != expectedTransformedVal {
					t.Fatalf("ThenApply with original "+
						"value: expected "+
						"transformed %v, got %v",
						expectedTransformedVal, val)
				}
			})
		}
	})
}

// TestFutureOnCompleteContextCancellation tests that OnComplete's callback
// receives a context error if its context is cancelled before the future
// completes.
func TestFutureOnCompleteContextCancellation(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// The original future will not complete in this test, allowing
		// us to focus on the cancellation of OnComplete's context.
		originalPromise := NewPromise[int]()
		originalFut := originalPromise.Future()

		// Create a context for OnComplete and cancel it immediately to
		// simulate a premature cancellation.
		ctxComplete, cancelComplete := context.WithCancel(
			context.Background(),
		)
		cancelComplete()

		var wg sync.WaitGroup
		wg.Add(1)
		var (
			callbackInvoked     atomic.Bool
			callbackResultValue fn.Result[int]

			// mu is a mutex to protect callbackResultValue as it's
			// written by the callback goroutine and read by the
			// test goroutine.
			mu sync.Mutex
		)

		// Register an OnComplete callback. The callback itself runs in
		// a new goroutine started by OnComplete.
		originalFut.OnComplete(ctxComplete, func(res fn.Result[int]) {
			mu.Lock()
			callbackResultValue = res
			mu.Unlock()

			callbackInvoked.Store(true)
			wg.Done()
		})

		// Use a wait group and a channel to wait for the callback to
		// be invoked.
		waitChan := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitChan)
		}()

		select {
		// The callback should be invoked, even if with a context error.
		case <-waitChan:
		case <-time.After(50 * time.Millisecond):
			t.Fatal("OnComplete callback timed out " +
				"waiting for execution after context cancel")
		}

		require.True(
			t, callbackInvoked.Load(),
			"OnComplete callback not invoked",
		)

		mu.Lock()
		defer mu.Unlock()

		// Verify that the callback received a context.Canceled error
		// because its context (ctxComplete) was cancelled.
		if !callbackResultValue.IsErr() ||
			!errors.Is(callbackResultValue.Err(),
				context.Canceled) {

			t.Fatalf("OnComplete with cancelled context: callback "+
				"expected context.Canceled, got %v",
				callbackResultValue.Err())
		}
	})
}

// TestFutureOnCompleteFutureCompletes tests OnComplete's behavior when the
// future completes (with value or error) before its context is cancelled.
func TestFutureOnCompleteFutureCompletes(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		valToSet := rapid.Int().Draw(t, "valToSet")

		// Configure whether the original future completes with an error
		// or a successful value.
		var originalErr error
		if rapid.Bool().Draw(t, "have_error") {
			originalErr = fmt.Errorf("original error")
		}

		originalPromise := NewPromise[int]()
		originalFut := originalPromise.Future()

		// Use a background context for OnComplete, as we expect the
		// future to complete normally.
		ctxComplete := context.Background()

		var wg sync.WaitGroup
		wg.Add(1)

		var (
			callbackInvoked     atomic.Bool
			callbackResultValue fn.Result[int]
			mu                  sync.Mutex
		)

		// Register an OnComplete callback. This callback will execute
		// once the originalFut completes.
		originalFut.OnComplete(ctxComplete, func(res fn.Result[int]) {
			mu.Lock()
			callbackResultValue = res
			mu.Unlock()

			callbackInvoked.Store(true)

			wg.Done()
		})

		// Complete the original future in a separate goroutine to
		// simulate an asynchronous operation.
		go func() {
			if originalErr != nil {
				originalPromise.Complete(
					fn.Err[int](originalErr),
				)
			} else {
				originalPromise.Complete(fn.Ok(valToSet))
			}
		}()

		// Use a wait group and a channel to wait for the callback's
		// execution.
		waitChan := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitChan)
		}()

		select {
		// The callback should be invoked as the future completes.
		case <-waitChan:
		case <-time.After(50 * time.Millisecond):
			t.Fatal("OnComplete callback timed out " +
				"waiting for execution")
		}

		require.True(t, callbackInvoked.Load())

		mu.Lock()
		defer mu.Unlock()

		// Verify that the callback received the correct result (either
		// the error or the value from the completed future).
		if originalErr != nil {
			if !callbackResultValue.IsErr() ||
				!errors.Is(callbackResultValue.Err(),
					originalErr) {

				t.Fatalf("OnComplete with error: callback "+
					"expected error %v, got %v",
					originalErr,
					callbackResultValue.Err())
			}
		} else {
			if callbackResultValue.IsErr() {
				t.Fatalf("OnComplete with value: callback "+
					"expected success, got error %v",
					callbackResultValue.Err())
			}
			callbackResultValue.WhenOk(func(val int) {
				if val != valToSet {
					t.Fatalf("OnComplete with value: "+
						"callback expected %v, got %v",
						valToSet, val)
				}
			})
		}
	})
}
