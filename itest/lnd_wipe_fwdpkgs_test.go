package itest

import (
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testWipeForwardingPackagesLocal tests that when a channel is closed, either
// through local force close, remote close, or coop close, all the forwarding
// packages of that particular channel are deleted. The test creates a
// topology as Alice -> Bob -> Carol, and sends payments from Alice to Carol
// through Bob. It then performs the test in two steps,
//   - Bob force closes the channel Alice->Bob, and checks from both Bob's
//     PoV(local force close) and Alice's Pov(remote close) that the forwarding
//     packages are wiped.
//   - Bob coop closes the channel Bob->Carol, and checks from both Bob PoVs
//     that the forwarding packages are wiped.
func testWipeForwardingPackages(ht *lntest.HarnessTest) {
	const (
		chanAmt        = 10e6
		paymentAmt     = 10e4
		finalCTLVDelta = chainreg.DefaultBitcoinTimeLockDelta
		numInvoices    = 3
	)

	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil},
		lntest.OpenChannelParams{Amt: chanAmt},
	)
	chanPointAB, chanPointBC := chanPoints[0], chanPoints[1]
	alice, bob, carol := nodes[0], nodes[1], nodes[2]

	// Before we continue, make sure Alice has seen the channel between Bob
	// and Carol.
	ht.AssertChannelInGraph(alice, chanPointBC)

	// Alice sends several payments to Carol through Bob, which triggers
	// Bob to create forwarding packages.
	for i := 0; i < numInvoices; i++ {
		// Add an invoice for Carol.
		invoice := &lnrpc.Invoice{Memo: "testing", Value: paymentAmt}
		resp := carol.RPC.AddInvoice(invoice)

		// Alice sends a payment to Carol through Bob.
		ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})
	}

	flakePaymentStreamReturnEarly()

	// Firstly, Bob force closes the channel.
	ht.CloseChannelAssertPending(bob, chanPointAB, true)

	// Now that the channel has been force closed, it should show up in
	// bob's PendingChannels RPC under the waiting close section.
	pendingAB := ht.AssertChannelWaitingClose(bob, chanPointAB).Channel

	// Check that Bob has created forwarding packages. We don't care the
	// exact number here as long as these packages are deleted when the
	// channel is closed.
	require.NotZero(ht, pendingAB.NumForwardingPackages)

	// Secondly, Bob coop closes the channel.
	ht.CloseChannelAssertPending(bob, chanPointBC, false)

	// Now that the channel has been coop closed, it should show up in
	// bob's PendingChannels RPC under the waiting close section.
	pendingBC := ht.AssertChannelWaitingClose(bob, chanPointBC).Channel

	// Check that Bob has created forwarding packages. We don't care the
	// exact number here as long as these packages are deleted when the
	// channel is closed.
	require.NotZero(ht, pendingBC.NumForwardingPackages)

	// Since it's a coop close, Carol should see the waiting close channel
	// too.
	pendingBC = ht.AssertChannelWaitingClose(carol, chanPointBC).Channel
	require.NotZero(ht, pendingBC.NumForwardingPackages)

	// Mine 1 block to get the two closing transactions confirmed.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Now that the closing transaction is confirmed, the above waiting
	// close channel should now become pending force closed channel.
	pendingAB = ht.AssertChannelPendingForceClose(bob, chanPointAB).Channel

	// Check the forwarding pacakges are deleted.
	require.Zero(ht, pendingAB.NumForwardingPackages)

	// For Alice, the forwarding packages should have been wiped too.
	pending := ht.AssertChannelPendingForceClose(alice, chanPointAB)
	pendingAB = pending.Channel
	require.Zero(ht, pendingAB.NumForwardingPackages)

	// Alice should one pending sweep.
	ht.AssertNumPendingSweeps(alice, 1)

	// Mine 1 block to get Alice's sweeping tx confirmed.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}
