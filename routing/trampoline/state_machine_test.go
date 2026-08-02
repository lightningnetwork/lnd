package trampoline

import (
	"encoding/json"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var testRelayConfig = RelayConfig{
	MaxParts:           100,
	MaxAttempts:        3,
	HoldBlocks:         40,
	RecoveryBlocks:     6,
	ExpirySafetyBlocks: 1,
	MaxReorgDepth:      6,
}

// TestRelayLifecycle verifies MPP collection, replay handling, retry, and
// exactly-once successful resolution.
func TestRelayLifecycle(t *testing.T) {
	t.Parallel()

	machine := newTestRelayMachine(t, 100, 90, 500)
	first := RelayPart{
		ID:         PartID{ChannelID: 2, HTLCID: 4},
		AmountMsat: 40,
		CLTV:       700,
	}
	second := RelayPart{
		ID:         PartID{ChannelID: 1, HTLCID: 9},
		AmountMsat: 60,
		CLTV:       700,
	}

	actions, err := machine.AddPart(first)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, RelayCollecting, machine.Snapshot().State)

	actions, err = machine.AddPart(first)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, uint64(40), machine.Snapshot().CollectedMsat)

	conflict := first
	conflict.AmountMsat++
	_, err = machine.AddPart(conflict)
	require.ErrorIs(t, err, ErrPartConflict)

	actions, err = machine.AddPart(second)
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:       ActionPaymentReady,
		AmountMsat: 100,
	}}, actions)
	require.Equal(t, RelayReady, machine.Snapshot().State)

	actions, err = machine.BeginAttempt()
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:       ActionStartDownstream,
		AttemptID:  1,
		AmountMsat: 90,
	}}, actions)

	_, err = machine.AttemptFailed(2, true)
	require.ErrorIs(t, err, ErrAttemptMismatch)

	actions, err = machine.AttemptFailed(1, true)
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:      ActionRetryReady,
		AttemptID: 1,
	}}, actions)
	require.Equal(t, RelayReady, machine.Snapshot().State)

	actions, err = machine.BeginAttempt()
	require.NoError(t, err)
	require.Equal(t, uint16(2), actions[0].AttemptID)

	actions, err = machine.AttemptSucceeded(2)
	require.NoError(t, err)
	require.Equal(t, []Action{
		{
			Kind:       ActionFulfillPart,
			PartID:     second.ID,
			AmountMsat: second.AmountMsat,
		},
		{
			Kind:       ActionFulfillPart,
			PartID:     first.ID,
			AmountMsat: first.AmountMsat,
		},
	}, actions)

	snapshot := machine.Snapshot()
	require.Equal(t, RelaySucceeded, snapshot.State)
	require.Len(t, snapshot.Parts, 2)
	for _, part := range snapshot.Parts {
		require.Equal(t, PartFulfilled, part.Resolution)
	}

	actions, err = machine.AttemptSucceeded(2)
	require.NoError(t, err)
	require.Empty(t, actions)
	actions, err = machine.Cancel()
	require.NoError(t, err)
	require.Empty(t, actions)
}

// TestRelayRejectsInvalidParts checks local part, amount, and CLTV bounds.
func TestRelayRejectsInvalidParts(t *testing.T) {
	t.Parallel()

	config := testRelayConfig
	config.MaxParts = 1
	machine, err := NewRelayMachine(
		config, RelayID{}, 100, 90, 600, 500,
	)
	require.NoError(t, err)

	_, err = machine.AddPart(RelayPart{
		ID:         PartID{HTLCID: 1},
		AmountMsat: 1,
		CLTV:       599,
	})
	require.ErrorIs(t, err, ErrPartCLTVInsufficient)

	_, err = machine.AddPart(RelayPart{
		ID:         PartID{HTLCID: 1},
		AmountMsat: 60,
		CLTV:       600,
	})
	require.NoError(t, err)

	_, err = machine.AddPart(RelayPart{
		ID:         PartID{HTLCID: 2},
		AmountMsat: 40,
		CLTV:       600,
	})
	require.ErrorIs(t, err, ErrPartLimit)

	machine = newTestRelayMachine(t, 100, 90, 500)
	_, err = machine.AddPart(RelayPart{
		ID:         PartID{HTLCID: 1},
		AmountMsat: 101,
		CLTV:       600,
	})
	require.ErrorIs(t, err, ErrPartTotalExceeded)
}

