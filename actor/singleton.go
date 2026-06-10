package actor

import (
	"context"
	"errors"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// Compile-time assertion that SingletonRef satisfies the ActorRef interface.
var _ ActorRef[Message, any] = (*SingletonRef[Message, any])(nil)

// SingletonRef is an ActorRef implementation that acts as a lookup proxy for
// service keys expected to have exactly one registered actor. It holds no
// direct reference to the target actor; instead, it performs a receptionist
// lookup on each Tell/Ask and forwards to whichever actor is currently
// registered. Compared to Router, this skips the routing strategy entirely,
// which is both semantically correct for "one-actor-per-key" patterns (e.g.
// "the RBF closer for channel X") and avoids unnecessary allocations.
//
// Because SingletonRef does not own the target actor's lifecycle, spawning
// and unregistering are done through the ServiceKey, typically via
// ServiceKey.SpawnSingleton. The only ActorRef this type holds is the
// optional DLO used when no actor is registered.
type SingletonRef[M Message, R any] struct {
	receptionist *Receptionist
	serviceKey   ServiceKey[M, R]
	dlo          ActorRef[Message, any]
}

// NewSingletonRef creates a new SingletonRef for the given service key. The
// receptionist is used to discover the actor registered under the key. The
// optional dlo is used as the destination for Tell messages sent when no
// actor is registered; if dlo is nil, such messages are dropped with a log
// warning.
func NewSingletonRef[M Message, R any](receptionist *Receptionist,
	key ServiceKey[M, R],
	dlo ActorRef[Message, any]) *SingletonRef[M, R] {

	return &SingletonRef[M, R]{
		receptionist: receptionist,
		serviceKey:   key,
		dlo:          dlo,
	}
}

// getActor performs a receptionist lookup for the singleton's service key. It
// returns ErrNoActorsAvailable if no actor is registered. If more than one
// actor is registered, it logs a warning about the invariant violation and
// returns the first registered actor, so callers remain functional during
// transient registration races.
func (s *SingletonRef[M, R]) getActor() (ActorRef[M, R], error) {
	refs := FindInReceptionist(s.receptionist, s.serviceKey)
	switch len(refs) {
	case 0:
		return nil, ErrNoActorsAvailable

	case 1:
		return refs[0], nil

	default:
		// Invariant violation: a singleton service key must have at
		// most one registered actor. This can happen transiently
		// during a re-registration race; log loudly so the bug is
		// visible, then fall through and use the first registered
		// actor to keep the system functional.
		log.Warnf("SingletonRef(%s): %d actors registered for "+
			"singleton service key, expected 1; using first",
			s.serviceKey.name, len(refs))

		return refs[0], nil
	}
}

// Tell sends a message to the singleton actor. If no actor is currently
// registered and a DLO is configured, the message is forwarded to the DLO;
// otherwise it is dropped with a log warning, matching Router's behavior.
func (s *SingletonRef[M, R]) Tell(ctx context.Context, msg M) {
	ref, err := s.getActor()
	if err != nil {
		if errors.Is(err, ErrNoActorsAvailable) && s.dlo != nil {
			s.dlo.Tell(context.Background(), msg)
		} else {
			log.Warnf("SingletonRef(%s): message %s dropped "+
				"(no actor registered, no DLO configured)",
				s.serviceKey.name, msg.MessageType())
		}

		return
	}

	ref.Tell(ctx, msg)
}

// Ask sends a message to the singleton actor and returns a Future for the
// response. If no actor is registered, the Future is completed immediately
// with ErrNoActorsAvailable.
func (s *SingletonRef[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	ref, err := s.getActor()
	if err != nil {
		promise := NewPromise[R]()
		promise.Complete(fn.Err[R](err))

		return promise.Future()
	}

	return ref.Ask(ctx, msg)
}

// ID returns an identifier for this singleton reference. Since SingletonRef
// is a lookup proxy rather than a direct reference to a concrete actor, its
// ID is derived from the service key.
func (s *SingletonRef[M, R]) ID() string {
	return "singleton(" + s.serviceKey.name + ")"
}

// SpawnSingleton registers a singleton actor under this service key. Any
// existing actors registered under the same key are unregistered and stopped
// first, so this method is safe to call repeatedly (e.g. when a channel
// closer is re-initialized for the same channel point). It returns the
// ActorRef of the newly spawned actor.
//
// NOTE: The unregister-then-register sequence is not atomic under the
// receptionist lock. Concurrent callers racing to spawn a singleton for the
// same key may temporarily leave two actors registered. SingletonRef
// tolerates this by logging and using the first registered actor. If strict
// at-most-one registration is required, callers must coordinate externally.
//
// NOTE: SpawnSingleton is also not transactional with respect to failure.
// UnregisterAll runs before RegisterWithSystem, so if the registration step
// returns an error (e.g. ErrEmptyActorID, ErrNilBehavior,
// ErrDuplicateActorID) any previously registered actor has already been
// stopped and there is no rollback. Callers must pass a valid config.
//
// TODO: make SpawnSingleton transactional by validating the config and
// reserving the actor ID before tearing down the previous registration, so a
// failed replacement leaves the existing singleton intact.
func (sk ServiceKey[M, R]) SpawnSingleton(as *ActorSystem, id string,
	behavior ActorBehavior[M, R],
	opts ...ActorOption[M, R]) (ActorRef[M, R], error) {

	// Stop and unregister any previous actor for this key so that only one
	// instance exists after we return.
	sk.UnregisterAll(as)

	return RegisterWithSystem(as, id, sk, behavior, opts...)
}

// Singleton returns a SingletonRef that can be used to reach the actor
// registered under this service key. The returned ref does not spawn an
// actor — it performs receptionist lookups on each Tell/Ask. Combined with
// SpawnSingleton (called elsewhere to register the actor), this is the
// preferred pattern for "one-actor-per-key" services: the spawner manages
// the actor lifecycle while consumers simply hold a Singleton ref.
func (sk ServiceKey[M, R]) Singleton(
	as *ActorSystem) *SingletonRef[M, R] {

	return NewSingletonRef(as.Receptionist(), sk, as.DeadLetters())
}
