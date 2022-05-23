package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(ht *lntest.HarnessTest) {
	// Run through the test with combinations of all the different
	// commitment types.
	allTypes := []lnrpc.CommitmentType{
		lnrpc.CommitmentType_LEGACY,
		lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		lnrpc.CommitmentType_ANCHORS,
	}

	// testFunding is a function closure that takes Carol and Dave's
	// commitment types and test the funding flow.
	testFunding := func(carolCommitType, daveCommitType lnrpc.CommitmentType) {
		// Based on the current tweak variable for Carol, we'll
		// preferentially signal the legacy commitment format.  We do
		// the same for Dave shortly below.
		carolArgs := nodeArgsForCommitType(carolCommitType)
		carol := ht.NewNode("Carol", carolArgs)
		defer ht.Shutdown(carol)

		// Each time, we'll send Carol a new set of coins in order to
		// fund the channel.
		ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

		daveArgs := nodeArgsForCommitType(daveCommitType)
		dave := ht.NewNode("Dave", daveArgs)
		defer ht.Shutdown(dave)

		// Before we start the test, we'll ensure both sides are
		// connected to the funding flow can properly be executed.
		ht.EnsureConnected(carol, dave)

		carolChan, daveChan, closeChan := basicChannelFundingTest(
			ht, carol, dave, nil,
		)

		// Both nodes should report the same commitment
		// type.
		chansCommitType := carolChan.CommitmentType
		require.Equal(ht, chansCommitType, daveChan.CommitmentType,
			"commit types don't match")

		// Now check that the commitment type reported
		// by both nodes is what we expect. It will be
		// the minimum of the two nodes' preference, in
		// the order Legacy, Tweakless, Anchors.
		expType := carolCommitType

		switch daveCommitType {
		// Dave supports anchors, type will be what
		// Carol supports.
		case lnrpc.CommitmentType_ANCHORS:

		// Dave only supports tweakless, channel will
		// be downgraded to this type if Carol supports
		// anchors.
		case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
			if expType == lnrpc.CommitmentType_ANCHORS {
				expType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
			}

		// Dave only supoprts legacy type, channel will
		// be downgraded to this type.
		case lnrpc.CommitmentType_LEGACY:
			expType = lnrpc.CommitmentType_LEGACY

		default:
			ht.Fatalf("invalid commit type %v", daveCommitType)
		}

		// Check that the signalled type matches what we
		// expect.
		switch {
		case expType == lnrpc.CommitmentType_ANCHORS &&
			chansCommitType == lnrpc.CommitmentType_ANCHORS:

		case expType == lnrpc.CommitmentType_STATIC_REMOTE_KEY &&
			chansCommitType == lnrpc.CommitmentType_STATIC_REMOTE_KEY:

		case expType == lnrpc.CommitmentType_LEGACY &&
			chansCommitType == lnrpc.CommitmentType_LEGACY:

		default:
			ht.Fatalf("expected nodes to signal "+
				"commit type %v, instead got "+
				"%v", expType, chansCommitType)
		}

		// As we've concluded this sub-test case we'll
		// now close out the channel for both sides.
		closeChan()
	}

test:
	// We'll test all possible combinations of the feature bit presence
	// that both nodes can signal for this new channel type. We'll make a
	// new Carol+Dave for each test instance as well.
	for _, carolCommitType := range allTypes {
		for _, daveCommitType := range allTypes {
			cc := carolCommitType
			dc := daveCommitType

			testName := fmt.Sprintf(
				"carol_commit=%v,dave_commit=%v", cc, dc,
			)
			success := ht.Run(testName, func(t *testing.T) {
				testFunding(cc, dc)
			})

			if !success {
				break test
			}
		}
	}
}

