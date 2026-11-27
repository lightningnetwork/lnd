package itest

import (
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
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

	resp, err := alice.RPC.SendOnion(sendReq)
	require.NoError(ht, err, "expected successful onion send")
	require.NotNil(ht, resp.GetAccepted(), "expected the switch to "+
		"accept the attempt")

	// Query for the result of the payment via onion and confirm that it
	// succeeded.
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
	}
	trackResp := alice.RPC.TrackOnion(trackReq)
	require.Equal(ht, invoices[0].RPreimage, trackResp.GetPreimage())

	// The invoice should show as settled for Dave.
	ht.AssertInvoiceSettled(dave, invoices[0].PaymentAddr)
}

// testSendOnionTwice tests that the switch never dispatches a second HTLC for
// an attempt it already holds. It sends an onion, then immediately sends the
// exact same onion with the same attempt ID. The test asserts that the second
// call is answered Accepted with already_registered set, both while the first
// HTLC is in flight and after it has settled.
func testSendOnionTwice(ht *lntest.HarnessTest) {
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

	const paymentAmt = 10000

	// Create a preimage, that will be held by Dave.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()

	// Add a hodl invoice at Dave's end.
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      int64(paymentAmt),
		CltvExpiry: finalCltvDelta,
		Hash:       payHash[:],
	}
	invoice := dave.RPC.AddHoldInvoice(invoiceReq)

	// Query for routes to Dave.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmt,
	}
	routes := alice.RPC.QueryRoutes(routesReq)
	route := routes.Routes[0]
	finalHop := route.Hops[len(route.Hops)-1]
	finalHop.MppRecord = &lnrpc.MPPRecord{
		PaymentAddr:  invoice.PaymentAddr,
		TotalAmtMsat: int64(lnwire.NewMSatFromSatoshis(paymentAmt)),
	}

	// Build the onion.
	onionReq := &switchrpc.BuildOnionRequest{
		Route:       route,
		PaymentHash: payHash[:],
	}
	onionResp := alice.RPC.BuildOnion(onionReq)

	// Send the onion for the first time.
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopPubkey: bob.PubKey[:],
		Amount:         route.TotalAmtMsat,
		Timelock:       route.TotalTimeLock,
		PaymentHash:    payHash[:],
		OnionBlob:      onionResp.OnionBlob,
		AttemptId:      1,
	}
	resp, err := alice.RPC.SendOnion(sendReq)
	require.NoError(ht, err, "expected successful onion send")
	require.NotNil(ht, resp.GetAccepted(), "expected the switch to "+
		"accept the attempt")

	// Assert that the HTLC reaches Dave.
	invoiceStream := dave.RPC.SubscribeSingleInvoice(payHash[:])
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// While the first onion is still in-flight, we'll send the same onion
	// again with the same attempt ID. The switch recognises the attempt and
	// acknowledges it without dispatching a second HTLC.
	resp, err = alice.RPC.SendOnion(sendReq)
	require.NoError(ht, err, "a duplicate dispatch is an acknowledgement, "+
		"not an error")

	accepted := resp.GetAccepted()
	require.NotNil(ht, accepted, "expected an acknowledgement")
	require.True(ht, accepted.AlreadyRegistered,
		"the switch already held this attempt and must say so")

	// Dave settles the invoice.
	dave.RPC.SettleInvoice(preimage[:])

	// Ensure Dave's invoice is settled.
	ht.AssertInvoiceSettled(dave, invoice.PaymentAddr)

	// Track the payment and verify success.
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: payHash[:],
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
	}
	trackResp := alice.RPC.TrackOnion(trackReq)
	require.Equal(ht, preimage[:], trackResp.GetPreimage())

	// Now that the original HTLC attempt has settled, we'll send the same
	// onion again with the same attempt ID. Confirm that it is again
	// acknowledged rather than dispatched.
	resp, err = alice.RPC.SendOnion(sendReq)
	require.NoError(ht, err, "a duplicate dispatch is an acknowledgement, "+
		"not an error")

	accepted = resp.GetAccepted()
	require.NotNil(ht, accepted, "expected an acknowledgement")
	require.True(ht, accepted.AlreadyRegistered,
		"the switch already held this attempt and must say so")

	// Now that we've confirmed that duplicate sends are acknowledged for
	// settled attempts, delete the attempt record from the store.
	deleteReq := &switchrpc.DeleteAttemptsRequest{
		AttemptIds: []uint64{1},
	}
	deleteResp := alice.RPC.DeleteAttempts(deleteReq)
	require.Len(ht, deleteResp.Results, 1)
	require.Equal(ht,
		switchrpc.AttemptDeletionStatus_DELETION_OK,
		deleteResp.Results[0].Status,
	)

	// Deleting the same attempt again should return NOT_FOUND,
	// confirming the record was fully removed.
	deleteResp = alice.RPC.DeleteAttempts(deleteReq)
	require.Len(ht, deleteResp.Results, 1)
	require.Equal(ht,
		switchrpc.AttemptDeletionStatus_DELETION_NOT_FOUND,
		deleteResp.Results[0].Status,
	)
}

