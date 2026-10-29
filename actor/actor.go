package actor

import (
	"context"
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// MailboxFactory is a function type that creates a Mailbox implementation.
// It receives the actor's context and the desired capacity, allowing custom
// mailbox implementations (e.g., BackpressureMailbox) to be injected.
type MailboxFactory[M Message, R any] func(ctx context.Context,
	capacity int) Mailbox[M, R]

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

	// MailboxFactory is an optional factory for creating the actor's
	// mailbox. If nil, a default ChannelMailbox will be used.
	MailboxFactory MailboxFactory[M, R]

	// MailboxOverflowLimit is the hard cap on the default mailbox's
	// overflow queue, which absorbs sends made from inside a receive turn
	// when the mailbox is full. A send that would exceed it fails with
	// ErrMailboxOverflow. Zero selects DefaultMailboxOverflowLimit. It has
	// no effect when MailboxFactory is set.
	MailboxOverflowLimit int
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
	mailbox Mailbox[M, R]

	// ctx is owned by the actor and shared with its mailbox and behavior.
	// It must remain available until Stop cancels the actor's lifecycle.
	//
	//nolint:containedctx
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

	// done is closed once the actor's processing goroutine has exited, or
	// when Stop is called on an actor that was never started. After it is
	// closed no Receive is running or will run.
	done chan struct{}
}

// NewActor creates a new actor instance with the given ID and behavior.
// It initializes the actor's internal structures but does not start its
// message processing goroutine. The Start() method must be called to begin
// processing messages.
func NewActor[M Message, R any](cfg ActorConfig[M, R]) (*Actor[M, R],
	error) {

	if cfg.ID == "" {
		return nil, ErrEmptyActorID
	}

	if cfg.Behavior == nil {
		return nil, ErrNilBehavior
	}

	// The cancel func is stored on the actor and called by Stop, which
	// gosec can't see through.
	//
	//nolint:gosec
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

	// Create the mailbox using the factory if provided, otherwise use
	// the default ChannelMailbox.
	var mailbox Mailbox[M, R]
	if cfg.MailboxFactory != nil {
		mailbox = cfg.MailboxFactory(ctx, mailboxCapacity)
	} else {
		mailbox = NewChannelMailbox[M, R](
			ctx, mailboxCapacity,
			WithMailboxID(cfg.ID),
			WithOverflowLimit(cfg.MailboxOverflowLimit),
		)
	}

	actor := &Actor[M, R]{
		id:       cfg.ID,
		behavior: cfg.Behavior,
		mailbox:  mailbox,
		ctx:      ctx,
		cancel:   cancel,
		dlo:      cfg.DLO,
		done:     make(chan struct{}),
	}

	// Create and cache the actor's own reference.
	actor.ref = &actorRefImpl[M, R]{
		actor: actor,
	}

	return actor, nil
}

// Start initiates the actor's message processing loop in a new goroutine. This
// method should be called once after the actor is created.
func (a *Actor[M, R]) Start() {
	a.startOnce.Do(func() {
		log.Infof("Actor %s: starting", a.id)

		go a.process()
	})
}

// process is the main event loop for the actor. It continuously monitors its
// mailbox for incoming messages and its context for cancellation signals.
func (a *Actor[M, R]) process() {
	// Signal the join only once the loop and the drain below are finished.
	defer close(a.done)

	// Use the new iterator pattern for receiving messages.
	for env := range a.mailbox.Receive(a.ctx) {
		// Mark the context as this actor's receive turn, so sends the
		// behavior makes with it never park on a full mailbox.
		turnCtx, endTurn := beginTurn(a.ctx, a.id)
		result := a.behavior.Receive(turnCtx, env.message)
		endTurn()

		// If a promise was provided (i.e., it was an "ask"
		// operation), complete the promise with the result from
		// the behavior.
		if env.promise != nil {
			env.promise.Complete(result)
		}
	}

	// Context was cancelled or mailbox closed, drain remaining messages.
	a.mailbox.Close()

	for env := range a.mailbox.Drain() {
		// If a DLO is configured, send the original message there
		// for auditing or potential manual reprocessing.
		if a.dlo != nil {
			a.dlo.Tell(context.Background(), env.message)
		}

		// If it was an Ask, complete the promise with an error
		// indicating the actor terminated.
		if env.promise != nil {
			env.promise.Complete(fn.Err[R](ErrActorTerminated))
		}
	}
}

