package itest

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	numPayments = 5
	paymentAmt  = 1000
	baseFee     = 1
)

// testSwitchCircuitPersistence creates a multihop network to ensure the sender
// and intermediaries are persisting their open payment circuits. After
// forwarding a packet via an outgoing link, all are restarted, and expected to
// forward a response back from the receiver once back online.
//
// The general flow of this test:
//  1. Carol --> Dave --> Alice --> Bob  forward payment
//  2. -------X       X         X   Bob  restart sender and intermediaries
//  3. Carol <-- Dave <-- Alice <-- Bob  expect settle to propagate
//
//nolint:dupword
func testSwitchCircuitPersistence(ht *lntemp.HarnessTest) {
	// Setup our test scenario. We should now have four nodes running with
	// three channels.
	s := setupScenarioFourNodes(ht)
	defer s.cleanUp()

	// Restart the intermediaries and the sender.
	ht.RestartNode(s.dave)
	ht.RestartNode(s.alice)
	ht.RestartNode(s.bob)

	// Ensure all of the intermediate links are reconnected.
	ht.EnsureConnected(s.alice, s.dave)
	ht.EnsureConnected(s.bob, s.alice)

	// Ensure all nodes in the network still have 5 outstanding htlcs.
	s.assertHTLCs(ht, numPayments)

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	s.carol.SetExtraArgs(nil)
	ht.RestartNode(s.carol)

	ht.EnsureConnected(s.dave, s.carol)

	// After the payments settle, there should be no active htlcs on any of
	// the nodes in the network.
	s.assertHTLCs(ht, 0)

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within
	// the payment flow generated above. The order of asserts corresponds
	// to increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	s.assertAmoutPaid(ht, amountPaid, numPayments)

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp := s.carol.RPC.AddInvoice(finalInvoice)
	payReqs := []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ht.CompletePaymentRequests(s.bob, payReqs)

	amountPaid = int64(6000)
	s.assertAmoutPaid(ht, amountPaid, numPayments+1)
}

