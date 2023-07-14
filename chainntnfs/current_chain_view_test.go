package chainntnfs_test

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntest/mock"
)

type blockEpoch chainntnfs.BlockEpoch

func (blockEpoch) Generate(r *rand.Rand, size int) reflect.Value {
	var chainHash, prevBlockHash, merkleRootHash chainhash.Hash
	r.Read(chainHash[:])
	r.Read(prevBlockHash[:])
	r.Read(merkleRootHash[:])

	return reflect.ValueOf(blockEpoch(chainntnfs.BlockEpoch{
		Hash:   &chainHash,
		Height: r.Int31n(1000000),
		BlockHeader: &wire.BlockHeader{
			Version:    2,
			PrevBlock:  prevBlockHash,
			MerkleRoot: merkleRootHash,
			Timestamp:  time.Now(),
			Bits:       r.Uint32(),
			Nonce:      r.Uint32(),
		},
	}))
}

func TestCurrentChainStateTracker(t *testing.T) {
	t.Parallel()

	notifier := &mock.ChainNotifier{
		SpendChan: nil,
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  nil,
	}

	chainNotifierI := chainntnfs.ChainNotifier(notifier)

	tracker := chainntnfs.NewChainStateTracker(&chainNotifierI)
	if tracker.Start() != nil {
		t.Fatalf("CurrentChainStateTacker could not be started")
	}

	correctness := func(epochRand blockEpoch) bool {
		epoch := chainntnfs.BlockEpoch(epochRand)
		notifier.EpochChan <- &epoch
		// sleep to allow new block to propagate
		time.Sleep(time.Millisecond)
		height := tracker.BestHeight()
		header := tracker.BestBlockHeader()

		return height == uint32(epoch.Height) &&
			header == epoch.BlockHeader
	}
	idempotence := func(epochRand blockEpoch) bool {
		epoch := chainntnfs.BlockEpoch(epochRand)
		notifier.EpochChan <- &epoch
		// sleep to allow new block to propagate
		time.Sleep(time.Millisecond)
		height0 := tracker.BestHeight()
		height1 := tracker.BestHeight()
		header0 := tracker.BestBlockHeader()
		header1 := tracker.BestBlockHeader()

		return height0 == height1 && header0 == header1
	}
	if err := quick.Check(correctness, nil); err != nil {
		t.Fatalf(
			"CurrentChainStateTracker does not give up to date "+
				"info: %v", err,
		)
	}
	if err := quick.Check(idempotence, nil); err != nil {
		t.Fatalf("CurrentChainStateTracker is not idempotent")
	}

	if tracker.Stop() != nil {
		t.Fatalf("CurrentChainState could not be stopped")
	}
}
