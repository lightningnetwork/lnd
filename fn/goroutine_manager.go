package fn

import (
	"context"
	"sync"
)

// GoroutineManager is used to launch goroutines until context expires or the
// manager is stopped. The Stop method blocks until all started goroutines stop.
type GoroutineManager struct {
	wg     sync.WaitGroup
	mu     sync.Mutex
	ctx    context.Context
	cancel func()
}

// NewGoroutineManager constructs and returns a new instance of
// GoroutineManager.
func NewGoroutineManager(ctx context.Context) *GoroutineManager {
	ctx, cancel := context.WithCancel(ctx)

	return &GoroutineManager{
		ctx:    ctx,
		cancel: cancel,
	}
}

// Go tries to start a new goroutine and returns a boolean indicating its
// success. It fails iff the goroutine manager is stopping or its context passed
// to NewGoroutineManager has expired.
func (g *GoroutineManager) Go(f func(ctx context.Context)) bool {
	// Calling wg.Add(1) and wg.Wait() when wg's counter is 0 is a race
	// condition, since it is not clear should Wait() block or not. This
	// kind of race condition is detected by Go runtime and results in a
	// crash if running with `-race`. To prevent this, whole Go method is
	// protected with a mutex. The call to wg.Wait() inside Stop() can still
	// run in parallel with Go, but in that case g.ctx is in expired state,
	// because cancel() was called in Stop, so Go returns before wg.Add(1)
	// call.
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.ctx.Err() != nil {
		return false
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		f(g.ctx)
	}()

	return true
}

// Stop prevents new goroutines from being added and waits for all running
// goroutines to finish.
func (g *GoroutineManager) Stop() {
	g.mu.Lock()
	g.cancel()
	g.mu.Unlock()

	// Wait for all goroutines to finish. Note that this wg.Wait() call is
	// safe, since it can't run in parallel with wg.Add(1) call in Go, since
	// we just cancelled the context and even if Go call starts running here
	// after acquiring the mutex, it would see that the context has expired
	// and return false instead of calling wg.Add(1).
	g.wg.Wait()
}

// Done returns a channel which is closed when either the context passed to
// NewGoroutineManager expires or when Stop is called.
func (g *GoroutineManager) Done() <-chan struct{} {
	return g.ctx.Done()
}