// TestRelayDeadlinesRespectIncomingExpiry verifies that the hold and recovery
// windows cannot outlive the earliest upstream HTLC safety deadline.
func TestRelayDeadlinesRespectIncomingExpiry(t *testing.T) {
	t.Parallel()

	config := testRelayConfig
	config.RecoveryBlocks = 10
	machine, err := NewRelayMachine(
		config, RelayID{}, 100, 90, 503, 500,
	)
	require.NoError(t, err)

	_, err = machine.AddPart(RelayPart{
		ID:         PartID{HTLCID: 1},
		AmountMsat: 100,
		CLTV:       505,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(504), machine.Snapshot().HoldDeadline)

	_, err = machine.BeginAttempt()
	require.NoError(t, err)
	restored, actions, err := RestoreRelayMachine(
		config, machine.Snapshot(),
	)
	require.NoError(t, err)
	require.Equal(t, RelayRecovering, restored.Snapshot().State)
	require.Equal(
		t, uint32(504), restored.Snapshot().RecoveryDeadline,
	)
	require.Equal(t, []Action{{
		Kind:       ActionQueryDownstream,
		AttemptID:  1,
		AmountMsat: 90,
	}}, actions)

	expired := machine.Snapshot()
	expired.ObservedHeight = expired.HoldDeadline
	expired.PeakHeight = expired.HoldDeadline
	restored, actions, err = RestoreRelayMachine(config, expired)
	require.NoError(t, err)
	require.Equal(t, RelayCanceling, restored.Snapshot().State)
	require.Equal(t, []Action{
		{
			Kind:       ActionCancelDownstream,
			AttemptID:  1,
			AmountMsat: 90,
		},
		{
			Kind:       ActionQueryDownstream,
			AttemptID:  1,
			AmountMsat: 90,
		},
	}, actions)

	unsafe := machine.Snapshot()
	unsafe.HoldDeadline++
	_, _, err = RestoreRelayMachine(config, unsafe)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)
}

// TestRelayTerminalPaths verifies non-retryable failure, cancellation, timeout,
// and terminal idempotency.
func TestRelayTerminalPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		terminate func(*testing.T, *RelayMachine) []Action
		state     RelayState
	}{
		{
			name: "non retryable failure",
			terminate: func(t *testing.T,
				m *RelayMachine) []Action {

				_, err := m.BeginAttempt()
				require.NoError(t, err)
				actions, err := m.AttemptFailed(1, false)
				require.NoError(t, err)

				return actions
			},
			state: RelayFailed,
		},
		{
			name: "cancel",
			terminate: func(t *testing.T,
				m *RelayMachine) []Action {

				actions, err := m.Cancel()
				require.NoError(t, err)
				return actions
			},
			state: RelayCanceled,
		},
		{
			name: "timeout",
			terminate: func(t *testing.T,
				m *RelayMachine) []Action {

				actions, err := m.ObserveHeight(540)
				require.NoError(t, err)
				return actions
			},
			state: RelayTimedOut,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			machine := readyTestRelayMachine(t)
			actions := test.terminate(t, machine)
			require.Len(t, actions, 2)
			for _, action := range actions {
				require.Equal(t, ActionFailPart, action.Kind)
			}
			require.Equal(t, test.state, machine.Snapshot().State)

			duplicate, err := machine.Cancel()
			require.NoError(t, err)
			require.Empty(t, duplicate)
		})
	}
}

