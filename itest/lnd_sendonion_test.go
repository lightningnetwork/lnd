package itest

import (
	"context"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/switchrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// testSendOnionTwice tests that the switch correctly rejects a duplicate
// payment attempt for an HTLC that is already in-flight. It sends an onion,
// then immediately sends the exact same onion with the same attempt ID. The
// test asserts that the second attempt is rejected with a DUPLICATE_HTLC
// error. It also verifies that sending again after the original HTLC has
// settled is also rejected.
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
	resp := alice.RPC.SendOnion(sendReq)
	require.True(ht, resp.Success, "expected successful onion send")
	require.Empty(ht, resp.ErrorMessage, "unexpected failure to send onion")

	// Assert that the HTLC reaches Dave.
	invoiceStream := dave.RPC.SubscribeSingleInvoice(payHash[:])
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// While the first onion is still in-flight, we'll send the same onion
	// again with the same attempt ID. This should error as our Switch will
	// detect duplicate ADDs for *in-flight* HTLCs.
	ctxt, cancel := context.WithTimeout(
		context.Background(), rpc.DefaultTimeout,
	)
	defer cancel()

	_, err := alice.RPC.Switch.SendOnion(ctxt, sendReq)
	require.Error(ht, err, "expected failure on onion send")

	// Check that we get the expected gRPC error.
	s, ok := status.FromError(err)
	require.True(ht, ok, "expected gRPC status error")
	require.Equal(ht, codes.AlreadyExists, s.Code(),
		"unexpected error code")

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
	require.Equal(ht, preimage[:], trackResp.Preimage)

	// Now that the original HTLC attempt has settled, we'll send the same
	// onion again with the same attempt ID. Confirm that this is also
	// prevented.
	ctxt, cancel = context.WithTimeout(context.Background(),
		rpc.DefaultTimeout)
	defer cancel()

	_, err = alice.RPC.Switch.SendOnion(ctxt, sendReq)
	require.Error(ht, err, "expected failure on onion send")

	// Check that we get the expected gRPC error.
	s, ok = status.FromError(err)
	require.True(ht, ok, "expected gRPC status error")
	require.Equal(ht, codes.AlreadyExists, s.Code(),
		"unexpected error code")
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

	// Use channels to collect the results from each goroutine.
	resultsChan := make(chan error,
		numConcurrentRequests)

	// Launch all requests concurrently to simulate a retry storm.
	for i := 0; i < numConcurrentRequests; i++ {
		go func() {
			defer wg.Done()
			ctxt, cancel := context.WithTimeout(
				context.Background(),
				rpc.DefaultTimeout,
			)
			defer cancel()

			_, err := alice.RPC.Switch.SendOnion(ctxt, sendReq)
			resultsChan <- err
		}()
	}

	wg.Wait()
	close(resultsChan)

	// We expect exactly one successful dispatch and the rest to be
	// rejected as duplicates.
	successCount := 0
	duplicateCount := 0

	for err := range resultsChan {
		// A nil error indicates a successful dispatch.
		if err == nil {
			successCount++
			continue
		}

		// For non-nil errors, we should receive a gRPC status error.
		s, ok := status.FromError(err)

		// If it's not a gRPC status error, it's an unexpected
		// condition.
		require.Truef(ht, ok, "unexpected error from SendOnion: %v, "+
			"code: %v", s.Err().Error(), s.Code())

		// Check if the error code indicates a duplicate acknowledgment.
		if s.Code() == codes.AlreadyExists {
			duplicateCount++
		} else {
			ht.Fatalf("unexpected error from SendOnion: %v, "+
				"code: %v", s.Err().Error(), s.Code())
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

	resp := alice.RPC.SendOnion(sendReq)
	require.True(ht, resp.Success, "expected successful onion send")
	require.Empty(ht, resp.ErrorMessage, "unexpected failure to send onion")

	serverErrorStr := ""
	clientErrorStr := ""

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
	require.NotEmpty(ht, trackResp.ErrorMessage,
		"expected onion tracking error")

	serverErrorStr = trackResp.ErrorMessage

	// Now we'll track the same payment attempt, but we'll specify that
	// we want to handle the error decryption ourselves client side.
	trackReq = &switchrpc.TrackOnionRequest{
		AttemptId:   1,
		PaymentHash: paymentHash,
	}
	trackResp = alice.RPC.TrackOnion(trackReq)
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
