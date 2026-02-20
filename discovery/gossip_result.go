package discovery

import (
	"context"

	"github.com/lightningnetwork/lnd/actor"
)

// completeGossipResult resolves a gossip processing promise with the provided
// error value. A nil error indicates successful processing. This function is
// safe to call multiple times; only the first call takes effect.
func completeGossipResult(p actor.Promise[error], err error) {
	actor.CompleteWith(p, err)
}

// AwaitGossipResult blocks until the gossip processing future resolves or the
// provided context is cancelled. It returns the gossip processing error on
// success, or a context cancellation error if the context expired first.
func AwaitGossipResult(ctx context.Context, f actor.Future[error]) error {
	gossipErr, ctxErr := actor.AwaitFuture[error](ctx, f)
	if ctxErr != nil {
		return ctxErr
	}

	return gossipErr
}

// awaitGossipResult is a package-internal alias for AwaitGossipResult.
func awaitGossipResult(ctx context.Context, f actor.Future[error]) error {
	return AwaitGossipResult(ctx, f)
}
