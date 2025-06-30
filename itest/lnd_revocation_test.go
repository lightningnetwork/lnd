package itest

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

func breachRetributionTestCase(ht *lntest.HarnessTest,
	commitType lnrpc.CommitmentType) {

	const (
		chanAmt     = funding.MaxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Carol will be the breached party. We set --nolisten to ensure Bob
	// won't be able to connect to her and trigger the channel data
	// protection logic automatically. We also can't have Carol
	// automatically re-connect too early, otherwise DLP would be initiated
	// instead of the breach we want to provoke.
	nodeArgs := lntest.NodeArgsForCommitType(commitType)
	carol := ht.NewNode(
		"Carol",
		append(
			nodeArgs,
			[]string{"--hodl.exit-settle", "--nolisten",
				"--minbackoff=1h"}...,
		),
	)

	// We must let Bob communicate with Carol before they are able to open
	// channel, so we connect Bob and Carol,
	bob := ht.NewNode("Bob", nodeArgs)
	ht.ConnectNodes(carol, bob)

	// Before we make a channel, we'll load up Carol with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// In order to test Carol's response to an uncooperative channel
	// closure by Bob, we'll first open up a channel between them with a
	// 0.5 BTC value.
	privateChan := commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT
	chanPoint := ht.OpenChannel(
		carol, bob, lntest.OpenChannelParams{
			CommitmentType: commitType,
			Amt:            chanAmt,
			Private:        privateChan,
		},
	)

	// With the channel open, we'll create a few invoices for Bob that
	// Carol will pay to in order to advance the state of the channel.
	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numInvoices)

	// Send payments from Carol to Bob using 3 of Bob's payment hashes
	// generated above.
	ht.CompletePaymentRequests(carol, bobPayReqs[:numInvoices/2])

	// Next query for Bob's channel state, as we sent 3 payments of 10k
	// satoshis each, Bob should now see his balance as being 30k satoshis.
	bobChan := ht.AssertChannelLocalBalance(bob, chanPoint, 30_000)

	// Grab Bob's current commitment height (update number), we'll later
	// revert him to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	bobStateNumPreCopy := bobChan.NumUpdates

	// With the temporary file created, copy Bob's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	ht.BackupDB(bob)

	// Reconnect the peers after the restart that was needed for the db
	// backup.
	ht.EnsureConnected(carol, bob)

	// Because Bob has been restarted, we need to make sure Carol has
	// remarked the channel as active after that.
	ht.AssertChannelExists(carol, chanPoint)

	// Finally, send payments from Carol to Bob, consuming Bob's remaining
	// payment hashes.
	ht.CompletePaymentRequests(carol, bobPayReqs[numInvoices/2:])

	// Now we shutdown Bob, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Bob to travel back in time
	// within the channel's history.
	ht.RestartNodeAndRestoreDB(bob)

	// Now query for Bob's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	ht.AssertChannelCommitHeight(bob, chanPoint, int(bobStateNumPreCopy))

	// Now force Bob to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Carol's retribution.
	_, breachCloseUpd := ht.CloseChannelAssertPending(bob, chanPoint, true)
	closeUpd := breachCloseUpd.GetClosePending()
	require.NotNil(ht, closeUpd)
	breachTXID, err := chainhash.NewHash(closeUpd.Txid)
	require.NoError(ht, err)

	// Here, Carol sees Bob's breach transaction in the mempool, but is
	// waiting for it to confirm before continuing her retribution. We
	// restart Carol to ensure that she is persisting her retribution state
	// and continues watching for the breach transaction to confirm even
	// after her node restarts.
	ht.RestartNode(carol)

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, *breachTXID)

	// Construct to_remote output which pays to Bob. Based on the output
	// ordering, the first output in this breach tx is the to_remote
	// output.
	toRemoteOp := wire.OutPoint{
		Hash:  *breachTXID,
		Index: 0,
	}

	// If this is an anchor-enabled channel, the first two outputs are
	// anchors, so the to_remote output is the third one.
	if lntest.CommitTypeHasAnchors(commitType) {
		toRemoteOp.Index = 2
	}

	// Query the mempool for Carol's justice transaction, this should be
	// broadcast as Bob's contract breaching transaction gets confirmed
	// above.
	//
	// NOTE: For channels with anchors, we will also see the anchor
	// sweeping transactions in the mempool. Thus we directly assert that
	// the breach transaction's outpoint is seen in the mempool instead of
	// checking the number of transactions.
	justiceTx := ht.AssertOutpointInMempool(toRemoteOp)

	// Assert that all the inputs of this transaction are spending outputs
	// generated by Bob's breach transaction above.
	for _, txIn := range justiceTx.TxIn {
		require.Equal(ht, *breachTXID, txIn.PreviousOutPoint.Hash,
			"justice tx not spending commitment utxo")
	}

	// We restart Carol here to ensure that she persists her retribution
	// state and successfully continues exacting retribution after
	// restarting. At this point, Carol has broadcast the justice
	// transaction, but it hasn't been confirmed yet; when Carol restarts,
	// she should start waiting for the justice transaction to confirm
	// again.
	ht.RestartNode(carol)

	// For anchor channels, we'd offer the anchor output to the sweeper.
	// However, this anchor output won't be swept due to it being
	// uneconomical.
	if lntest.CommitTypeHasAnchors(commitType) {
		ht.AssertNumPendingSweeps(carol, 1)
	}

	// Now mine a block, this transaction should include Carol's justice
	// transaction which was just accepted into the mempool.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	justiceTxid := justiceTx.TxHash()
	ht.AssertTxInBlock(block, justiceTxid)

	ht.AssertNodeNumChannels(carol, 0)

	// Mine enough blocks for Bob's channel arbitrator to wrap up the
	// references to the breached channel. The chanarb waits for commitment
	// tx's confHeight+CSV-1 blocks and since we've already mined one that
	// included the justice tx we only need to mine extra DefaultCSV-2
	// blocks to unlock it.
	ht.MineBlocks(defaultCSV - 2)

	ht.AssertNumPendingForceClose(bob, 0)
}

