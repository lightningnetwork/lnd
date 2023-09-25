package itest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
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
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	}

	// testFunding is a function closure that takes Carol and Dave's
	// commitment types and test the funding flow.
	testFunding := func(ht *lntest.HarnessTest, carolCommitType,
		daveCommitType lnrpc.CommitmentType) {

		// Based on the current tweak variable for Carol, we'll
		// preferentially signal the legacy commitment format.  We do
		// the same for Dave shortly below.
		carolArgs := lntest.NodeArgsForCommitType(carolCommitType)
		carol := ht.NewNode("Carol", carolArgs)

		// Each time, we'll send Carol a new set of coins in order to
		// fund the channel.
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

		daveArgs := lntest.NodeArgsForCommitType(daveCommitType)
		dave := ht.NewNode("Dave", daveArgs)

		// Before we start the test, we'll ensure both sides are
		// connected to the funding flow can properly be executed.
		ht.EnsureConnected(carol, dave)

		var privateChan bool

		// If this is to be a taproot channel type, then it needs to be
		// private, otherwise it'll be rejected by Dave.
		//
		// TODO(roasbeef): lift after gossip 1.75
		if carolCommitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
			privateChan = true
		}

		// If carol wants taproot, but dave wants something
		// else, then we'll assert that the channel negotiation
		// attempt fails.
		if carolCommitType == lnrpc.CommitmentType_SIMPLE_TAPROOT &&
			daveCommitType != lnrpc.CommitmentType_SIMPLE_TAPROOT {

			expectedErr := fmt.Errorf("requested channel type " +
				"not supported")
			amt := funding.MaxBtcFundingAmount
			ht.OpenChannelAssertErr(
				carol, dave, lntest.OpenChannelParams{
					Private:        privateChan,
					Amt:            amt,
					CommitmentType: carolCommitType,
				}, expectedErr,
			)

			return
		}

		carolChan, daveChan, closeChan := basicChannelFundingTest(
			ht, carol, dave, nil, privateChan, &carolCommitType,
		)

		// Both nodes should report the same commitment
		// type.
		chansCommitType := carolChan.CommitmentType
		require.Equal(ht, chansCommitType, daveChan.CommitmentType,
			"commit types don't match")

		// Now check that the commitment type reported by both nodes is
		// what we expect. It will be the minimum of the two nodes'
		// preference, in the order Legacy, Tweakless, Anchors.
		expType := carolCommitType

		switch daveCommitType {
		// Dave supports taproot, type will be what Carol supports.
		case lnrpc.CommitmentType_SIMPLE_TAPROOT:

		// Dave supports anchors, type will be what Carol supports.
		case lnrpc.CommitmentType_ANCHORS:
			// However if Alice wants taproot chans, then we
			// downgrade to anchors as this is still using implicit
			// negotiation.
			if expType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
				expType = lnrpc.CommitmentType_ANCHORS
			}

		// Dave only supports tweakless, channel will be downgraded to
		// this type if Carol supports anchors.
		case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
			switch expType {
			case lnrpc.CommitmentType_ANCHORS:
				expType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
			case lnrpc.CommitmentType_SIMPLE_TAPROOT:
				expType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
			}

		// Dave only supports legacy type, channel will be downgraded
		// to this type.
		case lnrpc.CommitmentType_LEGACY:
			expType = lnrpc.CommitmentType_LEGACY

		default:
			ht.Fatalf("invalid commit type %v", daveCommitType)
		}

		// Check that the signalled type matches what we expect.
		switch {
		case expType == lnrpc.CommitmentType_ANCHORS &&
			chansCommitType == lnrpc.CommitmentType_ANCHORS:

		case expType == lnrpc.CommitmentType_STATIC_REMOTE_KEY &&
			chansCommitType == lnrpc.CommitmentType_STATIC_REMOTE_KEY: //nolint:lll

		case expType == lnrpc.CommitmentType_LEGACY &&
			chansCommitType == lnrpc.CommitmentType_LEGACY:

		case expType == lnrpc.CommitmentType_SIMPLE_TAPROOT &&
			chansCommitType == lnrpc.CommitmentType_SIMPLE_TAPROOT:

		default:
			ht.Fatalf("expected nodes to signal "+
				"commit type %v, instead got "+
				"%v", expType, chansCommitType)
		}

		// As we've concluded this sub-test case we'll now close out
		// the channel for both sides.
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
				st := ht.Subtest(t)
				testFunding(st, cc, dc)
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
	alice, bob *node.HarnessNode, fundingShim *lnrpc.FundingShim,
	privateChan bool, commitType *lnrpc.CommitmentType) (*lnrpc.Channel,
	*lnrpc.Channel, func()) {

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	satPerVbyte := btcutil.Amount(1)

	// Record nodes' channel balance before testing.
	aliceChannelBalance := alice.RPC.ChannelBalance()
	bobChannelBalance := bob.RPC.ChannelBalance()

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *node.HarnessNode,
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
		newResp.Balance += int64(local)
		ht.AssertChannelBalanceResp(node, newResp)
	}

	// For taproot channels, the only way we can negotiate is using the
	// explicit commitment type. This allows us to continue supporting the
	// existing min version comparison for implicit negotiation.
	var commitTypeParam lnrpc.CommitmentType
	if commitType != nil &&
		*commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {

		commitTypeParam = *commitType
	}

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	chanPoint := ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Private:        privateChan,
		Amt:            chanAmt,
		PushAmt:        pushAmt,
		FundingShim:    fundingShim,
		SatPerVByte:    satPerVbyte,
		CommitmentType: commitTypeParam,
	})

	cType := ht.GetChannelCommitType(alice, chanPoint)

	// With the channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	aliceLocalBalance := chanAmt - pushAmt - lntest.CalcStaticFee(cType, 0)
	checkChannelBalance(
		alice, aliceChannelBalance, aliceLocalBalance, pushAmt,
	)
	checkChannelBalance(
		bob, bobChannelBalance, pushAmt, aliceLocalBalance,
	)

	aliceChannel := ht.GetChannelByChanPoint(alice, chanPoint)
	bobChannel := ht.GetChannelByChanPoint(bob, chanPoint)

	closeChan := func() {
		// Finally, immediately close the channel. This function will
		// also block until the channel is closed and will additionally
		// assert the relevant channel closing post conditions.
		ht.CloseChannel(alice, chanPoint)
	}

	return aliceChannel, bobChannel, closeChan
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

	alice := ht.Alice

	// We'll send her some unconfirmed funds.
	ht.FundCoinsUnconfirmed(2*chanAmt, carol)

	// Now, we'll connect her to Alice so that they can open a channel
	// together. The funding flow should select Carol's unconfirmed output
	// as she doesn't have any other funds since it's a new node.
	ht.ConnectNodes(carol, alice)

	chanOpenUpdate := ht.OpenChannelAssertStream(
		carol, alice, lntest.OpenChannelParams{
			Amt:              chanAmt,
			PushAmt:          pushAmt,
			SpendUnconfirmed: true,
		},
	)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *node.HarnessNode,
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
	carolLocalBalance := chanAmt - pushAmt - lntest.CalcStaticFee(cType, 0)
	checkChannelBalance(carol, 0, 0, carolLocalBalance, pushAmt)

	// For Alice, her local/remote balances should be zero, and the
	// local/remote balances are the mirror of Carol's.
	checkChannelBalance(alice, 0, 0, pushAmt, carolLocalBalance)

	// Confirm the channel and wait for it to be recognized by both
	// parties. For neutrino backend, the funding transaction should be
	// mined. Otherwise, two transactions should be mined, the unconfirmed
	// spend and the funding tx.
	if ht.IsNeutrinoBackend() {
		ht.MineBlocksAndAssertNumTxes(6, 1)
	} else {
		ht.MineBlocksAndAssertNumTxes(6, 2)
	}

	chanPoint := ht.WaitForChannelOpenEvent(chanOpenUpdate)

	// With the channel open, we'll check the balances on each side of the
	// channel as a sanity check to ensure things worked out as intended.
	checkChannelBalance(carol, carolLocalBalance, pushAmt, 0, 0)
	checkChannelBalance(alice, pushAmt, carolLocalBalance, 0, 0)

	// TODO(yy): remove the sleep once the following bug is fixed.
	//
	// We may get the error `unable to gracefully close channel while peer
	// is offline (try force closing it instead): channel link not found`.
	// This happens because the channel link hasn't been added yet but we
	// now proceed to closing the channel. We may need to revisit how the
	// channel open event is created and make sure the event is only sent
	// after all relevant states have been updated.
	time.Sleep(2 * time.Second)

	// Now that we're done with the test, the channel can be closed.
	ht.CloseChannel(carol, chanPoint)
}

