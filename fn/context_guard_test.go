package fn

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContextGuard tests the behaviour of the ContextGuard.
func TestContextGuard(t *testing.T) {
	t.Parallel()

	defer GuardTest(t)()

	// Test that the derived context is cancelled when the passed context is
	// cancelled.
	t.Run("Parent context is cancelled", func(t *testing.T) {
		t.Parallel()
		var (
			ctx, cancel = context.WithCancel(t.Context())
			g           = NewContextGuard()
		)

		ctxc, _ := g.Create(ctx)

		// Cancel the parent context.
		cancel()
		// Assert that the derived context is cancelled.
		select {
		case <-ctxc.Done():
		default:
			t.Errorf("The derived context should be cancelled at " +
				"this point")
		}
	})

	// Test that the derived context is cancelled when the returned cancel
	// function is called.
	t.Run("Derived context is cancelled", func(t *testing.T) {
		t.Parallel()
		var (
			ctx = t.Context()
			g   = NewContextGuard()
		)

		ctxc, cancel := g.Create(ctx)

		// Cancel the context.
		cancel()

		// Assert that the derived context is cancelled.
		select {
		case <-ctxc.Done():
		default:
			t.Errorf("The derived context should be cancelled at " +
				"this point")
		}
	})

	// Test that the derived context is cancelled when the quit channel is
	// closed.
	t.Run("Quit channel is closed", func(t *testing.T) {
		t.Parallel()

		var (
			ctx = t.Context()
			g   = NewContextGuard()
		)
		ctxc, _ := g.Create(ctx)

		// Close the quit channel.
		g.Quit()

		// Assert that the derived context is cancelled.
		select {
		case <-ctxc.Done():
		default:
			t.Errorf("The derived context should be cancelled at " +
				"this point")
		}
	})

	t.Run("Parent context is already closed", func(t *testing.T) {
		t.Parallel()

		var (
			ctx, cancel = context.WithCancel(t.Context())
			g           = NewContextGuard()
		)
		cancel()

		ctxc, _ := g.Create(ctx)

		// Assert that the derived context is already cancelled.
		select {
		case <-ctxc.Done():
		default:
			t.Errorf("The derived context should be cancelled at " +
				"this point")
		}
	})

	t.Run("Quit channel is already closed", func(t *testing.T) {
		t.Parallel()

		var (
			ctx = t.Context()
			g   = NewContextGuard()
		)

		g.Quit()

		ctxc, _ := g.Create(ctx)

		// Assert that the derived context is already cancelled.
		select {
		case <-ctxc.Done():
		default:
			t.Errorf("The derived context should be cancelled at " +
				"this point")
		}
	})

	t.Run("Child context should be cancelled synchronously with "+
		"parent", func(t *testing.T) {

		t.Parallel()

		var (
			ctx, cancel = context.WithCancel(t.Context())
			g           = NewContextGuard()
			task        = make(chan struct{})
			done        = make(chan struct{})
		)
		// Derive a child context.
		ctxc, _ := g.Create(ctx)

		// Spin off a routine that exists cleanly if the child context
		// is cancelled but fails if the task is performed.
		go func() {
			defer close(done)
			select {
			case <-ctxc.Done():
			case <-task:
				t.Fatalf("should not get here")
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// First cancel the parent context. Then immediately execute the
		// task.
		cancel()
		close(task)

		// Wait for the goroutine to exit.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	})

	t.Run("Child context should be cancelled synchronously with the "+
		"close of the quit channel", func(t *testing.T) {

		t.Parallel()

		var (
			ctx  = t.Context()
			g    = NewContextGuard()
			task = make(chan struct{})
			done = make(chan struct{})
		)

		// Derive a child context.
		ctxc, _ := g.Create(ctx)

		// Spin off a routine that exists cleanly if the child context
		// is cancelled but fails if the task is performed.
		go func() {
			defer close(done)
			select {
			case <-ctxc.Done():
			case <-task:
				t.Fatalf("should not get here")
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// First cancel the parent context. Then immediately execute the
		// task.
		g.Quit()

		// Execute the task.
		close(task)

		// Wait for the goroutine to exit.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	})

	// Test that if we add the BlockingCGOpt option, then the context will
	// not be cancelled when the quit channel is closed but will be when the
	// cancel function is called.
	t.Run("Blocking context no timeout", func(t *testing.T) {
		t.Parallel()

		var (
			ctx  = t.Context()
			g    = NewContextGuard()
			task = make(chan struct{})
			done = make(chan struct{})
		)

		// Derive a blocking child context.
		ctxc, cancel := g.Create(ctx, WithBlockingCG())

		// Spin of a routine that will exit cleanly if the context is
		// cancelled but will fail if the task is performed.
		go func() {
			defer func() {
				done <- struct{}{}
			}()

			select {
			case <-ctxc.Done():
			case <-task:
				t.Fatalf("Expected context to be cancelled")
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// Cancel the context.
		cancel()

		// Attempt to perform the task.
		select {
		case task <- struct{}{}:
			t.Fatalf("Expected task to not be performed")
		default:
		}

		// Assert that the task goroutine has now completed.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Derive a new blocking child context.
		ctxc, cancel = g.Create(ctx, WithBlockingCG())

		// Repeat the task but this time, we will call Quit first, but
		// since this is a blocking context, the context should not be
		// cancelled and the task _should_ be performed.
		go func() {
			defer func() {
				done <- struct{}{}
			}()

			select {
			case <-ctxc.Done():
				t.Fatalf("Expected task to be performed")
			case <-task:
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// Close the quit channel. This should NOT cause the context
		// to be cancelled.
		g.Quit()

		// Now, perform the task.
		select {
		case task <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Assert that the task goroutine has now completed.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Cancel the context.
		cancel()

		// Make sure wg's counter gets to 0 eventually.
		g.WgWait()
	})

	// Test that if we add the CustomTimeoutCGOpt option, then the context
	// will be not be cancelled when the quit channel is closed but will be
	// if either the context is cancelled or the timeout is reached.
	t.Run("Blocking context with timeout", func(t *testing.T) {
		t.Parallel()

		var (
			ctx     = t.Context()
			g       = NewContextGuard()
			task    = make(chan struct{})
			done    = make(chan struct{})
			timeout = time.Millisecond * 500
		)

		// Derive a blocking child context.
		ctxc, cancel := g.Create(
			ctx, WithBlockingCG(), WithCustomTimeoutCG(timeout),
		)

		// Spin of a routine that will exit cleanly if the context is
		// cancelled but will fail if the task is performed.
		go func() {
			defer func() {
				done <- struct{}{}
			}()

			select {
			case <-ctxc.Done():
			case <-task:
				t.Fatalf("Expected context to be cancelled")
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// Cancel the context.
		cancel()

		// Attempt to perform the task.
		select {
		case task <- struct{}{}:
			t.Fatalf("Expected task to not be performed")
		default:
		}

		// Assert that the task goroutine has now completed.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Derive a new blocking child context with a timeout.
		ctxc, cancel = g.Create(
			ctx, WithBlockingCG(), WithCustomTimeoutCG(timeout),
		)

		// Repeat the task but this time, but this time, we will assert
		// that the context is cancelled if the timeout is reached.
		// We will again fail if the task is performed.
		go func() {
			defer func() {
				done <- struct{}{}
			}()

			select {
			case <-ctxc.Done():
			case <-task:
				t.Fatalf("Expected context to be cancelled")
			}
		}()

		// Wait for the timeout to be reached.
		time.Sleep(timeout + time.Millisecond*100)

		// Attempt to perform the task.
		select {
		case task <- struct{}{}:
			t.Fatalf("Expected task to not be performed")
		default:
		}

		// Assert that the task goroutine has now completed.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Finally, repeat the task but this time show that calling
		// Quit does not cancel the context and that the task still gets
		// performed if it takes place before the context is timed out.
		ctxc, cancel = g.Create(
			ctx, WithBlockingCG(), WithCustomTimeoutCG(timeout),
		)

		go func() {
			defer func() {
				done <- struct{}{}
			}()

			select {
			case <-ctxc.Done():
				t.Fatalf("Expected the task to be performed")
			case <-task:
			}
		}()

		// Give the goroutine above a chance to spin up so that it's
		// waiting on the select.
		time.Sleep(time.Millisecond * 200)

		// Close the quit channel. This should NOT cause the context
		// to be cancelled.
		g.Quit()

		// Now, perform the task.
		select {
		case task <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}

		// Assert that the task goroutine has now completed.
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	})
}

// TestContextGuardCountGoroutines makes sure that ContextGuard doesn't create
// any goroutines while waiting for contexts.
func TestContextGuardCountGoroutines(t *testing.T) {
	// NOTE: t.Parallel() is not called in this test because it relies on an
	// accurate count of active goroutines. Running other tests in parallel
	// would introduce additional goroutines, leading to unreliable results.

	g := NewContextGuard()

	ctx, cancel := context.WithCancel(t.Context())

	// Count goroutines before contexts are created.
	count1 := runtime.NumGoroutine()

	// Create 1000 contexts of each type.
	for i := 0; i < 1000; i++ {
		_, _ = g.Create(ctx)
		_, _ = g.Create(ctx, WithBlockingCG())
		_, _ = g.Create(ctx, WithTimeoutCG())
		_, _ = g.Create(ctx, WithBlockingCG(), WithTimeoutCG())
	}

	// Make sure no new goroutine was launched.
	count2 := runtime.NumGoroutine()
	require.LessOrEqual(t, count2, count1)

	// Cancel root context.
	cancel()

	// Make sure wg's counter gets to 0 eventually.
	g.WgWait()
}
