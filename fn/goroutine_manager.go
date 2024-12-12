package fn

import (
	"context"
	"sync"
	"sync/atomic"
)

// GoroutineManager is used to launch goroutines until context expires or the
// manager is stopped. The Stop method blocks until all started goroutines stop.
type GoroutineManager struct {
	// id is used to generate unique ids for each goroutine.
	id atomic.Uint32

	// cancelFns is a map of cancel functions that can be used to cancel the
	// context of a goroutine. The mutex must be held when accessing this
	// map. The key is the id of the goroutine.
	cancelFns map[uint32]context.CancelFunc

	mu sync.Mutex

	stopped sync.Once
	quit    chan struct{}
	wg      sync.WaitGroup
}

// NewGoroutineManager constructs and returns a new instance of
// GoroutineManager.
func NewGoroutineManager() *GoroutineManager {
	return &GoroutineManager{
		cancelFns: make(map[uint32]context.CancelFunc),
		quit:      make(chan struct{}),
	}
}

// addCancelFn adds a context cancel function to the manager and returns an id
// that can can be used to cancel the context later on when the goroutine is
// done.
func (g *GoroutineManager) addCancelFn(cancel context.CancelFunc) uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()

	id := g.id.Add(1)
	g.cancelFns[id] = cancel

	return id
}

// cancel cancels the context associated with the passed id.
func (g *GoroutineManager) cancel(id uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.cancelUnsafe(id)
}

// cancelUnsafe cancels the context associated with the passed id without
// acquiring the mutex.
func (g *GoroutineManager) cancelUnsafe(id uint32) {
	fn, ok := g.cancelFns[id]
	if !ok {
		return
	}

	fn()

	delete(g.cancelFns, id)
}

// Go tries to start a new goroutine and returns a boolean indicating its
// success. It returns true if the goroutine was successfully created and false
// otherwise. A goroutine will fail to be created iff the goroutine manager is
// stopping or the passed context has already expired. The passed call-back
// function must exit if the passed context expires.
func (g *GoroutineManager) Go(ctx context.Context,
	f func(ctx context.Context)) bool {

	// Derive a cancellable context from the passed context and store its
	// cancel function in the manager. The context will be cancelled when
	// either the parent context is cancelled or the quit channel is closed
	// which will call the stored cancel function.
	ctx, cancel := context.WithCancel(ctx)
	id := g.addCancelFn(cancel)

	// Calling wg.Add(1) and wg.Wait() when the wg's counter is 0 is a race
	// condition, since it is not clear if Wait() should block or not. This
	// kind of race condition is detected by Go runtime and results in a
	// crash if running with `-race`. To prevent this, we protect the calls
	// to wg.Add(1) and wg.Wait() with a mutex. If we block here because
	// Stop is running first, then Stop will close the quit channel which
	// will cause the context to be cancelled, and we will exit before
	// calling wg.Add(1). If we grab the mutex here before Stop does, then
	// Stop will block until after we call wg.Add(1).
	g.mu.Lock()
	defer g.mu.Unlock()

	// Before continuing to start the goroutine, we need to check if the
	// context has already expired. This could be the case if the parent
	// context has already expired or if Stop has been called.
	if ctx.Err() != nil {
		g.cancelUnsafe(id)

		return false
	}

	// Ensure that the goroutine is not started if the manager has stopped.
	select {
	case <-g.quit:
		g.cancelUnsafe(id)

		return false
	default:
	}

	g.wg.Add(1)
	go func() {
		defer func() {
			g.cancel(id)
			g.wg.Done()
		}()

		f(ctx)
	}()

	return true
}

// Stop prevents new goroutines from being added and waits for all running
// goroutines to finish.
func (g *GoroutineManager) Stop() {
	g.stopped.Do(func() {
		// Closing the quit channel will prevent any new goroutines from
		// starting.
		g.mu.Lock()
		close(g.quit)
		for _, cancel := range g.cancelFns {
			cancel()
		}
		g.mu.Unlock()

		// Wait for all goroutines to finish. Note that this wg.Wait()
		// call is safe, since it can't run in parallel with wg.Add(1)
		// call in Go, since we just cancelled the context and even if
		// Go call starts running here after acquiring the mutex, it
		// would see that the context has expired and return false
		// instead of calling wg.Add(1).
		g.wg.Wait()
	})
}

// Done returns a channel which is closed once Stop has been called and the
// quit channel closed. Note that the channel closing indicates that shutdown
// of the GoroutineManager has started but not necessarily that the Stop method
// has finished.
func (g *GoroutineManager) Done() <-chan struct{} {
	return g.quit
}
