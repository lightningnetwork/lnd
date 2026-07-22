package discovery

import (
	"context"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// completeGossipResult resolves a gossip processing promise with the provided
// error value. A nil error indicates successful processing. This function is
// safe to call multiple times; only the first call takes effect.
//
// NOTE: The error is wrapped via fn.Ok (the "success" side of Result), so
// AwaitGossipResult can distinguish gossip errors from context cancellation.
func completeGossipResult(p fn.Promise[error], err error) {
	if p == nil {
		return
	}

	fn.CompleteWith(p, err)
}

// AwaitGossipResult blocks until the gossip processing future resolves or the
// provided context is cancelled. It returns the gossip processing error on
// success, or a context cancellation error if the context expired first.
func AwaitGossipResult(ctx context.Context, f fn.Future[error]) error {
	gossipErr, ctxErr := fn.AwaitFuture[error](ctx, f)
	if ctxErr != nil {
		return ctxErr
	}

	return gossipErr
}
