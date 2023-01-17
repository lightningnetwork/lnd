package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(ht *lntemp.HarnessTest) {
	// Skip test for neutrino, as we cannot disconnect the miner at will.
	// TODO(halseth): remove when either can disconnect at will, or restart
	// node with connection to new miner.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping reorg test for neutrino backend")
	}

	temp := "temp"

	// Set up a new miner that we can use to cause a reorg.
	tempLogDir := ".tempminerlogs"
	logFilename := "output-open_channel_reorg-temp_miner.log"
	tempMiner := lntemp.NewTempMiner(
		ht.Context(), ht.T, tempLogDir, logFilename,
	)
	defer tempMiner.Stop()

	// Setup the temp miner
	require.NoError(ht, tempMiner.SetUp(false, 0),
		"unable to set up mining node")

	miner := ht.Miner
	alice, bob := ht.Alice, ht.Bob

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	err := miner.Client.Node(
		btcjson.NConnect, tempMiner.P2PAddress(), &temp,
	)
	require.NoError(ht, err, "unable to connect miners")

	nodeSlice := []*rpctest.Harness{miner.Harness, tempMiner.Harness}
	err = rpctest.JoinNodes(nodeSlice, rpctest.Blocks)
	require.NoError(ht, err, "unable to join node on blocks")

	// The two miners should be on the same blockheight.
	assertMinerBlockHeightDelta(ht, miner, tempMiner, 0)

	// We disconnect the two miners, such that we can mine two different
	// chains and can cause a reorg later.
	err = miner.Client.Node(
		btcjson.NDisconnect, tempMiner.P2PAddress(), &temp,
	)
	require.NoError(ht, err, "unable to disconnect miners")

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	params := lntemp.OpenChannelParams{
		Amt:     funding.MaxBtcFundingAmount,
		Private: true,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, params)

	// Wait for miner to have seen the funding tx. The temporary miner is
	// disconnected, and won't see the transaction.
	ht.Miner.AssertNumTxsInMempool(1)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	require.NoError(ht, err, "convert funding txid into chainhash failed")

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel on the original miner's chain, which should be considered
	// open.
	block := ht.MineBlocks(10)[0]
	ht.Miner.AssertTxInBlock(block, fundingTxID)
	_, err = tempMiner.Client.Generate(15)
	require.NoError(ht, err, "unable to generate blocks")

	// Ensure the chain lengths are what we expect, with the temp miner
	// being 5 blocks ahead.
	assertMinerBlockHeightDelta(ht, miner, tempMiner, 5)

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	ht.AssertTopologyChannelOpen(alice, chanPoint)
	ht.AssertTopologyChannelOpen(bob, chanPoint)

	// Alice should now have 1 edge in her graph.
	ht.AssertNumEdges(alice, 1, true)

	// Now we disconnect Alice's chain backend from the original miner, and
	// connect the two miners together. Since the temporary miner knows
	// about a longer chain, both miners should sync to that chain.
	ht.DisconnectMiner()

	// Connecting to the temporary miner should now cause our original
	// chain to be re-orged out.
	err = miner.Client.Node(btcjson.NConnect, tempMiner.P2PAddress(), &temp)
	require.NoError(ht, err, "unable to connect temp miner")

	nodes := []*rpctest.Harness{tempMiner.Harness, miner.Harness}
	err = rpctest.JoinNodes(nodes, rpctest.Blocks)
	require.NoError(ht, err, "unable to join node on blocks")

	// Once again they should be on the same chain.
	assertMinerBlockHeightDelta(ht, miner, tempMiner, 0)

	// Now we disconnect the two miners, and connect our original miner to
	// our chain backend once again.
	err = miner.Client.Node(
		btcjson.NDisconnect, tempMiner.P2PAddress(), &temp,
	)
	require.NoError(ht, err, "unable to disconnect temp miner")

	ht.ConnectMiner()

	// This should have caused a reorg, and Alice should sync to the longer
	// chain, where the funding transaction is not confirmed.
	_, tempMinerHeight, err := tempMiner.Client.GetBestBlock()
	require.NoError(ht, err, "unable to get current blockheight")
	ht.WaitForNodeBlockHeight(alice, tempMinerHeight)

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	ht.AssertNumEdges(alice, 0, true)

	// Cleanup by mining the funding tx again, then closing the channel.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, fundingTxID)

	ht.CloseChannel(alice, chanPoint)
}

