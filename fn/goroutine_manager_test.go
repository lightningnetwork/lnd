package fn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGoroutineManager tests the behaviour of the GoroutineManager.
func TestGoroutineManager(t *testing.T) {
	t.Parallel()

	// Here we test that the GoroutineManager starts goroutines until it has
	// been stopped.
	t.Run("GM is stopped", func(t *testing.T) {
		t.Parallel()

		var (
			ctx      = t.Context()
			m        = NewGoroutineManager()
			taskChan = make(chan struct{})
		)

		// The gm has not stopped yet and the passed in context has not
		// expired, so we expect the goroutine to start. The taskChan is
		// blocking, so this goroutine will be live for a while.
		require.True(t, m.Go(ctx, func(ctx context.Context) {
			<-taskChan
		}))

		t1 := time.Now()

		// Close taskChan in 1s, causing the goroutine to stop.
		time.AfterFunc(time.Second, func() {
			close(taskChan)
		})

		m.Stop()
		stopDelay := time.Since(t1)

		// Make sure Stop was waiting for the goroutine to stop.
		require.Greater(t, stopDelay, time.Second)

		// Make sure new goroutines do not start after Stop.
		require.False(t, m.Go(ctx, func(ctx context.Context) {}))

		// When Stop() is called, gm quit channel has been closed and so
		// Done() should return.
		select {
		case <-m.Done():
		default:
			t.Errorf("Done() channel must be closed at this point")
		}
	})

	// Test that the GoroutineManager fails to start a goroutine or exits a
	// goroutine if the caller context has expired.
	t.Run("Caller context expires", func(t *testing.T) {
		t.Parallel()

		var (
			ctx      = t.Context()
			m        = NewGoroutineManager()
			taskChan = make(chan struct{})
		)

		// Derive a child context with a cancel function.
		ctxc, cancel := context.WithCancel(ctx)

		// The gm has not stopped yet and the passed in context has not
		// expired, so we expect the goroutine to start.
		require.True(t, m.Go(ctxc, func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-taskChan:
				t.Fatalf("The task was performed when it " +
					"should not have")
			}
		}))

		// Give the GM a little bit of time to start the goroutine so
		// that we can be sure that it is already listening on the
		// ctx and taskChan before calling cancel.
		time.Sleep(time.Millisecond * 500)

		// Cancel the context so that the goroutine exits.
		cancel()

		// Attempt to send a signal on the task channel, nothing should
		// happen since the goroutine has already exited.
		select {
		case taskChan <- struct{}{}:
		case <-time.After(time.Millisecond * 200):
		}

		// Again attempt to add a goroutine with the same cancelled
		// context. This should fail since the context has already
		// expired.
		require.False(t, m.Go(ctxc, func(ctx context.Context) {
			t.Fatalf("The goroutine should not have started")
		}))

		// Stop the goroutine manager.
		m.Stop()
	})

	// Start many goroutines while calling Stop. We do this to make sure
	// that the GoroutineManager does not crash when these calls are done in
	// parallel because of the potential race between wg.Add() and
	// wg.Done() when the wg counter is 0.
	t.Run("Stress test", func(t *testing.T) {
		t.Parallel()

		var (
			ctx      = t.Context()
			m        = NewGoroutineManager()
			stopChan = make(chan struct{})
		)

		time.AfterFunc(1*time.Millisecond, func() {
			m.Stop()
			close(stopChan)
		})

		// Start 100 goroutines sequentially. Sequential order is
		// needed to keep wg.counter low (0 or 1) to increase
		// probability of the race condition to triggered if it exists.
		// If mutex is removed in the implementation, this test crashes
		// under `-race`.
		for i := 0; i < 100; i++ {
			taskChan := make(chan struct{})
			ok := m.Go(ctx, func(ctx context.Context) {
				close(taskChan)
			})
			// If goroutine was started, wait for its completion.
			if ok {
				<-taskChan
			}
		}

		// Wait for Stop to complete.
		<-stopChan
	})
}
