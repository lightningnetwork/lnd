//go:build test_db_postgres || test_db_sqlite

package migration1

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"math"
	prand "math/rand"
	"net"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var (
	testPub = route.Vertex{2, 202, 4}

	testRBytes, _ = hex.DecodeString(
		"8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf03" +
			"14f882d7",
	)
	testSBytes, _ = hex.DecodeString(
		"299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a2" +
			"92d24ae",
	)
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)

	testChain         = *chaincfg.MainNetParams.GenesisHash
	testColor         = color.RGBA{R: 1, G: 2, B: 3}
	testTime          = time.Unix(11111, 0)
	testSigBytes      = testSig.Serialize()
	testExtraData     = []byte{1, 1, 1, 2, 2, 2, 2}
	testEmptyFeatures = lnwire.EmptyFeatureVector()
	testAuthProof     = &models.ChannelAuthProof{
		NodeSig1Bytes:    testSig.Serialize(),
		NodeSig2Bytes:    testSig.Serialize(),
		BitcoinSig1Bytes: testSig.Serialize(),
		BitcoinSig2Bytes: testSig.Serialize(),
	}

	// testOpaqueAddrWithEmbeddedDNSAddr is an opaque address that contains
	// a single DNS address within it.
	testOpaqueAddrWithEmbeddedDNSAddr = &lnwire.OpaqueAddrs{
		Payload: []byte{
			// The protocol level type for DNS addresses.
			0x05,
			// Hostname length: 11.
			0x0b,
			// The hostname itself.
			'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			// Port 8080 in big-endian.
			0x1f, 0x90,
		},
	}

	// testOpaqueAddrWithEmbeddedDNSAddrAndMore is an opaque address that
	// contains a DNS address within it, along with some extra bytes that
	// represent some other unknown address type.
	testOpaqueAddrWithEmbeddedDNSAddrAndMore = &lnwire.OpaqueAddrs{
		Payload: []byte{
			// The protocol level type for DNS addresses.
			0x05,
			// Hostname length: 11.
			0x0B,
			// The hostname itself.
			'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			// port 8080 in big-endian.
			0x1F, 0x90,
			// Now we add more opaque bytes to represent more
			// addresses that we don't know about yet.
			// NOTE: the 0xff is an address type that we definitely
			// don't know about yet
			0xff, 0x02, 0x03, 0x04, 0x05, 0x06,
		},
	}

	// testOpaqueAddrWithEmbeddedBadDNSAddr is an opaque address that
	// contains an invalid DNS address within it.
	testOpaqueAddrWithEmbeddedBadDNSAddr = &lnwire.OpaqueAddrs{
		Payload: []byte{
			// The protocol level type for DNS addresses.
			0x05,
			// Hostname length: We set this to a size that is
			// incorrect in order to simulate the bad DNS address.
			0xAA,
			// The hostname itself.
			'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			// port 9735 in big-endian.
			0x26, 0x07,
		},
	}

	// testOpaqueAddrWithTwoEmbeddedDNSAddrs is an opaque address that
	// contains two valid DNS addresses within it.
	testOpaqueAddrWithTwoEmbeddedDNSAddrs = &lnwire.OpaqueAddrs{
		Payload: []byte{
			// The protocol level type for DNS addresses.
			0x05,
			// Hostname length: 11.
			0x0B,
			// The hostname itself.
			'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			// port 8080 in big-endian.
			0x1F, 0x90,
			// Another DNS address.
			0x05,
			0x0B,
			'e', 'x', 'a', 'm', 'p', 'l', 'e', '.', 'c', 'o', 'm',
			0x1F, 0x90,
		},
	}

	testIP4 = net.ParseIP("192.168.1.1").To4()
	testIP6 = net.ParseIP("2001:0db8:0000:0000:0000:ff00:0042:8329")

	testIPV4Addr = &net.TCPAddr{
		IP:   testIP4,
		Port: 12345,
	}

	testIPV6Addr = &net.TCPAddr{
		IP:   testIP6,
		Port: 65535,
	}

	testOnionV2Addr = &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9735,
	}

	testOnionV3Addr = &tor.OnionAddr{
		OnionService: "vww6ybal4bd7szmgncyruucpgfkqahzddi37ktceo3ah7ngmcopnpyyd.onion", //nolint:ll
		Port:         80,
	}

	testOpaqueAddr = &lnwire.OpaqueAddrs{
		// NOTE: the first byte is a protocol level address type. So
		// for we set it to 0xff to guarantee that we do not know this
		// type yet.
		Payload: []byte{0xff, 0x02, 0x03, 0x04, 0x05, 0x06},
	}

	testDNSAddr = &lnwire.DNSAddress{
		Hostname: "example.com",
		Port:     8080,
	}

	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr, anotherAddr}

	testFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.GossipQueriesRequired),
		lnwire.Features,
	)

	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
)

