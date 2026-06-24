package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testZeroConfCoopCloseSubscribeEvents exercises the regression that was
// reported when production builds switched to a multi-confirmation
// reorg-aware close dispatch: SubscribeChannelEvents stopped emitting
// CLOSED_CHANNEL on cooperative closes for zero-conf channels until the
// close had reached the full confirmation depth, instead of firing at first
// detection like v0.20.1.
//
// The fix wires an early-dispatch callback into the chain watcher that fires
// a preliminary CLOSED_CHANNEL event over the channel notifier as soon as a
// coop close spend lands on chain. This test asserts:
//
//  1. CLOSED_CHANNEL fires on the SubscribeChannelEvents stream after only
//     the first confirmation of the close tx (not after the full N=3
//     depth required by --dev.force-channel-close-confs=3).
//  2. FULLY_RESOLVED_CHANNEL fires once the close has reached N confs and
//     the channel arbitrator has finished its resolution flow.
//  3. Exactly one CLOSED_CHANNEL is delivered — the suppression logic in
//     the channel arbitrator drops the duplicate that would otherwise fire
//     from MarkChannelClosed at N confs.
func testZeroConfCoopCloseSubscribeEvents(ht *lntest.HarnessTest) {
	// Force coop close to require 3 confs so we exercise the async path
	// in the chain watcher (the same path production hits via
	// CloseConfsForCapacity).
	const requiredConfs = 3

	// Zero-conf channels need option-scid-alias and anchors; force-confs
	// is what flips us out of the numConfs==1 fast-path so we can verify
	// the early dispatch is what surfaces the CLOSED_CHANNEL event.
	nodeArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
		"--dev.force-channel-close-confs=3",
	}

	alice := ht.NewNode("Alice", nodeArgs)
	bob := ht.NewNode("Bob", nodeArgs)

	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)
	ht.EnsureConnected(alice, bob)

	// A channel acceptor on Bob is needed to allow the zero-conf
	// negotiation to succeed.
	acceptStream, cancelAcceptor := bob.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, true, acceptStream)

	const chanAmt = btcutil.Amount(1_000_000)
	openParams := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        true,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ZeroConf:       true,
	}
	stream := ht.OpenChannelAssertStream(alice, bob, openParams)
	cancelAcceptor()

	// Wait for the channel-open update — for zero-conf this arrives
	// without any blocks needing to be mined.
	chanPoint := ht.WaitForChannelOpenEvent(stream)
	ht.AssertChannelInGraph(alice, chanPoint)
	ht.AssertChannelInGraph(bob, chanPoint)

	// Subscribe Alice to channel events BEFORE we initiate the close so
	// we capture the full close lifecycle on the wire.
	chanSub := alice.RPC.SubscribeChannelEvents()

	// Alice initiates the cooperative close; NoWait so the closing tx
	// just lands in the mempool.
	closeStream, _ := ht.CloseChannelAssertPending(alice, chanPoint, false)

	// Mine a single block so the close tx confirms once. For a zero-conf
	// channel the funding tx is still unconfirmed when we initiate the
	// close, so the mempool holds both the funding tx and the close tx
	// when we mine the first block. With the fix in place, the chain
	// watcher's processDetectedSpend should insta-dispatch
	// CLOSED_CHANNEL over the notifier as soon as the close spend lands,
	// even though the async path is still waiting on two more confs
	// before driving MarkChannelClosed.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	closedSeen := waitForChannelEventOfType(
		ht, chanSub,
		lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
	)
	require.NotNil(
		ht, closedSeen,
		"CLOSED_CHANNEL must fire after the first conf of the "+
			"close tx (regression: production was waiting for "+
			"the full 3-conf depth)",
	)

	// The CLOSED_CHANNEL summary must reflect a cooperative close
	// initiated by the local node.
	closedSummary := closedSeen.GetClosedChannel()
	require.NotNil(ht, closedSummary,
		"CLOSED_CHANNEL update must carry a close summary")
	require.Equal(ht,
		lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE,
		closedSummary.CloseType,
	)
	require.Equal(ht,
		lnrpc.Initiator_INITIATOR_LOCAL,
		closedSummary.CloseInitiator,
	)

	// Mine the remaining confs needed to take the close to its final
	// resolution state.
	ht.MineBlocksAndAssertNumTxes(requiredConfs-1, 0)

	// Drain the close-channel client stream so the test cleanup path
	// doesn't hang on it.
	go func() {
		for {
			if _, err := closeStream.Recv(); err != nil {
				return
			}
		}
	}()

	// FULLY_RESOLVED_CHANNEL must arrive after the close advances
	// through the channel arbitrator at full N confs. Crucially, no
	// second CLOSED_CHANNEL event must arrive between the early one and
	// the FULLY_RESOLVED_CHANNEL — the channel-arbitrator-side
	// suppression drops the duplicate that MarkChannelClosed would
	// otherwise emit. We use a stream walker here (rather than
	// waitForChannelEventOfType, which silently discards every event
	// that isn't the one being waited for) so that a regression which
	// re-fires CLOSED_CHANNEL while we wait for FULLY_RESOLVED_CHANNEL
	// is surfaced as a test failure instead of being swallowed.
	resolvedSeen := waitForChannelEventForbidClosed(ht, chanSub)
	require.NotNil(ht, resolvedSeen,
		"FULLY_RESOLVED_CHANNEL must fire after the close reaches "+
			"the required confirmation depth")

	// Belt-and-suspenders: also drain a small quiet window after
	// FULLY_RESOLVED to catch any late-arriving duplicate.
	assertNoMoreClosedEvents(ht, chanSub, 500*time.Millisecond)
}

