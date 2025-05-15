package actor

import (
	"context"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorConfig holds the configuration parameters for creating a new Actor.
// It is generic over M (Message type) and R (Response type) to accommodate
// the actor's specific behavior.
type ActorConfig[M Message, R any] struct {
	// ID is the unique identifier for the actor.
	ID string

	// Behavior defines how the actor responds to messages.
	Behavior ActorBehavior[M, R]

	// DLO is a reference to the dead letter office for this actor system.
	// If nil, undeliverable messages during shutdown or due to a full
	// mailbox (if such logic were added) might be dropped.
	DLO ActorRef[Message, any]

	// MailboxSize defines the buffer capacity of the actor's mailbox.
	MailboxSize int
}

// envelope wraps a message with its associated promise. This allows the sender
// of an "ask" message to await a response. If the promise is nil, it
// signifies a "tell" operation (fire-and-forget).
type envelope[M Message, R any] struct {
	message M
	promise Promise[R]
}

// Actor represents a concrete actor implementation. It encapsulates a behavior,
// manages its internal state implicitly through that behavior, and processes
// messages from its mailbox sequentially in its own goroutine.
type Actor[M Message, R any] struct {
	// id is the unique identifier for the actor.
	id string

	// behavior defines how the actor responds to messages.
	behavior ActorBehavior[M, R]

	// mailbox is the incoming message queue for the actor.
	mailbox chan envelope[M, R]

	// ctx is the context governing the actor's lifecycle.
	ctx context.Context

	// cancel is the function to cancel the actor's context.
	cancel context.CancelFunc

	// dlo is a reference to the dead letter office for this actor system.
	dlo ActorRef[Message, any]

	// startOnce ensures the actor's processing loop is started only once.
	startOnce sync.Once

	// stopOnce ensures the actor's processing loop is stopped only once.
	stopOnce sync.Once

	// ref is the cached ActorRef for this actor.
	ref ActorRef[M, R]
}

// NewActor creates a new actor instance with the given ID and behavior.
// It initializes the actor's internal structures but does not start its
// message processing goroutine. The Start() method must be called to begin
// processing messages.
func NewActor[M Message, R any](cfg ActorConfig[M, R]) *Actor[M, R] {
	ctx, cancel := context.WithCancel(context.Background())

	// Ensure MailboxSize has a sane default if not specified or zero. A
	// capacity of 0 would make the channel unbuffered, which is generally
	// not desired for actor mailboxes.
	mailboxCapacity := cfg.MailboxSize
	if mailboxCapacity <= 0 {
		// Default to a small capacity if an invalid one is given. This
		// could also come from a global constant.
		mailboxCapacity = 1
	}

	actor := &Actor[M, R]{
		id:       cfg.ID,
		behavior: cfg.Behavior,
		mailbox:  make(chan envelope[M, R], mailboxCapacity),
		ctx:      ctx,
		cancel:   cancel,
		dlo:      cfg.DLO,
	}

	// Create and cache the actor's own reference.
	actor.ref = &actorRefImpl[M, R]{
		actor: actor,
	}

	return actor
}

// Start initiates the actor's message processing loop in a new goroutine. This
// method should be called once after the actor is created.
func (a *Actor[M, R]) Start() {
	a.startOnce.Do(func() {
		go a.process()
	})
}

// process is the main event loop for the actor. It continuously monitors its
// mailbox for incoming messages and its context for cancellation signals.
func (a *Actor[M, R]) process() {
	for {
		select {
		case env := <-a.mailbox:
			result := a.behavior.Receive(a.ctx, env.message)

			// If a promise was provided (i.e., it was an "ask"
			// operation), complete the promise with the result from
			// the behavior.
			if env.promise != nil {
				env.promise.Complete(result)
			}

		// The actor's context has been cancelled, signaling a stop
		// request. Exit the processing loop to terminate the actor's
		// goroutine. Before exiting, drain any remaining messages from
		// the mailbox.
		case <-a.ctx.Done():
			// Close the mailbox to prevent new incoming messages
			// and to allow the range operator below to terminate.
			close(a.mailbox)

			// Drain any remaining messages.
			for env := range a.mailbox {
				// If a DLO is configured, send the original
				// message there for auditing or potential
				// manual reprocessing.
				if a.dlo != nil {
					a.dlo.Tell(
						context.Background(),
						env.message,
					)
				}

				// If it was an Ask, complete the promise with
				// an error indicating the actor terminated.
				if env.promise != nil {
					env.promise.Complete(fn.Err[R](
						ErrActorTerminated),
					)
				}
			}

			return
		}
	}
}

// Stop signals the actor to terminate its processing loop and shut down.
// This is achieved by cancelling the actor's internal context. The actor's
// goroutine will exit once it detects the context cancellation.
func (a *Actor[M, R]) Stop() {
	a.stopOnce.Do(func() {
		a.cancel()
	})
}

// actorRefImpl provides a concrete implementation of the ActorRef interface. It
// holds a reference to the target Actor instance, enabling message sending.
type actorRefImpl[M Message, R any] struct {
	actor *Actor[M, R]
}

// Tell sends a message without waiting for a response. If the context is
// cancelled before the message can be sent to the actor's mailbox, the message
// may be dropped.
//
//nolint:lll
func (ref *actorRefImpl[M, R]) Tell(ctx context.Context, msg M) {
	// If the actor's own context is already done, don't try to send.
	// Route to DLO if available.
	if ref.actor.ctx.Err() != nil {
		ref.trySendToDLO(msg)
		return
	}

	select {
	// Message successfully enqueued in the actor's mailbox.
	case ref.actor.mailbox <- envelope[M, R]{message: msg, promise: nil}:

	// The context for the Tell operation was cancelled before the message
	// could be enqueued. The message is dropped.
	case <-ctx.Done():

	// The actor itself has been stopped/terminated.
	case <-ref.actor.ctx.Done():
		// If the actor is terminated and has a DLO, send the message
		// there. Otherwise, it's dropped.
		ref.trySendToDLO(msg)
	}
}

// Ask sends a message and returns a Future for the response. The Future will be
// completed with the actor's reply or an error if the operation fails (e.g.,
// context cancellation before send).
//
//nolint:lll
func (ref *actorRefImpl[M, R]) Ask(ctx context.Context, msg M) Future[R] {
	// Create a new promise that will be fulfilled with the actor's response.
	promise := NewPromise[R]()

	// If the actor's own context is already done, complete the promise with
	// ErrActorTerminated and return immediately. This is the primary guard
	// against trying to send to a stopped actor.
	if ref.actor.ctx.Err() != nil {
		promise.Complete(fn.Err[R](ErrActorTerminated))
		return promise.Future()
	}

	select {
	// Attempt to send the message along with its promise to the actor's
	// mailbox.
	case ref.actor.mailbox <- envelope[M, R]{message: msg, promise: promise}:

	// The context for the Ask operation was cancelled before the message
	// could be enqueued. Complete the promise with the context's error to
	// unblock the caller.
	case <-ctx.Done():
		promise.Complete(fn.Err[R](ctx.Err()))

	// The actor's context was cancelled (e.g., actor stopped) while this
	// Ask operation was attempting to send (e.g., mailbox was full).
	case <-ref.actor.ctx.Done():
		promise.Complete(fn.Err[R](ErrActorTerminated))
	}

	// Return the future associated with the promise, allowing the caller to
	// await the response.
	return promise.Future()
}

// trySendToDLO attempts to send the message to the actor's DLO if configured.
func (ref *actorRefImpl[M, R]) trySendToDLO(msg M) {
	if ref.actor.dlo != nil {
		// Use context.Background() for sending to DLO as the
		// original context might be done or the operation
		// should not be bound by it.
		// This Tell to DLO is fire-and-forget.
		ref.actor.dlo.Tell(context.Background(), msg)
	}
}

// ID returns the unique identifier for this actor.
func (ref *actorRefImpl[M, R]) ID() string {
	return ref.actor.id
}

// Ref returns an ActorRef for this actor. This allows clients to interact with
// the actor (send messages) without having direct access to the Actor struct
// itself, promoting encapsulation and location transparency.
func (a *Actor[M, R]) Ref() ActorRef[M, R] {
	return a.ref
}

// TellRef returns a TellOnlyRef for this actor. This allows clients to send
// messages to the actor using only the "tell" pattern (fire-and-forget),
// without having access to "ask" capabilities.
func (a *Actor[M, R]) TellRef() TellOnlyRef[M] {
	return a.ref
}
