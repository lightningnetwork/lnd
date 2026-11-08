package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/switchrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// onionAttempt bundles a SendOnion request with the onion it carries, so a
// test can resend it and track it.
type onionAttempt struct {
	sendReq *switchrpc.SendOnionRequest
	onion   *switchrpc.BuildOnionResponse
}

// trackReq returns a TrackOnion request for the attempt.
func (a *onionAttempt) trackReq() *switchrpc.TrackOnionRequest {
	return &switchrpc.TrackOnionRequest{
		Key:         a.sendReq.Key,
		PaymentHash: a.sendReq.PaymentHash,
		SessionKey:  a.onion.SessionKey,
		HopPubkeys:  a.onion.HopPubkeys,
	}
}

// buildOnionAttempt routes a payment of amt from sender to dest, builds an
// onion for it, and returns a SendOnion request that dispatches it over the
// given first-hop channel under the given attempt key.
func buildOnionAttempt(ht *lntest.HarnessTest, sender, dest *node.HarnessNode,
	firstHopChanID uint64, payHash, payAddr []byte, amt int64,
	key *switchrpc.AttemptKey) *onionAttempt {

	routes := sender.RPC.QueryRoutes(&lnrpc.QueryRoutesRequest{
		PubKey: dest.PubKeyStr,
		Amt:    amt,
	})
	route := routes.Routes[0]
	route.Hops[len(route.Hops)-1].MppRecord = &lnrpc.MPPRecord{
		PaymentAddr: payAddr,
		TotalAmtMsat: int64(lnwire.NewMSatFromSatoshis(
			btcutil.Amount(amt),
		)),
	}

	onion := sender.RPC.BuildOnion(&switchrpc.BuildOnionRequest{
		Route:       route,
		PaymentHash: payHash,
	})

	return &onionAttempt{
		sendReq: &switchrpc.SendOnionRequest{
			FirstHopChanId: firstHopChanID,
			Amount:         route.TotalAmtMsat,
			Timelock:       route.TotalTimeLock,
			PaymentHash:    payHash,
			OnionBlob:      onion.OnionBlob,
			Key:            key,
		},
		onion: onion,
	}
}

// requireAccepted asserts that a SendOnion call succeeded with an Accepted
// outcome carrying the given already_registered flag.
func requireAccepted(ht *lntest.HarnessTest, resp *switchrpc.SendOnionResponse,
	err error, alreadyRegistered bool) {

	ht.Helper()

	require.NoError(ht, err)
	accepted := resp.GetAccepted()
	require.NotNil(ht, accepted, "expected accepted, got %v", resp)
	require.Equal(ht, alreadyRegistered, accepted.AlreadyRegistered)
}

// requireMismatch asserts that a SendOnion call succeeded with a Mismatch
// outcome.
func requireMismatch(ht *lntest.HarnessTest, resp *switchrpc.SendOnionResponse,
	err error) {

	ht.Helper()

	require.NoError(ht, err)
	require.NotNil(ht, resp.GetMismatch(), "expected mismatch, got %v",
		resp)
}

