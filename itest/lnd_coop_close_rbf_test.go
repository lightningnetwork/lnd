package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// rbfTestCase encapsulates the parameters and logic for a single RBF coop close test run.
func runRbfCoopCloseTest(st *lntest.HarnessTest, alice, bob *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, isTaproot bool) {

	// To start, we'll have Alice try to close the channel, with a fee rate
	// of 5 sat/byte.
	aliceFeeRate := chainfee.SatPerVByte(5)
	aliceCloseStream, aliceCloseUpdate := st.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Confirm that this new update was at 5 sat/vb.
	alicePendingUpdate := aliceCloseUpdate.GetClosePending()
	require.NotNil(st, aliceCloseUpdate)
	require.Equal(
		st, int64(aliceFeeRate), alicePendingUpdate.FeePerVbyte,
	)
	require.True(st, alicePendingUpdate.LocalCloseTx)

	// Now, we'll have Bob attempt to RBF the close transaction with a
	// higher fee rate, double that of Alice's.
	bobFeeRate := aliceFeeRate * 2
	bobCloseStream, bobCloseUpdate := st.CloseChannelAssertPending(
		bob, chanPoint, false, lntest.WithCoopCloseFeeRate(bobFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Confirm that this new update was at 10 sat/vb.
	bobPendingUpdate := bobCloseUpdate.GetClosePending()
	require.NotNil(st, bobCloseUpdate)
	require.Equal(st, bobPendingUpdate.FeePerVbyte, int64(bobFeeRate))
	require.True(st, bobPendingUpdate.LocalCloseTx)

	var err error

	// Alice should've also received a similar update that Bob has
	// increased the closing fee rate to 10 sat/vb with his settled funds.
	aliceCloseUpdate, err = st.ReceiveCloseChannelUpdate(aliceCloseStream)
	require.NoError(st, err)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(st, aliceCloseUpdate)
	// For taproot channels, due to different witness sizes, the fee per vbyte
	// might be slightly different due to rounding when converting between
	// absolute fee and fee per vbyte.
	if isTaproot {
		// Allow for a small difference in fee calculation for taproot
		require.InDelta(st, int64(bobFeeRate), alicePendingUpdate.FeePerVbyte, 1)
	} else {
		require.Equal(st, alicePendingUpdate.FeePerVbyte, int64(bobFeeRate))
	}
	require.False(st, alicePendingUpdate.LocalCloseTx)

	// We'll now attempt to make a fee update that increases Alice's fee
	// rate by 6 sat/vb, which should be rejected as it is too small of an
	// increase for the RBF rules. The RPC API however will return the new
	// fee. We'll skip the mempool check here as it won't make it in.
	aliceRejectedFeeRate := aliceFeeRate + 1
	_, aliceCloseUpdate = st.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceRejectedFeeRate),
		lntest.WithLocalTxNotify(), lntest.WithSkipMempoolCheck(),
	)
	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(st, aliceCloseUpdate)
	require.Equal(
		st, alicePendingUpdate.FeePerVbyte,
		int64(aliceRejectedFeeRate),
	)
	require.True(st, alicePendingUpdate.LocalCloseTx)

	_, err = st.ReceiveCloseChannelUpdate(bobCloseStream)
	require.NoError(st, err)

	// We'll now attempt a fee update that we can't actually pay for. This
	// will actually show up as an error to the remote party.
	aliceRejectedFeeRate = 100_000
	_, _ = st.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceRejectedFeeRate),
		lntest.WithLocalTxNotify(),
		lntest.WithExpectedErrString("cannot pay for fee"),
	)

	// At this point, we'll have Alice+Bob reconnect so we can ensure that
	// we can continue to do RBF bumps even after a reconnection.
	st.DisconnectNodes(alice, bob)
	st.ConnectNodes(alice, bob)

	// Next, we'll have Alice double that fee rate again to 20 sat/vb.
	aliceFeeRate = bobFeeRate * 2
	aliceCloseStream, aliceCloseUpdate = st.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(aliceFeeRate),
		lntest.WithLocalTxNotify(),
	)

	alicePendingUpdate = aliceCloseUpdate.GetClosePending()
	require.NotNil(st, aliceCloseUpdate)
	require.Equal(
		st, alicePendingUpdate.FeePerVbyte, int64(aliceFeeRate),
	)
	require.True(st, alicePendingUpdate.LocalCloseTx)

	// To conclude, we'll mine a block which should now confirm Alice's
	// version of the coop close transaction.
	block := st.MineBlocksAndAssertNumTxes(1, 1)[0]

	// Both Alice and Bob should trigger a final close update to signal the
	// closing transaction has confirmed.
	aliceClosingTxid := st.WaitForChannelCloseEvent(aliceCloseStream)
	st.AssertTxInBlock(block, aliceClosingTxid)
}

func testCoopCloseRbf(ht *lntest.HarnessTest) {
	// Test with different channel types including taproot
	channelTypes := []struct {
		name       string
		commitType lnrpc.CommitmentType
	}{
		{
			name:       "anchors",
			commitType: lnrpc.CommitmentType_ANCHORS,
		},
		{
			name:       "taproot",
			commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		},
	}

	for _, chanType := range channelTypes {
		chanType := chanType
		ht.Run(chanType.name, func(t1 *testing.T) {
			st := ht.Subtest(t1)
			// Set the fee estimate to 1sat/vbyte. This ensures that our manually
			// initiated RBF attempts will always be successful.
			st.SetFeeEstimate(250)
			st.SetFeeEstimateWithConf(250, 6)

			// Get the base node args for the commitment type and add RBF flag
			baseArgs := lntest.NodeArgsForCommitType(chanType.commitType)
			aliceArgs := append(baseArgs, "--protocol.rbf-coop-close")
			bobArgs := append(baseArgs, "--protocol.rbf-coop-close")

			// Create new nodes with the appropriate flags
			alice := st.NewNode("Alice", aliceArgs)
			bob := st.NewNode("Bob", bobArgs)

			// Connect the nodes
			st.ConnectNodes(alice, bob)

			// Fund Alice
			st.FundCoins(btcutil.SatoshiPerBitcoin, alice)

			// For taproot channels, we need to make them private
			privateChan := chanType.commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT

			// Open channel between Alice and Bob
			params := lntest.OpenChannelParams{
				Amt:            btcutil.Amount(1000000),
				PushAmt:        btcutil.Amount(1000000 / 2),
				CommitmentType: chanType.commitType,
				Private:        privateChan,
			}
			chanPoint := st.OpenChannel(alice, bob, params)

			// Run the RBF coop close test scenario
			isTaproot := chanType.commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT
			runRbfCoopCloseTest(st, alice, bob, chanPoint, isTaproot)

			// Clean up the nodes
			st.Shutdown(alice)
			st.Shutdown(bob)
		})
	}
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
