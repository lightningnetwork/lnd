package itest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testSendDirectPayment creates a topology Alice->Bob and then tests that
// Alice can send a direct payment to Bob. This test modifies the fee estimator
// to return floor fee rate(1 sat/vb).
func testSendDirectPayment(ht *lntest.HarnessTest) {
	// Grab Alice and Bob's nodes for convenience.
	alice, bob := ht.Alice, ht.Bob

	// Create a list of commitment types we want to test.
	commitTyes := []lnrpc.CommitmentType{
		lnrpc.CommitmentType_LEGACY,
		lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	}

	// testSendPayment opens a channel between Alice and Bob using the
	// specified params. It then sends a payment from Alice to Bob and
	// asserts it being successful.
	testSendPayment := func(ht *lntest.HarnessTest,
		params lntest.OpenChannelParams) {

		// Check that there are no payments before test.
		chanPoint := ht.OpenChannel(alice, bob, params)

		// Now that the channel is open, create an invoice for Bob
		// which expects a payment of 1000 satoshis from Alice paid via
		// a particular preimage.
		const paymentAmt = 1000
		preimage := ht.Random32Bytes()
		invoice := &lnrpc.Invoice{
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		invoiceResp := bob.RPC.AddInvoice(invoice)

		// With the invoice for Bob added, send a payment towards Alice
		// paying to the above generated invoice.
		payReqs := []string{invoiceResp.PaymentRequest}
		ht.CompletePaymentRequests(alice, payReqs)

		p := ht.AssertNumPayments(alice, 1)[0]
		path := p.Htlcs[len(p.Htlcs)-1].Route.Hops

		// Ensure that the stored path shows a direct payment to Bob
		// with no other nodes in-between.
		require.Len(ht, path, 1, "wrong number of routes in path")
		require.Equal(ht, bob.PubKeyStr, path[0].PubKey, "wrong pubkey")

		// The payment amount should also match our previous payment
		// directly.
		require.EqualValues(ht, paymentAmt, p.ValueSat,
			"incorrect sat amount")
		require.EqualValues(ht, paymentAmt*1000, p.ValueMsat,
			"incorrect msat amount")

		// The payment hash (or r-hash) should have been stored
		// correctly.
		correctRHash := hex.EncodeToString(invoiceResp.RHash)
		require.Equal(ht, correctRHash, p.PaymentHash, "incorrect hash")

		// As we made a single-hop direct payment, there should have
		// been no fee applied.
		require.Zero(ht, p.FeeSat, "fee should be 0")
		require.Zero(ht, p.FeeMsat, "fee should be 0")

		// Now verify that the payment request returned by the rpc
		// matches the invoice that we paid.
		require.Equal(ht, invoiceResp.PaymentRequest, p.PaymentRequest,
			"incorrect payreq")

		// Delete all payments from Alice. DB should have no payments.
		alice.RPC.DeleteAllPayments()
		ht.AssertNumPayments(alice, 0)

		// TODO(yy): remove the sleep once the following bug is fixed.
		// When the invoice is reported settled, the commitment dance
		// is not yet finished, which can cause an error when closing
		// the channel, saying there's active HTLCs. We need to
		// investigate this issue and reverse the order to, first
		// finish the commitment dance, then report the invoice as
		// settled.
		time.Sleep(2 * time.Second)

		// Close the channel.
		//
		// NOTE: This implicitly tests that the channel link is active
		// before closing this channel. The above payment will trigger
		// a commitment dance in both of the nodes. If the node fails
		// to update the commitment state, we will fail to close the
		// channel as the link won't be active.
		ht.CloseChannel(alice, chanPoint)
	}

	// Run the test cases.
	for _, ct := range commitTyes {
		ht.Run(ct.String(), func(t *testing.T) {
			st := ht.Subtest(t)

			// Set the fee estimate to 1sat/vbyte.
			st.SetFeeEstimate(250)

			// Restart the nodes with the specified commitment type.
			args := lntest.NodeArgsForCommitType(ct)
			st.RestartNodeWithExtraArgs(alice, args)
			st.RestartNodeWithExtraArgs(bob, args)

			// Make sure they are connected.
			st.EnsureConnected(alice, bob)

			// Open a channel with 100k satoshis between Alice and
			// Bob with Alice being the sole funder of the channel.
			params := lntest.OpenChannelParams{
				Amt:            100_000,
				CommitmentType: ct,
			}

			// Open private channel for taproot channels.
			params.Private = ct ==
				lnrpc.CommitmentType_SIMPLE_TAPROOT

			testSendPayment(st, params)
		})
	}
}

func testListPayments(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// Check that there are no payments before test.
	ht.AssertNumPayments(alice, 0)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Get the number of invoices Bob already has.
	//
	// TODO(yy): we can remove this check once the `DeleteAllInvoices` rpc
	// is added.
	invResp := bob.RPC.ListInvoices(nil)
	numOldInvoices := len(invResp.Invoices)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := ht.Random32Bytes()
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp := bob.RPC.AddInvoice(invoice)

	// Check that Bob has added the invoice.
	numInvoices := numOldInvoices + 1
	ht.AssertNumInvoices(bob, 1)

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	payReqs := []string{invoiceResp.PaymentRequest}
	ht.CompletePaymentRequests(alice, payReqs)

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	p := ht.AssertNumPayments(alice, 1)[0]
	path := p.Htlcs[len(p.Htlcs)-1].Route.Hops

	// Ensure that the stored path shows a direct payment to Bob with no
	// other nodes in-between.
	require.Len(ht, path, 1, "wrong number of routes in path")
	require.Equal(ht, bob.PubKeyStr, path[0].PubKey, "wrong pub key")

	// The payment amount should also match our previous payment directly.
	require.EqualValues(ht, paymentAmt, p.ValueSat, "incorrect sat amount")
	require.EqualValues(ht, paymentAmt*1000, p.ValueMsat,
		"incorrect msat amount")

	// The payment hash (or r-hash) should have been stored correctly.
	correctRHash := hex.EncodeToString(invoiceResp.RHash)
	require.Equal(ht, correctRHash, p.PaymentHash, "incorrect RHash")

	// As we made a single-hop direct payment, there should have been no
	// fee applied.
	require.Zero(ht, p.FeeSat, "fee should be 0")
	require.Zero(ht, p.FeeMsat, "fee should be 0")

	// Now verify that the payment request returned by the rpc matches the
	// invoice that we paid.
	require.Equal(ht, invoiceResp.PaymentRequest, p.PaymentRequest,
		"incorrect payreq")

	// We now check the timestamp filters in `ListPayments`.
	//
	// Use a start date long time ago should return us the payment.
	req := &lnrpc.ListPaymentsRequest{
		CreationDateStart: 1227035905,
	}
	resp := alice.RPC.ListPayments(req)
	require.Len(ht, resp.Payments, 1)

	// Use an end date long time ago should return us nothing.
	req = &lnrpc.ListPaymentsRequest{
		CreationDateEnd: 1227035905,
	}
	resp = alice.RPC.ListPayments(req)
	require.Empty(ht, resp.Payments)

	// Use a start date far in the future should return us nothing.
	req = &lnrpc.ListPaymentsRequest{
		CreationDateStart: 5392552705,
	}
	resp = alice.RPC.ListPayments(req)
	require.Empty(ht, resp.Payments)

	// Use an end date far in the future should return us the payment.
	req = &lnrpc.ListPaymentsRequest{
		CreationDateEnd: 5392552705,
	}
	resp = alice.RPC.ListPayments(req)
	require.Len(ht, resp.Payments, 1)

	// We now do the same check for `ListInvoices`
	//
	// Use a start date long time ago should return us the invoice.
	invReq := &lnrpc.ListInvoiceRequest{
		CreationDateStart: 1227035905,
	}
	invResp = bob.RPC.ListInvoices(invReq)
	require.Len(ht, invResp.Invoices, numInvoices)

	// Use an end date long time ago should return us nothing.
	invReq = &lnrpc.ListInvoiceRequest{
		CreationDateEnd: 1227035905,
	}
	invResp = bob.RPC.ListInvoices(invReq)
	require.Empty(ht, invResp.Invoices)

	// Use a start date far in the future should return us nothing.
	invReq = &lnrpc.ListInvoiceRequest{
		CreationDateStart: 5392552705,
	}
	invResp = bob.RPC.ListInvoices(invReq)
	require.Empty(ht, invResp.Invoices)

	// Use an end date far in the future should return us the invoice.
	invReq = &lnrpc.ListInvoiceRequest{
		CreationDateEnd: 5392552705,
	}
	invResp = bob.RPC.ListInvoices(invReq)
	require.Len(ht, invResp.Invoices, numInvoices)

	// Delete all payments from Alice. DB should have no payments.
	alice.RPC.DeleteAllPayments()

	// Check that there are no payments after test.
	ht.AssertNumPayments(alice, 0)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	// Close the channel.
	ht.CloseChannel(alice, chanPoint)
}

// testPaymentFollowingChannelOpen tests that the channel transition from
// 'pending' to 'open' state does not cause any inconsistencies within other
// subsystems trying to update the channel state in the db. We follow this
// transition with a payment that updates the commitment state and verify that
// the pending state is up to date.
func testPaymentFollowingChannelOpen(ht *lntest.HarnessTest) {
	const paymentAmt = btcutil.Amount(100)
	channelCapacity := paymentAmt * 1000

	// We first establish a channel between Alice and Bob.
	alice, bob := ht.Alice, ht.Bob
	p := lntest.OpenChannelParams{
		Amt: channelCapacity,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, p)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 1)

	// We are restarting Bob's node to let the link be created for the
	// pending channel.
	ht.RestartNode(bob)

	// We ensure that Bob reconnects to Alice.
	ht.EnsureConnected(bob, alice)

	// We mine six blocks for the channel to be confirmed.
	ht.MineBlocksAndAssertNumTxes(6, 1)

	// We verify that the channel is open from both nodes point of view.
	chanPoint := lntest.ChanPointFromPendingUpdate(pendingUpdate)
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 0)
	ht.AssertChannelExists(alice, chanPoint)
	ht.AssertChannelExists(bob, chanPoint)

	// With the channel open, we'll create invoices for Bob that Alice will
	// pay to in order to advance the state of the channel.
	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, 1)

	// Send payment to Bob so that a channel update to disk will be
	// executed.
	ht.CompletePaymentRequests(alice, []string{bobPayReqs[0]})

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint)
}