// testRevokedCloseRetribution tests that Carol is able carry out retribution
// in the event that she fails immediately after detecting Bob's breach txn in
// the mempool.
func testRevokedCloseRetribution(ht *lntest.HarnessTest) {
	for _, commitType := range []lnrpc.CommitmentType{
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	} {
		testName := fmt.Sprintf("%v", commitType.String())
		ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			breachRetributionTestCase(st, commitType)
		})
	}
}

func revokedCloseRetributionZeroValueRemoteOutputCase(ht *lntest.HarnessTest,
	commitType lnrpc.CommitmentType) {

	const (
		chanAmt     = funding.MaxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	nodeArgs := lntest.NodeArgsForCommitType(commitType)
	carol := ht.NewNode(
		"Carol",
		append(nodeArgs, []string{"--hodl.exit-settle"}...),
	)

	// Dave will be the breached party. We set --nolisten to ensure Carol
	// won't be able to connect to him and trigger the channel data
	// protection logic automatically. We also can't have Dave automatically
	// re-connect too early, otherwise DLP would be initiated instead of the
	// breach we want to provoke.
	dave := ht.NewNode(
		"Dave",
		append(
			nodeArgs,
			[]string{"--hodl.exit-settle", "--nolisten",
				"--minbackoff=1h"}...),
	)

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Carol,
	ht.ConnectNodes(dave, carol)

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// In order to test Dave's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 BTC value.
	privateChan := commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT
	chanPoint := ht.OpenChannel(
		dave, carol, lntest.OpenChannelParams{
			CommitmentType: commitType,
			Amt:            chanAmt,
			Private:        privateChan,
		},
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, numInvoices)

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should now see her balance as being 0 satoshis.
	carolChan := ht.AssertChannelLocalBalance(carol, chanPoint, 0)

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := int(carolChan.NumUpdates)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	ht.BackupDB(carol)

	// Reconnect the peers after the restart that was needed for the db
	// backup.
	ht.EnsureConnected(dave, carol)

	// Once connected, give Dave some time to enable the channel again.
	ht.AssertChannelInGraph(dave, chanPoint)

	// Finally, send payments from Dave to Carol, consuming Carol's
	// remaining payment hashes.
	ht.CompletePaymentRequestsNoWait(dave, carolPayReqs, chanPoint)

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	ht.RestartNodeAndRestoreDB(carol)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, not the *latest* state.
	ht.AssertChannelCommitHeight(carol, chanPoint, carolStateNumPreCopy)

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Dave's retribution.
	stream, closeUpdate := ht.CloseChannelAssertPending(
		carol, chanPoint, true,
	)

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Here, Dave receives a confirmation of Carol's breach transaction.
	// We restart Dave to ensure that he is persisting his retribution
	// state and continues exacting justice after his node restarts.
	ht.RestartNode(dave)

	closeTxID := closeUpdate.GetClosePending().Txid

	// The breachTXID should match the above closeTxID.
	breachTXID := ht.WaitForChannelCloseEvent(stream)
	require.EqualValues(ht, breachTXID[:], closeTxID)

	// Construct to_local output which pays to Dave. Based on the output
	// ordering, the first output in this breach tx is the to_local
	// output.
	toLocalOp := wire.OutPoint{
		Hash:  breachTXID,
		Index: 0,
	}

	// If this is an anchor-enabled channel, we usaually have two anchors,
	// one for local and one for remote. However, since the to_remote
	// balance is zero, the remote anchor won't be created, thus the
	// to_local output is the second output.
	if lntest.CommitTypeHasAnchors(commitType) {
		toLocalOp.Index = 1
	}

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	//
	// NOTE: For channels with anchors, we will also see the anchor
	// sweeping transactions in the mempool. Thus we directly assert that
	// the breach transaction's outpoint is seen in the mempool instead of
	// checking the number of transactions.
	justiceTx := ht.AssertOutpointInMempool(toLocalOp)

	// Assert that all the inputs of this transaction are spending outputs
	// generated by Carol's breach transaction above.
	for _, txIn := range justiceTx.TxIn {
		require.Equal(ht, breachTXID[:], txIn.PreviousOutPoint.Hash[:],
			"justice tx not spending commitment utxo ")
	}

	// We restart Dave here to ensure that he persists his retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Dave has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Dave restarts, he should start waiting for
	// the justice transaction to confirm again.
	ht.RestartNode(dave)

	// For anchor channels, we'd also create the sweeping transaction.
	if lntest.CommitTypeHasAnchors(commitType) {
		ht.AssertNumPendingSweeps(dave, 1)
	}

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	justiceTxid := justiceTx.TxHash()
	ht.AssertTxInBlock(block, justiceTxid)

	// At this point, Dave should have no pending channels.
	ht.AssertNodeNumChannels(dave, 0)
}

// testRevokedCloseRetributionZeroValueRemoteOutput tests that Dave is able
// carry out retribution in the event that he fails in state where the remote
// commitment output has zero-value.
func testRevokedCloseRetributionZeroValueRemoteOutput(ht *lntest.HarnessTest) {
	for _, commitType := range []lnrpc.CommitmentType{
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	} {
		testName := fmt.Sprintf("%v", commitType.String())
		ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			revokedCloseRetributionZeroValueRemoteOutputCase(
				st, commitType,
			)
		})
	}
}

