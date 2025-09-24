package graphdb

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// channelRecord captures the metadata we need to issue follow-up operations on
// an inserted channel.
type channelRecord struct {
	id        uint64
	shortID   lnwire.ShortChannelID
	outPoint  wire.OutPoint
	blockHash chainhash.Hash
}

// channelFixture bundles a freshly generated ChannelEdgeInfo with its
// associated record before it is inserted into the graph.
type channelFixture struct {
	edge   *models.ChannelEdgeInfo
	record channelRecord
}

// operation holds a lazily generated ChannelGraph call so the Rapid property
// can choose which action to execute at the last possible moment.
type operation struct {
	name     string
	generate func(rt *rapid.T) operationCall
}

// operationCall carries the runnable form of a ChannelGraph call alongside a
// human-readable label for debugging shrunk counterexamples.
type operationCall struct {
	name string
	call func()
}

// channelIDManager issues unique channel IDs, keeps their metadata, and offers
// random access to existing channels so operations interact with realistic
// targets.
type channelIDManager struct {
	mu      sync.Mutex
	ids     []uint64
	records map[uint64]channelRecord
	next    uint64
	rng     *rand.Rand
}

// newChannelIDManager seeds the ID allocator so the property run remains fully
// deterministic.
func newChannelIDManager(seed int64) *channelIDManager {
	return &channelIDManager{
		records: make(map[uint64]channelRecord),
		next:    1,
		rng:     rand.New(rand.NewSource(seed)),
	}
}

// NewFixture creates a unique channel fixture that callers can attempt to
// insert via AddChannelEdge.
func (m *channelIDManager) NewFixture(rt *rapid.T) channelFixture {
	m.mu.Lock()
	id := m.next
	m.next++
	m.mu.Unlock()

	shortID := lnwire.NewShortChanIDFromInt(id)

	var txHash chainhash.Hash
	copy(
		txHash[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			rt, fmt.Sprintf("tx_hash_%d", id),
		),
	)

	var blockHash chainhash.Hash
	copy(
		blockHash[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			rt, fmt.Sprintf("block_hash_%d", id),
		),
	)

	outPoint := wire.OutPoint{
		Hash:  txHash,
		Index: rapid.Uint32().Draw(rt, fmt.Sprintf("tx_index_%d", id)),
	}

	record := channelRecord{
		id:        id,
		shortID:   shortID,
		outPoint:  outPoint,
		blockHash: blockHash,
	}

	edge := newEdgeInfo(rt, record)

	return channelFixture{
		edge:   edge,
		record: record,
	}
}

// Register persists the record of a successfully inserted channel so future
// operations can reuse it.
func (m *channelIDManager) Register(record channelRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.records[record.id]; ok {
		return
	}

	m.records[record.id] = record
	m.ids = append(m.ids, record.id)
}

// RandomRecord returns a previously registered channel when one exists.
func (m *channelIDManager) RandomRecord() (channelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.ids) == 0 {
		return channelRecord{}, false
	}
	id := m.ids[m.rng.Intn(len(m.ids))]

	return m.records[id], true
}

// newEdgeInfo fabricates a minimal ChannelEdgeInfo that exercises the public
// AddChannelEdge path without reaching into unexported helpers.
func newEdgeInfo(rt *rapid.T, record channelRecord) *models.ChannelEdgeInfo {
	node1 := randomCompressedKey(rt, fmt.Sprintf("node1_%d", record.id))
	node2 := randomCompressedKey(rt, fmt.Sprintf("node2_%d", record.id))
	btc1 := randomCompressedKey(rt, fmt.Sprintf("btc1_%d", record.id))
	btc2 := randomCompressedKey(rt, fmt.Sprintf("btc2_%d", record.id))

	return &models.ChannelEdgeInfo{
		ChannelID:        record.id,
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: btc1,
		BitcoinKey2Bytes: btc2,
		Features:         lnwire.EmptyFeatureVector(),
		ChannelPoint:     record.outPoint,
		Capacity: btcutil.Amount(
			rapid.Int64Range(1, 1_000_000).Draw(
				rt, fmt.Sprintf("capacity_%d", record.id),
			),
		),
	}
}

