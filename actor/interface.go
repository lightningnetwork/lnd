package actor

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// Future represents the result of an asynchronous computation. It allows
// consumers to wait for the result (Await), apply transformations upon
// completion (ThenApply), or register a callback to be executed when the
// result is available (OnComplete).
type Future[T any] interface {
	// Await blocks until the result is available or the context is
	// cancelled, then returns it.
	Await(ctx context.Context) fn.Result[T]

	// ThenApply registers a function to transform the result of a future.
	// The original future is not modified, a new instance of the future is
	// returned. If the passed context is cancelled while waiting for the
	// original future to complete, the new future will complete with the
	// context's error.
	ThenApply(ctx context.Context, fn func(T) T) Future[T]

	// OnComplete registers a function to be called when the result of the
	// future is ready. If the passed context is cancelled before the future
	// completes, the callback function will be invoked with the context's
	// error.
	OnComplete(ctx context.Context, fn func(fn.Result[T]))
}

// Promise is an interface that allows for the completion of an associated
// Future. It provides a way to set the result of an asynchronous operation.
// The producer of an asynchronous result uses a Promise to set the outcome,
// while consumers use the associated Future to retrieve it.
type Promise[T any] interface {
	// Future returns the Future interface associated with this Promise.
	// Consumers can use this to Await the result or register callbacks.
	Future() Future[T]

	// Complete attempts to set the result of the future. It returns true if
	// this call successfully set the result (i.e., it was the first to
	// complete it), and false if the future had already been completed.
	Complete(result fn.Result[T]) bool
}
