package itest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// channelRestoreTestCases contains the test cases for the channel restore
// scenario.
var channelRestoreTestCases = []*lntest.TestCase{
	{
		// Restore the backup from the on-disk file, using the RPC
		// interface, for anchor commitment channels.
		Name: "restore anchor",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, lnrpc.CommitmentType_ANCHORS, false,
			)
		},
	},
	{
		// Restore the backup from the on-disk file, using the RPC
		// interface, for script-enforced leased channels.
		Name: "restore leased",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, leasedType, false,
			)
		},
	},
	{
		// Restore the backup from the on-disk file, using the RPC
		// interface, for zero-conf anchor channels.
		Name: "restore anchor zero conf",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, lnrpc.CommitmentType_ANCHORS, true,
			)
		},
	},
	{
		// Restore the backup from the on-disk file, using the RPC
		// interface for a zero-conf script-enforced leased channel.
		Name: "restore leased zero conf",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, leasedType, true,
			)
		},
	},
	{
		// Restore a channel back up of a taproot channel that was
		// confirmed.
		Name: "restore simple taproot",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, lnrpc.CommitmentType_SIMPLE_TAPROOT, false,
			)
		},
	},
	{
		// Restore a channel back up of an unconfirmed taproot channel.
		Name: "restore simple taproot zero conf",
		TestFunc: func(ht *lntest.HarnessTest) {
			runChanRestoreScenarioCommitTypes(
				ht, lnrpc.CommitmentType_SIMPLE_TAPROOT, true,
			)
		},
	},
	{
		Name:     "restore from rpc",
		TestFunc: testChannelBackupRestoreFromRPC,
	},
	{
		Name:     "restore from file",
		TestFunc: testChannelBackupRestoreFromFile,
	},
	{
		Name:     "restore during creation",
		TestFunc: testChannelBackupRestoreDuringCreation,
	},
	{
		Name:     "restore during unlock",
		TestFunc: testChannelBackupRestoreDuringUnlock,
	},
	{
		Name:     "restore twice",
		TestFunc: testChannelBackupRestoreTwice,
	},
}

type (
	// nodeRestorer is a function closure that allows each test case to
	// control exactly *how* the prior node is restored. This might be
	// using an backup obtained over RPC, or the file system, etc.
	nodeRestorer func() *node.HarnessNode

	// restoreMethod takes an old node, then returns a function closure
	// that'll return the same node, but with its state restored via a
	// custom method. We use this to abstract away _how_ a node is restored
	// from our assertions once the node has been fully restored itself.
	restoreMethodType func(ht *lntest.HarnessTest,
		oldNode *node.HarnessNode, backupFilePath string,
		password []byte, mnemonic []string) nodeRestorer
)

// revocationWindow is used when we specify the revocation window used when
// restoring node.
const revocationWindow = 100

// chanRestoreScenario represents a test case used by testing the channel
// restore methods.
type chanRestoreScenario struct {
	carol    *node.HarnessNode
	dave     *node.HarnessNode
	password []byte
	mnemonic []string
	params   lntest.OpenChannelParams
}

// newChanRestoreScenario creates a new scenario that has two nodes, Carol and
// Dave, connected and funded.
func newChanRestoreScenario(ht *lntest.HarnessTest, ct lnrpc.CommitmentType,
	zeroConf bool) *chanRestoreScenario {

	const (
		chanAmt = btcutil.Amount(10000000)
		pushAmt = btcutil.Amount(5000000)
	)

	password := []byte("El Psy Kongroo")
	nodeArgs := []string{
		"--minbackoff=50ms",
		"--maxbackoff=1s",
	}

	if ct != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		args := lntest.NodeArgsForCommitType(ct)
		nodeArgs = append(nodeArgs, args...)
	}

	if zeroConf {
		nodeArgs = append(
			nodeArgs, "--protocol.option-scid-alias",
			"--protocol.zero-conf",
		)
	}

	// First, we'll create a brand new node we'll use within the test. If
	// we have a custom backup file specified, then we'll also create that
	// for use.
	dave, mnemonic, _ := ht.NewNodeWithSeed(
		"dave", nodeArgs, password, false,
	)
	carol := ht.NewNode("carol", nodeArgs)

	// Now that our new nodes are created, we'll give them some coins for
	// channel opening and anchor sweeping.
	ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, carol)
	ht.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, dave)

	// Mine a block to confirm the funds.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// For the anchor output case we need two UTXOs for Carol so she can
	// sweep both the local and remote anchor.
	if lntest.CommitTypeHasAnchors(ct) {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// Next, we'll connect Dave to Carol, and open a new channel to her
	// with a portion pushed.
	ht.ConnectNodes(dave, carol)

	// If the commitment type is taproot, then the channel must also be
	// private.
	var privateChan bool
	if ct == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		privateChan = true
	}

	return &chanRestoreScenario{
		carol:    carol,
		dave:     dave,
		mnemonic: mnemonic,
		password: password,
		params: lntest.OpenChannelParams{
			Amt:            chanAmt,
			PushAmt:        pushAmt,
			ZeroConf:       zeroConf,
			CommitmentType: ct,
			Private:        privateChan,
		},
	}
}

