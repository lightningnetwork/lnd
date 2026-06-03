package itest

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testDeleteForwardingHistory tests the deletion of forwarding history events.
func testDeleteForwardingHistory(ht *lntest.HarnessTest) {
	// Run subtests for different deletion scenarios.
	testCases := []struct {
		name string
		test func(ht *lntest.HarnessTest)
	}{
		{
			name: "basic deletion",
			test: testBasicDeletion,
		},
		{
			name: "partial deletion",
			test: testPartialDeletion,
		},
		{
			name: "empty database",
			test: testEmptyDatabaseDeletion,
		},
		{
			name: "idempotency",
			test: testDeletionIdempotency,
		},
		{
			name: "time formats",
			test: testTimeFormats,
		},
	}

	for _, tc := range testCases {
		success := ht.Run(tc.name, func(t *testing.T) {
			st := ht.Subtest(t)
			tc.test(st)
		})

		if !success {
			return
		}
	}
}

// testBasicDeletion tests basic forwarding history deletion functionality.
func testBasicDeletion(ht *lntest.HarnessTest) {
	// Create a three-hop network: Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	cfgs := [][]string{nil, {"--dev.min-fwd-history-age=2s"}, nil}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	_ = chanPoints

	const numPayments = 10
	const paymentAmt = 1000

	// Send multiple payments from Alice to Carol through Bob. Sleep after
	// each payment to ensure minimum age validation.
	for i := 0; i < numPayments; i++ {
		invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: paymentAmt,
			Memo:      fmt.Sprintf("test payment %d", i),
		})

		payReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   100000,
		}

		ht.SendPaymentAssertSettled(alice, payReq)

		// Sleep to ensure events are old enough.
		time.Sleep(time.Second)
	}

	// Sleep an additional 2 seconds to ensure all events are old enough.
	time.Sleep(2 * time.Second)

	// Query Bob's forwarding history to verify events exist. The switch
	// flushes forwarding events to the DB asynchronously on a 15-second
	// ticker, so poll until the expected count appears.
	var fwdHistory *lnrpc.ForwardingHistoryResponse
	err := wait.NoError(func() error {
		fwdHistory = bob.RPC.ForwardingHistory(nil)
		if len(fwdHistory.ForwardingEvents) != numPayments {
			return fmt.Errorf("expected %d forwarding events, "+
				"got %d", numPayments,
				len(fwdHistory.ForwardingEvents))
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht, err, "timed out waiting for forwarding events")

	// Calculate expected total fees.
	var expectedFees int64
	for _, event := range fwdHistory.ForwardingEvents {
		expectedFees += int64(event.FeeMsat)
	}

	// Record the timestamp of the last event for testing.
	//
	//nolint:ll
	lastTimestamp := fwdHistory.ForwardingEvents[len(fwdHistory.ForwardingEvents)-1].TimestampNs

	// Delete all forwarding events using a timestamp that's 2 seconds in
	// the past to satisfy the minimum age validation.
	//
	//nolint:ll
	delResp := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeTime{
				DeleteBeforeTime: uint64(time.Now().Add(
					-2 * time.Second).Unix(),
				),
			},
		},
	)

	// Verify deletion statistics.
	require.Equal(
		ht, uint64(numPayments), delResp.EventsDeleted,
		"wrong number of events deleted",
	)
	require.Equal(
		ht, expectedFees, delResp.TotalFeeMsat,
		"wrong total fees",
	)
	require.Contains(
		ht, delResp.Status, "Successfully deleted",
		"unexpected status message",
	)

	// Query forwarding history again to verify events are deleted.
	fwdHistoryAfter := bob.RPC.ForwardingHistory(nil)
	require.Empty(
		ht, fwdHistoryAfter.ForwardingEvents,
		"forwarding events should be deleted",
	)

	// Verify that the last event timestamp is no longer in the history.
	fwdHistorySpecific := bob.RPC.ForwardingHistory(
		&lnrpc.ForwardingHistoryRequest{
			StartTime: 0,
			EndTime:   lastTimestamp,
		},
	)
	require.Empty(
		ht, fwdHistorySpecific.ForwardingEvents,
		"specific time range query should return no events",
	)
}

