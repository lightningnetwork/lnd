package protofsm

import (
	"fmt"
	"reflect"
	"strings"
)

// TransitionEntry represents a single state transition with type safety.
// This provides at-a-glance visibility into how states transition in response
// to events, including any outbox messages emitted.
type TransitionEntry[S, E, M any] struct {
	// Event is the event that triggers this transition.
	Event E

	// ToState is the destination state after this transition.
	ToState S

	// Description explains why this transition occurs and any important
	// context about validation or security checks.
	Description string

	// EmitsOutbox contains the strongly-typed outbox events emitted during
	// this transition. Only the types of these values are significant: a
	// representative zero value of each emitted type is enough.
	EmitsOutbox []M

	// IsTerminal indicates the ToState is a terminal state that only
	// self-loops.
	IsTerminal bool
}

// StateTransitions holds all transitions from a single state.
type StateTransitions[S, E, M any] struct {
	// FromState is the source state for these transitions.
	FromState S

	// Transitions is the list of possible transitions from this state.
	Transitions []TransitionEntry[S, E, M]
}

// TransitionTable defines all valid state transitions for a state machine.
// This provides complete, at-a-glance visibility into the state machine
// behavior and serves as both documentation and a validation target for tests.
//
// This is intended to be used as a static compile-time structure. It is not
// enforced by the compiler; CheckTransition lets tests assert that every
// transition a state machine actually takes appears in its table.
type TransitionTable[S, E, M any] struct {
	// MachineName identifies which state machine this table describes.
	MachineName string

	// States contains the transitions for each state in the machine.
	States []StateTransitions[S, E, M]
}

// GetTransitionsFrom returns all possible transitions from the given state.
// This allows callers to quickly see what events a state can handle and
// where they lead.
func (tt *TransitionTable[S, E, M]) GetTransitionsFrom(
	fromState S) []TransitionEntry[S, E, M] {

	fromType := reflect.TypeOf(fromState)
	for _, st := range tt.States {
		if reflect.TypeOf(st.FromState) == fromType {
			return st.Transitions
		}
	}

	return nil
}

// GetTransitionsFor returns all transitions triggered by a specific event type
// across all states. The result includes the source state for context.
func (tt *TransitionTable[S, E, M]) GetTransitionsFor(
	event E) []TransitionWithSource[S, E, M] {

	var result []TransitionWithSource[S, E, M]
	eventType := reflect.TypeOf(event)

	for _, st := range tt.States {
		for _, t := range st.Transitions {
			if reflect.TypeOf(t.Event) == eventType {
				result = append(result, TransitionWithSource[
					S,
					E,
					M,
				]{
					FromState:       st.FromState,
					TransitionEntry: t,
				})
			}
		}
	}

	return result
}

// TransitionWithSource pairs a transition with its source state.
type TransitionWithSource[S, E, M any] struct {
	// FromState is the state this transition originates from.
	FromState S

	// TransitionEntry contains the transition details.
	TransitionEntry[S, E, M]
}

// GetTerminalTransitions returns all transitions that lead to terminal states.
func (tt *TransitionTable[
	S,
	E,
	M,
]) GetTerminalTransitions() []TransitionWithSource[S, E, M] {

	var terminals []TransitionWithSource[S, E, M]

	for _, st := range tt.States {
		for _, t := range st.Transitions {
			if t.IsTerminal {
				terminals = append(
					terminals, TransitionWithSource[
						S,
						E,
						M,
					]{
						FromState:       st.FromState,
						TransitionEntry: t,
					},
				)
			}
		}
	}

	return terminals
}