// testChannelFundingInputTypes tests that any type of supported input type can
// be used to fund channels.
func testChannelFundingInputTypes(ht *lntest.HarnessTest) {
	// We'll start off by creating a node for Carol.
	carol := ht.NewNode("Carol", nil)

	// Now, we'll connect her to Alice so that they can open a
	// channel together.
	ht.ConnectNodes(carol, ht.Alice)

	runChannelFundingInputTypes(ht, ht.Alice, carol)
}

// runChannelFundingInputTypes tests that any type of supported input type can
// be used to fund channels.
func runChannelFundingInputTypes(ht *lntest.HarnessTest, alice,
	carol *node.HarnessNode) {

	const (
		chanAmt  = funding.MaxBtcFundingAmount
		burnAddr = "bcrt1qxsnqpdc842lu8c0xlllgvejt6rhy49u6fmpgyz"
	)

	fundMixed := func(amt btcutil.Amount, target *node.HarnessNode) {
		ht.FundCoins(amt/5, target)
		ht.FundCoins(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)
	}
	fundMultipleP2TR := func(amt btcutil.Amount, target *node.HarnessNode) {
		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)
	}
	fundWithTypes := []func(amt btcutil.Amount, target *node.HarnessNode){
		ht.FundCoins, ht.FundCoinsNP2WKH, ht.FundCoinsP2TR, fundMixed,
		fundMultipleP2TR,
	}

	// Creates a helper closure to be used below which asserts the
	// proper response to a channel balance RPC.
	checkChannelBalance := func(node *node.HarnessNode, local,
		remote, pendingLocal, pendingRemote btcutil.Amount) {

		expectedResponse := &lnrpc.ChannelBalanceResponse{
			LocalBalance: &lnrpc.Amount{
				Sat:  uint64(local),
				Msat: uint64(lnwire.NewMSatFromSatoshis(local)),
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

	for _, funder := range fundWithTypes {
		// We'll send her some confirmed funds. We send 10% more than
		// we need to account for fees.
		funder((chanAmt*11)/10, carol)

		chanOpenUpdate := ht.OpenChannelAssertStream(
			carol, alice, lntest.OpenChannelParams{
				Amt: chanAmt,
			},
		)

		// As the channel is pending open, it's expected Carol has both
		// zero local and remote balances, and pending local/remote
		// should not be zero.
		//
		// Note that atm we haven't obtained the chanPoint yet, so we
		// use the type directly.
		cType := lnrpc.CommitmentType_STATIC_REMOTE_KEY
		carolLocalBalance := chanAmt - lntest.CalcStaticFee(cType, 0)
		checkChannelBalance(carol, 0, 0, carolLocalBalance, 0)

		// For Alice, her local/remote balances should be zero, and the
		// local/remote balances are the mirror of Carol's.
		checkChannelBalance(alice, 0, 0, 0, carolLocalBalance)

		// Confirm the channel and wait for it to be recognized by both
		// parties. Two transactions should be mined, the unconfirmed
		// spend and the funding tx.
		ht.MineBlocksAndAssertNumTxes(1, 1)
		chanPoint := ht.WaitForChannelOpenEvent(chanOpenUpdate)

		// With the channel open, we'll check the balances on each side
		// of the channel as a sanity check to ensure things worked out
		// as intended.
		checkChannelBalance(carol, carolLocalBalance, 0, 0, 0)
		checkChannelBalance(alice, 0, carolLocalBalance, 0, 0)

		// TODO(yy): remove the sleep once the following bug is fixed.
		//
		// We may get the error `unable to gracefully close channel
		// while peer is offline (try force closing it instead):
		// channel link not found`. This happens because the channel
		// link hasn't been added yet but we now proceed to closing the
		// channel. We may need to revisit how the channel open event
		// is created and make sure the event is only sent after all
		// relevant states have been updated.
		time.Sleep(2 * time.Second)

		// Now that we're done with the test, the channel can be closed.
		ht.CloseChannel(carol, chanPoint)

		// Empty out the wallet so there aren't any lingering coins.
		sendAllCoinsConfirm(ht, carol, burnAddr)
	}
}

// sendAllCoinsConfirm sends all coins of the node's wallet to the given address
// and awaits one confirmation.
func sendAllCoinsConfirm(ht *lntest.HarnessTest, node *node.HarnessNode,
	addr string) {

	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:    addr,
		SendAll: true,
	}
	node.RPC.SendCoins(sweepReq)
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// testExternalFundingChanPoint tests that we're able to carry out a normal
// channel funding workflow given a channel point that was constructed outside
// the main daemon.
func testExternalFundingChanPoint(ht *lntest.HarnessTest) {
	runExternalFundingScriptEnforced(ht)
	runExternalFundingTaproot(ht)
}

// runExternalFundingChanPoint runs the actual test that tests we're able to
// carry out a normal channel funding workflow given a channel point that was
// constructed outside the main daemon for the script enforced channel type.
func runExternalFundingScriptEnforced(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)
	commitmentType := lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

	// Carol will be funding the channel, so we'll send some coins over to
	// her and ensure they have enough confirmations before we proceed.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	ht.EnsureConnected(carol, dave)

	// At this point, we're ready to simulate our external channel funding
	// flow. To start with, we'll create a pending channel with a shim for
	// a transaction that will never be published.
	const thawHeight uint32 = 10
	const chanSize = funding.MaxBtcFundingAmount
	fundingShim1, chanPoint1 := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, false, commitmentType,
	)
	ht.OpenChannelAssertPending(
		carol, dave, lntest.OpenChannelParams{
			Amt:         chanSize,
			FundingShim: fundingShim1,
		},
	)
	ht.AssertNodesNumPendingOpenChannels(carol, dave, 1)

	// That channel is now pending forever and normally would saturate the
	// max pending channel limit for both nodes. But because the channel is
	// externally funded, we should still be able to open another one. Let's
	// do exactly that now. For this one we publish the transaction so we
	// can mine it later.
	fundingShim2, chanPoint2 := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, true, commitmentType,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan, _ := basicChannelFundingTest(
		ht, carol, dave, fundingShim2, false, nil,
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
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})

	// Now that the channels are open, and we've confirmed that they're
	// operational, we'll now ensure that the channels are frozen as
	// intended (if requested).
	//
	// First, we'll try to close the channel as Carol, the initiator. This
	// should fail as a frozen channel only allows the responder to
	// initiate a channel close.
	err := ht.CloseChannelAssertErr(carol, chanPoint2, false)
	require.Contains(ht, err.Error(), "cannot co-op close frozen channel")

	// Before Dave closes the channel, he needs to check the invoice is
	// settled to avoid an error saying cannot close channel due to active
	// HTLCs.
	ht.AssertInvoiceSettled(dave, resp.PaymentAddr)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	// Next we'll try but this time with Dave (the responder) as the
	// initiator. This time the channel should be closed as normal.
	ht.CloseChannel(dave, chanPoint2)

	// As a last step, we check if we still have the pending channel
	// hanging around because we never published the funding TX.
	ht.AssertNodesNumPendingOpenChannels(carol, dave, 1)

	// Let's make sure we can abandon it.
	carol.RPC.AbandonChannel(&lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	dave.RPC.AbandonChannel(&lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})

	// It should now not appear in the pending channels anymore.
	ht.AssertNodesNumPendingOpenChannels(carol, dave, 0)
}