func revokedCloseRetributionRemoteHodlCase(ht *lntest.HarnessTest,
	commitType lnrpc.CommitmentType) {

	const (
		chanAmt     = funding.MaxBtcFundingAmount
		pushAmt     = 200000
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since this test will result in the counterparty being left in a
	// weird state, we will introduce another node into our test network:
	// Carol.
	nodeArgs := lntest.NodeArgsForCommitType(commitType)
	carol := ht.NewNode(
		"Carol",
		append(nodeArgs, []string{"--hodl.exit-settle"}...),
	)

	// We'll also create a new node Dave, who will have a channel with
	// Carol, and also use similar settings so we can broadcast a commit
	// with active HTLCs. Dave will be the breached party. We set
	// --nolisten to ensure Carol won't be able to connect to him and
	// trigger the channel data protection logic automatically.
	dave := ht.NewNode(
		"Dave",
		append(
			nodeArgs,
			[]string{"--hodl.exit-settle", "--nolisten"}...,
		),
	)

	// We must let Dave communicate with Carol before they are able to open
	// channel, so we connect Dave and Carol,
	ht.ConnectNodes(dave, carol)

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// In order to test Dave's response to an uncooperative channel closure
	// by Carol, we'll first open up a channel between them with a
	// funding.MaxBtcFundingAmount (2^24) satoshis value.
	privateChan := commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT
	chanPoint := ht.OpenChannel(
		dave, carol, lntest.OpenChannelParams{
			Amt:            chanAmt,
			PushAmt:        pushAmt,
			Private:        privateChan,
			CommitmentType: commitType,
		},
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, numInvoices)

	// We'll introduce an closure to validate that Carol's current
	// number of updates is at least as large as the provided minimum
	// number.
	checkCarolNumUpdatesAtLeast := func(carolChan *lnrpc.Channel,
		minimum int) {

		require.GreaterOrEqual(ht, int(carolChan.NumUpdates), minimum,
			"carol's numupdates is incorrect")
	}

	// Ensure that carol's balance starts with the amount we pushed to her.
	ht.AssertChannelLocalBalance(carol, chanPoint, pushAmt)

	// Send payments from Dave to Carol using 3 of Carol's payment hashes
	// generated above.
	ht.CompletePaymentRequestsNoWait(
		dave, carolPayReqs[:numInvoices/2], chanPoint,
	)

	// At this point, we'll also send over a set of HTLC's from Carol to
	// Dave. This ensures that the final revoked transaction has HTLC's in
	// both directions.
	davePayReqs, _, _ := ht.CreatePayReqs(dave, paymentAmt, numInvoices)

	// Send payments from Carol to Dave using 3 of Dave's payment hashes
	// generated above.
	ht.CompletePaymentRequestsNoWait(
		carol, davePayReqs[:numInvoices/2], chanPoint,
	)

	// Next query for Carol's channel state, as we sent 3 payments of 10k
	// satoshis each, however Carol should now see her balance as being
	// equal to the push amount in satoshis since she has not settled.

	// Ensure that carol's balance still reflects the original amount we
	// pushed to her, minus the HTLCs she just sent to Dave.
	carolChan := ht.AssertChannelLocalBalance(
		carol, chanPoint, pushAmt-3*paymentAmt,
	)

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := int(carolChan.NumUpdates)

	// Since Carol has not settled, she should only see at least one update
	// to her channel.
	checkCarolNumUpdatesAtLeast(carolChan, 1)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	ht.BackupDB(carol)

	// Reconnect the peers after the restart that was needed for the db
	// backup.
	ht.EnsureConnected(dave, carol)

	// Once connected, give Dave some time to enable the channel again.
	ht.AssertChannelInGraph(dave, chanPoint)

	// Finally, send payments from Dave to Carol, consuming Carol's
	// remaining payment hashes.
	ht.CompletePaymentRequestsNoWait(
		dave, carolPayReqs[numInvoices/2:], chanPoint,
	)

	// Ensure that carol's balance still shows the amount we originally
	// pushed to her (minus the HTLCs she sent to Bob), and that at least
	// one more update has occurred.
	carolChan = ht.AssertChannelLocalBalance(
		carol, chanPoint, pushAmt-3*paymentAmt,
	)
	checkCarolNumUpdatesAtLeast(carolChan, carolStateNumPreCopy+1)

	// Suspend Dave, such that Carol won't reconnect at startup, triggering
	// the data loss protection.
	restartDave := ht.SuspendNode(dave)

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	ht.RestartNodeAndRestoreDB(carol)

	// Ensure that Carol's view of the channel is consistent with the state
	// of the channel just before it was snapshotted.
	carolChan = ht.AssertChannelLocalBalance(
		carol, chanPoint, pushAmt-3*paymentAmt,
	)
	checkCarolNumUpdatesAtLeast(carolChan, 1)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, *not* the latest state.
	ht.AssertChannelCommitHeight(carol, chanPoint, carolStateNumPreCopy)

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Dave's retribution.
	closeUpdates, closeUpd := ht.CloseChannelAssertPending(
		carol, chanPoint, true,
	)
	pendingCloseUpd := closeUpd.GetClosePending()
	require.NotNil(ht, pendingCloseUpd)
	closeTxID, err := chainhash.NewHash(pendingCloseUpd.Txid)
	require.NoError(ht, err)

	// Generate a single block to mine the breach transaction.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// We resurrect Dave to ensure he will be exacting justice after his
	// node restarts.
	require.NoError(ht, restartDave(), "unable to restart Dave's node")

	// Finally, wait for the final close status update, then ensure that
	// the closing transaction was included in the block.
	breachTXID := ht.WaitForChannelCloseEvent(closeUpdates)
	require.Equal(ht, closeTxID[:], breachTXID[:],
		"expected breach ID to be equal to close ID")
	ht.AssertTxInBlock(block, breachTXID)

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above. Since Carol might have had the time to take some of the HTLC
	// outputs to the second level before Dave broadcasts his justice tx,
	// we'll search through the mempool for a tx that matches the number of
	// expected inputs in the justice tx.
	var justiceTxid chainhash.Hash
	errNotFound := errors.New("justice tx not found")
	findJusticeTx := func() (*chainhash.Hash, error) {
		mempool := ht.GetRawMempool()

		for _, txid := range mempool {
			// Check that the justice tx has the appropriate number
			// of inputs.
			//
			// NOTE: We don't use `ht.GetRawTransaction`
			// which asserts a txid must be found as the HTLC
			// spending txes might be aggregated.
			tx, err := ht.Miner().Client.GetRawTransaction(&txid)
			if err != nil {
				return nil, err
			}

			exNumInputs := 2 + numInvoices
			if len(tx.MsgTx().TxIn) == exNumInputs {
				return &txid, nil
			}
		}

		return nil, errNotFound
	}

	err = wait.NoError(func() error {
		txid, err := findJusticeTx()
		if err != nil {
			return err
		}
		justiceTxid = *txid

		return nil
	}, defaultTimeout)

	if err != nil && errors.Is(err, errNotFound) {
		// If Dave is unable to broadcast his justice tx on first
		// attempt because of the second layer transactions, he will
		// wait until the next block epoch before trying again. Because
		// of this, we'll mine a block if we cannot find the justice tx
		// immediately. Since we cannot tell for sure how many
		// transactions will be in the mempool at this point, we pass 0
		// as the last argument, indicating we don't care what's in the
		// mempool.
		ht.MineEmptyBlocks(1)
		err = wait.NoError(func() error {
			txid, err := findJusticeTx()
			if err != nil {
				return err
			}

			justiceTxid = *txid

			return nil
		}, defaultTimeout)
	}
	require.NoError(ht, err, "timeout finding justice tx")

	justiceTx := ht.GetRawTransaction(justiceTxid)

	// isSecondLevelSpend checks that the passed secondLevelTxid is a
	// potentitial second level spend spending from the commit tx.
	isSecondLevelSpend := func(commitTxid,
		secondLevelTxid chainhash.Hash) bool {

		secondLevel := ht.GetRawTransaction(secondLevelTxid)

		// A second level spend should have only one input, and one
		// output.
		if len(secondLevel.MsgTx().TxIn) != 1 {
			return false
		}
		if len(secondLevel.MsgTx().TxOut) != 1 {
			return false
		}

		// The sole input should be spending from the commit tx.
		txIn := secondLevel.MsgTx().TxIn[0]

		return bytes.Equal(txIn.PreviousOutPoint.Hash[:], commitTxid[:])
	}

	// Check that all the inputs of this transaction are spending outputs
	// generated by Carol's breach transaction above.
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			continue
		}

		// If the justice tx is spending from an output that was not on
		// the breach tx, Carol might have had the time to take an
		// output to the second level. In that case, check that the
		// justice tx is spending this second level output.
		if isSecondLevelSpend(breachTXID, txIn.PreviousOutPoint.Hash) {
			continue
		}
		require.Fail(ht, "justice tx not spending commitment utxo "+
			"instead is: %v", txIn.PreviousOutPoint)
	}

	// We restart Dave here to ensure that he persists he retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Dave has broadcast the justice transaction, but it
	// hasn't been confirmed yet; when Dave restarts, he should start
	// waiting for the justice transaction to confirm again.
	ht.RestartNode(dave)

	// For anchor channels, we'd also create the sweeping transaction.
	if lntest.CommitTypeHasAnchors(commitType) {
		ht.AssertNumPendingSweeps(dave, 1)
	}

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Dave should have no open channels.
	ht.AssertNodeNumChannels(dave, 0)
}

// testRevokedCloseRetributionRemoteHodl tests that Dave properly responds to a
// channel breach made by the remote party, specifically in the case that the
// remote party breaches before settling extended HTLCs.
func testRevokedCloseRetributionRemoteHodl(ht *lntest.HarnessTest) {
	for _, commitType := range []lnrpc.CommitmentType{
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	} {
		testName := fmt.Sprintf("%v", commitType.String())
		ht.Run(testName, func(t *testing.T) {
			st := ht.Subtest(t)
			revokedCloseRetributionRemoteHodlCase(st, commitType)
		})
	}
}
