package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/trampolinerelay"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestRepresentativeTraces replays the checked-in P scenarios against the
// production relay machine.
func TestRepresentativeTraces(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob(filepath.Join("..", "traces", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			trace, err := ParseTraceFile(path)
			require.NoError(t, err)
			observations, err := Replay(trace)
			require.NoError(t, err)
			require.Len(t, observations, len(trace.Events))
		})
	}
}

// TestParseTraceRejectsMalformedInput checks the bridge's format and resource
// bounds before a malformed trace can reach production code.
func TestParseTraceRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	valid := readTrace(t, "cancel.json")
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "empty",
		},
		{
			name: "unknown top level field",
			data: bytes.Replace(
				valid, []byte(`"version": 1`),
				[]byte(`"version": 1, "extra": true`), 1,
			),
		},
		{
			name: "unsupported version",
			data: bytes.Replace(
				valid, []byte(`"version": 1`),
				[]byte(`"version": 2`), 1,
			),
		},
		{
			name: "bad sequence",
			data: bytes.Replace(
				valid, []byte(`"seq": 0`),
				[]byte(`"seq": 9`), 1,
			),
		},
		{
			name: "unknown event",
			data: bytes.Replace(
				valid, []byte(`"event": "add_part"`),
				[]byte(`"event": "explode"`), 1,
			),
		},
		{
			name: "trailing json",
			data: append(
				append([]byte{}, valid...), []byte(`{}`)...,
			),
		},
		{
			name: "oversized",
			data: bytes.Repeat([]byte{'x'}, maxTraceSize+1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseTrace(bytes.NewReader(test.data))
			require.Error(t, err)
		})
	}
}

// TestReplayRejectsDivergence ensures expected P observations are enforced.
func TestReplayRejectsDivergence(t *testing.T) {
	t.Parallel()

	trace, err := ParseTrace(bytes.NewReader(readTrace(t, "cancel.json")))
	require.NoError(t, err)
	trace.Events[0].Expected.State = "paying"

	_, err = Replay(trace)
	require.ErrorContains(t, err, "state=ready, expected=paying")
}

// TestReplayChecksExpectedErrors verifies that rejected model events leave the
// machine usable and are compared by stable error meaning.
func TestReplayChecksExpectedErrors(t *testing.T) {
	t.Parallel()

	zero := uint64(0)
	trace := &Trace{
		Version: 1,
		ID:      "expected-error",
		Config: TraceConfig{
			MaxParts:           2,
			MaxAttempts:        2,
			HoldBlocks:         20,
			RecoveryBlocks:     2,
			ExpirySafetyBlocks: 1,
			MaxReorgDepth:      2,
		},
		Relay: TraceRelay{
			ExpectedTotalMsat: 100,
			DownstreamMsat:    90,
			RequiredCLTV:      20,
		},
		Events: []Event{{
			Seq:  0,
			Name: "add_part",
			Payload: json.RawMessage(
				`{"channel_id":1,"htlc_id":1,` +
					`"amount_msat":1,"cltv":19}`,
			),
			Expected: Expected{
				State:         "collecting",
				CollectedMsat: &zero,
				ErrorContains: "cltv insufficient",
			},
		}},
	}

	observations, err := Replay(trace)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	require.ErrorIs(
		t, observations[0].Err, trampoline.ErrPartCLTVInsufficient,
	)
}

// TestReplayRejectsMalformedPayload keeps strict payload parsing at the bridge
// boundary.
func TestReplayRejectsMalformedPayload(t *testing.T) {
	t.Parallel()

	trace, err := ParseTrace(bytes.NewReader(readTrace(t, "cancel.json")))
	require.NoError(t, err)
	trace.Events[0].Payload = json.RawMessage(
		`{"channel_id":8,"htlc_id":12,"amount_msat":100,` +
			`"cltv":700,"surprise":1}`,
	)

	_, err = Replay(trace)
	require.ErrorContains(t, err, "unknown field")
}

