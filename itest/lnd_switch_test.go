package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
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
//  2. X        X         X  Bob  restart sender and intermediaries
//  3. Carol <-- Dave <-- Alice <-- Bob  expect settle to propagate
//
//nolint:dupword
func testSwitchCircuitPersistence(ht *lntest.HarnessTest) {
	// Setup our test scenario. We should now have four nodes running with
	// three channels.
	s := setupScenarioFourNodes(ht)

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
	s.assertAmountPaid(ht, amountPaid, numPayments)

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
	s.assertAmountPaid(ht, amountPaid, numPayments+1)
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
func testSwitchOfflineDelivery(ht *lntest.HarnessTest) {
	// Setup our test scenario. We should now have four nodes running with
	// three channels.
	s := setupScenarioFourNodes(ht)

	// First, disconnect Dave and Alice so that their link is broken.
	ht.DisconnectNodes(s.dave, s.alice)

	// Then, reconnect them to ensure Dave doesn't just fail back the htlc.
	ht.EnsureConnected(s.dave, s.alice)

	// Wait to ensure that the payment remain are not failed back after
	// reconnecting. All node should report the number payments initiated
	// for the duration of the interval.
	s.assertHTLCs(ht, numPayments)

	// Now, disconnect Dave from Alice again before settling back the
	// payment.
	ht.DisconnectNodes(s.dave, s.alice)

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	s.carol.SetExtraArgs(nil)
	ht.RestartNode(s.carol)

	// Wait for Carol to report no outstanding htlcs.
	ht.AssertNumActiveHtlcs(s.carol, 0)

	// Now that the settles have reached Dave, reconnect him with Alice,
	// allowing the settles to return to the sender.
	ht.EnsureConnected(s.dave, s.alice)

	// Wait until all outstanding htlcs in the network have been settled.
	s.assertHTLCs(ht, 0)

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	s.assertAmountPaid(ht, amountPaid, numPayments)

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
	s.assertAmountPaid(ht, amountPaid, numPayments+1)
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
//
//nolint:dupword
func testSwitchOfflineDeliveryPersistence(ht *lntest.HarnessTest) {
	// Setup our test scenario. We should now have four nodes running with
	// three channels.
	s := setupScenarioFourNodes(ht)

	// Disconnect the two intermediaries, Alice and Dave, by shutting down
	// Alice.
	restartAlice := ht.SuspendNode(s.alice)

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	s.carol.SetExtraArgs(nil)
	ht.RestartNode(s.carol)

	// Make Carol and Dave are reconnected before waiting for the htlcs to
	// clear.
	ht.EnsureConnected(s.dave, s.carol)

	// Wait for Carol to report no outstanding htlcs, and also for Dave to
	// receive all the settles from Carol.
	ht.AssertNumActiveHtlcs(s.carol, 0)
	// As an intermediate node, Dave should now have zero outgoing HTLCs
	// and 5 incoming HTLCs from Alice.
	ht.AssertNumActiveHtlcs(s.dave, numPayments)

	// Finally, restart dave who received the settles, but was unable to
	// deliver them to Alice since they were disconnected.
	ht.RestartNode(s.dave)
	require.NoError(ht, restartAlice(), "restart alice failed")

	// Force Dave and Alice to reconnect before waiting for the htlcs to
	// clear.
	ht.EnsureConnected(s.dave, s.alice)

	// After reconnection succeeds, the settles should be propagated all
	// the way back to the sender. All nodes should report no active htlcs.
	s.assertHTLCs(ht, 0)

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	s.assertAmountPaid(ht, amountPaid, numPayments)

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp := s.carol.RPC.AddInvoice(finalInvoice)
	payReqs := []string{resp.PaymentRequest}

	// Before completing the final payment request, ensure that the
	// connection between Dave and Carol has been healed.
	ht.EnsureConnected(s.dave, s.carol)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ht.CompletePaymentRequests(s.bob, payReqs)

	amountPaid = int64(6000)
	s.assertAmountPaid(ht, amountPaid, numPayments+1)
}

// testSwitchOfflineDeliveryOutgoingOffline constructs a set of multihop
// payments, and tests that the returning payments are not lost if a peer on
// the backwards path is offline when the settle/fails are received AND the
// peer buffering the responses is completely restarts. We expect the payments
// to be reloaded from disk, and transmitted as soon as the intermediaries are
// reconnected.
//
// The general flow of this test:
//  1. Carol --> Dave --> Alice --> Bob  forward payment
//  2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//  3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//  4. Carol --- Dave  X         X       shutdown Bob, restart Alice
//  5. Carol <-- Dave <-- Alice  X       expect settle to propagate
//
//nolint:dupword
func testSwitchOfflineDeliveryOutgoingOffline(ht *lntest.HarnessTest) {
	// Setup our test scenario. We should now have four nodes running with
	// three channels. Note that we won't call the cleanUp function here as
	// we will manually stop the node Carol and her channel.
	s := setupScenarioFourNodes(ht)

	// Disconnect the two intermediaries, Alice and Dave, so that when carol
	// restarts, the response will be held by Dave.
	restartAlice := ht.SuspendNode(s.alice)

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	s.carol.SetExtraArgs(nil)
	ht.RestartNode(s.carol)

	// Wait for Carol to report no outstanding htlcs.
	ht.AssertNumActiveHtlcs(s.carol, 0)
	// As an intermediate node, Dave should now have zero outgoing HTLCs
	// and 5 incoming HTLCs from Alice.
	ht.AssertNumActiveHtlcs(s.dave, numPayments)

	// Now check that the total amount was transferred from Dave to Carol.
	// The amount transferred should be exactly equal to the invoice total
	// payment amount, 5k satsohis.
	const amountPaid = int64(5000)
	ht.AssertAmountPaid(
		"Dave(local) => Carol(remote)", s.carol,
		s.chanPointCarolDave, int64(0), amountPaid,
	)
	ht.AssertAmountPaid(
		"Dave(local) => Carol(remote)", s.dave,
		s.chanPointCarolDave, amountPaid, int64(0),
	)

	// Shutdown carol and leave her offline for the rest of the test. This
	// is critical, as we wish to see if Dave can propragate settles even if
	// the outgoing link is never revived.
	restartCarol := ht.SuspendNode(s.carol)

	// Now restart Dave, ensuring he is both persisting the settles, and is
	// able to reforward them to Alice after recovering from a restart.
	ht.RestartNode(s.dave)
	require.NoErrorf(ht, restartAlice(), "restart alice failed")

	// Ensure that Dave is reconnected to Alice before waiting for the
	// htlcs to clear.
	ht.EnsureConnected(s.dave, s.alice)

	// Since Carol has been shutdown permanently, we will wait until all
	// other nodes in the network report no active htlcs.
	ht.AssertNumActiveHtlcs(s.alice, 0)
	ht.AssertNumActiveHtlcs(s.bob, 0)
	ht.AssertNumActiveHtlcs(s.dave, 0)

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.

	// At this point, all channels (minus Carol, who is shutdown) should
	// show a shift of 5k satoshis towards Carol.  The order of asserts
	// corresponds to increasing of time is needed to embed the HTLC in
	// commitment transaction, in channel Bob->Alice->David, order is
	// David, Alice, Bob.
	ht.AssertAmountPaid(
		"Alice(local) => Dave(remote)", s.dave, s.chanPointDaveAlice,
		int64(0), amountPaid+(baseFee*numPayments),
	)
	ht.AssertAmountPaid(
		"Alice(local) => Dave(remote)", s.alice, s.chanPointDaveAlice,
		amountPaid+(baseFee*numPayments), int64(0),
	)
	ht.AssertAmountPaid(
		"Bob(local) => Alice(remote)", s.alice, s.chanPointAliceBob,
		int64(0), amountPaid+((baseFee*numPayments)*2),
	)
	ht.AssertAmountPaid(
		"Bob(local) => Alice(remote)", s.bob, s.chanPointAliceBob,
		amountPaid+(baseFee*numPayments)*2, int64(0),
	)

	// Finally, restart Carol so the cleanup process can be finished.
	require.NoError(ht, restartCarol())
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
func setupScenarioFourNodes(ht *lntest.HarnessTest) *scenarioFourNodes {
	const (
		chanAmt = btcutil.Amount(1000000)
		pushAmt = btcutil.Amount(900000)
	)

	params := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: pushAmt,
	}

	// Grab the standby nodes.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("bob", nil)
	ht.ConnectNodes(alice, bob)

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
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: params},
		{Local: dave, Remote: alice, Param: params},
		{Local: carol, Remote: dave, Param: params},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)

	// Wait for all nodes to have seen all channels.
	nodes := []*node.HarnessNode{alice, bob, carol, dave}
	for _, chanPoint := range resp {
		for _, node := range nodes {
			ht.AssertChannelInGraph(node, chanPoint)
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

	s := &scenarioFourNodes{
		alice, bob, carol, dave, chanPointAliceBob,
		chanPointCarolDave, chanPointDaveAlice,
	}

	// Wait until all nodes in the network have 5 outstanding htlcs.
	s.assertHTLCs(ht, numPayments)

	return s
}

// assertHTLCs is a helper function which asserts the desired num of
// HTLCs has been seen in the nodes.
func (s *scenarioFourNodes) assertHTLCs(ht *lntest.HarnessTest, num int) {
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

// assertAmountPaid is a helper method which takes a given paid amount
// and number of payments and asserts the desired payments are made in
// the four nodes.
func (s *scenarioFourNodes) assertAmountPaid(ht *lntest.HarnessTest,
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