// TestMigrateGraphToSQL tests various deterministic cases that we want to test
// for to ensure that our migration from a graph store backed by a KV DB to a
// SQL database works as expected. At the end of each test, the DBs are compared
// and expected to have the exact same data in them.
// This test also ensures that the migration is "retry-safe". This is needed
// because the migration is hooked up to 2 dbs: the source DB and the
// destination. The source DB is a db behind the kvdb.Backend interface and
// the migration makes use of methods on this interface that may be retried
// under the hood. The migration often does logic inside call-back functions
// passed to the source DB methods which may be retried, and so we need to
// ensure that the migration can handle this.
func TestMigrateGraphToSQL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dbFixture := NewTestDBFixture(t)

	writeUpdate := func(t *testing.T, db *KVStore, object any) {
		t.Helper()

		var err error
		switch obj := object.(type) {
		case *models.Node:
			err = db.AddNode(ctx, obj)
		case *models.ChannelEdgeInfo:
			err = db.AddChannelEdge(ctx, obj)
		case *models.ChannelEdgePolicy:
			_, _, err = db.UpdateEdgePolicy(ctx, obj)
		default:
			err = fmt.Errorf("unhandled object type: %T", obj)
		}
		require.NoError(t, err)
	}

	var (
		chanID1 = prand.Uint64()
		chanID2 = prand.Uint64()

		node1 = genPubKey(t)
		node2 = genPubKey(t)
	)

	type zombieIndexObject struct {
		scid    uint64
		pubKey1 route.Vertex
		pubKey2 route.Vertex
	}

	tests := []struct {
		name          string
		write         func(t *testing.T, db *KVStore, object any)
		objects       []any
		expGraphStats graphStats
	}{
		{
			name: "empty",
		},
		{
			name:  "nodes",
			write: writeUpdate,
			//nolint:ll
			objects: []any{
				// Normal node with all fields.
				makeTestNode(t),
				// A node with no node announcement.
				makeTestShellNode(t),
				// A node with an announcement but no addresses.
				makeTestNode(t, func(n *models.Node) {
					n.Addresses = nil
				}),
				// A node with all types of addresses.
				makeTestNode(t, func(n *models.Node) {
					n.Addresses = []net.Addr{
						testAddr,
						testIPV4Addr,
						testIPV6Addr,
						anotherAddr,
						testOnionV2Addr,
						testOnionV3Addr,
						testOpaqueAddr,
					}
				}),
				// No extra opaque data.
				makeTestNode(t, func(n *models.Node) {
					n.ExtraOpaqueData = nil
				}),
				// A node with no features.
				makeTestNode(t, func(n *models.Node) {
					n.Features = lnwire.EmptyFeatureVector()
				}),
			},
			expGraphStats: graphStats{
				numNodes: 6,
			},
		},
		{
			name: "source node",
			write: func(t *testing.T, db *KVStore, object any) {
				node, ok := object.(*models.Node)
				require.True(t, ok)

				err := db.SetSourceNode(ctx, node)
				require.NoError(t, err)
			},
			objects: []any{
				makeTestNode(t),
			},
			expGraphStats: graphStats{
				numNodes:   1,
				srcNodeSet: true,
			},
		},
		{
			name:  "channel with no policies",
			write: writeUpdate,
			objects: []any{
				// A channel with unknown nodes. This will
				// result in two shell nodes being created.
				// 	- channel count += 1
				// 	- node count += 2
				makeTestChannel(t),

				// Insert some nodes.
				// 	- node count += 1
				makeTestNode(t, func(n *models.Node) {
					n.PubKeyBytes = node1
				}),
				// 	- node count += 1
				makeTestNode(t, func(n *models.Node) {
					n.PubKeyBytes = node2
				}),

				// A channel with known nodes.
				// - channel count += 1
				makeTestChannel(
					t, func(c *models.ChannelEdgeInfo) {
						c.ChannelID = chanID1

						c.NodeKey1Bytes = node1
						c.NodeKey2Bytes = node2
					},
				),

				// Insert a channel with no auth proof, no
				// extra opaque data, and empty features.
				// Use known nodes.
				// 	- channel count += 1
				makeTestChannel(
					t, func(c *models.ChannelEdgeInfo) {
						c.ChannelID = chanID2

						c.NodeKey1Bytes = node1
						c.NodeKey2Bytes = node2

						c.AuthProof = nil
						c.ExtraOpaqueData = nil
						c.Features = testEmptyFeatures
					},
				),
			},
			expGraphStats: graphStats{
				numNodes:    4,
				numChannels: 3,
			},
		},
		{
			name:  "channels and policies",
			write: writeUpdate,
			objects: []any{
				// A channel with unknown nodes. This will
				// result in two shell nodes being created.
				// 	- channel count += 1
				// 	- node count += 2
				makeTestChannel(t),

				// Insert some nodes.
				// 	- node count += 1
				makeTestNode(t, func(n *models.Node) {
					n.PubKeyBytes = node1
				}),
				// 	- node count += 1
				makeTestNode(t, func(n *models.Node) {
					n.PubKeyBytes = node2
				}),

				// A channel with known nodes.
				// - channel count += 1
				makeTestChannel(
					t, func(c *models.ChannelEdgeInfo) {
						c.ChannelID = chanID1

						c.NodeKey1Bytes = node1
						c.NodeKey2Bytes = node2
					},
				),

				// Insert a channel with no auth proof, no
				// extra opaque data, and empty features.
				// Use known nodes.
				// 	- channel count += 1
				makeTestChannel(
					t, func(c *models.ChannelEdgeInfo) {
						c.ChannelID = chanID2

						c.NodeKey1Bytes = node1
						c.NodeKey2Bytes = node2

						c.AuthProof = nil
						c.ExtraOpaqueData = nil
						c.Features = testEmptyFeatures
					},
				),

				// Now, insert a single update for the
				// first channel.
				// 	- channel policy count += 1
				makeTestPolicy(chanID1, node1, false),

				// Insert two updates for the second
				// channel, one for each direction.
				// 	- channel policy count += 1
				makeTestPolicy(chanID2, node1, false),
				// This one also has no extra opaque data.
				// 	- channel policy count += 1
				makeTestPolicy(
					chanID2, node2, true,
					func(p *models.ChannelEdgePolicy) {
						p.ExtraOpaqueData = nil
					},
				),
			},
			expGraphStats: graphStats{
				numNodes:    4,
				numChannels: 3,
				numPolicies: 3,
			},
		},
		{
			name: "prune log",
			write: func(t *testing.T, db *KVStore, object any) {
				var hash chainhash.Hash
				_, err := rand.Read(hash[:])
				require.NoError(t, err)

				switch obj := object.(type) {
				case *models.Node:
					err = db.SetSourceNode(ctx, obj)
				default:
					height, ok := obj.(uint32)
					require.True(t, ok)

					_, _, err = db.PruneGraph(
						nil, &hash, height,
					)
				}
				require.NoError(t, err)
			},
			objects: []any{
				// The PruneGraph call requires that the source
				// node be set. So that is the first object
				// we will write.
				models.NewV1ShellNode(testPub),
				// Now we add some block heights to prune
				// the graph at.
				uint32(1), uint32(2), uint32(20), uint32(3),
				uint32(4),
			},
			expGraphStats: graphStats{
				numNodes:   1,
				srcNodeSet: true,
				pruneTip:   20,
			},
		},
		{
			name: "closed SCID index",
			write: func(t *testing.T, db *KVStore, object any) {
				scid, ok := object.(lnwire.ShortChannelID)
				require.True(t, ok)

				err := db.PutClosedScid(scid)
				require.NoError(t, err)
			},
			objects: []any{
				lnwire.NewShortChanIDFromInt(1),
				lnwire.NewShortChanIDFromInt(2),
				lnwire.NewShortChanIDFromInt(3),
				lnwire.NewShortChanIDFromInt(4),
			},
		},
		{
			name: "zombie index",
			write: func(t *testing.T, db *KVStore, object any) {
				obj, ok := object.(*zombieIndexObject)
				require.True(t, ok)

				err := db.MarkEdgeZombie(
					obj.scid, obj.pubKey1, obj.pubKey2,
				)
				require.NoError(t, err)
			},
			objects: []any{
				&zombieIndexObject{
					scid:    prand.Uint64(),
					pubKey1: genPubKey(t),
					pubKey2: genPubKey(t),
				},
				&zombieIndexObject{
					scid:    prand.Uint64(),
					pubKey1: genPubKey(t),
					pubKey2: genPubKey(t),
				},
				&zombieIndexObject{
					scid:    prand.Uint64(),
					pubKey1: genPubKey(t),
					pubKey2: genPubKey(t),
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Set up our source kvdb DB.
			kvDB := setUpKVStore(t)

			// Write the test objects to the kvdb store.
			for _, object := range test.objects {
				test.write(t, kvDB, object)
			}

			// Set up our destination SQL DB.
			db := NewTestDBWithFixture(t, dbFixture)
			sql, ok := db.(*SQLStore)
			require.True(t, ok)

			// Run the migration.
			err := MigrateGraphToSQL(ctx, sql.cfg, kvDB.db, sql.db)
			require.NoError(t, err)

			// Validate that the two databases are now in sync.
			assertInSync(t, kvDB, sql, test.expGraphStats)

			// The migration should be retry-safe, so running it
			// again should not change the state of the databases.
			err = MigrateGraphToSQL(ctx, sql.cfg, kvDB.db, sql.db)
			require.NoError(t, err)
			assertInSync(t, kvDB, sql, test.expGraphStats)
		})
	}
}

// graphStats holds expected statistics about the graph after migration.
type graphStats struct {
	numNodes    int
	srcNodeSet  bool
	numChannels int
	numPolicies int
	pruneTip    int
}

