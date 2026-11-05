//go:build integration

package itest

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// testLinkFailureForceClose verifies that overlapping cooperative-close and
// link-failure handling does not prevent a second channel from going to chain
// at an outgoing HTLC's deadline.
func testLinkFailureForceClose(ht *lntest.HarnessTest) {
	const (
		chanAmt   = btcutil.Amount(1_000_000)
		htlcAmt   = btcutil.Amount(100_000)
		cltvDelta = routing.MinCLTVDelta
	)

	mallory := ht.NewNode("Mallory", []string{
		"--hodl.exit-settle",
		fmt.Sprintf("--protocol.custom-message=%d", lnwire.MsgShutdown),
		fmt.Sprintf(
			"--protocol.custom-message=%d",
			lnwire.MsgUpdateFulfillHTLC,
		),
	})
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)

	ht.EnsureConnected(bob, alice)
	ht.EnsureConnected(alice, mallory)

	params := lntest.OpenChannelParams{Amt: chanAmt}
	bobAliceChan := ht.OpenChannel(bob, alice, params)
	htlcChan := ht.OpenChannel(alice, mallory, params)
	ht.AssertChannelInGraph(bob, htlcChan)

	preimage := ht.RandomPreimage()
	payHash := preimage.Hash()
	ht.SendPaymentAssertInflight(bob, &routerrpc.SendPaymentRequest{
		Dest:           mallory.PubKey[:],
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash[:],
		FinalCltvDelta: cltvDelta,
		FeeLimitMsat:   noFeeLimitMsat,
	})
	ht.AssertIncomingHTLCActive(alice, bobAliceChan, payHash[:])
	htlcOut := ht.AssertOutgoingHTLCActive(alice, htlcChan, payHash[:])

	// Open the channel used for close negotiation after the payment so it
	// cannot be selected for forwarding.
	wedgeChan := ht.OpenChannel(alice, mallory, params)
	wedgeID := lnwire.NewChanIDFromOutPoint(
		ht.OutPointFromChannelPoint(wedgeChan),
	)
	htlcPoint := ht.OutPointFromChannelPoint(htlcChan)
	htlcID := lnwire.NewChanIDFromOutPoint(htlcPoint)

	deliveryScript := append(
		[]byte{0x00, 0x14}, ht.Random32Bytes()[:20]...,
	)
	sendOverrideMsg(
		ht, mallory, alice, lnwire.NewShutdown(wedgeID, deliveryScript),
	)
	sendOverrideMsg(ht, mallory, alice, &lnwire.UpdateFulfillHTLC{
		ChanID: wedgeID,
		ID:     1000,
	})

	// Fail the link carrying the held HTLC with an unknown ID. This
	// keeps the failure action at ForceNone, leaving the arbitrator to
	// force close at the outgoing deadline.
	sendOverrideMsg(ht, mallory, alice, &lnwire.UpdateFulfillHTLC{
		ChanID: htlcID,
		ID:     1000,
	})

	expiry := htlcOut.ExpirationHeight
	currentHeight := ht.CurrentHeight()
	require.Greater(ht, expiry, currentHeight)
	ht.MineEmptyBlocks(int(expiry - currentHeight - 1))

	// The expiry block drives Alice's arbitrator to chain. Verify that the
	// resulting transaction spends the channel funding output, rather than
	// accepting an unrelated transaction from the mempool.
	ht.MineEmptyBlocks(1)
	closeTx := ht.GetNumTxsFromMempool(1)[0]
	require.Equal(ht, htlcPoint, closeTx.TxIn[0].PreviousOutPoint)
	ht.AssertConnected(alice, mallory)

	// Confirm the commitment. Once that confirmation reaches the
	// sweeper, it must publish the outgoing HTLC timeout transaction.
	// Checking the
	// child's input makes this an explicit sweep/publication assertion,
	// rather than only observing that Alice learned the next chain
	// height.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	htlcTimeoutTx := ht.GetNumTxsFromMempool(1)[0]
	require.Equal(
		ht, closeTx.TxHash(),
		htlcTimeoutTx.TxIn[0].PreviousOutPoint.Hash,
	)

	// Confirm the timeout transaction, then prove the dispatcher continues
	// through another blockbeat.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.MineEmptyBlocks(1)
	ht.WaitForNodeBlockHeight(alice, int32(ht.CurrentHeight()))
}

// sendOverrideMsg encodes a standard wire message as a custom-message payload.
// The sending node must enable an override for the message type.
func sendOverrideMsg(ht *lntest.HarnessTest, from, to *node.HarnessNode,
	msg lnwire.Message) {

	var payload bytes.Buffer
	require.NoError(ht, msg.Encode(&payload, 0))

	from.RPC.SendCustomMessage(&lnrpc.SendCustomMessageRequest{
		Peer: to.PubKey[:],
		Type: uint32(msg.MsgType()),
		Data: payload.Bytes(),
	})
}
