package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/trampolinerelay"
)

// Observation records the production state after one replayed model event.
type Observation struct {
	Seq      uint32
	Event    string
	Snapshot trampoline.RelaySnapshot
	Actions  []trampoline.Action
	// DispatchClaimed reports whether this event won the service scheduling
	// gate, while DispatchScheduled reports the gate state afterward.
	DispatchClaimed   bool
	DispatchScheduled bool
	Err               error
}

// Replay executes a trace against the real relay state machine.
func Replay(trace *Trace) ([]Observation, error) {
	if err := trace.Validate(); err != nil {
		return nil, err
	}

	config := trace.Config.RelayConfig()
	machine, err := trampoline.NewRelayMachine(
		config, trampoline.RelayID{},
		trace.Relay.ExpectedTotalMsat,
		trace.Relay.DownstreamMsat,
		trace.Relay.RequiredCLTV,
		trace.Relay.StartHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("construct relay: %w", err)
	}

	monitor := NewMonitor(config)
	var dispatcher trampolinerelay.DispatchGuard
	observations := make([]Observation, 0, len(trace.Events))
	for i := range trace.Events {
		event := &trace.Events[i]
		var actions []trampoline.Action
		var eventErr error

		var dispatchClaimed bool
		machine, actions, dispatchClaimed, eventErr = dispatch(
			machine, &dispatcher, config, event,
		)
		snapshot := machine.Snapshot()
		observation := Observation{
			Seq:               event.Seq,
			Event:             event.Name,
			Snapshot:          snapshot,
			Actions:           actions,
			DispatchClaimed:   dispatchClaimed,
			DispatchScheduled: dispatcher.Scheduled(),
			Err:               eventErr,
		}
		observations = append(observations, observation)

		if err := compareExpected(event, observation); err != nil {
			return observations, fmt.Errorf(
				"trace %s event %d: %w", trace.ID, i, err,
			)
		}
		if eventErr == nil {
			err := monitor.Observe(snapshot, actions)
			if err != nil {
				return observations, fmt.Errorf(
					"trace %s event %d: %w",
					trace.ID, i, err,
				)
			}
		}
	}

	return observations, nil
}

func dispatch(machine *trampoline.RelayMachine,
	dispatcher *trampolinerelay.DispatchGuard,
	config trampoline.RelayConfig,
	event *Event) (*trampoline.RelayMachine, []trampoline.Action,
	bool, error) {

	switch event.Name {
	case "add_part":
		var payload struct {
			ChannelID  uint64 `json:"channel_id"`
			HTLCID     uint64 `json:"htlc_id"`
			AmountMsat uint64 `json:"amount_msat"`
			CLTV       uint32 `json:"cltv"`
		}
		if err := decodePayload(event.Payload, &payload); err != nil {
			return machine, nil, false, err
		}
		actions, err := machine.AddPart(trampoline.RelayPart{
			ID: trampoline.PartID{
				ChannelID: payload.ChannelID,
				HTLCID:    payload.HTLCID,
			},
			AmountMsat: payload.AmountMsat,
			CLTV:       payload.CLTV,
		})

		claimed := false
		if err == nil {
			snapshot := machine.Snapshot()
			claimed = dispatcher.ScheduleReady(
				snapshot.State == trampoline.RelayReady,
			)
		}

		return machine, actions, claimed, err

	case "notify_ready":
		claimed := dispatcher.ScheduleReady(
			machine.Snapshot().State == trampoline.RelayReady,
		)

		return machine, nil, claimed, nil

	case "begin_attempt":
		actions, err := dispatcher.Begin(machine)

		return machine, actions, false, err

	case "attempt_success", "reconcile_success":
		var payload struct {
			AttemptID uint16 `json:"attempt_id"`
		}
		if err := decodePayload(event.Payload, &payload); err != nil {
			return machine, nil, false, err
		}
		actions, err := machine.AttemptSucceeded(payload.AttemptID)

		dispatcher.Finish(false)

		return machine, actions, false, err

	case "attempt_failure", "reconcile_failure":
		var payload struct {
			AttemptID uint16 `json:"attempt_id"`
			Retryable bool   `json:"retryable"`
		}
		if err := decodePayload(event.Payload, &payload); err != nil {
			return machine, nil, false, err
		}
		actions, err := machine.AttemptFailed(
			payload.AttemptID, payload.Retryable,
		)

		retry := err == nil && hasAction(
			actions, trampoline.ActionRetryReady,
		)
		dispatcher.Finish(retry)

		return machine, actions, false, err

	case "cancel":
		actions, err := machine.Cancel()

		dispatcher.Finish(false)

		return machine, actions, false, err

	case "observe_height":
		var payload struct {
			Height uint32 `json:"height"`
		}
		if err := decodePayload(event.Payload, &payload); err != nil {
			return machine, nil, false, err
		}
		actions, err := machine.ObserveHeight(payload.Height)

		return machine, actions, false, err

	case "restart":
		dispatcher.Finish(false)
		encoded, err := machine.EncodeSnapshot()
		if err != nil {
			return machine, nil, false, err
		}
		snapshot, err := trampoline.DecodeRelaySnapshot(encoded)
		if err != nil {
			return machine, nil, false, err
		}
		restored, actions, err := trampoline.RestoreRelayMachine(
			config, snapshot,
		)
		if err != nil {
			return machine, nil, false, err
		}

		return restored, actions, false, nil

	case "reconcile_pending":
		var payload struct {
			AttemptID uint16 `json:"attempt_id"`
		}
		if err := decodePayload(event.Payload, &payload); err != nil {
			return machine, nil, false, err
		}
		snapshot := machine.Snapshot()
		if (snapshot.State != trampoline.RelayRecovering &&
			snapshot.State != trampoline.RelayCanceling) ||
			snapshot.ActiveAttempt != payload.AttemptID {

			return machine, nil, false, fmt.Errorf(
				"pending result does not match recovery",
			)
		}

		return machine, nil, false, nil

	default:
		return machine, nil, false, fmt.Errorf(
			"unsupported event %q", event.Name,
		)
	}
}

