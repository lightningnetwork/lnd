package contractcourt

import (
	"net"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestChainArbitratorRepulishCloses tests that the chain arbitrator will
// republish closing transactions for channels marked CommitementBroadcast or
// CoopBroadcast in the database at startup.
func TestChainArbitratorRepublishCloses(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	// Create 10 test channels and sync them to the database.
	const numChans = 10
	var channels []*channeldb.OpenChannel
	for i := 0; i < numChans; i++ {
		lChannel, _, err := lnwallet.CreateTestChannels(
			t, channeldb.SingleFunderTweaklessBit,
		)
		if err != nil {
			t.Fatal(err)
		}

		channel := lChannel.State()

		// We manually set the db here to make sure all channels are
		// synced to the same db.
		channel.Db = db.ChannelStateDB()

		addr := &net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18556,
		}
		if err := channel.SyncPending(addr, 101); err != nil {
			t.Fatal(err)
		}

		channels = append(channels, channel)
	}

	// Mark half of the channels as commitment broadcasted.
	for i := 0; i < numChans/2; i++ {
		closeTx := channels[i].FundingTxn.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint
		err := channels[i].MarkCommitmentBroadcasted(
			closeTx, lntypes.Local,
		)
		if err != nil {
			t.Fatal(err)
		}

		err = channels[i].MarkCoopBroadcasted(closeTx, lntypes.Local)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We keep track of the transactions published by the ChainArbitrator
	// at startup.
	published := make(map[chainhash.Hash]int)

	chainArbCfg := ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			published[tx.TxHash()]++
			return nil
		},
		Clock:  clock.NewDefaultClock(),
		Budget: *DefaultBudgetConfig(),
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)

	beat := newBeatFromHeight(0)
	if err := chainArb.Start(beat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	// Half of the channels should have had their closing tx re-published.
	if len(published) != numChans/2 {
		t.Fatalf("expected %d re-published transactions, got %d",
			numChans/2, len(published))
	}

	// And make sure the published transactions are correct, and unique.
	for i := 0; i < numChans/2; i++ {
		closeTx := channels[i].FundingTxn.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint

		count, ok := published[closeTx.TxHash()]
		if !ok {
			t.Fatalf("closing tx not re-published")
		}

		// We expect one coop close and one force close.
		if count != 2 {
			t.Fatalf("expected 2 closing txns, only got %d", count)
		}

		delete(published, closeTx.TxHash())
	}

	if len(published) != 0 {
		t.Fatalf("unexpected tx published")
	}
}

// TestResolveContract tests that if we have an active channel being watched by
// the chain arb, then a call to ResolveContract will mark the channel as fully
// closed in the database, and also clean up all arbitrator state.
func TestResolveContract(t *testing.T) {
	t.Parallel()

	db := channeldb.OpenForTesting(t, t.TempDir())

	// With the DB created, we'll make a new channel, and mark it as
	// pending open within the database.
	newChannel, _, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err, "unable to make new test channel")
	channel := newChannel.State()
	channel.Db = db.ChannelStateDB()
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}
	if err := channel.SyncPending(addr, 101); err != nil {
		t.Fatalf("unable to write channel to db: %v", err)
	}

	// With the channel inserted into the database, we'll now create a new
	// chain arbitrator that should pick up these new channels and launch
	// resolver for them.
	chainArbCfg := ChainArbitratorConfig{
		ChainIO: &mock.ChainIO{},
		Notifier: &mock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			return nil
		},
		Clock:  clock.NewDefaultClock(),
		Budget: *DefaultBudgetConfig(),
		QueryIncomingCircuit: func(
			circuit models.CircuitKey) *models.CircuitKey {

			return nil
		},
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)
	beat := newBeatFromHeight(0)
	if err := chainArb.Start(beat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		require.NoError(t, chainArb.Stop())
	})

	channelArb := chainArb.activeChannels[channel.FundingOutpoint]

	// While the resolver are active, we'll now remove the channel from the
	// database (mark is as closed).
	err = db.ChannelStateDB().AbandonChannel(&channel.FundingOutpoint, 4)
	require.NoError(t, err, "unable to remove channel")

	// With the channel removed, we'll now manually call ResolveContract.
	// This stimulates needing to remove a channel from the chain arb due
	// to any possible external consistency issues.
	err = chainArb.ResolveContract(channel.FundingOutpoint)
	require.NoError(t, err, "unable to resolve contract")

	// The shouldn't be an active chain watcher or channel arb for this
	// channel.
	if len(chainArb.activeChannels) != 0 {
		t.Fatalf("expected zero active channels, instead have %v",
			len(chainArb.activeChannels))
	}
	if len(chainArb.activeWatchers) != 0 {
		t.Fatalf("expected zero active watchers, instead have %v",
			len(chainArb.activeWatchers))
	}

	// At this point, the channel's arbitrator log should also be empty as
	// well.
	_, err = channelArb.log.FetchContractResolutions()
	if err != errScopeBucketNoExist {
		t.Fatalf("channel arb log state should have been "+
			"removed: %v", err)
	}

	// If we attempt to call this method again, then we should get a nil
	// error, as there is no more state to be cleaned up.
	err = chainArb.ResolveContract(channel.FundingOutpoint)
	require.NoError(t, err, "second resolve call shouldn't fail")
}
