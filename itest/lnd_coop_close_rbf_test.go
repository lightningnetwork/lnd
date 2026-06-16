package itest

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// runRbfCoopCloseTest encapsulates the parameters and logic for a
// single RBF coop close test run.
func runRbfCoopCloseTest(st *lntest.HarnessTest,
	alice, bob *node.HarnessNode,
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

	// For taproot channels, due to different witness sizes,
	// the fee per vbyte might be slightly different due to
	// rounding when converting between absolute fee and fee
	// per vbyte.
	if isTaproot {
		// Allow for a small difference in fee
		// calculation for taproot.
		require.InDelta(
			st, int64(bobFeeRate),
			alicePendingUpdate.FeePerVbyte, 1,
		)
	} else {
		require.Equal(
			st, alicePendingUpdate.FeePerVbyte,
			int64(bobFeeRate),
		)
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
		ht.Run(chanType.name, func(t1 *testing.T) {
			st := ht.Subtest(t1)
			// Set the fee estimate to 1sat/vbyte. This ensures that
			// our manually initiated RBF attempts will always be
			// successful.
			st.SetFeeEstimate(250)
			st.SetFeeEstimateWithConf(250, 6)

			// Build node config with commitment type args and RBF
			// flag.
			baseArgs := lntest.NodeArgsForCommitType(
				chanType.commitType,
			)
			baseArgs = append(
				baseArgs, "--protocol.rbf-coop-close",
			)
			nodeArgs := baseArgs
			cfgs := [][]string{nodeArgs, nodeArgs}

			// For taproot channels, we need to make them private.
			isTaproot := chanType.commitType ==
				lnrpc.CommitmentType_SIMPLE_TAPROOT

			params := lntest.OpenChannelParams{
				Amt:            btcutil.Amount(1000000),
				PushAmt:        btcutil.Amount(1000000 / 2),
				CommitmentType: chanType.commitType,
				Private:        isTaproot,
			}

			// Create network with Alice -> Bob channel, then use
			// that to run the RBF coop close test.
			chanPoints, nodes := st.CreateSimpleNetwork(
				cfgs, params,
			)
			alice, bob := nodes[0], nodes[1]
			chanPoint := chanPoints[0]

			runRbfCoopCloseTest(
				st, alice, bob, chanPoint, isTaproot,
			)

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

// testCoopCloseRBFWithReorg tests that the RBF cooperative close flow handles
// chain reorganizations correctly. It verifies that when a close transaction
// is reorged out, the system can still confirm with any valid close tx.
func testCoopCloseRBFWithReorg(ht *lntest.HarnessTest) {
	// Skip this test for neutrino backend as we can't trigger reorgs.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping reorg test for neutrino backend")
	}

	// Force cooperative close to require 3 confirmations for predictable
	// testing.
	const requiredConfs = 3
	rbfCoopFlags := []string{
		"--protocol.rbf-coop-close",
		"--dev.force-channel-close-confs=3",
	}

	// Set the fee estimate to 1sat/vbyte to ensure our RBF attempts work.
	ht.SetFeeEstimate(250)
	ht.SetFeeEstimateWithConf(250, 6)

	// Create two nodes with enough coins for a 50/50 channel.
	cfgs := [][]string{rbfCoopFlags, rbfCoopFlags}
	params := lntest.OpenChannelParams{
		Amt:     btcutil.Amount(10_000_000),
		PushAmt: btcutil.Amount(5_000_000),
	}
	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
	alice, bob := nodes[0], nodes[1]
	chanPoint := chanPoints[0]

	// Initiate cooperative close with initial fee rate of 5 sat/vb.
	initialFeeRate := chainfee.SatPerVByte(5)
	_, aliceCloseUpdate := ht.CloseChannelAssertPending(
		alice, chanPoint, false,
		lntest.WithCoopCloseFeeRate(initialFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Verify the initial close transaction is at the expected fee rate.
	alicePendingUpdate := aliceCloseUpdate.GetClosePending()
	require.NotNil(ht, aliceCloseUpdate)
	require.Equal(
		ht, int64(initialFeeRate), alicePendingUpdate.FeePerVbyte,
	)

	// Capture the initial close transaction from the mempool.
	initialCloseTxid, err := chainhash.NewHash(alicePendingUpdate.Txid)
	require.NoError(ht, err)
	initialCloseTx := ht.AssertTxInMempool(*initialCloseTxid)

	// Create first RBF replacement before any mining.
	firstRbfFeeRate := chainfee.SatPerVByte(10)
	_, firstRbfUpdate := ht.CloseChannelAssertPending(
		bob, chanPoint, false,
		lntest.WithCoopCloseFeeRate(firstRbfFeeRate),
		lntest.WithLocalTxNotify(),
	)

	// Capture the first RBF transaction.
	closePending := firstRbfUpdate.GetClosePending()
	firstRbfTxid, err := chainhash.NewHash(closePending.Txid)
	require.NoError(ht, err)
	firstRbfTx := ht.AssertTxInMempool(*firstRbfTxid)

	_, bestHeight := ht.GetBestBlock()
	ht.Logf("Current block height: %d", bestHeight)

	// Mine n-1 blocks (2 blocks when requiring 3 confirmations) with the
	// first RBF transaction. This is just shy of full confirmation.
	block1 := ht.Miner().MineBlockWithTxes(
		[]*btcutil.Tx{btcutil.NewTx(firstRbfTx)},
	)

	ht.Logf("Mined block %d with first RBF tx", bestHeight+1)

	block2 := ht.MineEmptyBlocks(1)[0]

	ht.Logf("Mined block %d", bestHeight+2)

	ht.Logf("Re-orging two blocks to remove first RBF tx")

	// Trigger a reorganization that removes the last 2 blocks. This is
	// safe because we haven't reached full confirmation yet.
	bestBlockHash := block2.Header.BlockHash()
	require.NoError(
		ht, ht.Miner().InvalidateBlock(&bestBlockHash),
	)
	bestBlockHash = block1.Header.BlockHash()
	require.NoError(
		ht, ht.Miner().InvalidateBlock(&bestBlockHash),
	)

	_, bestHeight = ht.GetBestBlock()
	ht.Logf("Re-orged to block height: %d", bestHeight)

	ht.Log("Mining blocks to surpass previous chain")

	// Mine 2 empty blocks to trigger the reorg on the nodes.
	ht.MineEmptyBlocks(2)

	_, bestHeight = ht.GetBestBlock()
	ht.Logf("Mined blocks to reach height: %d", bestHeight)

	// Now, instead of mining the second RBF, mine the INITIAL transaction
	// to test that the system can handle any valid spend of the funding
	// output.
	block := ht.Miner().MineBlockWithTxes(
		[]*btcutil.Tx{btcutil.NewTx(initialCloseTx)},
	)
	ht.AssertTxInBlock(block, *initialCloseTxid)

	// Mine additional blocks to reach the required confirmations.
	ht.MineEmptyBlocks(requiredConfs - 1)

	// Both parties should see that the channel is now fully closed on
	// chain with the expected closing txid.
	expectedClosingTxid := initialCloseTxid.String()
	err = wait.NoError(func() error {
		req := &lnrpc.ClosedChannelsRequest{}
		aliceClosedChans := alice.RPC.ClosedChannels(req)
		bobClosedChans := bob.RPC.ClosedChannels(req)
		if len(aliceClosedChans.Channels) != 1 {
			return fmt.Errorf("alice: expected 1 closed "+
				"chan, got %d",
				len(aliceClosedChans.Channels))
		}
		if len(bobClosedChans.Channels) != 1 {
			return fmt.Errorf("bob: expected 1 closed "+
				"chan, got %d",
				len(bobClosedChans.Channels))
		}

		aliceClosedChan := aliceClosedChans.Channels[0]
		if aliceClosedChan.ClosingTxHash != expectedClosingTxid {
			return fmt.Errorf("alice: expected closing "+
				"txid %s, got %s",
				expectedClosingTxid,
				aliceClosedChan.ClosingTxHash)
		}
		if aliceClosedChan.CloseType !=
			lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE {

			return fmt.Errorf("alice: expected cooperative "+
				"close, got %v",
				aliceClosedChan.CloseType)
		}

		bobClosedChan := bobClosedChans.Channels[0]
		if bobClosedChan.ClosingTxHash != expectedClosingTxid {
			return fmt.Errorf("bob: expected closing "+
				"txid %s, got %s",
				expectedClosingTxid,
				bobClosedChan.ClosingTxHash)
		}
		if bobClosedChan.CloseType !=
			lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE {

			return fmt.Errorf("bob: expected cooperative "+
				"close, got %v",
				bobClosedChan.CloseType)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err)

	ht.Logf("Successfully verified closing txid: %s",
		expectedClosingTxid)
}