// restoreDave will call the `nodeRestorer` and asserts Dave is restored by
// checking his wallet balance against zero.
func (c *chanRestoreScenario) restoreDave(ht *lntest.HarnessTest,
	restoredNodeFunc nodeRestorer) *node.HarnessNode {

	// Next, we'll make a new Dave and start the bulk of our recovery
	// workflow.
	dave := restoredNodeFunc()

	// First ensure that the on-chain balance is restored.
	err := wait.NoError(func() error {
		daveBalResp := dave.RPC.WalletBalance()
		daveBal := daveBalResp.ConfirmedBalance
		if daveBal <= 0 {
			return fmt.Errorf("expected positive balance, had %v",
				daveBal)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "On-chain balance not restored")

	return dave
}

// testScenario runs a test case with a given setup and asserts the DLP is
// executed as expected, in details, it will,
//  1. shutdown Dave.
//  2. suspend Carol.
//  3. restore Dave.
//  4. validate pending channel state and check we cannot force close it.
//  5. validate Carol's UTXOs.
//  6. assert DLP is executed.
func (c *chanRestoreScenario) testScenario(ht *lntest.HarnessTest,
	restoredNodeFunc nodeRestorer) {

	carol, dave := c.carol, c.dave

	// Before we start the recovery, we'll record the balances of both
	// Carol and Dave to ensure they both sweep their coins at the end.
	carolBalResp := carol.RPC.WalletBalance()
	carolStartingBalance := carolBalResp.ConfirmedBalance

	daveBalance := dave.RPC.WalletBalance()
	daveStartingBalance := daveBalance.ConfirmedBalance

	// Now that we're able to make our restored now, we'll shutdown the old
	// Dave node as we'll be storing it shortly below.
	ht.Shutdown(dave)

	// To make sure the channel state is advanced correctly if the channel
	// peer is not online at first, we also shutdown Carol.
	restartCarol := ht.SuspendNode(carol)

	// We now restore Dave.
	dave = c.restoreDave(ht, restoredNodeFunc)

	// We now check that the restored channel is in the proper state. It
	// should not yet be force closing as no connection with the remote
	// peer was established yet. We should also not be able to close the
	// channel.
	channel := ht.AssertNumWaitingClose(dave, 1)[0]
	chanPointStr := channel.Channel.ChannelPoint

	// We also want to make sure we cannot force close in this state. That
	// would get the state machine in a weird state.
	chanPointParts := strings.Split(chanPointStr, ":")
	chanPointIndex, _ := strconv.ParseUint(chanPointParts[1], 10, 32)

	// We don't get an error directly but only when reading the first
	// message of the stream.
	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
				FundingTxidStr: chanPointParts[0],
			},
			OutputIndex: uint32(chanPointIndex),
		},
		Force: true,
	}
	err := ht.CloseChannelAssertErr(dave, req)
	require.Contains(ht, err.Error(), "cannot close channel with state: ")
	require.Contains(ht, err.Error(), "ChanStatusRestored")

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed in case of anchor commitments.
	ht.SetFeeEstimate(30000)

	// Now that we have ensured that the channels restored by the backup
	// are in the correct state even without the remote peer telling us so,
	// let's start up Carol again.
	require.NoError(ht, restartCarol(), "restart carol failed")

	if lntest.CommitTypeHasAnchors(c.params.CommitmentType) {
		ht.AssertNumUTXOs(carol, 2)
	} else {
		ht.AssertNumUTXOs(carol, 1)
	}

	// Now we'll assert that both sides properly execute the DLP protocol.
	// We grab their balances now to ensure that they're made whole at the
	// end of the protocol.
	assertDLPExecuted(
		ht, carol, carolStartingBalance, dave,
		daveStartingBalance, c.params.CommitmentType,
	)
}

// testChannelBackupRestoreFromRPC tests that we're able to recover from, and
// initiate the DLP protocol via the RPC restore command.
func testChannelBackupRestoreFromRPC(ht *lntest.HarnessTest) {
	// Restore from backups obtained via the RPC interface. Dave was the
	// initiator, of the non-advertised channel.
	restoreMethod := func(st *lntest.HarnessTest, oldNode *node.HarnessNode,
		backupFilePath string, password []byte,
		mnemonic []string) nodeRestorer {

		// For this restoration method, we'll grab the current
		// multi-channel backup from the old node, and use it to
		// restore a new node within the closure.
		chanBackup := oldNode.RPC.ExportAllChanBackups()

		multi := chanBackup.MultiChanBackup.
			MultiChanBackup

		// In our nodeRestorer function, we'll restore the node from
		// seed, then manually recover the channel backup.
		return chanRestoreViaRPC(
			st, password, mnemonic, multi,
		)
	}

	runChanRestoreScenarioBasic(ht, restoreMethod)
}

// testChannelBackupRestoreFromFile tests that we're able to recover from, and
// initiate the DLP protocol via the backup file.
func testChannelBackupRestoreFromFile(ht *lntest.HarnessTest) {
	// Restore the backup from the on-disk file, using the RPC interface.
	restoreMethod := func(st *lntest.HarnessTest, oldNode *node.HarnessNode,
		backupFilePath string, password []byte,
		mnemonic []string) nodeRestorer {

		// Read the entire Multi backup stored within this node's
		// channel.backup file.
		multi, err := os.ReadFile(backupFilePath)
		require.NoError(st, err)

		// Now that we have Dave's backup file, we'll create a new
		// nodeRestorer that will restore using the on-disk
		// channel.backup.
		return chanRestoreViaRPC(
			st, password, mnemonic, multi,
		)
	}

	runChanRestoreScenarioBasic(ht, restoreMethod)
}

// testChannelBackupRestoreFromFile tests that we're able to recover from, and
// initiate the DLP protocol via restoring from initial wallet creation.
func testChannelBackupRestoreDuringCreation(ht *lntest.HarnessTest) {
	// Restore the backup as part of node initialization with the prior
	// mnemonic and new backup seed.
	restoreMethod := func(st *lntest.HarnessTest, oldNode *node.HarnessNode,
		backupFilePath string, password []byte,
		mnemonic []string) nodeRestorer {

		// First, fetch the current backup state as is, to obtain our
		// latest Multi.
		chanBackup := oldNode.RPC.ExportAllChanBackups()
		backupSnapshot := &lnrpc.ChanBackupSnapshot{
			MultiChanBackup: chanBackup.
				MultiChanBackup,
		}

		// Create a new nodeRestorer that will restore the node using
		// the Multi backup we just obtained above.
		return func() *node.HarnessNode {
			return st.RestoreNodeWithSeed(
				"dave", nil, password, mnemonic,
				"", revocationWindow,
				backupSnapshot,
			)
		}
	}

	runChanRestoreScenarioBasic(ht, restoreMethod)
}

// testChannelBackupRestoreFromFile tests that we're able to recover from, and
// initiate the DLP protocol via restoring on unlock.
func testChannelBackupRestoreDuringUnlock(ht *lntest.HarnessTest) {
	// Restore the backup once the node has already been re-created, using
	// the Unlock call.
	restoreMethod := func(st *lntest.HarnessTest, oldNode *node.HarnessNode,
		backupFilePath string, password []byte,
		mnemonic []string) nodeRestorer {

		// First, fetch the current backup state as is, to obtain our
		// latest Multi.
		chanBackup := oldNode.RPC.ExportAllChanBackups()
		backupSnapshot := &lnrpc.ChanBackupSnapshot{
			MultiChanBackup: chanBackup.
				MultiChanBackup,
		}

		// Create a new nodeRestorer that will restore the node with
		// its seed, but no channel backup, shutdown this initialized
		// node, then restart it again using Unlock.
		return func() *node.HarnessNode {
			newNode := st.RestoreNodeWithSeed(
				"dave", nil, password, mnemonic,
				"", revocationWindow, nil,
			)
			st.RestartNodeWithChanBackups(
				newNode, backupSnapshot,
			)

			return newNode
		}
	}

	runChanRestoreScenarioBasic(ht, restoreMethod)
}

