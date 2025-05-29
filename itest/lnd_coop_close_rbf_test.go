package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

func testCoopCloseRbf(ht *lntest.HarnessTest) {
	rbfCoopFlags := []string{"--protocol.rbf-coop-close"}

	// Set the fee estimate to 1sat/vbyte. This ensures that our manually
	// initiated RBF attempts will always be successful.
	ht.SetFeeEstimate(250)
	ht.SetFeeEstimateWithConf(250, 6)

	// To kick things off, we'll create two new nodes, then fund them with
	// enough coins to make a 50/50 channel.
	cfgs := [][]string{rbfCoopFlags, rbfCoopFlags}
	params := lntest.OpenChannelParams{
		Amt:     btcutil.Amount(1000000),
		PushAmt: btcutil.Amount(1000000 / 2),
	}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob := nodes[0], nodes[1]
	chanPoint := chanPoints[0]

	// Now that both sides are active with a funded channel, we can kick
	// off the test.
	//
	// To start, we'll have Alice try to close the channel, with a fee rate
	// of 5 sat/byte.
	aliceFeeRate := chainfee.SatPerVByte(5)
	aliceCloseStream, aliceCloseUpdate := ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Confirm that this new update was at 5 sat/vb.
	alicePendingUpdate := aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, int64(aliceFeeRate), alicePendingUpdate.FeePerVbyte,
	)
	require.True(ht, alicePendingUpdate.LocalCloseTx)

	// Now, we'll have Bob attempt to RBF the close transaction with a
	// higher fee rate, double that of Alice's.
	bobFeeRate := aliceFeeRate * 2
	bobCloseStream, bobCloseUpdate := ht.CloseChannelAssertPending(
		bob, chanPoint, false, lntest.WithCoopCloseFeeRate(bobFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Confirm that this new update was at 10 sat/vb.
	bobPendingUpdate := bobCloseUpdate.GetClosePending()
	require.NotNil(ht, bobCloseUpdate)
	require.Equal(ht, bobPendingUpdate.FeePerVbyte, int64(bobFeeRate))
	require.True(ht, bobPendingUpdate.LocalCloseTx)

	var err error

	// Alice should've also received a similar update that Bob has
	// increased the closing fee rate to 10 sat/vb with his settled funds.
	aliceCloseUpdate, err = ht.ReceiveCloseChannelUpdate(aliceCloseStream)
	require.NoError(ht, err)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(ht, alicePendingUpdate.FeePerVbyte, int64(bobFeeRate))
	require.False(ht, alicePendingUpdate.LocalCloseTx)

	// We'll now attempt to make a fee update that increases Alice's fee
	// rate by 6 sat/vb, which should be rejected as it is too small of an
	// increase for the RBF rules. The RPC API however will return the new
	// fee. We'll skip the mempool check here as it won't make it in.
	aliceRejectedFeeRate := aliceFeeRate + 1
	_, aliceCloseUpdate = ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceRejectedFeeRate),
		lntest.WithLocalTxNotify(), lntest.WithSkipMempoolCheck(),
	)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, alicePendingUpdate.FeePerVbyte,
		int64(aliceRejectedFeeRate),
	)
	require.True(ht, alicePendingUpdate.LocalCloseTx)

	_, err = ht.ReceiveCloseChannelUpdate(bobCloseStream)
	require.NoError(ht, err)

	// We'll now attempt a fee update that we can't actually pay for. This
	// will actually show up as an error to the remote party.
	aliceRejectedFeeRate = 100_000
	_, _ = ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceRejectedFeeRate),
		lntest.WithLocalTxNotify(),
		lntest.WithExpectedErrString("cannot pay for fee"),
	)

	// At this point, we'll have Alice+Bob reconnect so we can ensure that
	// we can continue to do RBF bumps even after a reconnection.
	ht.DisconnectNodes(alice, bob)
	ht.ConnectNodes(alice, bob)

	// Next, we'll have Alice double that fee rate again to 20 sat/vb.
	aliceFeeRate = bobFeeRate * 2
	aliceCloseStream, aliceCloseUpdate = ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
		lntest.WithLocalTxNotify(),
	)

	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, alicePendingUpdate.FeePerVbyte, int64(aliceFeeRate),
	)
	require.True(ht, alicePendingUpdate.LocalCloseTx)

	// To conclude, we'll mine a block which should now confirm Alice's
	// version of the coop close transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// Both Alice and Bob should trigger a final close update to signal the
	// closing transaction has confirmed.
	aliceClosingTxid := ht.WaitForChannelCloseEvent(aliceCloseStream)
	ht.AssertTxInBlock(block, aliceClosingTxid)
}

// testRBFCoopCloseDisconnect tests that when a node disconnects that the node
// is properly disconnected.
func testRBFCoopCloseDisconnect(ht *lntest.HarnessTest) {
	rbfCoopFlags := []string{"--protocol.rbf-coop-close"}

	// To kick things off, we'll create two new nodes, then fund them with
	// enough coins to make a 50/50 channel.
	cfgs := [][]string{rbfCoopFlags, rbfCoopFlags}
	params := lntest.OpenChannelParams{
		Amt:     btcutil.Amount(1000000),
		PushAmt: btcutil.Amount(1000000 / 2),
	}
	_, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob := nodes[0], nodes[1]

	// Make sure the nodes are connected.
	ht.AssertConnected(alice, bob)

	// Disconnect Bob from Alice.
	ht.DisconnectNodes(alice, bob)
}