// RenderMarkdown generates a markdown table showing all transitions.
func (tt *TransitionTable[S, E, M]) RenderMarkdown() string {
	var sb strings.Builder

	sb.WriteString(
		fmt.Sprintf("# %s State Transitions\n\n", tt.MachineName),
	)
	sb.WriteString(
		"| From State | Event | To State | Outbox Messages | " +
			"Description |\n",
	)
	sb.WriteString(
		"|------------|-------|----------|-----------------|--------" +
			"-----|\n",
	)

	for _, st := range tt.States {
		fromStateName := trimPackagePrefix(
			reflect.TypeOf(st.FromState).String(),
		)

		for _, t := range st.Transitions {
			event := trimPackagePrefix(
				reflect.TypeOf(t.Event).String(),
			)
			toStateName := trimPackagePrefix(
				reflect.TypeOf(t.ToState).String(),
			)

			terminal := ""
			if t.IsTerminal {
				terminal = " (terminal)"
			}

			outbox := ""
			if len(t.EmitsOutbox) > 0 {
				var outboxBuilder strings.Builder
				for i, msg := range t.EmitsOutbox {
					if i > 0 {
						outboxBuilder.WriteString(", ")
					}
					msgType := trimPackagePrefix(
						reflect.TypeOf(msg).String(),
					)
					outboxBuilder.WriteString(msgType)
				}
				outbox = outboxBuilder.String()
			}

			sb.WriteString(
				fmt.Sprintf("| %s | %s | %s%s | "+
					"%s | %s |\n",
					fromStateName, event, toStateName,
					terminal, outbox, t.Description),
			)
		}
	}

	return sb.String()
}

// trimPackagePrefix removes package prefixes from type names while preserving
// composite type prefixes like [], *, []*. For example:
//   - "*pkg.Type" becomes "*Type"
//   - "[]*pkg.Type" becomes "[]*Type"
//   - "**pkg.Type" becomes "**Type"
//   - "[]pkg.Type" becomes "[]Type"
func trimPackagePrefix(typeName string) string {
	// Extract any leading type modifiers (*, [], []*) that should be
	// preserved. We scan for the start of the actual package/type name.
	prefixEnd := 0
	for i := 0; i < len(typeName); i++ {
		switch typeName[i] {
		case '*', '[', ']':
			prefixEnd = i + 1

		default:
			// Found the start of the package/type name.
			goto done
		}
	}
done:

	prefix := typeName[:prefixEnd]
	remainder := typeName[prefixEnd:]

	// Find the last dot in the remainder and take everything after it.
	if idx := strings.LastIndex(remainder, "."); idx != -1 {
		return prefix + remainder[idx+1:]
	}

	return typeName
}

// CheckTransition reports whether the table contains an entry for a
// transition from `from` on `event` that lands in `to` and emits exactly the
// given outbox event types, in order. Matching is by dynamic type only. It
// returns a descriptive error for the first mismatch, which makes it suitable
// for use from a TransitionObserver in property tests.
func (tt *TransitionTable[S, E, M]) CheckTransition(from S, event E, to S,
	outbox []M) error {

	fromName := typeName(from)
	eventName := typeName(event)
	toName := typeName(to)

	candidates := tt.GetTransitionsFrom(from)
	if candidates == nil {
		return fmt.Errorf("%s: no transitions listed from state %s",
			tt.MachineName, fromName)
	}

	var sawEvent bool
	for _, entry := range candidates {
		if reflect.TypeOf(entry.Event) != reflect.TypeOf(event) {
			continue
		}
		sawEvent = true

		if reflect.TypeOf(entry.ToState) != reflect.TypeOf(to) {
			continue
		}

		if sameTypes(entry.EmitsOutbox, outbox) {
			return nil
		}
	}

	if !sawEvent {
		return fmt.Errorf("%s: state %s has no listed transition on "+
			"event %s", tt.MachineName, fromName, eventName)
	}

	return fmt.Errorf("%s: unlisted transition %s --%s--> %s emitting "+
		"[%s]", tt.MachineName, fromName, eventName, toName,
		typeNames(outbox))
}

// sameTypes reports whether a and b hold values of the same dynamic types, in
// the same order.
func sameTypes[M any](a, b []M) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if reflect.TypeOf(a[i]) != reflect.TypeOf(b[i]) {
			return false
		}
	}

	return true
}

// typeName returns the package-trimmed dynamic type name of v.
func typeName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return "<nil>"
	}

	return trimPackagePrefix(t.String())
}

// typeNames returns a comma separated list of the dynamic type names of vs.
func typeNames[M any](vs []M) string {
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, typeName(v))
	}

	return strings.Join(names, ", ")
}
