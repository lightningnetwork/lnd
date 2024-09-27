package fn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGoroutineManager tests that the GoroutineManager starts goroutines until
// ctx expires. It also makes sure it fails to start new goroutines after the
// context expired and the GoroutineManager is in the process of waiting for
// already started goroutines in the Stop method.
func TestGoroutineManager(t *testing.T) {
	t.Parallel()

	m := NewGoroutineManager(context.Background())

	taskChan := make(chan struct{})

	require.NoError(t, m.Go(func(ctx context.Context) {
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
	require.ErrorIs(t, m.Go(func(ctx context.Context) {}), ErrStopping)

	// When Stop() is called, the internal context expires and m.Done() is
	// closed. Test this.
	select {
	case <-m.Done():
	default:
		t.Errorf("Done() channel must be closed at this point")
	}
}

// TestGoroutineManagerContextExpires tests the effect of context expiry.
func TestGoroutineManagerContextExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	m := NewGoroutineManager(ctx)

	require.NoError(t, m.Go(func(ctx context.Context) {
		<-ctx.Done()
	}))

	// The Done channel of the manager should not be closed, so the
	// following call must block.
	select {
	case <-m.Done():
		t.Errorf("Done() channel must not be closed at this point")
	default:
	}

	cancel()

	// The Done channel of the manager should be closed, so the following
	// call must not block.
	select {
	case <-m.Done():
	default:
		t.Errorf("Done() channel must be closed at this point")
	}

	// Make sure new goroutines do not start after context expiry.
	require.ErrorIs(t, m.Go(func(ctx context.Context) {}), ErrStopping)

	// Stop will wait for all goroutines to stop.
	m.Stop()
}

// TestGoroutineManagerStress starts many goroutines while calling Stop. It
// is needed to make sure the GoroutineManager does not crash if this happen.
// If the mutex was not used, it would crash because of a race condition between
// wg.Add(1) and wg.Wait().
func TestGoroutineManagerStress(t *testing.T) {
	t.Parallel()

	m := NewGoroutineManager(context.Background())

	stopChan := make(chan struct{})

	time.AfterFunc(1*time.Millisecond, func() {
		m.Stop()
		close(stopChan)
	})

	// Starts 100 goroutines sequentially. Sequential order is needed to
	// keep wg.counter low (0 or 1) to increase probability of race
	// condition to be caught if it exists. If mutex is removed in the
	// implementation, this test crashes under `-race`.
	for i := 0; i < 100; i++ {
		taskChan := make(chan struct{})
		err := m.Go(func(ctx context.Context) {
			close(taskChan)
		})
		// If goroutine was started, wait for its completion.
		if err == nil {
			<-taskChan
		}
	}

	// Wait for Stop to complete.
	<-stopChan
}