// TestMonitorFindsViolations proves that the runtime monitor rejects the
// safety violations represented by the P specifications.
func TestMonitorFindsViolations(t *testing.T) {
	t.Parallel()

	config := trampoline.RelayConfig{
		MaxParts:           2,
		MaxAttempts:        2,
		HoldBlocks:         20,
		RecoveryBlocks:     2,
		ExpirySafetyBlocks: 1,
		MaxReorgDepth:      2,
	}
	part := trampoline.RelayPart{
		ID:         trampoline.PartID{ChannelID: 1, HTLCID: 2},
		AmountMsat: 100,
		CLTV:       40,
		Resolution: trampoline.PartFulfilled,
	}
	success := trampoline.RelaySnapshot{
		State:          trampoline.RelaySucceeded,
		DownstreamMsat: 90,
		CollectedMsat:  100,
		Parts:          []trampoline.RelayPart{part},
	}
	action := trampoline.Action{
		Kind:       trampoline.ActionFulfillPart,
		PartID:     part.ID,
		AmountMsat: part.AmountMsat,
	}

	monitor := NewMonitor(config)
	require.NoError(t, monitor.Observe(success, []trampoline.Action{
		action,
	}))
	require.ErrorContains(
		t, monitor.Observe(success, []trampoline.Action{action}),
		"resolved more than once",
	)

	unresolved := success
	unresolved.Parts = append([]trampoline.RelayPart(nil), part)
	unresolved.Parts[0].Resolution = trampoline.PartUnresolved
	require.ErrorContains(
		t, NewMonitor(config).Observe(unresolved, nil),
		"unresolved",
	)

	createdValue := success
	createdValue.State = trampoline.RelayReady
	createdValue.DownstreamMsat = 101
	createdValue.Parts[0].Resolution = trampoline.PartUnresolved
	require.ErrorContains(
		t, NewMonitor(config).Observe(createdValue, nil),
		"created value",
	)
}

