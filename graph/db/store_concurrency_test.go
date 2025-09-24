package graphdb

import (
	"context"
	"flag"
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

// channelIDManager issues unique channel IDs and remembers them so later
// operations can pick from realistic targets instead of always inventing new
// ones.
type channelIDManager struct {
	mu   sync.Mutex
	ids  []uint64
	next uint64
	rng  *rand.Rand
}

// newChannelIDManager seeds the ID allocator so the property run remains fully
// deterministic.
func newChannelIDManager(seed int64) *channelIDManager {
	return &channelIDManager{
		next: 1,
		rng:  rand.New(rand.NewSource(seed)),
	}
}

// NextID hands out the next unused channel ID so AddChannelEdge calls can
// create edges without clashing.
func (m *channelIDManager) NextID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	return id
}

// Register records a channel ID, allowing later delete or zombie operations to
// reuse it.
func (m *channelIDManager) Register(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids = append(m.ids, id)
}

// RandomExisting returns a previously registered ID when one exists.
func (m *channelIDManager) RandomExisting() (uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.ids) == 0 {
		return 0, false
	}
	idx := m.rng.Intn(len(m.ids))
	return m.ids[idx], true
}

// newRandomEdgeInfo fabricates a minimal ChannelEdgeInfo that exercises the
// public AddChannelEdge path without reaching into unexported helpers.
func newRandomEdgeInfo(rt *rapid.T, chanID uint64) *models.ChannelEdgeInfo {
	node1 := randomCompressedKey(rt, "node1")
	node2 := randomCompressedKey(rt, "node2")
	btc1 := randomCompressedKey(rt, "btc1")
	btc2 := randomCompressedKey(rt, "btc2")

	var txHash chainhash.Hash
	copy(txHash[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "tx_hash"))

	return &models.ChannelEdgeInfo{
		ChannelID:        chanID,
		ChainHash:        *chaincfg.MainNetParams.GenesisHash,
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: btc1,
		BitcoinKey2Bytes: btc2,
		Features:         lnwire.EmptyFeatureVector(),
		ChannelPoint: wire.OutPoint{
			Hash:  txHash,
			Index: rapid.Uint32().Draw(rt, "tx_index"),
		},
		Capacity: btcutil.Amount(
			rapid.Int64Range(1, 1_000_000).Draw(rt, "capacity"),
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
			chanID := mgr.NextID()
			edge := newRandomEdgeInfo(rt, chanID)
			return operationCall{
				name: "AddChannel",
				call: func() {
					if err := graph.AddChannelEdge(ctx, edge); err == nil {
						mgr.Register(chanID)
					}
				},
			}
		},
	}
}

// newPruneGraphOp exercises pruning with randomized block metadata to imitate
// how the real graph reacts to confirmed closures.
func newPruneGraphOp(graph *ChannelGraph) operation {
	return operation{
		name: "PruneGraph",
		generate: func(rt *rapid.T) operationCall {
			n := rapid.IntRange(0, 3).Draw(rt, "spent_outputs")
			spentOutputs := make([]*wire.OutPoint, n)
			for i := 0; i < n; i++ {
				hashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, fmt.Sprintf("spent_hash_%d", i))
				var hash chainhash.Hash
				copy(hash[:], hashBytes)
				spentOutputs[i] = &wire.OutPoint{
					Hash:  hash,
					Index: rapid.Uint32().Draw(rt, fmt.Sprintf("spent_idx_%d", i)),
				}
			}

			blockHashBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "block_hash")
			var blockHash chainhash.Hash
			copy(blockHash[:], blockHashBytes)

			blockHeight := rapid.Uint32().Draw(rt, "block_height")
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
func newDisconnectBlockOp(graph *ChannelGraph) operation {
	return operation{
		name: "DisconnectBlock",
		generate: func(rt *rapid.T) operationCall {
			height := rapid.Uint32().Draw(rt, "disconnect_height")
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
				if id, ok := mgr.RandomExisting(); ok {
					chanIDs[i] = id
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
func newFilterKnownChanIDsOp(graph *ChannelGraph) operation {
	return operation{
		name: "FilterKnownChanIDs",
		generate: func(rt *rapid.T) operationCall {
			numInfos := rapid.IntRange(0, 5).Draw(
				rt, "num_chan_infos",
			)

			infos := make([]ChannelUpdateInfo, numInfos)
			for i := 0; i < numInfos; i++ {
				scid := lnwire.ShortChannelID{
					BlockHeight: rapid.Uint32Range(0, 1<<23).Draw(rt, fmt.Sprintf("block_%d", i)),
					TxIndex:     rapid.Uint32Range(0, 1<<23).Draw(rt, fmt.Sprintf("txindex_%d", i)),
					TxPosition:  rapid.Uint16().Draw(rt, fmt.Sprintf("txpos_%d", i)),
				}

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
			chanID := rapid.Uint64().Draw(rt, "zombie_chan")
			if id, ok := mgr.RandomExisting(); ok && rapid.Bool().Draw(rt, "use_existing_zombie") {
				chanID = id
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
			chanID := rapid.Uint64().Draw(rt, "live_chan")
			if id, ok := mgr.RandomExisting(); ok && rapid.Bool().Draw(rt, "use_existing_live") {
				chanID = id
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
	if err := flag.Set("rapid.checks", "10"); err != nil {
		t.Fatalf("unable to configure rapid checks: %v", err)
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
			chanID := mgr.NextID()
			edge := newRandomEdgeInfo(rt, chanID)
			require.NoError(t, store.AddChannelEdge(ctx, edge))
			mgr.Register(chanID)
		}

		// opGenerators keeps the menu of ChannelGraph actions the
		// property can mix while the goroutines race.
		opGenerators := []operation{
			newAddChannelOp(graph, mgr, ctx),
			newPruneGraphOp(graph),
			newDisconnectBlockOp(graph),
			newDeleteChannelEdgesOp(graph, mgr),
			newChanUpdatesInHorizonOp(graph),
			newFilterKnownChanIDsOp(graph),
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
