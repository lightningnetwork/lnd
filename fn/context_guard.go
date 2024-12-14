package fn

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// DefaultTimeout is the default timeout used for context operations.
	DefaultTimeout = 30 * time.Second
)

// ContextGuard is a struct that provides a wait group and main quit channel
// that can be used to create guarded contexts.
type ContextGuard struct {
	mu sync.Mutex
	wg sync.WaitGroup

	quit    chan struct{}
	stopped sync.Once

	// id is used to generate unique ids for each context that should be
	// cancelled when the main quit signal is triggered.
	id atomic.Uint32

	// cancelFns is a map of cancel functions that can be used to cancel
	// any context that should be cancelled when the main quit signal is
	// triggered. The key is the id of the context. The mutex must be held
	// when accessing this map.
	cancelFns map[uint32]context.CancelFunc
}

// NewContextGuard constructs and returns a new instance of ContextGuard.
func NewContextGuard() *ContextGuard {
	return &ContextGuard{
		quit:      make(chan struct{}),
		cancelFns: make(map[uint32]context.CancelFunc),
	}
}

// Quit is used to signal the main quit channel, which will cancel all
// non-blocking contexts derived from the ContextGuard.
func (g *ContextGuard) Quit() {
	g.stopped.Do(func() {
		g.mu.Lock()
		defer g.mu.Unlock()

		for _, cancel := range g.cancelFns {
			cancel()
		}

		// Clear cancelFns. It is safe to use nil, because no write
		// operations to it can happen after g.quit is closed.
		g.cancelFns = nil

		close(g.quit)
	})
}

// Done returns a channel that will be closed when the main quit signal is
// triggered.
func (g *ContextGuard) Done() <-chan struct{} {
	return g.quit
}

// WgAdd is used to add delta to the internal wait group of the ContextGuard.
func (g *ContextGuard) WgAdd(delta int) {
	g.wg.Add(delta)
}

// WgDone is used to decrement the internal wait group of the ContextGuard.
func (g *ContextGuard) WgDone() {
	g.wg.Done()
}

// WgWait is used to block until the internal wait group of the ContextGuard is
// empty.
func (g *ContextGuard) WgWait() {
	g.wg.Wait()
}

// ctxGuardOptions is used to configure the behaviour of the context derived
// via the WithCtx method of the ContextGuard.
type ctxGuardOptions struct {
	blocking    bool
	withTimeout bool
	timeout     time.Duration
}

// ContextGuardOption defines the signature of a functional option that can be
// used to configure the behaviour of the context derived via the WithCtx method
// of the ContextGuard.
type ContextGuardOption func(*ctxGuardOptions)

// WithBlockingCG is used to create a cancellable context that will NOT be
// cancelled if the main quit signal is triggered, to block shutdown of
// important tasks.
func WithBlockingCG() ContextGuardOption {
	return func(o *ctxGuardOptions) {
		o.blocking = true
	}
}

// WithCustomTimeoutCG is used to create a cancellable context with a custom
// timeout. Such a context will be cancelled if either the parent context is
// cancelled, the timeout is reached or, if the Blocking option is not provided,
// the main quit signal is triggered.
func WithCustomTimeoutCG(timeout time.Duration) ContextGuardOption {
	return func(o *ctxGuardOptions) {
		o.withTimeout = true
		o.timeout = timeout
	}
}

// WithTimeoutCG is used to create a cancellable context with a default timeout.
// Such a context will be cancelled if either the parent context is cancelled,
// the timeout is reached or, if the Blocking option is not provided, the main
// quit signal is triggered.
func WithTimeoutCG() ContextGuardOption {
	return func(o *ctxGuardOptions) {
		o.withTimeout = true
		o.timeout = DefaultTimeout
	}
}

// Create is used to derive a cancellable context from the parent. Various
// options can be provided to configure the behaviour of the derived context.
func (g *ContextGuard) Create(ctx context.Context,
	options ...ContextGuardOption) (context.Context, context.CancelFunc) {

	// Exit early if the parent context has already been cancelled.
	select {
	case <-ctx.Done():
		return ctx, func() {}
	default:
	}

	var opts ctxGuardOptions
	for _, o := range options {
		o(&opts)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	var cancel context.CancelFunc
	if opts.withTimeout {
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	if opts.blocking {
		g.ctxBlocking(ctx)

		return ctx, cancel
	}

	// If the call is non-blocking, then we can exit early if the main quit
	// signal has been triggered.
	select {
	case <-g.quit:
		cancel()

		return ctx, cancel
	default:
	}

	cancel = g.ctxQuitUnsafe(ctx, cancel)

	return ctx, cancel
}

// ctxQuitUnsafe increases the wait group counter, waits until the context is
// cancelled and decreases the wait group counter. It stores the passed cancel
// function and returns a wrapped version, which removed the stored one and
// calls it. The Quit method calls all the stored cancel functions.
//
// NOTE: the caller must hold the ContextGuard's mutex before calling this
// function.
func (g *ContextGuard) ctxQuitUnsafe(ctx context.Context,
	cancel context.CancelFunc) context.CancelFunc {

	cancel = g.addCancelFnUnsafe(cancel)

	g.wg.Add(1)

	// We don't have to wait on g.quit here: g.quit can be closed only in
	// the Quit method, which also closes the context we are waiting for.
	context.AfterFunc(ctx, func() {
		g.wg.Done()
	})

	return cancel
}

// ctxBlocking increases the wait group counter, waits until the context is
// cancelled and decreases the wait group counter.
//
// NOTE: the caller must hold the ContextGuard's mutex before calling this
// function.
func (g *ContextGuard) ctxBlocking(ctx context.Context) {
	g.wg.Add(1)

	context.AfterFunc(ctx, func() {
		g.wg.Done()
	})
}

// addCancelFnUnsafe adds a context cancel function to the manager and returns a
// call-back which can safely be used to cancel the context.
//
// NOTE: the caller must hold the ContextGuard's mutex before calling this
// function.
func (g *ContextGuard) addCancelFnUnsafe(
	cancel context.CancelFunc) context.CancelFunc {

	id := g.id.Add(1)
	g.cancelFns[id] = cancel

	return g.cancelCtxFn(id)
}

// cancelCtxFn returns a call-back that can be used to cancel the context
// associated with the passed id.
func (g *ContextGuard) cancelCtxFn(id uint32) context.CancelFunc {
	return func() {
		g.mu.Lock()

		fn, ok := g.cancelFns[id]
		if !ok {
			g.mu.Unlock()
			return
		}
		delete(g.cancelFns, id)
		g.mu.Unlock()

		fn()
	}
}