// basicChannelFundingTest is a sub-test of the main testBasicChannelFunding
// test. Given two nodes: Alice and Bob, it'll assert proper channel creation,
// then return a function closure that should be called to assert proper
// channel closure.
func basicChannelFundingTest(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode, bob *lntest.HarnessNode,
	fundingShim *lnrpc.FundingShim) (*lnrpc.Channel,
	*lnrpc.Channel, func()) {

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	satPerVbyte := btcutil.Amount(1)

	// Record nodes' channel balance before testing.
	aliceChannelBalance := ht.GetChannelBalance(alice)
	bobChannelBalance := ht.GetChannelBalance(bob)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *lntest.HarnessNode,
		oldChannelBalance *lnrpc.ChannelBalanceResponse,
		local, remote btcutil.Amount) {

		newResp := oldChannelBalance

		newResp.LocalBalance.Sat += uint64(local)
		newResp.LocalBalance.Msat += uint64(
			lnwire.NewMSatFromSatoshis(local),
		)
		newResp.RemoteBalance.Sat += uint64(remote)
		newResp.RemoteBalance.Msat += uint64(
			lnwire.NewMSatFromSatoshis(remote),
		)
		// Deprecated fields.
		newResp.Balance += int64(local) // nolint:staticcheck
		ht.AssertChannelBalanceResp(node, newResp)
	}

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:         chanAmt,
			PushAmt:     pushAmt,
			FundingShim: fundingShim,
			SatPerVByte: satPerVbyte,
		},
	)

	ht.AssertChannelOpen(alice, chanPoint)
	ht.AssertChannelOpen(bob, chanPoint)

	cType := ht.GetChannelCommitType(alice, chanPoint)

	// With the channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	aliceLocalBalance := chanAmt - pushAmt - calcStaticFee(cType, 0)
	checkChannelBalance(
		alice, aliceChannelBalance, aliceLocalBalance, pushAmt,
	)
	checkChannelBalance(
		bob, bobChannelBalance, pushAmt, aliceLocalBalance,
	)

	aliceChannel := ht.ListChannels(alice)
	bobChannel := ht.ListChannels(bob)

	closeChan := func() {
		// Finally, immediately close the channel. This function will
		// also block until the channel is closed and will additionally
		// assert the relevant channel closing post conditions.
		ht.CloseChannel(alice, chanPoint, false)
	}

	return aliceChannel.Channels[0], bobChannel.Channels[0], closeChan
}

// testUnconfirmedChannelFunding tests that our unconfirmed change outputs can
// be used to fund channels.
func testUnconfirmedChannelFunding(ht *lntest.HarnessTest) {
	const (
		chanAmt = funding.MaxBtcFundingAmount
		pushAmt = btcutil.Amount(100000)
	)

	// We'll start off by creating a node for Carol.
	carol := ht.NewNode("Carol", nil)
	defer ht.Shutdown(carol)

	alice := ht.Alice()
	defer ht.Shutdown(alice)

	// We'll send her some confirmed funds.
	ht.SendCoins(2*chanAmt, carol)

	// Now let Carol send some funds to herself, making a unconfirmed
	// change output.
	resp := ht.NewAddress(carol, lnrpc.AddressType_WITNESS_PUBKEY_HASH)
	ht.SendCoinFromNode(
		carol, &lnrpc.SendCoinsRequest{
			Addr:   resp.Address,
			Amount: int64(chanAmt) / 5,
		},
	)

	// Make sure the unconfirmed tx is seen in the mempool.
	ht.AssertNumTxsInMempool(1)

	// Now, we'll connect her to Alice so that they can open a channel
	// together. The funding flow should select Carol's unconfirmed output
	// as she doesn't have any other funds since it's a new node.
	ht.ConnectNodes(carol, alice)

	chanOpenUpdate := ht.OpenChannelStreamAndAssert(
		carol, alice, lntest.OpenChannelParams{
			Amt:              chanAmt,
			PushAmt:          pushAmt,
			SpendUnconfirmed: true,
		},
	)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *lntest.HarnessNode,
		local, remote, pendingLocal, pendingRemote btcutil.Amount) {

		expectedResponse := &lnrpc.ChannelBalanceResponse{
			LocalBalance: &lnrpc.Amount{
				Sat: uint64(local),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					local,
				)),
			},
			RemoteBalance: &lnrpc.Amount{
				Sat: uint64(remote),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					remote,
				)),
			},
			PendingOpenLocalBalance: &lnrpc.Amount{
				Sat: uint64(pendingLocal),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					pendingLocal,
				)),
			},
			PendingOpenRemoteBalance: &lnrpc.Amount{
				Sat: uint64(pendingRemote),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					pendingRemote,
				)),
			},
			UnsettledLocalBalance:  &lnrpc.Amount{},
			UnsettledRemoteBalance: &lnrpc.Amount{},
			// Deprecated fields.
			Balance:            int64(local),
			PendingOpenBalance: int64(pendingLocal),
		}
		ht.AssertChannelBalanceResp(node, expectedResponse)
	}

	// As the channel is pending open, it's expected Carol has both zero
	// local and remote balances, and pending local/remote should not be
	// zero.
	//
	// Note that atm we haven't obtained the chanPoint yet, so we use the
	// type directly.
	cType := lnrpc.CommitmentType_STATIC_REMOTE_KEY
	carolLocalBalance := chanAmt - pushAmt - calcStaticFee(cType, 0)
	checkChannelBalance(carol, 0, 0, carolLocalBalance, pushAmt)

	// For Alice, her local/remote balances should be zero, and the
	// local/remote balances are the mirror of Carol's.
	checkChannelBalance(alice, 0, 0, pushAmt, carolLocalBalance)

	// Confirm the channel and wait for it to be recognized by both
	// parties. Two transactions should be mined, the unconfirmed spend and
	// the funding tx.
	ht.MineBlocksAndAssertTx(6, 2)
	chanPoint := ht.WaitForChannelOpen(chanOpenUpdate)

	// With the channel open, we'll check the balances on each side of the
	// channel as a sanity check to ensure things worked out as intended.
	checkChannelBalance(carol, carolLocalBalance, pushAmt, 0, 0)
	checkChannelBalance(alice, pushAmt, carolLocalBalance, 0, 0)

	// Now that we're done with the test, the channel can be closed.
	ht.CloseChannel(carol, chanPoint, false)
}

