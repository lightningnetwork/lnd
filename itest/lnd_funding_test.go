package itest

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// basicFundingTestCases defines the test cases for the basic funding test.
var basicFundingTestCases = []*lntest.TestCase{
	{
		Name:     "basic flow static key remote",
		TestFunc: testBasicChannelFundingStaticRemote,
	},
	{
		Name:     "basic flow anchor",
		TestFunc: testBasicChannelFundingAnchor,
	},
	{
		Name:     "basic flow simple taproot",
		TestFunc: testBasicChannelFundingSimpleTaproot,
	},
}

// allFundingTypes defines the channel types to test for the basic funding
// test.
var allFundingTypes = []lnrpc.CommitmentType{
	lnrpc.CommitmentType_STATIC_REMOTE_KEY,
	lnrpc.CommitmentType_ANCHORS,
	lnrpc.CommitmentType_SIMPLE_TAPROOT,
}

// testBasicChannelFundingStaticRemote performs a test exercising expected
// behavior from a basic funding workflow. The test creates a new channel
// between Carol and Dave, with Carol using the static remote key commitment
// type, and Dave using allFundingTypes.
func testBasicChannelFundingStaticRemote(ht *lntest.HarnessTest) {
	carolCommitType := lnrpc.CommitmentType_STATIC_REMOTE_KEY

	// We'll test all possible combinations of the feature bit presence
	// that both nodes can signal for this new channel type. We'll make a
	// new Carol+Dave for each test instance as well.
	for _, daveCommitType := range allFundingTypes {
		cc := carolCommitType
		dc := daveCommitType

		testName := fmt.Sprintf(
			"carol_commit=%v,dave_commit=%v", cc, dc,
		)

		success := ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			runBasicFundingTest(st, cc, dc)
		})

		if !success {
			break
		}
	}
}

// testBasicChannelFundingAnchor performs a test exercising expected behavior
// from a basic funding workflow. The test creates a new channel between Carol
// and Dave, with Carol using the anchor commitment type, and Dave using
// allFundingTypes.
func testBasicChannelFundingAnchor(ht *lntest.HarnessTest) {
	carolCommitType := lnrpc.CommitmentType_ANCHORS

	// We'll test all possible combinations of the feature bit presence
	// that both nodes can signal for this new channel type. We'll make a
	// new Carol+Dave for each test instance as well.
	for _, daveCommitType := range allFundingTypes {
		cc := carolCommitType
		dc := daveCommitType

		testName := fmt.Sprintf(
			"carol_commit=%v,dave_commit=%v", cc, dc,
		)

		success := ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			runBasicFundingTest(st, cc, dc)
		})

		if !success {
			break
		}
	}
}

// testBasicChannelFundingSimpleTaproot performs a test exercising expected
// behavior from a basic funding workflow. The test creates a new channel
// between Carol and Dave, with Carol using the simple taproot commitment type,
// and Dave using allFundingTypes.
func testBasicChannelFundingSimpleTaproot(ht *lntest.HarnessTest) {
	carolCommitType := lnrpc.CommitmentType_SIMPLE_TAPROOT

	// We'll test all possible combinations of the feature bit presence
	// that both nodes can signal for this new channel type. We'll make a
	// new Carol+Dave for each test instance as well.
	for _, daveCommitType := range allFundingTypes {
		cc := carolCommitType
		dc := daveCommitType

		testName := fmt.Sprintf(
			"carol_commit=%v,dave_commit=%v", cc, dc,
		)

		success := ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			runBasicFundingTest(st, cc, dc)
		})

		if !success {
			break
		}
	}
}