// assertInSync checks that the KVStore and SQLStore both contain the same
// graph data after migration.
func assertInSync(t *testing.T, kvDB *KVStore, sqlDB *SQLStore,
	stats graphStats) {

	// 1) Compare the nodes in the two stores.
	sqlNodes := fetchAllNodes(t, sqlDB)
	require.Len(t, sqlNodes, stats.numNodes)
	require.Equal(t, fetchAllNodes(t, kvDB), sqlNodes)

	// 2) Check that the source nodes match (if indeed source nodes have
	// been set).
	sqlSourceNode := fetchSourceNode(t, sqlDB)
	require.Equal(t, stats.srcNodeSet, sqlSourceNode != nil)
	require.Equal(t, fetchSourceNode(t, kvDB), sqlSourceNode)

	// 3) Compare the channels and policies in the two stores.
	sqlChannels := fetchAllChannelsAndPolicies(t, sqlDB)
	require.Len(t, sqlChannels, stats.numChannels)
	require.Equal(t, stats.numPolicies, sqlChannels.CountPolicies())
	require.Equal(t, fetchAllChannelsAndPolicies(t, kvDB), sqlChannels)

	// 4) Assert prune logs match. For this one, we iterate through the
	// prune log of the kvdb store and check that the entries match the
	// entries in the SQL store. Then we just do a final check to ensure
	// that the prune tip also matches.
	checkKVPruneLogEntries(t, kvDB, sqlDB, stats.pruneTip)

	// 5) Assert that the closed SCID index is also in sync. Like the prune
	// log we iterate through the kvdb store and check that the entries
	// match the entries in the SQL store.
	checkClosedSCIDIndex(t, kvDB.db, sqlDB)

	// 6) Finally, check that the zombie index is also in sync.
	checkZombieIndex(t, kvDB.db, sqlDB)
}

// fetchAllNodes retrieves all nodes from the given store and returns them
// sorted by their public key.
func fetchAllNodes(t *testing.T, store V1Store) []*models.Node {
	nodes := make([]*models.Node, 0)

	err := store.ForEachNode(t.Context(),
		func(node *models.Node) error {
			// Call PubKey to ensure the objects cached pubkey is
			// set so that the objects can be compared as a whole.
			_, err := node.PubKey()
			require.NoError(t, err)

			// Sort the addresses to ensure a consistent order.
			sortAddrs(node.Addresses)

			nodes = append(nodes, node)

			return nil
		}, func() {
			nodes = nil
		},
	)
	require.NoError(t, err)

	// Sort the nodes by their public key to ensure a consistent order.
	slices.SortFunc(nodes, func(i, j *models.Node) int {
		return bytes.Compare(i.PubKeyBytes[:], j.PubKeyBytes[:])
	})

	return nodes
}

// fetchSourceNode retrieves the source node from the given store.
func fetchSourceNode(t *testing.T, store V1Store) *models.Node {
	node, err := store.SourceNode(t.Context())
	if errors.Is(err, ErrSourceNodeNotSet) {
		return nil
	} else {
		require.NoError(t, err)
	}

	return node
}

// chanInfo holds information about a channel, including its edge info
// and the policies for both directions.
type chanInfo struct {
	edgeInfo *models.ChannelEdgeInfo
	policy1  *models.ChannelEdgePolicy
	policy2  *models.ChannelEdgePolicy
}

// chanSet is a slice of chanInfo
type chanSet []chanInfo

// CountPolicies counts the total number of policies in the channel set.
func (c chanSet) CountPolicies() int {
	var count int
	for _, info := range c {
		if info.policy1 != nil {
			count++
		}
		if info.policy2 != nil {
			count++
		}
	}
	return count
}

// fetchAllChannelsAndPolicies retrieves all channels and their policies
// from the given store and returns them sorted by their channel ID.
func fetchAllChannelsAndPolicies(t *testing.T, store V1Store) chanSet {
	ctx := t.Context()
	channels := make(chanSet, 0)
	err := store.ForEachChannel(ctx, func(info *models.ChannelEdgeInfo,
		p1 *models.ChannelEdgePolicy,
		p2 *models.ChannelEdgePolicy) error {

		if len(info.ExtraOpaqueData) == 0 {
			info.ExtraOpaqueData = nil
		}
		if p1 != nil && len(p1.ExtraOpaqueData) == 0 {
			p1.ExtraOpaqueData = nil
		}
		if p2 != nil && len(p2.ExtraOpaqueData) == 0 {
			p2.ExtraOpaqueData = nil
		}

		channels = append(channels, chanInfo{
			edgeInfo: info,
			policy1:  p1,
			policy2:  p2,
		})

		return nil
	}, func() {})
	require.NoError(t, err)

	// Sort the channels by their channel ID to ensure a consistent order.
	slices.SortFunc(channels, func(i, j chanInfo) int {
		return cmp.Compare(i.edgeInfo.ChannelID, j.edgeInfo.ChannelID)
	})

	return channels
}

// checkKVPruneLogEntries iterates through the prune log entries in the
// KVStore and checks that there is an entry for each in the SQLStore. It then
// does a final check to ensure that the prune tips in both stores match.
func checkKVPruneLogEntries(t *testing.T, kv *KVStore, sql *SQLStore,
	expTip int) {

	// Iterate through the prune log entries in the KVStore and
	// check that each entry exists in the SQLStore.
	err := forEachPruneLogEntry(
		kv.db, func(height uint32, hash *chainhash.Hash) error {
			sqlHash, err := sql.db.GetPruneHashByHeight(
				t.Context(), int64(height),
			)
			require.NoError(t, err)
			require.Equal(t, hash[:], sqlHash)

			return nil
		},
		func() {},
	)
	require.NoError(t, err)

	kvPruneHash, kvPruneHeight, kvPruneErr := kv.PruneTip()
	sqlPruneHash, sqlPruneHeight, sqlPruneErr := sql.PruneTip()

	// If the prune error is ErrGraphNeverPruned, then we expect
	// the SQL prune error to also be ErrGraphNeverPruned.
	if errors.Is(kvPruneErr, ErrGraphNeverPruned) {
		require.ErrorIs(t, sqlPruneErr, ErrGraphNeverPruned)
		return
	}

	// Otherwise, we expect both prune errors to be nil and the
	// prune hashes and heights to match.
	require.NoError(t, kvPruneErr)
	require.NoError(t, sqlPruneErr)
	require.Equal(t, kvPruneHash[:], sqlPruneHash[:])
	require.Equal(t, kvPruneHeight, sqlPruneHeight)
	require.Equal(t, expTip, int(sqlPruneHeight))
}

// checkClosedSCIDIndex iterates through the closed SCID index in the
// KVStore and checks that each SCID is marked as closed in the SQLStore.
func checkClosedSCIDIndex(t *testing.T, kv kvdb.Backend, sql *SQLStore) {
	err := forEachClosedSCID(kv, func(scid lnwire.ShortChannelID) error {
		closed, err := sql.IsClosedScid(scid)
		require.NoError(t, err)
		require.True(t, closed)

		return nil
	}, func() {})
	require.NoError(t, err)
}

// checkZombieIndex iterates through the zombie index in the
// KVStore and checks that each SCID is marked as a zombie in the SQLStore.
func checkZombieIndex(t *testing.T, kv kvdb.Backend, sql *SQLStore) {
	err := forEachZombieEntry(kv, func(chanID uint64, pubKey1,
		pubKey2 [33]byte) error {

		scid := lnwire.NewShortChanIDFromInt(chanID)

		// The migration logic skips zombie entries if they are already
		// present in the closed SCID index in the SQL DB. We need to
		// replicate that check here.
		isClosed, err := sql.IsClosedScid(scid)
		require.NoError(t, err)

		isZombie, _, _, err := sql.IsZombieEdge(chanID)
		require.NoError(t, err)

		if isClosed {
			// If it's in the closed index, it should NOT be in the
			// zombie index.
			require.False(t, isZombie)
		} else {
			// If it's not in the closed index, it SHOULD be in the
			// zombie index.
			require.True(t, isZombie)
		}

		return nil
	}, func() {})
	require.NoError(t, err)
}