// testChannelFundingInputTypes tests that any type of supported input type can
// be used to fund channels.
func testChannelFundingInputTypes(net *lntest.NetworkHarness, t *harnessTest) {
	const (
		chanAmt  = funding.MaxBtcFundingAmount
		burnAddr = "bcrt1qxsnqpdc842lu8c0xlllgvejt6rhy49u6fmpgyz"
	)
	addrTypes := []lnrpc.AddressType{
		lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		lnrpc.AddressType_NESTED_PUBKEY_HASH,
		lnrpc.AddressType_TAPROOT_PUBKEY,
	}

	// We'll start off by creating a node for Carol.
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	// Now, we'll connect her to Alice so that they can open a
	// channel together.
	net.ConnectNodes(t.t, carol, net.Alice)

	for _, addrType := range addrTypes {
		// We'll send her some confirmed funds.
		err := net.SendCoinsOfType(chanAmt*2, carol, addrType, true)
		require.NoErrorf(
			t.t, err, "unable to send coins for carol and addr "+
				"type %v", addrType,
		)

		chanOpenUpdate := openChannelStream(
			t, net, carol, net.Alice, lntest.OpenChannelParams{
				Amt: chanAmt,
			},
		)

		// Creates a helper closure to be used below which asserts the
		// proper response to a channel balance RPC.
		checkChannelBalance := func(node *lntest.HarnessNode,
			local, remote, pendingLocal,
			pendingRemote btcutil.Amount) {

			expectedResponse := &lnrpc.ChannelBalanceResponse{
				LocalBalance: &lnrpc.Amount{
					Sat: uint64(local),
					Msat: uint64(lnwire.NewMSatFromSatoshis(
						local,
					)),
				},
				RemoteBalance: &lnrpc.Amount{
					Sat: uint64(remote),
					Msat: uint64(lnwire.NewMSatFromSatoshis(
						remote,
					)),
				},
				PendingOpenLocalBalance: &lnrpc.Amount{
					Sat: uint64(pendingLocal),
					Msat: uint64(lnwire.NewMSatFromSatoshis(
						pendingLocal,
					)),
				},
				PendingOpenRemoteBalance: &lnrpc.Amount{
					Sat: uint64(pendingRemote),
					Msat: uint64(lnwire.NewMSatFromSatoshis(
						pendingRemote,
					)),
				},
				UnsettledLocalBalance:  &lnrpc.Amount{},
				UnsettledRemoteBalance: &lnrpc.Amount{},
				// Deprecated fields.
				Balance:            int64(local),
				PendingOpenBalance: int64(pendingLocal),
			}
			assertChannelBalanceResp(t, node, expectedResponse)
		}

		// As the channel is pending open, it's expected Carol has both
		// zero local and remote balances, and pending local/remote
		// should not be zero.
		//
		// Note that atm we haven't obtained the chanPoint yet, so we
		// use the type directly.
		cType := lnrpc.CommitmentType_STATIC_REMOTE_KEY
		carolLocalBalance := chanAmt - calcStaticFee(cType, 0)
		checkChannelBalance(carol, 0, 0, carolLocalBalance, 0)

		// For Alice, her local/remote balances should be zero, and the
		// local/remote balances are the mirror of Carol's.
		checkChannelBalance(net.Alice, 0, 0, 0, carolLocalBalance)

		// Confirm the channel and wait for it to be recognized by both
		// parties. Two transactions should be mined, the unconfirmed
		// spend and the funding tx.
		mineBlocks(t, net, 6, 1)
		chanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
		require.NoError(
			t.t, err, "error while waiting for channel open",
		)

		// With the channel open, we'll check the balances on each side
		// of the channel as a sanity check to ensure things worked out
		// as intended.
		checkChannelBalance(carol, carolLocalBalance, 0, 0, 0)
		checkChannelBalance(net.Alice, 0, carolLocalBalance, 0, 0)

		// Now that we're done with the test, the channel can be closed.
		closeChannelAndAssert(t, net, carol, chanPoint, false)

		// Empty out the wallet so there aren't any lingering coins.
		sendAllCoinsConfirm(net, carol, t, burnAddr)
	}
}