// TestMonitorValidationBranches exercises each runtime counterpart of the P
// safety monitors with a deliberately invalid observation.
func TestMonitorValidationBranches(t *testing.T) {
	t.Parallel()

	config := trampoline.RelayConfig{
		MaxParts:           2,
		MaxAttempts:        2,
		HoldBlocks:         20,
		RecoveryBlocks:     2,
		ExpirySafetyBlocks: 1,
		MaxReorgDepth:      2,
	}
	part := trampoline.RelayPart{
		ID:         trampoline.PartID{ChannelID: 1, HTLCID: 1},
		AmountMsat: 10,
		CLTV:       40,
		Resolution: trampoline.PartUnresolved,
	}
	base := trampoline.RelaySnapshot{
		State:          trampoline.RelayReady,
		DownstreamMsat: 9,
		CollectedMsat:  10,
		Parts:          []trampoline.RelayPart{part},
	}

	tests := []struct {
		name     string
		snapshot trampoline.RelaySnapshot
		actions  []trampoline.Action
		contains string
	}{
		{
			name: "duplicate snapshot part",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.Parts = append(s.Parts, part)
				s.CollectedMsat = 20

				return s
			}(),
			contains: "duplicate part",
		},
		{
			name: "amount overflow",
			snapshot: trampoline.RelaySnapshot{
				State: trampoline.RelayCollecting,
				Parts: []trampoline.RelayPart{
					{ID: trampoline.PartID{HTLCID: 1},
						AmountMsat: math.MaxUint64},
					{ID: trampoline.PartID{HTLCID: 2},
						AmountMsat: 1},
				},
			},
			contains: "overflow",
		},
		{
			name: "collected mismatch",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.CollectedMsat++
				return s
			}(),
			contains: "collected=",
		},
		{
			name: "created value",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.DownstreamMsat = 11
				return s
			}(),
			contains: "created value",
		},
		{
			name: "retry bound",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.Attempts = 3
				return s
			}(),
			contains: "retry bound",
		},
		{
			name: "recovery deadline elapsed",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayRecovering
				s.ObservedHeight = 5
				s.RecoveryDeadline = 5
				s.HoldDeadline = 10

				return s
			}(),
			contains: "recovery deadline",
		},
		{
			name: "recovery deadline exceeds hold",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayRecovering
				s.ObservedHeight = 5
				s.RecoveryDeadline = 11
				s.HoldDeadline = 10

				return s
			}(),
			contains: "recovery deadline",
		},
		{
			name: "terminal unresolved",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayFailed
				return s
			}(),
			contains: "unresolved",
		},
		{
			name: "ready action state",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayCollecting
				return s
			}(),
			actions: []trampoline.Action{{
				Kind: trampoline.ActionPaymentReady,
			}},
			contains: "ready action",
		},
		{
			name: "forward state",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayCollecting
				return s
			}(),
			actions: []trampoline.Action{{
				Kind: trampoline.ActionStartDownstream,
			}},
			contains: "forward in state",
		},
		{
			name: "forward amount",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayPaying
				return s
			}(),
			actions: []trampoline.Action{{
				Kind:       trampoline.ActionStartDownstream,
				AttemptID:  1,
				AmountMsat: 10,
			}},
			contains: "downstream amount",
		},
		{
			name: "forward attempt",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayPaying
				return s
			}(),
			actions: []trampoline.Action{{
				Kind:       trampoline.ActionStartDownstream,
				AmountMsat: 9,
			}},
			contains: "downstream attempt",
		},
		{
			name: "retry action state",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				s.State = trampoline.RelayCollecting
				return s
			}(),
			actions: []trampoline.Action{{
				Kind: trampoline.ActionRetryReady,
			}},
			contains: "retry action",
		},
		{
			name:     "query action state",
			snapshot: base,
			actions: []trampoline.Action{{
				Kind: trampoline.ActionQueryDownstream,
			}},
			contains: "query action",
		},
		{
			name: "resolution mismatch",
			snapshot: func() trampoline.RelaySnapshot {
				s := base
				parts := []trampoline.RelayPart(nil)
				parts = append(parts, base.Parts...)
				s.Parts = parts
				s.State = trampoline.RelaySucceeded
				s.Parts[0].Resolution = trampoline.PartFulfilled

				return s
			}(),
			actions: []trampoline.Action{{
				Kind:       trampoline.ActionFulfillPart,
				PartID:     part.ID,
				AmountMsat: 11,
			}},
			contains: "does not match",
		},
		{
			name:     "fulfill wrong state",
			snapshot: base,
			actions: []trampoline.Action{{
				Kind:       trampoline.ActionFulfillPart,
				PartID:     part.ID,
				AmountMsat: 10,
			}},
			contains: "without success",
		},
		{
			name:     "fail wrong state",
			snapshot: base,
			actions: []trampoline.Action{{
				Kind:       trampoline.ActionFailPart,
				PartID:     part.ID,
				AmountMsat: 10,
			}},
			contains: "without terminal failure",
		},
		{
			name:     "unknown action",
			snapshot: base,
			actions:  []trampoline.Action{{Kind: 255}},
			contains: "unknown action",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := NewMonitor(config).Observe(
				test.snapshot, test.actions,
			)
			require.ErrorContains(t, err, test.contains)
		})
	}

	terminal := base
	terminal.Parts = append(
		[]trampoline.RelayPart(nil), base.Parts...,
	)
	terminal.State = trampoline.RelaySucceeded
	terminal.Parts[0].Resolution = trampoline.PartFulfilled
	monitor := NewMonitor(config)
	require.NoError(t, monitor.Observe(terminal, nil))
	paying := base
	paying.State = trampoline.RelayPaying
	require.ErrorContains(t, monitor.Observe(
		paying, []trampoline.Action{{
			Kind:       trampoline.ActionStartDownstream,
			AttemptID:  1,
			AmountMsat: 9,
		}},
	), "forward after terminal")

	monitor = NewMonitor(config)
	paying = base
	paying.State = trampoline.RelayPaying
	start := trampoline.Action{
		Kind:       trampoline.ActionStartDownstream,
		AttemptID:  1,
		AmountMsat: 9,
	}
	require.NoError(t, monitor.Observe(
		paying, []trampoline.Action{start},
	))
	require.ErrorContains(t, monitor.Observe(
		paying, []trampoline.Action{start},
	), "started twice")
}