func hasAction(actions []trampoline.Action, kind trampoline.ActionKind) bool {
	for _, action := range actions {
		if action.Kind == kind {
			return true
		}
	}

	return false
}

func decodePayload(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		return fmt.Errorf("event payload is empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode event payload: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode event payload: trailing JSON")
	}

	return nil
}

func compareExpected(event *Event, observation Observation) error {
	expected := event.Expected
	if expected.ErrorContains == "" {
		if observation.Err != nil {
			return fmt.Errorf(
				"unexpected error: %w", observation.Err,
			)
		}
	} else {
		if observation.Err == nil ||
			!strings.Contains(
				observation.Err.Error(), expected.ErrorContains,
			) {

			return fmt.Errorf(
				"error=%w, expected substring %q",
				observation.Err, expected.ErrorContains,
			)
		}
	}

	if expected.State != "" {
		actual := relayStateName(observation.Snapshot.State)
		if actual != expected.State {
			return fmt.Errorf(
				"state=%s, expected=%s", actual,
				expected.State,
			)
		}
	}
	if expected.CollectedMsat != nil &&
		observation.Snapshot.CollectedMsat != *expected.CollectedMsat {

		return fmt.Errorf(
			"collected=%d, expected=%d",
			observation.Snapshot.CollectedMsat,
			*expected.CollectedMsat,
		)
	}
	if expected.Attempts != nil &&
		observation.Snapshot.Attempts != *expected.Attempts {

		return fmt.Errorf(
			"attempts=%d, expected=%d",
			observation.Snapshot.Attempts, *expected.Attempts,
		)
	}
	if expected.ActiveAttempt != nil &&
		observation.Snapshot.ActiveAttempt != *expected.ActiveAttempt {

		return fmt.Errorf(
			"active attempt=%d, expected=%d",
			observation.Snapshot.ActiveAttempt,
			*expected.ActiveAttempt,
		)
	}
	if expected.DispatchClaimed != nil &&
		observation.DispatchClaimed != *expected.DispatchClaimed {

		return fmt.Errorf(
			"dispatch claimed=%v, expected=%v",
			observation.DispatchClaimed, *expected.DispatchClaimed,
		)
	}
	if expected.DispatchScheduled != nil &&
		observation.DispatchScheduled != *expected.DispatchScheduled {

		return fmt.Errorf(
			"dispatch scheduled=%v, expected=%v",
			observation.DispatchScheduled,
			*expected.DispatchScheduled,
		)
	}

	actualActions := make([]ExpectedAction, 0, len(observation.Actions))
	for _, action := range observation.Actions {
		actualActions = append(actualActions, ExpectedAction{
			Kind:       actionKindName(action.Kind),
			ChannelID:  action.PartID.ChannelID,
			HTLCID:     action.PartID.HTLCID,
			AttemptID:  action.AttemptID,
			AmountMsat: action.AmountMsat,
		})
	}
	if !equalActions(actualActions, expected.Actions) {
		return fmt.Errorf(
			"actions=%+v, expected=%+v",
			actualActions, expected.Actions,
		)
	}

	return nil
}

func equalActions(a, b []ExpectedAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func relayStateName(state trampoline.RelayState) string {
	switch state {
	case trampoline.RelayCollecting:
		return "collecting"
	case trampoline.RelayReady:
		return "ready"
	case trampoline.RelayPaying:
		return "paying"
	case trampoline.RelayRecovering:
		return "recovering"
	case trampoline.RelaySucceeded:
		return "succeeded"
	case trampoline.RelayFailed:
		return "failed"
	case trampoline.RelayCanceled:
		return "canceled"
	case trampoline.RelayTimedOut:
		return "timed_out"
	case trampoline.RelayCanceling:
		return "canceling"
	default:
		return fmt.Sprintf("unknown(%d)", state)
	}
}

func actionKindName(kind trampoline.ActionKind) string {
	switch kind {
	case trampoline.ActionPaymentReady:
		return "payment_ready"
	case trampoline.ActionStartDownstream:
		return "start_downstream"
	case trampoline.ActionRetryReady:
		return "retry_ready"
	case trampoline.ActionQueryDownstream:
		return "query_downstream"
	case trampoline.ActionFulfillPart:
		return "fulfill_part"
	case trampoline.ActionFailPart:
		return "fail_part"
	case trampoline.ActionCancelDownstream:
		return "cancel_downstream"
	default:
		return fmt.Sprintf("unknown(%d)", kind)
	}
}
