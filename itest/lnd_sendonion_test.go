package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/switchrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testSendOnion tests the basic success case for the SendOnion RPC. It
// constructs a multi-hop route from Alice -> Bob -> Carol -> Dave, builds an
// onion packet for this route, and then asserts that Alice can successfully
// dispatch the payment using the SendOnion RPC. The test concludes by using
// TrackOnion to wait for and verify the payment's success preimage.
func testSendOnion(ht *lntest.HarnessTest) {
	// Create a four-node context consisting of Alice, Bob, Carol, and
	// Dave with the following topology:
	//     Alice -> Bob -> Carol -> Dave
	const chanAmt = btcutil.Amount(100000)
	const numNodes = 4
	nodeCfgs := make([][]string, numNodes)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		nodeCfgs, lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob, carol, dave := nodes[0], nodes[1], nodes[2], nodes[3]
	defer ht.CloseChannel(alice, chanPoints[0])
	defer ht.CloseChannel(bob, chanPoints[1])
	defer ht.CloseChannel(carol, chanPoints[2])

	// Make sure Alice knows about all channels.
	aliceBobChan := ht.AssertChannelInGraph(alice, chanPoints[0])
	ht.AssertChannelInGraph(alice, chanPoints[1])
	ht.AssertChannelInGraph(alice, chanPoints[2])

	const (
		numPayments = 1
		paymentAmt  = 10000
	)

	// Request an invoice from Dave so he is expecting payment.
	_, rHashes, invoices := ht.CreatePayReqs(dave, paymentAmt, numPayments)
	var preimage lntypes.Preimage
	copy(preimage[:], invoices[0].RPreimage)

	// Query for routes to pay from Alice to Dave.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmt,
	}
	routes := alice.RPC.QueryRoutes(routesReq)
	route := routes.Routes[0]
	finalHop := route.Hops[len(route.Hops)-1]
	finalHop.MppRecord = &lnrpc.MPPRecord{
		PaymentAddr:  invoices[0].PaymentAddr,
		TotalAmtMsat: int64(lnwire.NewMSatFromSatoshis(paymentAmt)),
	}

	// Construct an onion for the route from Alice to Dave.
	paymentHash := rHashes[0]
	onionReq := &switchrpc.BuildOnionRequest{
		Route:       route,
		PaymentHash: paymentHash,
	}
	onionResp := alice.RPC.BuildOnion(onionReq)

	// Dispatch a payment via the SendOnion RPC.
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopChanId: aliceBobChan.ChannelId,
		Amount:         route.TotalAmtMsat,
		Timelock:       route.TotalTimeLock,
		PaymentHash:    paymentHash,
		OnionBlob:      onionResp.OnionBlob,
		AttemptId:      1,
	}

	resp := alice.RPC.SendOnion(sendReq)
	require.True(ht, resp.Success, "expected successful onion send")
	require.Empty(ht, resp.ErrorMessage, "unexpected failure to send onion")

	// Query for the result of the payment via onion and confirm that it
	// succeeded.
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
	}
	trackResp := alice.RPC.TrackOnion(trackReq)
	require.Equal(ht, invoices[0].RPreimage, trackResp.Preimage)

	// The invoice should show as settled for Dave.
	ht.AssertInvoiceSettled(dave, invoices[0].PaymentAddr)
}
