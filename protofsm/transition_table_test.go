package protofsm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestTrimPackagePrefix verifies that trimPackagePrefix correctly handles
// various type name formats including composite types with slices and
// multiple pointer indirections.
func TestTrimPackagePrefix(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple type no package",
			input:    "MyType",
			expected: "MyType",
		},
		{
			name:     "simple type with package",
			input:    "mypackage.MyType",
			expected: "MyType",
		},
		{
			name:     "pointer type with package",
			input:    "*mypackage.MyType",
			expected: "*MyType",
		},
		{
			name:     "double pointer with package",
			input:    "**mypackage.MyType",
			expected: "**MyType",
		},
		{
			name:     "slice type with package",
			input:    "[]mypackage.MyType",
			expected: "[]MyType",
		},
		{
			name:     "slice of pointers with package",
			input:    "[]*mypackage.MyType",
			expected: "[]*MyType",
		},
		{
			name:     "slice of double pointers with package",
			input:    "[]**mypackage.MyType",
			expected: "[]**MyType",
		},
		{
			name:     "nested package path",
			input:    "*github.com/foo/bar/pkg.Type",
			expected: "*Type",
		},
		{
			name:     "slice with nested package",
			input:    "[]*github.com/foo/bar.MyStruct",
			expected: "[]*MyStruct",
		},
		{
			name:     "pointer no package",
			input:    "*MyType",
			expected: "*MyType",
		},
		{
			name:     "slice no package",
			input:    "[]MyType",
			expected: "[]MyType",
		},
		{
			name:     "slice of pointers no package",
			input:    "[]*MyType",
			expected: "[]*MyType",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := trimPackagePrefix(tc.input)
			if result != tc.expected {
				t.Errorf("trimPackagePrefix(%q) = %q, want %q",
					tc.input, result, tc.expected)
			}
		})
	}
}

// counterTable is the transition table of the counter machine.
var counterTable = TransitionTable[
	State[counterEvent, counterOut, counterEnv], counterEvent, counterOut,
]{
	MachineName: "counter",
	States: []StateTransitions[
		State[counterEvent, counterOut, counterEnv], counterEvent,
		counterOut,
	]{{
		FromState: &counterState{},
		Transitions: []TransitionEntry[
			State[counterEvent, counterOut, counterEnv],
			counterEvent, counterOut,
		]{{
			Event:       incEvent{},
			ToState:     &counterState{},
			Description: "Increment and report the new value.",
			EmitsOutbox: []counterOut{emitValue{}},
		}, {
			Event:       burstEvent{},
			ToState:     &counterState{},
			Description: "Expand into N internal increments.",
			EmitsOutbox: []counterOut{burstStarted{}},
		}},
	}},
}

// TestTransitionTableConformance drives the counter machine with random
// events and asserts that every transition it takes is listed in its table.
func TestTransitionTableConformance(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		ctx := context.Background()

		var state State[counterEvent, counterOut, counterEnv]
		state = &counterState{}

		observe := func(ev counterEvent, from, to State[counterEvent,
			counterOut, counterEnv], outbox []counterOut) {

			err := counterTable.CheckTransition(
				from, ev, to, outbox,
			)
			require.NoError(rt, err)
		}

		events := rapid.SliceOfN(rapid.SampledFrom([]counterEvent{
			incEvent{}, burstEvent{n: 0}, burstEvent{n: 2},
		}), 1, 20).Draw(rt, "events")

		for _, ev := range events {
			var err error
			state, _, err = ApplyEventsObserved(
				ctx, state, ev, counterEnv{}, observe,
			)
			require.NoError(rt, err)
		}
	})
}

// TestCheckTransitionRejectsUnlisted asserts that CheckTransition reports
// transitions whose event, destination or outbox is not in the table.
func TestCheckTransitionRejectsUnlisted(t *testing.T) {
	t.Parallel()

	from := &counterState{}

	// An event that has no entry at all.
	err := counterTable.CheckTransition(from, failEvent{}, from, nil)
	require.ErrorContains(t, err, "no listed transition on event")

	// A listed event whose outbox differs from the table.
	err = counterTable.CheckTransition(
		from, incEvent{}, from, []counterOut{burstStarted{}},
	)
	require.ErrorContains(t, err, "unlisted transition")

	// The listed transition itself passes.
	err = counterTable.CheckTransition(
		from, incEvent{}, from, []counterOut{emitValue{v: 7}},
	)
	require.NoError(t, err)

	md := counterTable.RenderMarkdown()
	require.Contains(t, md, "| *counterState | incEvent | *counterState")
}