// testChannelBackupRestoreTwice tests that we're able to recover from, and
// initiate the DLP protocol twice by alternating between restoring form the on
// disk file, and restoring from the exported RPC command.
func testChannelBackupRestoreTwice(ht *lntest.HarnessTest) {
	// Restore the backup from the on-disk file a second time to make sure
	// imports can be canceled and later resumed.
	restoreMethod := func(st *lntest.HarnessTest, oldNode *node.HarnessNode,
		backupFilePath string, password []byte,
		mnemonic []string) nodeRestorer {

		// Read the entire Multi backup stored within this node's
		// channel.backup file.
		multi, err := os.ReadFile(backupFilePath)
		require.NoError(st, err)

		// Now that we have Dave's backup file, we'll create a new
		// nodeRestorer that will restore using the on-disk
		// channel.backup.
		backup := &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
			MultiChanBackup: multi,
		}

		return func() *node.HarnessNode {
			newNode := st.RestoreNodeWithSeed(
				"dave", nil, password, mnemonic,
				"", revocationWindow, nil,
			)

			req := &lnrpc.RestoreChanBackupRequest{
				Backup: backup,
			}
			newNode.RPC.RestoreChanBackups(req)

			req = &lnrpc.RestoreChanBackupRequest{
				Backup: backup,
			}
			newNode.RPC.RestoreChanBackups(req)

			return newNode
		}
	}

	runChanRestoreScenarioBasic(ht, restoreMethod)
}

// runChanRestoreScenarioBasic executes a given test case from end to end,
// ensuring that after Dave restores his channel state according to the
// testCase, the DLP protocol is executed properly and both nodes are made
// whole.
func runChanRestoreScenarioBasic(ht *lntest.HarnessTest,
	restoreMethod restoreMethodType) {

	// Create a new restore scenario.
	crs := newChanRestoreScenario(
		ht, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE, false,
	)
	carol, dave := crs.carol, crs.dave

	// Open a channel from Dave to Carol.
	ht.OpenChannel(dave, carol, crs.params)

	// At this point, we'll now execute the restore method to give us the
	// new node we should attempt our assertions against.
	backupFilePath := dave.Cfg.ChanBackupPath()
	restoredNodeFunc := restoreMethod(
		ht, dave, backupFilePath, crs.password, crs.mnemonic,
	)

	// Test the scenario.
	crs.testScenario(ht, restoredNodeFunc)
}

// testChannelBackupRestoreUnconfirmed tests that we're able to restore from
// disk file and the exported RPC command for unconfirmed channel.
func testChannelBackupRestoreUnconfirmed(ht *lntest.HarnessTest) {
	// Use the channel backup file that contains an unconfirmed channel and
	// make sure recovery works as well.
	ht.Run("restore unconfirmed channel file", func(t *testing.T) {
		st := ht.Subtest(t)
		runChanRestoreScenarioUnConfirmed(st, true)
	})

	// Create a backup using RPC that contains an unconfirmed channel and
	// make sure recovery works as well.
	ht.Run("restore unconfirmed channel RPC", func(t *testing.T) {
		st := ht.Subtest(t)
		runChanRestoreScenarioUnConfirmed(st, false)
	})
}

