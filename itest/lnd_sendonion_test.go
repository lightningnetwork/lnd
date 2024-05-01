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

func testSendOnion(ht *lntest.HarnessTest) {
	// Create a four-node context consisting of Alice, Bob and two new
	// nodes: Carol and Dave. This provides a 4 node, 3 channel topology.
	// Alice will make a channel with Bob, and Bob with Carol, and Carol
	// with Dave such that we arrive at the network topology:
	//     Alice -> Bob -> Carol -> Dave
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	// Connect nodes to ensure propagation of channels.
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(bob, carol)
	ht.EnsureConnected(carol, dave)

	const chanAmt = btcutil.Amount(100000)

	// We'll open a channel between Alice and Bob with Alice's liquidity
	// drained (eg: all funds on Bob's side of the channel) to exercise the
	// RPC logic which selects the appropriate channel link.
	chanPointBobAlice := ht.OpenChannel(
		bob, alice, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPointBobAlice)

	// Now, open a channel with 100k satoshis between Alice and Bob with
	// Alice being the sole funder of the channel. This will provide Alice
	// with outbound liquidity she can use to complete payments.
	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPointAliceBob)

	// We'll create Dave and establish a channel between Bob and Carol.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)
	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(bob, chanPointBob)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(carol, chanPointCarol)

	// Make sure Alice knows the channel between Bob and Carol.
	ht.AssertChannelInGraph(alice, chanPointBob)
	ht.AssertChannelInGraph(alice, chanPointCarol)

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
	firstHop := bob.PubKey
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopPubkey: firstHop[:],
		Amount:         route.TotalAmtMsat,
		Timelock:       route.TotalTimeLock,
		PaymentHash:    paymentHash,
		OnionBlob:      onionResp.OnionBlob,
		AttemptId:      1,
	}

	// NOTE(calvin): We may want our wrapper RPC client to allow errors
	// through so that we can make some assertions about them in various
	// scenarios.
	// resp, err := alice.RPC.SendOnion(onionReq)
	// require.NoError(ht, err, "unable to send payment via onion")
	resp := alice.RPC.SendOnion(sendReq)
	require.True(ht, resp.Success, "expected successful onion send")
	require.Empty(ht, resp.ErrorMessage, "unexpected failure to send onion")

	// Finally, check that the Alice's payment is correctly marked
	// succeeded.
	//
	// NOTE(calvin): We are not able to lookup the payment using normal
	// means currently. I think this is because we deliver the onion
	// directly to the switch without persisting any record via Control
	// Tower as is done by the ChannelRouter for other payments!
	// ht.AssertPaymentStatus(alice, preimage, lnrpc.Payment_SUCCEEDED)
	// ht.AssertAmountPaid()

	// Query for the result of the payment via onion!
	//
	// NOTE(calvin): This currently blocks until payment success/failure.
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
		// SharedSecrets: [][]byte,
	}
	trackResp := alice.RPC.TrackOnion(trackReq)
	require.True(ht, trackResp.Success, "expected successful onion track")
	require.Equal(ht, invoices[0].RPreimage, trackResp.Preimage)

	// The invoice should show as settled for Dave.
	ht.AssertInvoiceSettled(dave, invoices[0].PaymentAddr)
}