// sendAllCoinsConfirm sends all coins of the node's wallet to the given address
// and awaits one confirmation.
func sendAllCoinsConfirm(net *lntest.NetworkHarness, node *lntest.HarnessNode,
	t *harnessTest, addr string) {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:    addr,
		SendAll: true,
	}
	_, err := node.SendCoins(ctxt, sweepReq)
	require.NoError(t.t, err)

	// Make sure the unconfirmed tx is seen in the mempool.
	_, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err, "failed to find tx in miner mempool")

	mineBlocks(t, net, 1, 1)
}

// testExternalFundingChanPoint tests that we're able to carry out a normal
// channel funding workflow given a channel point that was constructed outside
// the main daemon.
func testExternalFundingChanPoint(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol := ht.NewNode("carol", nil)
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", nil)
	defer ht.Shutdown(dave)

	// Carol will be funding the channel, so we'll send some coins over to
	// her and ensure they have enough confirmations before we proceed.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	ht.EnsureConnected(carol, dave)

	// At this point, we're ready to simulate our external channel funding
	// flow. To start with, we'll create a pending channel with a shim for
	// a transaction that will never be published.
	const thawHeight uint32 = 10
	const chanSize = funding.MaxBtcFundingAmount
	fundingShim1, chanPoint1, _ := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, false,
	)
	ht.OpenChannelStreamAndAssert(
		carol, dave, lntest.OpenChannelParams{
			Amt:         chanSize,
			FundingShim: fundingShim1,
		},
	)
	ht.AssertNumOpenChannelsPending(carol, dave, 1)

	// That channel is now pending forever and normally would saturate the
	// max pending channel limit for both nodes. But because the channel is
	// externally funded, we should still be able to open another one. Let's
	// do exactly that now. For this one we publish the transaction so we
	// can mine it later.
	fundingShim2, chanPoint2, _ := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, true,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan, _ := basicChannelFundingTest(
		ht, carol, dave, fundingShim2,
	)

	// Both channels should be marked as frozen with the proper thaw
	// height.
	require.Equal(ht, thawHeight, carolChan.ThawHeight,
		"thaw height unmatched")
	require.Equal(ht, thawHeight, daveChan.ThawHeight,
		"thaw height unmatched")

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, dave)
	// TODO(yy): refactor the method to get rid of the RouterClient.
	err := completePaymentRequests(
		carol, carol.RouterClient, []string{resp.PaymentRequest}, true,
	)
	require.NoError(ht, err)

	// Now that the channels are open, and we've confirmed that they're
	// operational, we'll now ensure that the channels are frozen as
	// intended (if requested).
	//
	// First, we'll try to close the channel as Carol, the initiator. This
	// should fail as a frozen channel only allows the responder to
	// initiate a channel close.
	ht.CloseChannelAssertErr(carol, chanPoint2, false)

	// Next we'll try but this time with Dave (the responder) as the
	// initiator. This time the channel should be closed as normal.
	ht.CloseChannel(dave, chanPoint2, false)

	// As a last step, we check if we still have the pending channel hanging
	// around because we never published the funding TX.
	ht.AssertNumOpenChannelsPending(carol, dave, 1)

	// Let's make sure we can abandon it.
	ht.AbandonChannel(carol, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	ht.AbandonChannel(dave, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})

	// It should now not appear in the pending channels anymore.
	ht.AssertNumOpenChannelsPending(carol, dave, 0)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(ht *lntest.HarnessTest) {
	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(0)

	// As we need to create a channel that requires more than 1
	// confirmation before it's open, with the current set of defaults,
	// we'll need to create a new node instance.
	const numConfs = 5
	carolArgs := []string{
		fmt.Sprintf("--bitcoin.defaultchanconfs=%v", numConfs),
	}
	carol := ht.NewNode("Carol", carolArgs)

	// Clean up carol's node when the test finishes.
	defer ht.Shutdown(carol)

	alice := ht.Alice()
	ht.ConnectNodes(alice, carol)

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	pendingUpdate := ht.OpenPendingChannel(alice, carol, chanAmt, pushAmt)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ht.AssertNumOpenChannelsPending(alice, carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	ht.RestartNode(alice, nil)
	ht.RestartNode(carol, nil)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	require.NoError(ht, err, "unable to convert funding txid "+
		"into chainhash.Hash")
	fundingTxStr := fundingTxID.String()

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, fundingTxID)

	// Get the height that our transaction confirmed at.
	_, height := ht.GetBestBlock()

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	ht.RestartNode(alice, nil)
	ht.RestartNode(carol, nil)

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	ht.EnsureConnected(alice, carol)

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	ht.MineBlocks(3)

	// Assert that our wallet has our opening transaction with a label
	// that does not have a channel ID set yet, because we have not
	// reached our required confirmations.
	tx := ht.AssertTxAtHeight(alice, height, fundingTxStr)

	// At this stage, we expect the transaction to be labelled, but not with
	// our channel ID because our transaction has not yet confirmed.
	label := labels.MakeLabel(labels.LabelTypeChannelOpen, nil)
	require.Equal(ht, label, tx.Label, "open channel label wrong")

	// Both nodes should still show a single channel as pending.
	ht.AssertNumOpenChannelsPending(alice, carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	ht.MineBlocks(1)

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	ht.AssertNumOpenChannelsPending(alice, carol, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := lntest.ChanPointFromPendingUpdate(pendingUpdate)

	// Re-lookup our transaction in the block that it confirmed in.
	tx = ht.AssertTxAtHeight(alice, height, fundingTxStr)

	// Check both nodes to ensure that the channel is ready for operation.
	chanAlice := ht.AssertChannelExists(alice, chanPoint)
	ht.AssertChannelExists(carol, chanPoint)

	// Create an additional check for our channel assertion that will
	// check that our label is as expected.
	shortChanID := lnwire.NewShortChanIDFromInt(chanAlice.ChanId)
	label = labels.MakeLabel(labels.LabelTypeChannelOpen, &shortChanID)
	require.Equal(ht, label, tx.Label, "open channel label not updated")

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint, false)
}

// deriveFundingShim creates a channel funding shim by deriving the necessary
// keys on both sides.
func deriveFundingShim(ht *lntest.HarnessTest,
	carol, dave *lntest.HarnessNode, chanSize btcutil.Amount,
	thawHeight uint32, publish bool) (*lnrpc.FundingShim,
	*lnrpc.ChannelPoint, *chainhash.Hash) {

	keyLoc := &signrpc.KeyLocator{KeyFamily: 9999}
	carolFundingKey := ht.DeriveKey(carol, keyLoc)
	daveFundingKey := ht.DeriveKey(dave, keyLoc)

	// Now that we have the multi-sig keys for each party, we can manually
	// construct the funding transaction. We'll instruct the backend to
	// immediately create and broadcast a transaction paying out an exact
	// amount. Normally this would reside in the mempool, but we just
	// confirm it now for simplicity.
	_, fundingOutput, err := input.GenFundingPkScript(
		carolFundingKey.RawKeyBytes, daveFundingKey.RawKeyBytes,
		int64(chanSize),
	)
	require.NoError(ht, err)

	var txid *chainhash.Hash
	targetOutputs := []*wire.TxOut{fundingOutput}
	if publish {
		txid = ht.SendOutputsWithoutChange(targetOutputs, 5)
	} else {
		tx := ht.CreateTransaction(targetOutputs, 5)

		txHash := tx.TxHash()
		txid = &txHash
	}

	// At this point, we can being our external channel funding workflow.
	// We'll start by generating a pending channel ID externally that will
	// be used to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// Now that we have the pending channel ID, Dave (our responder) will
	// register the intent to receive a new channel funding workflow using
	// the pending channel ID.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: txid[:],
		},
	}
	chanPointShim := &lnrpc.ChanPointShim{
		Amt:       int64(chanSize),
		ChanPoint: chanPoint,
		LocalKey: &lnrpc.KeyDescriptor{
			RawKeyBytes: daveFundingKey.RawKeyBytes,
			KeyLoc: &lnrpc.KeyLocator{
				KeyFamily: daveFundingKey.KeyLoc.KeyFamily,
				KeyIndex:  daveFundingKey.KeyLoc.KeyIndex,
			},
		},
		RemoteKey:     carolFundingKey.RawKeyBytes,
		PendingChanId: pendingChanID,
		ThawHeight:    thawHeight,
	}
	fundingShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}
	ht.FundingStateStep(dave, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})

	// If we attempt to register the same shim (has the same pending chan
	// ID), then we should get an error.
	ht.FundingStateStepAssertErr(dave, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})

	// We'll take the chan point shim we just registered for Dave (the
	// responder), and swap the local/remote keys before we feed it in as
	// Carol's funding shim as the initiator.
	fundingShim.GetChanPointShim().LocalKey = &lnrpc.KeyDescriptor{
		RawKeyBytes: carolFundingKey.RawKeyBytes,
		KeyLoc: &lnrpc.KeyLocator{
			KeyFamily: carolFundingKey.KeyLoc.KeyFamily,
			KeyIndex:  carolFundingKey.KeyLoc.KeyIndex,
		},
	}
	fundingShim.GetChanPointShim().RemoteKey = daveFundingKey.RawKeyBytes

	return fundingShim, chanPoint, txid
}