// TestRelayRestartReconciliation verifies that a persisted active attempt is
// queried and cannot start a competing payment after restart.
func TestRelayRestartReconciliation(t *testing.T) {
	t.Parallel()

	machine := readyTestRelayMachine(t)
	_, err := machine.BeginAttempt()
	require.NoError(t, err)

	encoded, err := machine.EncodeSnapshot()
	require.NoError(t, err)
	decoded, err := DecodeRelaySnapshot(encoded)
	require.NoError(t, err)

	restored, actions, err := RestoreRelayMachine(
		testRelayConfig, decoded,
	)
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:       ActionQueryDownstream,
		AttemptID:  1,
		AmountMsat: 90,
	}}, actions)
	require.Equal(t, RelayRecovering, restored.Snapshot().State)

	restored, actions, err = RestoreRelayMachine(
		testRelayConfig, restored.Snapshot(),
	)
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:       ActionQueryDownstream,
		AttemptID:  1,
		AmountMsat: 90,
	}}, actions)
	require.Equal(t, RelayRecovering, restored.Snapshot().State)

	_, err = restored.BeginAttempt()
	require.ErrorIs(t, err, ErrRelayState)

	actions, err = restored.AttemptSucceeded(1)
	require.NoError(t, err)
	require.Len(t, actions, 2)
	require.Equal(t, RelaySucceeded, restored.Snapshot().State)
}

// TestRelayRecoveryTimeout verifies that an unresolved restarted payment is
// canceled at the reconciliation deadline and remains held until its durable
// downstream attempt reports a terminal failure.
func TestRelayRecoveryTimeout(t *testing.T) {
	t.Parallel()

	machine := readyTestRelayMachine(t)
	_, err := machine.BeginAttempt()
	require.NoError(t, err)
	restored, _, err := RestoreRelayMachine(
		testRelayConfig, machine.Snapshot(),
	)
	require.NoError(t, err)

	actions, err := restored.ObserveHeight(506)
	require.NoError(t, err)
	require.Equal(t, []Action{{
		Kind:       ActionCancelDownstream,
		AttemptID:  1,
		AmountMsat: 90,
	}}, actions)
	require.Equal(t, RelayCanceling, restored.Snapshot().State)

	actions, err = restored.ObserveHeight(507)
	require.NoError(t, err)
	require.Empty(t, actions)

	actions, err = restored.AttemptFailed(1, true)
	require.NoError(t, err)
	require.Len(t, actions, 2)
	require.Equal(t, RelayTimedOut, restored.Snapshot().State)
}

// TestRelayCancelingSurvivesRestart verifies that a crash between persisting
// cancellation and applying it externally reissues cancellation and payment
// reconciliation without resolving an upstream part.
func TestRelayCancelingSurvivesRestart(t *testing.T) {
	t.Parallel()

	machine := readyTestRelayMachine(t)
	_, err := machine.BeginAttempt()
	require.NoError(t, err)
	actions, err := machine.ObserveHeight(540)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	require.Equal(t, RelayCanceling, machine.Snapshot().State)

	restored, actions, err := RestoreRelayMachine(
		testRelayConfig, machine.Snapshot(),
	)
	require.NoError(t, err)
	require.Equal(t, []Action{
		{
			Kind:       ActionCancelDownstream,
			AttemptID:  1,
			AmountMsat: 90,
		},
		{
			Kind:       ActionQueryDownstream,
			AttemptID:  1,
			AmountMsat: 90,
		},
	}, actions)

	actions, err = restored.AttemptSucceeded(1)
	require.NoError(t, err)
	require.Len(t, actions, 2)
	require.Equal(t, RelaySucceeded, restored.Snapshot().State)
}

// TestRelayHeightAndReorgBoundaries verifies bounded backward movement and an
// irreversible terminal timeout.
func TestRelayHeightAndReorgBoundaries(t *testing.T) {
	t.Parallel()

	machine := newTestRelayMachine(t, 100, 90, 500)
	_, err := machine.ObserveHeight(510)
	require.NoError(t, err)
	_, err = machine.ObserveHeight(504)
	require.NoError(t, err)
	require.Equal(t, uint32(510), machine.Snapshot().PeakHeight)
	require.Equal(t, uint32(504), machine.Snapshot().ObservedHeight)

	_, err = machine.ObserveHeight(503)
	require.ErrorIs(t, err, ErrReorgTooDeep)
	require.Equal(t, uint32(504), machine.Snapshot().ObservedHeight)

	actions, err := machine.ObserveHeight(540)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, RelayTimedOut, machine.Snapshot().State)

	actions, err = machine.ObserveHeight(530)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.Equal(t, RelayTimedOut, machine.Snapshot().State)
}