// runChanRestoreScenarioUnConfirmed checks that Dave is able to restore for an
// unconfirmed channel.
func runChanRestoreScenarioUnConfirmed(ht *lntest.HarnessTest, useFile bool) {
	// Create a new restore scenario.
	crs := newChanRestoreScenario(
		ht, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE, false,
	)
	carol, dave := crs.carol, crs.dave

	// Open a pending channel.
	ht.OpenChannelAssertPending(dave, carol, crs.params)

	// Give the pubsub some time to update the channel backup.
	err := wait.NoError(func() error {
		fi, err := os.Stat(dave.Cfg.ChanBackupPath())
		if err != nil {
			return err
		}
		if fi.Size() <= chanbackup.NilMultiSizePacked {
			return fmt.Errorf("backup file empty")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "channel backup not updated in time")

	// At this point, we'll now execute the restore method to give us the
	// new node we should attempt our assertions against.
	var multi []byte
	if useFile {
		backupFilePath := dave.Cfg.ChanBackupPath()
		// Read the entire Multi backup stored within this node's
		// channel.backup file.
		multi, err = os.ReadFile(backupFilePath)
		require.NoError(ht, err)
	} else {
		// For this restoration method, we'll grab the current
		// multi-channel backup from the old node. The channel should
		// be included, even if it is not confirmed yet.
		chanBackup := dave.RPC.ExportAllChanBackups()
		chanPoints := chanBackup.MultiChanBackup.ChanPoints
		require.NotEmpty(ht, chanPoints,
			"unconfirmed channel not found")
		multi = chanBackup.MultiChanBackup.MultiChanBackup
	}

	// Let's assume time passes, the channel confirms in the meantime but
	// for some reason the backup we made while it was still unconfirmed is
	// the only backup we have. We should still be able to restore it. To
	// simulate time passing, we mine some blocks to get the channel
	// confirmed _after_ we saved the backup.
	ht.MineBlocksAndAssertNumTxes(6, 1)

	// In our nodeRestorer function, we'll restore the node from seed, then
	// manually recover the channel backup.
	restoredNodeFunc := chanRestoreViaRPC(
		ht, crs.password, crs.mnemonic, multi,
	)

	// Test the scenario.
	crs.testScenario(ht, restoredNodeFunc)
}

// runChanRestoreScenarioCommitTypes tests that the DLP is applied for
// different channel commitment types and zero-conf channel.
func runChanRestoreScenarioCommitTypes(ht *lntest.HarnessTest,
	ct lnrpc.CommitmentType, zeroConf bool) {

	// Create a new restore scenario.
	crs := newChanRestoreScenario(ht, ct, zeroConf)
	carol, dave := crs.carol, crs.dave

	// If we are testing zero-conf channels, setup a ChannelAcceptor for
	// the fundee.
	var cancelAcceptor context.CancelFunc
	if zeroConf {
		// Setup a ChannelAcceptor.
		acceptStream, cancel := carol.RPC.ChannelAcceptor()
		cancelAcceptor = cancel
		go acceptChannel(ht.T, true, acceptStream)
	}

	var fundingShim *lnrpc.FundingShim
	if ct == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		minerHeight := ht.CurrentHeight()
		thawHeight := minerHeight + thawHeightDelta

		fundingShim, _ = ht.DeriveFundingShim(
			dave, carol, crs.params.Amt, thawHeight, true, ct,
		)
		crs.params.FundingShim = fundingShim
	}
	ht.OpenChannel(dave, carol, crs.params)

	// Remove the ChannelAcceptor.
	if zeroConf {
		cancelAcceptor()
	}

	// At this point, we'll now execute the restore method to give us the
	// new node we should attempt our assertions against.
	backupFilePath := dave.Cfg.ChanBackupPath()

	// Read the entire Multi backup stored within this node's
	// channels.backup file.
	multi, err := os.ReadFile(backupFilePath)
	require.NoError(ht, err)

	// If this was a zero conf taproot channel, then since it's private,
	// we'll need to mine an extra block (framework won't mine extra blocks
	// otherwise).
	if ct == lnrpc.CommitmentType_SIMPLE_TAPROOT && zeroConf {
		ht.MineBlocksAndAssertNumTxes(1, 1)
	}

	// Now that we have Dave's backup file, we'll create a new nodeRestorer
	// that we'll restore using the on-disk channels.backup.
	restoredNodeFunc := chanRestoreViaRPC(
		ht, crs.password, crs.mnemonic, multi,
	)

	// Test the scenario.
	crs.testScenario(ht, restoredNodeFunc)
}

// testChannelBackupRestoreLegacy checks a channel with the legacy revocation
// producer format and makes sure old SCBs can still be recovered.
func testChannelBackupRestoreLegacy(ht *lntest.HarnessTest) {
	// Create a new restore scenario.
	crs := newChanRestoreScenario(
		ht, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE, false,
	)
	carol, dave := crs.carol, crs.dave

	createLegacyRevocationChannel(
		ht, crs.params.Amt, crs.params.PushAmt, dave, carol,
	)

	// For this restoration method, we'll grab the current multi-channel
	// backup from the old node, and use it to restore a new node within
	// the closure.
	chanBackup := dave.RPC.ExportAllChanBackups()
	multi := chanBackup.MultiChanBackup.MultiChanBackup

	// In our nodeRestorer function, we'll restore the node from seed, then
	// manually recover the channel backup.
	restoredNodeFunc := chanRestoreViaRPC(
		ht, crs.password, crs.mnemonic, multi,
	)

	// Test the scenario.
	crs.testScenario(ht, restoredNodeFunc)
}

// testChannelBackupRestoreForceClose checks that Dave can restore from force
// closed channels.
func testChannelBackupRestoreForceClose(ht *lntest.HarnessTest) {
	// Restore a channel that was force closed by dave just before going
	// offline.
	success := ht.Run("from backup file anchors", func(t *testing.T) {
		st := ht.Subtest(t)
		runChanRestoreScenarioForceClose(st, false)
	})

	// Only run the second test if the first passed.
	if !success {
		return
	}

	// Restore a zero-conf anchors channel that was force closed by dave
	// just before going offline.
	ht.Run("from backup file anchors w/ zero-conf", func(t *testing.T) {
		st := ht.Subtest(t)
		runChanRestoreScenarioForceClose(st, true)
	})
}

// runChanRestoreScenarioForceClose creates anchor-enabled force close channels
// and checks that Dave is able to restore from them.
func runChanRestoreScenarioForceClose(ht *lntest.HarnessTest, zeroConf bool) {
	crs := newChanRestoreScenario(
		ht, lnrpc.CommitmentType_ANCHORS, zeroConf,
	)
	carol, dave := crs.carol, crs.dave

	// For neutrino backend, we give Dave once more UTXO to fund the anchor
	// sweep.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)
	}

	// If we are testing zero-conf channels, setup a ChannelAcceptor for
	// the fundee.
	var cancelAcceptor context.CancelFunc
	if zeroConf {
		// Setup a ChannelAcceptor.
		acceptStream, cancel := carol.RPC.ChannelAcceptor()
		cancelAcceptor = cancel
		go acceptChannel(ht.T, true, acceptStream)
	}

	chanPoint := ht.OpenChannel(dave, carol, crs.params)

	// Remove the ChannelAcceptor.
	if zeroConf {
		cancelAcceptor()
	}

	// If we're testing that locally force closed channels can be restored
	// then we issue the force close now.
	ht.CloseChannelAssertPending(dave, chanPoint, true)

	// Dave should see one waiting close channel.
	ht.AssertNumWaitingClose(dave, 1)

	// Now we need to make sure that the channel is still in the backup.
	// Otherwise restoring won't work later.
	dave.RPC.ExportChanBackup(chanPoint)

	// Before we start the recovery, we'll record the balances of both
	// Carol and Dave to ensure they both sweep their coins at the end.
	carolBalResp := carol.RPC.WalletBalance()
	carolStartingBalance := carolBalResp.ConfirmedBalance

	daveBalance := dave.RPC.WalletBalance()
	daveStartingBalance := daveBalance.ConfirmedBalance

	// At this point, we'll now execute the restore method to give us the
	// new node we should attempt our assertions against.
	backupFilePath := dave.Cfg.ChanBackupPath()

	// Read the entire Multi backup stored within this node's
	// channel.backup file.
	multi, err := os.ReadFile(backupFilePath)
	require.NoError(ht, err)

	// Now that we have Dave's backup file, we'll create a new nodeRestorer
	// that will restore using the on-disk channel.backup.
	restoredNodeFunc := chanRestoreViaRPC(
		ht, crs.password, crs.mnemonic, multi,
	)

	// We now wait until both Dave's closing tx.
	ht.AssertNumTxsInMempool(1)

	// Now that we're able to make our restored now, we'll shutdown the old
	// Dave node as we'll be storing it shortly below. Use SuspendNode, not
	// Shutdown to keep its directory including channel.backup file.
	ht.SuspendNode(dave)

	// Read Dave's channel.backup file again to make sure it was updated
	// upon Dave's shutdown. In case LND state is lost and DLP protocol
	// fails, the channel.backup file and the commit tx in it are the
	// measure of last resort to recover funds from the channel. The file
	// is updated upon LND server shutdown to update the commit tx just in
	// case it is used this way. If an outdated commit tx is broadcasted,
	// the funds may be lost in a justice transaction. The file is encrypted
	// and we can't decrypt it here, so we just check that the content of
	// the file has changed.
	multi2, err := os.ReadFile(backupFilePath)
	require.NoError(ht, err)
	require.NotEqual(ht, multi, multi2)

	// Mine a block to confirm the closing tx from Dave.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// To make sure the channel state is advanced correctly if the channel
	// peer is not online at first, we also shutdown Carol.
	restartCarol := ht.SuspendNode(carol)

	dave = crs.restoreDave(ht, restoredNodeFunc)

	// For our force close scenario we don't need the channel to be closed
	// by Carol since it was already force closed before we started the
	// recovery. All we need is for Carol to send us over the commit height
	// so we can sweep the time locked output with the correct commit
	// point.
	ht.AssertNumPendingForceClose(dave, 1)

	require.NoError(ht, restartCarol(), "restart carol failed")

	// Now that we have our new node up, we expect that it'll re-connect to
	// Carol automatically based on the restored backup.
	ht.EnsureConnected(dave, carol)

	assertTimeLockSwept(
		ht, carol, dave, carolStartingBalance, daveStartingBalance,
	)
}

