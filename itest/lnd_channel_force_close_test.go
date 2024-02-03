package itest

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// testCommitmentTransactionDeadline tests that the anchor sweep transaction is
// taking account of the deadline of the commitment transaction. It tests three
// scenarios:
//  1. when the CPFP is skipped, checks that the deadline is not used.
//  2. when the CPFP is used, checks that the deadline is NOT applied when it's
//     larger than 144.
//  3. when the CPFP is used, checks that the deadline is applied when it's
//     less than 144.
//
// Note that whether the deadline is used or not is implicitly checked by its
// corresponding fee rates.
func testCommitmentTransactionDeadline(ht *lntest.HarnessTest) {
	// Get the default max fee rate used in sweeping the commitment
	// transaction.
	defaultMax := lnwallet.DefaultAnchorsCommitMaxFeeRateSatPerVByte
	maxPerKw := chainfee.SatPerKVByte(defaultMax * 1000).FeePerKWeight()

	const (
		// feeRateConfDefault(sat/kw) is used when no conf target is
		// set. This value will be returned by the fee estimator but
		// won't be used because our commitment fee rate is capped by
		// DefaultAnchorsCommitMaxFeeRateSatPerVByte.
		feeRateDefault = 20000

		// defaultDeadline is the anchorSweepConfTarget, which is used
		// when the commitment has no deadline pressure.
		defaultDeadline = 144

		// deadline is one block below the default deadline. A forced
		// anchor sweep will be performed when seeing this value.
		deadline = defaultDeadline - 1
	)

	// feeRateSmall(sat/kw) is used when we want to skip the CPFP
	// on anchor transactions. When the fee rate is smaller than
	// the parent's (commitment transaction) fee rate, the CPFP
	// will be skipped. Atm, the parent tx's fee rate is roughly
	// 2500 sat/kw in this test.
	feeRateSmall := maxPerKw / 2

	// feeRateLarge(sat/kw) is used when we want to use the anchor
	// transaction to CPFP our commitment transaction.
	feeRateLarge := maxPerKw * 2

	// Before we start, set up the default fee rate and we will test the
	// actual fee rate against it to decide whether we are using the
	// deadline to perform fee estimation.
	ht.SetFeeEstimate(feeRateDefault)

	// setupNode creates a new node and sends 1 btc to the node.
	setupNode := func(name string) *node.HarnessNode {
		// Create the node.
		args := []string{"--hodl.exit-settle"}
		args = append(args, lntest.NodeArgsForCommitType(
			lnrpc.CommitmentType_ANCHORS)...,
		)
		node := ht.NewNode(name, args)

		// Send some coins to the node.
		ht.FundCoins(btcutil.SatoshiPerBitcoin, node)

		// For neutrino backend, we need one additional UTXO to create
		// the sweeping tx for the remote anchor.
		if ht.IsNeutrinoBackend() {
			ht.FundCoins(btcutil.SatoshiPerBitcoin, node)
		}

		return node
	}

	// calculateSweepFeeRate runs multiple steps to calculate the fee rate
	// used in sweeping the transactions.
	calculateSweepFeeRate := func(expectedSweepTxNum, deadline int) int64 {
		// Create two nodes, Alice and Bob.
		alice := setupNode("Alice")
		defer ht.Shutdown(alice)

		bob := setupNode("Bob")
		defer ht.Shutdown(bob)

		// Connect Alice to Bob.
		ht.ConnectNodes(alice, bob)

		// Open a channel between Alice and Bob.
		chanPoint := ht.OpenChannel(
			alice, bob, lntest.OpenChannelParams{
				Amt:     10e6,
				PushAmt: 5e6,
			},
		)

		// Calculate the final ctlv delta based on the expected
		// deadline.
		finalCltvDelta := int32(deadline - int(routing.BlockPadding))

		// Send a payment with a specified finalCTLVDelta, which will
		// be used as our deadline later on when Alice force closes the
		// channel.
		req := &routerrpc.SendPaymentRequest{
			Dest:           bob.PubKey[:],
			Amt:            10e4,
			PaymentHash:    ht.Random32Bytes(),
			FinalCltvDelta: finalCltvDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		alice.RPC.SendPayment(req)

		// Once the HTLC has cleared, all the nodes in our mini network
		// should show that the HTLC has been locked in.
		ht.AssertNumActiveHtlcs(alice, 1)
		ht.AssertNumActiveHtlcs(bob, 1)

		// Alice force closes the channel.
		ht.CloseChannelAssertPending(alice, chanPoint, true)

		// Now that the channel has been force closed, it should show
		// up in the PendingChannels RPC under the waiting close
		// section.
		ht.AssertChannelWaitingClose(alice, chanPoint)

		// Check our sweep transactions can be found in mempool.
		sweepTxns := ht.Miner.GetNumTxsFromMempool(expectedSweepTxNum)

		// Mine a block to confirm these transactions such that they
		// don't remain in the mempool for any subsequent tests.
		ht.MineBlocks(1)

		// Calculate the fee rate used.
		feeRate := ht.CalculateTxesFeeRate(sweepTxns)

		return feeRate
	}

	// Setup our fee estimation for the deadline. Because the fee rate is
	// smaller than the parent tx's fee rate, this value won't be used and
	// we should see only one sweep tx in the mempool.
	ht.SetFeeEstimateWithConf(feeRateSmall, deadline)

	// Calculate fee rate used and assert only the force close tx is
	// broadcast.
	feeRate := calculateSweepFeeRate(1, deadline)

	// We expect the default max fee rate is used. Allow some deviation
	// because weight estimates during tx generation are estimates.
	require.InEpsilonf(
		ht, int64(maxPerKw), feeRate, 0.01,
		"expected fee rate:%d, got fee rate:%d", maxPerKw, feeRate,
	)

	// Setup our fee estimation for the deadline. Because the fee rate is
	// greater than the parent tx's fee rate, this value will be used to
	// sweep the anchor transaction. However, due to the default value
	// being used, we should not attempt CPFP here because we are not force
	// sweeping the anchor output.
	ht.SetFeeEstimateWithConf(feeRateLarge, defaultDeadline)

	// Calculate fee rate used and assert only the force close tx is
	// broadcast.
	feeRate = calculateSweepFeeRate(1, defaultDeadline)

	// We expect the default max fee rate is used. Allow some deviation
	// because weight estimates during tx generation are estimates.
	require.InEpsilonf(
		ht, int64(maxPerKw), feeRate, 0.01,
		"expected fee rate:%d, got fee rate:%d", maxPerKw, feeRate,
	)

	// Setup our fee estimation for the deadline. Because the fee rate is
	// greater than the parent tx's fee rate, this value will be used to
	// sweep the anchor transaction and we should see two sweep
	// transactions in the mempool.
	ht.SetFeeEstimateWithConf(feeRateLarge, deadline)

	// Calculate fee rate used and assert both the force close tx and the
	// anchor sweeping tx are broadcast.
	feeRate = calculateSweepFeeRate(2, deadline)

	// We expect the anchor to be swept with the deadline, which has the
	// fee rate of feeRateLarge.
	require.InEpsilonf(
		ht, int64(feeRateLarge), feeRate, 0.01,
		"expected fee rate:%d, got fee rate:%d", feeRateLarge, feeRate,
	)
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Carol, then
// force closes the channel after some cursory assertions. Within the test, a
// total of 3 + n transactions will be broadcast, representing the commitment
// transaction, a transaction sweeping the local CSV delayed output, a
// transaction sweeping the CSV delayed 2nd-layer htlcs outputs, and n
// htlc timeout transactions, where n is the number of payments Alice attempted
// to send to Carol.  This test includes several restarts to ensure that the
// transaction output states are persisted throughout the forced closure
// process.
//
// TODO(roasbeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(ht *lntest.HarnessTest) {
	// We'll test the scenario for some of the commitment types, to ensure
	// outputs can be swept.
	commitTypes := []lnrpc.CommitmentType{
		lnrpc.CommitmentType_LEGACY,
		lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	}

	for _, channelType := range commitTypes {
		testName := fmt.Sprintf("committype=%v", channelType)

		channelType := channelType
		success := ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)

			args := lntest.NodeArgsForCommitType(channelType)
			alice := st.NewNode("Alice", args)
			defer st.Shutdown(alice)

			// Since we'd like to test failure scenarios with
			// outstanding htlcs, we'll introduce another node into
			// our test network: Carol.
			carolArgs := []string{"--hodl.exit-settle"}
			carolArgs = append(carolArgs, args...)
			carol := st.NewNode("Carol", carolArgs)
			defer st.Shutdown(carol)

			// Each time, we'll send Alice  new set of coins in
			// order to fund the channel.
			st.FundCoins(btcutil.SatoshiPerBitcoin, alice)

			// Also give Carol some coins to allow her to sweep her
			// anchor.
			st.FundCoins(btcutil.SatoshiPerBitcoin, carol)

			channelForceClosureTest(st, alice, carol, channelType)
		})
		if !success {
			return
		}
	}
}