// testPartialDeletion tests deleting only a subset of forwarding events.
func testPartialDeletion(ht *lntest.HarnessTest) {
	// Create a three-hop network: Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	cfgs := [][]string{nil, {"--dev.min-fwd-history-age=2s"}, nil}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	_ = chanPoints

	const firstBatch = 5
	const paymentAmt = 1000

	// Send first batch of payments.
	for i := 0; i < firstBatch; i++ {
		invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: paymentAmt,
			Memo:      fmt.Sprintf("batch 1 payment %d", i),
		})

		payReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   100000,
		}

		ht.SendPaymentAssertSettled(alice, payReq)

		// Sleep to ensure events are old enough.
		time.Sleep(time.Second)
	}

	// Record the timestamp after first batch.
	cutoffTime := time.Now()

	// Send a second batch of payments.
	const secondBatch = 5
	for i := 0; i < secondBatch; i++ {
		invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: paymentAmt,
			Memo:      fmt.Sprintf("batch 2 payment %d", i),
		})

		payReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   100000,
		}

		ht.SendPaymentAssertSettled(alice, payReq)
	}

	// Query Bob's forwarding history to verify all events exist. The switch
	// flushes forwarding events to the DB asynchronously on a 15-second
	// ticker, so poll until the expected count appears.
	totalExpected := firstBatch + secondBatch
	var fwdHistory *lnrpc.ForwardingHistoryResponse
	err := wait.NoError(func() error {
		fwdHistory = bob.RPC.ForwardingHistory(nil)
		if len(fwdHistory.ForwardingEvents) != totalExpected {
			return fmt.Errorf("expected %d forwarding events, "+
				"got %d", totalExpected,
				len(fwdHistory.ForwardingEvents))
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht, err, "timed out waiting for forwarding events")

	// Delete only the first batch of events using the cutoff time.
	//
	//nolint:ll
	delResp := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeTime{
				DeleteBeforeTime: uint64(cutoffTime.Unix()),
			},
		},
	)

	// Should have deleted approximately the first batch.
	require.LessOrEqual(
		ht, delResp.EventsDeleted, uint64(firstBatch),
		"deleted more events than expected",
	)
	require.Greater(
		ht, delResp.EventsDeleted, uint64(0),
		"should have deleted some events",
	)

	// Query forwarding history to verify second batch remains.
	fwdHistoryAfter := bob.RPC.ForwardingHistory(nil)
	require.NotEmpty(
		ht, fwdHistoryAfter.ForwardingEvents,
		"some forwarding events should remain",
	)
	require.GreaterOrEqual(
		ht, len(fwdHistoryAfter.ForwardingEvents), secondBatch-1,
		"at least most of second batch should remain",
	)
}

// testEmptyDatabaseDeletion tests deletion on an empty forwarding log.
func testEmptyDatabaseDeletion(ht *lntest.HarnessTest) {
	// Create a standalone node (no channels, no forwards).
	bob := ht.NewNode("Bob", nil)

	// Try to delete from empty database using custom duration format.
	//
	//nolint:ll
	delResp := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeDuration{
				DeleteBeforeDuration: "-1d",
			},
		},
	)

	// Should successfully handle empty database.
	require.Equal(
		ht, uint64(0), delResp.EventsDeleted,
		"should delete 0 events from empty database",
	)
	require.Equal(
		ht, int64(0), delResp.TotalFeeMsat,
		"should have 0 fees from empty database",
	)
}

