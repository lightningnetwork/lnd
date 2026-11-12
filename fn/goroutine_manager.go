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

	mu   sync.Mutex
	cond *sync.Cond

	stopped sync.Once
	quit    chan struct{}

	// count is the number of goroutines currently running. Must be accessed
	// holding mu.
	count int
}

// NewGoroutineManager constructs and returns a new instance of
// GoroutineManager.
func NewGoroutineManager() *GoroutineManager {
	gm := &GoroutineManager{
		cancelFns: make(map[uint32]context.CancelFunc),
		quit:      make(chan struct{}),
	}
	gm.cond = sync.NewCond(&gm.mu)

	return gm
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

	// Protect the remaining part of the method with the mutex, because we
	// access quit and count.
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

	g.count++
	go func() {
		defer func() {
			g.cancel(id)

			g.mu.Lock()
			g.count--
			g.mu.Unlock()

			g.cond.Signal()
		}()

		f(ctx)
	}()

	return true
}

// GoBlocking tries to start a new blocking goroutine and returns a boolean
// indicating its success. It returns true if the goroutine was successfully
// created and false otherwise. A goroutine will fail to be created iff the
// goroutine manager has stopped (Stop() was called and all the goroutines
// have finished). To make sure GoBlocking succeeds, call it right after
// creating a GoroutineManager (in Start() method of your object) or from
// another goroutine created by the same GoroutineManager.
//
// The difference from Go() is that GoroutineManager doesn't manage contexts so
// the goroutine can run as long as needed. GoroutineManager will still wait for
// its completion in the Stop() method. But it is the caller's responsibility to
// stop the launched goroutine and to pass a context to it if needed.
//
// This method is intended to perform shutdown of important tasks, where
// interruption is not desirable.
func (g *GoroutineManager) GoBlocking(f func()) bool {
	// Protect the whole code of the method with the mutex, because we
	// access quit and count.
	g.mu.Lock()
	defer g.mu.Unlock()

	// If the goroutine manager has completelly stopped, stop. This happens
	// only if Stop() was called and all goroutines have finished.
	select {
	case <-g.quit:
		if g.count == 0 {
			return false
		}
	default:
	}

	g.count++
	go func() {
		defer func() {
			g.mu.Lock()
			g.count--
			g.mu.Unlock()

			// We use Signal() and not Broadcast(), because there
			// could be only one user of g.cond.Wait(), because of
			// g.stopped.
			g.cond.Signal()
		}()

		f()
	}()

	return true
}

// Stop prevents new goroutines from being added and waits for all running
// goroutines to finish.
func (g *GoroutineManager) Stop() {
	g.stopped.Do(func() {
		g.mu.Lock()
		defer g.mu.Unlock()

		// Closing the quit channel will prevent any new goroutines from
		// starting.
		close(g.quit)

		// Cancelling contexts of all launched goroutines.
		for _, cancel := range g.cancelFns {
			cancel()
		}

		// Wait for all goroutines to finish.
		for g.count != 0 {
			g.cond.Wait()
		}
	})
}

// Done returns a channel which is closed once Stop has been called and the
// quit channel closed. Note that the channel closing indicates that shutdown
// of the GoroutineManager has started but not necessarily that the Stop method
// has finished.
func (g *GoroutineManager) Done() <-chan struct{} {
	return g.quit
}
