package itest

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
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
	aliceBobChannel := ht.AssertChannelExists(alice, chanPointAliceBob)
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopChanId: aliceBobChannel.ChanId,
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

// testTrackOnion exercises the SwitchRPC server's TrackOnion endpoint,
// confirming that we can receive the result of an onion dispatch and decrypt
// the error result. We also verify that the error received from the dispatched
// onion is the same whether error is processed by the server or the rpc client.
func testTrackOnion(ht *lntest.HarnessTest) {
	// Create a four-node context consisting of Alice, Bob and two new
	// nodes: Carol and Dave. This will provide a 4 node, 3 channel topology.
	// Alice will make a  channel with Bob, and Bob with Carol, and Carol
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

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPointAlice)

	// We'll create Dave and establish a channel between Bob and Carol.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)
	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(bob, chanPointBob)

	// Next, we'll create Carol and establish a channel to from her to Dave.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(carol, chanPointCarol)

	// Make sure Alice knows the channel between Bob and Carol.
	ht.AssertChannelInGraph(alice, chanPointBob)
	ht.AssertChannelInGraph(alice, chanPointCarol)

	const paymentAmt = 10000

	// Query for routes to pay from Alice to Dave.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmt,
	}
	routes := alice.RPC.QueryRoutes(routesReq)
	route := routes.Routes[0]

	finalHop := route.Hops[len(route.Hops)-1]
	finalHop.MppRecord = &lnrpc.MPPRecord{
		PaymentAddr:  ht.Random32Bytes(),
		TotalAmtMsat: int64(lnwire.NewMSatFromSatoshis(paymentAmt)),
	}

	// Build the onion to use for our payment.
	paymentHash := ht.Random32Bytes()
	onionReq := &switchrpc.BuildOnionRequest{
		Route:       route,
		PaymentHash: paymentHash,
	}
	onionResp := alice.RPC.BuildOnion(onionReq)

	// Dispatch a payment via SendOnion.
	firstHop := bob.PubKey
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopPubkey: firstHop[:],
		Amount:         route.TotalAmtMsat,
		Timelock:       route.TotalTimeLock,
		PaymentHash:    paymentHash,
		OnionBlob:      onionResp.OnionBlob,
		AttemptId:      1,
	}

	resp := alice.RPC.SendOnion(sendReq)
	require.True(ht, resp.Success, "expected successful onion send")
	require.Empty(ht, resp.ErrorMessage, "unexpected failure to send onion")

	serverErrorStr := ""
	clientErrorStr := ""

	// Track the payment providing all necessary information to delegate
	// error decryption to the server. We expect this to fail as Dave is not
	// expecting payment.
	ctxt, _ := context.WithTimeout(context.Background(), defaultTimeout)
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
	}
	trackResp, err := alice.RPC.Switch.TrackOnion(ctxt, trackReq)
	require.Nil(ht, err, "unexpected onion tracking error")
	require.NotEmpty(ht, trackResp.ErrorMessage,
		"expected onion tracking error")

	serverErrorStr = trackResp.ErrorMessage

	// Now we'll track the same payment attempt, but we'll specify that
	// we want to handle the error decryption ourselves client side.
	trackReq = &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
	}
	trackResp, err = alice.RPC.Switch.TrackOnion(ctxt, trackReq)
	require.Nil(ht, err, "unexpected onion tracking error")
	require.NotNil(ht, trackResp.EncryptedError, "expected encrypted error")

	// Decrypt and inspect the error from the TrackOnion RPC response.
	sessionKey, _ := btcec.PrivKeyFromBytes(onionResp.SessionKey)
	var pubKeys []*btcec.PublicKey
	for _, keyBytes := range onionResp.HopPubkeys {
		pubKey, err := btcec.ParsePubKey(keyBytes)
		if err != nil {
			ht.Fatalf("Failed to parse public key: %v", err)
		}
		pubKeys = append(pubKeys, pubKey)
	}

	// Construct the circuit to create the error decryptor
	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: pubKeys,
	}
	errorDecryptor := &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Simulate an RPC client decrypting the onion error.
	encryptedError := lnwire.OpaqueReason(trackResp.EncryptedError)
	forwardingError, err := errorDecryptor.DecryptError(encryptedError)
	require.Nil(ht, err, "unable to decrypt error")

	clientErrorStr = forwardingError.Error()

	serverFwdErr, err := switchrpc.ParseForwardingError(serverErrorStr)
	require.Nil(ht, err, "expected to parse forwarding error from server")
	require.Equal(ht, serverFwdErr.Error(), clientErrorStr, "expect error "+
		"message to match whether handled by client or server")
}