// TestRelaySnapshotValidation rejects semantic corruption, duplicate parts,
// unknown fields, and trailing JSON values.
func TestRelaySnapshotValidation(t *testing.T) {
	t.Parallel()

	machine := readyTestRelayMachine(t)
	snapshot := machine.Snapshot()

	corrupt := snapshot
	corrupt.CollectedMsat++
	_, _, err := RestoreRelayMachine(testRelayConfig, corrupt)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	corrupt = snapshot
	corrupt.Parts = append(corrupt.Parts, corrupt.Parts[0])
	_, _, err = RestoreRelayMachine(testRelayConfig, corrupt)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	_, err = DecodeRelaySnapshot([]byte(`{"unknown":1}`))
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	encoded = append(encoded, []byte(` {}`)...)
	_, err = DecodeRelaySnapshot(encoded)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)
}

// TestRelayConfigurationAndTransitionGuards covers constructor, state, and
// resource-bound failures that cannot arise along a valid happy-path trace.
func TestRelayConfigurationAndTransitionGuards(t *testing.T) {
	t.Parallel()

	configs := []RelayConfig{
		{
			MaxAttempts: 1, HoldBlocks: 1,
			RecoveryBlocks: 1, ExpirySafetyBlocks: 1,
		},
		{
			MaxParts: 1, HoldBlocks: 1,
			RecoveryBlocks: 1, ExpirySafetyBlocks: 1,
		},
		{
			MaxParts: 1, MaxAttempts: 1,
			RecoveryBlocks: 1, ExpirySafetyBlocks: 1,
		},
		{
			MaxParts: 1, MaxAttempts: 1, HoldBlocks: 1,
			ExpirySafetyBlocks: 1,
		},
		{
			MaxParts: 1, MaxAttempts: 1, HoldBlocks: 1,
			RecoveryBlocks: 1,
		},
	}
	for _, config := range configs {
		require.ErrorIs(t, config.Validate(), ErrInvalidRelayConfig)
	}

	_, err := NewRelayMachine(testRelayConfig, RelayID{}, 0, 1, 1, 0)
	require.ErrorIs(t, err, ErrInvalidRelayConfig)
	_, err = NewRelayMachine(testRelayConfig, RelayID{}, 1, 0, 1, 0)
	require.ErrorIs(t, err, ErrInvalidRelayConfig)
	_, err = NewRelayMachine(testRelayConfig, RelayID{}, 1, 2, 1, 0)
	require.ErrorIs(t, err, ErrInvalidRelayConfig)
	_, err = NewRelayMachine(
		testRelayConfig, RelayID{}, 1, 1, 1,
		math.MaxUint32-testRelayConfig.HoldBlocks+1,
	)
	require.ErrorIs(t, err, ErrInvalidRelayConfig)

	machine := newTestRelayMachine(t, 100, 90, 500)
	_, err = machine.BeginAttempt()
	require.ErrorIs(t, err, ErrRelayState)
	_, err = machine.AttemptSucceeded(1)
	require.ErrorIs(t, err, ErrRelayState)
	_, err = machine.AttemptFailed(1, false)
	require.ErrorIs(t, err, ErrRelayState)
	_, err = machine.AddPart(RelayPart{CLTV: 600})
	require.ErrorIs(t, err, ErrPartConflict)

	terminal := readyTestRelayMachine(t)
	_, err = terminal.Cancel()
	require.NoError(t, err)
	_, err = terminal.AddPart(RelayPart{
		ID: PartID{HTLCID: 9}, AmountMsat: 1, CLTV: 700,
	})
	require.ErrorIs(t, err, ErrRelayTerminal)
	_, err = terminal.BeginAttempt()
	require.ErrorIs(t, err, ErrRelayTerminal)

	ready := readyTestRelayMachine(t).Snapshot()
	ready.Attempts = testRelayConfig.MaxAttempts
	restored, _, err := RestoreRelayMachine(testRelayConfig, ready)
	require.NoError(t, err)
	actions, err := restored.BeginAttempt()
	require.NoError(t, err)
	require.Len(t, actions, len(ready.Parts))
	require.Equal(t, RelayFailed, restored.Snapshot().State)

	_, _, err = RestoreRelayMachine(RelayConfig{}, ready)
	require.ErrorIs(t, err, ErrInvalidRelayConfig)

	overflowMachine, err := NewRelayMachine(
		testRelayConfig, RelayID{}, 100, 90, 600,
		math.MaxUint32-testRelayConfig.HoldBlocks,
	)
	require.NoError(t, err)
	for i, amount := range []uint64{40, 60} {
		_, err = overflowMachine.AddPart(RelayPart{
			ID: PartID{HTLCID: uint64(i)}, AmountMsat: amount,
			CLTV: math.MaxUint32,
		})
		require.NoError(t, err)
	}
	_, err = overflowMachine.BeginAttempt()
	require.NoError(t, err)
	overflow := overflowMachine.Snapshot()
	overflow.ObservedHeight = math.MaxUint32 - 2
	overflow.PeakHeight = math.MaxUint32 - 2
	_, _, err = RestoreRelayMachine(testRelayConfig, overflow)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)
}

