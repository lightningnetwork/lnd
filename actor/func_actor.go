package actor

import (
	"context"

	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorFunc is a function type that represents an actor which functions purely
// based on a simple function processor.
type ActorFunc[M Message, R any] func(context.Context, M) fn.Result[R]

// FunctionBehavior adapts a function to the ActorBehavior interface.
type FunctionBehavior[M Message, R any] struct {
	fn ActorFunc[M, R]
}

// NewFunctionBehavior creates a behavior from a function.
func NewFunctionBehavior[M Message, R any](
	fn ActorFunc[M, R]) *FunctionBehavior[M, R] {

	return &FunctionBehavior[M, R]{fn: fn}
}

// Receive implements ActorBehavior interface for the function.
//
// TODO(roasbeef): just base it off the function direct instead?
func (b *FunctionBehavior[M, R]) Receive(ctx context.Context,
	msg M) fn.Result[R] {

	return b.fn(ctx, msg)
}

// FunctionBehaviorFromSimple adapts a simpler function to the ActorBehavior
// interface.
func FunctionBehaviorFromSimple[M Message, R any](
	sFunc func(M) (R, error)) *FunctionBehavior[M, R] {

	return NewFunctionBehavior(
		func(ctx context.Context, msg M) fn.Result[R] {
			val, err := sFunc(msg)
			return fn.NewResult(val, err)
		},
	)
}