// runExternalFundingTaproot runs the actual test that tests we're able to carry
// out a normal channel funding workflow given a channel point that was
// constructed outside the main daemon for the taproot channel type.
func runExternalFundingTaproot(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	commitmentType := lnrpc.CommitmentType_SIMPLE_TAPROOT
	args := lntest.NodeArgsForCommitType(commitmentType)
	carol := ht.NewNode("carol", args)

	// We'll attempt two channels, so Dave will need to accept two pending
	// ones.
	dave := ht.NewNode("dave", append(args, "--maxpendingchannels=2"))

	// Carol will be funding the channel, so we'll send some coins over to
	// her and ensure they have enough confirmations before we proceed.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	ht.EnsureConnected(carol, dave)

	// At this point, we're ready to simulate our external channel funding
	// flow. To start with, we'll create a pending channel with a shim for
	// a transaction that will never be published.
	const thawHeight uint32 = 10
	const chanSize = funding.MaxBtcFundingAmount
	fundingShim1, chanPoint1 := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, false, commitmentType,
	)
	ht.OpenChannelAssertPending(carol, dave, lntest.OpenChannelParams{
		Amt:            chanSize,
		FundingShim:    fundingShim1,
		CommitmentType: commitmentType,
		Private:        true,
	})
	ht.AssertNodesNumPendingOpenChannels(carol, dave, 1)

	// That channel is now pending forever and normally would saturate the
	// max pending channel limit for both nodes. But because the channel is
	// externally funded, we should still be able to open another one. Let's
	// do exactly that now. For this one we publish the transaction so we
	// can mine it later.
	fundingShim2, chanPoint2 := deriveFundingShim(
		ht, carol, dave, chanSize, thawHeight, true, commitmentType,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan, _ := basicChannelFundingTest(
		ht, carol, dave, fundingShim2, true, &commitmentType,
	)

	// The itest harness doesn't mine blocks for private channels, so we
	// want to make sure the channel with the published and mined
	// transaction leaves the pending state.
	ht.MineBlocks(6)

	rpcChanPointToStr := func(cp *lnrpc.ChannelPoint) string {
		txid, err := chainhash.NewHash(cp.GetFundingTxidBytes())
		require.NoError(ht, err)
		return fmt.Sprintf("%v:%d", txid.String(), cp.OutputIndex)
	}

	pendingCarol := carol.RPC.PendingChannels().PendingOpenChannels
	require.Len(ht, pendingCarol, 1)
	require.Equal(
		ht, rpcChanPointToStr(chanPoint1),
		pendingCarol[0].Channel.ChannelPoint,
	)
	openCarol := carol.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		ActiveOnly:  true,
		PrivateOnly: true,
	})
	require.Len(ht, openCarol.Channels, 1)
	require.Equal(
		ht, rpcChanPointToStr(chanPoint2),
		openCarol.Channels[0].ChannelPoint,
	)

	pendingDave := dave.RPC.PendingChannels().PendingOpenChannels
	require.Len(ht, pendingDave, 1)
	require.Equal(
		ht, rpcChanPointToStr(chanPoint1),
		pendingDave[0].Channel.ChannelPoint,
	)

	openDave := dave.RPC.ListChannels(&lnrpc.ListChannelsRequest{
		ActiveOnly:  true,
		PrivateOnly: true,
	})
	require.Len(ht, openDave.Channels, 1)
	require.Equal(
		ht, rpcChanPointToStr(chanPoint2),
		openDave.Channels[0].ChannelPoint,
	)

	// Both channels should be marked as frozen with the proper thaw height.
	require.EqualValues(ht, thawHeight, carolChan.ThawHeight, "thaw height")
	require.EqualValues(ht, thawHeight, daveChan.ThawHeight, "thaw height")

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})

	// Now that the channels are open, and we've confirmed that they're
	// operational, we'll now ensure that the channels are frozen as
	// intended (if requested).
	//
	// First, we'll try to close the channel as Carol, the initiator. This
	// should fail as a frozen channel only allows the responder to
	// initiate a channel close.
	err := ht.CloseChannelAssertErr(carol, chanPoint2, false)
	require.Contains(ht, err.Error(), "cannot co-op close frozen channel")

	// Before Dave closes the channel, he needs to check the invoice is
	// settled to avoid an error saying cannot close channel due to active
	// HTLCs.
	ht.AssertInvoiceSettled(dave, resp.PaymentAddr)

	// TODO(yy): remove the sleep once the following bug is fixed.
	// When the invoice is reported settled, the commitment dance is not
	// yet finished, which can cause an error when closing the channel,
	// saying there's active HTLCs. We need to investigate this issue and
	// reverse the order to, first finish the commitment dance, then report
	// the invoice as settled.
	time.Sleep(2 * time.Second)

	// Next we'll try but this time with Dave (the responder) as the
	// initiator. This time the channel should be closed as normal.
	ht.CloseChannel(dave, chanPoint2)

	// Let's make sure we can abandon it.
	carol.RPC.AbandonChannel(&lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})
	dave.RPC.AbandonChannel(&lnrpc.AbandonChannelRequest{
		ChannelPoint:           chanPoint1,
		PendingFundingShimOnly: true,
	})

	// It should now not appear in the pending channels anymore.
	ht.AssertNodesNumPendingOpenChannels(carol, dave, 0)
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

	alice := ht.Alice
	ht.ConnectNodes(alice, carol)

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	param := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: pushAmt,
	}
	update := ht.OpenChannelAssertPending(alice, carol, param)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	ht.RestartNode(alice)
	ht.RestartNode(carol)

	fundingTxID, err := chainhash.NewHash(update.Txid)
	require.NoError(ht, err, "unable to convert funding txid "+
		"into chainhash.Hash")

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, fundingTxID)

	// Get the height that our transaction confirmed at.
	_, height := ht.Miner.GetBestBlock()

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	ht.RestartNode(alice)
	ht.RestartNode(carol)

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	ht.EnsureConnected(alice, carol)

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	ht.MineBlocks(3)

	// Assert that our wallet has our opening transaction with a label
	// that does not have a channel ID set yet, because we have not
	// reached our required confirmations.
	tx := ht.AssertTxAtHeight(alice, height, fundingTxID)

	// At this stage, we expect the transaction to be labelled, but not with
	// our channel ID because our transaction has not yet confirmed.
	label := labels.MakeLabel(labels.LabelTypeChannelOpen, nil)
	require.Equal(ht, label, tx.Label, "open channel label wrong")

	// Both nodes should still show a single channel as pending.
	ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	ht.MineBlocks(1)

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	ht.AssertNumPendingOpenChannels(alice, 0)
	ht.AssertNumPendingOpenChannels(carol, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := lntest.ChanPointFromPendingUpdate(update)

	// Re-lookup our transaction in the block that it confirmed in.
	tx = ht.AssertTxAtHeight(alice, height, fundingTxID)

	// Check both nodes to ensure that the channel is ready for operation.
	chanAlice := ht.AssertChannelExists(alice, chanPoint)
	ht.AssertChannelExists(carol, chanPoint)

	// Make sure Alice and Carol have seen the channel in their network
	// topology.
	ht.AssertTopologyChannelOpen(alice, chanPoint)
	ht.AssertTopologyChannelOpen(carol, chanPoint)

	// Create an additional check for our channel assertion that will
	// check that our label is as expected.
	shortChanID := lnwire.NewShortChanIDFromInt(chanAlice.ChanId)
	label = labels.MakeLabel(labels.LabelTypeChannelOpen, &shortChanID)
	require.Equal(ht, label, tx.Label, "open channel label not updated")

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.CloseChannel(alice, chanPoint)
}

