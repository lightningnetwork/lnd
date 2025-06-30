package itest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// watchtowerTestCases defines a set of tests to check the behaviour of the
// watchtower client and server.
var watchtowerTestCases = []*lntest.TestCase{
	{
		Name:     "revoked close retribution altruist",
		TestFunc: testRevokedCloseRetributionAltruistWatchtower,
	},
	{
		Name:     "client session deletion",
		TestFunc: testTowerClientSessionDeletion,
	},
	{
		Name:     "client tower and session management",
		TestFunc: testTowerClientTowerAndSessionManagement,
	},
}

// testTowerClientTowerAndSessionManagement tests the various control commands
// that a user has over the client's set of active towers and sessions.
func testTowerClientTowerAndSessionManagement(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	const (
		chanAmt           = funding.MaxBtcFundingAmount
		externalIP        = "1.2.3.4"
		externalIP2       = "1.2.3.5"
		sessionCloseRange = 1
	)

	// Set up Wallis the watchtower who will be used by Dave to watch over
	// his channel commitment transactions.
	wallisPk, wallisListener, _ := setUpNewTower(ht, "Wallis", externalIP)

	// Dave will be the tower client.
	daveArgs := []string{
		"--wtclient.active",
		fmt.Sprintf(
			"--wtclient.session-close-range=%d", sessionCloseRange,
		),
	}
	dave := ht.NewNode("Dave", daveArgs)

	addWallisReq := &wtclientrpc.AddTowerRequest{
		Pubkey:  wallisPk,
		Address: wallisListener,
	}
	dave.RPC.AddTower(addWallisReq)

	assertNumSessions := func(towerPk []byte, expectedNum int,
		mineOnFail bool) {

		err := wait.NoError(func() error {
			info := dave.RPC.GetTowerInfo(
				&wtclientrpc.GetTowerInfoRequest{
					Pubkey:          towerPk,
					IncludeSessions: true,
				},
			)

			var numSessions uint32
			for _, sessionType := range info.SessionInfo {
				numSessions += sessionType.NumSessions
			}
			if numSessions == uint32(expectedNum) {
				return nil
			}

			if mineOnFail {
				ht.MineBlocks(1)
			}

			return fmt.Errorf("expected %d sessions, got %d",
				expectedNum, numSessions)
		}, defaultTimeout)
		require.NoError(ht, err)
	}

	// Assert that there are a few sessions between Dave and Wallis. There
	// should be one per client. There are currently 3 types of clients, so
	// we expect 3 sessions.
	assertNumSessions(wallisPk, 3, false)

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Connect Dave and Alice.
	ht.ConnectNodes(dave, alice)

	// Open a channel between Dave and Alice.
	params := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	chanPoint := ht.OpenChannel(dave, alice, params)

	// Show that the Wallis tower is currently seen as an active session
	// candidate.
	info := dave.RPC.GetTowerInfo(&wtclientrpc.GetTowerInfoRequest{
		Pubkey: wallisPk,
	})
	require.GreaterOrEqual(ht, len(info.SessionInfo), 1)
	require.True(ht, info.SessionInfo[0].ActiveSessionCandidate)

	// Make some back-ups and assert that they are added to a session with
	// the tower.
	generateBackups(ht, dave, alice, 4)

	// Assert that one of the sessions now has 4 backups.
	assertNumBackups(ht, dave.RPC, wallisPk, 4, false)

	// Now, deactivate the tower and show that it is no longer considered
	// an active session candidate.
	dave.RPC.DeactivateTower(&wtclientrpc.DeactivateTowerRequest{
		Pubkey: wallisPk,
	})
	info = dave.RPC.GetTowerInfo(&wtclientrpc.GetTowerInfoRequest{
		Pubkey: wallisPk,
	})
	require.GreaterOrEqual(ht, len(info.SessionInfo), 1)
	require.False(ht, info.SessionInfo[0].ActiveSessionCandidate)

	// Back up a few more states.
	generateBackups(ht, dave, alice, 4)

	// These should _not_ be on the tower. Therefore, the number of
	// back-ups on the tower should be the same as before.
	assertNumBackups(ht, dave.RPC, wallisPk, 4, false)

	// Add new tower and connect Dave to it.
	wilmaPk, wilmaListener, _ := setUpNewTower(ht, "Wilma", externalIP2)
	dave.RPC.AddTower(&wtclientrpc.AddTowerRequest{
		Pubkey:  wilmaPk,
		Address: wilmaListener,
	})
	assertNumSessions(wilmaPk, 3, false)

	// The updates from before should now appear on the new watchtower.
	assertNumBackups(ht, dave.RPC, wilmaPk, 4, false)

	// Reactivate the Wallis tower and then deactivate the Wilma one.
	dave.RPC.AddTower(addWallisReq)
	dave.RPC.DeactivateTower(&wtclientrpc.DeactivateTowerRequest{
		Pubkey: wilmaPk,
	})

	// Generate some more back-ups.
	generateBackups(ht, dave, alice, 4)

	// Assert that they get added to the first tower (Wallis) and that the
	// number of sessions with Wallis has not changed - in other words, the
	// previously used session was re-used.
	assertNumBackups(ht, dave.RPC, wallisPk, 8, false)
	assertNumSessions(wallisPk, 3, false)

	findSession := func(towerPk []byte, numBackups uint32) []byte {
		info := dave.RPC.GetTowerInfo(&wtclientrpc.GetTowerInfoRequest{
			Pubkey:          towerPk,
			IncludeSessions: true,
		})

		for _, sessionType := range info.SessionInfo {
			for _, session := range sessionType.Sessions {
				if session.NumBackups == numBackups {
					return session.Id
				}
			}
		}
		ht.Fatalf("session with %d backups not found", numBackups)

		return nil
	}

	// Now we will test the termination of a session.
	// First, we need to figure out the ID of the session that has been used
	// for back-ups.
	sessionID := findSession(wallisPk, 8)

	// Now, terminate the session.
	dave.RPC.TerminateSession(&wtclientrpc.TerminateSessionRequest{
		SessionId: sessionID,
	})

	// This should force the client to negotiate a new session. The old
	// session still remains in our session list since the channel for which
	// it has updates for is still open.
	assertNumSessions(wallisPk, 4, false)

	// Any new back-ups should now be backed up on a different session.
	generateBackups(ht, dave, alice, 2)
	assertNumBackups(ht, dave.RPC, wallisPk, 10, false)
	findSession(wallisPk, 2)

	// Close the channel.
	ht.CloseChannelAssertPending(dave, chanPoint, false)

	// Mine enough blocks to surpass the session close range buffer.
	ht.MineBlocksAndAssertNumTxes(sessionCloseRange+6, 1)

	// The session that was previously terminated now gets deleted since
	// the channel for which it has updates has now been closed. All the
	// remaining sessions are not yet closable since they are not yet
	// exhausted and are all still active. We set mineOnFail to true here
	// so that we can ensure that the closable session queue continues to
	// be checked on each new block. It could have been the case that all
	// checks with the above mined blocks were completed before the
	// closable session was queued.
	assertNumSessions(wallisPk, 3, true)

	// For the sake of completion, we call RemoveTower here for both towers
	// to show that this should never error.
	dave.RPC.RemoveTower(&wtclientrpc.RemoveTowerRequest{
		Pubkey: wallisPk,
	})
	dave.RPC.RemoveTower(&wtclientrpc.RemoveTowerRequest{
		Pubkey: wilmaPk,
	})
}