// Stop signals the actor to terminate its processing loop and shut down.
// This is achieved by cancelling the actor's internal context. The actor's
// goroutine will exit once it detects the context cancellation.
func (a *Actor[M, R]) Stop() {
	a.stopOnce.Do(func() {
		log.Infof("Actor %s: stopping", a.id)

		a.cancel()

		// An actor that was never started has no goroutine to join,
		// and can no longer be started, so release its waiters here.
		a.startOnce.Do(func() {
			close(a.done)
		})
	})
}

// Done returns a channel that is closed once the actor has fully stopped: its
// processing goroutine has exited, so no Receive is running and none will
// run again, and every message left in its mailbox has been drained to the
// DLO or failed. It is closed only after Stop, or a closed mailbox, ends the
// loop.
func (a *Actor[M, R]) Done() <-chan struct{} {
	return a.done
}

// StopAndWait stops the actor and blocks until it has fully stopped or ctx is
// done. A nil error is a join: the behavior's last Receive has returned and
// no other will start, so the owner may close resources the behavior uses.
// If ctx expires first, the error is ctx's and nothing is implied about
// whether a Receive is still running. A Receive blocked on something other
// than the context it was passed keeps the actor from stopping, so behaviors
// must watch that context.
func (a *Actor[M, R]) StopAndWait(ctx context.Context) error {
	a.Stop()

	select {
	case <-a.done:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// actorRefImpl provides a concrete implementation of the ActorRef interface. It
// holds a reference to the target Actor instance, enabling message sending.
type actorRefImpl[M Message, R any] struct {
	actor *Actor[M, R]
}

// Tell sends a message without waiting for a response. If the context is
// cancelled before the message can be sent to the actor's mailbox, the message
// may be dropped.
func (ref *actorRefImpl[M, R]) Tell(ctx context.Context, msg M) {
	// If the actor's own context is already done, don't try to send.
	// Route to DLO if available.
	if ref.actor.ctx.Err() != nil {
		ref.trySendToDLO(msg)
		return
	}

	env := envelope[M, R]{message: msg, promise: nil}

	// A send made from inside a receive turn must not park the sending
	// actor, so it takes the mailbox's non-blocking in-turn path.
	if sender, ok := ref.inTurnSender(ctx); ok {
		if err := sender.SendInTurn(env); err != nil {
			if ref.actor.ctx.Err() != nil {
				ref.trySendToDLO(msg)
				return
			}

			// Tell has no way to report the failure, so make it
			// loud. A sender that needs the signal can use TryTell.
			log.Errorf("Actor %s: in-turn Tell of %s dropped: %v",
				ref.actor.id, msg.MessageType(), err)
		}

		return
	}

	// Use mailbox Send method which internally checks both contexts.
	if !ref.actor.mailbox.Send(ctx, env) {
		// Failed to send - check if actor terminated.
		if ref.actor.ctx.Err() != nil {
			ref.trySendToDLO(msg)
		}
		// Otherwise the message was either dropped by backpressure
		// (load shedding) or the caller's context was cancelled.
		// Both are intentionally silent — no DLO routing.
	}
}

// Ask sends a message and returns a Future for the response. The Future will be
// completed with the actor's reply or an error if the operation fails (e.g.,
// context cancellation before send).
//
//nolint:ll
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

	// Check if the context is already done before attempting to send. This
	// ensures deterministic behavior and prevents a race where the message
	// could be enqueued even though the context was already cancelled.
	if ctx.Err() != nil {
		promise.Complete(fn.Err[R](ctx.Err()))
		return promise.Future()
	}

	env := envelope[M, R]{message: msg, promise: promise}

	// As with Tell, a send from inside a receive turn never parks.
	if sender, ok := ref.inTurnSender(ctx); ok {
		if err := sender.SendInTurn(env); err != nil {
			if ref.actor.ctx.Err() != nil {
				err = ErrActorTerminated
			}

			promise.Complete(fn.Err[R](err))
		}

		return promise.Future()
	}

	// Use mailbox Send method which internally checks both contexts.
	if !ref.actor.mailbox.Send(ctx, env) {
		// Determine the error based on what failed.
		switch {
		case ref.actor.ctx.Err() != nil:
			promise.Complete(fn.Err[R](ErrActorTerminated))
		case ctx.Err() != nil:
			promise.Complete(fn.Err[R](ctx.Err()))
		default:
			// Neither context is done — the mailbox's
			// backpressure mechanism dropped the message.
			promise.Complete(fn.Err[R](ErrMessageDropped))
		}
	}

	// Return the future associated with the promise, allowing the caller to
	// await the response.
	return promise.Future()
}

// inTurnSender returns the mailbox's non-blocking send path if ctx belongs to
// an active receive turn and the mailbox supports one.
func (ref *actorRefImpl[M, R]) inTurnSender(
	ctx context.Context) (inTurnSender[M, R], bool) {

	if _, ok := activeTurn(ctx); !ok {
		return nil, false
	}

	sender, ok := ref.actor.mailbox.(inTurnSender[M, R])

	return sender, ok
}

// TryTell enqueues a message only if the mailbox can take it immediately,
// returning ErrMailboxFull rather than waiting for room. Unlike Tell it never
// blocks, whichever goroutine calls it.
func (ref *actorRefImpl[M, R]) TryTell(ctx context.Context, msg M) error {
	// A caller that has already given up shouldn't have its message
	// enqueued. TrySend never waits, so this is the only way the context
	// participates.
	if err := ctx.Err(); err != nil {
		return err
	}

	env := envelope[M, R]{message: msg, promise: nil}
	if err := ref.trySend(env); err != nil {
		// A terminated actor will never drain its mailbox, so the
		// message is dead and belongs in the DLO. A full mailbox is
		// transient and stays the caller's problem.
		if errors.Is(err, ErrActorTerminated) {
			ref.trySendToDLO(msg)
		}

		return err
	}

	return nil
}

// TryAsk is the non-blocking form of Ask. If the mailbox can't take the
// message immediately, the returned Future is already completed with the
// reason, so the caller never waits to learn that the send failed.
func (ref *actorRefImpl[M, R]) TryAsk(ctx context.Context,
	msg M) Future[R] {

	promise := NewPromise[R]()

	if err := ctx.Err(); err != nil {
		promise.Complete(fn.Err[R](err))
		return promise.Future()
	}

	env := envelope[M, R]{message: msg, promise: promise}
	if err := ref.trySend(env); err != nil {
		promise.Complete(fn.Err[R](err))
	}

	return promise.Future()
}

// trySend makes a single non-blocking delivery attempt and classifies a
// failure. The mailbox reports only whether it accepted the envelope, so the
// reason is recovered from the actor and mailbox state afterwards.
func (ref *actorRefImpl[M, R]) trySend(env envelope[M, R]) error {
	if ref.actor.ctx.Err() != nil {
		return ErrActorTerminated
	}

	if ref.actor.mailbox.TrySend(env) {
		return nil
	}

	switch {
	case ref.actor.ctx.Err() != nil:
		return ErrActorTerminated

	case ref.actor.mailbox.IsClosed():
		return ErrMailboxClosed

	default:
		return ErrMailboxFull
	}
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
