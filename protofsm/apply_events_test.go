package protofsm

import (
	"context"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// counterEvent is the event type of a small counter machine used to exercise
// ApplyEvents and TransitionTable without any daemon machinery.
type counterEvent interface {
	counterEvent()
}

// incEvent increments the counter by one and emits the new value.
type incEvent struct{}

func (incEvent) counterEvent() {}

// burstEvent increments the counter N times by emitting N internal incEvents.
type burstEvent struct{ n int }

func (burstEvent) counterEvent() {}

// failEvent makes the transition function return an error.
type failEvent struct{}

func (failEvent) counterEvent() {}

// counterOut is the outbox type of the counter machine.
type counterOut interface {
	counterOut()
}

// emitValue reports the counter value after an increment.
type emitValue struct{ v int }

func (emitValue) counterOut() {}

// burstStarted reports that a burst was expanded into internal events.
type burstStarted struct{ n int }

func (burstStarted) counterOut() {}

// counterEnv is the environment of the counter machine.
type counterEnv struct{}

func (counterEnv) Name() string { return "counter" }

// counterTransition is the transition type of the counter machine.
type counterTransition = StateTransition[counterEvent, counterOut, counterEnv]

// counterState is the single state of the counter machine.
type counterState struct{ v int }

func (c *counterState) String() string { return "counterState" }

func (c *counterState) IsTerminal() bool { return false }

func (c *counterState) ProcessEvent(_ context.Context, ev counterEvent,
	_ counterEnv) (*counterTransition, error) {

	switch e := ev.(type) {
	case incEvent:
		next := &counterState{v: c.v + 1}

		return &counterTransition{
			NextState: next,
			NewEvents: fn.Some(EmittedEvent[counterEvent, counterOut]{
				Outbox: []counterOut{emitValue{v: next.v}},
			}),
		}, nil

	case burstEvent:
		internal := make([]counterEvent, e.n)
		for i := range internal {
			internal[i] = incEvent{}
		}

		return &counterTransition{
			NextState: c,
			NewEvents: fn.Some(EmittedEvent[counterEvent, counterOut]{
				InternalEvent: internal,
				Outbox:        []counterOut{burstStarted{n: e.n}},
			}),
		}, nil

	case failEvent:
		return nil, errors.New("boom")
	}

	return nil, errors.New("unknown event")
}

// TestApplyEventsOrdering asserts that ApplyEvents processes internal events
// in FIFO order and accumulates every transition's outbox in emission order.
func TestApplyEventsOrdering(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	start := &counterState{v: 10}

	var observed []counterOut
	observe := func(_ counterEvent, _, _ State[counterEvent, counterOut,
		counterEnv], outbox []counterOut) {

		observed = append(observed, outbox...)
	}

	final, outbox, err := ApplyEventsObserved(
		ctx, State[counterEvent, counterOut, counterEnv](start),
		counterEvent(burstEvent{n: 3}), counterEnv{}, observe,
	)
	require.NoError(t, err)
	require.Equal(t, 13, final.(*counterState).v)

	expected := []counterOut{
		burstStarted{n: 3}, emitValue{v: 11}, emitValue{v: 12},
		emitValue{v: 13},
	}
	require.Equal(t, expected, outbox)

	// The observer saw the same outbox, split per transition.
	require.Equal(t, expected, observed)

	// The starting state is a value the caller still owns, and must not
	// have been mutated.
	require.Equal(t, 10, start.v)
}

// TestApplyEventsError asserts that a failing transition returns the last
// state successfully reached and discards the outbox.
func TestApplyEventsError(t *testing.T) {
	t.Parallel()

	start := &counterState{v: 1}
	final, outbox, err := ApplyEvents(
		t.Context(), State[counterEvent, counterOut, counterEnv](start),
		counterEvent(failEvent{}), counterEnv{},
	)
	require.Error(t, err)
	require.Nil(t, outbox)
	require.Same(t, start, final)
}

// TestApplyEventsBurstEquivalence asserts that expanding a burst into
// internal events reaches the same state, and emits the same values, as
// applying each increment as its own external event.
func TestApplyEventsBurstEquivalence(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()
		v0 := rapid.IntRange(-1000, 1000).Draw(rt, "v0")
		n := rapid.IntRange(0, 50).Draw(rt, "n")

		var burst State[counterEvent, counterOut, counterEnv]
		burst = &counterState{v: v0}
		burst, burstOut, err := ApplyEvents(
			ctx, burst, counterEvent(burstEvent{n: n}),
			counterEnv{},
		)
		require.NoError(rt, err)

		var step State[counterEvent, counterOut, counterEnv]
		step = &counterState{v: v0}
		stepOut := make([]counterOut, 0, n)
		for range n {
			var out []counterOut
			step, out, err = ApplyEvents(
				ctx, step, counterEvent(incEvent{}),
				counterEnv{},
			)
			require.NoError(rt, err)
			stepOut = append(stepOut, out...)
		}

		require.Equal(
			rt, step.(*counterState).v, burst.(*counterState).v,
		)
		require.Equal(rt, burstOut[0], counterOut(burstStarted{n: n}))
		require.Equal(rt, stepOut, burstOut[1:])
	})
}