// testBatchChanFunding makes sure multiple channels can be opened in one batch
// transaction in an atomic way.
func testBatchChanFunding(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channels
	// to during this test. Carol has a high minimum funding amount that
	// we'll use to trigger an error during the batch channel open.
	carol := ht.NewNode("carol", []string{"--minchansize=200000"})
	dave := ht.NewNode("dave", nil)

	// Next we create a node that will receive a zero-conf channel open from
	// Alice. We'll create the node with the required parameters.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}
	eve := ht.NewNode("eve", scidAliasArgs)

	alice, bob := ht.Alice, ht.Bob
	ht.RestartNodeWithExtraArgs(alice, scidAliasArgs)

	// Before we start the test, we'll ensure Alice is connected to Carol
	// and Dave, so she can open channels to both of them (and Bob).
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(alice, carol)
	ht.EnsureConnected(alice, dave)
	ht.EnsureConnected(alice, eve)

	// Let's create our batch TX request. This first one should fail as we
	// open a channel to Carol that is too small for her min chan size.
	batchReq := &lnrpc.BatchOpenChannelRequest{
		SatPerVbyte: 12,
		MinConfs:    1,
		Channels: []*lnrpc.BatchOpenChannel{{
			NodePubkey:         bob.PubKey[:],
			LocalFundingAmount: 100_000,
			BaseFee:            1337,
			UseBaseFee:         true,
		}, {
			NodePubkey:         carol.PubKey[:],
			LocalFundingAmount: 100_000,
			FeeRate:            1337,
			UseFeeRate:         true,
		}, {
			NodePubkey:         dave.PubKey[:],
			LocalFundingAmount: 100_000,
			BaseFee:            1337,
			UseBaseFee:         true,
			FeeRate:            1337,
			UseFeeRate:         true,
		}, {
			NodePubkey:         eve.PubKey[:],
			LocalFundingAmount: 100_000,
			Private:            true,
			ZeroConf:           true,
			CommitmentType:     lnrpc.CommitmentType_ANCHORS,
		}},
	}

	// Check that batch opening fails due to the minchansize requirement.
	err := alice.RPC.BatchOpenChannelAssertErr(batchReq)
	require.Contains(ht, err.Error(), "initial negotiation failed")

	// Let's fix the minimum amount for Alice now and try again.
	batchReq.Channels[1].LocalFundingAmount = 200_000

	// Set up a ChannelAcceptor for Eve to accept a zero-conf opening from
	// Alice.
	acceptStream, cancel := eve.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, true, acceptStream)

	// Batch-open all channels.
	batchResp := alice.RPC.BatchOpenChannel(batchReq)
	require.Len(ht, batchResp.PendingChannels, 4)

	txHash, err := chainhash.NewHash(batchResp.PendingChannels[0].Txid)
	require.NoError(ht, err)

	// Remove the ChannelAcceptor.
	cancel()

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
	chanPoint4 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: batchResp.PendingChannels[3].Txid,
		},
		OutputIndex: batchResp.PendingChannels[3].OutputIndex,
	}

	// Ensure that Alice can send funds to Eve via the zero-conf channel
	// before the batch transaction was mined.
	ht.AssertTopologyChannelOpen(alice, chanPoint4)
	eveInvoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	eveInvoiceResp := eve.RPC.AddInvoice(eveInvoiceParams)
	ht.CompletePaymentRequests(
		alice, []string{eveInvoiceResp.PaymentRequest},
	)

	// Mine the batch transaction and check the network topology.
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
	ht.Miner.AssertTxInBlock(block, txHash)
	ht.AssertTopologyChannelOpen(alice, chanPoint1)
	ht.AssertTopologyChannelOpen(alice, chanPoint2)
	ht.AssertTopologyChannelOpen(alice, chanPoint3)

	// Check if the change type from the batch_open_channel funding is P2TR.
	rawTx := ht.Miner.GetRawTransaction(txHash)
	require.Len(ht, rawTx.MsgTx().TxOut, 5)

	// For calculating the change output index we use the formula for the
	// sum of consecutive of integers (n(n+1)/2). All the channel point
	// indexes are known, so we just calculate the difference to get the
	// change output index.
	// Example: Batch outputs = 4, sum_consecutive_ints(4) = 10
	// Subtract all other output indices to get the change index:
	// 10 - 0 - 1 - 2 - 3 = 4
	changeIndex := uint32(10) - (chanPoint1.OutputIndex +
		chanPoint2.OutputIndex + chanPoint3.OutputIndex +
		chanPoint4.OutputIndex)
	ht.AssertOutputScriptClass(
		rawTx, changeIndex, txscript.WitnessV1TaprootTy,
	)

	// With the channel open, ensure that it is counted towards Alice's
	// total channel balance.
	balRes := alice.RPC.ChannelBalance()
	require.NotEqual(ht, int64(0), balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := carol.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// Confirm that Alice's channel partners see here initial fee settings.
	ensurePolicy(
		ht, alice, bob, chanPoint1,
		lnwire.MilliSatoshi(batchReq.Channels[0].BaseFee),
		chainreg.DefaultBitcoinFeeRate,
	)
	ensurePolicy(
		ht, alice, carol, chanPoint2,
		chainreg.DefaultBitcoinBaseFeeMSat,
		lnwire.MilliSatoshi(batchReq.Channels[1].FeeRate),
	)
	ensurePolicy(
		ht, alice, dave, chanPoint3,
		lnwire.MilliSatoshi(batchReq.Channels[2].BaseFee),
		lnwire.MilliSatoshi(batchReq.Channels[2].FeeRate),
	)
	ensurePolicy(
		ht, alice, eve, chanPoint4,
		chainreg.DefaultBitcoinBaseFeeMSat,
		chainreg.DefaultBitcoinFeeRate,
	)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(alice, chanPoint1)
	ht.CloseChannel(alice, chanPoint2)
	ht.CloseChannel(alice, chanPoint3)
	ht.CloseChannel(alice, chanPoint4)
}

