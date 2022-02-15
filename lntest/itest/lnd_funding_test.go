package itest

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {
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
		carol := net.NewNode(t.t, "Carol", carolArgs)
		defer shutdownAndAssert(net, t, carol)

		// Each time, we'll send Carol a new set of coins in order to
		// fund the channel.
		net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

		daveArgs := nodeArgsForCommitType(daveCommitType)
		dave := net.NewNode(t.t, "Dave", daveArgs)
		defer shutdownAndAssert(net, t, dave)

		// Before we start the test, we'll ensure both sides are
		// connected to the funding flow can properly be executed.
		net.EnsureConnected(t.t, carol, dave)

		carolChan, daveChan, closeChan, err := basicChannelFundingTest(
			t, net, carol, dave, nil,
		)
		require.NoError(t.t, err, "failed funding flow")

		// Both nodes should report the same commitment
		// type.
		chansCommitType := carolChan.CommitmentType
		require.Equal(
			t.t, chansCommitType,
			daveChan.CommitmentType,
			"commit types don't match",
		)

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
			t.Fatalf("invalid commit type %v", daveCommitType)
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
			t.Fatalf("expected nodes to signal "+
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

			logLine := fmt.Sprintf(
				"---- basic channel funding subtest %s ----\n",
				testName,
			)
			net.Alice.AddToLogf(logLine)

			success := t.t.Run(testName, func(t *testing.T) {
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
func basicChannelFundingTest(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode, bob *lntest.HarnessNode,
	fundingShim *lnrpc.FundingShim) (*lnrpc.Channel, *lnrpc.Channel,
	func(), error) {

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	satPerVbyte := btcutil.Amount(1)

	// Record nodes' channel balance before testing.
	aliceChannelBalance := getChannelBalance(t, alice)
	bobChannelBalance := getChannelBalance(t, bob)

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
		assertChannelBalanceResp(t, node, newResp)
	}

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	chanPoint := openChannelAndAssert(
		t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt:         chanAmt,
			PushAmt:     pushAmt,
			FundingShim: fundingShim,
			SatPerVByte: satPerVbyte,
		},
	)

	err := alice.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err, "alice didn't report channel")

	err = bob.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err, "bob didn't report channel")

	cType, err := channelCommitType(alice, chanPoint)
	require.NoError(t.t, err, "unable to get channel type")

	// With the channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	aliceLocalBalance := chanAmt - pushAmt - calcStaticFee(cType, 0)
	checkChannelBalance(
		alice, aliceChannelBalance, aliceLocalBalance, pushAmt,
	)
	checkChannelBalance(
		bob, bobChannelBalance, pushAmt, aliceLocalBalance,
	)

	req := &lnrpc.ListChannelsRequest{}
	aliceChannel, err := alice.ListChannels(context.Background(), req)
	require.NoError(t.t, err, "unable to obtain chan")

	bobChannel, err := bob.ListChannels(context.Background(), req)
	require.NoError(t.t, err, "unable to obtain chan")

	closeChan := func() {
		// Finally, immediately close the channel. This function will
		// also block until the channel is closed and will additionally
		// assert the relevant channel closing post conditions.
		closeChannelAndAssert(t, net, alice, chanPoint, false)
	}

	return aliceChannel.Channels[0], bobChannel.Channels[0], closeChan, nil
}

