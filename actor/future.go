package actor

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// promiseImpl is a structure that can be used to complete a Future. It provides
// methods to set the result of an asynchronous operation and to obtain the
// Future interface for consumers.
// The promiseImpl itself is not typically exposed directly to consumers of the
// future's result; they interact with the Future interface.
type promiseImpl[T any] struct {
	fut *futureImpl[T]
}

// NewPromise creates a new Promise. The associated Future, which consumers can
// use to await the result, can be obtained via the Future() method. The Future
// is completed by calling the Complete() method on this Promise.
func NewPromise[T any]() Promise[T] {
	return &promiseImpl[T]{
		fut: &futureImpl[T]{
			// done is a channel that will be closed when the future
			// is completed.
			done: make(chan struct{}),
		},
	}
}

// Future returns the Future interface associated with this Promise. Consumers
// can use this to Await the result or register callbacks.
func (p *promiseImpl[T]) Future() Future[T] {
	return p.fut
}

// Complete attempts to set the result of the future. It returns true if this
// call successfully set the result (i.e., it was the first to complete it),
// and false if the future had already been completed. This ensures that a
// future can only be completed once. The completion involves storing the result
// and signaling any goroutines waiting on the future's done channel.
func (p *promiseImpl[T]) Complete(result fn.Result[T]) bool {
	var success bool
	p.fut.completeOnce.Do(func() {
		p.fut.resultCache.Store(&result)
		close(p.fut.done)
		success = true
	})
	return success
}

// futureImpl is the concrete implementation of the Future interface. It manages
// the state of an asynchronous computation's result.
type futureImpl[T any] struct {
	// resultCache stores the fn.Result[T] after the future is completed.
	// It's of type atomic.Pointer to allow lock-free reads after completion
	// with improved type safety over atomic.Value.
	resultCache atomic.Pointer[fn.Result[T]]

	// done is closed once the future is completed, signaling any waiting
	// Await calls.
	done chan struct{}

	// completeOnce ensures that the logic to set the result and close the
	// done channel is executed only once.
	completeOnce sync.Once
}

// Await blocks until the result is available or the passed context is
// cancelled. If the future is already completed, it returns the result
// immediately. Otherwise, it waits for either the future's completion or the
// context's cancellation.
func (f *futureImpl[T]) Await(ctx context.Context) fn.Result[T] {
	// First, try a non-blocking load from the cache. If the future is
	// already completed, this will return the result directly.
	if resPtr := f.resultCache.Load(); resPtr != nil {
		return *resPtr
	}

	// Wait for either the future to be done or the context to be cancelled.
	select {
	case <-f.done:
		// The future has been completed. Load the result from the
		// cache. It must be present now. Load and dereference.
		// This load is safe because the 'done' channel is closed only
		// after the resultCache is written (ensured by completeOnce).
		resPtr := f.resultCache.Load()

		// resPtr should not be nil here as <-f.done was signaled.
		return *resPtr

	case <-ctx.Done():
		// The waiting context was cancelled before the future completed.
		return fn.Err[T](ctx.Err())
	}
}

// ThenApply registers a function to transform the result of a future. The
// original future is not modified; a new Future instance representing the
// transformed result is returned. Once the original future completes
// successfully, the provided transformation function (fApply) is called with
// the result. The transformation is applied asynchronously in a new goroutine.
// If the passed context is cancelled while waiting for the
// original future to complete, the returned future will yield the context's
// error.
func (f *futureImpl[T]) ThenApply(ctx context.Context, fApply func(T) T) Future[T] {
	// Create a new promise for the transformed result.
	transformedPromise := NewPromise[T]()

	go func() {
		// Await the original future's result, respecting the passed
		// context for cancellation.
		originalResult := f.Await(ctx)

		// If the original future completed with an error (or Await was
		// cancelled by its context), complete the transformed future
		// with the same error.
		// This also handles the case where originalResult.Await(ctx)
		// itself returned ctx.Err().
		if originalResult.IsErr() {
			transformedPromise.Complete(originalResult)
			return
		}

		// Otherwise, the original future completed successfully. Apply the
		// transformation function to its result.
		originalResult.WhenOk(func(res T) {
			newValue := fApply(res)
			transformedPromise.Complete(fn.Ok(newValue))
		})
	}()

	return transformedPromise.Future()
}

// OnComplete registers a function to be called when the result is ready. If the
// passed context is cancelled before the future completes, the callback
// function (cFunc) will be invoked with the context's error. The callback is
// executed in a new goroutine, so it does not block the completion path of the
// original future.
func (f *futureImpl[T]) OnComplete(ctx context.Context, cFunc func(fn.Result[T])) {
	go func() {
		// Await the original future's result, respecting the passed
		// context for cancellation.
		result := f.Await(ctx)

		// Call the callback function with the result.
		cFunc(result)
	}()
}