// setUpKVStore initializes a new KVStore for testing.
func setUpKVStore(t *testing.T) *KVStore {
	kvDB, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "graph")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	kvStore, err := NewKVStore(kvDB)
	require.NoError(t, err)

	return kvStore
}

// genPubKey generates a new public key for testing purposes.
func genPubKey(t require.TestingT) route.Vertex {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var pub route.Vertex
	copy(pub[:], key.PubKey().SerializeCompressed())

	return pub
}

// testNodeOpt defines a functional option type that can be used to
// modify the attributes of a models.Node crated by makeTestNode.
type testNodeOpt func(*models.Node)

// makeTestNode can be used to create a test models.Node. The
// functional options can be used to modify the node's attributes.
func makeTestNode(t *testing.T, opts ...testNodeOpt) *models.Node {
	n := models.NewV1Node(genPubKey(t), &models.NodeV1Fields{
		AuthSigBytes:    testSigBytes,
		LastUpdate:      testTime,
		Color:           testColor,
		Alias:           "kek",
		Features:        testFeatures.RawFeatureVector,
		Addresses:       testAddrs,
		ExtraOpaqueData: testExtraData,
	})

	for _, opt := range opts {
		opt(n)
	}

	// We call this method so that the internal pubkey field is populated
	// which then lets us to proper struct comparison later on.
	_, err := n.PubKey()
	require.NoError(t, err)

	return n
}

// makeTestShellNode creates a minimal models.Node
// that only contains the public key and no other attributes.
func makeTestShellNode(t *testing.T,
	opts ...testNodeOpt) *models.Node {

	n := models.NewV1ShellNode(genPubKey(t))

	for _, opt := range opts {
		opt(n)
	}

	// We call this method so that the internal pubkey field is populated
	// which then lets us to proper struct comparison later on.
	_, err := n.PubKey()
	require.NoError(t, err)

	return n
}

// modify the attributes of a models.ChannelEdgeInfo created by makeTestChannel.
type testChanOpt func(info *models.ChannelEdgeInfo)

