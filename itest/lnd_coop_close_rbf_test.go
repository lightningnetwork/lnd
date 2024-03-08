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
	// enough coins for the test below.
	alice := ht.NewNode("Carol", rbfCoopFlags)
	defer ht.Shutdown(alice)

	bob := ht.NewNode("Dave", rbfCoopFlags)
	defer ht.Shutdown(bob)

	// With the nodes active, we'll fund Alice's wallet, connect them, then
	// fund enough coins to make a 50/50 channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)
	ht.ConnectNodes(alice, bob)
	chanPoint := ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt:     btcutil.Amount(1000000),
		PushAmt: btcutil.Amount(1000000 / 2),
	})

	// Now that both sides are active with a funded channel, we can kick
	// off the test.
	//
	// To start, we'll have Alice try to close the channel, with a fee rate
	// of 5 sat/byte.
	aliceFeeRate := chainfee.SatPerVByte(5)
	aliceCloseStream, aliceCloseUpdate := ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
	)

	// Confirm that this new update was at 5 sat/vb.
	alicePendingUpdate := aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, int64(aliceFeeRate), alicePendingUpdate.FeeRatePerVbyte,
	)
	require.True(ht, alicePendingUpdate.LocalCloseTx)

	// Now, we'll have Bob attempt to RBF the close transaction with a
	// higher fee rate, double that of Alice's.
	bobFeeRate := aliceFeeRate * 2
	bobCloseStream, bobCloseUpdate := ht.CloseChannelAssertPending(
		bob, chanPoint, false, lntest.WithCoopCloseFeeRate(bobFeeRate),
	)

	// Confirm that this new update was at 10 sat/vb.
	bobPendingUpdate := bobCloseUpdate.GetClosePending()
	require.NotNil(ht, bobCloseUpdate)
	require.Equal(ht, bobPendingUpdate.FeeRatePerVbyte, int64(bobFeeRate))
	require.True(ht, bobPendingUpdate.LocalCloseTx)

	var err error

	// Alice should've also received a similar update that Bob has
	// increased the closing fee rate to 10 sat/vb with his settled funds.
	aliceCloseUpdate, err = aliceCloseStream.Recv()
	require.NoError(ht, err)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(ht, alicePendingUpdate.FeeRatePerVbyte, int64(bobFeeRate))
	require.False(ht, alicePendingUpdate.LocalCloseTx)

	// Next, we'll have Alice double that fee rate again to 20 sat/vb.
	aliceFeeRate = bobFeeRate * 2
	_, aliceCloseUpdate = ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
	)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, alicePendingUpdate.FeeRatePerVbyte, int64(aliceFeeRate),
	)
	require.True(ht, alicePendingUpdate.LocalCloseTx)

	// Bob should've also received a similar update that Alice has
	// increased the closing fee rate to 20 sat/vb with his settled funds.
	bobCloseUpdate, err = bobCloseStream.Recv()
	require.NoError(ht, err)
	bobPendingUpdate = bobCloseUpdate.GetClosePending()
	require.NotNil(ht, bobCloseUpdate)
	require.Equal(
		ht, bobPendingUpdate.FeeRatePerVbyte, int64(aliceFeeRate),
	)
	require.False(ht, bobPendingUpdate.LocalCloseTx)

	// Consume Alice's stream update, this is a duplicate of the one we
	// checked above.
	_, err = aliceCloseStream.Recv()
	require.NoError(ht, err)

	// To conclude, we'll mine a block which should now confirm Alice's
	// version of the coop close transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// Both Alice and Bob should trigger a final close update to signal the
	// closing transaction has confirmed.
	aliceClosingTxid := ht.WaitForChannelCloseEvent(aliceCloseStream)
	bobClosingTxid := ht.WaitForChannelCloseEvent(bobCloseStream)
	require.Equal(ht, aliceClosingTxid, bobClosingTxid)
	ht.Miner.AssertTxInBlock(block, aliceClosingTxid)
}