// randomCompressedKey samples a compressed secp256k1 public key, retrying until
// the curve library accepts the generated secret.
func randomCompressedKey(rt *rapid.T, label string) [33]byte {
	for {
		privBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			rt, label,
		)
		_, pubKey := btcec.PrivKeyFromBytes(privBytes)
		if pubKey == nil {
			continue
		}

		var out [33]byte
		copy(out[:], pubKey.SerializeCompressed())
		return out
	}
}

// newAddChannelOp constructs operations that add channels through the exported
// ChannelGraph API, ensuring the property works solely with public surface
// area.
func newAddChannelOp(graph *ChannelGraph, mgr *channelIDManager, ctx context.Context) operation {
	return operation{
		name: "AddChannel",
		generate: func(rt *rapid.T) operationCall {
			fixture := mgr.NewFixture(rt)
			return operationCall{
				name: "AddChannel",
				call: func() {
					err := graph.AddChannelEdge(
						ctx, fixture.edge,
					)
					if err == nil {
						mgr.Register(fixture.record)
					}
				},
			}
		},
	}
}

// newPruneGraphOp exercises pruning with randomized block metadata to imitate
// how the real graph reacts to confirmed closures.
func newPruneGraphOp(graph *ChannelGraph, mgr *channelIDManager) operation {
	return operation{
		name: "PruneGraph",
		generate: func(rt *rapid.T) operationCall {
			record, haveRecord := mgr.RandomRecord()
			useExisting := haveRecord && rapid.Bool().Draw(
				rt, "prune_use_existing",
			)

			var (
				spentOutputs []*wire.OutPoint
				blockHash    chainhash.Hash
				blockHeight  uint32
			)

			if useExisting {
				local := record
				spentOutputs = []*wire.OutPoint{
					{
						Hash:  local.outPoint.Hash,
						Index: local.outPoint.Index,
					},
				}
				blockHash = local.blockHash
				blockHeight = local.shortID.BlockHeight
			} else {
				n := rapid.IntRange(0, 3).Draw(
					rt, "spent_outputs",
				)

				spentOutputs = make([]*wire.OutPoint, n)
				for i := 0; i < n; i++ {
					hashBytes := rapid.SliceOfN(
						rapid.Byte(), 32, 32,
					).Draw(
						rt,
						fmt.Sprintf("spent_hash_%d", i),
					)

					var hash chainhash.Hash
					copy(hash[:], hashBytes)

					spentOutputs[i] = &wire.OutPoint{
						Hash: hash,
						Index: rapid.Uint32().Draw(
							rt, fmt.Sprintf(
								"spent_idx_%d",
								i,
							),
						),
					}
				}

				copy(
					blockHash[:],
					rapid.SliceOfN(
						rapid.Byte(), 32, 32,
					).Draw(rt, "rand_block_hash"),
				)

				blockHeight = rapid.Uint32().Draw(
					rt, "rand_block_height",
				)
			}

			return operationCall{
				name: "PruneGraph",
				call: func() {
					_, _ = graph.PruneGraph(
						spentOutputs, &blockHash,
						blockHeight,
					)
				},
			}
		},
	}
}

// newDisconnectBlockOp rewinds the graph to a random height to mimic chain
// reorg handling.
func newDisconnectBlockOp(graph *ChannelGraph, mgr *channelIDManager) operation {
	return operation{
		name: "DisconnectBlock",
		generate: func(rt *rapid.T) operationCall {
			record, haveRecord := mgr.RandomRecord()
			useExisting := haveRecord && rapid.Bool().Draw(
				rt, "disconnect_use_existing",
			)

			var height uint32
			if useExisting {
				height = record.shortID.BlockHeight
			} else {
				height = rapid.Uint32().Draw(
					rt, "disconnect_height",
				)
			}

			return operationCall{
				name: "DisconnectBlock",
				call: func() {
					_, _ = graph.DisconnectBlockAtHeight(
						height,
					)
				},
			}
		},
	}
}

