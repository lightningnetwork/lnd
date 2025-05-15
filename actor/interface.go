package actor

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrActorTerminated indicates that an operation failed because the target
// actor was terminated or in the process of shutting down.
var ErrActorTerminated = fmt.Errorf("actor terminated")

// BaseMessage is a helper struct that can be embedded in message types defined
// outside the actor package to satisfy the Message interface's unexported
// messageMarker method.
type BaseMessage struct{}

// messageMarker implements the unexported method for the Message interface,
// allowing types that embed BaseMessage to satisfy the Message interface.
func (BaseMessage) messageMarker() {}

// Message is a sealed interface for actor messages. Actors will receive
// messages conforming to this interface. The interface is "sealed" by the
// unexported messageMarker method, meaning only types that can satisfy it
// (e.g., by embedding BaseMessage or being in the same package) can be Messages.
type Message interface {
	// messageMarker is a private method that makes this a sealed interface
	// (see BaseMessage for embedding).
	messageMarker()

	// MessageType returns the type name of the message for
	// routing/filtering.
	MessageType() string
}

// PriorityMessage is an extension of the Message interface for messages that
// carry a priority level. This can be used by actor mailboxes or schedulers
// to prioritize message processing.
type PriorityMessage interface {
	Message

	// Priority returns the processing priority of this message (higher =
	// more important).
	Priority() int
}

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

// TellOnlyRef is a reference to an actor that only supports "tell" operations.
// This is useful for scenarios where only fire-and-forget message passing is
// needed, or to restrict capabilities.
type TellOnlyRef[M Message] interface {
	// Tell sends a message without waiting for a response. If the
	// context is cancelled before the message can be sent to the actor's
	// mailbox, the message may be dropped.
	Tell(ctx context.Context, msg M)

	// ID returns the unique identifier for this actor.
	ID() string
}

// ActorRef is a reference to an actor that supports both "tell" and "ask"
// operations. It embeds TellOnlyRef and adds the Ask method for
// request-response interactions.
type ActorRef[M Message, R any] interface {
	TellOnlyRef[M]

	// Ask sends a message and returns a Future for the response.
	// The Future will be completed with the actor's reply or an error
	// if the operation fails (e.g., context cancellation before send).
	Ask(ctx context.Context, msg M) Future[R]
}

// ActorBehavior defines the logic for how an actor processes incoming messages.
// It is a strategy interface that encapsulates the actor's reaction to messages.
type ActorBehavior[M Message, R any] interface {
	// Receive processes a message and returns a Result. The provided
	// context is the actor's internal context, which can be used to
	// detect actor shutdown requests.
	Receive(actorCtx context.Context, msg M) fn.Result[R]
}