// TestRelaySnapshotInvariantBranches verifies every persisted state class is
// rejected when one of its defining invariants is corrupted.
func TestRelaySnapshotInvariantBranches(t *testing.T) {
	t.Parallel()

	ready := readyTestRelayMachine(t).Snapshot()
	mutations := []func(*RelaySnapshot){
		func(s *RelaySnapshot) { s.Version++ },
		func(s *RelaySnapshot) { s.ExpectedTotalMsat = 0 },
		func(s *RelaySnapshot) { s.DownstreamMsat = 0 },
		func(s *RelaySnapshot) {
			s.DownstreamMsat = s.ExpectedTotalMsat + 1
		},
		func(s *RelaySnapshot) { s.HoldDeadline++ },
		func(s *RelaySnapshot) {
			s.ObservedHeight = s.PeakHeight + 1
		},
		func(s *RelaySnapshot) {
			s.Attempts = testRelayConfig.MaxAttempts + 1
		},
		func(s *RelaySnapshot) {
			s.Parts[0].AmountMsat = 0
		},
		func(s *RelaySnapshot) {
			s.Parts[0].CLTV = s.RequiredCLTV - 1
		},
		func(s *RelaySnapshot) { s.CollectedMsat++ },
		func(s *RelaySnapshot) {
			s.State = RelayCollecting
		},
		func(s *RelaySnapshot) {
			s.ActiveAttempt = 1
		},
		func(s *RelaySnapshot) {
			s.State = RelayPaying
		},
		func(s *RelaySnapshot) {
			s.State = RelayState(255)
		},
		func(s *RelaySnapshot) {
			s.RecoveryDeadline = s.ObservedHeight + 1
		},
	}
	for i, mutate := range mutations {
		snapshot := ready
		snapshot.Parts = append([]RelayPart(nil), ready.Parts...)
		mutate(&snapshot)
		_, _, err := RestoreRelayMachine(testRelayConfig, snapshot)
		require.ErrorIsf(
			t, err, ErrInvalidRelaySnapshot, "mutation %d", i,
		)
	}

	succeededMachine := readyTestRelayMachine(t)
	_, err := succeededMachine.BeginAttempt()
	require.NoError(t, err)
	_, err = succeededMachine.AttemptSucceeded(1)
	require.NoError(t, err)
	succeeded := succeededMachine.Snapshot()

	unresolvedSuccess := succeeded
	unresolvedSuccess.Parts = append(
		[]RelayPart(nil), succeeded.Parts...,
	)
	unresolvedSuccess.Parts[0].Resolution = PartUnresolved
	_, _, err = RestoreRelayMachine(testRelayConfig, unresolvedSuccess)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	unresolvedFailure := succeeded
	unresolvedFailure.State = RelayFailed
	unresolvedFailure.Parts = append(
		[]RelayPart(nil), succeeded.Parts...,
	)
	_, _, err = RestoreRelayMachine(testRelayConfig, unresolvedFailure)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	earlyResolution := ready
	earlyResolution.Parts = append([]RelayPart(nil), ready.Parts...)
	earlyResolution.Parts[0].Resolution = PartFulfilled
	_, _, err = RestoreRelayMachine(testRelayConfig, earlyResolution)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	overflow := ready
	overflow.State = RelayCollecting
	overflow.ExpectedTotalMsat = math.MaxUint64
	overflow.DownstreamMsat = 1
	overflow.CollectedMsat = 0
	overflow.Parts = []RelayPart{
		{
			ID: PartID{HTLCID: 1}, AmountMsat: math.MaxUint64,
			CLTV: ready.RequiredCLTV,
		},
		{
			ID: PartID{HTLCID: 2}, AmountMsat: 1,
			CLTV: ready.RequiredCLTV,
		},
	}
	_, _, err = RestoreRelayMachine(testRelayConfig, overflow)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	tooManyParts := ready
	tooManyParts.Parts = make(
		[]RelayPart, int(testRelayConfig.MaxParts)+1,
	)
	_, _, err = RestoreRelayMachine(testRelayConfig, tooManyParts)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)

	recovering := readyTestRelayMachine(t)
	_, err = recovering.BeginAttempt()
	require.NoError(t, err)
	recoveringSnapshot := recovering.Snapshot()
	recoveringSnapshot.State = RelayRecovering
	recoveringSnapshot.RecoveryDeadline =
		recoveringSnapshot.ObservedHeight
	_, _, err = RestoreRelayMachine(
		testRelayConfig, recoveringSnapshot,
	)
	require.ErrorIs(t, err, ErrInvalidRelaySnapshot)
}

