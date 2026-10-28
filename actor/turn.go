package actor

import (
	"context"
	"sync/atomic"
)

// turnKey is the context key under which the runtime records the receive turn
// a context belongs to.
type turnKey struct{}

// turn identifies one in-flight receive turn of an actor. The runtime stamps
// it onto the context it hands to the behavior, so a send made with that
// context can tell that it is running on an actor's own goroutine, where
// parking would stall the actor's whole mailbox.
type turn struct {
	// actorID is the ID of the actor processing the turn.
	actorID string

	// active is cleared when the turn returns. A goroutine the behavior
	// spawned keeps the context, and with it this marker, after the turn
	// is over; such a goroutine is not the actor's own and may park like
	// any other producer.
	active atomic.Bool
}

// beginTurn returns a context marked as the receive turn of the given actor,
// along with the function that ends the turn. The caller must invoke the end
// function once the behavior returns. A context that already carries a turn
// is shadowed, since only the innermost turn is the one running on this
// goroutine.
func beginTurn(ctx context.Context, actorID string) (context.Context, func()) {
	t := &turn{actorID: actorID}
	t.active.Store(true)

	return context.WithValue(ctx, turnKey{}, t), func() {
		t.active.Store(false)
	}
}

// activeTurn returns the receive turn ctx belongs to, if that turn is still
// running.
func activeTurn(ctx context.Context) (*turn, bool) {
	t, ok := ctx.Value(turnKey{}).(*turn)
	if !ok || t == nil || !t.active.Load() {
		return nil, false
	}

	return t, true
}

// TurnActor reports the ID of the actor whose receive turn ctx belongs to. The
// bool is false outside a turn, and for a context that outlived the turn it
// was created in.
func TurnActor(ctx context.Context) (string, bool) {
	t, ok := activeTurn(ctx)
	if !ok {
		return "", false
	}

	return t.actorID, true
}

// WithoutTurn returns ctx with its receive turn marker cleared, so a send made
// with the result is no longer treated as running on the actor's own
// goroutine. It is for handing a context to a goroutine that outlives the
// turn and must not be treated as the actor. All other values and the
// cancellation of ctx are kept. A context with no turn is returned unchanged.
func WithoutTurn(ctx context.Context) context.Context {
	if _, ok := ctx.Value(turnKey{}).(*turn); !ok {
		return ctx
	}

	return context.WithValue(ctx, turnKey{}, (*turn)(nil))
}

// WithTurnForTest marks ctx as the receive turn of the given actor, as the
// runtime does before invoking a behavior. It exists so tests outside this
// package can exercise turn-sensitive paths without standing up an actor. The
// returned function ends the turn.
func WithTurnForTest(ctx context.Context,
	actorID string) (context.Context, func()) {

	return beginTurn(ctx, actorID)
}
