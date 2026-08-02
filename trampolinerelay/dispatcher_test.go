package trampolinerelay

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/stretchr/testify/require"
)

// TestDispatchGuardReadyReplay verifies that concurrent and replayed Ready
// notifications can claim only one downstream dispatch.
func TestDispatchGuardReadyReplay(t *testing.T) {
	t.Parallel()

	var guard DispatchGuard
	var claims atomic.Uint32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if guard.ScheduleReady(true) {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, uint32(1), claims.Load())
	require.True(t, guard.Scheduled())

	machine, err := trampoline.NewRelayMachine(
		trampoline.RelayConfig{
			MaxParts:           1,
			MaxAttempts:        2,
			HoldBlocks:         40,
			RecoveryBlocks:     6,
			ExpirySafetyBlocks: 1,
			MaxReorgDepth:      6,
		}, trampoline.RelayID{}, 100, 90, 600, 500,
	)
	require.NoError(t, err)
	_, err = machine.AddPart(trampoline.RelayPart{
		ID:         trampoline.PartID{ChannelID: 1, HTLCID: 1},
		AmountMsat: 100,
		CLTV:       700,
	})
	require.NoError(t, err)
	actions, err := guard.Begin(machine)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	require.Equal(t, trampoline.ActionStartDownstream, actions[0].Kind)
	_, err = guard.Begin(machine)
	require.ErrorIs(t, err, ErrDispatchNotScheduled)
}

// TestDispatchGuardReadyRestart verifies that volatile scheduling state does
// not prevent a durable Ready relay from being dispatched after restart.
func TestDispatchGuardReadyRestart(t *testing.T) {
	t.Parallel()

	var beforeRestart DispatchGuard
	require.True(t, beforeRestart.ScheduleReady(true))
	require.False(t, beforeRestart.ScheduleReady(true))

	var afterRestart DispatchGuard
	require.True(t, afterRestart.ScheduleReady(true))
	require.False(t, afterRestart.ScheduleReady(true))
}
