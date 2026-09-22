package actor

import (
	"context"
	"fmt"
)

// MapInputRef is a message-transforming wrapper around a TellOnlyRef. It
// implements TellOnlyRef[In] and forwards transformed messages to a
// TellOnlyRef[Out]. This lets an actor that expects one message type receive
// notifications from a source that produces a different, but related, type.
//
// For example, a timer service produces a generic expiry message, while the
// actor that scheduled the timer expects its own timeout event. MapInputRef
// bridges the two without an intermediate adapter actor.
type MapInputRef[In Message, Out Message] struct {
	// targetRef is the underlying TellOnlyRef that receives transformed
	// messages.
	targetRef TellOnlyRef[Out]

	// mapFn transforms incoming messages from type In to type Out.
	mapFn func(In) Out
}

// NewMapInputRef creates a new message-transforming wrapper around a
// TellOnlyRef. The mapFn function is called for each message to transform it
// from type In to type Out before forwarding to targetRef.
func NewMapInputRef[In Message, Out Message](targetRef TellOnlyRef[Out],
	mapFn func(In) Out) *MapInputRef[In, Out] {

	return &MapInputRef[In, Out]{
		targetRef: targetRef,
		mapFn:     mapFn,
	}
}

// Tell transforms the incoming message using mapFn and forwards it to the
// target reference.
func (m *MapInputRef[In, Out]) Tell(ctx context.Context, msg In) {
	m.targetRef.Tell(ctx, m.mapFn(msg))
}

// TryTell transforms the incoming message using mapFn and hands it to the
// target's non-blocking send, so a wrapped ref is as backpressure-friendly as
// the ref it wraps.
func (m *MapInputRef[In, Out]) TryTell(ctx context.Context, msg In) error {
	return m.targetRef.TryTell(ctx, m.mapFn(msg))
}

// ID returns a composite identifier incorporating the target's ID.
func (m *MapInputRef[In, Out]) ID() string {
	return fmt.Sprintf("map-input->%s", m.targetRef.ID())
}

// A compile-time check that MapInputRef implements TellOnlyRef.
var _ TellOnlyRef[Message] = (*MapInputRef[Message, Message])(nil)

// FilterMapInputRef is a MapInputRef whose transform may also drop a message:
// when mapFn reports false, Tell succeeds without forwarding anything. Use it to
// adapt a notification source that emits events the target actor has no
// message for, without forcing every target to grow a no-op case.
type FilterMapInputRef[In Message, Out Message] struct {
	// targetRef is the underlying TellOnlyRef that receives transformed
	// messages.
	targetRef TellOnlyRef[Out]

	// mapFn transforms incoming messages from type In to type Out. A false
	// second return value drops the message.
	mapFn func(In) (Out, bool)
}

// NewFilterMapInputRef creates a message-transforming wrapper around a
// TellOnlyRef whose transform may drop messages.
func NewFilterMapInputRef[In Message, Out Message](targetRef TellOnlyRef[Out],
	mapFn func(In) (Out, bool)) *FilterMapInputRef[In, Out] {

	return &FilterMapInputRef[In, Out]{
		targetRef: targetRef,
		mapFn:     mapFn,
	}
}

// Tell transforms the incoming message using mapFn and forwards it to the
// target reference, or silently drops it when mapFn reports false.
func (m *FilterMapInputRef[In, Out]) Tell(ctx context.Context, msg In) {
	transformed, ok := m.mapFn(msg)
	if !ok {
		return
	}

	m.targetRef.Tell(ctx, transformed)
}

// TryTell transforms the incoming message using mapFn and hands it to the
// target's non-blocking send. A message the transform drops reports success,
// mirroring Tell: a dropped message is a normal outcome here, not a failure
// the caller should retry.
func (m *FilterMapInputRef[In, Out]) TryTell(ctx context.Context,
	msg In) error {

	transformed, ok := m.mapFn(msg)
	if !ok {
		return nil
	}

	return m.targetRef.TryTell(ctx, transformed)
}

// ID returns a composite identifier incorporating the target's ID.
func (m *FilterMapInputRef[In, Out]) ID() string {
	return fmt.Sprintf("filter-map-input->%s", m.targetRef.ID())
}

// A compile-time check that FilterMapInputRef implements TellOnlyRef.
var _ TellOnlyRef[Message] = (*FilterMapInputRef[Message, Message])(nil)