// TestBridgeGuardBranches checks strict trace validation, comparison errors,
// payload framing, and the complete stable enum vocabulary.
func TestBridgeGuardBranches(t *testing.T) {
	t.Parallel()

	valid, err := ParseTrace(bytes.NewReader(readTrace(t, "cancel.json")))
	require.NoError(t, err)

	invalid := []*Trace{
		{Version: 1},
		{
			Version: 1, ID: "bad-config",
			Config: TraceConfig{},
		},
		{
			Version: 1, ID: "zero-total",
			Config: valid.Config,
			Events: []Event{{Name: "cancel"}},
		},
		{
			Version: 1, ID: "no-events",
			Config: valid.Config,
			Relay:  TraceRelay{ExpectedTotalMsat: 1},
		},
	}
	for _, trace := range invalid {
		require.Error(t, trace.Validate())
	}

	_, err = ParseTraceFile(filepath.Join(t.TempDir(), "missing.json"))
	require.ErrorContains(t, err, "read trace")

	require.Error(t, decodePayload(nil, &struct{}{}))
	require.Error(t, decodePayload(
		json.RawMessage(`{} {}`), &struct{}{},
	))
	_, _, _, err = dispatch(
		newTestMachine(t), &trampolinerelay.DispatchGuard{},
		valid.Config.RelayConfig(),
		&Event{Name: "unsupported"},
	)
	require.ErrorContains(t, err, "unsupported event")

	baseEvent := &Event{Expected: Expected{}}
	require.ErrorContains(t, compareExpected(
		baseEvent, Observation{Err: errors.New("boom")},
	), "unexpected error")
	baseEvent.Expected.ErrorContains = "wanted"
	require.ErrorContains(t, compareExpected(
		baseEvent, Observation{},
	), "expected substring")
	baseEvent.Expected.ErrorContains = "wanted"
	require.ErrorContains(t, compareExpected(
		baseEvent, Observation{Err: errors.New("different")},
	), "expected substring")

	zero64 := uint64(0)
	zero16 := uint16(0)
	checks := []Expected{
		{CollectedMsat: &zero64},
		{Attempts: &zero16},
		{ActiveAttempt: &zero16},
		{Actions: []ExpectedAction{{Kind: "fail_part"}}},
	}
	for _, expected := range checks {
		event := &Event{Expected: expected}
		observation := Observation{Snapshot: trampoline.RelaySnapshot{
			CollectedMsat: 1, Attempts: 1, ActiveAttempt: 1,
		}}
		require.Error(t, compareExpected(event, observation))
	}

	require.False(t, equalActions(nil, []ExpectedAction{{}}))
	require.False(t, equalActions(
		[]ExpectedAction{{Kind: "a"}},
		[]ExpectedAction{{Kind: "b"}},
	))
	states := []trampoline.RelayState{
		trampoline.RelayCollecting,
		trampoline.RelayReady,
		trampoline.RelayPaying,
		trampoline.RelayRecovering,
		trampoline.RelaySucceeded,
		trampoline.RelayFailed,
		trampoline.RelayCanceled,
		trampoline.RelayTimedOut,
		trampoline.RelayCanceling,
	}
	for _, state := range states {
		require.NotContains(t, relayStateName(state), "unknown")
	}
	require.Contains(t, relayStateName(255), "unknown")
	actions := []trampoline.ActionKind{
		trampoline.ActionPaymentReady,
		trampoline.ActionStartDownstream,
		trampoline.ActionRetryReady,
		trampoline.ActionQueryDownstream,
		trampoline.ActionFulfillPart,
		trampoline.ActionFailPart,
		trampoline.ActionCancelDownstream,
	}
	for _, kind := range actions {
		require.NotContains(t, actionKindName(kind), "unknown")
	}
	require.Contains(t, actionKindName(255), "unknown")
}

