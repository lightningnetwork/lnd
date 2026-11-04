package itest

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	// reputationValueRe extracts the new outgoing reputation from the
	// reputation change line the subsystem logs on every resolution.
	reputationValueRe = regexp.MustCompile(
		`new_outgoing_reputation=(-?\d+)`,
	)

	// reputationSettledRe extracts whether each logged reputation change
	// came from a settled or a failed HTLC.
	reputationSettledRe = regexp.MustCompile(
		`Reputation change: .* settled=(true|false)`,
	)
)

// lastOutgoingReputation returns the outgoing reputation reported by the most
// recent reputation change line in the node's log.
func lastOutgoingReputation(ht *lntest.HarnessTest,
	hn *node.HarnessNode) int64 {

	matches := ht.NodeLogSubmatches(hn, reputationValueRe)
	require.NotEmpty(ht, matches, "no reputation change logged")

	value, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	require.NoError(ht, err)

	return value
}

// countReputationChanges returns how many reputation changes the node has
// logged for settled and for failed HTLCs.
func countReputationChanges(ht *lntest.HarnessTest,
	hn *node.HarnessNode) (int, int) {

	var settled, failed int
	for _, m := range ht.NodeLogSubmatches(hn, reputationSettledRe) {
		if m[1] == "true" {
			settled++
		} else {
			failed++
		}
	}

	return settled, failed
}

// addHoldInvoice creates a hold invoice at the node for the given amount and
// returns its preimage and payment request.
func addHoldInvoice(ht *lntest.HarnessTest, hn *node.HarnessNode,
	amt int64) (lntypes.Preimage, string) {

	preimage := ht.RandomPreimage()
	hash := preimage.Hash()
	invoice := hn.RPC.AddHoldInvoice(&invoicesrpc.AddHoldInvoiceRequest{
		Value: amt,
		Hash:  hash[:],
	})

	return preimage, invoice.PaymentRequest
}

// testLocalReputationPersistence verifies that the local reputation subsystem
// carries its state across a restart of the forwarding node: channel
// reputation is restored from the store when the native SQL backend is in use,
// HTLCs that were in flight during the restart are picked up again with their
// accountable signal and scored when they settle or fail, and closed channels
// are dropped.
func testLocalReputationPersistence(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100_000)
	const paymentAmt = 1000

	// Alice -> Bob -> Carol, with Bob as the forwarding node whose
	// reputation subsystem is under test.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("Carol", nil)

	ht.ConnectNodes(alice, bob)
	ht.ConnectNodes(bob, carol)

	chanPointAB := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	chanPointBC := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)
	ht.AssertChannelInGraph(alice, chanPointBC)

	// 1. A settled forward gives the Bob -> Carol channel its first
	// reputation. Record the value Bob reports for it.
	payReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)
	ht.CompletePaymentRequests(alice, payReqs)
	ht.AssertNodeLogContains(bob, reputationChangeLog)

	repBefore := lastOutgoingReputation(ht, bob)
	require.Positive(ht, repBefore, "settled forward earned nothing")

	// 2. Two more forwards are held at Carol with hold invoices, so they
	// are in flight on both of Bob's channels when he restarts. The
	// second one is sent with the experimental accountable signal, which
	// Bob must recover from his commitment state after the restart.
	settlePreimage, settleReq := addHoldInvoice(ht, carol, paymentAmt)
	cancelPreimage, cancelReq := addHoldInvoice(ht, carol, paymentAmt)

	ht.SendPaymentAndAssertStatus(alice, &routerrpc.SendPaymentRequest{
		PaymentRequest: settleReq,
		FeeLimitMsat:   noFeeLimitMsat,
	}, lnrpc.Payment_IN_FLIGHT)

	ht.SendPaymentAndAssertStatus(alice, &routerrpc.SendPaymentRequest{
		PaymentRequest: cancelReq,
		FeeLimitMsat:   noFeeLimitMsat,
		FirstHopCustomRecords: map[uint64][]byte{
			uint64(lnwire.ExperimentalAccountableType): {
				lnwire.ExperimentalAccountable,
			},
		},
	}, lnrpc.Payment_IN_FLIGHT)

	// Bob has the incoming and the outgoing HTLC of both forwards.
	ht.AssertNumActiveHtlcs(bob, 4)

	settledBefore, failedBefore := countReputationChanges(ht, bob)

	// 3. Restart Bob with both forwards still in flight.
	ht.RestartNode(bob)
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.AssertChannelActive(bob, chanPointAB)
	ht.AssertChannelActive(bob, chanPointBC)

	// Both in-flight forwards are rebuilt from the switch's open circuits,
	// with the accountable signal of the second one recovered.
	ht.AssertNodeLogContains(
		bob, "Reputation replayed 2 of 2 in-flight HTLCs "+
			"(1 accountable)",
	)

	// With the native SQL store both of Bob's channels come back from the
	// store: one earned reputation as the outgoing link and the other
	// revenue as the incoming link. Without it nothing is persisted.
	loadedLog := "Reputation loaded 0 channels from store"
	if *nativeSQLFlag {
		loadedLog = "Reputation loaded 2 channels from store"
	}
	ht.AssertNodeLogContains(bob, loadedLog)

	// 4. Carol settles the first held invoice. Bob must match the
	// resolution to the replayed HTLC and score it as settled.
	carol.RPC.SettleInvoice(settlePreimage[:])
	ht.AssertPaymentStatus(
		alice, settlePreimage.Hash(), lnrpc.Payment_SUCCEEDED,
	)

	err := waitForReputationChanges(
		ht, bob, settledBefore+1, failedBefore,
	)
	require.NoError(ht, err, "replayed settle not scored")

	// With persistence the fee earned by this settle is added on top of
	// the reputation from before the restart, so the channel ends up above
	// where it was. Without persistence it starts from zero again and the
	// single fee cannot exceed the value a single fee produced before.
	repAfter := lastOutgoingReputation(ht, bob)
	if *nativeSQLFlag {
		require.Greater(ht, repAfter, repBefore,
			"reputation not carried across the restart")
	} else {
		require.LessOrEqual(ht, repAfter, repBefore,
			"reputation carried across restart without a store")
	}

	// 5. Carol cancels the second held invoice. The failure must be
	// matched to the replayed HTLC and scored as failed.
	cancelHash := cancelPreimage.Hash()
	carol.RPC.CancelInvoice(cancelHash[:])
	ht.AssertPaymentStatus(alice, cancelHash, lnrpc.Payment_FAILED)

	err = waitForReputationChanges(
		ht, bob, settledBefore+1, failedBefore+1,
	)
	require.NoError(ht, err, "replayed fail not scored")

	// 6. Closing Bob's channels drops their reputation state.
	ht.CloseChannel(alice, chanPointAB)
	ht.CloseChannel(bob, chanPointBC)
	ht.AssertNodeLogCountAtLeast(bob, "Reputation removed channel", 2)
}

// waitForReputationChanges waits until the node has logged exactly the given
// number of settled and failed reputation changes.
func waitForReputationChanges(ht *lntest.HarnessTest, hn *node.HarnessNode,
	wantSettled, wantFailed int) error {

	return wait.NoError(func() error {
		settled, failed := countReputationChanges(ht, hn)
		if settled == wantSettled && failed == wantFailed {
			return nil
		}

		return fmt.Errorf("reputation changes: settled=%d failed=%d, "+
			"want settled=%d failed=%d", settled, failed,
			wantSettled, wantFailed)
	}, lntest.DefaultTimeout)
}