// testChannelBackupUpdates tests that both the streaming channel update RPC,
// and the on-disk channel.backup are updated each time a channel is
// opened/closed.
func testChannelBackupUpdates(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)

	// First, we'll make a temp directory that we'll use to store our
	// backup file, so we can check in on it during the test easily.
	backupDir := ht.T.TempDir()

	// First, we'll create a new node, Carol. We'll also create a temporary
	// file that Carol will use to store her channel backups.
	backupFilePath := filepath.Join(
		backupDir, chanbackup.DefaultBackupFileName,
	)
	carolArgs := fmt.Sprintf("--backupfilepath=%v", backupFilePath)
	carol := ht.NewNode("carol", []string{carolArgs})

	// Next, we'll register for streaming notifications for changes to the
	// backup file.
	backupStream := carol.RPC.SubscribeChannelBackups()

	// We'll use this goroutine to proxy any updates to a channel we can
	// easily use below.
	var wg sync.WaitGroup
	backupUpdates := make(chan *lnrpc.ChanBackupSnapshot)
	streamErr := make(chan error)
	streamQuit := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			snapshot, err := backupStream.Recv()
			if err != nil {
				select {
				case streamErr <- err:
				case <-streamQuit:
					return
				}
			}

			select {
			case backupUpdates <- snapshot:
			case <-streamQuit:
				return
			}
		}
	}()
	defer close(streamQuit)

	// With Carol up, we'll now connect her to Alice, and open a channel
	// between them.
	ht.ConnectNodes(carol, alice)

	// Next, we'll open two channels between Alice and Carol back to back.
	var chanPoints []*lnrpc.ChannelPoint
	numChans := 2
	chanAmt := btcutil.Amount(1000000)
	for i := 0; i < numChans; i++ {
		chanPoint := ht.OpenChannel(
			alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
		)
		chanPoints = append(chanPoints, chanPoint)
	}

	// Using this helper function, we'll maintain a pointer to the latest
	// channel backup so we can compare it to the on disk state.
	var currentBackup *lnrpc.ChanBackupSnapshot
	assertBackupNtfns := func(numNtfns int) {
		for i := 0; i < numNtfns; i++ {
			select {
			case err := <-streamErr:
				require.Failf(ht, "stream err",
					"error with backup stream: %v", err)

			case currentBackup = <-backupUpdates:

			case <-time.After(time.Second * 5):
				require.Failf(ht, "timeout", "didn't "+
					"receive channel backup "+
					"notification %v", i+1)
			}
		}
	}

	containsChan := func(b *lnrpc.VerifyChanBackupResponse,
		chanPoint *lnrpc.ChannelPoint) bool {

		hash, err := lnrpc.GetChanPointFundingTxid(chanPoint)
		require.NoError(ht, err)

		chanPointStr := fmt.Sprintf("%s:%d", hash.String(),
			chanPoint.OutputIndex)

		for idx := range b.ChanPoints {
			if b.ChanPoints[idx] == chanPointStr {
				return true
			}
		}

		return false
	}

	// assertBackupFileState is a helper function that we'll use to compare
	// the on disk back up file to our currentBackup pointer above.
	assertBackupFileState := func(expectAllChannels bool) {
		err := wait.NoError(func() error {
			packedBackup, err := os.ReadFile(backupFilePath)
			if err != nil {
				return fmt.Errorf("unable to read backup "+
					"file: %v", err)
			}

			// As each back up file will be encrypted with a fresh
			// nonce, we can't compare them directly, so instead
			// we'll compare the length which is a proxy for the
			// number of channels that the multi-backup contains.
			backup := currentBackup.MultiChanBackup.MultiChanBackup
			if len(backup) != len(packedBackup) {
				return fmt.Errorf("backup files don't match: "+
					"expected %x got %x", backup,
					packedBackup)
			}

			// Additionally, we'll assert that both backups up
			// returned are valid.
			for _, backup := range [][]byte{backup, packedBackup} {
				snapshot := &lnrpc.ChanBackupSnapshot{
					MultiChanBackup: &lnrpc.MultiChanBackup{
						MultiChanBackup: backup,
					},
				}

				res := carol.RPC.VerifyChanBackup(snapshot)

				if !expectAllChannels {
					continue
				}
				for idx := range chanPoints {
					if containsChan(res, chanPoints[idx]) {
						continue
					}

					return fmt.Errorf("backup %v doesn't "+
						"contain chan_point: %v",
						res.ChanPoints, chanPoints[idx])
				}
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout while checking "+
			"backup state: %v", err)
	}

	// As these two channels were just opened, we should've got two times
	// the pending and open notifications for channel backups.
	assertBackupNtfns(2 * 2)

	// The on disk file should also exactly match the latest backup that we
	// have.
	assertBackupFileState(true)

	// Next, we'll close the channels one by one. After each channel
	// closure, we should get a notification, and the on-disk state should
	// match this state as well.
	for i := 0; i < numChans; i++ {
		// To ensure force closes also trigger an update, we'll force
		// close half of the channels.
		forceClose := i%2 == 0

		chanPoint := chanPoints[i]

		// If we force closed the channel, then we'll mine enough
		// blocks to ensure all outputs have been swept.
		if forceClose {
			ht.ForceCloseChannel(alice, chanPoint)

			// A local force closed channel will trigger a
			// notification once the commitment TX confirms on
			// chain. But that won't remove the channel from the
			// backup just yet, that will only happen once the time
			// locked contract was fully resolved on chain.
			assertBackupNtfns(1)

			// Now that the channel's been fully resolved, we
			// expect another notification.
			assertBackupNtfns(1)
			assertBackupFileState(false)
		} else {
			ht.CloseChannel(alice, chanPoint)
			// We should get a single notification after closing,
			// and the on-disk state should match this latest
			// notifications.
			assertBackupNtfns(1)
			assertBackupFileState(false)
		}
	}
}

// testExportChannelBackup tests that we're able to properly export either a
// targeted channel's backup, or export backups of all the currents open
// channels.
func testExportChannelBackup(ht *lntest.HarnessTest) {
	// First, we'll create our primary test node: Carol. We'll use Carol to
	// open channels and also export backups that we'll examine throughout
	// the test.
	carol := ht.NewNode("carol", nil)

	// With Carol up, we'll now connect her to Alice, and open a channel
	// between them.
	alice := ht.NewNodeWithCoins("Alice", nil)
	ht.ConnectNodes(carol, alice)

	// Next, we'll open two channels between Alice and Carol back to back.
	var chanPoints []*lnrpc.ChannelPoint
	numChans := 2
	chanAmt := btcutil.Amount(1000000)
	for i := 0; i < numChans; i++ {
		chanPoint := ht.OpenChannel(
			alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
		)
		chanPoints = append(chanPoints, chanPoint)
	}

	// Now that the channels are open, we should be able to fetch the
	// backups of each of the channels.
	for _, chanPoint := range chanPoints {
		chanBackup := carol.RPC.ExportChanBackup(chanPoint)

		// The returned backup should be full populated. Since it's
		// encrypted, we can't assert any more than that atm.
		require.NotEmptyf(ht, chanBackup.ChanBackup,
			"obtained empty backup for channel: %v", chanPoint)

		// The specified chanPoint in the response should match our
		// requested chanPoint.
		require.Equal(ht, chanBackup.ChanPoint.String(),
			chanPoint.String())
	}

	// Before we proceed, we'll make two utility methods we'll use below
	// for our primary assertions.
	assertNumSingleBackups := func(numSingles int) {
		err := wait.NoError(func() error {
			chanSnapshot := carol.RPC.ExportAllChanBackups()

			if chanSnapshot.SingleChanBackups == nil {
				return fmt.Errorf("single chan backups not " +
					"populated")
			}

			backups := chanSnapshot.SingleChanBackups.ChanBackups
			if len(backups) != numSingles {
				return fmt.Errorf("expected %v singles, "+
					"got %v", len(backups), numSingles)
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout checking num single backup")
	}

	assertMultiBackupFound := func() func(bool,
		map[wire.OutPoint]struct{}) {

		chanSnapshot := carol.RPC.ExportAllChanBackups()

		return func(found bool, chanPoints map[wire.OutPoint]struct{}) {
			num := len(chanSnapshot.MultiChanBackup.MultiChanBackup)

			switch {
			case found && chanSnapshot.MultiChanBackup == nil:
				require.Fail(ht, "multi-backup not present")

			case !found && chanSnapshot.MultiChanBackup != nil &&
				num != chanbackup.NilMultiSizePacked:

				require.Fail(ht, "found multi-backup when "+
					"non should be found")
			}

			if !found {
				return
			}

			backedUpChans := chanSnapshot.MultiChanBackup.ChanPoints
			require.Len(ht, backedUpChans, len(chanPoints))

			for _, chanPoint := range backedUpChans {
				wp := ht.OutPointFromChannelPoint(chanPoint)
				_, ok := chanPoints[wp]
				require.True(ht, ok, "unexpected "+
					"backup: %v", wp)
			}
		}
	}

	chans := make(map[wire.OutPoint]struct{})
	for _, chanPoint := range chanPoints {
		chans[ht.OutPointFromChannelPoint(chanPoint)] = struct{}{}
	}

	// We should have exactly two single channel backups contained, and we
	// should also have a multi-channel backup.
	assertNumSingleBackups(2)
	assertMultiBackupFound()(true, chans)

	// We'll now close each channel on by one. After we close a channel, we
	// shouldn't be able to find that channel as a backup still. We should
	// also have one less single written to disk.
	for i, chanPoint := range chanPoints {
		ht.CloseChannel(alice, chanPoint)

		assertNumSingleBackups(len(chanPoints) - i - 1)

		delete(chans, ht.OutPointFromChannelPoint(chanPoint))
		assertMultiBackupFound()(true, chans)
	}

	// At this point we shouldn't have any single or multi-chan backups at
	// all.
	assertNumSingleBackups(0)
	assertMultiBackupFound()(false, nil)
}

// testDataLossProtection tests that if one of the nodes in a channel
// relationship lost state, they will detect this during channel sync, and the
// up-to-date party will force close the channel, giving the outdated party the
// opportunity to sweep its output.
func testDataLossProtection(ht *lntest.HarnessTest) {
	const (
		chanAmt     = funding.MaxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Carol will be the up-to-date party. We set --nolisten to ensure Dave
	// won't be able to connect to her and trigger the channel data
	// protection logic automatically. We also can't have Carol
	// automatically re-connect too early, otherwise DLP would be initiated
	// at the wrong moment.
	carol := ht.NewNode("Carol", []string{"--nolisten", "--minbackoff=1h"})

	// Dave will be the party losing his state.
	dave := ht.NewNode("Dave", nil)

	// Before we make a channel, we'll load up Carol with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// timeTravelDave is a method that will make Carol open a channel to
	// Dave, settle a series of payments, then Dave back to the state
	// before the payments happened. When this method returns Dave will
	// be unaware of the new state updates. The returned function can be
	// used to restart Dave in this state.
	timeTravelDave := func() (func() error, *lnrpc.ChannelPoint, int64) {
		// We must let the node communicate with Carol before they are
		// able to open channel, so we connect them.
		ht.EnsureConnected(carol, dave)

		// We'll first open up a channel between them with a 0.5 BTC
		// value.
		chanPoint := ht.OpenChannel(
			carol, dave, lntest.OpenChannelParams{
				Amt: chanAmt,
			},
		)

		// With the channel open, we'll create a few invoices for the
		// node that Carol will pay to in order to advance the state of
		// the channel.
		// TODO(halseth): have dangling HTLCs on the commitment, able to
		// retrieve funds?
		payReqs, _, _ := ht.CreatePayReqs(dave, paymentAmt, numInvoices)

		// Send payments from Carol using 3 of the payment hashes
		// generated above.
		ht.CompletePaymentRequests(carol, payReqs[:numInvoices/2])

		// Next query for Dave's channel state, as we sent 3 payments
		// of 10k satoshis each, it should now see his balance as being
		// 30k satoshis.
		nodeChan := ht.AssertChannelLocalBalance(
			dave, chanPoint, 30_000,
		)

		// Grab the current commitment height (update number), we'll
		// later revert him to this state after additional updates to
		// revoke this state.
		stateNumPreCopy := nodeChan.NumUpdates

		// With the temporary file created, copy the current state into
		// the temporary file we created above. Later after more
		// updates, we'll restore this state.
		ht.BackupDB(dave)

		// Reconnect the peers after the restart that was needed for
		// the db backup.
		ht.EnsureConnected(carol, dave)

		// Finally, send more payments from Carol, using the remaining
		// payment hashes.
		ht.CompletePaymentRequests(carol, payReqs[numInvoices/2:])

		flakePaymentStreamReturnEarly()

		// Now we shutdown Dave, copying over the its temporary
		// database state which has the *prior* channel state over his
		// current most up to date state. With this, we essentially
		// force Dave to travel back in time within the channel's
		// history.
		ht.RestartNodeAndRestoreDB(dave)

		// Make sure the channel is still there from the PoV of Dave.
		ht.AssertNodeNumChannels(dave, 1)

		// Now query for the channel state, it should show that it's at
		// a state number in the past, not the *latest* state.
		ht.AssertChannelNumUpdates(dave, stateNumPreCopy, chanPoint)

		balResp := dave.RPC.WalletBalance()
		restart := ht.SuspendNode(dave)

		return restart, chanPoint, balResp.ConfirmedBalance
	}

	// Reset Dave to a state where he has an outdated channel state.
	restartDave, _, daveStartingBalance := timeTravelDave()

	// We make a note of the nodes' current on-chain balances, to make sure
	// they are able to retrieve the channel funds eventually,
	carolBalResp := carol.RPC.WalletBalance()
	carolStartingBalance := carolBalResp.ConfirmedBalance

	// Restart Dave to trigger a channel resync.
	require.NoError(ht, restartDave(), "unable to restart dave")

	// Assert that once Dave comes up, they reconnect, Carol force closes
	// on chain, and both of them properly carry out the DLP protocol.
	assertDLPExecuted(
		ht, carol, carolStartingBalance, dave,
		daveStartingBalance, lnrpc.CommitmentType_STATIC_REMOTE_KEY,
	)

	// As a second part of this test, we will test the scenario where a
	// channel is closed while Dave is offline, loses his state and comes
	// back online. In this case the node should attempt to resync the
	// channel, and the peer should resend a channel sync message for the
	// closed channel, such that Dave can retrieve his funds.
	//
	// We start by letting Dave time travel back to an outdated state.
	restartDave, chanPoint2, daveStartingBalance := timeTravelDave()

	carolBalResp = carol.RPC.WalletBalance()
	carolStartingBalance = carolBalResp.ConfirmedBalance

	// Now let Carol force close the channel while Dave is offline.
	ht.ForceCloseChannel(carol, chanPoint2)

	// Make sure Carol got her balance back.
	carolBalResp = carol.RPC.WalletBalance()
	carolBalance := carolBalResp.ConfirmedBalance
	require.Greater(ht, carolBalance, carolStartingBalance,
		"expected carol to have balance increased")

	ht.AssertNodeNumChannels(carol, 0)

	// When Dave comes online, he will reconnect to Carol, try to resync
	// the channel, but it will already be closed. Carol should resend the
	// information Dave needs to sweep his funds.
	require.NoError(ht, restartDave(), "unable to restart Eve")

	// Mine a block to trigger Dave's chain watcher to process Carol's sweep
	// tx.
	//
	// TODO(yy): remove this block once the blockbeat starts remembering
	// its last processed block and can handle looking for spends in the
	// past blocks.
	ht.MineEmptyBlocks(1)

	// Make sure Dave still has the pending force close channel.
	ht.AssertNumPendingForceClose(dave, 1)

	// Dave should have a pending sweep.
	ht.AssertNumPendingSweeps(dave, 1)

	// Dave should sweep his funds.
	ht.AssertNumTxsInMempool(1)

	// Mine a block to confirm the sweep, and make sure Dave got his
	// balance back.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertNodeNumChannels(dave, 0)

	err := wait.NoError(func() error {
		daveBalResp := dave.RPC.WalletBalance()
		daveBalance := daveBalResp.ConfirmedBalance
		if daveBalance <= daveStartingBalance {
			return fmt.Errorf("expected dave to have balance "+
				"above %d, intead had %v", daveStartingBalance,
				daveBalance)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking dave's balance")
}

// createLegacyRevocationChannel creates a single channel using the legacy
// revocation producer format by using PSBT to signal a special pending channel
// ID.
func createLegacyRevocationChannel(ht *lntest.HarnessTest,
	chanAmt, pushAmt btcutil.Amount, from, to *node.HarnessNode) {

	// We'll signal to the wallet that we also want to create a channel
	// with the legacy revocation producer format that relies on deriving a
	// private key from the key ring. This is only available during itests
	// to make sure we don't hard depend on the DerivePrivKey method of the
	// key ring. We can signal the wallet by setting a custom pending
	// channel ID. To be able to do that, we need to set a funding shim
	// which is easiest by using PSBT funding. The ID is the hex
	// representation of the string "legacy-revocation".
	itestLegacyFormatChanID := [32]byte{
		0x6c, 0x65, 0x67, 0x61, 0x63, 0x79, 0x2d, 0x72, 0x65, 0x76,
		0x6f, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e,
	}
	shim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_PsbtShim{
			PsbtShim: &lnrpc.PsbtShim{
				PendingChanId: itestLegacyFormatChanID[:],
			},
		},
	}
	openChannelReq := lntest.OpenChannelParams{
		Amt:         chanAmt,
		PushAmt:     pushAmt,
		FundingShim: shim,
	}
	chanUpdates, tempPsbt := ht.OpenChannelPsbt(from, to, openChannelReq)

	// Fund the PSBT by using the source node's wallet.
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: tempPsbt,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
	}
	fundResp := from.RPC.FundPsbt(fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what
	// we need for the next step of verifying the PSBT with the funding
	// intents.
	msg := &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: itestLegacyFormatChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	}
	from.RPC.FundingStateStep(msg)

	// Now we'll ask the source node's wallet to sign the PSBT so we can
	// finish the funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := from.RPC.FinalizePsbt(finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	msg = &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: itestLegacyFormatChanID[:],
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	}
	from.RPC.FundingStateStep(msg)

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	ht.MineBlocksAndAssertNumTxes(6, 1)
	ht.AssertChannelInGraph(from, chanPoint)
	ht.AssertChannelInGraph(to, chanPoint)
}

// chanRestoreViaRPC is a helper test method that returns a nodeRestorer
// instance which will restore the target node from a password+seed, then
// trigger a SCB restore using the RPC interface.
func chanRestoreViaRPC(ht *lntest.HarnessTest, password []byte,
	mnemonic []string, multi []byte) nodeRestorer {

	backup := &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
		MultiChanBackup: multi,
	}

	return func() *node.HarnessNode {
		newNode := ht.RestoreNodeWithSeed(
			"dave", nil, password, mnemonic, "", revocationWindow,
			nil,
		)
		req := &lnrpc.RestoreChanBackupRequest{Backup: backup}
		res := newNode.RPC.RestoreChanBackups(req)
		require.Greater(ht, res.NumRestored, uint32(0))

		return newNode
	}
}

// assertTimeLockSwept when dave's outputs matures, he should claim them. This
// function will advance 2 blocks such that all the pending closing
// transactions would be swept in the end.
//
// Note: this function is only used in this test file and has been made
// specifically for testChanRestoreScenario.
func assertTimeLockSwept(ht *lntest.HarnessTest, carol, dave *node.HarnessNode,
	carolStartingBalance, daveStartingBalance int64) {

	// Carol should sweep her funds immediately, as they are not
	// timelocked.
	ht.AssertNumPendingSweeps(carol, 2)
	ht.AssertNumPendingSweeps(dave, 2)

	// We expect Carol to sweep her funds and her anchor in a single sweep
	// tx. In addition, Dave will attempt to sweep his anchor output but
	// fail due to the sweeping tx being uneconomical.
	expectedTxes := 1

	// Mine a block to trigger the sweeps.
	ht.AssertNumTxsInMempool(expectedTxes)

	// Carol should consider the channel pending force close (since she is
	// waiting for her sweep to confirm).
	ht.AssertNumPendingForceClose(carol, 1)

	// Dave is considering it "pending force close", as we must wait before
	// he can sweep her outputs.
	ht.AssertNumPendingForceClose(dave, 1)

	// Mine the sweep (and anchor) tx(ns).
	ht.MineBlocksAndAssertNumTxes(1, expectedTxes)

	// Now Carol should consider the channel fully closed.
	ht.AssertNumPendingForceClose(carol, 0)

	// We query Carol's balance to make sure it increased after the channel
	// closed. This checks that she was able to sweep the funds she had in
	// the channel.
	carolBalResp := carol.RPC.WalletBalance()
	carolBalance := carolBalResp.ConfirmedBalance
	require.Greater(ht, carolBalance, carolStartingBalance,
		"balance not increased")

	// After the Dave's output matures, he should reclaim his funds.
	//
	// The commit sweep resolver publishes the sweep tx at defaultCSV-1 and
	// we already mined one block after the commitment was published, and
	// one block to trigger Carol's sweeps, so take that into account.
	ht.MineBlocks(2)
	ht.AssertNumPendingSweeps(dave, 2)

	// Mine a block to trigger the sweeps.
	daveSweep := ht.AssertNumTxsInMempool(1)[0]
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, daveSweep)

	// Now the channel should be fully closed also from Dave's POV.
	ht.AssertNumPendingForceClose(dave, 0)

	// Make sure Dave got his balance back.
	err := wait.NoError(func() error {
		daveBalResp := dave.RPC.WalletBalance()
		daveBalance := daveBalResp.ConfirmedBalance
		if daveBalance <= daveStartingBalance {
			return fmt.Errorf("expected dave to have balance "+
				"above %d, instead had %v", daveStartingBalance,
				daveBalance)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err)

	ht.AssertNodeNumChannels(dave, 0)
	ht.AssertNodeNumChannels(carol, 0)
}

// assertDLPExecuted asserts that Dave is a node that has recovered their state
// form scratch. Carol should then force close on chain, with Dave sweeping his
// funds immediately, and Carol sweeping her fund after her CSV delay is up. If
// the blankSlate value is true, then this means that Dave won't need to sweep
// on chain as he has no funds in the channel.
func assertDLPExecuted(ht *lntest.HarnessTest,
	carol *node.HarnessNode, carolStartingBalance int64,
	dave *node.HarnessNode, daveStartingBalance int64,
	commitType lnrpc.CommitmentType) {

	ht.Helper()

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We disabled auto-reconnect for some tests to avoid timing issues.
	// To make sure the nodes are initiating DLP now, we have to manually
	// re-connect them.
	ht.EnsureConnected(carol, dave)

	// Upon reconnection, the nodes should detect that Dave is out of sync.
	// Carol should force close the channel using her latest commitment.
	ht.AssertNumTxsInMempool(1)

	// Channel should be in the state "waiting close" for Carol since she
	// broadcasted the force close tx.
	ht.AssertNumWaitingClose(carol, 1)

	// Dave should also consider the channel "waiting close", as he noticed
	// the channel was out of sync, and is now waiting for a force close to
	// hit the chain.
	ht.AssertNumWaitingClose(dave, 1)

	// Restart Dave to make sure he is able to sweep the funds after
	// shutdown.
	ht.RestartNode(dave)

	// Generate a single block, which should confirm the closing tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	blocksMined := uint32(1)

	// Dave should consider the channel pending force close (since he is
	// waiting for his sweep to confirm).
	ht.AssertNumPendingForceClose(dave, 1)

	// Carol is considering it "pending force close", as we must wait
	// before she can sweep her outputs.
	ht.AssertNumPendingForceClose(carol, 1)

	if commitType == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Dave should sweep his anchor only, since he still has the
		// lease CLTV constraint on his commitment output. We'd also
		// see Carol's anchor sweep here.

		// Both Dave and Carol should have an anchor sweep request.
		// Note that they cannot sweep them as these anchor sweepings
		// are uneconomical. In addition, they should also have their
		// leased to_local commit output.
		ht.AssertNumPendingSweeps(dave, 2)
		ht.AssertNumPendingSweeps(carol, 2)

		// After Carol's output matures, she should also reclaim her
		// funds.
		//
		// The commit sweep resolver publishes the sweep tx at
		// defaultCSV-1 and we already mined one block after the
		// commitmment was published, so take that into account.
		ht.MineEmptyBlocks(int(defaultCSV - blocksMined))

		// Carol should have two sweep requests - one for her commit
		// output and the other for her anchor.
		ht.AssertNumPendingSweeps(carol, 2)

		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Now the channel should be fully closed also from Carol's POV.
		ht.AssertNumPendingForceClose(carol, 0)

		// We'll now mine the remaining blocks to prompt Dave to sweep
		// his CLTV-constrained output.
		resp := dave.RPC.PendingChannels()
		blocksTilMaturity :=
			resp.PendingForceClosingChannels[0].BlocksTilMaturity
		require.Positive(ht, blocksTilMaturity)

		ht.MineEmptyBlocks(int(blocksTilMaturity))

		// Dave should have two sweep requests - one for his commit
		// output and the other for his anchor.
		ht.AssertNumPendingSweeps(dave, 2)

		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Now Dave should consider the channel fully closed.
		ht.AssertNumPendingForceClose(dave, 0)
	} else {
		// Dave should sweep his funds immediately, as they are not
		// timelocked. We also expect Carol and Dave sweep their
		// anchors if it's an anchor channel.
		if lntest.CommitTypeHasAnchors(commitType) {
			ht.AssertNumPendingSweeps(carol, 2)
			ht.AssertNumPendingSweeps(dave, 2)
		} else {
			ht.AssertNumPendingSweeps(dave, 1)
		}

		// Expect one tx - the commitment sweep from Dave. For anchor
		// channels, we expect the two anchor sweeping txns to be
		// failed due they are uneconomical.
		ht.MineBlocksAndAssertNumTxes(1, 1)
		blocksMined++

		// Now Dave should consider the channel fully closed.
		ht.AssertNumPendingForceClose(dave, 0)

		// After Carol's output matures, she should also reclaim her
		// funds.
		//
		// The commit sweep resolver publishes the sweep tx at
		// defaultCSV-1 and we already have blocks mined after the
		// commitmment was published, so take that into account.
		ht.MineEmptyBlocks(int(defaultCSV - blocksMined))

		// Carol should have two pending sweeps:
		// 1. her commit output.
		// 2. her anchor output, if this is anchor channel.
		if lntest.CommitTypeHasAnchors(commitType) {
			ht.AssertNumPendingSweeps(carol, 2)
		} else {
			ht.AssertNumPendingSweeps(carol, 1)
		}

		// Assert the sweeping tx is mined.
		ht.MineBlocksAndAssertNumTxes(1, 1)

		// Now the channel should be fully closed also from Carol's
		// POV.
		ht.AssertNumPendingForceClose(carol, 0)
	}

	// We query Dave's balance to make sure it increased after the channel
	// closed. This checks that he was able to sweep the funds he had in
	// the channel.
	daveBalResp := dave.RPC.WalletBalance()
	daveBalance := daveBalResp.ConfirmedBalance
	require.Greater(ht, daveBalance, daveStartingBalance,
		"balance not increased")

	// Make sure Carol got her balance back.
	err := wait.NoError(func() error {
		carolBalResp := carol.RPC.WalletBalance()
		carolBalance := carolBalResp.ConfirmedBalance

		// With Neutrino we don't get a backend error when trying to
		// publish an orphan TX (which is what the sweep for the remote
		// anchor is since the remote commitment TX was not broadcast).
		// That's why the wallet still sees that as unconfirmed and we
		// need to count the total balance instead of the confirmed.
		if ht.IsNeutrinoBackend() {
			carolBalance = carolBalResp.TotalBalance
		}

		if carolBalance <= carolStartingBalance {
			return fmt.Errorf("expected carol to have balance "+
				"above %d, instead had %v",
				carolStartingBalance, carolBalance)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout while checking carol's balance")

	ht.AssertNodeNumChannels(dave, 0)
	ht.AssertNodeNumChannels(carol, 0)
}