// makeTestChannel creates a test models.ChannelEdgeInfo. The functional options
// can be used to modify the channel's attributes.
func makeTestChannel(t *testing.T,
	opts ...testChanOpt) *models.ChannelEdgeInfo {

	c := &models.ChannelEdgeInfo{
		ChannelID:        prand.Uint64(),
		ChainHash:        testChain,
		NodeKey1Bytes:    genPubKey(t),
		NodeKey2Bytes:    genPubKey(t),
		BitcoinKey1Bytes: genPubKey(t),
		BitcoinKey2Bytes: genPubKey(t),
		Features:         testFeatures,
		AuthProof:        testAuthProof,
		ChannelPoint: wire.OutPoint{
			Hash:  rev,
			Index: prand.Uint32(),
		},
		Capacity:        10000,
		ExtraOpaqueData: testExtraData,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// testPolicyOpt defines a functional option type that can be used to modify the
// attributes of a models.ChannelEdgePolicy created by makeTestPolicy.
type testPolicyOpt func(*models.ChannelEdgePolicy)

var (
	updateTime   = prand.Int63()
	updateTimeMu sync.Mutex
)

func nextUpdateTime() time.Time {
	updateTimeMu.Lock()
	defer updateTimeMu.Unlock()

	updateTime++

	return time.Unix(updateTime, 0)
}

// makeTestPolicy creates a test models.ChannelEdgePolicy. The functional
// options can be used to modify the policy's attributes.
func makeTestPolicy(chanID uint64, toNode route.Vertex, isNode1 bool,
	opts ...testPolicyOpt) *models.ChannelEdgePolicy {

	chanFlags := lnwire.ChanUpdateChanFlags(1)
	if isNode1 {
		chanFlags = 0
	}

	p := &models.ChannelEdgePolicy{
		SigBytes:                  testSigBytes,
		ChannelID:                 chanID,
		LastUpdate:                nextUpdateTime(),
		MessageFlags:              1,
		ChannelFlags:              chanFlags,
		TimeLockDelta:             math.MaxUint16,
		MinHTLC:                   math.MaxUint64,
		MaxHTLC:                   math.MaxUint64,
		FeeBaseMSat:               math.MaxUint64,
		FeeProportionalMillionths: math.MaxUint64,
		ToNode:                    toNode,
		ExtraOpaqueData:           testExtraData,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// TestMigrationWithChannelDB tests the migration of the graph store from a
// bolt backed channel.db or a kvdb channel.sqlite to a SQL database. Note that
// this test does not attempt to be a complete migration test for all graph
// store types but rather is added as a tool for developers and users to debug
// graph migration issues with an actual channel.db/channel.sqlite file.
//
// NOTE: To use this test, place either of those files in the graph/db/testdata
// directory, uncomment the "Skipf" line, and set "chain" variable appropriately
// and set the "fileName" variable to the name of the channel database file you
// want to use for the migration test.
func TestMigrationWithChannelDB(t *testing.T) {
	ctx := t.Context()

	// NOTE: comment this line out to run the test.
	t.Skipf("skipping test meant for local debugging only")

	// NOTE: set this to the genesis hash of the chain that the store
	// was created on.
	chain := *chaincfg.MainNetParams.GenesisHash

	// NOTE: set this to the name of the channel database file you want
	// to use for the migration test. This may be either a bbolt ".db" file
	// or a SQLite ".sqlite" file. If you want to migrate from a
	// bbolt channel.db file, set this to "channel.db".
	const fileName = "channel.sqlite"

	cfg := &SQLStoreConfig{
		ChainHash: chain,
		QueryCfg:  sqldb.DefaultPostgresConfig(),
	}

	// Determine if we are using a SQLite file or a Bolt DB file.
	var isSqlite bool
	if strings.HasSuffix(fileName, ".sqlite") {
		isSqlite = true
		cfg.QueryCfg = sqldb.DefaultSQLiteConfig()
	}

	// Set up logging for the test.
	UseLogger(btclog.NewSLogger(btclog.NewDefaultHandler(os.Stdout)))

	// migrate runs the migration from the kvdb store to the SQL store.
	migrate := func(t *testing.T, kvBackend kvdb.Backend) {
		graphStore := newBatchQuerier(t)

		err := graphStore.ExecTx(
			ctx, sqldb.WriteTxOpt(), func(tx SQLQueries) error {
				return MigrateGraphToSQL(
					ctx, cfg, kvBackend, tx,
				)
			}, sqldb.NoOpReset,
		)
		require.NoError(t, err)
	}

	connectBBolt := func(t *testing.T, dbPath string) kvdb.Backend {
		cfg := &kvdb.BoltBackendConfig{
			DBPath:            dbPath,
			DBFileName:        fileName,
			NoFreelistSync:    true,
			AutoCompact:       false,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		}

		kvStore, err := kvdb.GetBoltBackend(cfg)
		require.NoError(t, err)

		return kvStore
	}

	connectSQLite := func(t *testing.T, dbPath string) kvdb.Backend {
		const (
			timeout  = 10 * time.Second
			maxConns = 50
		)
		sqlbase.Init(maxConns)

		cfg := &sqlite.Config{
			Timeout:        timeout,
			BusyTimeout:    timeout,
			MaxConnections: maxConns,
		}

		kvStore, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, cfg,
			dbPath, fileName,
			// NOTE: we use the raw string here else we get an
			// import cycle if we try to import lncfg.NSChannelDB.
			"channeldb",
		)
		require.NoError(t, err)

		return kvStore
	}

	tests := []struct {
		name   string
		dbPath string
	}{
		{
			name:   "empty",
			dbPath: t.TempDir(),
		},
		{
			name:   "testdata",
			dbPath: "testdata",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chanDBPath := path.Join(test.dbPath, fileName)
			t.Logf("Connecting to channel DB at: %s", chanDBPath)

			connectDB := connectBBolt
			if isSqlite {
				connectDB = connectSQLite
			}

			migrate(t, connectDB(t, test.dbPath))
		})
	}
}

// TestSQLMigrationEdgeCases tests various edge cases where the migration will
// still be successful but the final states of the KVStore and SQLStore
// will differ slightly.
func TestSQLMigrationEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var invalidTLVData = []byte{0x01, 0x02, 0x03}

	// Here, we test that in the case where the KV store contains a node
	// with invalid TLV data, the migration will still succeed, but the
	// node will not end up in the SQL store.
	t.Run("node with bad tlv data", func(t *testing.T) {
		t.Parallel()

		// Make one valid node and one node with invalid TLV data.
		n1 := makeTestNode(t)
		n2 := makeTestNode(t, func(n *models.Node) {
			n.ExtraOpaqueData = invalidTLVData
		})

		populateKV := func(t *testing.T, db *KVStore) {
			// Insert both nodes into the KV store.
			require.NoError(t, db.AddNode(ctx, n1))
			require.NoError(t, db.AddNode(ctx, n2))
		}

		runTestMigration(t, populateKV, dbState{
			// We expect only the valid node to be present in the
			// SQL db.
			nodes: []*models.Node{n1},
		})
	})

	// Here, we test that in the case where the KV store contains a channel
	// with invalid TLV data, the migration will still succeed, but the
	// channel and its policies will not end up in the SQL store.
	t.Run("channel with bad tlv data", func(t *testing.T) {
		t.Parallel()

		// Make two valid nodes to point to.
		n1 := makeTestNode(t)
		n2 := makeTestNode(t)

		// Create two channels between these nodes, one valid one
		// and one with invalid TLV data.
		c1 := makeTestChannel(t, func(c *models.ChannelEdgeInfo) {
			c.NodeKey1Bytes = n1.PubKeyBytes
			c.NodeKey2Bytes = n2.PubKeyBytes
		})
		c2 := makeTestChannel(t, func(c *models.ChannelEdgeInfo) {
			c.NodeKey1Bytes = n1.PubKeyBytes
			c.NodeKey2Bytes = n2.PubKeyBytes
			c.ExtraOpaqueData = invalidTLVData
		})

		// Create policies for both channels.
		p1 := makeTestPolicy(c1.ChannelID, n2.PubKeyBytes, true)
		p2 := makeTestPolicy(c2.ChannelID, n1.PubKeyBytes, false)

		populateKV := func(t *testing.T, db *KVStore) {
			// Insert both nodes into the KV store.
			require.NoError(t, db.AddNode(ctx, n1))
			require.NoError(t, db.AddNode(ctx, n2))

			// Insert both channels into the KV store.
			require.NoError(t, db.AddChannelEdge(ctx, c1))
			require.NoError(t, db.AddChannelEdge(ctx, c2))

			// Insert policies for both channels.
			_, _, err := db.UpdateEdgePolicy(ctx, p1)
			require.NoError(t, err)
			_, _, err = db.UpdateEdgePolicy(ctx, p2)
			require.NoError(t, err)
		}

		runTestMigration(t, populateKV, dbState{
			// Both nodes will be present.
			nodes: []*models.Node{n1, n2},
			// We only expect the first channel and its policy to
			// be present in the SQL db.
			chans: chanSet{{
				edgeInfo: c1,
				policy1:  p1,
			}},
		})
	})

	// Here, we test that in the case where the KV store contains a
	// channel policy with invalid TLV data, the migration will still
	// succeed, but the channel policy will not end up in the SQL store.
	t.Run("channel policy with bad tlv data", func(t *testing.T) {
		t.Parallel()

		// Make two valid nodes to point to.
		n1 := makeTestNode(t)
		n2 := makeTestNode(t)

		// Create one valid channels between these nodes.
		c := makeTestChannel(t, func(c *models.ChannelEdgeInfo) {
			c.NodeKey1Bytes = n1.PubKeyBytes
			c.NodeKey2Bytes = n2.PubKeyBytes
		})

		// Now, create two policies for this channel, one valid one
		// and one with invalid TLV data.
		p1 := makeTestPolicy(c.ChannelID, n2.PubKeyBytes, true)
		p2 := makeTestPolicy(
			c.ChannelID, n1.PubKeyBytes, false,
			func(p *models.ChannelEdgePolicy) {
				p.ExtraOpaqueData = invalidTLVData
			},
		)

		populateKV := func(t *testing.T, db *KVStore) {
			// Insert both nodes into the KV store.
			require.NoError(t, db.AddNode(ctx, n1))
			require.NoError(t, db.AddNode(ctx, n2))

			// Insert the channel into the KV store.
			require.NoError(t, db.AddChannelEdge(ctx, c))

			// Insert policies for the channel.
			_, _, err := db.UpdateEdgePolicy(ctx, p1)
			require.NoError(t, err)

			// We need to write this invalid one with the
			// updateEdgePolicy helper function in order to bypass
			// the newly added TLV validation in the
			// UpdateEdgePolicy method of the KVStore.
			err = db.db.Update(func(tx kvdb.RwTx) error {
				_, _, _, err := updateEdgePolicy(tx, p2)
				return err
			}, func() {})
			require.NoError(t, err)
		}

		runTestMigration(t, populateKV, dbState{
			// Both nodes will be present.
			nodes: []*models.Node{n1, n2},
			// The channel will be present, but only the
			// valid policy will be included in the SQL db.
			chans: chanSet{{
				edgeInfo: c,
				policy1:  p1,
			}},
		})
	})

	// Here, we test that in the case where the KV store contains a
	// channel policy that has a bit indicating that it contains a max HTLC
	// field, but the field is missing. The migration will still succeed,
	// but the policy will not end up in the SQL store.
	t.Run("channel policy with missing max htlc", func(t *testing.T) {
		t.Parallel()

		// Make two valid nodes to point to.
		n1 := makeTestNode(t)
		n2 := makeTestNode(t)

		// Create one valid channels between these nodes.
		c := makeTestChannel(t, func(c *models.ChannelEdgeInfo) {
			c.NodeKey1Bytes = n1.PubKeyBytes
			c.NodeKey2Bytes = n2.PubKeyBytes
		})

		// Now, create two policies for this channel, one valid one
		// and one with an invalid max htlc field.
		p1 := makeTestPolicy(c.ChannelID, n2.PubKeyBytes, true)
		p2 := makeTestPolicy(c.ChannelID, n1.PubKeyBytes, false)

		// We'll remove the no max_htlc field from the first edge
		// policy, and all other opaque data, and serialize it.
		p2.MessageFlags = 0
		p2.ExtraOpaqueData = nil

		var b bytes.Buffer
		require.NoError(t, serializeChanEdgePolicy(
			&b, p2, n1.PubKeyBytes[:],
		))

		// Set the max_htlc field. The extra bytes added to the
		// serialization will be the opaque data containing the
		// serialized field.
		p2.MessageFlags = lnwire.ChanUpdateRequiredMaxHtlc
		p2.MaxHTLC = math.MaxUint64
		var b2 bytes.Buffer
		require.NoError(t, serializeChanEdgePolicy(
			&b2, p2, n1.PubKeyBytes[:],
		))
		withMaxHtlc := b2.Bytes()

		// Remove the opaque data from the serialization.
		stripped := withMaxHtlc[:len(b.Bytes())]

		populateKV := func(t *testing.T, db *KVStore) {
			// Insert both nodes into the KV store.
			require.NoError(t, db.AddNode(ctx, n1))
			require.NoError(t, db.AddNode(ctx, n2))

			// Insert the channel into the KV store.
			require.NoError(t, db.AddChannelEdge(ctx, c))

			// Insert policies for the channel.
			_, _, err := db.UpdateEdgePolicy(ctx, p1)
			require.NoError(t, err)

			putSerializedPolicy(
				t, db.db, n2.PubKeyBytes[:], c.ChannelID,
				stripped,
			)
		}

		runTestMigration(t, populateKV, dbState{
			// Both nodes will be present.
			nodes: []*models.Node{n1, n2},
			// The channel will be present, but only the
			// valid policy will be included in the SQL db.
			chans: chanSet{{
				edgeInfo: c,
				policy1:  p1,
			}},
		})
	})

	// This test covers the case where the KV store contains zombie entries
	// that it also has entries for in the closed SCID index. In this case,
	// the SQL store will only insert zombie entries for channels that
	// are not yet closed.
	t.Run("zombies and closed scids", func(t *testing.T) {
		var (
			n1, n2 route.Vertex
			cID1   = uint64(1)
			cID2   = uint64(2)
		)

		populateKV := func(t *testing.T, db *KVStore) {
			// Mark both channels as zombies.
			err := db.MarkEdgeZombie(cID1, n1, n2)
			require.NoError(t, err)

			err = db.MarkEdgeZombie(cID2, n1, n2)
			require.NoError(t, err)

			// Mark channel 1 as closed.
			err = db.PutClosedScid(
				lnwire.NewShortChanIDFromInt(cID1),
			)
			require.NoError(t, err)
		}

		runTestMigration(t, populateKV, dbState{
			chans:   make(chanSet, 0),
			closed:  []uint64{1},
			zombies: []uint64{2},
		})
	})

	// We have used this migration as a chance to also extract any DNS
	// addresses that we previously may have wrapped in an opaque address.
	// If we do encounter such a case, then the migrated node set will look
	// slightly different from the original node set in the KV store, and so
	// we test for that here.
	t.Run("node with wrapped DNS address inside opaque addr",
		func(t *testing.T) {
			t.Parallel()

			var expectedNodes []*models.Node

			// Let the first node have an opaque address that we
			// still don't understand. This node will remain the
			// same in the SQL store.
			n1 := makeTestNode(t, func(n *models.Node) {
				n.Addresses = []net.Addr{
					testOpaqueAddr,
				}
			})
			expectedNodes = append(expectedNodes, n1)

			// The second node will have a wrapped DNS address
			// inside an opaque address. The opaque address will
			// only contain a DNS address and so the migrated node
			// will only contain a DNS address and no opaque
			// address.
			n2 := makeTestNode(t, func(n *models.Node) {
				n.Addresses = []net.Addr{
					testOpaqueAddrWithEmbeddedDNSAddr,
				}
			})
			n2Expected := *n2
			n2Expected.Addresses = []net.Addr{
				testDNSAddr,
			}
			expectedNodes = append(expectedNodes, &n2Expected)

			// The third node will have an opaque address that
			// wraps a DNS address along with some other data.
			// So the resulting migrated node should have both
			// the DNS address and remaining opaque address data.
			n3 := makeTestNode(t, func(n *models.Node) {
				n.Addresses = []net.Addr{
					//nolint:ll
					testOpaqueAddrWithEmbeddedDNSAddrAndMore,
				}
			})
			n3Expected := *n3
			n3Expected.Addresses = []net.Addr{
				testDNSAddr,
				testOpaqueAddr,
			}
			expectedNodes = append(expectedNodes, &n3Expected)

			// The fourth node will have an opaque address that
			// wraps an invalid DNS address. Such a node will not be
			// migrated since propagating an invalid DNS address
			// is not allowed.
			n4 := makeTestNode(t, func(n *models.Node) {
				n.Addresses = []net.Addr{
					testOpaqueAddrWithEmbeddedBadDNSAddr,
				}
			})
			// NOTE: we don't add this node to the expected nodes
			// slice.

			// The fifth node will have 2 DNS addresses embedded
			// in the opaque address. The migration will result
			// in _both_ dns addresses being extracted. This is
			// invalid at a protocol level, and so we should not
			// propagate such addresses, but this is left to higher
			// level gossip logic.
			n5 := makeTestNode(t, func(n *models.Node) {
				n.Addresses = []net.Addr{
					testOpaqueAddrWithTwoEmbeddedDNSAddrs,
				}
			})
			n5Expected := *n5
			n5Expected.Addresses = []net.Addr{
				testDNSAddr,
				testDNSAddr,
			}
			expectedNodes = append(expectedNodes, &n5Expected)

			populateKV := func(t *testing.T, db *KVStore) {
				require.NoError(t, db.AddNode(ctx, n1))
				require.NoError(t, db.AddNode(ctx, n2))
				require.NoError(t, db.AddNode(ctx, n3))
				require.NoError(t, db.AddNode(ctx, n4))
				require.NoError(t, db.AddNode(ctx, n5))
			}

			runTestMigration(t, populateKV, dbState{
				nodes: expectedNodes,
			})
		},
	)
}

// runTestMigration is a helper function that sets up the KVStore and SQLStore,
// populates the KVStore with the provided call-back, runs the migration, and
// asserts that the SQLStore contains the expected state.
func runTestMigration(t *testing.T, populateKV func(t *testing.T, db *KVStore),
	expState dbState) {

	ctx := t.Context()

	// Set up our source kvdb DB.
	kvDB := setUpKVStore(t)

	// Set up our destination SQL DB.
	sql, ok := NewTestDB(t).(*SQLStore)
	require.True(t, ok)

	// Populate the kvdb store with the test data.
	populateKV(t, kvDB)

	// Run the migration.
	err := MigrateGraphToSQL(
		ctx, sql.cfg, kvDB.db, sql.db,
	)
	require.NoError(t, err)

	assertResultState(t, sql, expState)
}

// dbState describes the expected state of the SQLStore after a migration.
type dbState struct {
	nodes   []*models.Node
	chans   chanSet
	closed  []uint64
	zombies []uint64
}

// assertResultState asserts that the SQLStore contains the expected
// state after a migration.
func assertResultState(t *testing.T, sql *SQLStore, expState dbState) {
	// Assert that the sql store contains the expected nodes.
	require.ElementsMatch(t, expState.nodes, fetchAllNodes(t, sql))
	require.ElementsMatch(
		t, expState.chans, fetchAllChannelsAndPolicies(t, sql),
	)

	for _, closed := range expState.closed {
		isClosed, err := sql.IsClosedScid(
			lnwire.NewShortChanIDFromInt(closed),
		)
		require.NoError(t, err)
		require.True(t, isClosed)

		// Any closed SCID should NOT be in the zombie
		// index.
		isZombie, _, _, err := sql.IsZombieEdge(closed)
		require.NoError(t, err)
		require.False(t, isZombie)
	}

	for _, zombie := range expState.zombies {
		isZombie, _, _, err := sql.IsZombieEdge(
			zombie,
		)
		require.NoError(t, err)
		require.True(t, isZombie)
	}
}

// TestMigrateGraphToSQLRapid tests the migration of graph nodes from a KV
// store to a SQL store using property-based testing to ensure that the
// migration works for a wide variety of randomly generated graph nodes.
func TestMigrateGraphToSQLRapid(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skipf("skipping test in short mode")
	}

	dbFixture := NewTestDBFixture(t)

	rapid.Check(t, func(rt *rapid.T) {
		const (
			maxNumNodes    = 5
			maxNumChannels = 5
		)

		testMigrateGraphToSQLRapidOnce(
			t, rt, dbFixture, maxNumNodes, maxNumChannels,
		)
	})
}