// ensurePolicy ensures that the peer sees alice's channel fee settings.
func ensurePolicy(ht *lntest.HarnessTest, alice, peer *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, expectedBaseFee lnwire.MilliSatoshi,
	expectedFeeRate lnwire.MilliSatoshi) {

	channel := ht.AssertChannelExists(peer, chanPoint)
	policy, err := peer.RPC.LN.GetChanInfo(
		context.Background(), &lnrpc.ChanInfoRequest{
			ChanId: channel.ChanId,
		},
	)
	require.NoError(ht, err)
	alicePolicy := policy.Node1Policy
	if alice.PubKeyStr == policy.Node2Pub {
		alicePolicy = policy.Node2Policy
	}
	require.EqualValues(ht, expectedBaseFee, alicePolicy.FeeBaseMsat)
	require.EqualValues(ht, expectedFeeRate, alicePolicy.FeeRateMilliMsat)
}

// deriveFundingShim creates a channel funding shim by deriving the necessary
// keys on both sides.
func deriveFundingShim(ht *lntest.HarnessTest, carol, dave *node.HarnessNode,
	chanSize btcutil.Amount, thawHeight uint32, publish bool,
	commitType lnrpc.CommitmentType) (*lnrpc.FundingShim,
	*lnrpc.ChannelPoint) {

	keyLoc := &signrpc.KeyLocator{KeyFamily: 9999}
	carolFundingKey := carol.RPC.DeriveKey(keyLoc)
	daveFundingKey := dave.RPC.DeriveKey(keyLoc)

	// Now that we have the multi-sig keys for each party, we can manually
	// construct the funding transaction. We'll instruct the backend to
	// immediately create and broadcast a transaction paying out an exact
	// amount. Normally this would reside in the mempool, but we just
	// confirm it now for simplicity.
	var (
		fundingOutput *wire.TxOut
		musig2        bool
		err           error
	)
	if commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		var carolKey, daveKey *btcec.PublicKey
		carolKey, err = btcec.ParsePubKey(carolFundingKey.RawKeyBytes)
		require.NoError(ht, err)
		daveKey, err = btcec.ParsePubKey(daveFundingKey.RawKeyBytes)
		require.NoError(ht, err)

		_, fundingOutput, err = input.GenTaprootFundingScript(
			carolKey, daveKey, int64(chanSize),
		)
		require.NoError(ht, err)

		musig2 = true
	} else {
		_, fundingOutput, err = input.GenFundingPkScript(
			carolFundingKey.RawKeyBytes, daveFundingKey.RawKeyBytes,
			int64(chanSize),
		)
		require.NoError(ht, err)
	}

	var txid *chainhash.Hash
	targetOutputs := []*wire.TxOut{fundingOutput}
	if publish {
		txid = ht.Miner.SendOutputsWithoutChange(targetOutputs, 5)
	} else {
		tx := ht.Miner.CreateTransaction(targetOutputs, 5)

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
		Musig2:        musig2,
	}
	fundingShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}
	dave.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})

	// If we attempt to register the same shim (has the same pending chan
	// ID), then we should get an error.
	dave.RPC.FundingStateStepAssertErr(&lnrpc.FundingTransitionMsg{
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

	return fundingShim, chanPoint
}
