package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

// testResHandoff tests that the contractcourt is able to properly hand-off
// resolution messages to the switch.
func testResHandoff(net *lntest.NetworkHarness, t *harnessTest) {
	const (
		chanAmt    = btcutil.Amount(1000000)
		paymentAmt = 50000
	)

	ctxb := context.Background()

	// First we'll create a channel between Alice and Bob.
	net.EnsureConnected(t.t, net.Alice, net.Bob)

	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, chanPointAlice, false)

	// Wait for Alice and Bob to receive the channel edge from the funding
	// manager.
	err := net.Alice.WaitForNetworkChannelOpen(chanPointAlice)
	require.NoError(t.t, err)

	err = net.Bob.WaitForNetworkChannelOpen(chanPointAlice)
	require.NoError(t.t, err)

	// Create a new node Carol that will be in hodl mode. This is used to
	// trigger the behavior of checkRemoteDanglingActions in the
	// contractcourt. This will cause Bob to fail the HTLC back to Alice.
	carol := net.NewNode(t.t, "Carol", []string{"--hodl.commit"})
	defer shutdownAndAssert(net, t, carol)

	// Connect Bob to Carol.
	net.ConnectNodes(t.t, net.Bob, carol)

	// Open a channel between Bob and Carol.
	chanPointCarol := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for Bob and Carol to receive the channel edge from the funding
	// manager.
	err = net.Bob.WaitForNetworkChannelOpen(chanPointCarol)
	require.NoError(t.t, err)

	err = carol.WaitForNetworkChannelOpen(chanPointCarol)
	require.NoError(t.t, err)

	// Wait for Alice to see the channel edge in the graph.
	err = net.Alice.WaitForNetworkChannelOpen(chanPointCarol)
	require.NoError(t.t, err)

	// We'll create an invoice for Carol that Alice will attempt to pay.
	// Since Carol is in hodl.commit mode, she won't send back any commit
	// sigs.
	carolPayReqs, _, _, err := createPayReqs(
		carol, paymentAmt, 1,
	)
	require.NoError(t.t, err)

	// Alice will now attempt to fulfill the invoice.
	err = completePaymentRequests(
		net.Alice, net.Alice.RouterClient, carolPayReqs, false,
	)
	require.NoError(t.t, err)

	// Wait until Carol has received the Add, CommitSig from Bob, and has
	// responded with a RevokeAndAck. We expect NumUpdates to be 1 meaning
	// Carol's CommitHeight is 1.
	err = wait.Predicate(func() bool {
		carolInfo, err := getChanInfo(carol)
		if err != nil {
			return false
		}

		return carolInfo.NumUpdates == 1
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Before we shutdown Alice, we'll assert that she only has 1 update.
	err = wait.Predicate(func() bool {
		aliceInfo, err := getChanInfo(net.Alice)
		if err != nil {
			return false
		}

		return aliceInfo.NumUpdates == 1
	}, defaultTimeout)
	require.NoError(t.t, err)

	// We'll shutdown Alice so that Bob can't connect to her.
	restartAlice, err := net.SuspendNode(net.Alice)
	require.NoError(t.t, err)

	// Bob will now force close his channel with Carol such that resolution
	// messages are created and forwarded backwards to Alice.
	_, _, err = net.CloseChannel(net.Bob, chanPointCarol, true)
	require.NoError(t.t, err)

	// The channel should be listed in the PendingChannels result.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := net.Bob.PendingChannels(
		ctxt, pendingChansRequest,
	)
	require.NoError(t.t, err)
	require.NoError(t.t, checkNumWaitingCloseChannels(pendingChanResp, 1))

	// We'll mine a block to confirm the force close transaction and to
	// advance Bob's contract state with Carol to StateContractClosed.
	mineBlocks(t, net, 1, 1)

	// We sleep here so we can be sure that the hand-off has occurred from
	// Bob's contractcourt to Bob's htlcswitch. This sleep could be removed
	// if there was some feedback (i.e. API in switch) that allowed for
	// querying the state of resolution messages.
	time.Sleep(10 * time.Second)

	// Mine blocks until Bob has no waiting close channels. This tests
	// that the circuit-deletion logic is skipped if a resolution message
	// exists.
	for {
		_, err = net.Miner.Client.Generate(1)
		require.NoError(t.t, err)

		pendingChanResp, err = net.Bob.PendingChannels(
			ctxt, pendingChansRequest,
		)
		require.NoError(t.t, err)

		isErr := checkNumForceClosedChannels(pendingChanResp, 0)
		if isErr == nil {
			break
		}

		time.Sleep(150 * time.Millisecond)
	}

	// We will now restart Bob so that we can test whether the resolution
	// messages are re-forwarded on start-up.
	restartBob, err := net.SuspendNode(net.Bob)
	require.NoError(t.t, err)

	err = restartBob()
	require.NoError(t.t, err)

	// We'll now also restart Alice and connect her with Bob.
	err = restartAlice()
	require.NoError(t.t, err)

	net.EnsureConnected(t.t, net.Alice, net.Bob)

	// We'll assert that Alice has received the failure resolution
	// message.
	err = wait.Predicate(func() bool {
		aliceInfo, err := getChanInfo(net.Alice)
		if err != nil {
			return false
		}

		return aliceInfo.NumUpdates == 2
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Assert that Alice's payment failed.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	paymentsResp, err := net.Alice.ListPayments(
		ctxt, &lnrpc.ListPaymentsRequest{
			IncludeIncomplete: true,
		},
	)
	require.NoError(t.t, err)
	require.Equal(t.t, 1, len(paymentsResp.Payments))

	htlcs := paymentsResp.Payments[0].Htlcs

	require.Equal(t.t, 1, len(htlcs))
	require.Equal(t.t, lnrpc.HTLCAttempt_FAILED, htlcs[0].Status)
}