// testOpenChannelFeePolicy checks if different channel fee scenarios
// are correctly handled when the optional channel fee parameters
// baseFee and feeRate are provided. If the OpenChannelRequest is not
// provided with a value for baseFee/feeRate the expectation is that the
// default baseFee/feeRate is applied.
// 1.) no params provided to OpenChannelRequest
// ChannelUpdate --> defaultBaseFee, defaultFeeRate
// 2.) only baseFee provided to OpenChannelRequest
// ChannelUpdate --> provided baseFee, defaultFeeRate
// 3.) only feeRate provided to OpenChannelRequest
// ChannelUpdate --> defaultBaseFee, provided FeeRate
// 4.) baseFee and feeRate provided to OpenChannelRequest
// ChannelUpdate --> provided baseFee, provided feeRate.
func testOpenChannelUpdateFeePolicy(net *lntest.NetworkHarness,
	t *harnessTest) {

	const (
		defaultBaseFee       = 1000
		defaultFeeRate       = 1
		defaultTimeLockDelta = chainreg.DefaultBitcoinTimeLockDelta
		defaultMinHtlc       = 1000
		optionalBaseFee      = 1337
		optionalFeeRate      = 1337
	)

	defaultMaxHtlc := calculateMaxHtlc(funding.MaxBtcFundingAmount)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	feeScenarios := []lntest.OpenChannelParams{
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			UseBaseFee: false,
			UseFeeRate: false,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			BaseFee:    optionalBaseFee,
			UseBaseFee: true,
			UseFeeRate: false,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			FeeRate:    optionalFeeRate,
			UseBaseFee: false,
			UseFeeRate: true,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			BaseFee:    optionalBaseFee,
			FeeRate:    optionalFeeRate,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	}

	expectedPolicies := []lnrpc.RoutingPolicy{
		{
			FeeBaseMsat:      defaultBaseFee,
			FeeRateMilliMsat: defaultFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      optionalBaseFee,
			FeeRateMilliMsat: defaultFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      defaultBaseFee,
			FeeRateMilliMsat: optionalFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      optionalBaseFee,
			FeeRateMilliMsat: optionalFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
	}

	bobExpectedPolicy := lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultBaseFee,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}

	for i, feeScenario := range feeScenarios {
		// Create a channel Alice->Bob.
		chanPoint := openChannelAndAssert(
			t, net, net.Alice, net.Bob,
			feeScenario,
		)

		defer closeChannelAndAssert(t, net, net.Alice, chanPoint, false)

		// We add all the nodes' update channels to a slice, such that we can
		// make sure they all receive the expected updates.
		nodes := []*lntest.HarnessNode{net.Alice, net.Bob}

		// Alice and Bob should see each other's ChannelUpdates, advertising
		// the preferred routing policies.
		assertPolicyUpdate(
			t, nodes, net.Alice.PubKeyStr,
			&expectedPolicies[i], chanPoint,
		)
		assertPolicyUpdate(
			t, nodes, net.Bob.PubKeyStr,
			&bobExpectedPolicy, chanPoint,
		)

		// They should now know about the default policies.
		for _, node := range nodes {
			assertChannelPolicy(
				t, node, net.Alice.PubKeyStr,
				&expectedPolicies[i], chanPoint,
			)
			assertChannelPolicy(
				t, node, net.Bob.PubKeyStr,
				&bobExpectedPolicy, chanPoint,
			)
		}

		require.NoError(
			t.t, net.Alice.WaitForNetworkChannelOpen(chanPoint),
			"alice reports channel opening",
		)

		require.NoError(
			t.t, net.Bob.WaitForNetworkChannelOpen(chanPoint),
			"bob reports channel opening",
		)
	}
}

// testBasicChannelCreationAndUpdates tests multiple channel opening and
// closing, and ensures that if a node is subscribed to channel updates
// they will be received correctly for both cooperative and force closed
// channels.
func testBasicChannelCreationAndUpdates(net *lntest.NetworkHarness,
	t *harnessTest) {

	runBasicChannelCreationAndUpdates(net, t, net.Alice, net.Bob)
}