// runBasicFundingTest is a helper function that takes Carol and Dave's
// commitment types and test the funding flow.
func runBasicFundingTest(ht *lntest.HarnessTest, carolCommitType,
	daveCommitType lnrpc.CommitmentType) {

	// Based on the current tweak variable for Carol, we'll preferentially
	// signal the legacy commitment format.  We do the same for Dave
	// shortly below.
	carolArgs := lntest.NodeArgsForCommitType(carolCommitType)
	carol := ht.NewNode("Carol", carolArgs)

	// Each time, we'll send Carol a new set of coins in order to fund the
	// channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	daveArgs := lntest.NodeArgsForCommitType(daveCommitType)
	dave := ht.NewNode("Dave", daveArgs)

	// Before we start the test, we'll ensure both sides are connected to
	// the funding flow can properly be executed.
	ht.EnsureConnected(carol, dave)

	var privateChan bool

	// If this is to be a taproot channel type, then it needs to be
	// private, otherwise it'll be rejected by Dave.
	//
	// TODO(roasbeef): lift after gossip 1.75
	if carolCommitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		privateChan = true
	}

	// If carol wants taproot, but dave wants something else, then we'll
	// assert that the channel negotiation attempt fails.
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

	carolChan, daveChan := basicChannelFundingTest(
		ht, carol, dave, nil, privateChan, &carolCommitType,
	)

	// Both nodes should report the same commitment type.
	chansCommitType := carolChan.CommitmentType
	require.Equal(ht, chansCommitType, daveChan.CommitmentType,
		"commit types don't match")

	// Now check that the commitment type reported by both nodes is what we
	// expect. It will be the minimum of the two nodes' preference, in the
	// order Legacy, Tweakless, Anchors.
	expType := carolCommitType

	switch daveCommitType {
	// Dave supports taproot, type will be what Carol supports.
	case lnrpc.CommitmentType_SIMPLE_TAPROOT:

	// Dave supports anchors, type will be what Carol supports.
	case lnrpc.CommitmentType_ANCHORS:
		// However if Alice wants taproot chans, then we downgrade to
		// anchors as this is still using implicit negotiation.
		if expType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
			expType = lnrpc.CommitmentType_ANCHORS
		}

	// Dave only supports tweakless, channel will be downgraded to this
	// type if Carol supports anchors.
	case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
		switch expType {
		case lnrpc.CommitmentType_ANCHORS:
			expType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
		case lnrpc.CommitmentType_SIMPLE_TAPROOT:
			expType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
		}

	// Dave only supports legacy type, channel will be downgraded to this
	// type.
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
		chansCommitType == lnrpc.CommitmentType_STATIC_REMOTE_KEY:

	case expType == lnrpc.CommitmentType_LEGACY &&
		chansCommitType == lnrpc.CommitmentType_LEGACY:

	case expType == lnrpc.CommitmentType_SIMPLE_TAPROOT &&
		chansCommitType == lnrpc.CommitmentType_SIMPLE_TAPROOT:

	default:
		ht.Fatalf("expected nodes to signal commit type %v, instead "+
			"got %v", expType, chansCommitType)
	}
}

// basicChannelFundingTest is a sub-test of the main testBasicChannelFunding
// test. Given two nodes: Alice and Bob, it'll assert proper channel creation,
// then return a function closure that should be called to assert proper
// channel closure.
func basicChannelFundingTest(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode, fundingShim *lnrpc.FundingShim,
	privateChan bool, commitType *lnrpc.CommitmentType) (*lnrpc.Channel,
	*lnrpc.Channel) {

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

	return aliceChannel, bobChannel
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
	alice := ht.NewNode("Alice", nil)

	// We'll send her some unconfirmed funds.
	ht.FundCoinsUnconfirmed(2*chanAmt, carol)

	// For neutrino backend, we will confirm the coins sent above and let
	// Carol send all her funds to herself to create unconfirmed output.
	if ht.IsNeutrinoBackend() {
		// Confirm the above coins.
		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Create a new address and send to herself.
		resp := carol.RPC.NewAddress(&lnrpc.NewAddressRequest{
			Type: lnrpc.AddressType_TAPROOT_PUBKEY,
		})

		// Once sent, Carol would have one unconfirmed UTXO.
		carol.RPC.SendCoins(&lnrpc.SendCoinsRequest{
			Addr:    resp.Address,
			SendAll: true,
		})
	}

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
	ht.MineBlocksAndAssertNumTxes(6, 2)

	ht.WaitForChannelOpenEvent(chanOpenUpdate)

	// With the channel open, we'll check the balances on each side of the
	// channel as a sanity check to ensure things worked out as intended.
	checkChannelBalance(carol, carolLocalBalance, pushAmt, 0, 0)
	checkChannelBalance(alice, pushAmt, carolLocalBalance, 0, 0)
}

// testChannelFundingInputTypes tests that any type of supported input type can
// be used to fund channels.
func testChannelFundingInputTypes(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// We'll start off by creating a node for Carol.
	carol := ht.NewNode("Carol", nil)

	// Now, we'll connect her to Alice so that they can open a
	// channel together.
	ht.ConnectNodes(carol, alice)

	runChannelFundingInputTypes(ht, alice, carol)
}