// testBatchChanFunding makes sure multiple channels can be opened in one batch
// transaction in an atomic way.
func testBatchChanFunding(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channels
	// to during this test. Carol has a high minimum funding amount that
	// we'll use to trigger an error during the batch channel open.
	carol := ht.NewNode("carol", []string{"--minchansize=200000"})
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", nil)
	defer ht.Shutdown(dave)

	alice, bob := ht.Alice(), ht.Bob()

	// Before we start the test, we'll ensure Alice is connected to Carol
	// and Dave so she can open channels to both of them (and Bob).
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(alice, carol)
	ht.EnsureConnected(alice, dave)

	// Let's create our batch TX request. This first one should fail as we
	// open a channel to Carol that is too small for her min chan size.
	batchReq := &lnrpc.BatchOpenChannelRequest{
		SatPerVbyte: 12,
		MinConfs:    1,
		Channels: []*lnrpc.BatchOpenChannel{{
			NodePubkey:         bob.PubKey[:],
			LocalFundingAmount: 100_000,
		}, {
			NodePubkey:         carol.PubKey[:],
			LocalFundingAmount: 100_000,
		}, {
			NodePubkey:         dave.PubKey[:],
			LocalFundingAmount: 100_000,
		}},
	}

	err := ht.BatchOpenChannelAssertErr(alice, batchReq)
	require.Contains(ht, err.Error(), "initial negotiation failed")

	// Let's fix the minimum amount for Alice now and try again.
	batchReq.Channels[1].LocalFundingAmount = 200_000
	batchResp := ht.BatchOpenChannel(alice, batchReq)
	require.Len(ht, batchResp.PendingChannels, 3)

	txHash, err := chainhash.NewHash(batchResp.PendingChannels[0].Txid)
	require.NoError(ht, err)

	chanPoint1 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: batchResp.PendingChannels[0].Txid,
		},
		OutputIndex: batchResp.PendingChannels[0].OutputIndex,
	}
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: batchResp.PendingChannels[1].Txid,
		},
		OutputIndex: batchResp.PendingChannels[1].OutputIndex,
	}
	chanPoint3 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: batchResp.PendingChannels[2].Txid,
		},
		OutputIndex: batchResp.PendingChannels[2].OutputIndex,
	}

	block := ht.MineBlocksAndAssertTx(6, 1)[0]
	ht.AssertTxInBlock(block, txHash)
	ht.AssertChannelOpen(alice, chanPoint1)
	ht.AssertChannelOpen(alice, chanPoint2)
	ht.AssertChannelOpen(alice, chanPoint3)

	// With the channel open, ensure that it is counted towards Alice's
	// total channel balance.
	balRes := ht.GetChannelBalance(alice)
	require.NotEqual(ht, int64(0), balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, carol)
	// TODO(yy): refactor completePaymentRequests to get rid of
	// RouterClient.
	err = completePaymentRequests(
		alice, alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	require.NoError(ht, err)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(alice, chanPoint1, false)
	ht.CloseChannel(alice, chanPoint2, false)
	ht.CloseChannel(alice, chanPoint3, false)
}
