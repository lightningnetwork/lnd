package discovery

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestGossipSyncerSingleBacklogSend tests that only one goroutine can send the
// backlog at a time using the atomic flag.
func TestGossipSyncerSingleBacklogSend(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Track how many goroutines are actively sending.
	var (
		activeGoroutines        atomic.Int32
		totalGoroutinesLaunched atomic.Int32
	)

	// Create a blocking sendToPeerSync function. We'll use this to simulate
	// sending a large backlog.
	blockingSendChan := make(chan struct{})
	mockSendMsg := func(_ context.Context, sync bool,
		msgs ...lnwire.Message) error {

		// Sync is only true when calling `sendToPeerSync`.
		if !sync {
			return nil
		}

		// Track that we're in a send goroutine.
		count := activeGoroutines.Add(1)
		totalGoroutinesLaunched.Add(1)

		// Verify only one goroutine is active.
		require.Equal(
			t, int32(1), count,
			"only one goroutine should be sending at a time",
		)

		// We'll now block to simulate slow sending.
		<-blockingSendChan

		// When we exit, we should decrement the count on the way out
		activeGoroutines.Add(-1)

		return nil
	}

	// Now we'll kick off the test by making a syncer that uses our blocking
	// send function.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize, true, true, true,
	)

	// Override the sendMsg to use our blocking version.
	syncer.cfg.sendMsg = mockSendMsg
	syncer.cfg.ignoreHistoricalFilters = false

	syncer.Start()
	defer syncer.Stop()

	// Next, we'll launch a goroutine to send out a backlog of messages.
	go func() {
		for {
			select {
			case <-chanSeries.horizonReq:
				cid := lnwire.NewShortChanIDFromInt(1)
				chanSeries.horizonResp <- []lnwire.Message{
					&lnwire.ChannelUpdate1{
						ShortChannelID: cid,
						Timestamp: uint32(
							time.Now().Unix(),
						),
					},
				}

			case <-time.After(5 * time.Second):
				return
			}
		}
	}()

	// Now we'll create a filter, then apply it in a goroutine.
	filter := &lnwire.GossipTimestampRange{
		FirstTimestamp: uint32(time.Now().Unix() - 3600),
		TimestampRange: 7200,
	}
	go func() {
		err := syncer.ApplyGossipFilter(ctx, filter)
		require.NoError(t, err)
	}()

	// Wait for the first goroutine to start and block.
	time.Sleep(100 * time.Millisecond)

	// Verify the atomic flag is set, as the first goroutine should be
	// blocked on the send.
	require.True(
		t, syncer.isSendingBacklog.Load(),
		"isSendingBacklog should be true while first goroutine "+
			"is active",
	)

	// Now apply more filters concurrently - they should all return early as
	// we're still sending out the first backlog.
	var (
		wg           sync.WaitGroup
		earlyReturns atomic.Int32
	)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Record the flag state before calling.
			flagWasSet := syncer.isSendingBacklog.Load()

			err := syncer.ApplyGossipFilter(ctx, filter)
			require.NoError(t, err)

			// If the flag was already set, we should have returned
			// early.
			if flagWasSet {
				earlyReturns.Add(1)
			}
		}()
	}

	// Give time for the concurrent attempts to execute.
	time.Sleep(100 * time.Millisecond)

	// There should still be only a single active goroutine.
	require.Equal(
		t, int32(1), activeGoroutines.Load(),
		"only one goroutine should be active despite multiple attempts",
	)

	// Now we'll unblock the first goroutine, then wait for them all to
	// exit.
	close(blockingSendChan)
	wg.Wait()

	// Give time for cleanup.
	time.Sleep(100 * time.Millisecond)

	// At this point, only a single goroutine should have been launched,
	require.Equal(
		t, int32(1), totalGoroutinesLaunched.Load(),
		"only one goroutine should have been launched total",
	)
	require.GreaterOrEqual(
		t, earlyReturns.Load(), int32(4),
		"at least 4 calls should have returned early due to atomic "+
			"flag",
	)

	// The atomic flag should be cleared now.
	require.False(
		t, syncer.isSendingBacklog.Load(),
		"isSendingBacklog should be false after goroutine completes",
	)

	// Drain any messages.
	select {
	case <-msgChan:
	case <-time.After(100 * time.Millisecond):
	}
}
