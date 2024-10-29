package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testResHandoff tests that the contractcourt is able to properly hand-off
// resolution messages to the switch.
func testResHandoff(ht *lntest.HarnessTest) {
	const (
		chanAmt    = btcutil.Amount(1000000)
		paymentAmt = 50000
	)

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)

	// First we'll create a channel between Alice and Bob.
	ht.EnsureConnected(alice, bob)

	params := lntest.OpenChannelParams{Amt: chanAmt}
	chanPointAlice := ht.OpenChannel(alice, bob, params)

	// Create a new node Carol that will be in hodl mode. This is used to
	// trigger the behavior of checkRemoteDanglingActions in the
	// contractcourt. This will cause Bob to fail the HTLC back to Alice.
	carol := ht.NewNode("Carol", []string{"--hodl.commit"})

	// Connect Bob to Carol.
	ht.ConnectNodes(bob, carol)

	// Open a channel between Bob and Carol.
	chanPointCarol := ht.OpenChannel(bob, carol, params)

	// Wait for Alice to see the channel edge in the graph.
	ht.AssertChannelInGraph(alice, chanPointCarol)

	// We'll create an invoice for Carol that Alice will attempt to pay.
	// Since Carol is in hodl.commit mode, she won't send back any commit
	// sigs.
	carolPayReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, 1)

	// Alice will now attempt to fulfill the invoice.
	ht.CompletePaymentRequestsNoWait(alice, carolPayReqs, chanPointAlice)

	// Wait until Carol has received the Add, CommitSig from Bob, and has
	// responded with a RevokeAndAck. We expect NumUpdates to be 1 meaning
	// Carol's CommitHeight is 1.
	ht.AssertChannelCommitHeight(carol, chanPointCarol, 1)

	// Before we shutdown Alice, we'll assert that she only has 1 update.
	ht.AssertChannelCommitHeight(alice, chanPointAlice, 1)

	// We'll shutdown Alice so that Bob can't connect to her.
	restartAlice := ht.SuspendNode(alice)

	// Bob will now force close his channel with Carol such that resolution
	// messages are created and forwarded backwards to Alice.
	ht.CloseChannelAssertPending(bob, chanPointCarol, true)

	// The channel should be listed in the PendingChannels result.
	ht.AssertNumWaitingClose(bob, 1)

	// Mine a block to confirm the closing tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We sleep here so we can be sure that the hand-off has occurred from
	// Bob's contractcourt to Bob's htlcswitch. This sleep could be removed
	// if there was some feedback (i.e. API in switch) that allowed for
	// querying the state of resolution messages.
	time.Sleep(10 * time.Second)

	// Mine blocks until Bob has no waiting close channels. This tests that
	// the circuit-deletion logic is skipped if a resolution message
	// exists.
	ht.CleanupForceClose(bob)

	// We will now restart Bob so that we can test whether the resolution
	// messages are re-forwarded on start-up.
	ht.RestartNode(bob)

	// We'll now also restart Alice and connect her with Bob.
	require.NoError(ht, restartAlice())

	ht.EnsureConnected(alice, bob)

	// We'll assert that Alice has received the failure resolution message.
	ht.AssertChannelCommitHeight(alice, chanPointAlice, 2)

	// Assert that Alice's payment failed.
	ht.AssertFirstHTLCError(alice, lnrpc.Failure_PERMANENT_CHANNEL_FAILURE)
}