// testMigrateGraphToSQLRapidOnce is a helper function that performs the actual
// migration test using property-based testing. It sets up a KV store and a
// SQL store, generates random nodes and channels, populates the KV store,
// runs the migration, and asserts that the SQL store contains the expected
// state.
//
// The migration is run twice in order to test idempotency and retry-safety.
func testMigrateGraphToSQLRapidOnce(t *testing.T, rt *rapid.T,
	dbFixture *sqldb.TestPgFixture, maxNumNodes, maxNumChannels int) {

	ctx := t.Context()

	// Set up our source kvdb DB.
	kvDB := setUpKVStore(t)

	// Set up our destination SQL DB.
	sql, ok := NewTestDBWithFixture(t, dbFixture).(*SQLStore)
	require.True(t, ok)

	// Generate a list of random nodes.
	nodes := rapid.SliceOfN(
		rapid.Custom(genRandomNode), 1, maxNumNodes,
	).Draw(rt, "nodes")

	// Keep track of all nodes that should be in the database. We may expect
	// more than just the ones we generated above if we have channels that
	// point to shell nodes.
	allNodes := make(map[route.Vertex]*models.Node)
	var nodePubs []route.Vertex
	for _, node := range nodes {
		allNodes[node.PubKeyBytes] = node
		nodePubs = append(nodePubs, node.PubKeyBytes)
	}

	// Generate a list of random channels and policies for those channels.
	var (
		channels []*models.ChannelEdgeInfo
		chanIDs  = make(map[uint64]struct{})
		policies []*models.ChannelEdgePolicy
	)
	channelGen := rapid.Custom(func(rtt *rapid.T) *models.ChannelEdgeInfo {
		var (
			edge     *models.ChannelEdgeInfo
			newNodes []route.Vertex
		)
		// Loop to ensure that we skip channels with channel IDs
		// that we have already used.
		for {
			edge, newNodes = genRandomChannel(rtt, nodePubs)
			if _, ok := chanIDs[edge.ChannelID]; ok {
				continue
			}
			chanIDs[edge.ChannelID] = struct{}{}

			break
		}

		// If the new channel points to nodes we don't yet know
		// of, then update our expected node list to include
		// shell node entries for these.
		for _, n := range newNodes {
			if _, ok := allNodes[n]; ok {
				continue
			}

			shellNode := makeTestShellNode(
				t, func(node *models.Node) {
					node.PubKeyBytes = n
				},
			)
			allNodes[n] = shellNode
		}

		// Generate either 0, 1 or two policies for this
		// channel.
		numPolicies := rapid.IntRange(0, 2).Draw(
			rtt, "numPolicies",
		)
		switch numPolicies {
		case 0:
		case 1:
			// Randomly pick the direction.
			policy := genRandomPolicy(
				rtt, edge, rapid.Bool().Draw(rtt, "isNode1"),
			)

			policies = append(policies, policy)
		case 2:
			// Generate two policies, one for each
			// direction.
			policy1 := genRandomPolicy(rtt, edge, true)
			policy2 := genRandomPolicy(rtt, edge, false)

			policies = append(policies, policy1)
			policies = append(policies, policy2)
		}

		return edge
	})
	channels = rapid.SliceOfN(
		channelGen, 1, maxNumChannels,
	).Draw(rt, "channels")

	// Write the test objects to the kvdb store.
	for _, node := range allNodes {
		err := kvDB.AddNode(ctx, node)
		require.NoError(t, err)
	}
	for _, channel := range channels {
		err := kvDB.AddChannelEdge(ctx, channel)
		require.NoError(t, err)
	}
	for _, policy := range policies {
		_, _, err := kvDB.UpdateEdgePolicy(ctx, policy)
		require.NoError(t, err)
	}

	// Run the migration.
	err := MigrateGraphToSQL(ctx, sql.cfg, kvDB.db, sql.db)
	require.NoError(t, err)

	// Create a slice of all nodes.
	var nodesSlice []*models.Node
	for _, node := range allNodes {
		nodesSlice = append(nodesSlice, node)
	}

	// Create a map of channels to their policies.
	chanMap := make(map[uint64]*chanInfo)
	for _, channel := range channels {
		chanMap[channel.ChannelID] = &chanInfo{
			edgeInfo: channel,
		}
	}

	for _, policy := range policies {
		info, ok := chanMap[policy.ChannelID]
		require.True(t, ok)

		// The IsNode1 flag is encoded in the ChannelFlags.
		if policy.ChannelFlags&lnwire.ChanUpdateDirection == 0 {
			info.policy1 = policy
		} else {
			info.policy2 = policy
		}
	}

	var chanSetForState chanSet
	for _, info := range chanMap {
		chanSetForState = append(chanSetForState, *info)
	}

	// Validate that the sql database has the correct state.
	assertResultState(t, sql, dbState{
		nodes: nodesSlice,
		chans: chanSetForState,
	})

	// The migration is expected to be idempotent and retry-safe. So running
	// it again should yield the same result.
	err = MigrateGraphToSQL(ctx, sql.cfg, kvDB.db, sql.db)
	require.NoError(t, err)
	assertResultState(t, sql, dbState{
		nodes: nodesSlice,
		chans: chanSetForState,
	})
}