// testSendOnionConcurrency simulates a client that crashes and attempts to
// retry a payment with the same attempt ID concurrently. This test provides a
// strong guarantee that the SendOnion RPC is idempotent and correctly prevents
// duplicate payment attempts from succeeding.
func testSendOnionConcurrency(ht *lntest.HarnessTest) {
	// Create a two-node context consisting of Alice and Bob.
	const chanAmt = btcutil.Amount(100000)
	const numNodes = 2
	nodeCfgs := make([][]string, numNodes)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		nodeCfgs, lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob := nodes[0], nodes[1]

	// Make sure Alice knows about the channel.
	aliceBobChan := ht.AssertChannelInGraph(alice, chanPoints[0])

	const paymentAmt = 10000

	// Request an invoice from Bob so he is expecting payment.
	_, rHashes, invoices := ht.CreatePayReqs(bob, paymentAmt, 1)
	paymentHash := rHashes[0]

	// Query for a route to pay from Alice to Bob.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: bob.PubKeyStr,
		Amt:    paymentAmt,
	}
	routes := alice.RPC.QueryRoutes(routesReq)
	route := routes.Routes[0]
	finalHop := route.Hops[len(route.Hops)-1]
	finalHop.MppRecord = &lnrpc.MPPRecord{
		PaymentAddr:  invoices[0].PaymentAddr,
		TotalAmtMsat: int64(lnwire.NewMSatFromSatoshis(paymentAmt)),
	}

	// Construct the onion for the route.
	onionReq := &switchrpc.BuildOnionRequest{
		Route:       route,
		PaymentHash: paymentHash,
	}
	onionResp := alice.RPC.BuildOnion(onionReq)

	// Create the SendOnion request that all goroutines will use.
	// The AttemptId MUST be the same for all calls.
	sendReq := &switchrpc.SendOnionRequest{
		FirstHopChanId: aliceBobChan.ChannelId,
		Amount:         route.TotalAmtMsat,
		Timelock:       route.TotalTimeLock,
		PaymentHash:    paymentHash,
		OnionBlob:      onionResp.OnionBlob,
		AttemptId:      42,
	}

	const numConcurrentRequests = 50
	var wg sync.WaitGroup
	wg.Add(numConcurrentRequests)

	// Collect each response and error, and assert on them below, since
	// require must run on the test goroutine.
	type sendResult struct {
		resp *switchrpc.SendOnionResponse
		err  error
	}
	resultsChan := make(chan sendResult, numConcurrentRequests)

	// Launch all requests concurrently to simulate a retry storm.
	for range numConcurrentRequests {
		go func() {
			defer wg.Done()

			resp, err := alice.RPC.SendOnion(sendReq)
			resultsChan <- sendResult{resp: resp, err: err}
		}()
	}

	wg.Wait()
	close(resultsChan)

	// We expect exactly one dispatch, with the rest acknowledged as
	// duplicates.
	successCount := 0
	duplicateCount := 0

	for res := range resultsChan {
		require.NoError(ht, res.err, "dispatch must not error")
		accepted := res.resp.GetAccepted()

		// Every concurrent dispatch of the same attempt id must be
		// accepted: exactly one does the work and the rest are
		// acknowledged as already held. None may be rejected or
		// ambiguous, because the switch always knows the answer here.
		require.NotNilf(ht, accepted, "concurrent dispatch was not "+
			"accepted: %v", res.resp.Outcome)

		if accepted.AlreadyRegistered {
			duplicateCount++
		} else {
			successCount++
		}
	}

	// Confirm that only a single dispatch succeeds.
	require.Equal(ht, 1, successCount, "expected exactly one success")
	require.Equal(ht, numConcurrentRequests-1, duplicateCount,
		"expected all other attempts to be duplicates")

	// The invoice should eventually show as settled for Bob.
	ht.AssertInvoiceSettled(bob, invoices[0].PaymentAddr)
}

