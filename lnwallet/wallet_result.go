package lnwallet

import (
	"context"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// errRequest is embedded within the wallet requests whose only result is an
// error. It holds the promise that error is delivered over.
type errRequest struct {
	// resp is the promise the outcome of the request is delivered over.
	resp actor.Promise[error]
}

// complete resolves the request's promise with the passed error. A nil error
// signals that the request was handled successfully.
func (e *errRequest) complete(err error) {
	completeWalletResult(e.resp, err)
}

// openChanRequest is embedded within the two requests that finalize a funding
// workflow. It holds the promise their result is delivered over.
type openChanRequest struct {
	// resp is the promise the result of the request is delivered over.
	resp actor.Promise[fn.Result[*chanstate.OpenChannel]]
}

// fail resolves the request's promise with the passed error, signaling that
// the funding workflow could not be completed.
func (o *openChanRequest) fail(err error) {
	completeWalletResult(o.resp, fn.Err[*chanstate.OpenChannel](err))
}

// succeed resolves the request's promise with the finalized channel state.
func (o *openChanRequest) succeed(channel *chanstate.OpenChannel) {
	completeWalletResult(o.resp, fn.Ok(channel))
}

// completeWalletResult resolves a wallet request promise with the provided
// value. This function is safe to call multiple times; only the first call
// takes effect.
func completeWalletResult[T any](p actor.Promise[T], val T) {
	if p == nil {
		return
	}

	actor.CompleteWith(p, val)
}

// awaitWalletResult blocks until the passed future is resolved by the wallet's
// request handler, returning the value it was resolved with. A background
// context is used so the wait is never cut short, preserving the semantics of
// the plain channel receive this replaced.
func awaitWalletResult[T any](f actor.Future[T]) T {
	// Every promise the wallet hands out is completed with a value rather
	// than being failed, so the only error AwaitFuture can report here is
	// a cancelled context. As the context above is never cancelled, that
	// error is always nil.
	val, _ := actor.AwaitFuture[T](context.Background(), f)

	return val
}