// genRandomChannel is a rapid generator for creating random channel edge infos.
// It takes a slice of existing node public keys to draw from. If the slice is
// empty, it will always generate new random nodes.
func genRandomChannel(rt *rapid.T,
	nodes []route.Vertex) (*models.ChannelEdgeInfo, []route.Vertex) {

	var newNodes []route.Vertex

	// Generate a random channel ID.
	chanID := lnwire.RandShortChannelID(rt).ToUint64()

	// Generate a random outpoint.
	var hash chainhash.Hash
	_, err := rand.Read(hash[:])
	require.NoError(rt, err)
	outpoint := wire.OutPoint{
		Hash:  hash,
		Index: rapid.Uint32().Draw(rt, "outpointIndex"),
	}

	// Generate random capacity.
	capacity := rapid.Int64Range(1, btcutil.MaxSatoshi).Draw(rt, "capacity")

	// Generate random features.
	features := lnwire.NewFeatureVector(
		lnwire.RandFeatureVector(rt),
		lnwire.Features,
	)

	// Generate random keys for the channel.
	bitcoinKey1Bytes := genPubKey(rt)
	bitcoinKey2Bytes := genPubKey(rt)

	// Decide if we should use existing nodes or generate new ones.
	var nodeKey1Bytes, nodeKey2Bytes route.Vertex
	// With a 50/50 chance, we'll use existing nodes.
	if len(nodes) > 1 && rapid.Bool().Draw(rt, "useExistingNodes") {
		// Pick two random nodes from the existing set.
		idx1 := rapid.IntRange(0, len(nodes)-1).Draw(rt, "node1")
		idx2 := rapid.IntRange(0, len(nodes)-1).Draw(rt, "node2")
		if idx1 == idx2 {
			idx2 = (idx1 + 1) % len(nodes)
		}
		nodeKey1Bytes = nodes[idx1]
		nodeKey2Bytes = nodes[idx2]
	} else {
		// Generate new random nodes.
		nodeKey1Bytes = genPubKey(rt)
		nodeKey2Bytes = genPubKey(rt)
		newNodes = append(newNodes, nodeKey1Bytes, nodeKey2Bytes)
	}

	node1Sig := lnwire.RandSignature(rt)
	node2Sig := lnwire.RandSignature(rt)
	btc1Sig := lnwire.RandSignature(rt)
	btc2Sig := lnwire.RandSignature(rt)

	// Generate a random auth proof.
	authProof := &models.ChannelAuthProof{
		NodeSig1Bytes:    node1Sig.RawBytes(),
		NodeSig2Bytes:    node2Sig.RawBytes(),
		BitcoinSig1Bytes: btc1Sig.RawBytes(),
		BitcoinSig2Bytes: btc2Sig.RawBytes(),
	}

	extraOpaque := lnwire.RandExtraOpaqueData(rt, nil)
	if len(extraOpaque) == 0 {
		extraOpaque = nil
	}

	info := &models.ChannelEdgeInfo{
		ChannelID:        chanID,
		ChainHash:        testChain,
		NodeKey1Bytes:    nodeKey1Bytes,
		NodeKey2Bytes:    nodeKey2Bytes,
		BitcoinKey1Bytes: bitcoinKey1Bytes,
		BitcoinKey2Bytes: bitcoinKey2Bytes,
		Features:         features,
		AuthProof:        authProof,
		ChannelPoint:     outpoint,
		Capacity:         btcutil.Amount(capacity),
		ExtraOpaqueData:  extraOpaque,
	}

	return info, newNodes
}