// testAsyncPayments tests the performance of the async payments.
func testAsyncPayments(ht *lntest.HarnessTest) {
	// We use new nodes here as the benchmark test creates lots of data
	// which can be costly to be carried on.
	alice := ht.NewNode("Alice", []string{"--pending-commit-interval=3m"})
	bob := ht.NewNode("Bob", []string{"--pending-commit-interval=3m"})

	ht.EnsureConnected(alice, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	runAsyncPayments(ht, alice, bob, nil)
}

// runAsyncPayments tests the performance of the async payments.
func runAsyncPayments(ht *lntest.HarnessTest, alice, bob *node.HarnessNode,
	commitType *lnrpc.CommitmentType) {

	const paymentAmt = 100

	channelCapacity := btcutil.Amount(paymentAmt * 2000)

	chanArgs := lntest.OpenChannelParams{
		Amt: channelCapacity,
	}

	if commitType != nil {
		chanArgs.CommitmentType = *commitType

		if *commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
			chanArgs.Private = true
		}
	}

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	chanPoint := ht.OpenChannel(
		alice, bob, chanArgs,
	)

	info := ht.QueryChannelByChanPoint(alice, chanPoint)

	// We'll create a number of invoices equal the max number of HTLCs that
	// can be carried in one direction. The number on the commitment will
	// likely be lower, but we can't guarantee that any more HTLCs will
	// succeed due to the limited path diversity and inability of the router
	// to retry via another path.
	numInvoices := int(input.MaxHTLCNumber / 2)

	bobAmt := int64(numInvoices * paymentAmt)
	aliceAmt := info.LocalBalance - bobAmt

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numInvoices)

	// Simultaneously send payments from Alice to Bob using of Bob's
	// payment hashes generated above.
	now := time.Now()

	settled := make(chan struct{})
	defer close(settled)

	timeout := wait.AsyncBenchmarkTimeout
	for i := 0; i < numInvoices; i++ {
		payReq := bobPayReqs[i]
		go func() {
			req := &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: int32(timeout.Seconds()),
				FeeLimitMsat:   noFeeLimitMsat,
			}
			// AssertPaymentStatusWithTimeout will assert that the
			// payment is settled.
			stream := alice.RPC.SendPayment(req)
			ht.AssertPaymentSucceedWithTimeout(stream, timeout)

			settled <- struct{}{}
		}()
	}

	// Wait until all the payments have settled.
	timer := time.After(timeout)
	for i := 0; i < numInvoices; i++ {
		select {
		case <-settled:
		case <-timer:
			require.Fail(ht, "timeout", "wait payment failed")
		}
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Wait for the revocation to be received so alice no longer has
	// pending htlcs listed and has correct balances. This is needed due to
	// the fact that we now pipeline the settles.
	assertChannelState(ht, alice, chanPoint, aliceAmt, bobAmt)

	// Wait for Bob to receive revocation from Alice.
	assertChannelState(ht, bob, chanPoint, bobAmt, aliceAmt)

	ht.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	ht.Log("\tBenchmark info: TPS: ",
		float64(numInvoices)/timeTaken.Seconds())

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(ht *lntest.HarnessTest) {
	const paymentAmt = 1000

	// We use new nodes here as the benchmark test creates lots of data
	// which can be costly to be carried on.
	args := []string{
		// Increase the dust threshold to avoid the payments fail due
		// to threshold limit reached.
		"--dust-threshold=5000000",

		// Increase the pending commit interval since there are lots of
		// commitment dances.
		"--pending-commit-interval=5m",

		// Increase the mailbox delivery timeout as there are lots of
		// ADDs going on.
		"--htlcswitch.mailboxdeliverytimeout=2m",
	}
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	ht.EnsureConnected(alice, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     paymentAmt * 2000,
			PushAmt: paymentAmt * 1000,
		},
	)

	info := ht.QueryChannelByChanPoint(alice, chanPoint)

	// We'll create a number of invoices equal the max number of HTLCs that
	// can be carried in one direction. The number on the commitment will
	// likely be lower, but we can't guarantee that any more HTLCs will
	// succeed due to the limited path diversity and inability of the router
	// to retry via another path.
	numInvoices := int(input.MaxHTLCNumber / 2)

	// Nodes should exchange the same amount of money and because of this
	// at the end balances should remain the same.
	aliceAmt := info.LocalBalance
	bobAmt := info.RemoteBalance

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numInvoices)

	// With the channel open, we'll create invoices for Alice that Bob
	// will pay to in order to advance the state of the channel.
	alicePayReqs, _, _ := ht.CreatePayReqs(alice, paymentAmt, numInvoices)

	// Reset mission control to prevent previous payment results from
	// interfering with this test. A new channel has been opened, but
	// mission control operates on node pairs.
	alice.RPC.ResetMissionControl()

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	settled := make(chan struct{})
	defer close(settled)

	timeout := wait.AsyncBenchmarkTimeout * 2
	send := func(node *node.HarnessNode, payReq string) {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: int32(timeout.Seconds()),
			FeeLimitMsat:   noFeeLimitMsat,
		}
		// AssertPaymentStatusWithTimeout will assert that the
		// payment is settled.
		stream := node.RPC.SendPayment(req)
		ht.AssertPaymentSucceedWithTimeout(stream, timeout)

		settled <- struct{}{}
	}

	for i := 0; i < numInvoices; i++ {
		go send(bob, alicePayReqs[i])
		go send(alice, bobPayReqs[i])
	}

	// Expect all payments to succeed.
	timer := time.After(timeout)
	for i := 0; i < 2*numInvoices; i++ {
		select {
		case <-settled:
		case <-timer:
			require.Fail(ht, "timeout", "wait payment failed")
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	assertChannelState(ht, alice, chanPoint, aliceAmt, bobAmt)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	assertChannelState(ht, bob, chanPoint, bobAmt, aliceAmt)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint)
}