// testTrackOnion exercises the SwitchRPC server's TrackOnion endpoint,
// confirming that we can receive the result of an onion dispatch and decrypt
// the error result. We also verify that the error received from the dispatched
// onion is the same whether error is processed by the server or the rpc client.
func testTrackOnion(ht *lntest.HarnessTest) {
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

	resp, err := alice.RPC.SendOnion(sendReq)
	require.NoError(ht, err, "expected successful onion send")
	require.NotNil(ht, resp.GetAccepted(), "expected the switch to "+
		"accept the attempt")

	// Track the payment providing all necessary information to delegate
	// error decryption to the server. We expect this to fail as Dave is not
	// expecting payment.
	trackReq := &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
		SessionKey:  onionResp.SessionKey,
		HopPubkeys:  onionResp.HopPubkeys,
	}
	trackResp := alice.RPC.TrackOnion(trackReq)
	serverFailure := trackResp.GetFailureDetails()
	require.NotNil(ht, serverFailure, "expected onion tracking error")

	serverFwdFailure := serverFailure.GetForwardingFailure()
	require.NotNil(ht, serverFwdFailure, "expected forwarding failure")

	// Now we'll track the same payment attempt, but we'll specify that
	// we want to handle the error decryption ourselves client side.
	trackReq = &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
	}
	trackResp = alice.RPC.TrackOnion(trackReq)
	clientFailure := trackResp.GetFailureDetails()
	require.NotNil(ht, clientFailure, "expected client tracking error")

	encryptedErrorBytes := clientFailure.GetEncryptedErrorData()
	require.NotNil(ht, encryptedErrorBytes, "expected encrypted error")

	// Decrypt and inspect the error from the TrackOnion RPC response.
	sessionKey, _ := btcec.PrivKeyFromBytes(onionResp.SessionKey)
	var pubKeys []*btcec.PublicKey
	for _, keyBytes := range onionResp.HopPubkeys {
		pubKey, err := btcec.ParsePubKey(keyBytes)
		require.NoError(ht, err, "Failed to parse public key")
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
	encryptedError := lnwire.OpaqueReason(encryptedErrorBytes)
	clientFwdErr, err := errorDecryptor.DecryptError(encryptedError)
	require.NoError(ht, err, "unable to decrypt error")

	// Finally, assert that the structured forwarding failure is the same
	// whether it was decrypted on the server or on the client.
	serverFwdErr, err := switchrpc.UnmarshallForwardingError(
		serverFwdFailure,
	)
	require.NoError(ht, err, "unable to decode server forwarding failure")

	require.Equal(ht, serverFwdFailure.FailureSourceIndex,
		uint32(clientFwdErr.FailureSourceIdx), "source index mismatch")
	require.Equal(ht, serverFwdErr.WireMessage(),
		clientFwdErr.WireMessage(), "wire message mismatch")
}