// testTowerClientSessionDeletion tests that sessions are correctly deleted
// when they are deemed closable.
func testTowerClientSessionDeletion(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	const (
		chanAmt           = funding.MaxBtcFundingAmount
		numInvoices       = 5
		maxUpdates        = numInvoices * 2
		externalIP        = "1.2.3.4"
		sessionCloseRange = 1
	)

	// Set up Wallis the watchtower who will be used by Dave to watch over
	// his channel commitment transactions.
	wallisPk, listener, _ := setUpNewTower(ht, "Wallis", externalIP)

	// Dave will be the tower client.
	daveArgs := []string{
		"--wtclient.active",
		fmt.Sprintf("--wtclient.max-updates=%d", maxUpdates),
		fmt.Sprintf(
			"--wtclient.session-close-range=%d", sessionCloseRange,
		),
	}
	dave := ht.NewNode("Dave", daveArgs)

	addTowerReq := &wtclientrpc.AddTowerRequest{
		Pubkey:  wallisPk,
		Address: listener,
	}
	dave.RPC.AddTower(addTowerReq)

	// Assert that there exists a session between Dave and Wallis.
	err := wait.NoError(func() error {
		info := dave.RPC.GetTowerInfo(&wtclientrpc.GetTowerInfoRequest{
			Pubkey:          wallisPk,
			IncludeSessions: true,
		})

		var numSessions uint32
		for _, sessionType := range info.SessionInfo {
			numSessions += sessionType.NumSessions
		}
		if numSessions > 0 {
			return nil
		}

		return fmt.Errorf("expected a non-zero number of sessions")
	}, defaultTimeout)
	require.NoError(ht, err)

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Connect Dave and Alice.
	ht.ConnectNodes(dave, alice)

	// Open a channel between Dave and Alice.
	params := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	chanPoint := ht.OpenChannel(dave, alice, params)

	// Since there are 2 updates made for every payment and the maximum
	// number of updates per session has been set to 10, make 5 payments
	// between the pair so that the session is exhausted.
	generateBackups(ht, dave, alice, maxUpdates)

	// Assert that one of the sessions now has 10 backups.
	assertNumBackups(ht, dave.RPC, wallisPk, 10, false)

	// Now close the channel and wait for the close transaction to appear
	// in the mempool so that it is included in a block when we mine.
	ht.CloseChannelAssertPending(dave, chanPoint, false)

	// Mine enough blocks to surpass the session-close-range. This should
	// trigger the session to be deleted.
	ht.MineBlocksAndAssertNumTxes(sessionCloseRange+6, 1)

	// Wait for the session to be deleted. We know it has been deleted once
	// the number of backups is back to zero. We check for number of backups
	// instead of number of sessions because it is expected that the client
	// would immediately negotiate another session after deleting the
	// exhausted one. This time we set the "mineOnFail" parameter to true to
	// ensure that the session deleting logic is run.
	assertNumBackups(ht, dave.RPC, wallisPk, 0, true)
}

