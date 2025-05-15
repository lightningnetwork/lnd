package actor

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrNoActorsAvailable is returned when a router cannot find any actors
// registered for its service key to forward a message to.
var ErrNoActorsAvailable = errors.New("no actors available for service key")

// RoutingStrategy defines the interface for selecting an actor from a list of
// available actors.
// The M (Message) and R (Response) type parameters ensure that the strategy
// is compatible with the types of actors it will be selecting.
type RoutingStrategy[M Message, R any] interface {
	// Select chooses an ActorRef from the provided slice. It returns the
	// selected actor or an error if no actor can be selected (e.g., if the
	// list is empty or another strategy-specific issue occurs).
	Select(refs []ActorRef[M, R]) (ActorRef[M, R], error)
}

// RoundRobinStrategy implements a round-robin selection strategy. It is generic
// over M and R to match the RoutingStrategy interface, though its logic doesn't
// depend on these types directly for the selection mechanism itself.
type RoundRobinStrategy[M Message, R any] struct {
	// index is used to pick the next actor in a round-robin fashion. It
	// must be accessed atomically to ensure thread-safety if multiple
	// goroutines use the same strategy instance (which they will via the
	// router).
	index uint64
}

// NewRoundRobinStrategy creates a new RoundRobinStrategy, initialized for
// round-robin selection.
func NewRoundRobinStrategy[M Message, R any]() *RoundRobinStrategy[M, R] {
	return &RoundRobinStrategy[M, R]{}
}

// Select picks an actor from the list using a round-robin algorithm.
func (s *RoundRobinStrategy[M, R]) Select(refs []ActorRef[M, R]) (ActorRef[M, R], error) {
	if len(refs) == 0 {
		return nil, ErrNoActorsAvailable
	}

	// Atomically increment and get the current index for selection.
	// We subtract 1 because AddUint64 returns the new value (which is
	// 1-based for the first call after initialization to 0), and slice
	// indexing is 0-based.
	idx := atomic.AddUint64(&s.index, 1) - 1
	selectedRef := refs[idx%uint64(len(refs))]

	return selectedRef, nil
}

// Router is a message-dispatching component that fronts multiple actors
// registered under a specific ServiceKey. It uses a RoutingStrategy to
// distribute messages to one of the available actors. It is generic over M
// (Message type) and R (Response type) to match the actors it routes to.
type Router[M Message, R any] struct {
	receptionist *Receptionist
	serviceKey   ServiceKey[M, R]
	strategy     RoutingStrategy[M, R]
	dlo          ActorRef[Message, any] // Dead Letter Office reference.
}

// NewRouter creates a new Router for a given service key and strategy. The
// receptionist is used to discover actors registered with the service key.
// The router itself is not an actor but a message dispatcher that behaves like
// an ActorRef from the sender's perspective.
func NewRouter[M Message, R any](receptionist *Receptionist,
	key ServiceKey[M, R], strategy RoutingStrategy[M, R],
	dlo ActorRef[Message, any]) *Router[M, R] {

	return &Router[M, R]{
		receptionist: receptionist,
		serviceKey:   key,
		strategy:     strategy,
		dlo:          dlo,
	}
}

// getActor dynamically finds available actors for the service key and selects
// one using the configured strategy. This method is called internally by Tell
// and Ask on each invocation to ensure up-to-date actor discovery.
func (r *Router[M, R]) getActor() (ActorRef[M, R], error) {
	// Discover available actors from the receptionist.
	availableActors := FindInReceptionist(r.receptionist, r.serviceKey)
	if len(availableActors) == 0 {
		return nil, ErrNoActorsAvailable
	}

	// Select one actor using the strategy.
	return r.strategy.Select(availableActors)
}

// Tell sends a message to one of the actors managed by the router, selected by
// the routing strategy. If no actors are available or the send context is
// cancelled before the message can be enqueued in the target actor's mailbox,
// the message may be dropped. Errors during actor selection (e.g.,
// ErrNoActorsAvailable) are currently not propagated from Tell, aligning with
// its fire-and-forget nature. Such errors could be logged internally if needed.
func (r *Router[M, R]) Tell(ctx context.Context, msg M) {
	selectedActor, err := r.getActor()
	if err != nil {
		// If no actors are available for the service, and a DLO is
		// configured, forward the message there.
		if errors.Is(err, ErrNoActorsAvailable) && r.dlo != nil {
			r.dlo.Tell(context.Background(), msg)
		}
		return
	}

	selectedActor.Tell(ctx, msg)
}

// Ask sends a message to one of the actors managed by the router, selected by
// the routing strategy, and returns a Future for the response. If no actors are
// available (ErrNoActorsAvailable), the Future will be completed with this
// error. If the send context is cancelled before the message can be enqueued in
// the chosen actor's mailbox, the Future will be completed with the context's error.
func (r *Router[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	selectedActor, err := r.getActor()
	if err != nil {
		// If no actor could be selected (e.g., none available),
		// complete the promise immediately with the selection error.
		promise := NewPromise[R]()
		promise.Complete(fn.Err[R](err))
		return promise.Future()
	}

	return selectedActor.Ask(ctx, msg)
}

// ID provides an identifier for the router. Since a router isn't an actor
// itself but a dispatcher for a service, its ID can be based on the service
// key.
func (r *Router[M, R]) ID() string {
	return "router(" + r.serviceKey.name + ")"
}