func testInvoiceSubscriptions(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(500000)

	alice, bob := ht.Alice, ht.Bob

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	bobInvoiceSubscription := bob.RPC.SubscribeInvoices(req)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next create a new invoice for Bob requesting 1k satoshis.
	const paymentAmt = 1000
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: ht.Random32Bytes(),
		Value:     paymentAmt,
	}
	invoiceResp := bob.RPC.AddInvoice(invoice)
	lastAddIndex := invoiceResp.AddIndex

	// With the above invoice added, we should receive an update event.
	invoiceUpdate := ht.ReceiveInvoiceUpdate(bobInvoiceSubscription)
	require.NotEqual(ht, lnrpc.Invoice_SETTLED, invoiceUpdate.State,
		"invoice should not be settled")

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	ht.CompletePaymentRequests(alice, []string{invoiceResp.PaymentRequest})

	// The invoice update should exactly match the invoice created
	// above, but should now be settled and have SettleDate
	invoiceUpdate = ht.ReceiveInvoiceUpdate(bobInvoiceSubscription)
	require.Equal(ht, lnrpc.Invoice_SETTLED, invoiceUpdate.State,
		"invoice not settled but should be")
	require.NotZero(ht, invoiceUpdate.SettleDate,
		"invoice should have non zero settle date, but doesn't")
	require.Equal(ht, invoice.RPreimage, invoiceUpdate.RPreimage,
		"payment preimages don't match")
	require.NotZero(ht, invoiceUpdate.SettleIndex,
		"invoice should have settle index")
	settleIndex := invoiceUpdate.SettleIndex

	// We'll now add 3 more invoices to Bob's invoice registry.
	const numInvoices = 3
	payReqs, _, newInvoices := ht.CreatePayReqs(
		bob, paymentAmt, numInvoices,
	)

	// Now that the set of invoices has been added, we'll re-register for
	// streaming invoice notifications for Bob, this time specifying the
	// add invoice of the last prior invoice.
	req = &lnrpc.InvoiceSubscription{AddIndex: lastAddIndex}
	bobInvoiceSubscription = bob.RPC.SubscribeInvoices(req)

	// Since we specified a value of the prior add index above, we should
	// now immediately get the invoices we just added as we should get the
	// backlog of notifications.
	for i := 0; i < numInvoices; i++ {
		invoiceUpdate := ht.ReceiveInvoiceUpdate(bobInvoiceSubscription)

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		require.NotEqual(ht, lnrpc.Invoice_SETTLED, invoiceUpdate.State,
			"should have only received add events")

		originalInvoice := newInvoices[i]
		rHash := sha256.Sum256(originalInvoice.RPreimage)
		require.Equal(ht, rHash[:], invoiceUpdate.RHash,
			"invoices have mismatched payment hashes")
	}

	// We'll now have Bob settle out the remainder of these invoices so we
	// can test that all settled invoices are properly notified.
	ht.CompletePaymentRequests(alice, payReqs)

	// With the set of invoices paid, we'll now cancel the old
	// subscription, and create a new one for Bob, this time using the
	// settle index to obtain the backlog of settled invoices.
	req = &lnrpc.InvoiceSubscription{
		SettleIndex: settleIndex,
	}
	bobInvoiceSubscription = bob.RPC.SubscribeInvoices(req)

	// As we specified the index of the past settle index, we should now
	// receive notifications for the three HTLCs that we just settled. As
	// the order that the HTLCs will be settled in is partially randomized,
	// we'll use a map to assert that the proper set has been settled.
	settledInvoices := make(map[[32]byte]struct{})
	for _, invoice := range newInvoices {
		rHash := sha256.Sum256(invoice.RPreimage)
		settledInvoices[rHash] = struct{}{}
	}
	for i := 0; i < numInvoices; i++ {
		invoiceUpdate := ht.ReceiveInvoiceUpdate(bobInvoiceSubscription)

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		require.Equal(ht, lnrpc.Invoice_SETTLED, invoiceUpdate.State,
			"should have only received settle events")

		var rHash [32]byte
		copy(rHash[:], invoiceUpdate.RHash)
		require.Contains(ht, settledInvoices, rHash,
			"unknown invoice settled")

		delete(settledInvoices, rHash)
	}

	// At this point, all the invoices should be fully settled.
	require.Empty(ht, settledInvoices, "not all invoices settled")

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	ht.CloseChannel(alice, chanPoint)
}

// assertChannelState asserts the channel state by checking the values in
// fields, LocalBalance, RemoteBalance and num of PendingHtlcs.
func assertChannelState(ht *lntest.HarnessTest, hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, localBalance, remoteBalance int64) {

	// Get the funding point.
	err := wait.NoError(func() error {
		// Find the target channel first.
		target := ht.GetChannelByChanPoint(hn, cp)

		if len(target.PendingHtlcs) != 0 {
			return fmt.Errorf("pending htlcs is "+
				"incorrect, got %v, expected %v",
				len(target.PendingHtlcs), 0)
		}

		if target.LocalBalance != localBalance {
			return fmt.Errorf("local balance is "+
				"incorrect, got %v, expected %v",
				target.LocalBalance, localBalance)
		}

		if target.RemoteBalance != remoteBalance {
			return fmt.Errorf("remote balance is "+
				"incorrect, got %v, expected %v",
				target.RemoteBalance, remoteBalance)
		}

		return nil
	}, lntest.DefaultTimeout)
	require.NoError(ht, err, "timeout while chekcing for balance")
}
