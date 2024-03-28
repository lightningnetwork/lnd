package fn

import (
	"context"
	"errors"
)

// Async represents an action whose results are being computed outside this
// control thread.
type Async[A any] interface {
	// Await may be called to block on the completion of the remote
	// computation.
	Await() (*A, error)

	// Cancel may be called to abandon the remote computation.
	Cancel()
}

var (
	// ContextCancelled indicates that this Async was cancelled because the
	// surrounding context was being torn down.
	ContextCancelled = errors.New("context cancelled")
)

// RunAsyncWithCancel is a function that will transform a synchronous function
// and make it a cancellable asynchronous call. This can be useful if the
// function in the argument can block indefinitely and the calling context needs
// a way to abandon waiting for it to complete.
func RunAsyncWithCancel[A any](ctx context.Context,
	f func(ctx context.Context) (*A, error)) Async[A] {

	localCtx, cancel := context.WithCancel(ctx)

	a := async[A]{
		ctx:    localCtx,
		cancel: cancel,
		res:    make(chan *A, 1),
		err:    make(chan error, 1),
	}

	go func() {
		res, err := f(localCtx)

		// If the error we get back from f is non-nil we try to send an
		// err back on the err channel, bailing if we're being
		// cancelled.
		if err != nil {
			select {
			case a.err <- err:
			case <-a.ctx.Done():
			}

			return
		}

		// Otherwise we will send the result back on the result channel,
		// bailing if we were cancelled.
		select {
		case a.res <- res:
		case <-a.ctx.Done():
		}
	}()

	return &a
}

// RunAsync runs the argument closure asynchronously without pinning the action
// to a calling context.
func RunAsync[A any](f func() (*A, error)) Async[A] {
	return RunAsyncWithCancel(
		context.Background(),
		func(_ context.Context) (*A, error) {
			return f()
		},
	)
}

// Await may be called to block on the completion of the remote computation.
func (a *async[A]) Await() (*A, error) {
	select {
	case r := <-a.res:
		return r, nil
	case e := <-a.err:
		return nil, e
	case <-a.ctx.Done():
		return nil, ContextCancelled
	}
}

// Cancel may be called to abandon the remote computation.
func (a *async[A]) Cancel() {
	a.cancel()
}

type async[A any] struct {
	// contextCancel is a receive only signal that we use to pin the async
	// action to the context, making it such that we don't have dangling
	// actions.
	ctx context.Context

	// cancel is a function that can be used to abort the asynchronous
	// action.
	cancel context.CancelFunc

	// res is the channel we will send the non-error result back over.
	res chan *A

	// err is the channel we will send an error result back over.
	err chan error
}