// testUnconfirmedChannelFunding tests that our unconfirmed change outputs can
// be used to fund channels.
func testUnconfirmedChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const (
		chanAmt = funding.MaxBtcFundingAmount
		pushAmt = btcutil.Amount(100000)
	)

	// We'll start off by creating a node for Carol.
	carol := net.NewNode(t.t, "Carol", nil)
	defer shutdownAndAssert(net, t, carol)

	// We'll send her some confirmed funds.
	net.SendCoins(t.t, 2*chanAmt, carol)

	// Now let Carol send some funds to herself, making a unconfirmed
	// change output.
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := carol.NewAddress(ctxt, addrReq)
	require.NoError(t.t, err, "unable to get new address")

	sendReq := &lnrpc.SendCoinsRequest{
		Addr:   resp.Address,
		Amount: int64(chanAmt) / 5,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.SendCoins(ctxt, sendReq)
	require.NoError(t.t, err, "unable to send coins")

	// Make sure the unconfirmed tx is seen in the mempool.
	_, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err, "failed to find tx in miner mempool")

	// Now, we'll connect her to Alice so that they can open a channel
	// together. The funding flow should select Carol's unconfirmed output
	// as she doesn't have any other funds since it's a new node.
	net.ConnectNodes(t.t, carol, net.Alice)

	chanOpenUpdate := openChannelStream(
		t, net, carol, net.Alice,
		lntest.OpenChannelParams{
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
		assertChannelBalanceResp(t, node, expectedResponse)
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
	checkChannelBalance(net.Alice, 0, 0, pushAmt, carolLocalBalance)

	// Confirm the channel and wait for it to be recognized by both
	// parties. Two transactions should be mined, the unconfirmed spend and
	// the funding tx.
	mineBlocks(t, net, 6, 2)
	chanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
	require.NoError(t.t, err, "error while waitinng for channel open")

	// With the channel open, we'll check the balances on each side of the
	// channel as a sanity check to ensure things worked out as intended.
	checkChannelBalance(carol, carolLocalBalance, pushAmt, 0, 0)
	checkChannelBalance(net.Alice, pushAmt, carolLocalBalance, 0, 0)

	// Now that we're done with the test, the channel can be closed.
	closeChannelAndAssert(t, net, carol, chanPoint, false)
}

// testExternalFundingChanPoint tests that we're able to carry out a normal
// channel funding workflow given a channel point that was constructed outside
// the main daemon.
func testExternalFundingChanPoint(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Carol will be funding the channel, so we'll send some coins over to
	// her and ensure they have enough confirmations before we proceed.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	net.EnsureConnected(t.t, carol, dave)

	// At this point, we're ready to simulate our external channel funding
	// flow. To start with, we'll create a pending channel with a shim for
	// a transaction that will never be published.
	const thawHeight uint32 = 10
	const chanSize = funding.MaxBtcFundingAmount
	fundingShim1, chanPoint1, _ := deriveFundingShim(
		net, t, carol, dave, chanSize, thawHeight, false,
	)
	_ = openChannelStream(
		t, net, carol, dave, lntest.OpenChannelParams{
			Amt:         chanSize,
			FundingShim: fundingShim1,
		},
	)
	assertNumOpenChannelsPending(t, carol, dave, 1)

	// That channel is now pending forever and normally would saturate the
	// max pending channel limit for both nodes. But because the channel is
	// externally funded, we should still be able to open another one. Let's
	// do exactly that now. For this one we publish the transaction so we
	// can mine it later.
	fundingShim2, chanPoint2, _ := deriveFundingShim(
		net, t, carol, dave, chanSize, thawHeight, true,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan, _, err := basicChannelFundingTest(
		t, net, carol, dave, fundingShim2,
	)
	require.NoError(t.t, err)

	// Both channels should be marked as frozen with the proper thaw
	// height.
	require.Equal(
		t.t, thawHeight, carolChan.ThawHeight, "thaw height unmatched",
	)
	require.Equal(
		t.t, thawHeight, daveChan.ThawHeight, "thaw height unmatched",
	)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := dave.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	err = completePaymentRequests(
		carol, carol.RouterClient, []string{resp.PaymentRequest}, true,
	)
	require.NoError(t.t, err)

	// Now that the channels are open, and we've confirmed that they're
	// operational, we'll now ensure that the channels are frozen as
	// intended (if requested).
	//
	// First, we'll try to close the channel as Carol, the initiator. This
	// should fail as a frozen channel only allows the responder to
	// initiate a channel close.
	_, _, err = net.CloseChannel(carol, chanPoint2, false)
	require.Error(t.t, err,
		"carol wasn't denied a co-op close attempt "+
			"for a frozen channel",
	)

	// Next we'll try but this time with Dave (the responder) as the
	// initiator. This time the channel should be closed as normal.
	closeChannelAndAssert(t, net, dave, chanPoint2, false)

	// As a last step, we check if we still have the pending channel hanging
	// around because we never published the funding TX.
	assertNumOpenChannelsPending(t, carol, dave, 1)

	// Let's make sure we can abandon it.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.AbandonChannel(ctxt, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	require.NoError(t.t, err)
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.AbandonChannel(ctxt, &lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	require.NoError(t.t, err)

	// It should now not appear in the pending channels anymore.
	assertNumOpenChannelsPending(t, carol, dave, 0)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(0)

	// As we need to create a channel that requires more than 1
	// confirmation before it's open, with the current set of defaults,
	// we'll need to create a new node instance.
	const numConfs = 5
	carolArgs := []string{fmt.Sprintf("--bitcoin.defaultchanconfs=%v", numConfs)}
	carol := net.NewNode(t.t, "Carol", carolArgs)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	net.ConnectNodes(t.t, net.Alice, carol)

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	pendingUpdate, err := net.OpenPendingChannel(
		net.Alice, carol, chanAmt, pushAmt,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	assertNumOpenChannelsPending(t, net.Alice, carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}
	fundingTxStr := fundingTxID.String()

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	// Get the height that our transaction confirmed at.
	_, height, err := net.Miner.Client.GetBestBlock()
	require.NoError(t.t, err, "could not get best block")

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	net.EnsureConnected(t.t, net.Alice, carol)

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	if _, err := net.Miner.Client.Generate(3); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Assert that our wallet has our opening transaction with a label
	// that does not have a channel ID set yet, because we have not
	// reached our required confirmations.
	tx := findTxAtHeight(t, height, fundingTxStr, net.Alice)

	// At this stage, we expect the transaction to be labelled, but not with
	// our channel ID because our transaction has not yet confirmed.
	label := labels.MakeLabel(labels.LabelTypeChannelOpen, nil)
	require.Equal(t.t, label, tx.Label, "open channel label wrong")

	// Both nodes should still show a single channel as pending.
	time.Sleep(time.Second * 1)
	assertNumOpenChannelsPending(t, net.Alice, carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	if _, err := net.Miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Second * 1)
	assertNumOpenChannelsPending(t, net.Alice, carol, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Re-lookup our transaction in the block that it confirmed in.
	tx = findTxAtHeight(t, height, fundingTxStr, net.Alice)

	// Create an additional check for our channel assertion that will
	// check that our label is as expected.
	check := func(channel *lnrpc.Channel) {
		shortChanID := lnwire.NewShortChanIDFromInt(
			channel.ChanId,
		)

		label := labels.MakeLabel(
			labels.LabelTypeChannelOpen, &shortChanID,
		)
		require.Equal(t.t, label, tx.Label,
			"open channel label not updated")
	}

	// Check both nodes to ensure that the channel is ready for operation.
	err = net.AssertChannelExists(net.Alice, &outPoint, check)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	if err := net.AssertChannelExists(carol, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}
	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

// deriveFundingShim creates a channel funding shim by deriving the necessary
// keys on both sides.
func deriveFundingShim(net *lntest.NetworkHarness, t *harnessTest,
	carol, dave *lntest.HarnessNode, chanSize btcutil.Amount,
	thawHeight uint32, publish bool) (*lnrpc.FundingShim,
	*lnrpc.ChannelPoint, *chainhash.Hash) {

	ctxb := context.Background()
	keyLoc := &walletrpc.KeyReq{KeyFamily: 9999}
	carolFundingKey, err := carol.WalletKitClient.DeriveNextKey(ctxb, keyLoc)
	require.NoError(t.t, err)
	daveFundingKey, err := dave.WalletKitClient.DeriveNextKey(ctxb, keyLoc)
	require.NoError(t.t, err)

	// Now that we have the multi-sig keys for each party, we can manually
	// construct the funding transaction. We'll instruct the backend to
	// immediately create and broadcast a transaction paying out an exact
	// amount. Normally this would reside in the mempool, but we just
	// confirm it now for simplicity.
	_, fundingOutput, err := input.GenFundingPkScript(
		carolFundingKey.RawKeyBytes, daveFundingKey.RawKeyBytes,
		int64(chanSize),
	)
	require.NoError(t.t, err)

	var txid *chainhash.Hash
	targetOutputs := []*wire.TxOut{fundingOutput}
	if publish {
		txid, err = net.Miner.SendOutputsWithoutChange(
			targetOutputs, 5,
		)
		require.NoError(t.t, err)
	} else {
		tx, err := net.Miner.CreateTransaction(targetOutputs, 5, false)
		require.NoError(t.t, err)

		txHash := tx.TxHash()
		txid = &txHash
	}

	// At this point, we can being our external channel funding workflow.
	// We'll start by generating a pending channel ID externally that will
	// be used to track this new funding type.
	var pendingChanID [32]byte
	_, err = rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

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
		PendingChanId: pendingChanID[:],
		ThawHeight:    thawHeight,
	}
	fundingShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}
	_, err = dave.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})
	require.NoError(t.t, err)

	// If we attempt to register the same shim (has the same pending chan
	// ID), then we should get an error.
	_, err = dave.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})
	if err == nil {
		t.Fatalf("duplicate pending channel ID funding shim " +
			"registration should trigger an error")
	}

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
func testBatchChanFunding(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// First, we'll create two new nodes that we'll use to open channels
	// to during this test. Carol has a high minimum funding amount that
	// we'll use to trigger an error during the batch channel open.
	carol := net.NewNode(t.t, "carol", []string{"--minchansize=200000"})
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Before we start the test, we'll ensure Alice is connected to Carol
	// and Dave so she can open channels to both of them (and Bob).
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	net.EnsureConnected(t.t, net.Alice, carol)
	net.EnsureConnected(t.t, net.Alice, dave)

	// Let's create our batch TX request. This first one should fail as we
	// open a channel to Carol that is too small for her min chan size.
	batchReq := &lnrpc.BatchOpenChannelRequest{
		SatPerVbyte: 12,
		MinConfs:    1,
		Channels: []*lnrpc.BatchOpenChannel{{
			NodePubkey:         net.Bob.PubKey[:],
			LocalFundingAmount: 100_000,
		}, {
			NodePubkey:         carol.PubKey[:],
			LocalFundingAmount: 100_000,
		}, {
			NodePubkey:         dave.PubKey[:],
			LocalFundingAmount: 100_000,
		}},
	}

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	_, err := net.Alice.BatchOpenChannel(ctxt, batchReq)
	require.Error(t.t, err)
	require.Contains(t.t, err.Error(), "initial negotiation failed")

	// Let's fix the minimum amount for Carol now and try again.
	batchReq.Channels[1].LocalFundingAmount = 200_000
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	batchResp, err := net.Alice.BatchOpenChannel(ctxt, batchReq)
	require.NoError(t.t, err)
	require.Len(t.t, batchResp.PendingChannels, 3)

	txHash, err := chainhash.NewHash(batchResp.PendingChannels[0].Txid)
	require.NoError(t.t, err)

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

	block := mineBlocks(t, net, 6, 1)[0]
	assertTxInBlock(t, block, txHash)
	err = net.Alice.WaitForNetworkChannelOpen(chanPoint1)
	require.NoError(t.t, err)
	err = net.Alice.WaitForNetworkChannelOpen(chanPoint2)
	require.NoError(t.t, err)
	err = net.Alice.WaitForNetworkChannelOpen(chanPoint3)
	require.NoError(t.t, err)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balReq := &lnrpc.ChannelBalanceRequest{}
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	balRes, err := net.Alice.ChannelBalance(ctxt, balReq)
	require.NoError(t.t, err)
	require.NotEqual(t.t, int64(0), balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	resp, err := carol.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	err = completePaymentRequests(
		net.Alice, net.Alice.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	require.NoError(t.t, err)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	closeChannelAndAssert(t, net, net.Alice, chanPoint1, false)
	closeChannelAndAssert(t, net, net.Alice, chanPoint2, false)
	closeChannelAndAssert(t, net, net.Alice, chanPoint3, false)
}