// TestRelayConcurrentParts checks that concurrent arrival cannot create value,
// lose a part, or emit readiness more than once.
func TestRelayConcurrentParts(t *testing.T) {
	t.Parallel()

	const numParts = 100
	machine := newTestRelayMachine(t, numParts, numParts-1, 500)

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		readyActions int
		errs         []error
	)
	for i := range numParts {
		wg.Add(1)
		go func() {
			defer wg.Done()

			actions, err := machine.AddPart(RelayPart{
				ID: PartID{
					ChannelID: uint64(i % 4),
					HTLCID:    uint64(i),
				},
				AmountMsat: 1,
				CLTV:       600,
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			for _, action := range actions {
				if action.Kind == ActionPaymentReady {
					readyActions++
				}
			}
		}()
	}
	wg.Wait()

	require.Empty(t, errs)
	require.Equal(t, 1, readyActions)
	snapshot := machine.Snapshot()
	require.Equal(t, uint64(numParts), snapshot.CollectedMsat)
	require.Len(t, snapshot.Parts, numParts)
	require.Equal(t, RelayReady, snapshot.State)
}

// TestRelayLifecycleProperties checks value conservation, replay idempotency,
// terminal resolution, and retry safety over generated MPP groups.
func TestRelayLifecycleProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		numParts := rapid.IntRange(1, 32).Draw(rt, "parts")
		partAmount := rapid.Uint64Range(
			1, math.MaxUint64/uint64(numParts)/2,
		).Draw(rt, "part_amount")
		total := uint64(numParts) * partAmount
		reverse := rapid.Bool().Draw(rt, "reverse")

		machine := newTestRelayMachineRT(
			rt, total, total, 500,
		)
		readyActions := 0
		for n := range numParts {
			index := n
			if reverse {
				index = numParts - n - 1
			}
			part := RelayPart{
				ID: PartID{
					ChannelID: uint64(index % 3),
					HTLCID:    uint64(index),
				},
				AmountMsat: partAmount,
				CLTV:       600,
			}

			actions, err := machine.AddPart(part)
			require.NoError(rt, err)
			for _, action := range actions {
				if action.Kind == ActionPaymentReady {
					readyActions++
				}
			}

			duplicate, err := machine.AddPart(part)
			require.NoError(rt, err)
			require.Empty(rt, duplicate)
		}

		require.Equal(rt, 1, readyActions)
		require.Equal(rt, total, machine.Snapshot().CollectedMsat)

		_, err := machine.BeginAttempt()
		require.NoError(rt, err)
		actions, err := machine.AttemptSucceeded(1)
		require.NoError(rt, err)
		require.Len(rt, actions, numParts)

		var fulfilled uint64
		for _, action := range actions {
			require.Equal(rt, ActionFulfillPart, action.Kind)
			fulfilled += action.AmountMsat
		}
		require.Equal(rt, total, fulfilled)

		duplicate, err := machine.AttemptSucceeded(1)
		require.NoError(rt, err)
		require.Empty(rt, duplicate)
		require.Equal(rt, RelaySucceeded, machine.Snapshot().State)
	})
}