// newDeleteChannelEdgesOp removes or zombifies channels, favouring IDs we
// actually added so cache state stays realistic.
func newDeleteChannelEdgesOp(graph *ChannelGraph,
	mgr *channelIDManager) operation {

	return operation{
		name: "DeleteChannelEdges",
		generate: func(rt *rapid.T) operationCall {
			numIDs := rapid.IntRange(1, 3).Draw(rt, "delete_ids")

			chanIDs := make([]uint64, numIDs)
			for i := 0; i < numIDs; i++ {
				if rec, ok := mgr.RandomRecord(); ok &&
					rapid.Bool().Draw(rt, fmt.Sprintf(
						"delete_use_existing_%d", i),
					) {

					chanIDs[i] = rec.id
					continue
				}

				chanIDs[i] = rapid.Uint64().Draw(
					rt, fmt.Sprintf("rand_chan_%d", i),
				)
			}

			strictZombie := rapid.Bool().Draw(rt, "strict_zombie")
			markZombie := rapid.Bool().Draw(rt, "mark_zombie")
			return operationCall{
				name: "DeleteChannelEdges",
				call: func() {
					_ = graph.DeleteChannelEdges(
						strictZombie, markZombie,
						chanIDs...,
					)
				},
			}
		},
	}
}

// newChanUpdatesInHorizonOp fires horizon queries to stress read paths while
// write-heavy operations run in parallel.
func newChanUpdatesInHorizonOp(graph *ChannelGraph) operation {
	return operation{
		name: "ChanUpdatesInHorizon",
		generate: func(rt *rapid.T) operationCall {
			start := time.Unix(
				int64(rapid.Int64Range(-1000, 0).Draw(
					rt, "start_time"),
				), 0,
			)
			delta := time.Duration(
				rapid.Int64Range(0, 1000).Draw(
					rt, "delta_secs",
				),
			) * time.Second

			end := start.Add(delta)

			return operationCall{
				name: "ChanUpdatesInHorizon",
				call: func() {
					_, _ = graph.ChanUpdatesInHorizon(
						start, end,
					)
				},
			}
		},
	}
}

// newFilterKnownChanIDsOp sends lookups with randomized gossip state so the
// reject cache is exercised under contention.
func newFilterKnownChanIDsOp(graph *ChannelGraph, mgr *channelIDManager) operation {
	return operation{
		name: "FilterKnownChanIDs",
		generate: func(rt *rapid.T) operationCall {
			numInfos := rapid.IntRange(0, 5).Draw(
				rt, "num_chan_infos",
			)

			infos := make([]ChannelUpdateInfo, numInfos)
			for i := 0; i < numInfos; i++ {
				scid := func() lnwire.ShortChannelID {
					if rec, ok := mgr.RandomRecord(); ok && rapid.Bool().Draw(rt, fmt.Sprintf("filter_use_existing_%d", i)) {
						return rec.shortID
					}

					return lnwire.ShortChannelID{
						BlockHeight: rapid.Uint32Range(
							0, 1<<23).Draw(
							rt, fmt.Sprintf(
								"block_%d", i,
							),
						),
						TxIndex: rapid.Uint32Range(
							0, 1<<23).Draw(
							rt, fmt.Sprintf(
								"txindex_%d", i,
							),
						),
						TxPosition: rapid.Uint16().Draw(
							rt, fmt.Sprintf(
								"txpos_%d", i,
							),
						),
					}
				}()

				node1Ts := time.Unix(
					int64(rapid.Int64Range(-1000, 1000).Draw(
						rt, fmt.Sprintf(
							"node1_ts_%d", i,
						),
					)), 0,
				)
				node2Ts := time.Unix(
					int64(rapid.Int64Range(-1000, 1000).Draw(
						rt, fmt.Sprintf(
							"node2_ts_%d", i,
						),
					)), 0,
				)
				infos[i] = NewChannelUpdateInfo(
					scid, node1Ts, node2Ts,
				)
			}

			return operationCall{
				name: "FilterKnownChanIDs",
				call: func() {
					_, _ = graph.FilterKnownChanIDs(
						infos,
						func(time.Time,
							time.Time) bool {

							return false
						},
					)
				},
			}
		},
	}
}

