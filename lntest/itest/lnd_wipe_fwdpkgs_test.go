package itest

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

type pendingChan *lnrpc.PendingChannelsResponse_PendingChannel

// testWipeForwardingPackagesLocal tests that when a channel is closed, either
// through local force close, remote close, or coop close, all the forwarding
// packages of that particular channel are deleted. The test creates a
// topology as Alice -> Bob -> Carol, and sends payments from Alice to Carol
// through Bob. It then performs the test in two steps,
// - Bob force closes the channel Alice->Bob, and checks from both Bob's
//   PoV(local force close) and Alice's Pov(remote close) that the forwarding
//   packages are wiped.
// - Bob coop closes the channel Bob->Carol, and checks from both Bob PoVs that
//   the forwarding packages are wiped.
func testWipeForwardingPackages(net *lntest.NetworkHarness,
	t *harnessTest) {

	// Setup the test and get the channel points.
	pointAB, pointBC, carol, cleanUp := setupFwdPkgTest(net, t)
	defer cleanUp()

	// Firstly, Bob force closes the channel.
	_, _, err := net.CloseChannel(net.Bob, pointAB, true)
	require.NoError(t.t, err, "unable to force close channel")

	// Now that the channel has been force closed, it should show up in
	// bob's PendingChannels RPC under the waiting close section.
	pendingChan := assertWaitingCloseChannel(t.t, net.Bob)

	// Check that Bob has created forwarding packages. We don't care the
	// exact number here as long as these packages are deleted when the
	// channel is closed.
	require.NotZero(t.t, pendingChan.NumForwardingPackages)

	// Mine 1 block to get the closing transaction confirmed.
	_, err = net.Miner.Client.Generate(1)
	require.NoError(t.t, err, "unable to mine blocks")

	// Now that the closing transaction is confirmed, the above waiting
	// close channel should now become pending force closed channel.
	pendingChan = assertPendingForceClosedChannel(t.t, net.Bob)

	// Check the forwarding pacakges are deleted.
	require.Zero(t.t, pendingChan.NumForwardingPackages)

	// For Alice, the forwarding packages should have been wiped too.
	pendingChanAlice := assertPendingForceClosedChannel(t.t, net.Alice)
	require.Zero(t.t, pendingChanAlice.NumForwardingPackages)

	// Secondly, Bob coop closes the channel.
	_, _, err = net.CloseChannel(net.Bob, pointBC, false)
	require.NoError(t.t, err, "unable to coop close channel")

	// Now that the channel has been coop closed, it should show up in
	// bob's PendingChannels RPC under the waiting close section.
	pendingChan = assertWaitingCloseChannel(t.t, net.Bob)

	// Check that Bob has created forwarding packages. We don't care the
	// exact number here as long as these packages are deleted when the
	// channel is closed.
	require.NotZero(t.t, pendingChan.NumForwardingPackages)

	// Since it's a coop close, Carol should see the waiting close channel
	// too.
	pendingChanCarol := assertWaitingCloseChannel(t.t, carol)
	require.NotZero(t.t, pendingChanCarol.NumForwardingPackages)

	// Mine 1 block to get the closing transaction confirmed.
	_, err = net.Miner.Client.Generate(1)
	require.NoError(t.t, err, "unable to mine blocks")

	// Now that the closing transaction is confirmed, the above waiting
	// close channel should now become pending closed channel. Note that
	// the name PendingForceClosingChannels is a bit confusing, what it
	// really contains is channels whose closing tx has been broadcast.
	pendingChan = assertPendingForceClosedChannel(t.t, net.Bob)

	// Check the forwarding pacakges are deleted.
	require.Zero(t.t, pendingChan.NumForwardingPackages)

	// Mine a block to confirm sweep transactions such that they
	// don't remain in the mempool for any subsequent tests.
	_, err = net.Miner.Client.Generate(1)
	require.NoError(t.t, err, "unable to mine blocks")
}

// setupFwdPkgTest prepares the wipe forwarding packages tests. It creates a
// network topology that has a channel direction: Alice -> Bob -> Carol, sends
// several payments from Alice to Carol, and returns the two channel points(one
// for Alice and Bob, the other for Bob and Carol), the node Carol, and a
// cleanup function to be used when the test finishes.
func setupFwdPkgTest(net *lntest.NetworkHarness,
	t *harnessTest) (*lnrpc.ChannelPoint, *lnrpc.ChannelPoint,
	*lntest.HarnessNode, func()) {

	ctxb := context.Background()

	const (
		chanAmt        = 10e6
		paymentAmt     = 10e4
		finalCTLVDelta = chainreg.DefaultBitcoinTimeLockDelta
		numInvoices    = 3
	)

	// Grab Alice and Bob from harness net.
	alice, bob := net.Alice, net.Bob

	// Create a new node Carol, which will create invoices that require
	// Alice to pay.
	carol := net.NewNode(t.t, "Carol", nil)

	// Connect Bob to Carol.
	net.ConnectNodes(t.t, bob, carol)

	// Open a channel between Alice and Bob.
	chanPointAB := openChannelAndAssert(
		t, net, alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Open a channel between Bob and Carol.
	chanPointBC := openChannelAndAssert(
		t, net, bob, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Alice sends several payments to Carol through Bob, which triggers
	// Bob to create forwarding packages.
	for i := 0; i < numInvoices; i++ {
		// Add an invoice for Carol.
		invoice := &lnrpc.Invoice{Memo: "testing", Value: paymentAmt}
		invoiceResp, err := carol.AddInvoice(ctxt, invoice)
		require.NoError(t.t, err, "unable to add invoice")

		// Alice sends a payment to Carol through Bob.
		sendAndAssertSuccess(
			t, net.Alice, &routerrpc.SendPaymentRequest{
				PaymentRequest: invoiceResp.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitSat:    noFeeLimitMsat,
			},
		)
	}

	return chanPointAB, chanPointBC, carol, func() {
		shutdownAndAssert(net, t, alice)
		shutdownAndAssert(net, t, bob)
		shutdownAndAssert(net, t, carol)
	}
}

// assertWaitingCloseChannel checks there is a single channel that is waiting
// for close and returns the channel found.
func assertWaitingCloseChannel(t *testing.T,
	node *lntest.HarnessNode) pendingChan {

	ctxb := context.Background()

	var channel pendingChan
	require.Eventually(t, func() bool {
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()

		req := &lnrpc.PendingChannelsRequest{}
		resp, err := node.PendingChannels(ctxt, req)

		// We require the RPC call to be succeeded and won't retry upon
		// an error.
		require.NoError(t, err, "unable to query for pending channels")

		if err := checkNumWaitingCloseChannels(resp, 1); err != nil {
			return false
		}

		channel = resp.WaitingCloseChannels[0].Channel
		return true
	}, defaultTimeout, 200*time.Millisecond)

	return channel
}

// assertForceClosedChannel checks there is a single channel that is pending
// force closed and returns the channel found.
func assertPendingForceClosedChannel(t *testing.T,
	node *lntest.HarnessNode) pendingChan {

	ctxb := context.Background()

	var channel pendingChan
	require.Eventually(t, func() bool {
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()

		req := &lnrpc.PendingChannelsRequest{}
		resp, err := node.PendingChannels(ctxt, req)

		// We require the RPC call to be succeeded and won't retry upon
		// an error.
		require.NoError(t, err, "unable to query for pending channels")

		if err := checkNumForceClosedChannels(resp, 1); err != nil {
			return false
		}

		channel = resp.PendingForceClosingChannels[0].Channel
		return true
	}, defaultTimeout, 200*time.Millisecond)

	return channel
}