// TestRapidReplayPartitions checks arbitrary positive MPP partitions against
// the real relay and bridge observation rules.
func TestRapidReplayPartitions(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		total := rapid.Uint64Range(2, 1_000_000).Draw(t, "total")
		first := rapid.Uint64Range(1, total-1).Draw(t, "first")
		second := total - first
		collectedFirst := first
		collectedTotal := total
		attempt := uint16(1)
		zeroAttempt := uint16(0)
		successActions := []ExpectedAction{
			{
				Kind:       "fulfill_part",
				ChannelID:  1,
				HTLCID:     1,
				AmountMsat: second,
			},
			{
				Kind:       "fulfill_part",
				ChannelID:  2,
				HTLCID:     2,
				AmountMsat: first,
			},
		}

		trace := &Trace{
			Version: 1,
			ID:      "rapid-partition",
			Config: TraceConfig{
				MaxParts:           2,
				MaxAttempts:        2,
				HoldBlocks:         20,
				RecoveryBlocks:     2,
				ExpirySafetyBlocks: 1,
				MaxReorgDepth:      2,
			},
			Relay: TraceRelay{
				ExpectedTotalMsat: total,
				DownstreamMsat:    total - 1,
				RequiredCLTV:      20,
			},
		}
		trace.Events = []Event{
			addPartEvent(
				0, 2, 2, first, collectedFirst, nil,
			),
			addPartEvent(
				1, 1, 1, second, collectedTotal,
				[]ExpectedAction{{
					Kind:       "payment_ready",
					AmountMsat: total,
				}},
			),
			{
				Seq:  2,
				Name: "begin_attempt",
				Expected: Expected{
					State:         "paying",
					Attempts:      &attempt,
					ActiveAttempt: &attempt,
					Actions: []ExpectedAction{{
						Kind:       "start_downstream",
						AttemptID:  1,
						AmountMsat: total - 1,
					}},
				},
			},
			{
				Seq:     3,
				Name:    "attempt_success",
				Payload: json.RawMessage(`{"attempt_id":1}`),
				Expected: Expected{
					State:         "succeeded",
					ActiveAttempt: &zeroAttempt,
					Actions:       successActions,
				},
			},
		}

		_, err := Replay(trace)
		require.NoError(t, err)
	})
}

// FuzzParseTrace exercises the bridge's strict bounded parser.
func FuzzParseTrace(f *testing.F) {
	for _, name := range []string{
		"happy_retry.json", "restart_timeout.json", "cancel.json",
		"ready_replay.json",
	} {
		f.Add(readTrace(f, name))
	}
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		trace, err := ParseTrace(bytes.NewReader(data))
		if err != nil {
			return
		}

		require.NotPanics(t, func() {
			_, _ = Replay(trace)
		})
	})
}

func addPartEvent(seq uint32, channelID, htlcID, amount,
	collected uint64, actions []ExpectedAction) Event {

	payload, err := json.Marshal(map[string]uint64{
		"channel_id":  channelID,
		"htlc_id":     htlcID,
		"amount_msat": amount,
		"cltv":        20,
	})
	if err != nil {
		panic(err)
	}

	state := "collecting"
	if actions != nil {
		state = "ready"
	}

	return Event{
		Seq:     seq,
		Name:    "add_part",
		Payload: payload,
		Expected: Expected{
			State:         state,
			CollectedMsat: &collected,
			Actions:       actions,
		},
	}
}

type testFataler interface {
	Helper()
	Fatalf(string, ...any)
}

func readTrace(t testFataler, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "traces", name))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	return data
}

func newTestMachine(t testing.TB) *trampoline.RelayMachine {
	t.Helper()

	machine, err := trampoline.NewRelayMachine(
		trampoline.RelayConfig{
			MaxParts:           2,
			MaxAttempts:        2,
			HoldBlocks:         20,
			RecoveryBlocks:     2,
			ExpirySafetyBlocks: 1,
			MaxReorgDepth:      2,
		},
		trampoline.RelayID{}, 10, 9, 20, 0,
	)
	require.NoError(t, err)

	return machine
}