func channelForceClosureTest(ht *lntest.HarnessTest,
	alice, carol *node.HarnessNode, channelType lnrpc.CommitmentType) {

	const (
		chanAmt     = btcutil.Amount(10e6)
		pushAmt     = btcutil.Amount(5e6)
		paymentAmt  = 100000
		numInvoices = 6
	)

	const commitFeeRate = 20000
	ht.SetFeeEstimate(commitFeeRate)

	// TODO(roasbeef): should check default value in config here
	// instead, or make delay a param
	defaultCLTV := uint32(chainreg.DefaultBitcoinTimeLockDelta)

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	ht.ConnectNodes(alice, carol)

	// We need one additional UTXO for sweeping the remote anchor.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Before we start, obtain Carol's current wallet balance, we'll check
	// to ensure that at the end of the force closure by Alice, Carol
	// recognizes his new on-chain output.
	carolBalResp := carol.RPC.WalletBalance()
	carolStartingBalance := carolBalResp.ConfirmedBalance

	// If the channel is a taproot channel, then we'll need to create a
	// private channel.
	//
	// TODO(roasbeef): lift after G175
	var privateChan bool
	if channelType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		privateChan = true
	}

	chanPoint := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Private:        privateChan,
			Amt:            chanAmt,
			PushAmt:        pushAmt,
			CommitmentType: channelType,
		},
	)

	// Send payments from Alice to Carol, since Carol is htlchodl mode, the
	// htlc outputs should be left unsettled, and should be swept by the
	// utxo nursery.
	carolPubKey := carol.PubKey[:]
	for i := 0; i < numInvoices; i++ {
		req := &routerrpc.SendPaymentRequest{
			Dest:           carolPubKey,
			Amt:            int64(paymentAmt),
			PaymentHash:    ht.Random32Bytes(),
			FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		alice.RPC.SendPayment(req)
	}

	// Once the HTLC has cleared, all the nodes n our mini network should
	// show that the HTLC has been locked in.
	ht.AssertNumActiveHtlcs(alice, numInvoices)
	ht.AssertNumActiveHtlcs(carol, numInvoices)

	// Fetch starting height of this test so we can compute the block
	// heights we expect certain events to take place.
	_, curHeight := ht.Miner.GetBestBlock()

	// Using the current height of the chain, derive the relevant heights
	// for incubating two-stage htlcs.
	var (
		startHeight           = uint32(curHeight)
		commCsvMaturityHeight = startHeight + 1 + defaultCSV
		htlcExpiryHeight      = padCLTV(startHeight + defaultCLTV)
		htlcCsvMaturityHeight = padCLTV(
			startHeight + defaultCLTV + 1 + defaultCSV,
		)
	)

	// If we are dealing with an anchor channel type, the sweeper will
	// sweep the HTLC second level output one block earlier (than the
	// nursery that waits an additional block, and handles non-anchor
	// channels). So we set a maturity height that is one less.
	if lntest.CommitTypeHasAnchors(channelType) {
		htlcCsvMaturityHeight = padCLTV(
			startHeight + defaultCLTV + defaultCSV,
		)
	}

	aliceChan := ht.QueryChannelByChanPoint(alice, chanPoint)
	require.NotZero(ht, aliceChan.NumUpdates,
		"alice should see at least one update to her channel")

	// Now that the channel is open and we have unsettled htlcs,
	// immediately execute a force closure of the channel. This will also
	// assert that the commitment transaction was immediately broadcast in
	// order to fulfill the force closure request.
	const actualFeeRate = 30000
	ht.SetFeeEstimate(actualFeeRate)

	ht.CloseChannelAssertPending(alice, chanPoint, true)

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	waitingClose := ht.AssertChannelWaitingClose(alice, chanPoint)

	// Immediately after force closing, all of the funds should be in
	// limbo.
	require.NotZero(ht, waitingClose.LimboBalance,
		"all funds should still be in limbo")

	// Create a map of outpoints to expected resolutions for alice and
	// carol which we will add reports to as we sweep outputs.
	var (
		aliceReports = make(map[string]*lnrpc.Resolution)
		carolReports = make(map[string]*lnrpc.Resolution)
	)

	// The several restarts in this test are intended to ensure that when a
	// channel is force-closed, the UTXO nursery has persisted the state of
	// the channel in the closure process and will recover the correct
	// state when the system comes back on line. This restart tests state
	// persistence at the beginning of the process, when the commitment
	// transaction has been broadcast but not yet confirmed in a block.
	ht.RestartNode(alice)

	// To give the neutrino backend some time to catch up with the chain, we
	// wait here until we have enough UTXOs to actually sweep the local and
	// remote anchor.
	const expectedUtxos = 2
	ht.AssertNumUTXOs(alice, expectedUtxos)

	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure. If there are anchors, we
	// also expect the anchor sweep tx to be in the mempool.
	expectedTxes := 1
	expectedFeeRate := commitFeeRate
	if lntest.CommitTypeHasAnchors(channelType) {
		expectedTxes = 2
		expectedFeeRate = actualFeeRate
	}

	sweepTxns := ht.Miner.GetNumTxsFromMempool(expectedTxes)

	// Verify fee rate of the commitment tx plus anchor if present.
	feeRate := ht.CalculateTxesFeeRate(sweepTxns)

	// Allow some deviation because weight estimates during tx generation
	// are estimates.
	require.InEpsilon(ht, expectedFeeRate, feeRate, 0.005)

	// Find alice's commit sweep and anchor sweep (if present) in the
	// mempool.
	aliceCloseTx := waitingClose.Commitments.LocalTxid
	_, aliceAnchor := ht.FindCommitAndAnchor(sweepTxns, aliceCloseTx)

	// If we expect anchors, add alice's anchor to our expected set of
	// reports.
	if lntest.CommitTypeHasAnchors(channelType) {
		aliceReports[aliceAnchor.OutPoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_ANCHOR,
			Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
			SweepTxid:      aliceAnchor.SweepTx.TxHash().String(),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   aliceAnchor.OutPoint.Hash[:],
				TxidStr:     aliceAnchor.OutPoint.Hash.String(),
				OutputIndex: aliceAnchor.OutPoint.Index,
			},
			AmountSat: uint64(anchorSize),
		}
	}

	ht.MineBlocks(1)

	// Now that the commitment has been confirmed, the channel should be
	// marked as force closed.
	err := wait.NoError(func() error {
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)

		// Now that the channel has been force closed, it should now
		// have the height and number of blocks to confirm populated.
		err := checkCommitmentMaturity(
			forceClose, commCsvMaturityHeight, int32(defaultCSV),
		)
		if err != nil {
			return err
		}

		// None of our outputs have been swept, so they should all be in
		// limbo. For anchors, we expect the anchor amount to be
		// recovered.
		if forceClose.LimboBalance == 0 {
			return errors.New("all funds should still be in " +
				"limbo")
		}
		expectedRecoveredBalance := int64(0)
		if lntest.CommitTypeHasAnchors(channelType) {
			expectedRecoveredBalance = anchorSize
		}
		if forceClose.RecoveredBalance != expectedRecoveredBalance {
			return errors.New("no funds should yet be shown " +
				"as recovered")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking force closed channel")

	// The following restart is intended to ensure that outputs from the
	// force close commitment transaction have been persisted once the
	// transaction has been confirmed, but before the outputs are spendable
	// (the "kindergarten" bucket.)
	ht.RestartNode(alice)

	// Carol's sweep tx should be in the mempool already, as her output is
	// not timelocked. If there are anchors, we also expect Carol's anchor
	// sweep now.
	sweepTxns = ht.Miner.GetNumTxsFromMempool(expectedTxes)

	// Calculate the total fee Carol paid.
	var totalFeeCarol btcutil.Amount
	for _, tx := range sweepTxns {
		fee := ht.CalculateTxFee(tx)
		totalFeeCarol += fee
	}

	// We look up the sweep txns we have found in mempool and create
	// expected resolutions for carol.
	carolCommit, carolAnchor := ht.FindCommitAndAnchor(
		sweepTxns, aliceCloseTx,
	)

	// If we have anchors, add an anchor resolution for carol.
	if lntest.CommitTypeHasAnchors(channelType) {
		carolReports[carolAnchor.OutPoint.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_ANCHOR,
			Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
			SweepTxid:      carolAnchor.SweepTx.TxHash().String(),
			AmountSat:      anchorSize,
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   carolAnchor.OutPoint.Hash[:],
				TxidStr:     carolAnchor.OutPoint.Hash.String(),
				OutputIndex: carolAnchor.OutPoint.Index,
			},
		}
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// For the persistence test, we generate two blocks, then trigger
	// a restart and then generate the final block that should trigger
	// the creation of the sweep transaction.
	ht.MineBlocks(defaultCSV - 2)

	// The following restart checks to ensure that outputs in the
	// kindergarten bucket are persisted while waiting for the required
	// number of confirmations to be reported.
	ht.RestartNode(alice)

	// Alice should see the channel in her set of pending force closed
	// channels with her funds still in limbo.
	var aliceBalance int64
	var closingTxID *chainhash.Hash
	err = wait.NoError(func() error {
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)

		// Get the closing txid.
		txid, err := chainhash.NewHashFromStr(forceClose.ClosingTxid)
		if err != nil {
			return err
		}
		closingTxID = txid

		// Make a record of the balances we expect for alice and carol.
		aliceBalance = forceClose.Channel.LocalBalance

		// At this point, the nursery should show that the commitment
		// output has 2 block left before its CSV delay expires. In
		// total, we have mined exactly defaultCSV blocks, so the htlc
		// outputs should also reflect that this many blocks have
		// passed.
		err = checkCommitmentMaturity(
			forceClose, commCsvMaturityHeight, 2,
		)
		if err != nil {
			return err
		}

		// All funds should still be shown in limbo.
		if forceClose.LimboBalance == 0 {
			return errors.New("all funds should still be in " +
				"limbo")
		}
		expectedRecoveredBalance := int64(0)
		if lntest.CommitTypeHasAnchors(channelType) {
			expectedRecoveredBalance = anchorSize
		}
		if forceClose.RecoveredBalance != expectedRecoveredBalance {
			return errors.New("no funds should yet be shown " +
				"as recovered")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking force closed channel")

	// Generate an additional block, which should cause the CSV delayed
	// output from the commitment txn to expire.
	ht.MineBlocks(1)

	// At this point, the CSV will expire in the next block, meaning that
	// the sweeping transaction should now be broadcast. So we fetch the
	// node's mempool to ensure it has been properly broadcast.
	sweepingTXID := ht.Miner.AssertNumTxsInMempool(1)[0]

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx := ht.Miner.GetRawTransaction(sweepingTXID)
	for _, txIn := range sweepTx.MsgTx().TxIn {
		require.Equal(ht, &txIn.PreviousOutPoint.Hash, closingTxID,
			"sweep transaction not spending from commit")
	}

	// We expect a resolution which spends our commit output.
	output := sweepTx.MsgTx().TxIn[0].PreviousOutPoint
	aliceReports[output.String()] = &lnrpc.Resolution{
		ResolutionType: lnrpc.ResolutionType_COMMIT,
		Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
		SweepTxid:      sweepingTXID.String(),
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   output.Hash[:],
			TxidStr:     output.Hash.String(),
			OutputIndex: output.Index,
		},
		AmountSat: uint64(aliceBalance),
	}

	carolReports[carolCommit.OutPoint.String()] = &lnrpc.Resolution{
		ResolutionType: lnrpc.ResolutionType_COMMIT,
		Outcome:        lnrpc.ResolutionOutcome_CLAIMED,
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   carolCommit.OutPoint.Hash[:],
			TxidStr:     carolCommit.OutPoint.Hash.String(),
			OutputIndex: carolCommit.OutPoint.Index,
		},
		AmountSat: uint64(pushAmt),
		SweepTxid: carolCommit.SweepTx.TxHash().String(),
	}

	// Check that we can find the commitment sweep in our set of known
	// sweeps, using the simple transaction id ListSweeps output.
	ht.AssertSweepFound(alice, sweepingTXID.String(), false, 0)

	// Restart Alice to ensure that she resumes watching the finalized
	// commitment sweep txid.
	ht.RestartNode(alice)

	// Next, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	block := ht.MineBlocks(1)[0]
	ht.Miner.AssertTxInBlock(block, sweepTx.Hash())

	// Update current height
	_, curHeight = ht.Miner.GetBestBlock()

	// checkForceClosedChannelNumHtlcs verifies that a force closed channel
	// has the proper number of htlcs.
	checkPendingChannelNumHtlcs := func(
		forceClose lntest.PendingForceClose) error {

		if len(forceClose.PendingHtlcs) != numInvoices {
			return fmt.Errorf("expected force closed channel to "+
				"have %d pending htlcs, found %d instead",
				numInvoices, len(forceClose.PendingHtlcs))
		}

		return nil
	}

	err = wait.NoError(func() error {
		// Now that the commit output has been fully swept, check to
		// see that the channel remains open for the pending htlc
		// outputs.
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)

		// The commitment funds will have been recovered after the
		// commit txn was included in the last block. The htlc funds
		// will be shown in limbo.
		err := checkPendingChannelNumHtlcs(forceClose)
		if err != nil {
			return err
		}

		err = checkPendingHtlcStageAndMaturity(
			forceClose, 1, htlcExpiryHeight,
			int32(htlcExpiryHeight)-curHeight,
		)
		if err != nil {
			return err
		}

		if forceClose.LimboBalance == 0 {
			return fmt.Errorf("expected funds in limbo, found 0")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking pending "+
		"force close channel")

	// Compute the height preceding that which will cause the htlc CLTV
	// timeouts will expire. The outputs entered at the same height as the
	// output spending from the commitment txn, so we must deduct the
	// number of blocks we have generated since adding it to the nursery,
	// and take an additional block off so that we end up one block shy of
	// the expiry height, and add the block padding.
	cltvHeightDelta := padCLTV(defaultCLTV - defaultCSV - 1 - 1)

	// Advance the blockchain until just before the CLTV expires, nothing
	// exciting should have happened during this time.
	ht.MineBlocks(cltvHeightDelta)

	// We now restart Alice, to ensure that she will broadcast the presigned
	// htlc timeout txns after the delay expires after experiencing a while
	// waiting for the htlc outputs to incubate.
	ht.RestartNode(alice)

	// Alice should now see the channel in her set of pending force closed
	// channels with one pending HTLC.
	err = wait.NoError(func() error {
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)

		// We should now be at the block just before the utxo nursery
		// will attempt to broadcast the htlc timeout transactions.
		err = checkPendingChannelNumHtlcs(forceClose)
		if err != nil {
			return err
		}
		err = checkPendingHtlcStageAndMaturity(
			forceClose, 1, htlcExpiryHeight, 1,
		)
		if err != nil {
			return err
		}

		// Now that our commitment confirmation depth has been
		// surpassed, we should now see a non-zero recovered balance.
		// All htlc outputs are still left in limbo, so it should be
		// non-zero as well.
		if forceClose.LimboBalance == 0 {
			return errors.New("htlc funds should still be in " +
				"limbo")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking force closed channel")

	// Now, generate the block which will cause Alice to broadcast the
	// presigned htlc timeout txns.
	ht.MineBlocks(1)

	// Since Alice had numInvoices (6) htlcs extended to Carol before force
	// closing, we expect Alice to broadcast an htlc timeout txn for each
	// one.
	expectedTxes = numInvoices

	// In case of anchors, the timeout txs will be aggregated into one.
	if lntest.CommitTypeHasAnchors(channelType) {
		expectedTxes = 1
	}

	// Wait for them all to show up in the mempool.
	htlcTxIDs := ht.Miner.AssertNumTxsInMempool(expectedTxes)

	// Retrieve each htlc timeout txn from the mempool, and ensure it is
	// well-formed. This entails verifying that each only spends from
	// output, and that output is from the commitment txn. In case this is
	// an anchor channel, the transactions are aggregated by the sweeper
	// into one.
	numInputs := 1
	if lntest.CommitTypeHasAnchors(channelType) {
		numInputs = numInvoices + 1
	}

	// Construct a map of the already confirmed htlc timeout outpoints,
	// that will count the number of times each is spent by the sweep txn.
	// We prepopulate it in this way so that we can later detect if we are
	// spending from an output that was not a confirmed htlc timeout txn.
	var htlcTxOutpointSet = make(map[wire.OutPoint]int)

	var htlcLessFees uint64
	for _, htlcTxID := range htlcTxIDs {
		// Fetch the sweep transaction, all input it's spending should
		// be from the commitment transaction which was broadcast
		// on-chain. In case of an anchor type channel, we expect one
		// extra input that is not spending from the commitment, that
		// is added for fees.
		htlcTx := ht.Miner.GetRawTransaction(htlcTxID)

		// Ensure the htlc transaction has the expected number of
		// inputs.
		inputs := htlcTx.MsgTx().TxIn
		require.Len(ht, inputs, numInputs, "num inputs mismatch")

		// The number of outputs should be the same.
		outputs := htlcTx.MsgTx().TxOut
		require.Len(ht, outputs, numInputs, "num outputs mismatch")

		// Ensure all the htlc transaction inputs are spending from the
		// commitment transaction, except if this is an extra input
		// added to pay for fees for anchor channels.
		nonCommitmentInputs := 0
		for i, txIn := range inputs {
			if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
				nonCommitmentInputs++

				require.Lessf(ht, nonCommitmentInputs, 2,
					"htlc transaction not "+
						"spending from commit "+
						"tx %v, instead spending %v",
					closingTxID, txIn.PreviousOutPoint)

				// This was an extra input added to pay fees,
				// continue to the next one.
				continue
			}

			// For each htlc timeout transaction, we expect a
			// resolver report recording this on chain resolution
			// for both alice and carol.
			outpoint := txIn.PreviousOutPoint
			resolutionOutpoint := &lnrpc.OutPoint{
				TxidBytes:   outpoint.Hash[:],
				TxidStr:     outpoint.Hash.String(),
				OutputIndex: outpoint.Index,
			}

			// We expect alice to have a timeout tx resolution with
			// an amount equal to the payment amount.
			aliceReports[outpoint.String()] = &lnrpc.Resolution{
				ResolutionType: lnrpc.ResolutionType_OUTGOING_HTLC,
				Outcome:        lnrpc.ResolutionOutcome_FIRST_STAGE,
				SweepTxid:      htlcTx.Hash().String(),
				Outpoint:       resolutionOutpoint,
				AmountSat:      uint64(paymentAmt),
			}

			// We expect carol to have a resolution with an
			// incoming htlc timeout which reflects the full amount
			// of the htlc. It has no spend tx, because carol stops
			// monitoring the htlc once it has timed out.
			carolReports[outpoint.String()] = &lnrpc.Resolution{
				ResolutionType: lnrpc.ResolutionType_INCOMING_HTLC,
				Outcome:        lnrpc.ResolutionOutcome_TIMEOUT,
				SweepTxid:      "",
				Outpoint:       resolutionOutpoint,
				AmountSat:      uint64(paymentAmt),
			}

			// Recorf the HTLC outpoint, such that we can later
			// check whether it gets swept
			op := wire.OutPoint{
				Hash:  *htlcTxID,
				Index: uint32(i),
			}
			htlcTxOutpointSet[op] = 0
		}

		// We record the htlc amount less fees here, so that we know
		// what value to expect for the second stage of our htlc
		// resolution.
		htlcLessFees = uint64(outputs[0].Value)
	}

	// With the htlc timeout txns still in the mempool, we restart Alice to
	// verify that she can resume watching the htlc txns she broadcasted
	// before crashing.
	ht.RestartNode(alice)

	// Generate a block that mines the htlc timeout txns. Doing so now
	// activates the 2nd-stage CSV delayed outputs.
	ht.MineBlocks(1)

	// Alice is restarted here to ensure that she promptly moved the crib
	// outputs to the kindergarten bucket after the htlc timeout txns were
	// confirmed.
	ht.RestartNode(alice)

	// Advance the chain until just before the 2nd-layer CSV delays expire.
	// For anchor channels thhis is one block earlier.
	numBlocks := uint32(defaultCSV - 1)
	if lntest.CommitTypeHasAnchors(channelType) {
		numBlocks = defaultCSV - 2
	}
	ht.MineBlocks(numBlocks)

	// Restart Alice to ensure that she can recover from a failure before
	// having graduated the htlc outputs in the kindergarten bucket.
	ht.RestartNode(alice)

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	err = wait.NoError(func() error {
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)

		if forceClose.LimboBalance == 0 {
			return fmt.Errorf("htlc funds should still be in limbo")
		}

		return checkPendingChannelNumHtlcs(forceClose)
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking force closed channel")

	// Generate a block that causes Alice to sweep the htlc outputs in the
	// kindergarten bucket.
	ht.MineBlocks(1)

	// Wait for the single sweep txn to appear in the mempool.
	htlcSweepTxID := ht.Miner.AssertNumTxsInMempool(1)[0]

	// Fetch the htlc sweep transaction from the mempool.
	htlcSweepTx := ht.Miner.GetRawTransaction(htlcSweepTxID)

	// Ensure the htlc sweep transaction only has one input for each htlc
	// Alice extended before force closing.
	require.Len(ht, htlcSweepTx.MsgTx().TxIn, numInvoices,
		"htlc transaction has wrong num of inputs")
	require.Len(ht, htlcSweepTx.MsgTx().TxOut, 1,
		"htlc sweep transaction should have one output")

	// Ensure that each output spends from exactly one htlc timeout output.
	for _, txIn := range htlcSweepTx.MsgTx().TxIn {
		outpoint := txIn.PreviousOutPoint
		// Check that the input is a confirmed htlc timeout txn.
		_, ok := htlcTxOutpointSet[outpoint]
		require.Truef(ht, ok, "htlc sweep output not spending from "+
			"htlc tx, instead spending output %v", outpoint)

		// Increment our count for how many times this output was spent.
		htlcTxOutpointSet[outpoint]++

		// Check that each is only spent once.
		require.Lessf(ht, htlcTxOutpointSet[outpoint], 2,
			"htlc sweep tx has multiple spends from "+
				"outpoint %v", outpoint)

		// Since we have now swept our htlc timeout tx, we expect to
		// have timeout resolutions for each of our htlcs.
		output := txIn.PreviousOutPoint
		aliceReports[output.String()] = &lnrpc.Resolution{
			ResolutionType: lnrpc.ResolutionType_OUTGOING_HTLC,
			Outcome:        lnrpc.ResolutionOutcome_TIMEOUT,
			SweepTxid:      htlcSweepTx.Hash().String(),
			Outpoint: &lnrpc.OutPoint{
				TxidBytes:   output.Hash[:],
				TxidStr:     output.Hash.String(),
				OutputIndex: output.Index,
			},
			AmountSat: htlcLessFees,
		}
	}

	// Check that each HTLC output was spent exactly once.
	for op, num := range htlcTxOutpointSet {
		require.Equalf(ht, 1, num,
			"HTLC outpoint:%s was spent times", op)
	}

	// Check that we can find the htlc sweep in our set of sweeps using
	// the verbose output of the listsweeps output.
	ht.AssertSweepFound(alice, htlcSweepTx.Hash().String(), true, 0)

	// The following restart checks to ensure that the nursery store is
	// storing the txid of the previously broadcast htlc sweep txn, and that
	// it begins watching that txid after restarting.
	ht.RestartNode(alice)

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	err = wait.NoError(func() error {
		forceClose := ht.AssertChannelPendingForceClose(
			alice, chanPoint,
		)
		err := checkPendingChannelNumHtlcs(forceClose)
		if err != nil {
			return err
		}

		err = checkPendingHtlcStageAndMaturity(
			forceClose, 2, htlcCsvMaturityHeight, 0,
		)
		if err != nil {
			return err
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking force closed channel")

	// Generate the final block that sweeps all htlc funds into the user's
	// wallet, and make sure the sweep is in this block.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, htlcSweepTxID)

	// Now that the channel has been fully swept, it should no longer show
	// up within the pending channels RPC.
	err = wait.NoError(func() error {
		ht.AssertNumPendingForceClose(alice, 0)
		// In addition to there being no pending channels, we verify
		// that pending channels does not report any money still in
		// limbo.
		pendingChanResp := alice.RPC.PendingChannels()
		if pendingChanResp.TotalLimboBalance != 0 {
			return errors.New("no user funds should be left " +
				"in limbo after incubation")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking limbo balance")

	// At this point, Carol should now be aware of her new immediately
	// spendable on-chain balance, as it was Alice who broadcast the
	// commitment transaction.
	carolBalResp = carol.RPC.WalletBalance()

	// Carol's expected balance should be its starting balance plus the
	// push amount sent by Alice and minus the miner fee paid.
	carolExpectedBalance := btcutil.Amount(carolStartingBalance) +
		pushAmt - totalFeeCarol

	// In addition, if this is an anchor-enabled channel, further add the
	// anchor size.
	if lntest.CommitTypeHasAnchors(channelType) {
		carolExpectedBalance += btcutil.Amount(anchorSize)
	}

	require.Equal(ht, carolExpectedBalance,
		btcutil.Amount(carolBalResp.ConfirmedBalance),
		"carol's balance is incorrect")

	// Finally, we check that alice and carol have the set of resolutions
	// we expect.
	assertReports(ht, alice, chanPoint, aliceReports)
	assertReports(ht, carol, chanPoint, carolReports)
}

// padCLTV is a small helper function that pads a cltv value with a block
// padding.
func padCLTV(cltv uint32) uint32 {
	return cltv + uint32(routing.BlockPadding)
}

// testFailingChannel tests that we will fail the channel by force closing it
// in the case where a counterparty tries to settle an HTLC with the wrong
// preimage.
func testFailingChannel(ht *lntest.HarnessTest) {
	const paymentAmt = 10000

	chanAmt := lnd.MaxFundingAmount

	// We'll introduce Carol, which will settle any incoming invoice with a
	// totally unrelated preimage.
	carol := ht.NewNode("Carol", []string{"--hodl.bogus-settle"})

	alice := ht.Alice
	ht.ConnectNodes(alice, carol)

	// Let Alice connect and open a channel to Carol,
	ht.OpenChannel(alice, carol, lntest.OpenChannelParams{Amt: chanAmt})

	// With the channel open, we'll create a invoice for Carol that Alice
	// will attempt to pay.
	preimage := bytes.Repeat([]byte{byte(192)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	resp := carol.RPC.AddInvoice(invoice)

	// Send the payment from Alice to Carol. We expect Carol to attempt to
	// settle this payment with the wrong preimage.
	//
	// NOTE: cannot use `CompletePaymentRequestsNoWait` here as the channel
	// will be force closed, so the num of updates check in that function
	// won't work as the channel cannot be found.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPaymentAndAssertStatus(alice, req, lnrpc.Payment_IN_FLIGHT)

	// Since Alice detects that Carol is trying to trick her by providing a
	// fake preimage, she should fail and force close the channel.
	ht.AssertNumWaitingClose(alice, 1)

	// Mine a block to confirm the broadcasted commitment.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	require.Len(ht, block.Transactions, 2, "transaction wasn't mined")

	// The channel should now show up as force closed both for Alice and
	// Carol.
	ht.AssertNumPendingForceClose(alice, 1)
	ht.AssertNumPendingForceClose(carol, 1)

	// Carol will use the correct preimage to resolve the HTLC on-chain.
	ht.Miner.AssertNumTxsInMempool(1)

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel.
	ht.MineBlocks(defaultCSV - 1)

	// Wait for the sweeping tx to be broadcast.
	ht.Miner.AssertNumTxsInMempool(1)

	// Mine the sweep.
	ht.MineBlocks(1)

	// No pending channels should be left.
	ht.AssertNumPendingForceClose(alice, 0)
}

// assertReports checks that the count of resolutions we have present per
// type matches a set of expected resolutions.
//
// NOTE: only used in current test file.
func assertReports(ht *lntest.HarnessTest, hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, expected map[string]*lnrpc.Resolution) {

	op := ht.OutPointFromChannelPoint(chanPoint)

	// Get our node's closed channels.
	req := &lnrpc.ClosedChannelsRequest{Abandoned: false}
	closed := hn.RPC.ClosedChannels(req)

	var resolutions []*lnrpc.Resolution
	for _, close := range closed.Channels {
		if close.ChannelPoint == op.String() {
			resolutions = close.Resolutions
			break
		}
	}

	require.NotNil(ht, resolutions)
	require.Equal(ht, len(expected), len(resolutions))

	for _, res := range resolutions {
		outPointStr := fmt.Sprintf("%v:%v", res.Outpoint.TxidStr,
			res.Outpoint.OutputIndex)

		expected, ok := expected[outPointStr]
		require.True(ht, ok)
		require.Equal(ht, expected, res)
	}
}

// checkCommitmentMaturity checks that both the maturity height and blocks
// maturity height are as expected.
//
// NOTE: only used in current test file.
func checkCommitmentMaturity(forceClose lntest.PendingForceClose,
	maturityHeight uint32, blocksTilMaturity int32) error {

	if forceClose.MaturityHeight != maturityHeight {
		return fmt.Errorf("expected commitment maturity height to be "+
			"%d, found %d instead", maturityHeight,
			forceClose.MaturityHeight)
	}
	if forceClose.BlocksTilMaturity != blocksTilMaturity {
		return fmt.Errorf("expected commitment blocks til maturity to "+
			"be %d, found %d instead", blocksTilMaturity,
			forceClose.BlocksTilMaturity)
	}

	return nil
}

// checkPendingHtlcStageAndMaturity uniformly tests all pending htlc's belonging
// to a force closed channel, testing for the expected stage number, blocks till
// maturity, and the maturity height.
//
// NOTE: only used in current test file.
func checkPendingHtlcStageAndMaturity(
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	stage, maturityHeight uint32, blocksTillMaturity int32) error {

	for _, pendingHtlc := range forceClose.PendingHtlcs {
		if pendingHtlc.Stage != stage {
			return fmt.Errorf("expected pending htlc to be stage "+
				"%d, found %d", stage, pendingHtlc.Stage)
		}
		if pendingHtlc.MaturityHeight != maturityHeight {
			return fmt.Errorf("expected pending htlc maturity "+
				"height to be %d, instead has %d",
				maturityHeight, pendingHtlc.MaturityHeight)
		}
		if pendingHtlc.BlocksTilMaturity != blocksTillMaturity {
			return fmt.Errorf("expected pending htlc blocks til "+
				"maturity to be %d, instead has %d",
				blocksTillMaturity,
				pendingHtlc.BlocksTilMaturity)
		}
	}

	return nil
}