// testRevokedCloseRetributionAltruistWatchtower establishes a channel between
// Carol and Dave, where Carol is using a third node Willy as her watchtower.
// After sending some payments, Dave reverts his state and force closes to
// trigger a breach. Carol is kept offline throughout the process and the test
// asserts that Willy responds by broadcasting the justice transaction on
// Carol's behalf sweeping her funds without a reward.
func testRevokedCloseRetributionAltruistWatchtower(ht *lntest.HarnessTest) {
	for _, commitType := range []lnrpc.CommitmentType{
		lnrpc.CommitmentType_LEGACY,
		lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	} {
		testName := fmt.Sprintf("%v", commitType.String())
		ct := commitType
		testFunc := func(ht *lntest.HarnessTest) {
			testRevokedCloseRetributionAltruistWatchtowerCase(
				ht, ct,
			)
		}

		success := ht.Run(testName, func(tt *testing.T) {
			st := ht.Subtest(tt)

			st.RunTestCase(&lntest.TestCase{
				Name:     testName,
				TestFunc: testFunc,
			})
		})

		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			ht.Logf("Failure time: %v", time.Now().Format(
				"2006-01-02 15:04:05.000",
			))

			break
		}
	}
}

func testRevokedCloseRetributionAltruistWatchtowerCase(ht *lntest.HarnessTest,
	commitType lnrpc.CommitmentType) {

	const (
		chanAmt     = funding.MaxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
		externalIP  = "1.2.3.4"
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carolArgs := lntest.NodeArgsForCommitType(commitType)
	carolArgs = append(carolArgs, "--hodl.exit-settle")

	carol := ht.NewNode("Carol", carolArgs)

	// Set up Willy the watchtower who will protect Dave from Carol's
	// breach. He will remain online in order to punish Carol on Dave's
	// behalf, since the breach will happen while Dave is offline.
	willyInfoPk, listener, willy := setUpNewTower(ht, "Willy", externalIP)

	// Dave will be the breached party. We set --nolisten to ensure Carol
	// won't be able to connect to him and trigger the channel data
	// protection logic automatically.
	daveArgs := lntest.NodeArgsForCommitType(commitType)
	daveArgs = append(daveArgs, "--nolisten", "--wtclient.active")
	dave := ht.NewNodeWithCoins("Dave", daveArgs)

	addTowerReq := &wtclientrpc.AddTowerRequest{
		Pubkey:  willyInfoPk,
		Address: listener,
	}
	dave.RPC.AddTower(addTowerReq)

	// We must let Dave have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	ht.ConnectNodes(dave, carol)

	// Send one more UTXOs if this is a neutrino backend.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)
	}

	// In order to test Dave's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 BTC value.
	params := lntest.OpenChannelParams{
		Amt:            3 * (chanAmt / 4),
		PushAmt:        chanAmt / 4,
		CommitmentType: commitType,
		Private:        true,
	}
	chanPoint := ht.OpenChannel(dave, carol, params)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs, _, _ := ht.CreatePayReqs(carol, paymentAmt, numInvoices)

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should still see her balance as the push amount, which is 1/4 of the
	// capacity.
	carolChan := ht.AssertChannelLocalBalance(
		carol, chanPoint, int64(chanAmt/4),
	)

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force him to
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

	daveBalResp := dave.RPC.WalletBalance()
	davePreSweepBalance := daveBalResp.ConfirmedBalance

	// Wait until the backup has been accepted by the watchtower before
	// shutting down Dave.
	err := wait.NoError(func() error {
		bkpStats := dave.RPC.WatchtowerStats()
		if bkpStats == nil {
			return errors.New("no active backup sessions")
		}
		if bkpStats.NumBackups == 0 {
			return errors.New("no backups accepted")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "unable to verify backup task completed")

	// Shutdown Dave to simulate going offline for an extended period of
	// time. Once he's not watching, Carol will try to breach the channel.
	restart := ht.SuspendNode(dave)

	// Now we shutdown Carol, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	ht.RestartNodeAndRestoreDB(carol)

	// Now query for Carol's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	ht.AssertChannelCommitHeight(carol, chanPoint, carolStateNumPreCopy)

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Dave's retribution.
	closeUpdates, pendingClose := ht.CloseChannelAssertPending(
		carol, chanPoint, true,
	)

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	breachTXID := ht.WaitForChannelCloseEvent(closeUpdates)
	ht.AssertTxInBlock(block, breachTXID)

	// The breachTXID should match the above closeTxID.
	closeTxID := pendingClose.GetClosePending().Txid
	require.EqualValues(ht, breachTXID[:], closeTxID)

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID := ht.AssertNumTxsInMempool(1)[0]

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
	justiceTx := ht.GetRawTransaction(justiceTXID)
	for _, txIn := range justiceTx.MsgTx().TxIn {
		require.Equal(ht, breachTXID[:], txIn.PreviousOutPoint.Hash[:],
			"justice tx not spending commitment utxo")
	}

	willyBalResp := willy.WalletBalance()
	require.Zero(ht, willyBalResp.ConfirmedBalance,
		"willy should have 0 balance before mining justice transaction")

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	require.Len(ht, block.Transactions, 2, "transaction wasn't mined")
	justiceSha := block.Transactions[1].TxHash()
	require.Equal(ht, justiceTx.Hash()[:], justiceSha[:],
		"justice tx wasn't mined")

	// Ensure that Willy doesn't get any funds, as he is acting as an
	// altruist watchtower.
	err = wait.NoError(func() error {
		willyBalResp := willy.WalletBalance()

		if willyBalResp.ConfirmedBalance != 0 {
			return fmt.Errorf("expected Willy to have no funds "+
				"after justice transaction was mined, found %v",
				willyBalResp)
		}

		return nil
	}, time.Second*5)
	require.NoError(ht, err, "timeout checking willy's balance")

	// Before restarting Dave, shutdown Carol so Dave won't sync with her.
	// Otherwise, during the restart, Dave will realize Carol is falling
	// behind and return `ErrCommitSyncRemoteDataLoss`, thus force closing
	// the channel. Although this force close tx will be later replaced by
	// the breach tx, it will create two anchor sweeping txes for neutrino
	// backend, causing the confirmed wallet balance to be zero later on
	// because the utxos are used in sweeping.
	ht.Shutdown(carol)

	// Restart Dave, who will still think his channel with Carol is open.
	// We should him to detect the breach, but realize that the funds have
	// then been swept to his wallet by Willy.
	require.NoError(ht, restart(), "unable to restart dave")

	// For neutrino backend, we may need to mine one more block to trigger
	// the chain watcher to act.
	//
	// TODO(yy): remove it once the blockbeat remembers the last block
	// processed.
	if ht.IsNeutrinoBackend() {
		ht.MineEmptyBlocks(1)
	}

	err = wait.NoError(func() error {
		daveBalResp := dave.RPC.ChannelBalance()
		if daveBalResp.LocalBalance.Sat != 0 {
			return fmt.Errorf("Dave should end up with zero "+
				"channel balance, instead has %d",
				daveBalResp.LocalBalance.Sat)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking dave's channel balance")

	ht.AssertNumPendingForceClose(dave, 0)

	// Check that Dave's wallet balance is increased.
	err = wait.NoError(func() error {
		daveBalResp := dave.RPC.WalletBalance()

		if daveBalResp.ConfirmedBalance <= davePreSweepBalance {
			return fmt.Errorf("Dave should have more than %d "+
				"after sweep, instead has %d",
				davePreSweepBalance,
				daveBalResp.ConfirmedBalance)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking dave's wallet balance")

	// Dave should have no open channels.
	ht.AssertNodeNumChannels(dave, 0)
}

func setUpNewTower(ht *lntest.HarnessTest, name, externalIP string) ([]byte,
	string, *rpc.HarnessRPC) {

	port := port.NextAvailablePort()

	listenAddr := fmt.Sprintf("0.0.0.0:%d", port)

	// Set up the new watchtower.
	tower := ht.NewNode(name, []string{
		"--watchtower.active",
		"--watchtower.externalip=" + externalIP,
		"--watchtower.listen=" + listenAddr,
	})

	towerInfo := tower.RPC.GetInfoWatchtower()

	require.Len(ht, towerInfo.Listeners, 1)
	listener := towerInfo.Listeners[0]
	require.True(
		ht, listener == listenAddr ||
			listener == fmt.Sprintf("[::]:%d", port),
	)

	// Assert the Tower's URIs properly display the chosen external IP.
	require.Len(ht, towerInfo.Uris, 1)
	require.Contains(ht, towerInfo.Uris[0], externalIP)

	return towerInfo.Pubkey, towerInfo.Listeners[0], tower.RPC
}

// generateBackups is a helper function that can be used to create a number of
// watchtower back-ups.
func generateBackups(ht *lntest.HarnessTest, srcNode,
	dstNode *node.HarnessNode, numBackups int64) {

	const paymentAmt = 10_000

	require.EqualValuesf(ht, numBackups%2, 0, "the number of desired "+
		"back-ups must be even")

	// Two updates are made for every payment.
	numPayments := int(numBackups / 2)

	// Create the required number of invoices.
	alicePayReqs, _, _ := ht.CreatePayReqs(
		dstNode, paymentAmt, numPayments,
	)

	send := func(node *node.HarnessNode, payReq string) {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		ht.SendPaymentAssertSettled(node, req)
	}

	// Pay each invoice.
	for i := 0; i < numPayments; i++ {
		send(srcNode, alicePayReqs[i])
	}
}

// assertNumBackups is a helper that asserts that the given node has a certain
// number of backups backed up to the tower. If mineOnFail is true, then a block
// will be mined each time the assertion fails.
func assertNumBackups(ht *lntest.HarnessTest, node *rpc.HarnessRPC,
	towerPk []byte, expectedNumBackups int, mineOnFail bool) {

	err := wait.NoError(func() error {
		info := node.GetTowerInfo(
			&wtclientrpc.GetTowerInfoRequest{
				Pubkey:          towerPk,
				IncludeSessions: true,
			},
		)

		var numBackups uint32
		for _, sessionType := range info.SessionInfo {
			for _, session := range sessionType.Sessions {
				numBackups += session.NumBackups
			}
		}

		if numBackups == uint32(expectedNumBackups) {
			return nil
		}

		if mineOnFail {
			ht.MineBlocks(1)
		}

		return fmt.Errorf("expected %d backups, got %d",
			expectedNumBackups, numBackups)
	}, defaultTimeout)
	require.NoError(ht, err)
}
