package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(net *lntest.NetworkHarness, t *harnessTest) {
	// Skip test for neutrino, as we cannot disconnect the miner at will.
	// TODO(halseth): remove when either can disconnect at will, or restart
	// node with connection to new miner.
	if net.BackendCfg.Name() == lntest.NeutrinoBackendName {
		t.Skipf("skipping reorg test for neutrino backend")
	}

	var (
		ctxb = context.Background()
		temp = "temp"
	)

	// Set up a new miner that we can use to cause a reorg.
	tempLogDir := ".tempminerlogs"
	logFilename := "output-open_channel_reorg-temp_miner.log"
	tempMiner, err := lntest.NewTempMiner(tempLogDir, logFilename)
	require.NoError(t.t, err, "failed to create temp miner")
	defer func() {
		require.NoError(
			t.t, tempMiner.Stop(),
			"failed to clean up temp miner",
		)
	}()

	// Setup the temp miner
	require.NoError(
		t.t, tempMiner.SetUp(false, 0), "unable to set up mining node",
	)

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	err = net.Miner.Client.Node(
		btcjson.NConnect, tempMiner.P2PAddress(), &temp,
	)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}
	nodeSlice := []*rpctest.Harness{net.Miner.Harness, tempMiner.Harness}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two miners should be on the same blockheight.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 0)

	// We disconnect the two miners, such that we can mine two different
	// chains and can cause a reorg later.
	err = net.Miner.Client.Node(
		btcjson.NDisconnect, tempMiner.P2PAddress(), &temp,
	)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(0)
	pendingUpdate, err := net.OpenPendingChannel(
		net.Alice, net.Bob, chanAmt, pushAmt,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Wait for miner to have seen the funding tx. The temporary miner is
	// disconnected, and won't see the transaction.
	_, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("failed to find funding tx in mempool: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	assertNumOpenChannelsPending(t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel on the original miner's chain, which should be considered
	// open.
	block := mineBlocks(t, net, 10, 1)[0]
	assertTxInBlock(t, block, fundingTxID)
	if _, err := tempMiner.Client.Generate(15); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Ensure the chain lengths are what we expect, with the temp miner
	// being 5 blocks ahead.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 5)

	// Wait for Alice to sync to the original miner's chain.
	_, minerHeight, err := net.Miner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}
	err = waitForNodeBlockHeight(net.Alice, minerHeight)
	if err != nil {
		t.Fatalf("unable to sync to chain: %v", err)
	}

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	assertNumOpenChannelsPending(t, net.Alice, net.Bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	err = net.Alice.WaitForNetworkChannelOpen(chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// Alice should now have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	chanGraph, err := net.Alice.DescribeGraph(ctxt, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges := len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find one edge in the graph, found %d",
			numEdges)
	}

	// Now we disconnect Alice's chain backend from the original miner, and
	// connect the two miners together. Since the temporary miner knows
	// about a longer chain, both miners should sync to that chain.
	err = net.BackendCfg.DisconnectMiner()
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Connecting to the temporary miner should now cause our original
	// chain to be re-orged out.
	err = net.Miner.Client.Node(
		btcjson.NConnect, tempMiner.P2PAddress(), &temp,
	)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	nodes := []*rpctest.Harness{tempMiner.Harness, net.Miner.Harness}
	if err := rpctest.JoinNodes(nodes, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// Once again they should be on the same chain.
	assertMinerBlockHeightDelta(t, net.Miner, tempMiner, 0)

	// Now we disconnect the two miners, and connect our original miner to
	// our chain backend once again.
	err = net.Miner.Client.Node(
		btcjson.NDisconnect, tempMiner.P2PAddress(), &temp,
	)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	err = net.BackendCfg.ConnectMiner()
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// This should have caused a reorg, and Alice should sync to the longer
	// chain, where the funding transaction is not confirmed.
	_, tempMinerHeight, err := tempMiner.Client.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}
	err = waitForNodeBlockHeight(net.Alice, tempMinerHeight)
	if err != nil {
		t.Fatalf("unable to sync to chain: %v", err)
	}

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	req = &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}

	var predErr error
	err = wait.Predicate(func() bool {
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		chanGraph, err = net.Alice.DescribeGraph(ctxt, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for alice's routing table: %v", err)
			return false
		}

		numEdges = len(chanGraph.Edges)
		if numEdges != 0 {
			predErr = fmt.Errorf("expected to find no edge in the graph, found %d",
				numEdges)
			return false
		}
		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// Cleanup by mining the funding tx again, then closing the channel.
	block = mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	closeReorgedChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

// testBasicChannelCreationAndUpdates tests multiple channel opening and closing,
// and ensures that if a node is subscribed to channel updates they will be
// received correctly for both cooperative and force closed channels.
func testBasicChannelCreationAndUpdates(net *lntest.NetworkHarness, t *harnessTest) {
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