// testSendOnionLocalCoexistence checks that the local router and a SendOnion
// client can use the same attempt ID numbers without interfering. The local
// router first pays several invoices, then a SendOnion client dispatches new
// payments under keys that encode exactly the attempt IDs the local router
// used. Each SendOnion attempt must be dispatched as new and resolve with its
// own preimage, and the local router must keep paying afterwards. The client's
// numbering never reaches the switch, which uses internal IDs from the shared
// sequencer.
func testSendOnionLocalCoexistence(ht *lntest.HarnessTest) {
	// Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(1_000_000)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		make([][]string, 3), lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, carol := nodes[0], nodes[2]
	aliceBobChan := ht.AssertChannelInGraph(alice, chanPoints[0])

	const (
		numPayments = 3
		paymentAmt  = 10_000
	)

	// sendLocal pays one of Carol's invoices through Alice's own router
	// and returns the attempt IDs it used.
	sendLocal := func(payReq string) []uint64 {
		payment := ht.SendPaymentAssertSettled(
			alice, &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)

		ids := make([]uint64, 0, len(payment.Htlcs))
		for _, htlc := range payment.Htlcs {
			ids = append(ids, htlc.AttemptId)
		}

		return ids
	}

	// The local router pays first, filling the store with records under
	// its own attempt IDs.
	payReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, numPayments)
	var localIDs []uint64
	for _, payReq := range payReqs {
		localIDs = append(localIDs, sendLocal(payReq)...)
	}
	require.NotEmpty(ht, localIDs)

	// A SendOnion client now dispatches new payments under the very same
	// attempt ID numbers. Each must be dispatched, not acknowledged as a
	// duplicate, and must resolve with its own preimage.
	_, rHashes, invoices := ht.CreatePayReqs(
		carol, paymentAmt, len(localIDs),
	)
	for i, id := range localIDs {
		attempt := buildOnionAttempt(
			ht, alice, carol, aliceBobChan.ChannelId, rHashes[i],
			invoices[i].PaymentAddr, paymentAmt, testAttemptKey(id),
		)

		resp, err := alice.RPC.SendOnion(attempt.sendReq)
		require.NoError(ht, err)
		accepted := resp.GetAccepted()
		require.NotNil(ht, accepted)
		require.False(ht, accepted.AlreadyRegistered, "attempt ID %d "+
			"reused from the local router was not dispatched", id)

		trackResp := alice.RPC.TrackOnion(attempt.trackReq())
		require.Equal(ht, invoices[i].RPreimage,
			trackResp.GetPreimage(), "attempt ID %d resolved "+
				"with another payment's result", id)

		ht.AssertInvoiceSettled(carol, invoices[i].PaymentAddr)
	}

	// The local router keeps working after the SendOnion attempts, which
	// drew their internal IDs from the same sequencer.
	payReqs, _, _ = ht.CreatePayReqs(carol, paymentAmt, numPayments)
	for _, payReq := range payReqs {
		sendLocal(payReq)
	}
}

// testSendOnionKeyMismatch checks that a key registered for one request is
// answered Mismatch for a different request, while an exact replay is
// acknowledged as already registered, and that the original attempt is
// unaffected.
func testSendOnionKeyMismatch(ht *lntest.HarnessTest) {
	// Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(100_000)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		make([][]string, 3), lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, carol := nodes[0], nodes[2]
	aliceBobChan := ht.AssertChannelInGraph(alice, chanPoints[0])

	const paymentAmt = 10_000

	// Carol holds the invoice, so the original HTLC stays in flight while
	// we resend.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoice := carol.RPC.AddHoldInvoice(&invoicesrpc.AddHoldInvoiceRequest{
		Value:      paymentAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	})

	attempt := buildOnionAttempt(
		ht, alice, carol, aliceBobChan.ChannelId, payHash[:],
		invoice.PaymentAddr, paymentAmt, testAttemptKey(1),
	)
	resp, err := alice.RPC.SendOnion(attempt.sendReq)
	requireAccepted(ht, resp, err, false)

	invoiceStream := carol.RPC.SubscribeSingleInvoice(payHash[:])
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// A rebuilt onion for the same route has a fresh session key, so its
	// bytes differ. Sent under the same key it must be answered Mismatch.
	rebuilt := buildOnionAttempt(
		ht, alice, carol, aliceBobChan.ChannelId, payHash[:],
		invoice.PaymentAddr, paymentAmt, testAttemptKey(1),
	)
	require.NotEqual(ht, attempt.sendReq.OnionBlob,
		rebuilt.sendReq.OnionBlob)
	resp, err = alice.RPC.SendOnion(rebuilt.sendReq)
	requireMismatch(ht, resp, err)

	// A field outside the onion changed under the same key is a mismatch
	// too.
	changedAmt, ok := proto.Clone(
		attempt.sendReq,
	).(*switchrpc.SendOnionRequest)
	require.True(ht, ok)
	changedAmt.Amount++
	resp, err = alice.RPC.SendOnion(changedAmt)
	requireMismatch(ht, resp, err)

	// A changed payment hash under the same key is a mismatch as well,
	// rather than a new attempt.
	changedHash, ok := proto.Clone(
		attempt.sendReq,
	).(*switchrpc.SendOnionRequest)
	require.True(ht, ok)
	changedHash.PaymentHash = ht.Random32Bytes()
	resp, err = alice.RPC.SendOnion(changedHash)
	requireMismatch(ht, resp, err)

	// An exact replay is still acknowledged as already registered.
	resp, err = alice.RPC.SendOnion(attempt.sendReq)
	requireAccepted(ht, resp, err, true)

	// The original attempt is unaffected: it settles, tracks with its own
	// preimage, and Carol was paid exactly once.
	carol.RPC.SettleInvoice(preimage[:])
	ht.AssertInvoiceSettled(carol, invoice.PaymentAddr)

	trackResp := alice.RPC.TrackOnion(attempt.trackReq())
	require.Equal(ht, preimage[:], trackResp.GetPreimage())

	settled := carol.RPC.LookupInvoice(payHash[:])
	require.EqualValues(ht, paymentAmt, settled.AmtPaidSat)
	require.Len(ht, settled.Htlcs, 1)
}

