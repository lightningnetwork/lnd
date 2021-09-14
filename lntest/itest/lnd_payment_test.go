package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

func testListPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First start by deleting all payments that Alice knows of. This will
	// allow us to execute the test with a clean state for Alice.
	delPaymentsReq := &lnrpc.DeleteAllPaymentsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	if _, err := net.Alice.DeleteAllPayments(ctxt, delPaymentsReq); err != nil {
		t.Fatalf("unable to delete payments: %v", err)
	}

	// Check that there are no payments before test.
	reqInit := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	paymentsRespInit, err := net.Alice.ListPayments(ctxt, reqInit)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsRespInit.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

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
	addInvoiceCtxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := net.Bob.AddInvoice(addInvoiceCtxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	if err = net.Alice.WaitForNetworkChannelOpen(chanPoint); err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(chanPoint); err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitSat:    1000000,
		},
	)

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	req := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(ctxt, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 1 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), 1)
	}
	p := paymentsResp.Payments[0] // nolint:staticcheck
	path := p.Htlcs[len(p.Htlcs)-1].Route.Hops

	// Ensure that the stored path shows a direct payment to Bob with no
	// other nodes in-between.
	if len(path) != 1 || path[0].PubKey != net.Bob.PubKeyStr {
		t.Fatalf("incorrect path")
	}

	// The payment amount should also match our previous payment directly.
	if p.Value != paymentAmt { // nolint:staticcheck
		t.Fatalf("incorrect amount, got %v, want %v",
			p.Value, paymentAmt) // nolint:staticcheck
	}

	// The payment hash (or r-hash) should have been stored correctly.
	correctRHash := hex.EncodeToString(invoiceResp.RHash)
	if !reflect.DeepEqual(p.PaymentHash, correctRHash) {
		t.Fatalf("incorrect RHash, got %v, want %v",
			p.PaymentHash, correctRHash)
	}

	// As we made a single-hop direct payment, there should have been no fee
	// applied.
	if p.Fee != 0 { // nolint:staticcheck
		t.Fatalf("incorrect Fee, got %v, want %v", p.Fee, 0) // nolint:staticcheck
	}

	// Finally, verify that the payment request returned by the rpc matches
	// the invoice that we paid.
	if p.PaymentRequest != invoiceResp.PaymentRequest {
		t.Fatalf("incorrect payreq, got: %v, want: %v",
			p.PaymentRequest, invoiceResp.PaymentRequest)
	}

	// Delete all payments from Alice. DB should have no payments.
	delReq := &lnrpc.DeleteAllPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.DeleteAllPayments(ctxt, delReq)
	if err != nil {
		t.Fatalf("Can't delete payments at the end: %v", err)
	}

	// Check that there are no payments after test.
	listReq := &lnrpc.ListPaymentsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	paymentsResp, err = net.Alice.ListPayments(ctxt, listReq)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), 0)
	}

	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

// testPaymentFollowingChannelOpen tests that the channel transition from
// 'pending' to 'open' state does not cause any inconsistencies within other
// subsystems trying to update the channel state in the db. We follow this
// transition with a payment that updates the commitment state and verify that
// the pending state is up to date.
func testPaymentFollowingChannelOpen(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const paymentAmt = btcutil.Amount(100)
	channelCapacity := paymentAmt * 1000

	// We first establish a channel between Alice and Bob.
	pendingUpdate, err := net.OpenPendingChannel(
		net.Alice, net.Bob, channelCapacity, 0,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	assertNumOpenChannelsPending(t, net.Alice, net.Bob, 1)

	// We are restarting Bob's node to let the link be created for the
	// pending channel.
	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Bob restart failed: %v", err)
	}

	// We ensure that Bob reconnects to Alice.
	net.EnsureConnected(t.t, net.Bob, net.Alice)

	// We mine one block for the channel to be confirmed.
	_ = mineBlocks(t, net, 6, 1)[0]

	// We verify that the channel is open from both nodes point of view.
	assertNumOpenChannelsPending(t, net.Alice, net.Bob, 0)

	// With the channel open, we'll create invoices for Bob that Alice will
	// pay to in order to advance the state of the channel.
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, 1,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Send payment to Bob so that a channel update to disk will be
	// executed.
	sendAndAssertSuccess(
		t, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: bobPayReqs[0],
			TimeoutSeconds: 60,
			FeeLimitSat:    1000000,
		},
	)

	// At this point we want to make sure the channel is opened and not
	// pending.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	res, err := net.Bob.ListChannels(ctxt, &lnrpc.ListChannelsRequest{})
	if err != nil {
		t.Fatalf("unable to list bob channels: %v", err)
	}
	if len(res.Channels) == 0 {
		t.Fatalf("bob list of channels is empty")
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}
	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