// testDeletionIdempotency tests that deletion is idempotent.
func testDeletionIdempotency(ht *lntest.HarnessTest) {
	// Create a three-hop network: Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	cfgs := [][]string{nil, {"--dev.min-fwd-history-age=2s"}, nil}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	_ = chanPoints

	// Send a few payments to create forwarding events.
	const numPayments = 5
	const paymentAmt = 1000

	for i := 0; i < numPayments; i++ {
		invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: paymentAmt,
			Memo:      fmt.Sprintf("payment %d", i),
		})

		payReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   100000,
		}

		ht.SendPaymentAssertSettled(alice, payReq)

		// Sleep to ensure events are old enough.
		time.Sleep(time.Second)
	}

	// Sleep an additional 2 seconds to ensure all events are old enough.
	time.Sleep(2 * time.Second)

	// Verify events exist. Poll until the async flush has persisted them.
	var fwdHistory *lnrpc.ForwardingHistoryResponse
	err := wait.NoError(func() error {
		fwdHistory = bob.RPC.ForwardingHistory(nil)
		if len(fwdHistory.ForwardingEvents) != numPayments {
			return fmt.Errorf("expected %d forwarding events, "+
				"got %d", numPayments,
				len(fwdHistory.ForwardingEvents))
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht, err, "timed out waiting for forwarding events")

	// Delete all events using a timestamp 2 seconds in the past.
	deleteTime := uint64(time.Now().Add(-2 * time.Second).Unix())

	//nolint:ll
	delResp1 := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeTime{
				DeleteBeforeTime: deleteTime,
			},
		},
	)

	require.Equal(
		ht, uint64(numPayments), delResp1.EventsDeleted,
		"first deletion should delete all events",
	)

	// Delete again with same parameters.
	//
	//nolint:ll
	delResp2 := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeTime{
				DeleteBeforeTime: deleteTime,
			},
		},
	)

	// Second deletion should delete nothing (idempotent).
	require.Equal(
		ht, uint64(0), delResp2.EventsDeleted,
		"second deletion should delete 0 events (idempotent)",
	)
	require.Equal(
		ht, int64(0), delResp2.TotalFeeMsat,
		"second deletion should have 0 fees",
	)
}

// testTimeFormats tests different time specification formats.
func testTimeFormats(ht *lntest.HarnessTest) {
	// Create a three-hop network: Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(300000)
	p := lntest.OpenChannelParams{Amt: chanAmt}

	cfgs := [][]string{nil, {"--dev.min-fwd-history-age=2s"}, nil}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, p)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	_ = chanPoints

	// Helper function to create forwarding events.
	createForwards := func(count int) {
		for i := 0; i < count; i++ {
			invoice := carol.RPC.AddInvoice(&lnrpc.Invoice{
				ValueMsat: 1000,
				Memo:      fmt.Sprintf("payment %d", i),
			})

			payReq := &routerrpc.SendPaymentRequest{
				PaymentRequest: invoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitMsat:   100000,
			}

			ht.SendPaymentAssertSettled(alice, payReq)

			// Sleep to ensure events are old enough.
			time.Sleep(time.Second)
		}

		// Sleep an additional 2 seconds to ensure all events are old
		// enough.
		time.Sleep(2 * time.Second)
	}

	// Test relative duration format.
	createForwards(3)

	// Use duration format. Events are just created, so "-1d" (1 day ago)
	// will not delete them.
	//
	//nolint:ll
	delResp := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeDuration{
				DeleteBeforeDuration: "-1d",
			},
		},
	)

	// Should delete nothing since events are recent.
	require.Equal(
		ht, uint64(0), delResp.EventsDeleted,
		"recent events should not be deleted with -1d duration",
	)

	// Test absolute timestamp format. Query current events and use a
	// timestamp 2 seconds in the past. Poll until the async flush has
	// persisted all 3 events.
	var fwdHistory2 *lnrpc.ForwardingHistoryResponse
	err2 := wait.NoError(func() error {
		fwdHistory2 = bob.RPC.ForwardingHistory(nil)
		if len(fwdHistory2.ForwardingEvents) != 3 {
			return fmt.Errorf("expected 3 forwarding events, "+
				"got %d", len(fwdHistory2.ForwardingEvents))
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht, err2, "timed out waiting for forwarding events")

	//nolint:ll
	delResp2 := bob.RPC.DeleteForwardingHistory(
		&routerrpc.DeleteForwardingHistoryRequest{
			TimeSpec: &routerrpc.DeleteForwardingHistoryRequest_DeleteBeforeTime{
				DeleteBeforeTime: uint64(
					time.Now().Add(-2 * time.Second).Unix(),
				),
			},
		},
	)

	require.Equal(
		ht, uint64(3), delResp2.EventsDeleted,
		"absolute timestamp should delete all events",
	)
}