// testSwitchOfflineDelivery constructs a set of multihop payments, and tests
// that the returning payments are not lost if a peer on the backwards path is
// offline when the settle/fails are received. We expect the payments to be
// buffered in memory, and transmitted as soon as the disconnect link comes back
// online.
//
// The general flow of this test:
//  1. Carol --> Dave --> Alice --> Bob  forward payment
//  2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//  3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//  4. Carol <-- Dave <-- Alice --- Bob  reconnect, expect settle to propagate
func testSwitchOfflineDelivery(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	net.ConnectNodes(t.t, dave, net.Alice)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, dave)

	chanPointDave := openChannelAndAssert(
		t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol := net.NewNode(t.t, "Carol", []string{"--hodl.exit-settle"})
	defer shutdownAndAssert(net, t, carol)

	net.ConnectNodes(t.t, carol, dave)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	chanPointCarol := openChannelAndAssert(
		t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			err = node.WaitForNetworkChannelOpen(chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	err = dave.WaitForNetworkChannelOpen(chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Make sure all nodes are fully synced before we continue.
	for _, node := range nodes {
		err := node.WaitForBlockchainSync()
		if err != nil {
			t.Fatalf("unable to wait for sync: %v", err)
		}
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	err = completePaymentRequests(
		net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all of the payments to reach Carol.
	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	peerReq := &lnrpc.PeerEventSubscription{}
	peerClient, err := dave.SubscribePeerEvents(ctxb, peerReq)
	require.NoError(t.t, err)

	// First, disconnect Dave and Alice so that their link is broken.
	if err := net.DisconnectNodes(dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Wait to receive the PEER_OFFLINE event before reconnecting them.
	peerEvent, err := peerClient.Recv()
	require.NoError(t.t, err)
	require.Equal(t.t, lnrpc.PeerEvent_PEER_OFFLINE, peerEvent.GetType())

	// Then, reconnect them to ensure Dave doesn't just fail back the htlc.
	// We use EnsureConnected here in case they have already re-connected.
	net.EnsureConnected(t.t, dave, net.Alice)

	// Wait to ensure that the payment remain are not failed back after
	// reconnecting. All node should report the number payments initiated
	// for the duration of the interval.
	err = wait.Invariant(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc change: %v", predErr)
	}

	// Now, disconnect Dave from Alice again before settling back the
	// payment.
	if err := net.DisconnectNodes(dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Wait to receive the PEER_ONLINE and then the PEER_OFFLINE event
	// before advancing.
	peerEvent2, err := peerClient.Recv()
	require.NoError(t.t, err)
	require.Equal(t.t, lnrpc.PeerEvent_PEER_ONLINE, peerEvent2.GetType())

	peerEvent3, err := peerClient.Recv()
	require.NoError(t.t, err)
	require.Equal(t.t, lnrpc.PeerEvent_PEER_OFFLINE, peerEvent3.GetType())

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Make sure all nodes are fully synced again.
	for _, node := range nodes {
		err := node.WaitForBlockchainSync()
		if err != nil {
			t.Fatalf("unable to wait for sync: %v", err)
		}
	}

	// Now that the settles have reached Dave, reconnect him with Alice,
	// allowing the settles to return to the sender.
	net.EnsureConnected(t.t, dave, net.Alice)

	// Wait until all outstanding htlcs in the network have been settled.
	err = wait.Predicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0) == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	err = completePaymentRequests(
		net.Bob, net.Bob.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	closeChannelAndAssert(t, net, net.Alice, chanPointAlice, false)
	closeChannelAndAssert(t, net, dave, chanPointDave, false)
	closeChannelAndAssert(t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryPersistence constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//  1. Carol --> Dave --> Alice --> Bob  forward payment
//  2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//  3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//  4. Carol --- Dave  X         X  Bob  restart Alice
//  5. Carol <-- Dave <-- Alice --- Bob  expect settle to propagate
func testSwitchOfflineDeliveryPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	net.ConnectNodes(t.t, dave, net.Alice)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, dave)

	chanPointDave := openChannelAndAssert(
		t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol := net.NewNode(t.t, "Carol", []string{"--hodl.exit-settle"})
	defer shutdownAndAssert(net, t, carol)

	net.ConnectNodes(t.t, carol, dave)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	chanPointCarol := openChannelAndAssert(
		t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			err = node.WaitForNetworkChannelOpen(chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	err = dave.WaitForNetworkChannelOpen(chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	err = completePaymentRequests(
		net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	var predErr error
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodes, numPayments)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Disconnect the two intermediaries, Alice and Dave, by shutting down
	// Alice.
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Make Carol and Dave are reconnected before waiting for the htlcs to
	// clear.
	net.EnsureConnected(t.t, dave, carol)

	// Wait for Carol to report no outstanding htlcs, and also for Dav to
	// receive all the settles from Carol.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		if predErr != nil {
			return false
		}

		predErr = assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Finally, restart dave who received the settles, but was unable to
	// deliver them to Alice since they were disconnected.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Force Dave and Alice to reconnect before waiting for the htlcs to
	// clear.
	net.EnsureConnected(t.t, dave, net.Alice)

	// After reconnection succeeds, the settles should be propagated all
	// the way back to the sender. All nodes should report no active htlcs.
	err = wait.Predicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0) == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.AddInvoice(ctxt, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Before completing the final payment request, ensure that the
	// connection between Dave and Carol has been healed.
	net.EnsureConnected(t.t, dave, carol)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	err = completePaymentRequests(
		net.Bob, net.Bob.RouterClient, payReqs, true,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	closeChannelAndAssert(t, net, net.Alice, chanPointAlice, false)
	closeChannelAndAssert(t, net, dave, chanPointDave, false)
	closeChannelAndAssert(t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryOutgoingOffline constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//  1. Carol --> Dave --> Alice --> Bob  forward payment
//  2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//  3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//  4. Carol --- Dave  X         X       shutdown Bob, restart Alice
//  5. Carol <-- Dave <-- Alice  X       expect settle to propagate
func testSwitchOfflineDeliveryOutgoingOffline(
	net *lntest.NetworkHarness, t *harnessTest) {

	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	net.ConnectNodes(t.t, dave, net.Alice)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, dave)

	chanPointDave := openChannelAndAssert(
		t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol := net.NewNode(t.t, "Carol", []string{"--hodl.exit-settle"})
	net.ConnectNodes(t.t, carol, dave)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	chanPointCarol := openChannelAndAssert(
		t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := lnrpc.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			err = node.WaitForNetworkChannelOpen(chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		carol, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	err = dave.WaitForNetworkChannelOpen(chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	err = completePaymentRequests(
		net.Bob, net.Bob.RouterClient, payReqs, false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all payments to reach Carol.
	var predErr error
	err = wait.Predicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments) == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Disconnect the two intermediaries, Alice and Dave, so that when carol
	// restarts, the response will be held by Dave.
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(carolNode, 0)
		if predErr != nil {
			return false
		}

		predErr = assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Now check that the total amount was transferred from Dave to Carol.
	// The amount transferred should be exactly equal to the invoice total
	// payment amount, 5k satsohis.
	const amountPaid = int64(5000)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))

	// Shutdown carol and leave her offline for the rest of the test. This
	// is critical, as we wish to see if Dave can propragate settles even if
	// the outgoing link is never revived.
	shutdownAndAssert(net, t, carol)

	// Now restart Dave, ensuring he is both persisting the settles, and is
	// able to reforward them to Alice after recovering from a restart.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Ensure that Dave is reconnected to Alice before waiting for the
	// htlcs to clear.
	net.EnsureConnected(t.t, dave, net.Alice)

	// Since Carol has been shutdown permanently, we will wait until all
	// other nodes in the network report no active htlcs.
	nodesMinusCarol := []*lntest.HarnessNode{net.Bob, net.Alice, dave}
	err = wait.Predicate(func() bool {
		predErr = assertNumActiveHtlcs(nodesMinusCarol, 0)
		return predErr == nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point, all channels (minus Carol, who is shutdown) should
	// show a shift of 5k satoshis towards Carol.  The order of asserts
	// corresponds to increasing of time is needed to embed the HTLC in
	// commitment transaction, in channel Bob->Alice->David, order is
	// David, Alice, Bob.
	assertAmountPaid(t, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	closeChannelAndAssert(t, net, net.Alice, chanPointAlice, false)
	closeChannelAndAssert(t, net, dave, chanPointDave, false)
}

// scenarioFourNodes specifies a scenario which we have a topology that has
// four nodes and three channels.
type scenarioFourNodes struct {
	alice *node.HarnessNode
	bob   *node.HarnessNode
	carol *node.HarnessNode
	dave  *node.HarnessNode

	chanPointAliceBob  *lnrpc.ChannelPoint
	chanPointCarolDave *lnrpc.ChannelPoint
	chanPointDaveAlice *lnrpc.ChannelPoint

	cleanUp func()
}

// setupScenarioFourNodes creates a topology for switch tests. It will create
// two new nodes: Carol and Dave, such that there will be a 4 nodes, 3 channel
// topology. Dave will make a channel with Alice, and Carol with Dave. After
// this setup, the network topology should now look like:
//
//	Carol -> Dave -> Alice -> Bob
//
// Once the network is created, Carol will generate 5 invoices and Bob will pay
// them using the above path.
//
// NOTE: caller needs to call cleanUp to clean the nodes and channels created
// from this setup.
func setupScenarioFourNodes(ht *lntemp.HarnessTest) *scenarioFourNodes {
	const (
		chanAmt = btcutil.Amount(1000000)
		pushAmt = btcutil.Amount(900000)
	)

	params := lntemp.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: pushAmt,
	}

	// Grab the standby nodes.
	alice, bob := ht.Alice, ht.Bob

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 node, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave := ht.NewNode("Dave", nil)
	ht.ConnectNodes(dave, alice)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect
	// the intermediary hops before starting the settle.
	carol := ht.NewNode("Carol", []string{"--hodl.exit-settle"})
	ht.ConnectNodes(carol, dave)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Open channels in batch to save blocks mined.
	reqs := []*lntemp.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: params},
		{Local: dave, Remote: alice, Param: params},
		{Local: carol, Remote: dave, Param: params},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)

	// Wait for all nodes to have seen all channels.
	nodes := []*node.HarnessNode{alice, bob, carol, dave}
	for _, chanPoint := range resp {
		for _, node := range nodes {
			ht.AssertTopologyChannelOpen(node, chanPoint)
		}
	}

	chanPointAliceBob := resp[0]
	chanPointDaveAlice := resp[1]
	chanPointCarolDave := resp[2]

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	payReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, numPayments)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ht.CompletePaymentRequestsNoWait(bob, payReqs, chanPointAliceBob)

	// Create a cleanUp to wipe the states.
	cleanUp := func() {
		if ht.Failed() {
			ht.Skip("Skipped cleanup for failed test")
			return
		}

		ht.CloseChannel(alice, chanPointAliceBob)
		ht.CloseChannel(dave, chanPointDaveAlice)
		ht.CloseChannel(carol, chanPointCarolDave)
	}

	s := &scenarioFourNodes{
		alice, bob, carol, dave, chanPointAliceBob,
		chanPointCarolDave, chanPointDaveAlice, cleanUp,
	}

	// Wait until all nodes in the network have 5 outstanding htlcs.
	s.assertHTLCs(ht, numPayments)

	return s
}

// assertHTLCs is a helper function which asserts the desired num of
// HTLCs has been seen in the nodes.
func (s *scenarioFourNodes) assertHTLCs(ht *lntemp.HarnessTest, num int) {
	// Alice should have both the same number of outgoing and
	// incoming HTLCs.
	ht.AssertNumActiveHtlcs(s.alice, num*2)
	// Bob should have num of incoming HTLCs.
	ht.AssertNumActiveHtlcs(s.bob, num)
	// Dave should have both the same number of outgoing and
	// incoming HTLCs.
	ht.AssertNumActiveHtlcs(s.dave, num*2)
	// Carol should have the num of outgoing HTLCs.
	ht.AssertNumActiveHtlcs(s.carol, num)
}

// assertAmoutPaid is a helper method which takes a given paid amount
// and number of payments and asserts the desired payments are made in
// the four nodes.
func (s *scenarioFourNodes) assertAmoutPaid(ht *lntemp.HarnessTest,
	amt int64, num int64) {

	ht.AssertAmountPaid(
		"Dave(local) => Carol(remote)", s.carol,
		s.chanPointCarolDave, int64(0), amt,
	)
	ht.AssertAmountPaid(
		"Dave(local) => Carol(remote)", s.dave,
		s.chanPointCarolDave, amt, int64(0),
	)
	ht.AssertAmountPaid(
		"Alice(local) => Dave(remote)", s.dave,
		s.chanPointDaveAlice,
		int64(0), amt+(baseFee*num),
	)
	ht.AssertAmountPaid(
		"Alice(local) => Dave(remote)", s.alice,
		s.chanPointDaveAlice,
		amt+(baseFee*num), int64(0),
	)
	ht.AssertAmountPaid(
		"Bob(local) => Alice(remote)", s.alice,
		s.chanPointAliceBob,
		int64(0), amt+((baseFee*num)*2),
	)
	ht.AssertAmountPaid(
		"Bob(local) => Alice(remote)", s.bob,
		s.chanPointAliceBob,
		amt+(baseFee*num)*2, int64(0),
	)
}