// TestRelayCancelingProperties checks that arbitrary MPP groups remain held
// throughout downstream cancellation and resolve exactly once from the
// authoritative terminal result.
func TestRelayCancelingProperties(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		numParts := rapid.IntRange(1, 32).Draw(rt, "parts")
		partAmount := rapid.Uint64Range(
			1, math.MaxUint64/uint64(numParts)/2,
		).Draw(rt, "part_amount")
		total := uint64(numParts) * partAmount
		settles := rapid.Bool().Draw(rt, "settles")
		retryable := rapid.Bool().Draw(rt, "retryable")

		machine := newTestRelayMachineRT(rt, total, total, 500)
		for i := range numParts {
			_, err := machine.AddPart(RelayPart{
				ID: PartID{
					ChannelID: uint64(i % 4),
					HTLCID:    uint64(i),
				},
				AmountMsat: partAmount,
				CLTV:       600,
			})
			require.NoError(rt, err)
		}

		_, err := machine.BeginAttempt()
		require.NoError(rt, err)
		actions, err := machine.ObserveHeight(540)
		require.NoError(rt, err)
		require.Equal(rt, []Action{{
			Kind:       ActionCancelDownstream,
			AttemptID:  1,
			AmountMsat: total,
		}}, actions)
		for _, part := range machine.Snapshot().Parts {
			require.Equal(rt, PartUnresolved, part.Resolution)
		}

		if settles {
			actions, err = machine.AttemptSucceeded(1)
		} else {
			actions, err = machine.AttemptFailed(1, retryable)
		}
		require.NoError(rt, err)
		require.Len(rt, actions, numParts)
		for _, action := range actions {
			expectedKind := ActionFailPart
			if settles {
				expectedKind = ActionFulfillPart
			}
			require.Equal(rt, expectedKind, action.Kind)
		}

		duplicate, err := machine.AttemptFailed(1, false)
		require.NoError(rt, err)
		require.Empty(rt, duplicate)
	})
}

// FuzzDecodeRelaySnapshot checks parser totality and invariant validation for
// persisted lifecycle state.
func FuzzDecodeRelaySnapshot(f *testing.F) {
	machine := readyTestRelayMachine(f)
	encoded, err := machine.EncodeSnapshot()
	require.NoError(f, err)

	f.Add(encoded)
	f.Add([]byte{})
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		snapshot, err := DecodeRelaySnapshot(data)
		if err != nil {
			return
		}

		restored, _, err := RestoreRelayMachine(
			testRelayConfig, snapshot,
		)
		if err != nil {
			return
		}

		reencoded, err := restored.EncodeSnapshot()
		require.NoError(t, err)
		redecoded, err := DecodeRelaySnapshot(reencoded)
		require.NoError(t, err)
		require.Equal(t, restored.Snapshot(), redecoded)
	})
}

func newTestRelayMachine(t testing.TB, total, downstream uint64,
	height uint32) *RelayMachine {

	t.Helper()

	machine, err := NewRelayMachine(
		testRelayConfig, RelayID{}, total, downstream, 600, height,
	)
	require.NoError(t, err)

	return machine
}

func newTestRelayMachineRT(t *rapid.T, total, downstream uint64,
	height uint32) *RelayMachine {

	machine, err := NewRelayMachine(
		testRelayConfig, RelayID{}, total, downstream, 600, height,
	)
	require.NoError(t, err)

	return machine
}

func readyTestRelayMachine(t testing.TB) *RelayMachine {
	t.Helper()

	machine := newTestRelayMachine(t, 100, 90, 500)
	for i, amount := range []uint64{40, 60} {
		_, err := machine.AddPart(RelayPart{
			ID: PartID{
				ChannelID: uint64(2 - i),
				HTLCID:    uint64(i),
			},
			AmountMsat: amount,
			CLTV:       700,
		})
		require.NoError(t, err)
	}

	return machine
}
