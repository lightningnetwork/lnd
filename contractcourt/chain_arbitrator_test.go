package contractcourt

import (
	"io/ioutil"
	"net"
	"os"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// TestChainArbitratorRepulishCommitment testst that the chain arbitrator will
// republish closing transactions for channels marked CommitementBroadcast in
// the database at startup.
func TestChainArbitratorRepublishCommitment(t *testing.T) {
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
		lChannel, _, cleanup, err := lnwallet.CreateTestChannels(true)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		channel := lChannel.State()

		// We manually set the db here to make sure all channels are
		// synced to the same db.
		channel.Db = db

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
		err := channels[i].MarkCommitmentBroadcasted(closeTx)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We keep track of the transactions published by the ChainArbitrator
	// at startup.
	published := make(map[chainhash.Hash]struct{})

	chainArbCfg := ChainArbitratorConfig{
		ChainIO:  &mockChainIO{},
		Notifier: &mockNotifier{},
		PublishTx: func(tx *wire.MsgTx) error {
			published[tx.TxHash()] = struct{}{}
			return nil
		},
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

		_, ok := published[closeTx.TxHash()]
		if !ok {
			t.Fatalf("closing tx not re-published")
		}

		delete(published, closeTx.TxHash())
	}

	if len(published) != 0 {
		t.Fatalf("unexpected tx published")
	}
}