// runChannelFundingInputTypes tests that any type of supported input type can
// be used to fund channels.
func runChannelFundingInputTypes(ht *lntest.HarnessTest, alice,
	carol *node.HarnessNode) {

	const (
		chanAmt  = funding.MaxBtcFundingAmount
		burnAddr = "bcrt1qxsnqpdc842lu8c0xlllgvejt6rhy49u6fmpgyz"
	)

	fundMixed := func(amt btcutil.Amount,
		target *node.HarnessNode) *wire.MsgTx {

		ht.FundCoins(amt/5, target)
		ht.FundCoins(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)
		ht.FundCoinsP2TR(amt/5, target)

		return nil
	}
	fundMultipleP2TR := func(amt btcutil.Amount,
		target *node.HarnessNode) *wire.MsgTx {

		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)
		ht.FundCoinsP2TR(amt/4, target)

		return nil
	}
	fundWithTypes := []func(amt btcutil.Amount,
		target *node.HarnessNode) *wire.MsgTx{
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
		Addr:       addr,
		SendAll:    true,
		TargetConf: 6,
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
	fundingShim1, chanPoint1 := ht.DeriveFundingShim(
		carol, dave, chanSize, thawHeight, false, commitmentType,
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
	fundingShim2, chanPoint2 := ht.DeriveFundingShim(
		carol, dave, chanSize, thawHeight, true, commitmentType,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan := basicChannelFundingTest(
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
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint2,
	}
	err := ht.CloseChannelAssertErr(carol, req)
	require.Contains(ht, err.Error(), "cannot co-op close frozen channel")

	// Before Dave closes the channel, he needs to check the invoice is
	// settled to avoid an error saying cannot close channel due to active
	// HTLCs.
	ht.AssertInvoiceSettled(dave, resp.PaymentAddr)

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
	fundingShim1, chanPoint1 := ht.DeriveFundingShim(
		carol, dave, chanSize, thawHeight, false, commitmentType,
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
	fundingShim2, chanPoint2 := ht.DeriveFundingShim(
		carol, dave, chanSize, thawHeight, true, commitmentType,
	)

	// At this point, we'll now carry out the normal basic channel funding
	// test as everything should now proceed as normal (a regular channel
	// funding flow).
	carolChan, daveChan := basicChannelFundingTest(
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
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint2,
	}
	err := ht.CloseChannelAssertErr(carol, req)
	require.Contains(ht, err.Error(), "cannot co-op close frozen channel")

	// Before Dave closes the channel, he needs to check the invoice is
	// settled to avoid an error saying cannot close channel due to active
	// HTLCs.
	ht.AssertInvoiceSettled(dave, resp.PaymentAddr)

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

	alice := ht.NewNodeWithCoins("Alice", nil)
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
	ht.AssertTxInBlock(block, *fundingTxID)

	// Get the height that our transaction confirmed at.
	height := int32(ht.CurrentHeight())

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
	ht.AssertChannelInGraph(alice, chanPoint)
	ht.AssertChannelInGraph(carol, chanPoint)

	// Create an additional check for our channel assertion that will
	// check that our label is as expected.
	shortChanID := lnwire.NewShortChanIDFromInt(chanAlice.ChanId)
	label = labels.MakeLabel(labels.LabelTypeChannelOpen, &shortChanID)
	require.Equal(ht, label, tx.Label, "open channel label not updated")
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

	alice := ht.NewNodeWithCoins("Alice", scidAliasArgs)
	bob := ht.NewNodeWithCoins("Bob", nil)

	// Before we start the test, we'll ensure Alice is connected to Carol
	// and Dave, so she can open channels to both of them (and Bob).
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(alice, carol)
	ht.EnsureConnected(alice, dave)
	ht.EnsureConnected(alice, eve)

	expectedFeeRate := chainfee.SatPerKWeight(2500)

	// We verify that the channel opening uses the correct fee rate.
	ht.SetFeeEstimateWithConf(expectedFeeRate, 3)

	// Let's create our batch TX request. This first one should fail as we
	// open a channel to Carol that is too small for her min chan size.
	batchReq := &lnrpc.BatchOpenChannelRequest{
		TargetConf: 3,
		MinConfs:   1,
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
	ht.AssertChannelInGraph(alice, chanPoint4)
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
	ht.AssertTxInBlock(block, *txHash)
	ht.AssertChannelInGraph(alice, chanPoint1)
	ht.AssertChannelInGraph(alice, chanPoint2)
	ht.AssertChannelInGraph(alice, chanPoint3)

	// Check if the change type from the batch_open_channel funding is P2TR.
	rawTx := ht.GetRawTransaction(*txHash)
	require.Len(ht, rawTx.MsgTx().TxOut, 5)

	// Check the fee rate of the batch-opening transaction. We expect slight
	// inaccuracies because of the DER signature fee estimation.
	openingFeeRate := ht.CalculateTxFeeRate(rawTx.MsgTx())
	require.InEpsilonf(ht, uint64(expectedFeeRate), uint64(openingFeeRate),
		0.01, "want %v, got %v", expectedFeeRate, openingFeeRate)

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
}

// ensurePolicy ensures that the peer sees alice's channel fee settings.
func ensurePolicy(ht *lntest.HarnessTest, alice, peer *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, expectedBaseFee lnwire.MilliSatoshi,
	expectedFeeRate lnwire.MilliSatoshi) {

	channel := ht.AssertChannelExists(peer, chanPoint)
	policy, err := peer.RPC.LN.GetChanInfo(
		ht.Context(), &lnrpc.ChanInfoRequest{
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

// testChannelFundingWithUnstableUtxos tests channel openings with restricted
// utxo selection. Internal wallet utxos might be restricted due to another
// subsystems still using it therefore it would be unsecure to use them for
// channel openings. This test focuses on unconfirmed utxos which are still
// being used by the sweeper subsystem hence should only be used when confirmed.
func testChannelFundingWithUnstableUtxos(ht *lntest.HarnessTest) {
	// Select funding amt below wumbo size because we later use fundMax to
	// open a channel with the total balance.
	fundingAmt := btcutil.Amount(3_000_000)

	// We use STATIC_REMOTE_KEY channels because anchor sweeps would
	// interfere and create additional utxos.
	// Although its the current default we explicitly signal it.
	cType := lnrpc.CommitmentType_STATIC_REMOTE_KEY

	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol := ht.NewNode("carol", nil)

	// We'll attempt at max 2 pending channels, so Dave will need to accept
	// two pending ones.
	dave := ht.NewNode("dave", []string{
		"--maxpendingchannels=2",
	})
	ht.EnsureConnected(carol, dave)

	// Fund Carol's wallet with a confirmed utxo.
	ht.FundCoins(fundingAmt, carol)

	// Now spend the coins to create an unconfirmed transaction. This is
	// necessary to test also the neutrino behaviour. For neutrino nodes
	// only unconfirmed transactions originating from this node will be
	// recognized as unconfirmed.
	req := &lnrpc.NewAddressRequest{Type: AddrTypeTaprootPubkey}
	resp := carol.RPC.NewAddress(req)

	sendCoinsResp := carol.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:        resp.Address,
		SendAll:     true,
		SatPerVbyte: 1,
	})

	walletUtxo := ht.AssertNumUTXOsUnconfirmed(carol, 1)[0]
	require.EqualValues(ht, sendCoinsResp.Txid, walletUtxo.Outpoint.TxidStr)

	// We will attempt to open 2 channels at a time.
	chanSize := btcutil.Amount(walletUtxo.AmountSat / 3)

	// Open a channel to dave with an unconfirmed utxo. Although this utxo
	// is unconfirmed it can be used to open a channel because it did not
	// originated from the sweeper subsystem.
	ht.OpenChannelAssertPending(carol, dave,
		lntest.OpenChannelParams{
			Amt:              chanSize,
			SpendUnconfirmed: true,
			CommitmentType:   cType,
		})

	// Verify that both nodes know about the channel.
	ht.AssertNumPendingOpenChannels(carol, 1)
	ht.AssertNumPendingOpenChannels(dave, 1)

	// We open another channel on the fly, funds are unconfirmed but because
	// the tx was not created by the sweeper we can use it and open another
	// channel. This is a common use case when opening zeroconf channels,
	// so unconfirmed utxos originated from prior channel opening are safe
	// to use because channel opening should not be RBFed, at least not for
	// now.
	update := ht.OpenChannelAssertPending(carol, dave,
		lntest.OpenChannelParams{
			Amt:              chanSize,
			SpendUnconfirmed: true,
			CommitmentType:   cType,
		})

	chanPoint2 := lntest.ChanPointFromPendingUpdate(update)

	ht.AssertNumPendingOpenChannels(carol, 2)
	ht.AssertNumPendingOpenChannels(dave, 2)

	// We expect the initial funding tx to confirm and also the two
	// unconfirmed channel openings.
	ht.MineBlocksAndAssertNumTxes(1, 3)

	// Now we create an unconfirmed utxo which originated from the sweeper
	// subsystem and hence is not safe to use for channel openings. We do
	// that by dave force-closing the channel. Which let's carol sweep its
	// to_remote output which is not encumbered by any relative locktime.
	ht.CloseChannelAssertPending(dave, chanPoint2, true)

	// Mine the force close commitment transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Make sure Carol sees her to_remote output from the force close tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// We need to wait for carol initiating the sweep of the to_remote
	// output of chanPoint2.
	utxo := ht.AssertNumUTXOsUnconfirmed(carol, 1)[0]

	// We now try to open channel using the unconfirmed utxo.
	fundingUtxo := utxo

	// Now try to open the channel with this utxo and expect an error.
	expectedErr := fmt.Errorf("outpoint already spent or "+
		"locked by another subsystem: %s:%d",
		fundingUtxo.Outpoint.TxidStr,
		fundingUtxo.Outpoint.OutputIndex)

	ht.OpenChannelAssertErr(carol, dave,
		lntest.OpenChannelParams{
			FundMax:          true,
			SpendUnconfirmed: true,
			Outpoints: []*lnrpc.OutPoint{
				fundingUtxo.Outpoint,
			},
		}, expectedErr)

	// The channel opening failed because the utxo was unconfirmed and
	// originated from the sweeper subsystem. Now we confirm the
	// to_remote sweep and expect the channel opening to work.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Try opening the channel with the same utxo (now confirmed) again.
	update = ht.OpenChannelAssertPending(carol, dave,
		lntest.OpenChannelParams{
			FundMax:          true,
			SpendUnconfirmed: true,
			Outpoints: []*lnrpc.OutPoint{
				fundingUtxo.Outpoint,
			},
		})

	chanPoint3 := lntest.ChanPointFromPendingUpdate(update)
	ht.AssertNumPendingOpenChannels(carol, 1)
	ht.AssertNumPendingOpenChannels(dave, 1)

	// We expect chanPoint3 to confirm.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Force Close the channel and test the opening flow without preselected
	// utxos.
	// Before we tested the channel funding with a selected coin, now we
	// want to make sure that our internal coin selection also adheres to
	// the restictions of unstable utxos.
	// We create the unconfirmed sweeper originating utxo just like before
	// by force-closing a channel from dave's side.
	ht.CloseChannelAssertPending(dave, chanPoint3, true)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Make sure Carol sees her to_remote output from the force close tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// Wait for the to_remote sweep tx to show up in carol's wallet.
	ht.AssertNumUTXOsUnconfirmed(carol, 1)

	// Calculate the maximum amount our wallet has for the channel funding
	// so that we will use all utxos.
	carolBalance := carol.RPC.WalletBalance()

	// Now calculate the fee for the channel opening transaction. We don't
	// have to keep a channel reserve because we are using STATIC_REMOTE_KEY
	// channels.
	// NOTE: The TotalBalance includes the unconfirmed balance as well.
	chanSize = btcutil.Amount(carolBalance.TotalBalance) -
		fundingFee(2, false)

	// We are trying to open a channel with the maximum amount and expect it
	// to fail because one of the utxos cannot be used because it is
	// unstable.
	expectedErr = fmt.Errorf("not enough witness outputs to create " +
		"funding transaction")

	ht.OpenChannelAssertErr(carol, dave,
		lntest.OpenChannelParams{
			Amt:              chanSize,
			SpendUnconfirmed: true,
			CommitmentType:   cType,
		}, expectedErr)

	// Confirm the to_remote sweep utxo.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	ht.AssertNumUTXOsConfirmed(carol, 2)

	// Now after the sweep utxo is confirmed it is stable and can be used
	// for channel openings again.
	ht.OpenChannelAssertPending(carol, dave,
		lntest.OpenChannelParams{
			Amt:              chanSize,
			SpendUnconfirmed: true,
			CommitmentType:   cType,
		})

	// Verify that both nodes know about the channel.
	ht.AssertNumPendingOpenChannels(carol, 1)
	ht.AssertNumPendingOpenChannels(dave, 1)

	ht.MineBlocksAndAssertNumTxes(1, 1)
}