// testSendOnionRestart checks that the mapping from a client's key to the
// gateway's internal attempt survives a restart of the gateway while the HTLC
// is in flight, that the settled result survives a second restart, and that
// the attempt can then be tracked and deleted by key.
func testSendOnionRestart(ht *lntest.HarnessTest) {
	// Alice -> Bob -> Carol.
	const chanAmt = btcutil.Amount(100_000)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		make([][]string, 3), lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	aliceBobChan := ht.AssertChannelInGraph(alice, chanPoints[0])

	const paymentAmt = 10_000

	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()
	invoice := carol.RPC.AddHoldInvoice(&invoicesrpc.AddHoldInvoiceRequest{
		Value:      paymentAmt,
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	})

	attempt := buildOnionAttempt(
		ht, alice, carol, aliceBobChan.ChannelId, payHash[:],
		invoice.PaymentAddr, paymentAmt, testAttemptKey(7),
	)
	resp, err := alice.RPC.SendOnion(attempt.sendReq)
	requireAccepted(ht, resp, err, false)

	invoiceStream := carol.RPC.SubscribeSingleInvoice(payHash[:])
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// Restart the gateway with the HTLC in flight.
	ht.RestartNode(alice)
	ht.EnsureConnected(alice, bob)
	ht.AssertChannelActive(alice, chanPoints[0])

	// The key still maps to the in-flight attempt: an exact replay is
	// already registered, not a new dispatch.
	resp, err = alice.RPC.SendOnion(attempt.sendReq)
	requireAccepted(ht, resp, err, true)

	carol.RPC.SettleInvoice(preimage[:])
	ht.AssertInvoiceSettled(carol, invoice.PaymentAddr)

	trackResp := alice.RPC.TrackOnion(attempt.trackReq())
	require.Equal(ht, preimage[:], trackResp.GetPreimage())

	// Restart the gateway again with the attempt settled but not yet
	// deleted. The router's startup cleanup must keep the indexed result,
	// so the client can still read it.
	ht.RestartNode(alice)

	trackResp = alice.RPC.TrackOnion(attempt.trackReq())
	require.Equal(ht, preimage[:], trackResp.GetPreimage())

	// Delete the attempt by its key. The record and the key go together,
	// so tracking afterwards reports the key as not found.
	deleteReq := &switchrpc.DeleteAttemptsRequest{
		Keys: []*switchrpc.AttemptKey{attempt.sendReq.Key},
	}
	deleteResp := alice.RPC.DeleteAttempts(deleteReq)
	require.Len(ht, deleteResp.Results, 1)
	require.Equal(ht, switchrpc.AttemptDeletionStatus_DELETION_OK,
		deleteResp.Results[0].Status)
	require.Equal(ht, attempt.sendReq.Key.Id,
		deleteResp.Results[0].Key.GetId())

	err = wait.NoError(func() error {
		ctx, cancel := context.WithTimeout(
			ht.Context(), wait.DefaultTimeout,
		)
		defer cancel()

		_, err := alice.RPC.Switch.TrackOnion(ctx, attempt.trackReq())
		if status.Code(err) == codes.NotFound {
			return nil
		}

		return fmt.Errorf("expected NotFound, got %w", err)
	}, wait.DefaultTimeout)
	require.NoError(ht, err, "expected NotFound after delete")
}
