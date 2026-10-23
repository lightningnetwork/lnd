package gossipsync

import (
	"testing"
	"testing/synctest"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/stretchr/testify/require"
)

// TestManagerRefusesConnectAfterShutdown asserts that a connection that
// reaches the manager actor after its shutdown is refused, rather than
// spawning peer actors that nothing would ever stop. This is the order a
// peer that starts while the node shuts down produces: its connect passes the
// handle's stopped check, then lands in the mailbox behind the stop.
func TestManagerRefusesConnectAfterShutdown(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()

		system := actor.NewActorSystem()
		t.Cleanup(func() { require.NoError(t, system.Shutdown()) })

		timeouts := timeout.NewActor()
		timeoutRef, err := actor.RegisterWithSystem(
			system, "timeout",
			actor.NewServiceKey[timeout.Msg, timeout.Resp](
				"timeout",
			),
			timeouts,
		)
		require.NoError(t, err)
		timeouts.Start(timeoutRef)

		mgr, err := NewManager(Config{
			ChainHash:  simChain,
			Graph:      newSimGraph(),
			BestHeight: func() uint32 { return 1_000 },
			System:     system,
			Timeouts:   timeoutRef,
		})
		require.NoError(t, err)

		// Run the actor's shutdown, then deliver a connect behind it,
		// as a racing InitSyncState would.
		stop := mgr.ref.Ask(ctx, &lifecycleMsg{start: false})
		_, err = stop.Await(ctx).Unpack()
		require.NoError(t, err)

		res := mgr.ref.Ask(ctx, &connectMsg{conn: &recordingConn{}})
		_, err = res.Await(ctx).Unpack()
		require.ErrorIs(t, err, ErrManagerStopped)
		require.Empty(t, *mgr.peers.Load())

		mgr.Stop()
	})
}
