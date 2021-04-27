package itest

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/amp"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testSendPaymentAMP tests that we can send an AMP payment to a specified
// destination using SendPaymentV2.
func testSendPaymentAMP(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	ctx := newMppTestContext(t, net)
	defer ctx.shutdownNodes()

	const paymentAmt = btcutil.Amount(300000)

	// Set up a network with three different paths Alice <-> Bob. Channel
	// capacities are set such that the payment can only succeed if (at
	// least) three paths are used.
	//
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	ctx.openChannel(ctx.carol, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.carol, 235000)
	ctx.openChannel(ctx.dave, ctx.bob, 135000)
	ctx.openChannel(ctx.alice, ctx.dave, 135000)
	ctx.openChannel(ctx.eve, ctx.bob, 135000)
	ctx.openChannel(ctx.carol, ctx.eve, 135000)

	defer ctx.closeChannels()

	ctx.waitForChannels()

	// Increase Dave's fee to make the test deterministic. Otherwise it
	// would be unpredictable whether pathfinding would go through Charlie
	// or Dave for the first shard.
	_, err := ctx.dave.UpdateChannelPolicy(
		context.Background(),
		&lnrpc.PolicyUpdateRequest{
			Scope:         &lnrpc.PolicyUpdateRequest_Global{Global: true},
			BaseFeeMsat:   500000,
			FeeRate:       0.001,
			TimeLockDelta: 40,
		},
	)
	if err != nil {
		t.Fatalf("dave policy update: %v", err)
	}

	ctxt, _ := context.WithTimeout(context.Background(), 4*defaultTimeout)
	payment := sendAndAssertSuccess(
		ctxt, t, net.Alice,
		&routerrpc.SendPaymentRequest{
			Dest:           net.Bob.PubKey[:],
			Amt:            int64(paymentAmt),
			FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
			MaxParts:       10,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
			Amp:            true,
		},
	)

	// Check that Alice split the payment in at least three shards. Because
	// the hand-off of the htlc to the link is asynchronous (via a mailbox),
	// there is some non-determinism in the process. Depending on whether
	// the new pathfinding round is started before or after the htlc is
	// locked into the channel, different sharding may occur. Therefore we
	// can only check if the number of shards isn't below the theoretical
	// minimum.
	succeeded := 0
	for _, htlc := range payment.Htlcs {
		if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
			succeeded++
		}
	}

	const minExpectedShards = 3
	if succeeded < minExpectedShards {
		t.Fatalf("expected at least %v shards, but got %v",
			minExpectedShards, succeeded)
	}

	// Fetch Bob's invoices.
	invoiceResp, err := net.Bob.ListInvoices(
		ctxb, &lnrpc.ListInvoiceRequest{},
	)
	require.NoError(t.t, err)

	// There should only be one invoice.
	require.Equal(t.t, 1, len(invoiceResp.Invoices))
	rpcInvoice := invoiceResp.Invoices[0]

	// Assert that the invoice is settled for the total payment amount and
	// has the correct payment address.
	require.True(t.t, rpcInvoice.Settled)
	require.Equal(t.t, lnrpc.Invoice_SETTLED, rpcInvoice.State)
	require.Equal(t.t, int64(paymentAmt), rpcInvoice.AmtPaidSat)
	require.Equal(t.t, int64(paymentAmt*1000), rpcInvoice.AmtPaidMsat)

	// Finally, assert that the same set id is recorded for each htlc, and
	// that the preimage hash pair is valid.
	var setID []byte
	require.Equal(t.t, succeeded, len(rpcInvoice.Htlcs))
	for _, htlc := range rpcInvoice.Htlcs {
		require.NotNil(t.t, htlc.Amp)
		if setID == nil {
			setID = make([]byte, 32)
			copy(setID, htlc.Amp.SetId)
		}
		require.Equal(t.t, setID, htlc.Amp.SetId)

		// Parse the child hash and child preimage, and assert they are
		// well-formed.
		childHash, err := lntypes.MakeHash(htlc.Amp.Hash)
		require.NoError(t.t, err)
		childPreimage, err := lntypes.MakePreimage(htlc.Amp.Preimage)
		require.NoError(t.t, err)

		// Assert that the preimage actually matches the hashes.
		validPreimage := childPreimage.Matches(childHash)
		require.True(t.t, validPreimage)
	}
}