// waitForChannelEventOfType drains the channel events subscription until
// one of the supplied type lands or the harness's default timeout elapses.
// Other event types (PENDING_OPEN, OPEN, ACTIVE, INACTIVE, CHANNEL_UPDATE)
// are expected during a normal close flow and must be tolerated rather than
// fail the test.
func waitForChannelEventOfType(ht *lntest.HarnessTest,
	sub rpc.ChannelEventsClient,
	want lnrpc.ChannelEventUpdate_UpdateType) *lnrpc.ChannelEventUpdate {

	type result struct {
		event *lnrpc.ChannelEventUpdate
		err   error
	}

	results := make(chan result, 1)
	deadline := time.After(wait.DefaultTimeout)

	go func() {
		for {
			ev, err := sub.Recv()
			if err != nil {
				results <- result{err: err}
				return
			}
			if ev.Type == want {
				results <- result{event: ev}
				return
			}
		}
	}()

	select {
	case r := <-results:
		require.NoErrorf(ht, r.err,
			"error from channel event stream while waiting "+
				"for %v", want)

		return r.event

	case <-deadline:
		ht.Fatalf("timed out waiting for channel event %v", want)
		return nil
	}
}

// waitForChannelEventForbidClosed drains the channel events subscription
// until FULLY_RESOLVED_CHANNEL lands, failing the test immediately if a
// second CLOSED_CHANNEL is observed along the way. This is the strict
// variant of waitForChannelEventOfType for the gap between the early
// CLOSED_CHANNEL (fired by the chain watcher at first conf) and the
// FULLY_RESOLVED_CHANNEL (fired by the channel arbitrator at N confs):
// any CLOSED_CHANNEL in that window would be the duplicate that the
// MarkChannelClosed suppression in the arbitrator is meant to drop.
func waitForChannelEventForbidClosed(ht *lntest.HarnessTest,
	sub rpc.ChannelEventsClient) *lnrpc.ChannelEventUpdate {

	type result struct {
		event *lnrpc.ChannelEventUpdate
		err   error
	}

	results := make(chan result, 1)
	deadline := time.After(wait.DefaultTimeout)

	go func() {
		for {
			ev, err := sub.Recv()
			if err != nil {
				results <- result{err: err}
				return
			}

			switch ev.Type {
			case lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL:
				results <- result{event: ev}
				return

			case lnrpc.ChannelEventUpdate_CLOSED_CHANNEL:
				results <- result{event: ev}
				return
			}
		}
	}()

	select {
	case r := <-results:
		require.NoErrorf(ht, r.err,
			"error from channel event stream while waiting "+
				"for FULLY_RESOLVED_CHANNEL")

		require.Equal(ht,
			lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL,
			r.event.Type,
			"unexpected duplicate CLOSED_CHANNEL event observed "+
				"between the early dispatch and "+
				"FULLY_RESOLVED_CHANNEL: %v", r.event,
		)

		return r.event

	case <-deadline:
		ht.Fatalf("timed out waiting for FULLY_RESOLVED_CHANNEL")
		return nil
	}
}

// assertNoMoreClosedEvents reads from the subscription for the supplied
// quiet window and fails the test if a CLOSED_CHANNEL event is observed.
// FULLY_RESOLVED_CHANNEL has already been observed by this point so a
// second CLOSED_CHANNEL would represent the duplicate-notify regression
// that the suppression logic is meant to prevent.
func assertNoMoreClosedEvents(ht *lntest.HarnessTest,
	sub rpc.ChannelEventsClient, window time.Duration) {

	done := make(chan struct{})
	defer close(done)

	errs := make(chan error, 1)
	dups := make(chan *lnrpc.ChannelEventUpdate, 1)

	go func() {
		for {
			ev, err := sub.Recv()
			if err != nil {
				select {
				case errs <- err:
				case <-done:
				}

				return
			}
			if ev.Type ==
				lnrpc.ChannelEventUpdate_CLOSED_CHANNEL {

				select {
				case dups <- ev:
				case <-done:
				}

				return
			}
		}
	}()

	select {
	case dup := <-dups:
		ht.Fatalf("unexpected duplicate CLOSED_CHANNEL event: %v",
			dup)

	case err := <-errs:
		// Stream EOF or context cancel is fine; we just want the
		// quiet window to elapse without a duplicate.
		_ = err

	case <-time.After(window):
		// Quiet window elapsed without a duplicate. Pass.
	}
}