// newMarkEdgeZombieOp pushes channels into the zombie index, preferring actual
// IDs when available.
func newMarkEdgeZombieOp(graph *ChannelGraph, mgr *channelIDManager) operation {
	return operation{
		name: "MarkEdgeZombie",
		generate: func(rt *rapid.T) operationCall {
			record, haveRecord := mgr.RandomRecord()
			chanID := rapid.Uint64().Draw(rt, "zombie_chan")
			if haveRecord && rapid.Bool().Draw(
				rt, "use_existing_zombie",
			) {
				chanID = record.id
			}

			var pub1, pub2 [33]byte
			copy(
				pub1[:], rapid.SliceOfN(
					rapid.Byte(), 33, 33,
				).Draw(rt, "zombie_pub1"),
			)
			copy(
				pub2[:], rapid.SliceOfN(
					rapid.Byte(), 33, 33,
				).Draw(rt, "zombie_pub2"),
			)

			return operationCall{
				name: "MarkEdgeZombie",
				call: func() {
					_ = graph.MarkEdgeZombie(
						chanID, pub1, pub2,
					)
				},
			}
		},
	}
}

// newMarkEdgeLiveOp brings channels back from the zombie set to complete the
// lifecycle covered by the property.
func newMarkEdgeLiveOp(graph *ChannelGraph, mgr *channelIDManager) operation {
	return operation{
		name: "MarkEdgeLive",
		generate: func(rt *rapid.T) operationCall {
			record, haveRecord := mgr.RandomRecord()
			chanID := rapid.Uint64().Draw(rt, "live_chan")

			if haveRecord && rapid.Bool().Draw(
				rt, "use_existing_live",
			) {
				chanID = record.id
			}

			return operationCall{
				name: "MarkEdgeLive",
				call: func() {
					_ = graph.MarkEdgeLive(chanID)
				},
			}
		},
	}
}

// TestStoreCacheConcurrentAccess verifies that the cache mutex behaves under
// contention by repeatedly hitting the ChannelGraph through its public API from
// multiple goroutines.
func TestStoreCacheConcurrentAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cache concurrency property in short mode")
	}

	rapid.Check(t, func(rt *rapid.T) {
		store := NewTestDB(t)
		graph, err := NewChannelGraph(store)
		require.NoError(t, err)
		require.NoError(t, graph.Start())
		ctx := context.Background()

		t.Cleanup(func() {
			require.NoError(t, graph.Stop())
		})

		seed := int64(rapid.Int64().Draw(rt, "seed"))
		mgr := newChannelIDManager(seed)

		// Load in some initial channels into the database.
		initialChannels := rapid.IntRange(50, 200).Draw(
			rt, "initial_channels",
		)
		for i := 0; i < initialChannels; i++ {
			fixture := mgr.NewFixture(rt)
			require.NoError(t, store.AddChannelEdge(
				ctx, fixture.edge),
			)
			mgr.Register(fixture.record)
		}

		// opGenerators keeps the menu of ChannelGraph actions the
		// property can mix while the goroutines race.
		opGenerators := []operation{
			newAddChannelOp(graph, mgr, ctx),
			newPruneGraphOp(graph, mgr),
			newDisconnectBlockOp(graph, mgr),
			newDeleteChannelEdgesOp(graph, mgr),
			newChanUpdatesInHorizonOp(graph),
			newFilterKnownChanIDsOp(graph, mgr),
			newMarkEdgeZombieOp(graph, mgr),
			newMarkEdgeLiveOp(graph, mgr),
		}

		numWorkers := rapid.IntRange(1, 10).Draw(rt, "workers")
		opsPerWorker := rapid.IntRange(1, 50).Draw(rt, "ops_per_worker")

		type workerPlan []operationCall

		// workerPlan captures the operations assigned to a single
		// goroutine for the duration of one property iteration.
		plans := make([]workerPlan, numWorkers)
		for w := 0; w < numWorkers; w++ {
			opSeq := make(workerPlan, opsPerWorker)
			for i := 0; i < opsPerWorker; i++ {
				idx := rapid.IntRange(
					0, len(opGenerators)-1).Draw(
					rt, fmt.Sprintf("op_%d_%d", w, i),
				)
				opSeq[i] = opGenerators[idx].generate(rt)
			}

			plans[w] = opSeq
		}

		var wg sync.WaitGroup
		for _, plan := range plans {
			wg.Add(1)
			go func(seq workerPlan) {
				defer wg.Done()
				for _, call := range seq {
					call.call()
				}
			}(plan)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		}
	})
}
