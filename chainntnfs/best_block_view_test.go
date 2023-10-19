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
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
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

// TestBestBlockTracker ensures that the most recent event pushed on the
// underlying EpochChan is remembered by the BestBlockView functions as well
// as testing the idempotence of the BestBlockView interface.
func TestBestBlockTracker(t *testing.T) {
	t.Parallel()

	notifier := &mock.ChainNotifier{
		SpendChan: nil,
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  nil,
	}

	chainNotifierI := chainntnfs.ChainNotifier(notifier)

	tracker := chainntnfs.NewBestBlockTracker(chainNotifierI)
	require.Nil(t, tracker.Start(),
		"BestBlockTacker could not be started")

	// we have to limit test cases because the poll interval of
	// wait.Predicate isn't tight enough to support the usual 100
	cfg := quick.Config{MaxCount: 50}
	correctness := func(epochRand blockEpoch) bool {
		epoch := chainntnfs.BlockEpoch(epochRand)
		notifier.EpochChan <- &epoch

		// wait for new block to propagate
		err := wait.Predicate(
			func() bool {
				_, err := tracker.BestHeight()
				return err == nil
			},
			1*time.Second,
		)
		require.Nil(t, err,
			"BestBlockTracker: block propagation timeout")

		height, _ := tracker.BestHeight()
		header, _ := tracker.BestBlockHeader()

		return height == uint32(epoch.Height) &&
			header == epoch.BlockHeader
	}
	idempotence := func(epochRand blockEpoch) bool {
		epoch := chainntnfs.BlockEpoch(epochRand)
		notifier.EpochChan <- &epoch

		// wait for new block to propagate
		err := wait.Predicate(
			func() bool {
				_, err := tracker.BestHeight()
				return err == nil
			},
			1*time.Second,
		)
		require.Nil(t, err,
			"ChainStateTracker: block propagation timeout")

		height0, _ := tracker.BestHeight()
		height1, _ := tracker.BestHeight()
		header0, _ := tracker.BestBlockHeader()
		header1, _ := tracker.BestBlockHeader()

		return height0 == height1 && header0 == header1
	}
	err := quick.Check(correctness, &cfg)
	require.Nil(t, err,
		"ChainStateTracker does not give up to date info: %v", err)

	require.Nil(t, quick.Check(idempotence, &cfg),
		"ChainStateTracker is not idempotent")

	require.Nil(t, tracker.Stop(), "ChainStateTracker could not be stopped")
}