// runBasicChannelCreationAndUpdates tests multiple channel opening and closing,
// and ensures that if a node is subscribed to channel updates they will be
// received correctly for both cooperative and force closed channels.
func runBasicChannelCreationAndUpdates(net *lntest.NetworkHarness,
	t *harnessTest, alice, bob *lntest.HarnessNode) {

	ctxb := context.Background()
	const (
		numChannels = 2
		amount      = funding.MaxBtcFundingAmount
	)

	// Subscribe Bob and Alice to channel event notifications.
	bobChanSub := subscribeChannelNotifications(ctxb, t, bob)
	defer close(bobChanSub.quit)

	aliceChanSub := subscribeChannelNotifications(ctxb, t, alice)
	defer close(aliceChanSub.quit)

	// Open the channels between Alice and Bob, asserting that the channels
	// have been properly opened on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	for i := 0; i < numChannels; i++ {
		chanPoints[i] = openChannelAndAssert(
			t, net, alice, bob, lntest.OpenChannelParams{
				Amt: amount,
			},
		)
	}

	// Since each of the channels just became open, Bob and Alice should
	// each receive an open and an active notification for each channel.
	const numExpectedOpenUpdates = 3 * numChannels
	verifyOpenUpdatesReceived := func(sub channelSubscription) error {
		numChannelUpds := 0
		for numChannelUpds < numExpectedOpenUpdates {
			select {
			case update := <-sub.updateChan:
				switch update.Type {
				case lnrpc.ChannelEventUpdate_PENDING_OPEN_CHANNEL:
					if numChannelUpds%3 != 0 {
						return fmt.Errorf("expected " +
							"open or active" +
							"channel ntfn, got pending open " +
							"channel ntfn instead")
					}
				case lnrpc.ChannelEventUpdate_OPEN_CHANNEL:
					if numChannelUpds%3 != 1 {
						return fmt.Errorf("expected " +
							"pending open or active" +
							"channel ntfn, got open" +
							"channel ntfn instead")
					}
				case lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL:
					if numChannelUpds%3 != 2 {
						return fmt.Errorf("expected " +
							"pending open or open" +
							"channel ntfn, got active " +
							"channel ntfn instead")
					}
				default:
					return fmt.Errorf("update type mismatch: "+
						"expected open or active channel "+
						"notification, got: %v",
						update.Type)
				}
				numChannelUpds++

			case <-time.After(time.Second * 10):
				return fmt.Errorf("timeout waiting for channel "+
					"notifications, only received %d/%d "+
					"chanupds", numChannelUpds,
					numExpectedOpenUpdates)
			}
		}

		return nil
	}

	require.NoError(
		t.t, verifyOpenUpdatesReceived(bobChanSub), "bob open channels",
	)
	require.NoError(
		t.t, verifyOpenUpdatesReceived(aliceChanSub), "alice open "+
			"channels",
	)

	// Close the channels between Alice and Bob, asserting that the channels
	// have been properly closed on-chain.
	for i, chanPoint := range chanPoints {
		// Force close the first of the two channels.
		force := i%2 == 0
		closeChannelAndAssert(t, net, alice, chanPoint, force)
		if force {
			cleanupForceClose(t, net, alice, chanPoint)
		}
	}

	// verifyCloseUpdatesReceived is used to verify that Alice and Bob
	// receive the correct channel updates in order.
	const numExpectedCloseUpdates = 3 * numChannels
	verifyCloseUpdatesReceived := func(sub channelSubscription,
		forceType lnrpc.ChannelCloseSummary_ClosureType,
		closeInitiator lnrpc.Initiator) error {

		// Ensure one inactive and one closed notification is received
		// for each closed channel.
		numChannelUpds := 0
		for numChannelUpds < numExpectedCloseUpdates {
			expectedCloseType := lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE

			// Every other channel should be force closed. If this
			// channel was force closed, set the expected close type
			// to the type passed in.
			force := (numChannelUpds/3)%2 == 0
			if force {
				expectedCloseType = forceType
			}

			select {
			case chanUpdate := <-sub.updateChan:
				err := verifyCloseUpdate(
					chanUpdate, expectedCloseType,
					closeInitiator,
				)
				if err != nil {
					return err
				}

				numChannelUpds++

			case err := <-sub.errChan:
				return err

			case <-time.After(time.Second * 10):
				return fmt.Errorf("timeout waiting "+
					"for channel notifications, only "+
					"received %d/%d chanupds",
					numChannelUpds, numChannelUpds)
			}
		}

		return nil
	}

	// Verify Bob receives all closed channel notifications. He should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a remote
	// close initiator because Alice closed the channels.
	require.NoError(
		t.t, verifyCloseUpdatesReceived(
			bobChanSub,
			lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE,
			lnrpc.Initiator_INITIATOR_REMOTE,
		), "verifying bob close updates",
	)

	// Verify Alice receives all closed channel notifications. She should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a local
	// close initiator because Alice closed the channels.
	require.NoError(
		t.t, verifyCloseUpdatesReceived(
			aliceChanSub,
			lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE,
			lnrpc.Initiator_INITIATOR_LOCAL,
		), "verifying alice close updates",
	)
}

// assertMinerBlockHeightDelta ensures that tempMiner is 'delta' blocks ahead
// of miner.
func assertMinerBlockHeightDelta(ht *lntemp.HarnessTest,
	miner, tempMiner *lntemp.HarnessMiner, delta int32) {

	// Ensure the chain lengths are what we expect.
	err := wait.NoError(func() error {
		_, tempMinerHeight, err := tempMiner.Client.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get current "+
				"blockheight %v", err)
		}

		_, minerHeight, err := miner.Client.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get current "+
				"blockheight %v", err)
		}

		if tempMinerHeight != minerHeight+delta {
			return fmt.Errorf("expected new miner(%d) to be %d "+
				"blocks ahead of original miner(%d)",
				tempMinerHeight, delta, minerHeight)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "failed to assert block height delta")
}
