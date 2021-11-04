package itest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testListPayments(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// Check that there are no payments before test.
	ht.AssertNumPayments(alice, 0, true)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)
	defer ht.CloseChannel(alice, chanPoint, false)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("B"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp := ht.AddInvoice(invoice, bob)

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitSat:    1000000,
	}
	ht.SendPaymentAndAssert(alice, req)

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	p := ht.AssertNumPayments(alice, 1, true)[0]
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

	// Finally, verify that the payment request returned by the rpc matches
	// the invoice that we paid.
	require.Equal(ht, invoiceResp.PaymentRequest, p.PaymentRequest,
		"incorrect payreq")

	// Delete all payments from Alice. DB should have no payments.
	ht.DeleteAllPayments(alice)

	// Snapshot the node's state so that the old payments won't affect us.
	ht.SnapshotNodeStates()

	// Check that there are no payments after test.
	ht.AssertNumPayments(alice, 0, true)
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
	pendingUpdate := ht.OpenPendingChannel(alice, bob, channelCapacity, 0)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ht.AssertNumOpenChannelsPending(alice, bob, 1)

	// We are restarting Bob's node to let the link be created for the
	// pending channel.
	ht.RestartNode(bob)

	// We ensure that Bob reconnects to Alice.
	ht.EnsureConnected(bob, alice)

	// We mine one block for the channel to be confirmed.
	ht.MineBlocksAndAssertTx(6, 1)

	// We verify that the channel is open from both nodes point of view.
	chanPoint := lntest.ChanPointFromPendingUpdate(pendingUpdate)
	ht.AssertNumOpenChannelsPending(alice, bob, 0)
	ht.AssertChannelExists(alice, chanPoint)
	ht.AssertChannelExists(bob, chanPoint)

	// With the channel open, we'll create invoices for Bob that Alice will
	// pay to in order to advance the state of the channel.
	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, 1)

	// Send payment to Bob so that a channel update to disk will be
	// executed.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: bobPayReqs[0],
		TimeoutSeconds: 60,
		FeeLimitSat:    1000000,
	}
	ht.SendPaymentAndAssert(alice, req)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint, false)
}

// testAsyncPayments tests the performance of the async payments.
func testAsyncPayments(ht *lntest.HarnessTest) {
	// We use new nodes here as the benchmark test creates lots of data
	// which can be costly to be carried on.
	// TODO(yy): further investigate this test as the lnd seems to be
	// stuck when using standby nodes.
	alice := ht.NewNode("Alice", nil)
	defer ht.Shutdown(alice)
	bob := ht.NewNode("Bob", nil)
	defer ht.Shutdown(bob)

	ht.EnsureConnected(alice, bob)
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

	runAsyncPayments(ht, alice, bob)
}

// runAsyncPayments tests the performance of the async payments.
func runAsyncPayments(ht *lntest.HarnessTest, alice, bob *lntest.HarnessNode) {
	const paymentAmt = 100

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	channelCapacity := btcutil.Amount(paymentAmt * 2000)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: channelCapacity},
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
	for i := 0; i < numInvoices; i++ {
		payReq := bobPayReqs[i]
		go func() {
			req := &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			}
			// SendPaymentAndAssert will assert that the
			// payment is settled.
			ht.SendPaymentAndAssert(alice, req)

			settled <- struct{}{}
		}()
	}

	// Wait until all the payments have settled.
	timer := time.After(lntest.AsyncBenchmarkTimeout)
	for i := 0; i < numInvoices; i++ {
		select {
		case <-settled:
		case <-timer:
			require.Fail(ht, "timeout", "wait payment failed")
		}
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Wait for the revocation to be received so alice no longer has pending
	// htlcs listed and has correct balances. This is needed due to the fact
	// that we now pipeline the settles.
	ht.AssertChannelState(alice, chanPoint, aliceAmt, bobAmt, 0)

	// Wait for Bob to receive revocation from Alice.
	ht.AssertChannelState(bob, chanPoint, bobAmt, aliceAmt, 0)

	ht.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	ht.Log("\tBenchmark info: TPS: ",
		float64(numInvoices)/timeTaken.Seconds())

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint, false)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(ht *lntest.HarnessTest) {
	const paymentAmt = 1000

	// We use new nodes here as the benchmark test creates lots of data
	// which can be costly to be carried on.
	// TODO(yy): further investigate this test as the lnd seems to be
	// stuck when using standby nodes.
	alice := ht.NewNode("Alice", nil)
	defer ht.Shutdown(alice)
	bob := ht.NewNode("Bob", nil)
	defer ht.Shutdown(bob)

	ht.EnsureConnected(alice, bob)
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

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
	ht.ResetMissionControl(alice)

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	settled := make(chan struct{})
	defer close(settled)
	send := func(node *lntest.HarnessNode, payReq string) {
		go func() {
			req := &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			}
			ht.SendPaymentAndAssert(node, req)

			settled <- struct{}{}
		}()
	}

	for i := 0; i < numInvoices; i++ {
		send(bob, alicePayReqs[i])
		send(alice, bobPayReqs[i])
	}

	// Expect all payments to succeed.
	timer := time.After(lntest.AsyncBenchmarkTimeout)
	for i := 0; i < 2*numInvoices; i++ {
		select {
		case <-settled:
		case <-timer:
			require.Fail(ht, "timeout", "wait payment failed")
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	ht.AssertChannelState(alice, chanPoint, aliceAmt, bobAmt, 0)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	ht.AssertChannelState(bob, chanPoint, bobAmt, aliceAmt, 0)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint, false)
}

func testInvoiceSubscriptions(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(500000)

	alice, bob := ht.Alice, ht.Bob

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	bobInvoiceSubscription := ht.SubscribeInvoices(bob, req)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next create a new invoice for Bob requesting 1k satoshis.
	// TODO(roasbeef): make global list of invoices for each node to re-use
	// and avoid collisions
	const paymentAmt = 1000
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: ht.Random32Bytes(),
		Value:     paymentAmt,
	}
	invoiceResp := ht.AddInvoice(invoice, bob)
	lastAddIndex := invoiceResp.AddIndex

	// With the above invoice added, we should receive an update event.
	invoiceUpdate := ht.ReceiveInvoiceUpdate(bobInvoiceSubscription)
	require.NotEqual(ht, lnrpc.Invoice_SETTLED, invoiceUpdate.State,
		"invoice should not be settled")

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAndAssert(alice, sendReq)

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
	bobInvoiceSubscription = ht.SubscribeInvoices(bob, req)

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
	ht.CompletePaymentRequests(alice, payReqs, true)

	// With the set of invoices paid, we'll now cancel the old
	// subscription, and create a new one for Bob, this time using the
	// settle index to obtain the backlog of settled invoices.
	req = &lnrpc.InvoiceSubscription{
		SettleIndex: settleIndex,
	}
	bobInvoiceSubscription = ht.SubscribeInvoices(bob, req)

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

	ht.CloseChannel(alice, chanPoint, false)
}