// testAsyncPayments tests the performance of the async payments.
func testAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	runAsyncPayments(net, t, net.Alice, net.Bob)
}

// runAsyncPayments tests the performance of the async payments.
func runAsyncPayments(net *lntest.NetworkHarness, t *harnessTest, alice,
	bob *lntest.HarnessNode) {

	ctxb := context.Background()

	const (
		paymentAmt = 100
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	channelCapacity := btcutil.Amount(paymentAmt * 2000)
	chanPoint := openChannelAndAssert(
		t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt: channelCapacity,
		},
	)

	info, err := getChanInfo(alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

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
	bobPayReqs, _, _, err := createPayReqs(
		bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	err = alice.WaitForNetworkChannelOpen(chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	// Simultaneously send payments from Alice to Bob using of Bob's payment
	// hashes generated above.
	now := time.Now()
	errChan := make(chan error)
	statusChan := make(chan *lnrpc.Payment)
	for i := 0; i < numInvoices; i++ {
		payReq := bobPayReqs[i]
		go func() {
			ctxt, _ := context.WithTimeout(ctxb, lntest.AsyncBenchmarkTimeout)
			stream, err := alice.RouterClient.SendPaymentV2(
				ctxt,
				&routerrpc.SendPaymentRequest{
					PaymentRequest: payReq,
					TimeoutSeconds: 60,
					FeeLimitMsat:   noFeeLimitMsat,
				},
			)
			if err != nil {
				errChan <- err
			}
			result, err := getPaymentResult(stream)
			if err != nil {
				errChan <- err
			}

			statusChan <- result
		}()
	}

	// Wait until all the payments have settled.
	for i := 0; i < numInvoices; i++ {
		select {
		case result := <-statusChan:
			if result.Status == lnrpc.Payment_SUCCEEDED {
				continue
			}

		case err := <-errChan:
			t.Fatalf("payment error: %v", err)
		}
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.

	// Wait for the revocation to be received so alice no longer has pending
	// htlcs listed and has correct balances. This is needed due to the fact
	// that we now pipeline the settles.
	err = wait.Predicate(func() bool {
		aliceChan, err := getChanInfo(alice)
		if err != nil {
			return false
		}
		if len(aliceChan.PendingHtlcs) != 0 {
			return false
		}
		if aliceChan.RemoteBalance != bobAmt {
			return false
		}
		if aliceChan.LocalBalance != aliceAmt {
			return false
		}

		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("failed to assert alice's pending htlcs and/or remote/local balance")
	}

	// Wait for Bob to receive revocation from Alice.
	err = wait.NoError(func() error {
		bobChan, err := getChanInfo(bob)
		if err != nil {
			t.Fatalf("unable to get bob's channel info: %v", err)
		}

		if len(bobChan.PendingHtlcs) != 0 {
			return fmt.Errorf("bob's pending htlcs is incorrect, "+
				"got %v, expected %v",
				len(bobChan.PendingHtlcs), 0)
		}

		if bobChan.LocalBalance != bobAmt {
			return fmt.Errorf("bob's local balance is incorrect, "+
				"got %v, expected %v", bobChan.LocalBalance,
				bobAmt)
		}

		if bobChan.RemoteBalance != aliceAmt {
			return fmt.Errorf("bob's remote balance is incorrect, "+
				"got %v, expected %v", bobChan.RemoteBalance,
				aliceAmt)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)

	t.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	t.Log("\tBenchmark info: TPS: ", float64(numInvoices)/timeTaken.Seconds())

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	closeChannelAndAssert(t, net, alice, chanPoint, false)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		paymentAmt = 1000
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     paymentAmt * 2000,
			PushAmt: paymentAmt * 1000,
		},
	)

	info, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

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
	bobPayReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// With the channel open, we'll create invoices for Alice that Bob
	// will pay to in order to advance the state of the channel.
	alicePayReqs, _, _, err := createPayReqs(
		net.Alice, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	if err = net.Alice.WaitForNetworkChannelOpen(chanPoint); err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(chanPoint); err != nil {
		t.Fatalf("bob didn't see the bob->alice channel before "+
			"timeout: %v", err)
	}

	// Reset mission control to prevent previous payment results from
	// interfering with this test. A new channel has been opened, but
	// mission control operates on node pairs.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	_, err = net.Alice.RouterClient.ResetMissionControl(
		ctxt, &routerrpc.ResetMissionControlRequest{},
	)
	if err != nil {
		t.Fatalf("unable to reset mc for alice: %v", err)
	}

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	errChan := make(chan error)
	statusChan := make(chan *lnrpc.Payment)

	send := func(node *lntest.HarnessNode, payReq string) {
		go func() {
			ctxt, _ = context.WithTimeout(
				ctxb, lntest.AsyncBenchmarkTimeout,
			)
			stream, err := node.RouterClient.SendPaymentV2(
				ctxt,
				&routerrpc.SendPaymentRequest{
					PaymentRequest: payReq,
					TimeoutSeconds: 60,
					FeeLimitMsat:   noFeeLimitMsat,
				},
			)
			if err != nil {
				errChan <- err
			}
			result, err := getPaymentResult(stream)
			if err != nil {
				errChan <- err
			}

			statusChan <- result
		}()
	}

	for i := 0; i < numInvoices; i++ {
		send(net.Bob, alicePayReqs[i])
		send(net.Alice, bobPayReqs[i])
	}

	// Expect all payments to succeed.
	for i := 0; i < 2*numInvoices; i++ {
		select {
		case result := <-statusChan:
			if result.Status != lnrpc.Payment_SUCCEEDED {
				t.Fatalf("payment error: %v", result.Status)
			}

		case err := <-errChan:
			t.Fatalf("payment error: %v", err)
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	err = wait.NoError(func() error {
		aliceInfo, err := getChanInfo(net.Alice)
		if err != nil {
			t.Fatalf("unable to get alice's channel info: %v", err)
		}

		if aliceInfo.RemoteBalance != bobAmt {
			return fmt.Errorf("alice's remote balance is incorrect, "+
				"got %v, expected %v", aliceInfo.RemoteBalance,
				bobAmt)
		}

		if aliceInfo.LocalBalance != aliceAmt {
			return fmt.Errorf("alice's local balance is incorrect, "+
				"got %v, expected %v", aliceInfo.LocalBalance,
				aliceAmt)
		}

		if len(aliceInfo.PendingHtlcs) != 0 {
			return fmt.Errorf("alice's pending htlcs is incorrect, "+
				"got %v expected %v",
				len(aliceInfo.PendingHtlcs), 0)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	err = wait.NoError(func() error {
		bobInfo, err := getChanInfo(net.Bob)
		if err != nil {
			t.Fatalf("unable to get bob's channel info: %v", err)
		}

		if bobInfo.LocalBalance != bobAmt {
			return fmt.Errorf("bob's local balance is incorrect, "+
				"got %v, expected %v", bobInfo.LocalBalance,
				bobAmt)
		}

		if bobInfo.RemoteBalance != aliceAmt {
			return fmt.Errorf("bob's remote balance is incorrect, "+
				"got %v, expected %v", bobInfo.RemoteBalance,
				aliceAmt)
		}

		if len(bobInfo.PendingHtlcs) != 0 {
			return fmt.Errorf("bob's pending htlcs is incorrect, "+
				"got %v, expected %v",
				len(bobInfo.PendingHtlcs), 0)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

func testInvoiceSubscriptions(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(500000)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Next create a new invoice for Bob requesting 1k satoshis.
	// TODO(roasbeef): make global list of invoices for each node to re-use
	// and avoid collisions
	const paymentAmt = 1000
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: makeFakePayHash(t),
		Value:     paymentAmt,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	lastAddIndex := invoiceResp.AddIndex

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	ctx, cancelInvoiceSubscription := context.WithCancel(ctxb)
	bobInvoiceSubscription, err := net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	var settleIndex uint64
	quit := make(chan struct{})
	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		select {
		case <-quit:
			// Received cancellation
			return
		default:
		}

		if err != nil {
			t.Fatalf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled and have SettleDate
		if !invoiceUpdate.Settled { // nolint:staticcheck
			t.Fatalf("invoice not settled but should be")
		}
		if invoiceUpdate.SettleDate == 0 {
			t.Fatalf("invoice should have non zero settle date, but doesn't")
		}

		if !bytes.Equal(invoiceUpdate.RPreimage, invoice.RPreimage) {
			t.Fatalf("payment preimages don't match: expected %v, got %v",
				invoice.RPreimage, invoiceUpdate.RPreimage)
		}

		if invoiceUpdate.SettleIndex == 0 {
			t.Fatalf("invoice should have settle index")
		}

		settleIndex = invoiceUpdate.SettleIndex

		close(updateSent)
	}()

	// Wait for the channel to be recognized by both Alice and Bob before
	// continuing the rest of the test.
	err = net.Alice.WaitForNetworkChannelOpen(chanPoint)
	if err != nil {
		// TODO(roasbeef): will need to make num blocks to advertise a
		// node param
		close(quit)
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	stream, err := net.Alice.RouterClient.SendPaymentV2(ctxt, sendReq)
	if err != nil {
		close(quit)
		t.Fatalf("unable to send payment: %v", err)
	}
	result, err := getPaymentResult(stream)
	if err != nil {
		close(quit)
		t.Fatalf("cannot get payment result: %v", err)
	}
	if result.Status != lnrpc.Payment_SUCCEEDED {
		close(quit)
		t.Fatalf("error when attempting recv: %v", result.Status)
	}

	select {
	case <-time.After(time.Second * 10):
		close(quit)
		t.Fatalf("update not sent after 10 seconds")
	case <-updateSent: // Fall through on success
	}

	// With the base case working, we'll now cancel Bob's current
	// subscription in order to exercise the backlog fill behavior.
	cancelInvoiceSubscription()

	// We'll now add 3 more invoices to Bob's invoice registry.
	const numInvoices = 3
	payReqs, _, newInvoices, err := createPayReqs(
		net.Bob, paymentAmt, numInvoices,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// Now that the set of invoices has been added, we'll re-register for
	// streaming invoice notifications for Bob, this time specifying the
	// add invoice of the last prior invoice.
	req = &lnrpc.InvoiceSubscription{
		AddIndex: lastAddIndex,
	}
	ctx, cancelInvoiceSubscription = context.WithCancel(ctxb)
	bobInvoiceSubscription, err = net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	// Since we specified a value of the prior add index above, we should
	// now immediately get the invoices we just added as we should get the
	// backlog of notifications.
	for i := 0; i < numInvoices; i++ {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			t.Fatalf("unable to receive subscription")
		}

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		if invoiceUpdate.Settled { // nolint:staticcheck
			t.Fatalf("should have only received add events")
		}
		originalInvoice := newInvoices[i]
		rHash := sha256.Sum256(originalInvoice.RPreimage)
		if !bytes.Equal(invoiceUpdate.RHash, rHash[:]) {
			t.Fatalf("invoices have mismatched payment hashes: "+
				"expected %x, got %x", rHash[:],
				invoiceUpdate.RHash)
		}
	}

	cancelInvoiceSubscription()

	// We'll now have Bob settle out the remainder of these invoices so we
	// can test that all settled invoices are properly notified.
	err = completePaymentRequests(
		net.Alice, net.Alice.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// With the set of invoices paid, we'll now cancel the old
	// subscription, and create a new one for Bob, this time using the
	// settle index to obtain the backlog of settled invoices.
	req = &lnrpc.InvoiceSubscription{
		SettleIndex: settleIndex,
	}
	ctx, cancelInvoiceSubscription = context.WithCancel(ctxb)
	bobInvoiceSubscription, err = net.Bob.SubscribeInvoices(ctx, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	defer cancelInvoiceSubscription()

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
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			t.Fatalf("unable to receive subscription")
		}

		// We should now get the ith invoice we added, as they should
		// be returned in order.
		if !invoiceUpdate.Settled { // nolint:staticcheck
			t.Fatalf("should have only received settle events")
		}

		var rHash [32]byte
		copy(rHash[:], invoiceUpdate.RHash)
		if _, ok := settledInvoices[rHash]; !ok {
			t.Fatalf("unknown invoice settled: %x", rHash)
		}

		delete(settledInvoices, rHash)
	}

	// At this point, all the invoices should be fully settled.
	if len(settledInvoices) != 0 {
		t.Fatalf("not all invoices settled")
	}

	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}