func testSendToRouteAMP(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	ctx := newMppTestContext(t, net)
	defer ctx.shutdownNodes()

	const (
		paymentAmt = btcutil.Amount(300000)
		numShards  = 3
		shardAmt   = paymentAmt / numShards
		chanAmt    = shardAmt * 3 / 2
	)

	// Set up a network with three different paths Alice <-> Bob.
	//              _ Eve _
	//             /       \
	// Alice -- Carol ---- Bob
	//      \              /
	//       \__ Dave ____/
	//
	ctx.openChannel(ctx.carol, ctx.bob, chanAmt)
	ctx.openChannel(ctx.dave, ctx.bob, chanAmt)
	ctx.openChannel(ctx.alice, ctx.dave, chanAmt)
	ctx.openChannel(ctx.eve, ctx.bob, chanAmt)
	ctx.openChannel(ctx.carol, ctx.eve, chanAmt)

	// Since the channel Alice-> Carol will have to carry two
	// shards, we make it larger.
	ctx.openChannel(ctx.alice, ctx.carol, chanAmt+shardAmt)

	defer ctx.closeChannels()

	ctx.waitForChannels()

	// We'll send shards along three routes from Alice.
	sendRoutes := [numShards][]*lntest.HarnessNode{
		{ctx.carol, ctx.bob},
		{ctx.dave, ctx.bob},
		{ctx.carol, ctx.eve, ctx.bob},
	}

	payAddr := make([]byte, 32)
	_, err := rand.Read(payAddr)
	require.NoError(t.t, err)

	setID := make([]byte, 32)
	_, err = rand.Read(setID)
	require.NoError(t.t, err)

	var sharer amp.Sharer
	sharer, err = amp.NewSeedSharer()
	require.NoError(t.t, err)

	childPreimages := make(map[lntypes.Preimage]uint32)
	responses := make(chan *lnrpc.HTLCAttempt, len(sendRoutes))
	for i, hops := range sendRoutes {
		// Build a route for the specified hops.
		r, err := ctx.buildRoute(ctxb, shardAmt, net.Alice, hops)
		if err != nil {
			t.Fatalf("unable to build route: %v", err)
		}

		// Set the MPP records to indicate this is a payment shard.
		hop := r.Hops[len(r.Hops)-1]
		hop.TlvPayload = true
		hop.MppRecord = &lnrpc.MPPRecord{
			PaymentAddr:  payAddr,
			TotalAmtMsat: int64(paymentAmt * 1000),
		}

		var child *amp.Child
		if i < len(sendRoutes)-1 {
			var left amp.Sharer
			left, sharer, err = sharer.Split()
			require.NoError(t.t, err)

			child = left.Child(uint32(i))
		} else {
			child = sharer.Child(uint32(i))
		}
		childPreimages[child.Preimage] = child.Index

		hop.AmpRecord = &lnrpc.AMPRecord{
			RootShare:  child.Share[:],
			SetId:      setID,
			ChildIndex: child.Index,
		}

		// Send the shard.
		sendReq := &routerrpc.SendToRouteRequest{
			PaymentHash: child.Hash[:],
			Route:       r,
		}

		// We'll send all shards in their own goroutine, since SendToRoute will
		// block as long as the payment is in flight.
		go func() {
			ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
			resp, err := net.Alice.RouterClient.SendToRouteV2(ctxt, sendReq)
			if err != nil {
				t.Fatalf("unable to send payment: %v", err)
			}

			responses <- resp
		}()
	}

	// Assert that all of the child preimages are unique.
	require.Equal(t.t, len(sendRoutes), len(childPreimages))

	// Make a copy of the childPreimages map for validating the resulting
	// invoice.
	childPreimagesCopy := make(map[lntypes.Preimage]uint32)
	for preimage, childIndex := range childPreimages {
		childPreimagesCopy[preimage] = childIndex
	}

	// Wait for all responses to be back, and check that they all
	// succeeded.
	for range sendRoutes {
		var resp *lnrpc.HTLCAttempt
		select {
		case resp = <-responses:
		case <-time.After(defaultTimeout):
			t.Fatalf("response not received")
		}

		if resp.Failure != nil {
			t.Fatalf("received payment failure : %v", resp.Failure)
		}

		preimage, err := lntypes.MakePreimage(resp.Preimage)
		require.NoError(t.t, err)

		// Assert that the response includes one of our child preimages.
		_, ok := childPreimages[preimage]
		require.True(t.t, ok)

		// Remove this preimage from out set so that we ensure all
		// responses have a unique child preimage.
		delete(childPreimages, preimage)
	}
	childPreimages = childPreimagesCopy

	// Fetch Bob's invoices.
	invoiceResp, err := net.Bob.ListInvoices(
		ctxb, &lnrpc.ListInvoiceRequest{},
	)
	require.NoError(t.t, err)

	// There should only be one invoice.
	require.Equal(t.t, 1, len(invoiceResp.Invoices))
	rpcInvoice := invoiceResp.Invoices[0]

	// Assert that the invoice is settled for the total payment amount and
	// has the correct payment address.
	require.True(t.t, rpcInvoice.Settled)
	require.Equal(t.t, lnrpc.Invoice_SETTLED, rpcInvoice.State)
	require.Equal(t.t, int64(paymentAmt), rpcInvoice.AmtPaidSat)
	require.Equal(t.t, int64(paymentAmt*1000), rpcInvoice.AmtPaidMsat)
	require.Equal(t.t, payAddr, rpcInvoice.PaymentAddr)

	// Finally, assert that the proper set id is recorded for each htlc, and
	// that the preimage hash pair is valid.
	require.Equal(t.t, numShards, len(rpcInvoice.Htlcs))
	for _, htlc := range rpcInvoice.Htlcs {
		require.NotNil(t.t, htlc.Amp)
		require.Equal(t.t, setID, htlc.Amp.SetId)

		// Parse the child hash and child preimage, and assert they are
		// well-formed.
		childHash, err := lntypes.MakeHash(htlc.Amp.Hash)
		require.NoError(t.t, err)
		childPreimage, err := lntypes.MakePreimage(htlc.Amp.Preimage)
		require.NoError(t.t, err)

		// Assert that the preimage actually matches the hashes.
		validPreimage := childPreimage.Matches(childHash)
		require.True(t.t, validPreimage)

		// Assert that the HTLC includes one of our child preimages.
		childIndex, ok := childPreimages[childPreimage]
		require.True(t.t, ok)

		// Assert that the correct child index is reflected.
		require.Equal(t.t, childIndex, htlc.Amp.ChildIndex)

		// Remove this preimage from our set so that we ensure all HTLCs
		// have a unique child preimage.
		delete(childPreimages, childPreimage)
	}
}