// genRandomPolicy is a rapid generator for creating random channel edge
// policies. It takes a slice of existing channels to draw from.
func genRandomPolicy(rt *rapid.T, channel *models.ChannelEdgeInfo,
	isNode1 bool) *models.ChannelEdgePolicy {

	var toNode route.Vertex
	if isNode1 {
		toNode = channel.NodeKey2Bytes
	} else {
		toNode = channel.NodeKey1Bytes
	}

	// Generate a random timestamp.
	randTime := time.Unix(rapid.Int64Range(
		0, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
	).Draw(rt, "policyTimestamp"), 0)

	// Generate random channel update flags and then just make sure to
	// unset/set the correct direction bit.
	chanFlags := lnwire.ChanUpdateChanFlags(
		rapid.Uint8().Draw(rt, "chanFlags"),
	)
	if isNode1 {
		chanFlags &= ^lnwire.ChanUpdateDirection
	} else {
		chanFlags |= lnwire.ChanUpdateDirection
	}

	extraOpaque := lnwire.RandExtraOpaqueData(rt, nil)
	if len(extraOpaque) == 0 {
		extraOpaque = nil
	}

	hasMaxHTLC := rapid.Bool().Draw(rt, "hasMaxHTLC")
	var maxHTLC lnwire.MilliSatoshi
	msgFlags := lnwire.ChanUpdateMsgFlags(
		rapid.Uint8().Draw(rt, "msgFlags"),
	)
	if hasMaxHTLC {
		msgFlags |= lnwire.ChanUpdateRequiredMaxHtlc
		maxHTLC = lnwire.MilliSatoshi(
			rapid.Uint64().Draw(rt, "maxHtlc"),
		)
	} else {
		msgFlags &= ^lnwire.ChanUpdateRequiredMaxHtlc
	}

	return &models.ChannelEdgePolicy{
		SigBytes:      testSigBytes,
		ChannelID:     channel.ChannelID,
		LastUpdate:    randTime,
		MessageFlags:  msgFlags,
		ChannelFlags:  chanFlags,
		TimeLockDelta: rapid.Uint16().Draw(rt, "timeLock"),
		MinHTLC: lnwire.MilliSatoshi(
			rapid.Uint64().Draw(rt, "minHtlc"),
		),
		MaxHTLC: maxHTLC,
		FeeBaseMSat: lnwire.MilliSatoshi(
			rapid.Uint64().Draw(rt, "baseFee"),
		),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			rapid.Uint64().Draw(rt, "feeRate"),
		),
		ToNode:          toNode,
		ExtraOpaqueData: extraOpaque,
	}
}

// sortAddrs sorts a slice of net.Addr.
func sortAddrs(addrs []net.Addr) {
	if addrs == nil {
		return
	}

	slices.SortFunc(addrs, func(i, j net.Addr) int {
		return strings.Compare(i.String(), j.String())
	})
}

// genRandomNode is a rapid generator for creating random lightning nodes.
func genRandomNode(t *rapid.T) *models.Node {
	// Generate a random alias that is valid.
	alias := lnwire.RandNodeAlias(t)

	// Generate a random public key.
	pubKey := lnwire.RandPubKey(t)
	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey.SerializeCompressed())

	// Generate a random signature.
	sig := lnwire.RandSignature(t)
	sigBytes := sig.ToSignatureBytes()

	// Generate a random color.
	randColor := color.RGBA{
		R: uint8(rapid.IntRange(0, 255).
			Draw(t, "R")),
		G: uint8(rapid.IntRange(0, 255).
			Draw(t, "G")),
		B: uint8(rapid.IntRange(0, 255).
			Draw(t, "B")),
		A: 0,
	}

	// Generate a random timestamp.
	randTime := time.Unix(
		rapid.Int64Range(
			0, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
		).Draw(t, "timestamp"), 0,
	)

	// Generate random addresses.
	addrs := lnwire.RandNetAddrs(t)
	sortAddrs(addrs)

	// Generate a random feature vector.
	features := lnwire.RandFeatureVector(t)

	// Generate random extra opaque data.
	extraOpaqueData := lnwire.RandExtraOpaqueData(t, nil)
	if len(extraOpaqueData) == 0 {
		extraOpaqueData = nil
	}

	node := models.NewV1Node(pubKeyBytes, &models.NodeV1Fields{
		AuthSigBytes:    sigBytes,
		LastUpdate:      randTime,
		Color:           randColor,
		Alias:           alias.String(),
		Features:        features,
		Addresses:       addrs,
		ExtraOpaqueData: extraOpaqueData,
	})

	// We call this method so that the internal pubkey field is populated
	// which then lets us to proper struct comparison later on.
	_, err := node.PubKey()
	require.NoError(t, err)

	return node
}

// putSerializedPolicy is a helper function that writes a serialized
// ChannelEdgePolicy to the edge bucket in the database.
func putSerializedPolicy(t *testing.T, db kvdb.Backend, from []byte,
	chanID uint64, b []byte) {

	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		require.NotNil(t, edges)

		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		require.NotNil(t, edgeIndex)

		var edgeKey [33 + 8]byte
		copy(edgeKey[:], from)
		byteOrder.PutUint64(edgeKey[33:], chanID)

		var scratch [8]byte
		var indexKey [8 + 8]byte
		copy(indexKey[:], scratch[:])
		byteOrder.PutUint64(indexKey[8:], chanID)

		updateIndex, err := edges.CreateBucketIfNotExists(
			edgeUpdateIndexBucket,
		)
		require.NoError(t, err)
		require.NoError(t, updateIndex.Put(indexKey[:], nil))

		return edges.Put(edgeKey[:], b)
	}, func() {})
	require.NoError(t, err, "error writing db")
}
