package itest

import (
	"fmt"
	"io/ioutil"
	"math"
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
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testChannelBackupRestore tests that we're able to recover from, and initiate
// the DLP protocol via: the RPC restore command, restoring on unlock, and
// restoring from initial wallet creation. We'll also alternate between
// restoring form the on disk file, and restoring from the exported RPC command
// as well.
func testChannelBackupRestore(ht *lntest.HarnessTest) {
	password := []byte("El Psy Kongroo")

	var testCases = []chanRestoreTestCase{
		// Restore from backups obtained via the RPC interface. Dave
		// was the initiator, of the non-advertised channel.
		{
			name:            "restore from RPC backup",
			channelsUpdated: false,
			initiator:       true,
			private:         false,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// For this restoration method, we'll grab the
				// current multi-channel backup from the old
				// node, and use it to restore a new node
				// within the closure.
				chanBackup := st.ExportAllChanBackups(oldNode)

				multi := chanBackup.MultiChanBackup.MultiChanBackup

				// In our nodeRestorer function, we'll restore
				// the node from seed, then manually recover
				// the channel backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore the backup from the on-disk file, using the RPC
		// interface.
		{
			name:      "restore from backup file",
			initiator: true,
			private:   false,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channel.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Now that we have Dave's backup file, we'll
				// create a new nodeRestorer that will restore
				// using the on-disk channel.backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore the backup as part of node initialization with the
		// prior mnemonic and new backup seed.
		{
			name:      "restore during creation",
			initiator: true,
			private:   false,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// First, fetch the current backup state as is,
				// to obtain our latest Multi.
				chanBackup := st.ExportAllChanBackups(oldNode)
				backupSnapshot := &lnrpc.ChanBackupSnapshot{
					MultiChanBackup: chanBackup.MultiChanBackup,
				}

				// Create a new nodeRestorer that will restore
				// the node using the Multi backup we just
				// obtained above.
				return func() *lntest.HarnessNode {
					return st.RestoreNodeWithSeed(
						"dave", nil, password, mnemonic,
						"", 1000, backupSnapshot,
						copyPorts(oldNode),
					)
				}
			},
		},

		// Restore the backup once the node has already been
		// re-created, using the Unlock call.
		{
			name:      "restore during unlock",
			initiator: true,
			private:   false,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// First, fetch the current backup state as is,
				// to obtain our latest Multi.
				chanBackup := st.ExportAllChanBackups(oldNode)
				backupSnapshot := &lnrpc.ChanBackupSnapshot{
					MultiChanBackup: chanBackup.MultiChanBackup,
				}

				// Create a new nodeRestorer that will restore
				// the node with its seed, but no channel
				// backup, shutdown this initialized node, then
				// restart it again using Unlock.
				return func() *lntest.HarnessNode {
					newNode := st.RestoreNodeWithSeed(
						"dave", nil, password, mnemonic,
						"", 1000, nil,
						copyPorts(oldNode),
					)

					st.RestartNode(newNode, backupSnapshot)

					return newNode
				}
			},
		},

		// Restore the backup from the on-disk file a second time to
		// make sure imports can be canceled and later resumed.
		{
			name:      "restore from backup file twice",
			initiator: true,
			private:   false,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channel.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Now that we have Dave's backup file, we'll
				// create a new nodeRestorer that will restore
				// using the on-disk channel.backup.
				backup := &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
					MultiChanBackup: multi,
				}

				return func() *lntest.HarnessNode {
					newNode := st.RestoreNodeWithSeed(
						"dave", nil, password, mnemonic,
						"", 1000, nil,
						copyPorts(oldNode),
					)

					req := &lnrpc.RestoreChanBackupRequest{
						Backup: backup,
					}
					st.RestoreChanBackups(newNode, req)

					req = &lnrpc.RestoreChanBackupRequest{
						Backup: backup,
					}
					st.RestoreChanBackups(newNode, req)

					return newNode
				}
			},
		},

		// Use the channel backup file that contains an unconfirmed
		// channel and make sure recovery works as well.
		{
			name:            "restore unconfirmed channel file",
			channelsUpdated: false,
			initiator:       true,
			private:         false,
			unconfirmed:     true,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channel.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Let's assume time passes, the channel
				// confirms in the meantime but for some reason
				// the backup we made while it was still
				// unconfirmed is the only backup we have. We
				// should still be able to restore it. To
				// simulate time passing, we mine some blocks
				// to get the channel confirmed _after_ we saved
				// the backup.
				st.MineBlocksAndAssertTx(6, 1)

				// In our nodeRestorer function, we'll restore
				// the node from seed, then manually recover
				// the channel backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Create a backup using RPC that contains an unconfirmed
		// channel and make sure recovery works as well.
		{
			name:            "restore unconfirmed channel RPC",
			channelsUpdated: false,
			initiator:       true,
			private:         false,
			unconfirmed:     true,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// For this restoration method, we'll grab the
				// current multi-channel backup from the old
				// node. The channel should be included, even if
				// it is not confirmed yet.
				chanBackup := st.ExportAllChanBackups(oldNode)
				chanPoints := chanBackup.MultiChanBackup.ChanPoints
				require.NotEmpty(st, chanPoints,
					"unconfirmed channel not found")

				// Let's assume time passes, the channel
				// confirms in the meantime but for some reason
				// the backup we made while it was still
				// unconfirmed is the only backup we have. We
				// should still be able to restore it. To
				// simulate time passing, we mine some blocks
				// to get the channel confirmed _after_ we saved
				// the backup.
				st.MineBlocksAndAssertTx(6, 1)

				// In our nodeRestorer function, we'll restore
				// the node from seed, then manually recover
				// the channel backup.
				multi := chanBackup.MultiChanBackup.MultiChanBackup
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore the backup from the on-disk file, using the RPC
		// interface, for anchor commitment channels.
		{
			name:           "restore from backup file anchors",
			initiator:      true,
			private:        false,
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channels.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Now that we have Dave's backup file, we'll
				// create a new nodeRestorer that will restore
				// using the on-disk channels.backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore the backup from the on-disk file, using the RPC
		// interface, for script-enforced leased channels.
		{
			name:           "restore from backup file script enforced lease",
			initiator:      true,
			private:        false,
			commitmentType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channel.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Now that we have Dave's backup file, we'll
				// create a new nodeRestorer that will restore
				// using the on-disk channel.backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore by also creating a channel with the legacy revocation
		// producer format to make sure old SCBs can still be recovered.
		{
			name:             "old revocation producer format",
			initiator:        true,
			legacyRevocation: true,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// For this restoration method, we'll grab the
				// current multi-channel backup from the old
				// node, and use it to restore a new node
				// within the closure.
				chanBackup := st.ExportAllChanBackups(oldNode)
				multi := chanBackup.MultiChanBackup.MultiChanBackup

				// In our nodeRestorer function, we'll restore
				// the node from seed, then manually recover the
				// channel backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},

		// Restore a channel that was force closed by dave just before
		// going offline.
		{
			name: "restore force closed from backup file " +
				"anchors",
			initiator:       true,
			private:         false,
			commitmentType:  lnrpc.CommitmentType_ANCHORS,
			localForceClose: true,
			restoreMethod: func(st *lntest.HarnessTest,
				oldNode *lntest.HarnessNode,
				backupFilePath string,
				mnemonic []string) nodeRestorer {

				// Read the entire Multi backup stored within
				// this node's channel.backup file.
				multi, err := ioutil.ReadFile(backupFilePath)
				require.NoError(st, err)

				// Now that we have Dave's backup file, we'll
				// create a new nodeRestorer that will restore
				// using the on-disk channel.backup.
				return chanRestoreViaRPC(
					st, password, mnemonic, multi, oldNode,
				)
			},
		},
	}

	// TODO(roasbeef): online vs offline close?

	// TODO(roasbeef): need to re-trigger the on-disk file once the node
	// ann is updated?

	for _, testCase := range testCases {
		testCase := testCase
		success := ht.Run(testCase.name, func(t *testing.T) {
			h := ht.Subtest(t)

			// Start each test with the default static fee estimate.
			h.SetFeeEstimate(12500)

			testChanRestoreScenario(h, &testCase, password)
		})
		if !success {
			break
		}
	}
}

// testChannelBackupUpdates tests that both the streaming channel update RPC,
// and the on-disk channel.backup are updated each time a channel is
// opened/closed.
func testChannelBackupUpdates(ht *lntest.HarnessTest) {
	alice := ht.Alice

	// First, we'll make a temp directory that we'll use to store our
	// backup file, so we can check in on it during the test easily.
	backupDir, err := ioutil.TempDir("", "")
	require.NoError(ht, err, "unable to create backup dir")
	defer os.RemoveAll(backupDir)

	// First, we'll create a new node, Carol. We'll also create a temporary
	// file that Carol will use to store her channel backups.
	backupFilePath := filepath.Join(
		backupDir, chanbackup.DefaultBackupFileName,
	)
	carolArgs := fmt.Sprintf("--backupfilepath=%v", backupFilePath)
	carol := ht.NewNode("carol", []string{carolArgs})
	defer ht.Shutdown(carol)

	// Next, we'll register for streaming notifications for changes to the
	// backup file.
	backupStream := ht.SubscribeChannelBackups(carol)

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

	// assertBackupFileState is a helper function that we'll use to compare
	// the on disk back up file to our currentBackup pointer above.
	assertBackupFileState := func() {
		err := wait.NoError(func() error {
			packedBackup, err := ioutil.ReadFile(backupFilePath)
			if err != nil {
				return fmt.Errorf("unable to read backup "+
					"file: %v", err)
			}

			// As each back up file will be encrypted with a fresh
			// nonce, we can't compare them directly, so instead
			// we'll compare the length which is a proxy for the
			// number of channels that the multi-backup contains.
			rawBackup := currentBackup.MultiChanBackup.MultiChanBackup
			if len(rawBackup) != len(packedBackup) {
				return fmt.Errorf("backup files don't match: "+
					"expected %x got %x", rawBackup, packedBackup)
			}

			// Additionally, we'll assert that both backups up
			// returned are valid.
			for _, backup := range [][]byte{rawBackup, packedBackup} {
				snapshot := &lnrpc.ChanBackupSnapshot{
					MultiChanBackup: &lnrpc.MultiChanBackup{
						MultiChanBackup: backup,
					},
				}

				ht.VerifyChanBackup(carol, snapshot)
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
	assertBackupFileState()

	// Next, we'll close the channels one by one. After each channel
	// closure, we should get a notification, and the on-disk state should
	// match this state as well.
	for i := 0; i < numChans; i++ {
		// To ensure force closes also trigger an update, we'll force
		// close half of the channels.
		forceClose := i%2 == 0

		chanPoint := chanPoints[i]

		ht.CloseChannel(alice, chanPoint, forceClose)

		// If we force closed the channel, then we'll mine enough
		// blocks to ensure all outputs have been swept.
		if forceClose {
			// A local force closed channel will trigger a
			// notification once the commitment TX confirms on
			// chain. But that won't remove the channel from the
			// backup just yet, that will only happen once the time
			// locked contract was fully resolved on chain.
			assertBackupNtfns(1)

			ht.CleanupForceClose(alice, chanPoint)

			// Now that the channel's been fully resolved, we expect
			// another notification.
			assertBackupNtfns(1)
			assertBackupFileState()
		} else {
			// We should get a single notification after closing,
			// and the on-disk state should match this latest
			// notifications.
			assertBackupNtfns(1)
			assertBackupFileState()
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
	defer ht.Shutdown(carol)

	// With Carol up, we'll now connect her to Alice, and open a channel
	// between them.
	alice := ht.Alice
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
		chanBackup := ht.ExportChanBackup(carol, chanPoint)

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
			chanSnapshot := ht.ExportAllChanBackups(carol)

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

	assertMultiBackupFound := func() func(bool, map[wire.OutPoint]struct{}) {
		chanSnapshot := ht.ExportAllChanBackups(carol)

		return func(found bool, chanPoints map[wire.OutPoint]struct{}) {
			switch {
			case found && chanSnapshot.MultiChanBackup == nil:
				require.Fail(ht, "multi-backup not present")

			case !found && chanSnapshot.MultiChanBackup != nil &&
				(len(chanSnapshot.MultiChanBackup.MultiChanBackup) !=
					chanbackup.NilMultiSizePacked):

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
		ht.CloseChannel(alice, chanPoint, false)

		assertNumSingleBackups(len(chanPoints) - i - 1)

		delete(chans, ht.OutPointFromChannelPoint(chanPoint))
		assertMultiBackupFound()(true, chans)
	}

	// At this point we shouldn't have any single or multi-chan backups at
	// all.
	assertNumSingleBackups(0)
	assertMultiBackupFound()(false, nil)
}

// nodeRestorer is a function closure that allows each chanRestoreTestCase to
// control exactly *how* the prior node is restored. This might be using an
// backup obtained over RPC, or the file system, etc.
type nodeRestorer func() *lntest.HarnessNode

// chanRestoreTestCase describes a test case for an end to end SCB restoration
// work flow. One node will start from scratch using an existing SCB. At the
// end of the est, both nodes should be made whole via the DLP protocol.
type chanRestoreTestCase struct {
	// name is the name of the target test case.
	name string

	// channelsUpdated is false then this means that no updates
	// have taken place within the channel before restore.
	// Otherwise, HTLCs will be settled between the two parties
	// before restoration modifying the balance beyond the initial
	// allocation.
	channelsUpdated bool

	// initiator signals if Dave should be the one that opens the
	// channel to Alice, or if it should be the other way around.
	initiator bool

	// private signals if the channel from Dave to Carol should be
	// private or not.
	private bool

	// unconfirmed signals if the channel from Dave to Carol should be
	// confirmed or not.
	unconfirmed bool

	// commitmentType specifies the commitment type that should be used for
	// the channel from Dave to Carol.
	commitmentType lnrpc.CommitmentType

	// legacyRevocation signals if a channel with the legacy revocation
	// producer format should also be created before restoring.
	legacyRevocation bool

	// localForceClose signals if the channel should be force closed by the
	// node that is going to recover.
	localForceClose bool

	// restoreMethod takes an old node, then returns a function
	// closure that'll return the same node, but with its state
	// restored via a custom method. We use this to abstract away
	// _how_ a node is restored from our assertions once the node
	// has been fully restored itself.
	restoreMethod func(ht *lntest.HarnessTest,
		oldNode *lntest.HarnessNode, backupFilePath string,
		mnemonic []string) nodeRestorer
}

// testChanRestoreScenario executes a chanRestoreTestCase from end to end,
// ensuring that after Dave restores his channel state according to the
// testCase, the DLP protocol is executed properly and both nodes are made
// whole.
func testChanRestoreScenario(ht *lntest.HarnessTest,
	testCase *chanRestoreTestCase, password []byte) {

	const (
		chanAmt = btcutil.Amount(10000000)
		pushAmt = btcutil.Amount(5000000)
	)

	nodeArgs := []string{
		"--minbackoff=50ms",
		"--maxbackoff=1s",
	}
	if testCase.commitmentType != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		args := nodeArgsForCommitType(testCase.commitmentType)
		nodeArgs = append(nodeArgs, args...)
	}

	// First, we'll create a brand new node we'll use within the test. If
	// we have a custom backup file specified, then we'll also create that
	// for use.
	dave, mnemonic, _ := ht.NewNodeWithSeed(
		"dave", nodeArgs, password, false,
	)
	// Defer to a closure instead of to shutdownAndAssert due to the value
	// of 'dave' changing throughout the test.
	defer func() {
		ht.Shutdown(dave)
	}()

	carol := ht.NewNode("carol", nodeArgs)
	defer ht.Shutdown(carol)

	// Now that our new nodes are created, we'll give them some coins for
	// channel opening and anchor sweeping.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

	// For the anchor output case we need two UTXOs for Carol so she can
	// sweep both the local and remote anchor.
	if commitTypeHasAnchors(testCase.commitmentType) {
		ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	ht.SendCoins(btcutil.SatoshiPerBitcoin, dave)

	var from, to *lntest.HarnessNode
	if testCase.initiator {
		from, to = dave, carol
	} else {
		from, to = carol, dave
	}

	// Next, we'll connect Dave to Carol, and open a new channel to her
	// with a portion pushed.
	ht.ConnectNodes(dave, carol)

	// We will either open a confirmed or unconfirmed channel, depending on
	// the requirements of the test case.
	var chanPoint *lnrpc.ChannelPoint
	switch {
	case testCase.unconfirmed:
		ht.OpenPendingChannel(from, to, chanAmt, pushAmt)

		// Give the pubsub some time to update the channel backup.
		err := wait.NoError(func() error {
			fi, err := os.Stat(dave.ChanBackupPath())
			if err != nil {
				return err
			}
			if fi.Size() <= chanbackup.NilMultiSizePacked {
				return fmt.Errorf("backup file empty")
			}
			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "channel backup not updated in time")

	// Also create channels with the legacy revocation producer format if
	// requested.
	case testCase.legacyRevocation:
		createLegacyRevocationChannel(ht, chanAmt, pushAmt, from, to)

	default:
		var fundingShim *lnrpc.FundingShim
		if testCase.commitmentType == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
			_, minerHeight := ht.GetBestBlock()
			thawHeight := uint32(minerHeight + 144)

			fundingShim, _, _ = deriveFundingShim(
				ht, from, to, chanAmt, thawHeight, true,
			)
		}
		chanPoint = ht.OpenChannel(
			from, to, lntest.OpenChannelParams{
				Amt:            chanAmt,
				PushAmt:        pushAmt,
				Private:        testCase.private,
				FundingShim:    fundingShim,
				CommitmentType: testCase.commitmentType,
			},
		)

		// Wait for both sides to see the opened channel.
		ht.AssertChannelOpen(dave, chanPoint)
		ht.AssertChannelOpen(carol, chanPoint)
	}

	// If both parties should start with existing channel updates, then
	// we'll send+settle an HTLC between 'from' and 'to' now.
	if testCase.channelsUpdated {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: 100000,
		}
		invoiceResp := ht.AddInvoice(invoice, to)

		ht.CompletePaymentRequests(
			from, []string{invoiceResp.PaymentRequest}, true,
		)
	}

	// If we're testing that locally force closed channels can be restored
	// then we issue the force close now.
	if testCase.localForceClose && chanPoint != nil {
		ht.CloseChannelStreamAndAssert(dave, chanPoint, true)

		// After closing the channel we mine one transaction to make
		// sure the commitment TX was confirmed.
		ht.MineBlocksAndAssertTx(1, 1)

		// Now we need to make sure that the channel is still in the
		// backup. Otherwise restoring won't work later.
		ht.ExportChanBackup(dave, chanPoint)
	}

	// Before we start the recovery, we'll record the balances of both
	// Carol and Dave to ensure they both sweep their coins at the end.
	carolBalResp := ht.GetWalletBalance(carol)
	carolStartingBalance := carolBalResp.ConfirmedBalance

	daveBalance := ht.GetWalletBalance(dave)
	daveStartingBalance := daveBalance.ConfirmedBalance

	// At this point, we'll now execute the restore method to give us the
	// new node we should attempt our assertions against.
	backupFilePath := dave.ChanBackupPath()
	restoredNodeFunc := testCase.restoreMethod(
		ht, dave, backupFilePath, mnemonic,
	)

	// Now that we're able to make our restored now, we'll shutdown the old
	// Dave node as we'll be storing it shortly below.
	ht.Shutdown(dave)

	// To make sure the channel state is advanced correctly if the channel
	// peer is not online at first, we also shutdown Carol.
	restartCarol := ht.SuspendNode(carol)

	// Next, we'll make a new Dave and start the bulk of our recovery
	// workflow.
	dave = restoredNodeFunc()

	// First ensure that the on-chain balance is restored.
	err := wait.NoError(func() error {
		daveBalResp := ht.GetWalletBalance(dave)
		daveBal := daveBalResp.ConfirmedBalance
		if daveBal <= 0 {
			return fmt.Errorf("expected positive balance, had %v",
				daveBal)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "On-chain balance not restored")

	// For our force close scenario we don't need the channel to be closed
	// by Carol since it was already force closed before we started the
	// recovery. All we need is for Carol to send us over the commit height
	// so we can sweep the time locked output with the correct commit point.
	if testCase.localForceClose {
		ht.AssertNumPendingCloseChannels(dave, 0, 1)

		require.NoError(ht, restartCarol(), "restart carol failed")

		// Now that we have our new node up, we expect that it'll
		// re-connect to Carol automatically based on the restored
		// backup.
		ht.EnsureConnected(dave, carol)

		assertTimeLockSwept(
			ht, carol, carolStartingBalance, dave,
			daveStartingBalance,
			commitTypeHasAnchors(testCase.commitmentType),
		)

		return
	}

	// We now check that the restored channel is in the proper state. It
	// should not yet be force closing as no connection with the remote
	// peer was established yet. We should also not be able to close the
	// channel.
	ht.AssertNumPendingCloseChannels(dave, 1, 0)
	pendingChanResp := ht.GetPendingChannels(dave)

	// We now need to make sure the server is fully started before we can
	// actually close the channel.
	ht.AssertNodeStarted(dave)

	// We also want to make sure we cannot force close in this state. That
	// would get the state machine in a weird state.
	chanPointParts := strings.Split(
		pendingChanResp.WaitingCloseChannels[0].Channel.ChannelPoint,
		":",
	)
	chanPointIndex, _ := strconv.ParseUint(chanPointParts[1], 10, 32)
	resp := ht.CloseChannelStreamAndAssert(
		dave, &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
				FundingTxidStr: chanPointParts[0],
			},
			OutputIndex: uint32(chanPointIndex),
		}, true,
	)

	// We don't get an error directly but only when reading the first
	// message of the stream.
	_, err = resp.Recv()
	require.Error(ht, err)
	require.Contains(ht, err.Error(), "cannot close channel with state: ")
	require.Contains(ht, err.Error(), "ChanStatusRestored")

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed in case of anchor commitments.
	ht.SetFeeEstimate(30000)

	// Now that we have ensured that the channels restored by the backup are
	// in the correct state even without the remote peer telling us so,
	// let's start up Carol again.
	require.NoError(ht, restartCarol(), "restart carol failed")

	numUTXOs := 1
	if commitTypeHasAnchors(testCase.commitmentType) {
		numUTXOs = 2
	}
	ht.AssertNumUTXOs(carol, numUTXOs, math.MaxInt32, 0)

	// Now that we have our new node up, we expect that it'll re-connect to
	// Carol automatically based on the restored backup.
	ht.EnsureConnected(dave, carol)

	// TODO(roasbeef): move dave restarts?

	// Now we'll assert that both sides properly execute the DLP protocol.
	// We grab their balances now to ensure that they're made whole at the
	// end of the protocol.
	assertDLPExecuted(
		ht, carol, carolStartingBalance, dave,
		daveStartingBalance, testCase.commitmentType,
	)
}

// createLegacyRevocationChannel creates a single channel using the legacy
// revocation producer format by using PSBT to signal a special pending channel
// ID.
func createLegacyRevocationChannel(ht *lntest.HarnessTest,
	chanAmt, pushAmt btcutil.Amount, from, to *lntest.HarnessNode) {

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
	fundResp := ht.FundPsbt(from, fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step of verifying the PSBT with the funding intents.
	msg := &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: itestLegacyFormatChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	}
	ht.FundingStateStep(from, msg)

	// Now we'll ask the source node's wallet to sign the PSBT so we can
	// finish the funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := ht.FinalizePsbt(from, finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	msg = &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: itestLegacyFormatChanID[:],
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	}
	ht.FundingStateStep(from, msg)

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveChanUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	ht.MineBlocksAndAssertTx(6, 1)
	ht.AssertChannelOpen(from, chanPoint)
	ht.AssertChannelOpen(to, chanPoint)
}

// chanRestoreViaRPC is a helper test method that returns a nodeRestorer
// instance which will restore the target node from a password+seed, then
// trigger a SCB restore using the RPC interface.
func chanRestoreViaRPC(ht *lntest.HarnessTest, password []byte,
	mnemonic []string, multi []byte,
	oldNode *lntest.HarnessNode) nodeRestorer {

	backup := &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
		MultiChanBackup: multi,
	}

	return func() *lntest.HarnessNode {
		newNode := ht.RestoreNodeWithSeed(
			"dave", nil, password, mnemonic, "", 1000, nil,
			copyPorts(oldNode),
		)
		ht.RestoreChanBackups(
			newNode, &lnrpc.RestoreChanBackupRequest{
				Backup: backup,
			},
		)
		return newNode
	}
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
	defer ht.Shutdown(carol)

	// Dave will be the party losing his state.
	dave := ht.NewNode("Dave", nil)
	defer ht.Shutdown(dave)

	// Before we make a channel, we'll load up Carol with some coins sent
	// directly from the miner.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)

	// timeTravel is a method that will make Carol open a channel to the
	// passed node, settle a series of payments, then reset the node back
	// to the state before the payments happened. When this method returns
	// the node will be unaware of the new state updates. The returned
	// function can be used to restart the node in this state.
	timeTravel := func(node *lntest.HarnessNode) (func() error,
		*lnrpc.ChannelPoint, int64) {

		// We must let the node communicate with Carol before they are
		// able to open channel, so we connect them.
		ht.EnsureConnected(carol, node)

		// We'll first open up a channel between them with a 0.5 BTC
		// value.
		chanPoint := ht.OpenChannel(
			carol, node, lntest.OpenChannelParams{
				Amt: chanAmt,
			},
		)

		// With the channel open, we'll create a few invoices for the
		// node that Carol will pay to in order to advance the state of
		// the channel.
		// TODO(halseth): have dangling HTLCs on the commitment, able to
		// retrieve funds?
		payReqs, _, _ := ht.CreatePayReqs(node, paymentAmt, numInvoices)

		// Wait for Carol to receive the channel edge from the funding
		// manager.
		ht.AssertChannelOpen(carol, chanPoint)

		// Send payments from Carol using 3 of the payment hashes
		// generated above.
		ht.CompletePaymentRequests(carol, payReqs[:numInvoices/2], true)

		// Next query for the node's channel state, as we sent 3
		// payments of 10k satoshis each, it should now see his balance
		// as being 30k satoshis.
		nodeChan := ht.AssertLocalBalance(node, chanPoint, 30_000)

		// Grab the current commitment height (update number), we'll
		// later revert him to this state after additional updates to
		// revoke this state.
		stateNumPreCopy := nodeChan.NumUpdates

		// With the temporary file created, copy the current state into
		// the temporary file we created above. Later after more
		// updates, we'll restore this state.
		ht.BackupDb(node)

		// Reconnect the peers after the restart that was needed for the db
		// backup.
		ht.EnsureConnected(carol, node)

		// Finally, send more payments from , using the remaining
		// payment hashes.
		ht.CompletePaymentRequests(carol, payReqs[numInvoices/2:], true)

		// Now we shutdown the node, copying over the its temporary
		// database state which has the *prior* channel state over his
		// current most up to date state. With this, we essentially
		// force the node to travel back in time within the channel's
		// history.
		ht.RestartNodeAndRestoreDb(node)

		// Make sure the channel is still there from the PoV of the
		// node.
		ht.AssertNodeNumChannels(node, 1)

		// Now query for the channel state, it should show that it's at
		// a state number in the past, not the *latest* state.
		ht.AssertNumUpdates(node, stateNumPreCopy, chanPoint)

		balResp := ht.GetWalletBalance(node)
		restart := ht.SuspendNode(node)

		return restart, chanPoint, balResp.ConfirmedBalance
	}

	// Reset Dave to a state where he has an outdated channel state.
	restartDave, _, daveStartingBalance := timeTravel(dave)

	// We make a note of the nodes' current on-chain balances, to make sure
	// they are able to retrieve the channel funds eventually,
	carolBalResp := ht.GetWalletBalance(carol)
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
	restartDave, chanPoint2, daveStartingBalance := timeTravel(dave)

	carolBalResp = ht.GetWalletBalance(carol)
	carolStartingBalance = carolBalResp.ConfirmedBalance

	// Now let Carol force close the channel while Dave is offline.
	ht.CloseChannel(carol, chanPoint2, true)

	// Wait for the channel to be marked pending force close.
	ht.WaitForChannelPendingForceClose(carol, chanPoint2)

	// Mine enough blocks for Carol to sweep her funds.
	ht.MineBlocks(defaultCSV - 1)

	carolSweep := ht.AssertNumTxsInMempool(1)[0]
	block := ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, carolSweep)

	// Now the channel should be fully closed also from Carol's POV.
	ht.AssertNumPendingCloseChannels(carol, 0, 0)

	// Make sure Carol got her balance back.
	carolBalResp = ht.GetWalletBalance(carol)
	carolBalance := carolBalResp.ConfirmedBalance
	require.Greater(ht, carolBalance, carolStartingBalance,
		"expected carol to have balance increased")

	ht.AssertNodeNumChannels(carol, 0)

	// When Dave comes online, he will reconnect to Carol, try to resync
	// the channel, but it will already be closed. Carol should resend the
	// information Dave needs to sweep his funds.
	require.NoError(ht, restartDave(), "unable to restart Eve")

	// Dave should sweep his funds.
	ht.AssertNumTxsInMempool(1)

	// Mine a block to confirm the sweep, and make sure Dave got his
	// balance back.
	ht.MineBlocksAndAssertTx(1, 1)
	ht.AssertNodeNumChannels(dave, 0)

	err := wait.NoError(func() error {
		daveBalResp := ht.GetWalletBalance(dave)
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

// copyPorts returns a node option function that copies the ports of an
// existing node over to the newly created one.
//
// Note: only used in current test file.
func copyPorts(oldNode *lntest.HarnessNode) lntest.NodeOption {
	return func(cfg *lntest.BaseNodeConfig) {
		cfg.P2PPort = oldNode.Cfg.P2PPort
		cfg.RPCPort = oldNode.Cfg.RPCPort
		cfg.RESTPort = oldNode.Cfg.RESTPort
		cfg.ProfilePort = oldNode.Cfg.ProfilePort
	}
}

// assertDLPExecuted asserts that Dave is a node that has recovered their state
// form scratch. Carol should then force close on chain, with Dave sweeping his
// funds immediately, and Carol sweeping her fund after her CSV delay is up. If
// the blankSlate value is true, then this means that Dave won't need to sweep
// on chain as he has no funds in the channel.
func assertDLPExecuted(ht *lntest.HarnessTest,
	carol *lntest.HarnessNode, carolStartingBalance int64,
	dave *lntest.HarnessNode, daveStartingBalance int64,
	commitType lnrpc.CommitmentType) {

	// Increase the fee estimate so that the following force close tx will
	// be cpfp'ed.
	ht.SetFeeEstimate(30000)

	// We disabled auto-reconnect for some tests to avoid timing issues.
	// To make sure the nodes are initiating DLP now, we have to manually
	// re-connect them.
	ht.EnsureConnected(carol, dave)

	// Upon reconnection, the nodes should detect that Dave is out of sync.
	// Carol should force close the channel using her latest commitment.
	expectedTxes := 1
	if commitTypeHasAnchors(commitType) {
		expectedTxes = 2
	}
	ht.AssertNumTxsInMempool(expectedTxes)

	// Channel should be in the state "waiting close" for Carol since she
	// broadcasted the force close tx.
	ht.AssertNumPendingCloseChannels(carol, 1, 0)

	// Dave should also consider the channel "waiting close", as he noticed
	// the channel was out of sync, and is now waiting for a force close to
	// hit the chain.
	ht.AssertNumPendingCloseChannels(dave, 1, 0)

	// Restart Dave to make sure he is able to sweep the funds after
	// shutdown.
	ht.RestartNode(dave)

	// Generate a single block, which should confirm the closing tx.
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// Dave should consider the channel pending force close (since he is
	// waiting for his sweep to confirm).
	ht.AssertNumPendingCloseChannels(dave, 0, 1)

	// Carol is considering it "pending force close", as we must wait
	// before she can sweep her outputs.
	ht.AssertNumPendingCloseChannels(carol, 0, 1)

	if commitType == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		// Dave should sweep his anchor only, since he still has the
		// lease CLTV constraint on his commitment output.
		ht.AssertNumTxsInMempool(1)

		// Mine Dave's anchor sweep tx.
		ht.MineBlocksAndAssertTx(1, 1)

		// After Carol's output matures, she should also reclaim her
		// funds.
		//
		// The commit sweep resolver publishes the sweep tx at
		// defaultCSV-1 and we already mined one block after the
		// commitmment was published, so take that into account.
		ht.MineBlocks(defaultCSV - 1 - 1)
		carolSweep := ht.AssertNumTxsInMempool(1)[0]
		block := ht.MineBlocksAndAssertTx(1, 1)[0]
		ht.AssertTxInBlock(block, carolSweep)

		// Now the channel should be fully closed also from Carol's POV.
		ht.AssertNumPendingCloseChannels(carol, 0, 0)

		// We'll now mine the remaining blocks to prompt Dave to sweep
		// his CLTV-constrained output.
		resp := ht.GetPendingChannels(dave)
		blocksTilMaturity :=
			resp.PendingForceClosingChannels[0].BlocksTilMaturity
		require.Positive(ht, blocksTilMaturity)

		ht.MineBlocks(uint32(blocksTilMaturity))
		daveSweep := ht.AssertNumTxsInMempool(1)[0]
		block = ht.MineBlocksAndAssertTx(1, 1)[0]
		ht.AssertTxInBlock(block, daveSweep)

		// Now Dave should consider the channel fully closed.
		ht.AssertNumPendingCloseChannels(dave, 0, 0)
	} else {
		// Dave should sweep his funds immediately, as they are not
		// timelocked. We also expect Dave to sweep his anchor, if
		// present.
		ht.AssertNumTxsInMempool(expectedTxes)

		// Mine the sweep tx.
		ht.MineBlocksAndAssertTx(1, expectedTxes)

		// Now Dave should consider the channel fully closed.
		ht.AssertNumPendingCloseChannels(dave, 0, 0)

		// After Carol's output matures, she should also reclaim her
		// funds.
		//
		// The commit sweep resolver publishes the sweep tx at
		// defaultCSV-1 and we already mined one block after the
		// commitmment was published, so take that into account.
		ht.MineBlocks(defaultCSV - 1 - 1)
		carolSweep := ht.AssertNumTxsInMempool(1)[0]
		block := ht.MineBlocksAndAssertTx(1, 1)[0]
		ht.AssertTxInBlock(block, carolSweep)

		// Now the channel should be fully closed also from Carol's POV.
		ht.AssertNumPendingCloseChannels(carol, 0, 0)
	}

	// We query Dave's balance to make sure it increased after the channel
	// closed. This checks that he was able to sweep the funds he had in
	// the channel.
	daveBalResp := ht.GetWalletBalance(dave)
	daveBalance := daveBalResp.ConfirmedBalance
	require.Greater(ht, daveBalance, daveStartingBalance,
		"balance not increased")

	// Make sure Carol got her balance back.
	err := wait.NoError(func() error {
		carolBalResp := ht.GetWalletBalance(carol)
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

// assertTimeLockSwept when dave's outputs matures, he should claim them. This
// function will advance 2 blocks such that all the pending closing
// transactions would be swept in the end.
//
// Note: this function is only used in this test file and has been made
// specifically for testChanRestoreScenario.
func assertTimeLockSwept(ht *lntest.HarnessTest,
	carol *lntest.HarnessNode, carolStartingBalance int64,
	dave *lntest.HarnessNode, daveStartingBalance int64, anchors bool) {

	expectedTxes := 2
	if anchors {
		expectedTxes = 3
	}

	// Carol should sweep her funds immediately, as they are not timelocked.
	// We also expect Carol and Dave to sweep their anchor, if present.
	ht.AssertNumTxsInMempool(expectedTxes)

	// Carol should consider the channel pending force close (since she is
	// waiting for her sweep to confirm).
	ht.AssertNumPendingCloseChannels(carol, 0, 1)

	// Dave is considering it "pending force close", as we must wait
	// before he can sweep her outputs.
	ht.AssertNumPendingCloseChannels(dave, 0, 1)

	// Mine the sweep (and anchor) tx(ns).
	ht.MineBlocksAndAssertTx(1, expectedTxes)

	// Now Carol should consider the channel fully closed.
	ht.AssertNumPendingCloseChannels(carol, 0, 0)

	// We query Carol's balance to make sure it increased after the channel
	// closed. This checks that she was able to sweep the funds she had in
	// the channel.
	carolBalResp := ht.GetWalletBalance(carol)
	carolBalance := carolBalResp.ConfirmedBalance
	require.Greater(ht, carolBalance, carolStartingBalance,
		"balance not increased")

	// After the Dave's output matures, he should reclaim his funds.
	//
	// The commit sweep resolver publishes the sweep tx at defaultCSV-1 and
	// we already mined one block after the commitment was published, so
	// take that into account.
	ht.MineBlocks(defaultCSV - 1 - 1)
	daveSweep := ht.AssertNumTxsInMempool(1)[0]
	block := ht.MineBlocksAndAssertTx(1, 1)[0]
	ht.AssertTxInBlock(block, daveSweep)

	// Now the channel should be fully closed also from Dave's POV.
	ht.AssertNumPendingCloseChannels(dave, 0, 0)

	// Make sure Dave got his balance back.
	err := wait.NoError(func() error {
		daveBalResp := ht.GetWalletBalance(dave)
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
