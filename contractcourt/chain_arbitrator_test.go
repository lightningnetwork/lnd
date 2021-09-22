package contractcourt

import (
	"io/ioutil"
	"net"
	"os"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// TestChainArbitratorRepulishCloses tests that the chain arbitrator will
// republish closing transactions for channels marked CommitementBroadcast or
// CoopBroadcast in the database at startup.
func TestChainArbitratorRepublishCloses(t *testing.T) {
	t.Parallel()

	tempPath, err := ioutil.TempDir("", "testdb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempPath)

	db, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create 10 test channels and sync them to the database.
	const numChans = 10
	var channels []*channeldb.OpenChannel
	for i := 0; i < numChans; i++ {
		lChannel, _, cleanup, err := lnwallet.CreateTestChannels(
			channeldb.SingleFunderTweaklessBit,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

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
		err := channels[i].MarkCommitmentBroadcasted(closeTx, true)
		if err != nil {
			t.Fatal(err)
		}

		err = channels[i].MarkCoopBroadcasted(closeTx, true)
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
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			published[tx.TxHash()]++
			return nil
		},
		Clock: clock.NewDefaultClock(),
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)

	if err := chainArb.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := chainArb.Stop(); err != nil {
			t.Fatal(err)
		}
	}()

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

	// To start with, we'll create a new temp DB for the duration of this
	// test.
	tempPath, err := ioutil.TempDir("", "testdb")
	if err != nil {
		t.Fatalf("unable to make temp dir: %v", err)
	}
	defer os.RemoveAll(tempPath)
	db, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatalf("unable to open db: %v", err)
	}
	defer db.Close()

	// With the DB created, we'll make a new channel, and mark it as
	// pending open within the database.
	newChannel, _, cleanup, err := lnwallet.CreateTestChannels(
		channeldb.SingleFunderTweaklessBit,
	)
	if err != nil {
		t.Fatalf("unable to make new test channel: %v", err)
	}
	defer cleanup()
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
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		},
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			return nil
		},
		Clock: clock.NewDefaultClock(),
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)
	if err := chainArb.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := chainArb.Stop(); err != nil {
			t.Fatal(err)
		}
	}()

	channelArb := chainArb.activeChannels[channel.FundingOutpoint]

	// While the resolver are active, we'll now remove the channel from the
	// database (mark is as closed).
	err = db.ChannelStateDB().AbandonChannel(&channel.FundingOutpoint, 4)
	if err != nil {
		t.Fatalf("unable to remove channel: %v", err)
	}

	// With the channel removed, we'll now manually call ResolveContract.
	// This stimulates needing to remove a channel from the chain arb due
	// to any possible external consistency issues.
	err = chainArb.ResolveContract(channel.FundingOutpoint)
	if err != nil {
		t.Fatalf("unable to resolve contract: %v", err)
	}

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
	if err != nil {
		t.Fatalf("second resolve call shouldn't fail: %v", err)
	}
}
