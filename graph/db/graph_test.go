package graphdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"math"
	prand "math/rand"
	"net"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr, anotherAddr}

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18" +
		"e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6" +
		"700d72a0ead154c03be696a292d24ae")
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)

	testFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.GossipQueriesRequired),
		lnwire.Features,
	)

	testPub = route.Vertex{2, 202, 4}

	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
)

func createNode(priv *btcec.PrivateKey) *models.Node {
	pubKey := route.NewVertex(priv.PubKey())

	return models.NewV1Node(
		pubKey, &models.NodeV1Fields{
			LastUpdate:   nextUpdateTime(),
			Color:        color.RGBA{1, 2, 3, 0},
			Alias:        "kek" + hex.EncodeToString(pubKey[:]),
			Addresses:    testAddrs,
			Features:     testFeatures.RawFeatureVector,
			AuthSigBytes: testSig.Serialize(),
		},
	)
}

func createTestVertex(t testing.TB) *models.Node {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return createNode(priv)
}

// TestNodeInsertionAndDeletion tests the CRUD operations for a Node.
func TestNodeInsertionAndDeletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test basic insertion/deletion for vertexes from the
	// graph, so we'll create a test vertex to start with.
	timeStamp := int64(1232342)
	nodeWithAddrs := func(addrs []net.Addr) *models.Node {
		timeStamp++

		return models.NewV1Node(
			testPub, &models.NodeV1Fields{
				AuthSigBytes:    testSig.Serialize(),
				LastUpdate:      time.Unix(timeStamp, 0),
				Color:           color.RGBA{1, 2, 3, 0},
				Alias:           "kek",
				Features:        testFeatures.RawFeatureVector,
				Addresses:       addrs,
				ExtraOpaqueData: []byte{1, 1, 1, 2, 2, 2, 2},
			},
		)
	}

	// First, insert the node into the graph DB. This should succeed
	// without any errors.
	node := nodeWithAddrs(testAddrs)
	require.NoError(t, graph.AddNode(ctx, node))
	assertNodeInCache(t, graph, node, testFeatures)

	// Our AddNode implementation uses the batcher meaning that it is
	// possible that two updates for the same node announcement may be
	// processed in the same batch. So to avoid the conflict error (since we
	// require at the DB level that the new timestamp is strictly
	// greater than the previous one), we need to gracefully handle the
	// case where the exact same node announcement is added twice.
	require.NoError(t, graph.AddNode(ctx, node))

	// Next, fetch the node from the database to ensure everything was
	// serialized properly.
	dbNode, err := graph.FetchNode(ctx, testPub)
	require.NoError(t, err, "unable to locate node")

	_, exists, err := graph.HasNode(ctx, dbNode.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, exists)

	// The two nodes should match exactly!
	compareNodes(t, node, dbNode)

	// Check that the node's features are fetched correctly. This check
	// will use the graph cache to fetch the features.
	features, err := graph.FetchNodeFeatures(node.PubKeyBytes)
	require.NoError(t, err)
	require.Equal(t, testFeatures, features)

	// Check that the node's features are fetched correctly. This check
	// will check the database directly.
	features, err = graph.V1Store.FetchNodeFeatures(node.PubKeyBytes)
	require.NoError(t, err)
	require.Equal(t, testFeatures, features)

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	require.NoError(t, graph.DeleteNode(ctx, testPub))
	assertNodeNotInCache(t, graph, testPub)

	// Attempting to delete the node again should return an error since
	// the node is no longer known.
	require.ErrorIs(
		t, graph.DeleteNode(ctx, testPub),
		ErrGraphNodeNotFound,
	)

	// Finally, attempt to fetch the node again. This should fail as the
	// node should have been deleted from the database.
	_, err = graph.FetchNode(ctx, testPub)
	require.ErrorIs(t, err, ErrGraphNodeNotFound)

	// Now, we'll specifically test the updating of addresses of a node
	// since the serialisation and persistence of addresses is a bit
	// tricky.

	pub, err := node.PubKey()
	require.NoError(t, err)

	// Initially, the node is unknown to the graph and there should be no
	// addresses for it.
	known, addrs, err := graph.AddrsForNode(ctx, pub)
	require.NoError(t, err)
	require.False(t, known)
	require.Empty(t, addrs)

	// Add the node without any addresses.
	node = nodeWithAddrs(nil)
	require.NoError(t, graph.AddNode(ctx, node))

	// Fetch the node and assert the empty addresses.
	dbNode, err = graph.FetchNode(ctx, testPub)
	require.NoError(t, err)
	compareNodes(t, node, dbNode)

	known, addrs, err = graph.AddrsForNode(ctx, pub)
	require.NoError(t, err)
	require.True(t, known)
	require.Empty(t, addrs)

	// Now, update the node's addresses.
	expAddrs := []net.Addr{
		// Add 2 IPV4 addresses.
		testAddr,
		testIPV4Addr,
		// Add 2 IPV6 addresses.
		testIPV6Addr,
		anotherAddr,
		// Add one v2 and one v3 onion address.
		testOnionV2Addr,
		testOnionV3Addr,
		// Add a DNS host address.
		testDNSAddr,
		// Make sure to also test the opaque address type.
		testOpaqueAddr,
	}
	node = nodeWithAddrs(expAddrs)
	require.NoError(t, graph.AddNode(ctx, node))

	// Fetch the node and assert the updated addresses.
	dbNode, err = graph.FetchNode(ctx, testPub)
	require.NoError(t, err)
	require.Equal(t, expAddrs, dbNode.Addresses)

	known, addrs, err = graph.AddrsForNode(ctx, pub)
	require.NoError(t, err)
	require.True(t, known)
	require.EqualValues(t, expAddrs, addrs)

	// Now, change the address set a bit: change the order of the
	// IPV4 addresses, remove one IPV6 address and remove both onion
	// addresses.
	expAddrs = []net.Addr{
		testIPV4Addr,
		testAddr,
		testIPV6Addr,
	}
	node = nodeWithAddrs(expAddrs)
	require.NoError(t, graph.AddNode(ctx, node))

	// Fetch the node and assert the updated addresses.
	dbNode, err = graph.FetchNode(ctx, testPub)
	require.NoError(t, err)
	require.Equal(t, expAddrs, dbNode.Addresses)

	// Finally, update the set to only contain the Tor addresses.
	expAddrs = []net.Addr{
		testOnionV2Addr,
		testOnionV3Addr,
	}
	node = nodeWithAddrs(expAddrs)
	require.NoError(t, graph.AddNode(ctx, node))

	// Fetch the node and assert the updated addresses.
	dbNode, err = graph.FetchNode(ctx, testPub)
	require.NoError(t, err)
	require.Equal(t, expAddrs, dbNode.Addresses)

	// Also check that the withAddr param of ForEachNodeCached correctly
	// returns the addresses we expect for this node.
	err = graph.ForEachNodeCached(
		ctx, true, func(ctx context.Context, node route.Vertex,
			addrs []net.Addr,
			chans map[uint64]*DirectedChannel) error {

			if node != dbNode.PubKeyBytes {
				return nil
			}

			require.Equal(t, expAddrs, addrs)

			return nil
		}, func() {},
	)
	require.NoError(t, err)
}

// TestPartialNode checks that we can add and retrieve a Node where
// only the pubkey is known to the database.
func TestPartialNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// To insert a partial node, we need to add a channel edge that has
	// node keys for nodes we are not yet aware
	var node1, node2 models.Node
	copy(node1.PubKeyBytes[:], pubKey1Bytes)
	copy(node2.PubKeyBytes[:], pubKey2Bytes)

	// Create an edge attached to these nodes and add it to the graph.
	edgeInfo, _ := createEdge(140, 0, 0, 0, &node1, &node2)
	require.NoError(t, graph.AddChannelEdge(ctx, &edgeInfo))

	// Both of the nodes should now be in both the graph (as partial/shell)
	// nodes _and_ the cache should also have an awareness of both nodes.
	assertNodeInCache(t, graph, &node1, nil)
	assertNodeInCache(t, graph, &node2, nil)

	// Next, fetch the node2 from the database to ensure everything was
	// serialized properly.
	dbNode1, err := graph.FetchNode(ctx, pubKey1)
	require.NoError(t, err)
	dbNode2, err := graph.FetchNode(ctx, pubKey2)
	require.NoError(t, err)

	_, exists, err := graph.HasNode(ctx, dbNode1.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, exists)

	// The two nodes should match exactly! (with default values for
	// LastUpdate and db set to satisfy compareNodes())
	expectedNode1 := models.NewV1ShellNode(pubKey1)
	compareNodes(t, expectedNode1, dbNode1)

	_, exists, err = graph.HasNode(ctx, dbNode2.PubKeyBytes)
	require.NoError(t, err)
	require.True(t, exists)

	// The two nodes should match exactly! (with default values for
	// LastUpdate and db set to satisfy compareNodes())
	expectedNode2 := models.NewV1ShellNode(pubKey2)
	compareNodes(t, expectedNode2, dbNode2)

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	require.NoError(t, graph.DeleteNode(ctx, pubKey1))
	assertNodeNotInCache(t, graph, testPub)

	// Finally, attempt to fetch the node again. This should fail as the
	// node should have been deleted from the database.
	_, err = graph.FetchNode(ctx, testPub)
	require.ErrorIs(t, err, ErrGraphNodeNotFound)
}

// TestAliasLookup tests the alias lookup functionality of the graph store.
func TestAliasLookup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test the alias index within the database, so first
	// create a new test node.
	testNode := createTestVertex(t)

	// Add the node to the graph's database, this should also insert an
	// entry into the alias index for this node.
	require.NoError(t, graph.AddNode(ctx, testNode))

	// Next, attempt to lookup the alias. The alias should exactly match
	// the one which the test node was assigned.
	nodePub, err := testNode.PubKey()
	require.NoError(t, err, "unable to generate pubkey")
	dbAlias, err := graph.LookupAlias(ctx, nodePub)
	require.NoError(t, err, "unable to find alias")
	require.Equal(t, testNode.Alias.UnwrapOr(""), dbAlias)

	// Ensure that looking up a non-existent alias results in an error.
	node := createTestVertex(t)
	nodePub, err = node.PubKey()
	require.NoError(t, err, "unable to generate pubkey")
	_, err = graph.LookupAlias(ctx, nodePub)
	require.ErrorIs(t, err, ErrNodeAliasNotFound)
}

// TestSourceNode tests the source node functionality of the graph store.
func TestSourceNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test the setting/getting of the source node, so we
	// first create a fake node to use within the test.
	testNode := createTestVertex(t)

	// Attempt to fetch the source node, this should return an error as the
	// source node hasn't yet been set.
	_, err := graph.SourceNode(ctx)
	require.ErrorIs(t, err, ErrSourceNodeNotSet)

	// Set the source node, this should insert the node into the
	// database in a special way indicating it's the source node.
	require.NoError(t, graph.SetSourceNode(ctx, testNode))

	// Retrieve the source node from the database, it should exactly match
	// the one we set above.
	sourceNode, err := graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")
	compareNodes(t, testNode, sourceNode)
}

// TestSetSourceNodeSameTimestamp tests that SetSourceNode accepts updates
// with the same timestamp. This is necessary because multiple code paths
// (setSelfNode, createNewHiddenService, RPC updates) can race during startup,
// reading the same old timestamp and independently incrementing it to the same
// new value. For our own node, we want parameter changes to persist even with
// timestamp collisions (unlike network gossip where same timestamp means same
// content).
func TestSetSourceNodeSameTimestamp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Create and set the initial source node.
	testNode := createTestVertex(t)
	require.NoError(t, graph.SetSourceNode(ctx, testNode))

	// Verify the source node was set correctly.
	sourceNode, err := graph.SourceNode(ctx)
	require.NoError(t, err)
	compareNodes(t, testNode, sourceNode)

	// Create a modified version of the node with the same timestamp but
	// different parameters (e.g., different alias and color). This
	// simulates the race condition where multiple goroutines read the
	// same old timestamp, independently increment it, and try to update
	// with different changes.
	modifiedNode := models.NewV1Node(
		testNode.PubKeyBytes, &models.NodeV1Fields{
			// Same timestamp.
			LastUpdate: testNode.LastUpdate,
			// Different alias.
			Alias:        "different-alias",
			Color:        color.RGBA{R: 100, G: 200, B: 50, A: 0},
			Addresses:    testNode.Addresses,
			Features:     testNode.Features.RawFeatureVector,
			AuthSigBytes: testNode.AuthSigBytes,
		},
	)

	// Attempt to set the source node with the same timestamp but
	// different parameters. This should now succeed for both SQL and KV
	// stores. The SQL store uses UpsertSourceNode which removes the
	// strict timestamp constraint, allowing last-write-wins semantics.
	require.NoError(t, graph.SetSourceNode(ctx, modifiedNode))

	// Verify that the parameter changes actually persisted.
	updatedNode, err := graph.SourceNode(ctx)
	require.NoError(t, err)
	require.Equal(t, "different-alias", updatedNode.Alias.UnwrapOr(""))
	require.Equal(
		t, color.RGBA{R: 100, G: 200, B: 50, A: 0},
		updatedNode.Color.UnwrapOr(color.RGBA{}),
	)
	require.Equal(t, testNode.LastUpdate, updatedNode.LastUpdate)
}

// TestEdgeInsertionDeletion tests the basic CRUD operations for channel edges.
func TestEdgeInsertionDeletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	chanID := uint64(prand.Int63())
	outpoint := wire.OutPoint{
		Hash:  rev,
		Index: 9,
	}

	// Add the new edge to the database, this should proceed without any
	// errors.
	node1Pub, err := node1.PubKey()
	require.NoError(t, err, "unable to generate node key")
	node2Pub, err := node2.PubKey()
	require.NoError(t, err, "unable to generate node key")
	edgeInfo := models.ChannelEdgeInfo{
		ChannelID: chanID,
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		Features:     lnwire.EmptyFeatureVector(),
		ChannelPoint: outpoint,
		Capacity:     9000,
	}
	copy(edgeInfo.NodeKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.NodeKey2Bytes[:], node2Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey2Bytes[:], node2Pub.SerializeCompressed())

	require.NoError(t, graph.AddChannelEdge(ctx, &edgeInfo))
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo)

	// Show that trying to insert the same channel again will return the
	// expected error.
	err = graph.AddChannelEdge(ctx, &edgeInfo)
	require.ErrorIs(t, err, ErrEdgeAlreadyExist)

	// Ensure that both policies are returned as unknown (nil).
	_, e1, e2, err := graph.FetchChannelEdgesByID(chanID)
	require.NoError(t, err)
	require.Nil(t, e1)
	require.Nil(t, e2)

	// Next, attempt to delete the edge from the database, again this
	// should proceed without any issues.
	require.NoError(t, graph.DeleteChannelEdges(false, true, chanID))
	assertNoEdge(t, graph, chanID)

	// Ensure that any query attempts to lookup the delete channel edge are
	// properly deleted.
	_, _, _, err = graph.FetchChannelEdgesByOutpoint(&outpoint)
	require.ErrorIs(t, err, ErrEdgeNotFound)

	// Assert that if the edge is a zombie, then FetchChannelEdgesByID
	// still returns a populated models.ChannelEdgeInfo as its comment
	// description promises.
	edge, _, _, err := graph.FetchChannelEdgesByID(chanID)
	require.ErrorIs(t, err, ErrZombieEdge)
	require.NotNil(t, edge)

	isZombie, _, _, err := graph.IsZombieEdge(chanID)
	require.NoError(t, err)
	require.True(t, isZombie)

	// Finally, attempt to delete a (now) non-existent edge within the
	// database, this should result in an error.
	err = graph.DeleteChannelEdges(false, true, chanID)
	require.ErrorIs(t, err, ErrEdgeNotFound)
}

func createEdge(height, txIndex uint32, txPosition uint16, outPointIndex uint32,
	node1, node2 *models.Node) (models.ChannelEdgeInfo,
	lnwire.ShortChannelID) {

	shortChanID := lnwire.ShortChannelID{
		BlockHeight: height,
		TxIndex:     txIndex,
		TxPosition:  txPosition,
	}
	outpoint := wire.OutPoint{
		Hash:  rev,
		Index: outPointIndex,
	}

	node1Pub, _ := node1.PubKey()
	node2Pub, _ := node2.PubKey()
	edgeInfo := models.ChannelEdgeInfo{
		ChannelID: shortChanID.ToUint64(),
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		ChannelPoint:    outpoint,
		Capacity:        9000,
		ExtraOpaqueData: make([]byte, 0),
		Features:        lnwire.EmptyFeatureVector(),
	}

	copy(edgeInfo.NodeKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.NodeKey2Bytes[:], node2Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey2Bytes[:], node2Pub.SerializeCompressed())

	return edgeInfo, shortChanID
}

// TestDisconnectBlockAtHeight checks that the pruned state of the channel
// database is what we expect after calling DisconnectBlockAtHeight.
func TestDisconnectBlockAtHeight(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	sourceNode := createTestVertex(t)
	if err := graph.SetSourceNode(ctx, sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	var spendOutputs []*wire.OutPoint
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))

	// Prune the graph a few times to make sure we have entries in the
	// prune log.
	_, err := graph.PruneGraph(spendOutputs, &blockHash, 155)
	require.NoError(t, err, "unable to prune graph")
	var blockHash2 chainhash.Hash
	copy(blockHash2[:], bytes.Repeat([]byte{2}, 32))

	_, err = graph.PruneGraph(spendOutputs, &blockHash2, 156)
	require.NoError(t, err, "unable to prune graph")

	// We'll create 3 almost identical edges, so first create a helper
	// method containing all logic for doing so.

	// Create an edge which has its block height at 156.
	height := uint32(156)
	edgeInfo, _ := createEdge(height, 0, 0, 0, node1, node2)

	// Create an edge with block height 157. We give it
	// maximum values for tx index and position, to make
	// sure our database range scan get edges from the
	// entire range.
	edgeInfo2, _ := createEdge(
		height+1, math.MaxUint32&0x00ffffff, math.MaxUint16, 1,
		node1, node2,
	)

	// Create a third edge, this with a block height of 155.
	edgeInfo3, _ := createEdge(height-1, 0, 0, 2, node1, node2)

	// Now add all these new edges to the database.
	if err := graph.AddChannelEdge(ctx, &edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	if err := graph.AddChannelEdge(ctx, &edgeInfo2); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	if err := graph.AddChannelEdge(ctx, &edgeInfo3); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo)
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo2)
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo3)

	// Call DisconnectBlockAtHeight, which should prune every channel
	// that has a funding height of 'height' or greater.
	removed, err := graph.DisconnectBlockAtHeight(uint32(height))
	if err != nil {
		t.Fatalf("unable to prune %v", err)
	}
	assertNoEdge(t, graph, edgeInfo.ChannelID)
	assertNoEdge(t, graph, edgeInfo2.ChannelID)
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo3)

	// The two edges should have been removed.
	if len(removed) != 2 {
		t.Fatalf("expected two edges to be removed from graph, "+
			"only %d were", len(removed))
	}
	if removed[0].ChannelID != edgeInfo.ChannelID {
		t.Fatalf("expected edge to be removed from graph")
	}
	if removed[1].ChannelID != edgeInfo2.ChannelID {
		t.Fatalf("expected edge to be removed from graph")
	}

	// The two first edges should be removed from the db.
	_, _, has, isZombie, err := graph.HasChannelEdge(edgeInfo.ChannelID)
	require.NoError(t, err, "unable to query for edge")
	if has {
		t.Fatalf("edge1 was not pruned from the graph")
	}
	if isZombie {
		t.Fatal("reorged edge1 should not be marked as zombie")
	}
	_, _, has, isZombie, err = graph.HasChannelEdge(edgeInfo2.ChannelID)
	require.NoError(t, err, "unable to query for edge")
	if has {
		t.Fatalf("edge2 was not pruned from the graph")
	}
	if isZombie {
		t.Fatal("reorged edge2 should not be marked as zombie")
	}

	// Edge 3 should not be removed.
	_, _, has, isZombie, err = graph.HasChannelEdge(edgeInfo3.ChannelID)
	require.NoError(t, err, "unable to query for edge")
	if !has {
		t.Fatalf("edge3 was pruned from the graph")
	}
	if isZombie {
		t.Fatal("edge3 was marked as zombie")
	}

	// PruneTip should be set to the blockHash we specified for the block
	// at height 155.
	hash, h, err := graph.PruneTip()
	require.NoError(t, err, "unable to get prune tip")
	require.True(t, blockHash.IsEqual(hash))
	require.Equal(t, h, height-1)
}

func assertEdgeInfoEqual(t *testing.T, e1 *models.ChannelEdgeInfo,
	e2 *models.ChannelEdgeInfo) {

	if e1.ChannelID != e2.ChannelID {
		t.Fatalf("chan id's don't match: %v vs %v", e1.ChannelID,
			e2.ChannelID)
	}

	if e1.ChainHash != e2.ChainHash {
		t.Fatalf("chain hashes don't match: %v vs %v", e1.ChainHash,
			e2.ChainHash)
	}

	if !bytes.Equal(e1.NodeKey1Bytes[:], e2.NodeKey1Bytes[:]) {
		t.Fatalf("nodekey1 doesn't match")
	}
	if !bytes.Equal(e1.NodeKey2Bytes[:], e2.NodeKey2Bytes[:]) {
		t.Fatalf("nodekey2 doesn't match")
	}
	if !bytes.Equal(e1.BitcoinKey1Bytes[:], e2.BitcoinKey1Bytes[:]) {
		t.Fatalf("bitcoinkey1 doesn't match")
	}
	if !bytes.Equal(e1.BitcoinKey2Bytes[:], e2.BitcoinKey2Bytes[:]) {
		t.Fatalf("bitcoinkey2 doesn't match")
	}

	if !e1.Features.Equals(e2.Features.RawFeatureVector) {
		t.Fatalf("features don't match: %v vs %v", e1.Features,
			e2.Features)
	}

	require.True(t, bytes.Equal(
		e1.AuthProof.NodeSig1Bytes, e2.AuthProof.NodeSig1Bytes,
	))
	require.True(t, bytes.Equal(
		e1.AuthProof.NodeSig2Bytes, e2.AuthProof.NodeSig2Bytes,
	))
	require.True(t, bytes.Equal(
		e1.AuthProof.BitcoinSig1Bytes,
		e2.AuthProof.BitcoinSig1Bytes,
	))
	require.True(t, bytes.Equal(
		e1.AuthProof.BitcoinSig2Bytes, e2.AuthProof.BitcoinSig2Bytes,
	))

	if e1.ChannelPoint != e2.ChannelPoint {
		t.Fatalf("channel point match: %v vs %v", e1.ChannelPoint,
			e2.ChannelPoint)
	}

	if e1.Capacity != e2.Capacity {
		t.Fatalf("capacity doesn't match: %v vs %v", e1.Capacity,
			e2.Capacity)
	}

	if !bytes.Equal(e1.ExtraOpaqueData, e2.ExtraOpaqueData) {
		t.Fatalf("extra data doesn't match: %v vs %v",
			e2.ExtraOpaqueData, e2.ExtraOpaqueData)
	}
}

type createEdgeConfig struct {
	skipProofs bool
}

type createEdgeOpt func(*createEdgeConfig)

// withSkipProofs will let createChannelEdge create an edge without auth
// proofs. In this case, createChannelEdge will then also not create policies.
func withSkipProofs() createEdgeOpt {
	return func(cfg *createEdgeConfig) {
		cfg.skipProofs = true
	}
}

func createChannelEdge(node1, node2 *models.Node,
	options ...createEdgeOpt) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) {

	var opts createEdgeConfig
	for _, o := range options {
		o(&opts)
	}

	var (
		firstNode  [33]byte
		secondNode [33]byte
	)
	if bytes.Compare(node1.PubKeyBytes[:], node2.PubKeyBytes[:]) == -1 {
		firstNode = node1.PubKeyBytes
		secondNode = node2.PubKeyBytes
	} else {
		firstNode = node2.PubKeyBytes
		secondNode = node1.PubKeyBytes
	}

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	chanID := uint64(prand.Int63())
	outpoint := wire.OutPoint{
		Hash:  rev,
		Index: prand.Uint32(),
	}

	// Add the new edge to the database, this should proceed without any
	// errors.
	edgeInfo := &models.ChannelEdgeInfo{
		ChannelID:    chanID,
		ChainHash:    *chaincfg.MainNetParams.GenesisHash,
		ChannelPoint: outpoint,
		Capacity:     1000,
		ExtraOpaqueData: []byte{
			1, 1, 1,
			2, 2, 2, 2,
			3, 3, 3, 3, 3,
		},
		Features: lnwire.EmptyFeatureVector(),
	}
	copy(edgeInfo.NodeKey1Bytes[:], firstNode[:])
	copy(edgeInfo.NodeKey2Bytes[:], secondNode[:])
	copy(edgeInfo.BitcoinKey1Bytes[:], firstNode[:])
	copy(edgeInfo.BitcoinKey2Bytes[:], secondNode[:])

	if opts.skipProofs {
		return edgeInfo, nil, nil
	}

	edgeInfo.AuthProof = &models.ChannelAuthProof{
		NodeSig1Bytes:    testSig.Serialize(),
		NodeSig2Bytes:    testSig.Serialize(),
		BitcoinSig1Bytes: testSig.Serialize(),
		BitcoinSig2Bytes: testSig.Serialize(),
	}

	edge1 := &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                nextUpdateTime(),
		MessageFlags:              1,
		ChannelFlags:              0,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		MaxHTLC:                   13928598,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 3452352,
		ToNode:                    secondNode,
		ExtraOpaqueData:           []byte{1, 0},
	}
	edge2 := &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                nextUpdateTime(),
		MessageFlags:              1,
		ChannelFlags:              1,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		MaxHTLC:                   13928598,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 90392423,
		ToNode:                    firstNode,
		ExtraOpaqueData:           []byte{1, 0},
	}

	return edgeInfo, edge1, edge2
}

func TestEdgeInfoUpdates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node1, testFeatures)
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node2, testFeatures)

	// Create an edge and add it to the db.
	edgeInfo, edge1, edge2 := createChannelEdge(node1, node2)

	// Make sure inserting the policy at this point, before the edge info
	// is added, will fail.
	err := graph.UpdateEdgePolicy(ctx, edge1)
	require.ErrorIs(t, err, ErrEdgeNotFound)
	require.Len(t, graph.graphCache.nodeChannels, 0)

	// Add the edge info.
	if err := graph.AddChannelEdge(ctx, edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	assertEdgeWithNoPoliciesInCache(t, graph, edgeInfo)

	chanID := edgeInfo.ChannelID
	outpoint := edgeInfo.ChannelPoint

	// Next, insert both edge policies into the database, they should both
	// be inserted without any issues.
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	assertEdgeWithPolicyInCache(t, graph, edgeInfo, edge1, true)
	if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	assertEdgeWithPolicyInCache(t, graph, edgeInfo, edge2, false)

	// Check for existence of the edge within the database, it should be
	// found.
	_, _, found, isZombie, err := graph.HasChannelEdge(chanID)
	require.NoError(t, err, "unable to query for edge")
	if !found {
		t.Fatalf("graph should have of inserted edge")
	}
	if isZombie {
		t.Fatal("live edge should not be marked as zombie")
	}

	// We should also be able to retrieve the channelID only knowing the
	// channel point of the channel.
	dbChanID, err := graph.ChannelID(&outpoint)
	require.NoError(t, err, "unable to retrieve channel ID")
	if dbChanID != chanID {
		t.Fatalf("chan ID's mismatch, expected %v got %v", dbChanID,
			chanID)
	}

	// With the edges inserted, perform some queries to ensure that they've
	// been inserted properly.
	dbEdgeInfo, dbEdge1, dbEdge2, err := graph.FetchChannelEdgesByID(chanID)
	require.NoError(t, err, "unable to fetch channel by ID")
	if err := compareEdgePolicies(dbEdge1, edge1); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)

	// Next, attempt to query the channel edges according to the outpoint
	// of the channel.
	dbEdgeInfo, dbEdge1, dbEdge2, err = graph.FetchChannelEdgesByOutpoint(
		&outpoint,
	)
	require.NoError(t, err, "unable to fetch channel by ID")
	if err := compareEdgePolicies(dbEdge1, edge1); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)
}

// TestEdgePolicyCRUD tests basic CRUD operations for edge policies.
func TestEdgePolicyCRUD(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// Create an edge. Don't add it to the DB yet.
	edgeInfo, edge1, edge2 := createChannelEdge(node1, node2)

	updateAndAssertPolicies := func() {
		// Make copies of the policies before calling UpdateEdgePolicy
		// to avoid any data race's that can occur during async calls
		// that UpdateEdgePolicy may trigger.
		edge1 := copyEdgePolicy(edge1)
		edge2 := copyEdgePolicy(edge2)

		edge1.LastUpdate = nextUpdateTime()
		edge2.LastUpdate = nextUpdateTime()

		require.NoError(t, graph.UpdateEdgePolicy(ctx, edge1))
		require.NoError(t, graph.UpdateEdgePolicy(ctx, edge2))

		// Even though we assert at the DB level that any newer edge
		// update has a newer timestamp, we need to still gracefully
		// handle the case where the same exact policy is re-added since
		// it could be possible that our batch executor has two of the
		// same policy updates in the same batch.
		require.NoError(t, graph.UpdateEdgePolicy(ctx, edge1))

		// Use the ForEachChannel method to fetch the policies and
		// assert that the deserialized policies match the original
		// ones.
		err := graph.ForEachChannel(
			ctx, func(info *models.ChannelEdgeInfo,
				policy1 *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error {

				require.NoError(
					t, compareEdgePolicies(edge1, policy1),
				)
				require.NoError(
					t, compareEdgePolicies(edge2, policy2),
				)

				return nil
			}, func() {},
		)
		require.NoError(t, err)
	}

	// Make sure inserting the policy at this point, before the edge info
	// is added, will fail.
	require.ErrorIs(t, graph.UpdateEdgePolicy(ctx, edge1), ErrEdgeNotFound)

	// Now add the edge.
	require.NoError(t, graph.AddChannelEdge(ctx, edgeInfo))

	updateAndAssertPolicies()

	// Update one of the edges to have no extra opaque data.
	edge1.ExtraOpaqueData = nil

	updateAndAssertPolicies()

	// Update one of the edges to have ChannelFlags include a bit unknown
	// to us.
	edge1.ChannelFlags |= 1 << 6

	// Update the other edge to have MessageFlags include a bit unknown to
	// us.
	edge2.MessageFlags |= 1 << 4

	updateAndAssertPolicies()
}

func assertNodeInCache(t *testing.T, g *ChannelGraph, n *models.Node,
	expectedFeatures *lnwire.FeatureVector) {

	// Let's check the internal view first.
	require.Equal(
		t, expectedFeatures, g.graphCache.nodeFeatures[n.PubKeyBytes],
	)

	// The external view should reflect this as well. Except when we expect
	// the features to be nil internally, we return an empty feature vector
	// on the public interface instead.
	if expectedFeatures == nil {
		expectedFeatures = lnwire.EmptyFeatureVector()
	}
	features := g.graphCache.GetFeatures(n.PubKeyBytes)
	require.Equal(t, expectedFeatures, features)
}

func assertNodeNotInCache(t *testing.T, g *ChannelGraph, n route.Vertex) {
	_, ok := g.graphCache.nodeFeatures[n]
	require.False(t, ok)

	_, ok = g.graphCache.nodeChannels[n]
	require.False(t, ok)

	// We should get the default features for this node.
	features := g.graphCache.GetFeatures(n)
	require.Equal(t, lnwire.EmptyFeatureVector(), features)
}

func assertEdgeWithNoPoliciesInCache(t *testing.T, g *ChannelGraph,
	e *models.ChannelEdgeInfo) {

	// Let's check the internal view first.
	require.NotEmpty(t, g.graphCache.nodeChannels[e.NodeKey1Bytes])
	require.NotEmpty(t, g.graphCache.nodeChannels[e.NodeKey2Bytes])

	expectedNode1Channel := &DirectedChannel{
		ChannelID:    e.ChannelID,
		IsNode1:      true,
		OtherNode:    e.NodeKey2Bytes,
		Capacity:     e.Capacity,
		OutPolicySet: false,
		InPolicy:     nil,
	}
	require.Contains(
		t, g.graphCache.nodeChannels[e.NodeKey1Bytes], e.ChannelID,
	)
	require.Equal(
		t, expectedNode1Channel,
		g.graphCache.nodeChannels[e.NodeKey1Bytes][e.ChannelID],
	)

	expectedNode2Channel := &DirectedChannel{
		ChannelID:    e.ChannelID,
		IsNode1:      false,
		OtherNode:    e.NodeKey1Bytes,
		Capacity:     e.Capacity,
		OutPolicySet: false,
		InPolicy:     nil,
	}
	require.Contains(
		t, g.graphCache.nodeChannels[e.NodeKey2Bytes], e.ChannelID,
	)
	require.Equal(
		t, expectedNode2Channel,
		g.graphCache.nodeChannels[e.NodeKey2Bytes][e.ChannelID],
	)

	// The external view should reflect this as well.
	var foundChannel *DirectedChannel
	err := g.graphCache.ForEachChannel(
		e.NodeKey1Bytes, func(c *DirectedChannel) error {
			if c.ChannelID == e.ChannelID {
				foundChannel = c
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, foundChannel)
	require.Equal(t, expectedNode1Channel, foundChannel)

	err = g.graphCache.ForEachChannel(
		e.NodeKey2Bytes, func(c *DirectedChannel) error {
			if c.ChannelID == e.ChannelID {
				foundChannel = c
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, foundChannel)
	require.Equal(t, expectedNode2Channel, foundChannel)
}

func assertNoEdge(t *testing.T, g *ChannelGraph, chanID uint64) {
	// Make sure no channel in the cache has the given channel ID. If there
	// are no channels at all, that is fine as well.
	for _, channels := range g.graphCache.nodeChannels {
		for _, channel := range channels {
			require.NotEqual(t, channel.ChannelID, chanID)
		}
	}
}

func assertEdgeWithPolicyInCache(t *testing.T, g *ChannelGraph,
	e *models.ChannelEdgeInfo, p *models.ChannelEdgePolicy, policy1 bool) {

	// Check the internal state first.
	c1, ok := g.graphCache.nodeChannels[e.NodeKey1Bytes][e.ChannelID]
	require.True(t, ok)

	if policy1 {
		require.True(t, c1.OutPolicySet)
	} else {
		require.NotNil(t, c1.InPolicy)
		require.Equal(
			t, p.FeeProportionalMillionths,
			c1.InPolicy.FeeProportionalMillionths,
		)
	}

	c2, ok := g.graphCache.nodeChannels[e.NodeKey2Bytes][e.ChannelID]
	require.True(t, ok)

	if policy1 {
		require.NotNil(t, c2.InPolicy)
		require.Equal(
			t, p.FeeProportionalMillionths,
			c2.InPolicy.FeeProportionalMillionths,
		)
	} else {
		require.True(t, c2.OutPolicySet)
	}

	// Now for both nodes make sure that the external view is also correct.
	var (
		c1Ext *DirectedChannel
		c2Ext *DirectedChannel
	)
	require.NoError(t, g.graphCache.ForEachChannel(
		e.NodeKey1Bytes, func(c *DirectedChannel) error {
			c1Ext = c

			return nil
		},
	))
	require.NoError(t, g.graphCache.ForEachChannel(
		e.NodeKey2Bytes, func(c *DirectedChannel) error {
			c2Ext = c

			return nil
		},
	))

	// Only compare the fields that are actually copied, then compare the
	// values of the functions separately.
	require.Equal(t, c1, c1Ext.DeepCopy())
	require.Equal(t, c2, c2Ext.DeepCopy())
	if policy1 {
		require.Equal(
			t, p.FeeProportionalMillionths,
			c2Ext.InPolicy.FeeProportionalMillionths,
		)
		require.Equal(
			t, route.Vertex(e.NodeKey2Bytes),
			c2Ext.InPolicy.ToNodePubKey(),
		)
		require.Equal(t, testFeatures, c2Ext.InPolicy.ToNodeFeatures)
	} else {
		require.Equal(
			t, p.FeeProportionalMillionths,
			c1Ext.InPolicy.FeeProportionalMillionths,
		)
		require.Equal(
			t, route.Vertex(e.NodeKey1Bytes),
			c1Ext.InPolicy.ToNodePubKey(),
		)
		require.Equal(t, testFeatures, c1Ext.InPolicy.ToNodeFeatures)
	}
}

func randEdgePolicy(chanID uint64) *models.ChannelEdgePolicy {
	update := prand.Int63()

	return newEdgePolicy(chanID, update)
}

func copyEdgePolicy(p *models.ChannelEdgePolicy) *models.ChannelEdgePolicy {
	return &models.ChannelEdgePolicy{
		SigBytes:                  p.SigBytes,
		ChannelID:                 p.ChannelID,
		LastUpdate:                p.LastUpdate,
		MessageFlags:              p.MessageFlags,
		ChannelFlags:              p.ChannelFlags,
		TimeLockDelta:             p.TimeLockDelta,
		MinHTLC:                   p.MinHTLC,
		MaxHTLC:                   p.MaxHTLC,
		FeeBaseMSat:               p.FeeBaseMSat,
		FeeProportionalMillionths: p.FeeProportionalMillionths,
		ToNode:                    p.ToNode,
		ExtraOpaqueData:           p.ExtraOpaqueData,
	}
}

func newEdgePolicy(chanID uint64, updateTime int64) *models.ChannelEdgePolicy {
	return &models.ChannelEdgePolicy{
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(updateTime, 0),
		MessageFlags:              1,
		ChannelFlags:              0,
		TimeLockDelta:             uint16(prand.Int63()),
		MinHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		MaxHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		FeeBaseMSat:               lnwire.MilliSatoshi(prand.Int63()),
		FeeProportionalMillionths: lnwire.MilliSatoshi(prand.Int63()),
	}
}

// TestAddEdgeProof tests the ability to add an edge proof to an existing edge.
func TestAddEdgeProof(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Add an edge with no proof.
	node1 := createTestVertex(t)
	node2 := createTestVertex(t)
	edge1, _, _ := createChannelEdge(node1, node2, withSkipProofs())
	require.NoError(t, graph.AddChannelEdge(ctx, edge1))

	// Fetch the edge and assert that the proof is nil and that the rest
	// of the edge info is correct.
	dbEdge, _, _, err := graph.FetchChannelEdgesByID(edge1.ChannelID)
	require.NoError(t, err)
	require.Nil(t, dbEdge.AuthProof)
	require.Equal(t, edge1, dbEdge)

	// Now, add the edge proof.
	proof := &models.ChannelAuthProof{
		NodeSig1Bytes:    testSig.Serialize(),
		NodeSig2Bytes:    testSig.Serialize(),
		BitcoinSig1Bytes: testSig.Serialize(),
		BitcoinSig2Bytes: testSig.Serialize(),
	}

	// First, add the proof to the rest of the channel edge info and try
	// to call AddChannelEdge again - this should fail due to the channel
	// already existing.
	edge1.AuthProof = proof
	err = graph.AddChannelEdge(ctx, edge1)
	require.Error(t, err, ErrEdgeAlreadyExist)

	// Now add just the proof.
	scid1 := lnwire.NewShortChanIDFromInt(edge1.ChannelID)
	require.NoError(t, graph.AddEdgeProof(scid1, proof))

	// Fetch the edge again and assert that the proof is now set.
	dbEdge, _, _, err = graph.FetchChannelEdgesByID(edge1.ChannelID)
	require.NoError(t, err)
	require.NotNil(t, dbEdge.AuthProof)
	require.Equal(t, edge1, dbEdge)

	// For completeness, also test the case where we insert a new edge with
	// an edge proof. Show that the proof is present from the get go.
	edge2, _, _ := createChannelEdge(node1, node2)
	require.NoError(t, graph.AddChannelEdge(ctx, edge2))

	// Fetch the edge and assert that the proof is nil and that the rest
	// of the edge info is correct.
	dbEdge2, _, _, err := graph.FetchChannelEdgesByID(edge2.ChannelID)
	require.NoError(t, err)
	require.NotNil(t, dbEdge2.AuthProof)
	require.Equal(t, edge2, dbEdge2)
}

// TestForEachSourceNodeChannel tests that the ForEachSourceNodeChannel
// correctly iterates through the channels of the set source node.
func TestForEachSourceNodeChannel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Create a source node (A) and set it as such in the DB.
	nodeA := createTestVertex(t)
	require.NoError(t, graph.SetSourceNode(ctx, nodeA))

	// Now, create a few more nodes (B, C, D) along with some channels
	// between them. We'll create the following graph:
	//
	// 	A -- B -- D
	//  	|
	// 	C
	//
	// The graph includes a channel (B-D) that does not belong to the source
	// node along with 2 channels (A-B and A-C) that do belong to the source
	// node. For the A-B channel, we will let the source node set an
	// outgoing policy but for the A-C channel, we will set only an incoming
	// policy.

	nodeB := createTestVertex(t)
	nodeC := createTestVertex(t)
	nodeD := createTestVertex(t)

	abEdge, abPolicy1, abPolicy2 := createChannelEdge(nodeA, nodeB)
	require.NoError(t, graph.AddChannelEdge(ctx, abEdge))
	acEdge, acPolicy1, acPolicy2 := createChannelEdge(nodeA, nodeC)
	require.NoError(t, graph.AddChannelEdge(ctx, acEdge))
	bdEdge, _, _ := createChannelEdge(nodeB, nodeD)
	require.NoError(t, graph.AddChannelEdge(ctx, bdEdge))

	// Figure out which of the policies returned above are node A's so that
	// we know which to persist.
	//
	// First, set the outgoing policy for the A-B channel.
	abPolicyAOutgoing := abPolicy1
	if !bytes.Equal(abPolicy1.ToNode[:], nodeB.PubKeyBytes[:]) {
		abPolicyAOutgoing = abPolicy2
	}
	require.NoError(t, graph.UpdateEdgePolicy(ctx, abPolicyAOutgoing))

	// Now, set the incoming policy for the A-C channel.
	acPolicyAIncoming := acPolicy1
	if !bytes.Equal(acPolicy1.ToNode[:], nodeA.PubKeyBytes[:]) {
		acPolicyAIncoming = acPolicy2
	}
	require.NoError(t, graph.UpdateEdgePolicy(ctx, acPolicyAIncoming))

	type sourceNodeChan struct {
		otherNode  route.Vertex
		havePolicy bool
	}

	// Put together our expected source node channels.
	expectedSrcChans := map[wire.OutPoint]*sourceNodeChan{
		abEdge.ChannelPoint: {
			otherNode:  nodeB.PubKeyBytes,
			havePolicy: true,
		},
		acEdge.ChannelPoint: {
			otherNode:  nodeC.PubKeyBytes,
			havePolicy: false,
		},
	}

	// Now, we'll use the ForEachSourceNodeChannel and assert that it
	// returns the expected data in the call-back.
	err := graph.ForEachSourceNodeChannel(ctx, func(chanPoint wire.OutPoint,
		havePolicy bool, otherNode *models.Node) error {

		require.Contains(t, expectedSrcChans, chanPoint)
		expected := expectedSrcChans[chanPoint]

		require.Equal(
			t, expected.otherNode[:], otherNode.PubKeyBytes[:],
		)
		require.Equal(t, expected.havePolicy, havePolicy)

		delete(expectedSrcChans, chanPoint)

		return nil
	}, func() {})
	require.NoError(t, err)
	require.Empty(t, expectedSrcChans)
}

func TestGraphTraversal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test some of the graph traversal capabilities within
	// the DB, so we'll create a series of fake nodes to insert into the
	// graph. And we'll create 5 channels between each node pair.
	const numNodes = 20
	const numChannels = 5
	chanIndex, nodeList := fillTestGraph(t, graph, numNodes, numChannels)

	// Make an index of the node list for easy look up below.
	nodeIndex := make(map[route.Vertex]struct{})
	for _, node := range nodeList {
		nodeIndex[node.PubKeyBytes] = struct{}{}
	}

	// If we turn the channel graph cache _off_, then iterate through the
	// set of channels (to force the fall back), we should find all the
	// channel as well as the nodes included.
	graph.graphCache = nil
	err := graph.ForEachNodeCached(ctx, false, func(_ context.Context,
		node route.Vertex, _ []net.Addr,
		chans map[uint64]*DirectedChannel) error {

		if _, ok := nodeIndex[node]; !ok {
			return fmt.Errorf("node %x not found in graph", node)
		}

		for chanID := range chans {
			if _, ok := chanIndex[chanID]; !ok {
				return fmt.Errorf("chan %v not found in "+
					"graph", chanID)
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	// Iterate through all the known channels within the graph DB, once
	// again if the map is empty that indicates that all edges have
	// properly been reached.
	err = graph.ForEachChannel(ctx, func(ei *models.ChannelEdgeInfo,
		_ *models.ChannelEdgePolicy,
		_ *models.ChannelEdgePolicy) error {

		delete(chanIndex, ei.ChannelID)
		return nil
	}, func() {})
	require.NoError(t, err)
	require.Len(t, chanIndex, 0)

	// Finally, we want to test the ability to iterate over all the
	// outgoing channels for a particular node.
	numNodeChans := 0
	firstNode, secondNode := nodeList[0], nodeList[1]
	err = graph.ForEachNodeChannel(
		ctx, firstNode.PubKeyBytes,
		func(_ *models.ChannelEdgeInfo, outEdge,
			inEdge *models.ChannelEdgePolicy) error {

			// All channels between first and second node should
			// have fully (both sides) specified policies.
			if inEdge == nil || outEdge == nil {
				return fmt.Errorf("channel policy not present")
			}

			// Each should indicate that it's outgoing (pointed
			// towards the second node).
			if !bytes.Equal(
				outEdge.ToNode[:], secondNode.PubKeyBytes[:],
			) {

				return fmt.Errorf("wrong outgoing edge")
			}

			// The incoming edge should also indicate that it's
			// pointing to the origin node.
			if !bytes.Equal(
				inEdge.ToNode[:], firstNode.PubKeyBytes[:],
			) {

				return fmt.Errorf("wrong outgoing edge")
			}

			numNodeChans++

			return nil
		}, func() {},
	)
	require.NoError(t, err)
	require.Equal(t, numChannels, numNodeChans)
}

// TestGraphTraversalCacheable tests that the memory optimized node traversal is
// working correctly.
func TestGraphTraversalCacheable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test some of the graph traversal capabilities within
	// the DB, so we'll create a series of fake nodes to insert into the
	// graph. And we'll create 5 channels between the first two nodes.
	const numNodes = 20
	const numChannels = 5
	chanIndex, _ := fillTestGraph(t, graph, numNodes, numChannels)

	// Create a map of all nodes with the iteration we know works (because
	// it is tested in another test).
	nodeMap := make(map[route.Vertex]struct{})
	err := graph.ForEachNode(ctx, func(n *models.Node) error {
		nodeMap[n.PubKeyBytes] = struct{}{}

		return nil
	}, func() {})
	require.NoError(t, err)
	require.Len(t, nodeMap, numNodes)

	// Iterate through all the known channels within the graph DB by
	// iterating over each node, once again if the map is empty that
	// indicates that all edges have properly been reached.
	var nodes []route.Vertex
	err = graph.ForEachNodeCacheable(ctx, func(node route.Vertex,
		features *lnwire.FeatureVector) error {

		delete(nodeMap, node)
		nodes = append(nodes, node)

		return nil
	}, func() {
		nodes = nil
	})
	require.NoError(t, err)
	require.Len(t, nodeMap, 0)

	// Duplicate the map before we start deleting from it so that we can
	// check that both the cached and db version of
	// ForEachNodeDirectedChannel works as expected here.
	chanIndex2 := make(map[uint64]struct{})
	for k, v := range chanIndex {
		chanIndex2[k] = v
	}

	for _, node := range nodes {
		// Query the ChannelGraph which uses the cache to iterate
		// through the channels for each node.
		err = graph.ForEachNodeDirectedChannel(
			node, func(d *DirectedChannel) error {
				delete(chanIndex, d.ChannelID)
				return nil
			}, func() {},
		)
		require.NoError(t, err)

		// Now skip the cache and query the DB directly.
		err = graph.V1Store.ForEachNodeDirectedChannel(
			node, func(d *DirectedChannel) error {
				delete(chanIndex2, d.ChannelID)
				return nil
			}, func() {},
		)
		require.NoError(t, err)
	}
	require.Len(t, chanIndex, 0)
	require.Len(t, chanIndex2, 0)
}

func TestGraphCacheTraversal(t *testing.T) {
	t.Parallel()

	graph := MakeTestGraph(t)

	// We'd like to test some of the graph traversal capabilities within
	// the DB, so we'll create a series of fake nodes to insert into the
	// graph. And we'll create 5 channels between each node pair.
	const numNodes = 20
	const numChannels = 5
	chanIndex, nodeList := fillTestGraph(t, graph, numNodes, numChannels)

	// Iterate through all the known channels within the graph DB, once
	// again if the map is empty that indicates that all edges have
	// properly been reached.
	numNodeChans := 0
	for _, node := range nodeList {
		node := node

		err := graph.graphCache.ForEachChannel(
			node.PubKeyBytes, func(d *DirectedChannel) error {
				delete(chanIndex, d.ChannelID)

				if !d.OutPolicySet || d.InPolicy == nil {
					return fmt.Errorf("channel policy " +
						"not present")
				}

				// The incoming edge should also indicate that
				// it's pointing to the origin node.
				inPolicyNodeKey := d.InPolicy.ToNodePubKey()
				if !bytes.Equal(
					inPolicyNodeKey[:], node.PubKeyBytes[:],
				) {

					return fmt.Errorf("wrong outgoing edge")
				}

				numNodeChans++

				return nil
			},
		)
		require.NoError(t, err)
	}
	require.Len(t, chanIndex, 0)

	// We count the channels for both nodes, so there should be double the
	// amount now. Except for the very last node, that doesn't have any
	// channels to make the loop easier in fillTestGraph().
	require.Equal(t, numChannels*2*(numNodes-1), numNodeChans)
}

func fillTestGraph(t testing.TB, graph *ChannelGraph, numNodes,
	numChannels int) (map[uint64]struct{}, []*models.Node) {

	ctx := t.Context()

	nodes := make([]*models.Node, numNodes)
	nodeIndex := map[string]struct{}{}
	for i := 0; i < numNodes; i++ {
		node := createTestVertex(t)

		nodes[i] = node
		nodeIndex[node.Alias.UnwrapOr("")] = struct{}{}
	}

	// Add each of the nodes into the graph, they should be inserted
	// without error.
	for _, node := range nodes {
		require.NoError(t, graph.AddNode(ctx, node))
	}

	// Iterate over each node as returned by the graph, if all nodes are
	// reached, then the map created above should be empty.
	err := graph.ForEachNode(ctx, func(n *models.Node) error {
		delete(nodeIndex, n.Alias.UnwrapOr(""))
		return nil
	}, func() {})
	require.NoError(t, err)
	require.Len(t, nodeIndex, 0)

	// Create a number of channels between each of the node pairs generated
	// above. This will result in numChannels*(numNodes-1) channels.
	chanIndex := map[uint64]struct{}{}
	for n := 0; n < numNodes-1; n++ {
		node1 := nodes[n]
		node2 := nodes[n+1]
		if bytes.Compare(
			node1.PubKeyBytes[:], node2.PubKeyBytes[:],
		) == -1 {
			node1, node2 = node2, node1
		}

		for i := 0; i < numChannels; i++ {
			txHash := sha256.Sum256([]byte{byte(i)})
			chanID := uint64((n << 8) + i + 1)
			op := wire.OutPoint{
				Hash:  txHash,
				Index: 0,
			}

			edgeInfo := models.ChannelEdgeInfo{
				ChannelID: chanID,
				ChainHash: *chaincfg.MainNetParams.GenesisHash,
				AuthProof: &models.ChannelAuthProof{
					NodeSig1Bytes:    testSig.Serialize(),
					NodeSig2Bytes:    testSig.Serialize(),
					BitcoinSig1Bytes: testSig.Serialize(),
					BitcoinSig2Bytes: testSig.Serialize(),
				},
				Features:     lnwire.EmptyFeatureVector(),
				ChannelPoint: op,
				Capacity:     1000,
			}
			copy(edgeInfo.NodeKey1Bytes[:], node1.PubKeyBytes[:])
			copy(edgeInfo.NodeKey2Bytes[:], node2.PubKeyBytes[:])
			copy(edgeInfo.BitcoinKey1Bytes[:], node1.PubKeyBytes[:])
			copy(edgeInfo.BitcoinKey2Bytes[:], node2.PubKeyBytes[:])
			err := graph.AddChannelEdge(ctx, &edgeInfo)
			require.NoError(t, err)

			// Create and add an edge with random data that points
			// from node1 -> node2.
			edge := randEdgePolicy(chanID)
			edge.ChannelFlags = 0
			edge.ToNode = node2.PubKeyBytes
			edge.SigBytes = testSig.Serialize()
			require.NoError(t, graph.UpdateEdgePolicy(ctx, edge))

			// Create another random edge that points from
			// node2 -> node1 this time.
			edge = randEdgePolicy(chanID)
			edge.ChannelFlags = 1
			edge.ToNode = node1.PubKeyBytes
			edge.SigBytes = testSig.Serialize()
			require.NoError(t, graph.UpdateEdgePolicy(ctx, edge))

			chanIndex[chanID] = struct{}{}
		}
	}

	return chanIndex, nodes
}

func assertPruneTip(t *testing.T, graph *ChannelGraph,
	blockHash *chainhash.Hash, blockHeight uint32) {

	pruneHash, pruneHeight, err := graph.PruneTip()
	if err != nil {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: unable to fetch prune tip: %v", line, err)
	}
	if !bytes.Equal(blockHash[:], pruneHash[:]) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line: %v, prune tips don't match, expected %x got %x",
			line, blockHash, pruneHash)
	}
	if pruneHeight != blockHeight {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: prune heights don't match, expected %v "+
			"got %v", line, blockHeight, pruneHeight)
	}
}

func assertNumChans(t *testing.T, graph *ChannelGraph, n int) {
	numChans := 0
	err := graph.ForEachChannel(
		t.Context(), func(*models.ChannelEdgeInfo,
			*models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy) error {

			numChans++
			return nil
		}, func() {
			numChans = 0
		},
	)
	require.NoError(t, err)
	require.Equal(t, n, numChans)
}

func assertNumNodes(t *testing.T, graph *ChannelGraph, n int) {
	numNodes := 0
	err := graph.ForEachNode(t.Context(),
		func(_ *models.Node) error {
			numNodes++

			return nil
		}, func() {})
	if err != nil {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: unable to scan nodes: %v", line, err)
	}

	if numNodes != n {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: expected %v nodes, got %v", line, n,
			numNodes)
	}
}

func assertChanViewEqual(t *testing.T, a []EdgePoint, b []EdgePoint) {
	if len(a) != len(b) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: chan views don't match", line)
	}

	chanViewSet := make(map[wire.OutPoint]struct{})
	for _, op := range a {
		chanViewSet[op.OutPoint] = struct{}{}
	}

	for _, op := range b {
		if _, ok := chanViewSet[op.OutPoint]; !ok {
			_, _, line, _ := runtime.Caller(1)
			t.Fatalf("line %v: chanPoint(%v) not found in first "+
				"view", line, op)
		}
	}
}

func assertChanViewEqualChanPoints(t *testing.T, a []EdgePoint,
	b []*wire.OutPoint) {

	if len(a) != len(b) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: chan views don't match", line)
	}

	chanViewSet := make(map[wire.OutPoint]struct{})
	for _, op := range a {
		chanViewSet[op.OutPoint] = struct{}{}
	}

	for _, op := range b {
		if _, ok := chanViewSet[*op]; !ok {
			_, _, line, _ := runtime.Caller(1)
			t.Fatalf("line %v: chanPoint(%v) not found in first "+
				"view", line, op)
		}
	}
}

func TestGraphPruning(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	sourceNode := createTestVertex(t)
	if err := graph.SetSourceNode(ctx, sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// As initial set up for the test, we'll create a graph with 5 vertexes
	// and enough edges to create a fully connected graph. The graph will
	// be rather simple, representing a straight line.
	const numNodes = 5
	graphNodes := make([]*models.Node, numNodes)
	for i := 0; i < numNodes; i++ {
		node := createTestVertex(t)

		if err := graph.AddNode(ctx, node); err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		graphNodes[i] = node
	}

	// With the vertexes created, we'll next create a series of channels
	// between them.
	channelPoints := make([]*wire.OutPoint, 0, numNodes-1)
	edgePoints := make([]EdgePoint, 0, numNodes-1)
	for i := 0; i < numNodes-1; i++ {
		txHash := sha256.Sum256([]byte{byte(i)})
		chanID := uint64(i + 1)
		op := wire.OutPoint{
			Hash:  txHash,
			Index: 0,
		}

		channelPoints = append(channelPoints, &op)

		edgeInfo := models.ChannelEdgeInfo{
			ChannelID: chanID,
			ChainHash: *chaincfg.MainNetParams.GenesisHash,
			AuthProof: &models.ChannelAuthProof{
				NodeSig1Bytes:    testSig.Serialize(),
				NodeSig2Bytes:    testSig.Serialize(),
				BitcoinSig1Bytes: testSig.Serialize(),
				BitcoinSig2Bytes: testSig.Serialize(),
			},
			Features:     lnwire.EmptyFeatureVector(),
			ChannelPoint: op,
			Capacity:     1000,
		}
		copy(edgeInfo.NodeKey1Bytes[:], graphNodes[i].PubKeyBytes[:])
		copy(edgeInfo.NodeKey2Bytes[:], graphNodes[i+1].PubKeyBytes[:])
		copy(edgeInfo.BitcoinKey1Bytes[:], graphNodes[i].PubKeyBytes[:])
		copy(
			edgeInfo.BitcoinKey2Bytes[:],
			graphNodes[i+1].PubKeyBytes[:],
		)
		if err := graph.AddChannelEdge(ctx, &edgeInfo); err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		pkScript, err := genMultiSigP2WSH(
			edgeInfo.BitcoinKey1Bytes[:],
			edgeInfo.BitcoinKey2Bytes[:],
		)
		if err != nil {
			t.Fatalf("unable to gen multi-sig p2wsh: %v", err)
		}
		edgePoints = append(edgePoints, EdgePoint{
			FundingPkScript: pkScript,
			OutPoint:        op,
		})

		// Create and add an edge with random data that points from
		// node_i -> node_i+1
		edge := randEdgePolicy(chanID)
		edge.ChannelFlags = 0
		edge.ToNode = graphNodes[i].PubKeyBytes
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node_i+1 ->
		// node_i this time.
		edge = randEdgePolicy(chanID)
		edge.ChannelFlags = 1
		edge.ToNode = graphNodes[i].PubKeyBytes
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}
	}

	// With all the channel points added, we'll consult the graph to ensure
	// it has the same channel view as the one we just constructed.
	channelView, err := graph.ChannelView()
	require.NoError(t, err, "unable to get graph channel view")
	assertChanViewEqual(t, channelView, edgePoints)

	// Now with our test graph created, we can test the pruning
	// capabilities of the channel graph.

	// First we create a mock block that ends up closing the first two
	// channels.
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))
	blockHeight := uint32(1)
	block := channelPoints[:2]
	prunedChans, err := graph.PruneGraph(block, &blockHash, blockHeight)
	require.NoError(t, err, "unable to prune graph")
	if len(prunedChans) != 2 {
		t.Fatalf("incorrect number of channels pruned: "+
			"expected %v, got %v", 2, prunedChans)
	}

	// Now ensure that the prune tip has been updated.
	assertPruneTip(t, graph, &blockHash, blockHeight)

	// Count up the number of channels known within the graph, only 2
	// should be remaining.
	assertNumChans(t, graph, 2)

	// Those channels should also be missing from the channel view.
	channelView, err = graph.ChannelView()
	require.NoError(t, err, "unable to get graph channel view")
	assertChanViewEqualChanPoints(t, channelView, channelPoints[2:])

	// Next we'll create a block that doesn't close any channels within the
	// graph to test the negative error case.
	fakeHash := sha256.Sum256([]byte("test prune"))
	nonChannel := &wire.OutPoint{
		Hash:  fakeHash,
		Index: 9,
	}
	blockHash = sha256.Sum256(blockHash[:])
	blockHeight = 2
	prunedChans, err = graph.PruneGraph(
		[]*wire.OutPoint{nonChannel}, &blockHash, blockHeight,
	)
	require.NoError(t, err, "unable to prune graph")

	// No channels should have been detected as pruned.
	if len(prunedChans) != 0 {
		t.Fatalf("channels were pruned but shouldn't have been")
	}

	// Once again, the prune tip should have been updated. We should still
	// see both channels and their participants, along with the source node.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	assertNumChans(t, graph, 2)
	assertNumNodes(t, graph, 4)

	// Finally, create a block that prunes the remainder of the channels
	// from the graph.
	blockHash = sha256.Sum256(blockHash[:])
	blockHeight = 3
	prunedChans, err = graph.PruneGraph(
		channelPoints[2:], &blockHash, blockHeight,
	)
	require.NoError(t, err, "unable to prune graph")

	// The remainder of the channels should have been pruned from the
	// graph.
	if len(prunedChans) != 2 {
		t.Fatalf("incorrect number of channels pruned: "+
			"expected %v, got %v", 2, len(prunedChans))
	}

	// The prune tip should be updated, no channels should be found, and
	// only the source node should remain within the current graph.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	assertNumChans(t, graph, 0)
	assertNumNodes(t, graph, 1)

	// Finally, the channel view at this point in the graph should now be
	// completely empty.  Those channels should also be missing from the
	// channel view.
	channelView, err = graph.ChannelView()
	require.NoError(t, err, "unable to get graph channel view")
	if len(channelView) != 0 {
		t.Fatalf("channel view should be empty, instead have: %v",
			channelView)
	}
}

// TestHighestChanID tests that we're able to properly retrieve the highest
// known channel ID in the database.
func TestHighestChanID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// If we don't yet have any channels in the database, then we should
	// get a channel ID of zero if we ask for the highest channel ID.
	bestID, err := graph.HighestChanID(ctx)
	require.NoError(t, err, "unable to get highest ID")
	if bestID != 0 {
		t.Fatalf("best ID w/ no chan should be zero, is instead: %v",
			bestID)
	}

	// Next, we'll insert two channels into the database, with each channel
	// connecting the same two nodes.
	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// The first channel with be at height 10, while the other will be at
	// height 100.
	edge1, _ := createEdge(10, 0, 0, 0, node1, node2)
	edge2, chanID2 := createEdge(100, 0, 0, 0, node1, node2)

	if err := graph.AddChannelEdge(ctx, &edge1); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	if err := graph.AddChannelEdge(ctx, &edge2); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Now that the edges has been inserted, we'll query for the highest
	// known channel ID in the database.
	bestID, err = graph.HighestChanID(ctx)
	require.NoError(t, err, "unable to get highest ID")

	if bestID != chanID2.ToUint64() {
		t.Fatalf("expected %v got %v for best chan ID: ",
			chanID2.ToUint64(), bestID)
	}

	// If we add another edge, then the current best chan ID should be
	// updated as well.
	edge3, chanID3 := createEdge(1000, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(ctx, &edge3); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	bestID, err = graph.HighestChanID(ctx)
	require.NoError(t, err, "unable to get highest ID")

	if bestID != chanID3.ToUint64() {
		t.Fatalf("expected %v got %v for best chan ID: ",
			chanID3.ToUint64(), bestID)
	}
}

// TestChanUpdatesInHorizon tests the we're able to properly retrieve all known
// channel updates within a specific time horizon. It also tests that upon
// insertion of a new edge, the edge update index is updated properly.
func TestChanUpdatesInHorizon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// If we issue an arbitrary query before any channel updates are
	// inserted in the database, we should get zero results.
	chanIter := graph.ChanUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)

	chanUpdates, err := fn.CollectErr(chanIter)
	require.NoError(t, err, "unable to updates for updates")

	if len(chanUpdates) != 0 {
		t.Fatalf("expected 0 chan updates, instead got %v",
			len(chanUpdates))
	}

	// We'll start by creating two nodes which will seed our test graph.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll now create 10 channels between the two nodes, with update
	// times 10 seconds after each other.
	const numChans = 10
	startTime := time.Unix(1234, 0)
	endTime := startTime
	edges := make([]ChannelEdge, 0, numChans)
	for i := 0; i < numChans; i++ {
		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(ctx, &channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		edge1UpdateTime := endTime
		edge2UpdateTime := edge1UpdateTime.Add(time.Second)
		endTime = endTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(
			chanID.ToUint64(), edge1UpdateTime.Unix(),
		)
		edge1.ChannelFlags = 0
		edge1.ToNode = node2.PubKeyBytes
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(
			chanID.ToUint64(), edge2UpdateTime.Unix(),
		)
		edge2.ChannelFlags = 1
		edge2.ToNode = node1.PubKeyBytes
		edge2.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edges = append(edges, ChannelEdge{
			Info:    &channel,
			Policy1: edge1,
			Policy2: edge2,
		})
	}

	// With our channels loaded, we'll now start our series of queries.
	queryCases := []struct {
		start time.Time
		end   time.Time

		resp []ChannelEdge
	}{
		// If we query for a time range that's strictly below our set
		// of updates, then we'll get an empty result back.
		{
			start: time.Unix(100, 0),
			end:   time.Unix(200, 0),
		},

		// If we query for a time range that's well beyond our set of
		// updates, we should get an empty set of results back.
		{
			start: time.Unix(99999, 0),
			end:   time.Unix(999999, 0),
		},

		// If we query for the start time, and 10 seconds directly
		// after it, we should only get a single update, that first
		// one.
		{
			start: time.Unix(1234, 0),
			end:   startTime.Add(time.Second * 10),

			resp: []ChannelEdge{edges[0]},
		},

		// If we add 10 seconds past the first update, and then
		// subtract 10 from the last update, then we should only get
		// the 8 edges in the middle.
		{
			start: startTime.Add(time.Second * 10),
			end:   endTime.Add(-time.Second * 10),

			resp: edges[1:9],
		},

		// If we use the start and end time as is, we should get the
		// entire range.
		{
			start: startTime,
			end:   endTime,

			resp: edges,
		},
	}
	for _, queryCase := range queryCases {
		respIter := graph.ChanUpdatesInHorizon(
			queryCase.start, queryCase.end,
		)

		resp, err := fn.CollectErr(respIter)
		if err != nil {
			t.Fatalf("unable to query for updates: %v", err)
		}

		if len(resp) != len(queryCase.resp) {
			t.Fatalf("expected %v chans, got %v chans",
				len(queryCase.resp), len(resp))

		}

		for i := 0; i < len(resp); i++ {
			chanExp := queryCase.resp[i]
			chanRet := resp[i]

			assertEdgeInfoEqual(t, chanExp.Info, chanRet.Info)

			err = compareEdgePolicies(
				chanExp.Policy1, chanRet.Policy1,
			)
			require.NoError(t, err)

			err = compareEdgePolicies(
				chanExp.Policy2, chanRet.Policy2,
			)
			require.NoError(t, err)
		}
	}
}

// TestNodeUpdatesInHorizon tests that we're able to properly scan and retrieve
// the most recent node updates within a particular time horizon.
func TestNodeUpdatesInHorizon(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	startTime := time.Unix(1234, 0)
	endTime := startTime

	// If we issue an arbitrary query before we insert any nodes into the
	// database, then we shouldn't get any results back.
	nodeUpdatesIter := graph.NodeUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)
	nodeUpdates, err := fn.CollectErr(nodeUpdatesIter)
	require.NoError(t, err, "unable to query for node updates")
	require.Len(t, nodeUpdates, 0)

	// We'll create 10 node announcements, each with an update timestamp 10
	// seconds after the other.
	const numNodes = 10
	nodeAnns := make([]models.Node, 0, numNodes)
	for i := 0; i < numNodes; i++ {
		nodeAnn := createTestVertex(t)

		// The node ann will use the current end time as its last
		// update them, then we'll add 10 seconds in order to create
		// the proper update time for the next node announcement.
		updateTime := endTime
		endTime = updateTime.Add(time.Second * 10)

		nodeAnn.LastUpdate = updateTime

		nodeAnns = append(nodeAnns, *nodeAnn)

		require.NoError(t, graph.AddNode(ctx, nodeAnn))
	}

	queryCases := []struct {
		start time.Time
		end   time.Time

		resp []models.Node
	}{
		// If we query for a time range that's strictly below our set
		// of updates, then we'll get an empty result back.
		{
			start: time.Unix(100, 0),
			end:   time.Unix(200, 0),
		},

		// If we query for a time range that's well beyond our set of
		// updates, we should get an empty set of results back.
		{
			start: time.Unix(99999, 0),
			end:   time.Unix(999999, 0),
		},

		// If we skip he first time epoch with out start time, then we
		// should get back every now but the first.
		{
			start: startTime.Add(time.Second * 10),
			end:   endTime,

			resp: nodeAnns[1:],
		},

		// If we query for the range as is, we should get all 10
		// announcements back.
		{
			start: startTime,
			end:   endTime,

			resp: nodeAnns,
		},

		// If we reduce the ending time by 1 nanosecond before the last
		// node's timestamp, then we should get all but the last node.
		{
			start: startTime,
			end:   endTime.Add(-time.Second*10 - time.Nanosecond),

			resp: nodeAnns[:9],
		},
	}
	for _, queryCase := range queryCases {
		iter := graph.NodeUpdatesInHorizon(
			queryCase.start, queryCase.end,
		)

		resp, err := fn.CollectErr(iter)
		require.NoError(t, err, "unable to query for node updates")
		require.Len(t, resp, len(queryCase.resp))

		for i := 0; i < len(resp); i++ {
			compareNodes(t, &queryCase.resp[i], resp[i])
		}
	}
}

// testNodeUpdatesWithBatchSize is a helper function that tests node updates
// with a specific batch size to ensure the iterator works correctly across
// batch boundaries.
func testNodeUpdatesWithBatchSize(t *testing.T, ctx context.Context,
	batchSize int) {

	// Create a fresh graph for each test.
	testGraph := MakeTestGraph(t)

	// Add 25 nodes with increasing timestamps.
	startTime := time.Unix(1234567890, 0)
	var nodeAnns []models.Node

	for i := 0; i < 25; i++ {
		nodeAnn := createTestVertex(t)
		nodeAnn.LastUpdate = startTime.Add(
			time.Duration(i) * time.Hour,
		)
		nodeAnns = append(nodeAnns, *nodeAnn)
		require.NoError(
			t, testGraph.AddNode(ctx, nodeAnn),
		)
	}

	testCases := []struct {
		name  string
		start time.Time
		end   time.Time
		want  int
	}{
		{
			name:  "all nodes",
			start: startTime,
			end:   startTime.Add(26 * time.Hour),
			want:  25,
		},
		{
			name:  "first batch only",
			start: startTime,
			end: startTime.Add(
				time.Duration(
					min(batchSize, 25)-1,
				) * time.Hour,
			),
			want: min(batchSize, 25),
		},
		{
			name:  "cross batch boundary",
			start: startTime,
			end: startTime.Add(
				time.Duration(
					min(batchSize, 24),
				) * time.Hour,
			),
			want: min(batchSize+1, 25),
		},
		{
			name: "exact boundary",
			start: func() time.Time {
				// Test querying exactly at a
				// batch boundary.
				if batchSize <= 25 {
					return startTime.Add(
						time.Duration(
							batchSize-1,
						) * time.Hour,
					)
				}

				// For batch sizes > 25, test
				// beyond our data range.
				return startTime.Add(
					time.Duration(25) * time.Hour,
				)
			}(),
			end: func() time.Time {
				if batchSize <= 25 {
					return startTime.Add(
						time.Duration(
							batchSize-1,
						) * time.Hour,
					)
				}

				return startTime.Add(
					time.Duration(25) * time.Hour,
				)
			}(),
			want: func() int {
				if batchSize <= 25 {
					return 1
				}

				// No nodes exist at hour 25 or
				// beyond.
				return 0
			}(),
		},
		{
			name:  "empty range before",
			start: startTime.Add(-time.Hour),
			end:   startTime.Add(-time.Minute),
			want:  0,
		},
		{
			name:  "empty range after",
			start: startTime.Add(30 * time.Hour),
			end:   startTime.Add(40 * time.Hour),
			want:  0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			iter := testGraph.NodeUpdatesInHorizon(
				tc.start, tc.end,
				WithNodeUpdateIterBatchSize(
					batchSize,
				),
			)

			nodes, err := fn.CollectErr(iter)
			require.NoError(t, err)
			require.Len(
				t, nodes, tc.want,
				"expected %d nodes, got %d",
				tc.want, len(nodes),
			)

			// Verify nodes are in the correct time
			// order.
			for i := 1; i < len(nodes); i++ {
				require.True(t,
					nodes[i-1].LastUpdate.Before(
						nodes[i].LastUpdate,
					) || nodes[i-1].LastUpdate.Equal(
						nodes[i].LastUpdate,
					),
					"nodes should be in "+
						"chronological order",
				)
			}
		})
	}
}

// TestNodeUpdatesInHorizonBoundaryConditions tests the iterator boundary
// conditions, specifically around batch boundaries and edge cases.
func TestNodeUpdatesInHorizonBoundaryConditions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Test with various batch sizes to ensure the iterator works correctly
	// across batch boundaries.
	batchSizes := []int{1, 3, 5, 10, 25, 100}

	for _, batchSize := range batchSizes {
		testName := fmt.Sprintf("BatchSize%d", batchSize)
		t.Run(testName, func(t *testing.T) {
			testNodeUpdatesWithBatchSize(t, ctx, batchSize)
		})
	}
}

// TestNodeUpdatesInHorizonEarlyTermination tests that the iterator properly
// handles early termination when the caller stops iterating.
func TestNodeUpdatesInHorizonEarlyTermination(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll start by creating 100 nodes, each with an update time spaced
	// one hour apart.
	startTime := time.Unix(1234567890, 0)
	for i := 0; i < 100; i++ {
		nodeAnn := createTestVertex(t)
		nodeAnn.LastUpdate = startTime.Add(time.Duration(i) * time.Hour)
		require.NoError(t, graph.AddNode(ctx, nodeAnn))
	}

	// Test early termination at various points
	terminationPoints := []int{0, 1, 5, 10, 23, 50, 99}

	for _, stopAt := range terminationPoints {
		t.Run(fmt.Sprintf("StopAt%d", stopAt), func(t *testing.T) {
			iter := graph.NodeUpdatesInHorizon(
				startTime, startTime.Add(200*time.Hour),
				WithNodeUpdateIterBatchSize(10),
			)

			// Collect only up to stopAt nodes, breaking afterwards.
			var collected []*models.Node
			count := 0
			for node := range iter {
				if count >= stopAt {
					break
				}
				collected = append(collected, node)
				count++
			}

			require.Len(
				t, collected, stopAt,
				"should have collected exactly %d nodes",
				stopAt,
			)
		})
	}
}

// TestChanUpdatesInHorizonBoundaryConditions tests the channel iterator
// boundary conditions.
func TestChanUpdatesInHorizonBoundaryConditions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	batchSizes := []int{1, 3, 5, 10}

	for _, batchSize := range batchSizes {
		testName := fmt.Sprintf("BatchSize%d", batchSize)
		t.Run(testName, func(t *testing.T) {
			// Create a fresh graph for each test, then add two new
			// nodes to the graph.
			graph := MakeTestGraph(t)
			node1 := createTestVertex(t)
			node2 := createTestVertex(t)
			require.NoError(t, graph.AddNode(ctx, node1))
			require.NoError(t, graph.AddNode(ctx, node2))

			// Next, we'll create 25 channels between the two nodes,
			// each with increasing timestamps.
			startTime := time.Unix(1234567890, 0)
			const numChans = 25

			for i := 0; i < numChans; i++ {
				updateTime := startTime.Add(
					time.Duration(i) * time.Hour,
				)

				channel, chanID := createEdge(
					uint32(i*10), 0, 0, 0, node1, node2,
				)
				require.NoError(
					t, graph.AddChannelEdge(ctx, &channel),
				)

				edge1 := newEdgePolicy(
					chanID.ToUint64(), updateTime.Unix(),
				)
				edge1.ChannelFlags = 0
				edge1.ToNode = node2.PubKeyBytes
				edge1.SigBytes = testSig.Serialize()
				require.NoError(
					t, graph.UpdateEdgePolicy(ctx, edge1),
				)

				edge2 := newEdgePolicy(
					chanID.ToUint64(), updateTime.Unix(),
				)
				edge2.ChannelFlags = 1
				edge2.ToNode = node1.PubKeyBytes
				edge2.SigBytes = testSig.Serialize()
				require.NoError(
					t, graph.UpdateEdgePolicy(ctx, edge2),
				)
			}

			// Now we'll run the main query, and verify that we get
			// back the expected number of channels.
			iter := graph.ChanUpdatesInHorizon(
				startTime, startTime.Add(26*time.Hour),
				WithChanUpdateIterBatchSize(batchSize),
			)

			channels, err := fn.CollectErr(iter)
			require.NoError(t, err)
			require.Len(
				t, channels, numChans,
				"expected %d channels, got %d", numChans,
				len(channels),
			)
		})
	}
}

// TestFilterKnownChanIDsZombieRevival tests that if a ChannelUpdateInfo is
// passed to FilterKnownChanIDs that contains a channel that we have marked as
// a zombie, then we will mark it as live again if the new ChannelUpdate has
// timestamps that would make the channel be considered live again.
//
// NOTE: this tests focuses on zombie revival. The main logic of
// FilterKnownChanIDs is tested in TestFilterKnownChanIDs.
func TestFilterKnownChanIDsZombieRevival(t *testing.T) {
	t.Parallel()

	graph := MakeTestGraph(t)

	var (
		scid1 = lnwire.ShortChannelID{BlockHeight: 1}
		scid2 = lnwire.ShortChannelID{BlockHeight: 2}
		scid3 = lnwire.ShortChannelID{BlockHeight: 3}
	)

	isZombie := func(scid lnwire.ShortChannelID) bool {
		zombie, _, _, err := graph.IsZombieEdge(scid.ToUint64())
		require.NoError(t, err)

		return zombie
	}

	// Mark channel 1 and 2 as zombies.
	err := graph.MarkEdgeZombie(scid1.ToUint64(), [33]byte{}, [33]byte{})
	require.NoError(t, err)
	err = graph.MarkEdgeZombie(scid2.ToUint64(), [33]byte{}, [33]byte{})
	require.NoError(t, err)

	require.True(t, isZombie(scid1))
	require.True(t, isZombie(scid2))
	require.False(t, isZombie(scid3))

	// Call FilterKnownChanIDs with an isStillZombie call-back that would
	// result in the current zombies still be considered as zombies.
	_, err = graph.FilterKnownChanIDs([]ChannelUpdateInfo{
		{ShortChannelID: scid1},
		{ShortChannelID: scid2},
		{ShortChannelID: scid3},
	}, func(_ time.Time, _ time.Time) bool {
		return true
	})
	require.NoError(t, err)

	require.True(t, isZombie(scid1))
	require.True(t, isZombie(scid2))
	require.False(t, isZombie(scid3))

	// Now call it again but this time with a isStillZombie call-back that
	// would result in channel with SCID 2 no longer being considered a
	// zombie.
	_, err = graph.FilterKnownChanIDs([]ChannelUpdateInfo{
		{ShortChannelID: scid1},
		{
			ShortChannelID:       scid2,
			Node1UpdateTimestamp: time.Unix(1000, 0),
		},
		{ShortChannelID: scid3},
	}, func(t1 time.Time, _ time.Time) bool {
		return !t1.Equal(time.Unix(1000, 0))
	})
	require.NoError(t, err)

	// Show that SCID 2 has been marked as live.
	require.True(t, isZombie(scid1))
	require.False(t, isZombie(scid2))
	require.False(t, isZombie(scid3))
}

// TestFilterKnownChanIDs tests that we're able to properly perform the set
// differences of an incoming set of channel ID's, and those that we already
// know of on disk.
func TestFilterKnownChanIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	isZombieUpdate := func(updateTime1 time.Time,
		updateTime2 time.Time) bool {

		return true
	}

	var (
		scid1 = lnwire.ShortChannelID{BlockHeight: 1}
		scid2 = lnwire.ShortChannelID{BlockHeight: 2}
		scid3 = lnwire.ShortChannelID{BlockHeight: 3}
	)

	// If we try to filter out a set of channel ID's before we even know of
	// any channels, then we should get the entire set back.
	preChanIDs := []ChannelUpdateInfo{
		{ShortChannelID: scid1},
		{ShortChannelID: scid2},
		{ShortChannelID: scid3},
	}
	filteredIDs, err := graph.FilterKnownChanIDs(preChanIDs, isZombieUpdate)
	require.NoError(t, err, "unable to filter chan IDs")
	require.EqualValues(t, []uint64{
		scid1.ToUint64(),
		scid2.ToUint64(),
		scid3.ToUint64(),
	}, filteredIDs)

	// We'll start by creating two nodes which will seed our test graph.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, we'll add 5 channel ID's to the graph, each of them having a
	// block height 10 blocks after the previous.
	const numChans = 5
	chanIDs := make([]ChannelUpdateInfo, 0, numChans)
	for i := 0; i < numChans; i++ {
		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(ctx, &channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		chanIDs = append(chanIDs, NewChannelUpdateInfo(
			chanID, time.Time{}, time.Time{},
		))
	}

	const numZombies = 5
	zombieIDs := make([]ChannelUpdateInfo, 0, numZombies)
	for i := 0; i < numZombies; i++ {
		channel, chanID := createEdge(
			uint32(i*10+1), 0, 0, 0, node1, node2,
		)
		if err := graph.AddChannelEdge(ctx, &channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}
		err := graph.DeleteChannelEdges(false, true, channel.ChannelID)
		if err != nil {
			t.Fatalf("unable to mark edge zombie: %v", err)
		}

		zombieIDs = append(
			zombieIDs, ChannelUpdateInfo{ShortChannelID: chanID},
		)
	}

	queryCases := []struct {
		queryIDs []ChannelUpdateInfo

		resp []ChannelUpdateInfo
	}{
		// If we attempt to filter out all chanIDs we know of, the
		// response should be the empty set.
		{
			queryIDs: chanIDs,
		},
		// If we attempt to filter out all zombies that we know of, the
		// response should be the empty set.
		{
			queryIDs: zombieIDs,
		},

		// If we query for a set of ID's that we didn't insert, we
		// should get the same set back.
		{
			queryIDs: []ChannelUpdateInfo{
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 99,
					},
				},
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 100,
					},
				},
			},
			resp: []ChannelUpdateInfo{
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 99,
					},
				},
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 100,
					},
				},
			},
		},

		// If we query for a super-set of our the chan ID's inserted,
		// we should only get those new chanIDs back.
		{
			queryIDs: append(chanIDs, []ChannelUpdateInfo{
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 99,
					},
				},
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 101,
					},
				},
			}...),
			resp: []ChannelUpdateInfo{
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 99,
					},
				},
				{
					ShortChannelID: lnwire.ShortChannelID{
						BlockHeight: 101,
					},
				},
			},
		},
	}

	for _, queryCase := range queryCases {
		resp, err := graph.FilterKnownChanIDs(
			queryCase.queryIDs, isZombieUpdate,
		)
		require.NoError(t, err)

		expectedSCIDs := make([]uint64, len(queryCase.resp))
		for i, info := range queryCase.resp {
			expectedSCIDs[i] = info.ShortChannelID.ToUint64()
		}

		if len(expectedSCIDs) == 0 {
			expectedSCIDs = nil
		}

		require.EqualValues(t, expectedSCIDs, resp)
	}
}

// TestStressTestChannelGraphAPI is a stress test that concurrently calls some
// of the ChannelGraph methods in various orders in order to ensure that no
// deadlock can occur. This test currently focuses on stress testing all the
// methods that acquire the cache mutex along with the DB mutex.
func TestStressTestChannelGraphAPI(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skipf("Skipping test in short mode")
	}

	ctx := t.Context()

	graph := MakeTestGraph(t)

	node1 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node1))

	node2 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node2))

	// We need to update the node's timestamp since this call to
	// SetSourceNode will trigger an upsert which will only be allowed if
	// the newest LastUpdate time is greater than the current one.
	node1.LastUpdate = node1.LastUpdate.Add(time.Second)
	require.NoError(t, graph.SetSourceNode(ctx, node1))

	type chanInfo struct {
		info models.ChannelEdgeInfo
		id   lnwire.ShortChannelID
	}

	var (
		chans []*chanInfo
		mu    sync.RWMutex
	)

	// newBlockHeight returns a random block height between 0 and 100.
	newBlockHeight := func() uint32 {
		return uint32(rand.Int31n(100))
	}

	// addNewChan is a will create and return a new random channel and will
	// add it to the set of channels.
	addNewChan := func() *chanInfo {
		mu.Lock()
		defer mu.Unlock()

		channel, chanID := createEdge(
			newBlockHeight(), rand.Uint32(), uint16(rand.Int()),
			rand.Uint32(), node1, node2,
		)

		newChan := &chanInfo{
			info: channel,
			id:   chanID,
		}
		chans = append(chans, newChan)

		return newChan
	}

	// getRandChan picks a random channel from the set and returns it.
	getRandChan := func() *chanInfo {
		mu.RLock()
		defer mu.RUnlock()

		if len(chans) == 0 {
			return nil
		}

		return chans[rand.Intn(len(chans))]
	}

	// getRandChanSet returns a random set of channels.
	getRandChanSet := func() []*chanInfo {
		mu.RLock()
		defer mu.RUnlock()

		if len(chans) == 0 {
			return nil
		}

		start := rand.Intn(len(chans))
		end := rand.Intn(len(chans))

		if end < start {
			start, end = end, start
		}

		var infoCopy []*chanInfo
		for i := start; i < end; i++ {
			infoCopy = append(infoCopy, &chanInfo{
				info: chans[i].info,
				id:   chans[i].id,
			})
		}

		return infoCopy
	}

	// delChan deletes the channel with the given ID from the set if it
	// exists.
	delChan := func(id lnwire.ShortChannelID) {
		mu.Lock()
		defer mu.Unlock()

		index := -1
		for i, c := range chans {
			if c.id == id {
				index = i
				break
			}
		}

		if index == -1 {
			return
		}

		chans = append(chans[:index], chans[index+1:]...)
	}

	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{2}, 32))

	var methodsMu sync.Mutex
	methods := []struct {
		name string
		fn   func() error
	}{
		{
			name: "MarkEdgeZombie",
			fn: func() error {
				channel := getRandChan()
				if channel == nil {
					return nil
				}

				return graph.MarkEdgeZombie(
					channel.id.ToUint64(),
					node1.PubKeyBytes,
					node2.PubKeyBytes,
				)
			},
		},
		{
			name: "FilterKnownChanIDs",
			fn: func() error {
				chanSet := getRandChanSet()
				var chanIDs []ChannelUpdateInfo

				for _, c := range chanSet {
					chanIDs = append(
						chanIDs,
						ChannelUpdateInfo{
							ShortChannelID: c.id,
						},
					)
				}

				_, err := graph.FilterKnownChanIDs(
					chanIDs,
					func(t time.Time, t2 time.Time) bool {
						return rand.Intn(2) == 0
					},
				)

				return err
			},
		},
		{
			name: "HasChannelEdge",
			fn: func() error {
				channel := getRandChan()
				if channel == nil {
					return nil
				}

				_, _, _, _, err := graph.HasChannelEdge(
					channel.id.ToUint64(),
				)

				return err
			},
		},
		{
			name: "PruneGraph",
			fn: func() error {
				chanSet := getRandChanSet()
				var spentOutpoints []*wire.OutPoint

				for _, c := range chanSet {
					spentOutpoints = append(
						spentOutpoints,
						&c.info.ChannelPoint,
					)
				}

				_, err := graph.PruneGraph(
					spentOutpoints, &blockHash, 100,
				)

				return err
			},
		},
		{
			name: "ChanUpdateInHorizon",
			fn: func() error {
				iter := graph.ChanUpdatesInHorizon(
					time.Now().Add(-time.Hour), time.Now(),
				)
				_, err := fn.CollectErr(iter)

				return err
			},
		},
		{
			name: "DeleteChannelEdges",
			fn: func() error {
				var (
					strictPruning = rand.Intn(2) == 0
					markZombie    = rand.Intn(2) == 0
					channels      = getRandChanSet()
					chanIDs       []uint64
				)

				for _, c := range channels {
					chanIDs = append(
						chanIDs, c.id.ToUint64(),
					)
					delChan(c.id)
				}

				err := graph.DeleteChannelEdges(
					strictPruning, markZombie, chanIDs...,
				)
				if err != nil &&
					!errors.Is(err, ErrEdgeNotFound) {

					return err
				}

				return nil
			},
		},
		{
			name: "DisconnectBlockAtHeight",
			fn: func() error {
				_, err := graph.DisconnectBlockAtHeight(
					newBlockHeight(),
				)

				return err
			},
		},
		{
			name: "AddChannelEdge",
			fn: func() error {
				channel := addNewChan()

				return graph.AddChannelEdge(ctx, &channel.info)
			},
		},
	}

	const (
		// concurrencyLevel is the number of concurrent goroutines that
		// will be run simultaneously.
		concurrencyLevel = 10

		// executionCount is the number of methods that will be called
		// per goroutine.
		executionCount = 100
	)

	for i := 0; i < concurrencyLevel; i++ {
		i := i

		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			t.Parallel()

			for j := 0; j < executionCount; j++ {
				// Randomly select a method to execute.
				methodIndex := rand.Intn(len(methods))

				methodsMu.Lock()
				fn := methods[methodIndex].fn
				name := methods[methodIndex].name
				methodsMu.Unlock()

				err := fn()
				require.NoErrorf(t, err, name)
			}
		})
	}
}

// TestFilterChannelRange tests that we're able to properly retrieve the full
// set of short channel ID's for a given block range.
func TestFilterChannelRange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node1))

	node2 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node2))

	// If we try to filter a channel range before we have any channels
	// inserted, we should get an empty slice of results.
	resp, err := graph.FilterChannelRange(10, 100, false)
	require.NoError(t, err)
	require.Empty(t, resp)

	// To start, we'll create a set of channels, two mined in a block 10
	// blocks after the prior one.
	startHeight := uint32(100)
	endHeight := startHeight
	const numChans = 10

	var (
		channelRanges = make(
			[]BlockChannelRange, 0, numChans/2,
		)
		channelRangesWithTimestamps = make(
			[]BlockChannelRange, 0, numChans/2,
		)
	)

	updateTimeSeed := time.Now().Unix()
	maybeAddPolicy := func(chanID uint64, node *models.Node,
		node2 bool) time.Time {

		var chanFlags lnwire.ChanUpdateChanFlags
		if node2 {
			chanFlags = lnwire.ChanUpdateDirection
		}

		var updateTime = time.Unix(0, 0)
		if rand.Int31n(2) == 0 {
			updateTime = time.Unix(updateTimeSeed, 0)
			err = graph.UpdateEdgePolicy(
				ctx, &models.ChannelEdgePolicy{
					ToNode:       node.PubKeyBytes,
					ChannelFlags: chanFlags,
					ChannelID:    chanID,
					LastUpdate:   updateTime,
				},
			)
			require.NoError(t, err)
		}
		updateTimeSeed++

		return updateTime
	}

	for i := 0; i < numChans/2; i++ {
		chanHeight := endHeight
		channel1, chanID1 := createEdge(
			chanHeight, uint32(i+1), 0, 0, node1, node2,
		)
		require.NoError(t, graph.AddChannelEdge(ctx, &channel1))

		channel2, chanID2 := createEdge(
			chanHeight, uint32(i+2), 0, 0, node1, node2,
		)
		require.NoError(t, graph.AddChannelEdge(ctx, &channel2))

		chanInfo1 := NewChannelUpdateInfo(
			chanID1, time.Time{}, time.Time{},
		)
		chanInfo2 := NewChannelUpdateInfo(
			chanID2, time.Time{}, time.Time{},
		)
		channelRanges = append(channelRanges, BlockChannelRange{
			Height: chanHeight,
			Channels: []ChannelUpdateInfo{
				chanInfo1, chanInfo2,
			},
		})

		var (
			time1 = maybeAddPolicy(channel1.ChannelID, node1, false)
			time2 = maybeAddPolicy(channel1.ChannelID, node2, true)
			time3 = maybeAddPolicy(channel2.ChannelID, node1, false)
			time4 = maybeAddPolicy(channel2.ChannelID, node2, true)
		)

		chanInfo1 = NewChannelUpdateInfo(
			chanID1, time1, time2,
		)
		chanInfo2 = NewChannelUpdateInfo(
			chanID2, time3, time4,
		)
		channelRangesWithTimestamps = append(
			channelRangesWithTimestamps, BlockChannelRange{
				Height: chanHeight,
				Channels: []ChannelUpdateInfo{
					chanInfo1, chanInfo2,
				},
			},
		)

		endHeight += 10
	}

	// With our channels inserted, we'll construct a series of queries that
	// we'll execute below in order to exercise the features of the
	// FilterKnownChanIDs method.
	tests := []struct {
		name string

		startHeight uint32
		endHeight   uint32

		resp          []BlockChannelRange
		expStartIndex int
		expEndIndex   int
	}{
		// If we query for the entire range, then we should get the same
		// set of short channel IDs back.
		{
			name:        "entire range",
			startHeight: startHeight,
			endHeight:   endHeight,

			resp:          channelRanges,
			expStartIndex: 0,
			expEndIndex:   len(channelRanges),
		},

		// If we query for a range of channels right before our range,
		// we shouldn't get any results back.
		{
			name:        "range before",
			startHeight: 0,
			endHeight:   10,
		},

		// If we only query for the last height (range wise), we should
		// only get that last channel.
		{
			name:        "last height",
			startHeight: endHeight - 10,
			endHeight:   endHeight - 10,

			resp:          channelRanges[4:],
			expStartIndex: 4,
			expEndIndex:   len(channelRanges),
		},

		// If we query for just the first height, we should only get a
		// single channel back (the first one).
		{
			name:        "first height",
			startHeight: startHeight,
			endHeight:   startHeight,

			resp:          channelRanges[:1],
			expStartIndex: 0,
			expEndIndex:   1,
		},

		{
			name:        "subset",
			startHeight: startHeight + 10,
			endHeight:   endHeight - 10,

			resp:          channelRanges[1:5],
			expStartIndex: 1,
			expEndIndex:   5,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// First, do the query without requesting timestamps.
			resp, err := graph.FilterChannelRange(
				test.startHeight, test.endHeight, false,
			)
			require.NoError(t, err)

			expRes := channelRanges[test.expStartIndex:test.expEndIndex] //nolint:ll

			if len(expRes) == 0 {
				require.Nil(t, resp)
			} else {
				require.Equal(t, expRes, resp)
			}

			// Now, query the timestamps as well.
			resp, err = graph.FilterChannelRange(
				test.startHeight, test.endHeight, true,
			)
			require.NoError(t, err)

			expRes = channelRangesWithTimestamps[test.expStartIndex:test.expEndIndex] //nolint:ll

			if len(expRes) == 0 {
				require.Nil(t, resp)
			} else {
				require.Equal(t, expRes, resp)
			}
		})
	}
}

// TestFetchChanInfos tests that we're able to properly retrieve the full set
// of ChannelEdge structs for a given set of short channel ID's.
func TestFetchChanInfos(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll make 5 test channels, ensuring we keep track of which channel
	// ID corresponds to a particular ChannelEdge.
	const numChans = 5
	startTime := time.Unix(1234, 0)
	endTime := startTime
	edges := make([]ChannelEdge, 0, numChans)
	edgeQuery := make([]uint64, 0, numChans)
	for i := 0; i < numChans; i++ {
		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(ctx, &channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		updateTime := endTime
		endTime = updateTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(chanID.ToUint64(), updateTime.Unix())
		edge1.ChannelFlags = 0
		edge1.ToNode = node2.PubKeyBytes
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(chanID.ToUint64(), updateTime.Unix())
		edge2.ChannelFlags = 1
		edge2.ToNode = node1.PubKeyBytes
		edge2.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edges = append(edges, ChannelEdge{
			Info:    &channel,
			Policy1: edge1,
			Policy2: edge2,
		})

		edgeQuery = append(edgeQuery, chanID.ToUint64())
	}

	// Add an additional edge that does not exist. The query should skip
	// this channel and return only infos for the edges that exist.
	edgeQuery = append(edgeQuery, 500)

	// Add an another edge to the query that has been marked as a zombie
	// edge. The query should also skip this channel.
	zombieChan, zombieChanID := createEdge(
		666, 0, 0, 0, node1, node2,
	)
	if err := graph.AddChannelEdge(ctx, &zombieChan); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	err := graph.DeleteChannelEdges(false, true, zombieChan.ChannelID)
	require.NoError(t, err, "unable to delete and mark edge zombie")
	edgeQuery = append(edgeQuery, zombieChanID.ToUint64())

	// We'll now attempt to query for the range of channel ID's we just
	// inserted into the database. We should get the exact same set of
	// edges back.
	resp, err := graph.FetchChanInfos(edgeQuery)
	require.NoError(t, err, "unable to fetch chan edges")
	if len(resp) != len(edges) {
		t.Fatalf("expected %v edges, instead got %v", len(edges),
			len(resp))
	}

	for i := 0; i < len(resp); i++ {
		err := compareEdgePolicies(resp[i].Policy1, edges[i].Policy1)
		if err != nil {
			t.Fatalf("edge doesn't match: %v", err)
		}
		err = compareEdgePolicies(resp[i].Policy2, edges[i].Policy2)
		if err != nil {
			t.Fatalf("edge doesn't match: %v", err)
		}
		assertEdgeInfoEqual(t, resp[i].Info, edges[i].Info)
	}
}

// TestIncompleteChannelPolicies tests that a channel that only has a policy
// specified on one end is properly returned in ForEachChannel calls from
// both sides.
func TestIncompleteChannelPolicies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Create two nodes.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	channel, chanID := createEdge(
		uint32(0), 0, 0, 0, node1, node2,
	)

	if err := graph.AddChannelEdge(ctx, &channel); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Ensure that channel is reported with unknown policies.
	checkPolicies := func(node *models.Node, expectedIn,
		expectedOut bool) {

		calls := 0
		err := graph.ForEachNodeChannel(
			ctx, node.PubKeyBytes,
			func(_ *models.ChannelEdgeInfo, outEdge,
				inEdge *models.ChannelEdgePolicy) error {

				if !expectedOut && outEdge != nil {
					t.Fatalf("Expected no outgoing policy")
				}

				if expectedOut && outEdge == nil {
					t.Fatalf("Expected an outgoing policy")
				}

				if !expectedIn && inEdge != nil {
					t.Fatalf("Expected no incoming policy")
				}

				if expectedIn && inEdge == nil {
					t.Fatalf("Expected an incoming policy")
				}

				calls++

				return nil
			}, func() {},
		)
		require.NoError(t, err)
		require.Equal(t, 1, calls)
	}

	checkPolicies(node2, false, false)

	// Only create an edge policy for node1 and leave the policy for node2
	// unknown.
	updateTime := time.Unix(1234, 0)

	edgePolicy := newEdgePolicy(chanID.ToUint64(), updateTime.Unix())
	edgePolicy.ChannelFlags = 0
	edgePolicy.ToNode = node2.PubKeyBytes
	edgePolicy.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(ctx, edgePolicy); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	checkPolicies(node1, false, true)
	checkPolicies(node2, true, false)

	// Create second policy and assert that both policies are reported
	// as present.
	edgePolicy = newEdgePolicy(chanID.ToUint64(), updateTime.Unix())
	edgePolicy.ChannelFlags = 1
	edgePolicy.ToNode = node1.PubKeyBytes
	edgePolicy.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(ctx, edgePolicy); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	checkPolicies(node1, true, true)
	checkPolicies(node2, true, true)
}

// TestChannelEdgePruningUpdateIndexDeletion tests that once edges are deleted
// from the graph, then their entries within the update index are also cleaned
// up.
func TestChannelEdgePruningUpdateIndexDeletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// The update index only applies to the bbolt graph.
	boltStore, ok := graph.V1Store.(*KVStore)
	if !ok {
		t.Skipf("skipping test that is aimed at a bbolt graph DB")
	}

	sourceNode := createTestVertex(t)
	if err := graph.SetSourceNode(ctx, sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// With the two nodes created, we'll now create a random channel, as
	// well as two edges in the database with distinct update times.
	edgeInfo, chanID := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(ctx, &edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge1 := randEdgePolicy(chanID.ToUint64())
	edge1.ChannelFlags = 0
	edge1.ToNode = node1.PubKeyBytes
	edge1.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	edge1 = copyEdgePolicy(edge1) // Avoid read/write race conditions.

	edge2 := randEdgePolicy(chanID.ToUint64())
	edge2.ChannelFlags = 1
	edge2.ToNode = node2.PubKeyBytes
	edge2.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	edge2 = copyEdgePolicy(edge2) // Avoid read/write race conditions.

	// checkIndexTimestamps is a helper function that checks the edge update
	// index only includes the given timestamps.
	checkIndexTimestamps := func(timestamps ...uint64) {
		timestampSet := make(map[uint64]struct{})
		for _, t := range timestamps {
			timestampSet[t] = struct{}{}
		}

		err := kvdb.View(boltStore.db, func(tx kvdb.RTx) error {
			edges := tx.ReadBucket(edgeBucket)
			if edges == nil {
				return ErrGraphNoEdgesFound
			}
			edgeUpdateIndex := edges.NestedReadBucket(
				edgeUpdateIndexBucket,
			)
			if edgeUpdateIndex == nil {
				return ErrGraphNoEdgesFound
			}

			var numEntries int
			err := edgeUpdateIndex.ForEach(func(k, v []byte) error {
				numEntries++
				return nil
			})
			if err != nil {
				return err
			}

			expectedEntries := len(timestampSet)
			if numEntries != expectedEntries {
				return fmt.Errorf("expected %v entries in the "+
					"update index, got %v", expectedEntries,
					numEntries)
			}

			return edgeUpdateIndex.ForEach(func(k, _ []byte) error {
				t := byteOrder.Uint64(k[:8])
				if _, ok := timestampSet[t]; !ok {
					return fmt.Errorf("found unexpected "+
						"timestamp "+"%d", t)
				}

				return nil
			})
		}, func() {})
		if err != nil {
			t.Fatal(err)
		}
	}

	// With both edges policies added, we'll make sure to check they exist
	// within the edge update index.
	checkIndexTimestamps(
		uint64(edge1.LastUpdate.Unix()),
		uint64(edge2.LastUpdate.Unix()),
	)

	// Now, we'll update the edge policies to ensure the old timestamps are
	// removed from the update index.
	edge1.ChannelFlags = 2
	edge1.LastUpdate = time.Now()
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	edge2.ChannelFlags = 3
	edge2.LastUpdate = edge1.LastUpdate.Add(time.Hour)
	if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// With the policies updated, we should now be able to find their
	// updated entries within the update index.
	checkIndexTimestamps(
		uint64(edge1.LastUpdate.Unix()),
		uint64(edge2.LastUpdate.Unix()),
	)

	// Now we'll prune the graph, removing the edges, and also the update
	// index entries from the database all together.
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{2}, 32))
	_, err := graph.PruneGraph(
		[]*wire.OutPoint{&edgeInfo.ChannelPoint}, &blockHash, 101,
	)
	require.NoError(t, err, "unable to prune graph")

	// Finally, we'll check the database state one last time to conclude
	// that we should no longer be able to locate _any_ entries within the
	// edge update index.
	checkIndexTimestamps()
}

// TestPruneGraphNodes tests that unconnected vertexes are pruned via the
// PruneSyncState method.
func TestPruneGraphNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll start off by inserting our source node, to ensure that it's
	// the only node left after we prune the graph.
	sourceNode := createTestVertex(t)
	if err := graph.SetSourceNode(ctx, sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// With the source node inserted, we'll now add three nodes to the
	// channel graph, at the end of the scenario, only two of these nodes
	// should still be in the graph.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node3 := createTestVertex(t)
	if err := graph.AddNode(ctx, node3); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll now add a new edge to the graph, but only actually advertise
	// the edge of *one* of the nodes.
	edgeInfo, chanID := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(ctx, &edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// We'll now insert an advertised edge, but it'll only be the edge that
	// points from the first to the second node.
	edge1 := randEdgePolicy(chanID.ToUint64())
	edge1.ChannelFlags = 0
	edge1.ToNode = node1.PubKeyBytes
	edge1.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// We'll now initiate a around of graph pruning.
	if err := graph.PruneGraphNodes(); err != nil {
		t.Fatalf("unable to prune graph nodes: %v", err)
	}

	// At this point, there should be 3 nodes left in the graph still: the
	// source node (which can't be pruned), and node 1+2. Nodes 1 and two
	// should still be left in the graph as there's half of an advertised
	// edge between them.
	assertNumNodes(t, graph, 3)

	// Finally, we'll ensure that node3, the only fully unconnected node as
	// properly deleted from the graph and not another node in its place.
	_, err := graph.FetchNode(ctx, node3.PubKeyBytes)
	require.NotNil(t, err)
}

// TestAddChannelEdgeShellNodes tests that when we attempt to add a ChannelEdge
// to the graph, one or both of the nodes the edge involves aren't found in the
// database, then shell edges are created for each node if needed.
func TestAddChannelEdgeShellNodes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// To start, we'll create two nodes, and only add one of them to the
	// channel graph.
	node1 := createTestVertex(t)
	require.NoError(t, graph.SetSourceNode(ctx, node1))
	node2 := createTestVertex(t)

	// We'll now create an edge between the two nodes, as a result, node2
	// should be inserted into the database as a shell node.
	edgeInfo, _ := createEdge(100, 0, 0, 0, node1, node2)
	require.NoError(t, graph.AddChannelEdge(ctx, &edgeInfo))

	// Ensure that node1 was inserted as a full node, while node2 only has
	// a shell node present.
	node1, err := graph.FetchNode(ctx, node1.PubKeyBytes)
	require.NoError(t, err, "unable to fetch node1")
	require.True(t, node1.HaveAnnouncement())

	node2, err = graph.FetchNode(ctx, node2.PubKeyBytes)
	require.NoError(t, err, "unable to fetch node2")
	require.False(t, node2.HaveAnnouncement())

	// Show that attempting to add the channel again will result in an
	// error.
	err = graph.AddChannelEdge(ctx, &edgeInfo)
	require.ErrorIs(t, err, ErrEdgeAlreadyExist)

	// Show that updating the shell node to a full node record works.
	require.NoError(t, graph.AddNode(ctx, node2))
}

// TestNodePruningUpdateIndexDeletion tests that once a node has been removed
// from the channel graph, we also remove the entry from the update index as
// well.
func TestNodePruningUpdateIndexDeletion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'll first populate our graph with a single node that will be
	// removed shortly.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll confirm that we can retrieve the node using
	// NodeUpdatesInHorizon, using a time that's slightly beyond the last
	// update time of our test node.
	startTime := time.Unix(9, 0)
	endTime := node1.LastUpdate.Add(time.Minute)
	nodesInHorizonIter := graph.NodeUpdatesInHorizon(startTime, endTime)

	// We should only have a single node, and that node should exactly
	// match the node we just inserted.
	nodesInHorizon, err := fn.CollectErr(nodesInHorizonIter)
	require.NoError(t, err, "unable to fetch nodes in horizon")
	if len(nodesInHorizon) != 1 {
		t.Fatalf("should have 1 nodes instead have: %v",
			len(nodesInHorizon))
	}
	compareNodes(t, node1, nodesInHorizon[0])

	// We'll now delete the node from the graph, this should result in it
	// being removed from the update index as well.
	err = graph.DeleteNode(ctx, node1.PubKeyBytes)
	require.NoError(t, err)

	// Now that the node has been deleted, we'll again query the nodes in
	// the horizon. This time we should have no nodes at all.
	nodesInHorizonIter = graph.NodeUpdatesInHorizon(startTime, endTime)
	nodesInHorizon, err = fn.CollectErr(nodesInHorizonIter)
	require.NoError(t, err, "unable to fetch nodes in horizon")

	if len(nodesInHorizon) != 0 {
		t.Fatalf("should have zero nodes instead have: %v",
			len(nodesInHorizon))
	}
}

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

// TestNodeIsPublic ensures that we properly detect nodes that are seen as
// public within the network graph.
func TestNodeIsPublic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// We'll start off the test by creating a small network of 3
	// participants with the following graph:
	//
	//	Alice <-> Bob <-> Carol
	//
	// We'll need to create a separate database and channel graph for each
	// participant to replicate real-world scenarios (private edges being in
	// some graphs but not others, etc.).
	aliceGraph := MakeTestGraph(t)
	aliceNode := createTestVertex(t)
	if err := aliceGraph.SetSourceNode(ctx, aliceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	bobGraph := MakeTestGraph(t)
	bobNode := createTestVertex(t)
	if err := bobGraph.SetSourceNode(ctx, bobNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	carolGraph := MakeTestGraph(t)
	carolNode := createTestVertex(t)
	if err := carolGraph.SetSourceNode(ctx, carolNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	aliceBobEdge, _ := createEdge(10, 0, 0, 0, aliceNode, bobNode)
	bobCarolEdge, _ := createEdge(10, 1, 0, 1, bobNode, carolNode)

	// After creating all of our nodes and edges, we'll add them to each
	// participant's graph.
	nodes := []*models.Node{aliceNode, bobNode, carolNode}
	edges := []*models.ChannelEdgeInfo{&aliceBobEdge, &bobCarolEdge}
	graphs := []*ChannelGraph{aliceGraph, bobGraph, carolGraph}
	for _, graph := range graphs {
		for _, node := range nodes {
			node.LastUpdate = nextUpdateTime()
			err := graph.AddNode(ctx, node)
			require.NoError(t, err)
		}
		for _, edge := range edges {
			err := graph.AddChannelEdge(ctx, edge)
			require.NoError(t, err)
		}
	}

	// checkNodes is a helper closure that will be used to assert that the
	// given nodes are seen as public/private within the given graphs.
	checkNodes := func(nodes []*models.Node,
		graphs []*ChannelGraph, public bool) {

		t.Helper()

		for _, node := range nodes {
			for _, graph := range graphs {
				isPublic, err := graph.IsPublicNode(
					node.PubKeyBytes,
				)
				if err != nil {
					t.Fatalf("unable to determine if "+
						"pivot is public: %v", err)
				}

				switch {
				case isPublic && !public:
					t.Fatalf("expected %x to be private",
						node.PubKeyBytes)
				case !isPublic && public:
					t.Fatalf("expected %x to be public",
						node.PubKeyBytes)
				}
			}
		}
	}

	// Due to the way the edges were set up above, we'll make sure each node
	// can correctly determine that every other node is public.
	checkNodes(nodes, graphs, true)

	// Now, we'll remove the edge between Alice and Bob from everyone's
	// graph. This will make Alice be seen as a private node as it no longer
	// has any advertised edges.
	for _, graph := range graphs {
		err := graph.DeleteChannelEdges(
			false, true, aliceBobEdge.ChannelID,
		)
		if err != nil {
			t.Fatalf("unable to remove edge: %v", err)
		}
	}
	checkNodes(
		[]*models.Node{aliceNode},
		[]*ChannelGraph{bobGraph, carolGraph},
		false,
	)

	// We'll also make the edge between Bob and Carol private. Within Bob's
	// and Carol's graph, the edge will exist, but it will not have a proof
	// that allows it to be advertised. Within Alice's graph, we'll
	// completely remove the edge as it is not possible for her to know of
	// it without it being advertised.
	for _, graph := range graphs {
		err := graph.DeleteChannelEdges(
			false, true, bobCarolEdge.ChannelID,
		)
		if err != nil {
			t.Fatalf("unable to remove edge: %v", err)
		}

		if graph == aliceGraph {
			continue
		}

		bobCarolEdge.AuthProof = nil
		if err := graph.AddChannelEdge(ctx, &bobCarolEdge); err != nil {
			t.Fatalf("unable to add edge: %v", err)
		}
	}

	// With the modifications above, Bob should now be seen as a private
	// node from both Alice's and Carol's perspective.
	checkNodes(
		[]*models.Node{bobNode},
		[]*ChannelGraph{aliceGraph, carolGraph},
		false,
	)
}

// TestDisabledChannelIDs ensures that the disabled channels within the
// disabledEdgePolicyBucket are managed properly and the list returned from
// DisabledChannelIDs is correct.
func TestDisabledChannelIDs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Create first node and add it to the graph.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Create second node and add it to the graph.
	node2 := createTestVertex(t)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Adding a new channel edge to the graph.
	edgeInfo, edge1, edge2 := createChannelEdge(node1, node2)
	node2.LastUpdate = nextUpdateTime()
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	if err := graph.AddChannelEdge(ctx, edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Ensure no disabled channels exist in the bucket on start.
	disabledChanIds, err := graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled "+
			"channels", len(disabledChanIds))
	}

	// Add one disabled policy and ensure the channel is still not in the
	// disabled list.
	edge1.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled "+
			"channels", len(disabledChanIds))
	}

	// Add second disabled policy and ensure the channel is now in the
	// disabled list.
	edge2.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.UpdateEdgePolicy(ctx, edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) != 1 ||
		disabledChanIds[0] != edgeInfo.ChannelID {

		t.Fatalf("expected disabled channel with id %v, "+
			"got %v", edgeInfo.ChannelID, disabledChanIds)
	}

	// Delete the channel edge and ensure it is removed from the disabled
	// list.
	if err = graph.DeleteChannelEdges(
		false, true, edgeInfo.ChannelID,
	); err != nil {
		t.Fatalf("unable to delete channel edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled "+
			"channels", len(disabledChanIds))
	}
}

// TestEdgePolicyMissingMaxHTLC tests that if we find a ChannelEdgePolicy in
// the DB that indicates that it should support the htlc_maximum_value_msat
// field, but it is not part of the opaque data, then we'll handle it as it is
// unknown. It also checks that we are correctly able to overwrite it when we
// receive the proper update.
func TestEdgePolicyMissingMaxHTLC(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// This test currently directly edits the bytes stored in the bbolt DB.
	boltStore, ok := graph.V1Store.(*KVStore)
	if !ok {
		t.Skipf("skipping test that is aimed at a bbolt graph DB")
	}

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1 := createTestVertex(t)
	if err := graph.AddNode(ctx, node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2 := createTestVertex(t)

	edgeInfo, edge1, edge2 := createChannelEdge(node1, node2)
	if err := graph.AddNode(ctx, node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	if err := graph.AddChannelEdge(ctx, edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	chanID := edgeInfo.ChannelID
	from := edge2.ToNode[:]
	to := edge1.ToNode[:]

	// We'll remove the no max_htlc field from the first edge policy, and
	// all other opaque data, and serialize it.
	edge1.MessageFlags = 0
	edge1.ExtraOpaqueData = nil

	var b bytes.Buffer
	require.NoError(t, serializeChanEdgePolicy(&b, edge1, to))

	// Set the max_htlc field. The extra bytes added to the serialization
	// will be the opaque data containing the serialized field.
	edge1.MessageFlags = lnwire.ChanUpdateRequiredMaxHtlc
	edge1.MaxHTLC = 13928598
	var b2 bytes.Buffer
	require.NoError(t, serializeChanEdgePolicy(&b2, edge1, to))

	withMaxHtlc := b2.Bytes()

	// Remove the opaque data from the serialization.
	stripped := withMaxHtlc[:len(b.Bytes())]

	// Attempting to deserialize these bytes should return an error.
	r := bytes.NewReader(stripped)
	_, err := deserializeChanEdgePolicy(r)
	require.ErrorIs(t, err, ErrEdgePolicyOptionalFieldNotFound)

	// Put the stripped bytes in the DB.
	putSerializedPolicy(t, boltStore.db, from, chanID, stripped)

	// And add the second, unmodified edge.
	require.NoError(t, graph.UpdateEdgePolicy(ctx, edge2))

	// Attempt to fetch the edge and policies from the DB. Since the policy
	// we added is invalid according to the new format, it should be as we
	// are not aware of the policy (indicated by the policy returned being
	// nil)
	dbEdgeInfo, dbEdge1, dbEdge2, err := graph.FetchChannelEdgesByID(chanID)
	require.NoError(t, err, "unable to fetch channel by ID")

	// The first edge should have a nil-policy returned
	if dbEdge1 != nil {
		t.Fatalf("expected db edge to be nil")
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)

	// Now add the original, unmodified edge policy, and make sure the edge
	// policies then become fully populated.
	if err := graph.UpdateEdgePolicy(ctx, edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	dbEdgeInfo, dbEdge1, dbEdge2, err = graph.FetchChannelEdgesByID(chanID)
	require.NoError(t, err, "unable to fetch channel by ID")
	if err := compareEdgePolicies(dbEdge1, edge1); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)
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

// assertNumZombies queries the provided ChannelGraph for NumZombies, and
// asserts that the returned number is equal to expZombies.
func assertNumZombies(t *testing.T, graph *ChannelGraph, expZombies uint64) {
	t.Helper()

	numZombies, err := graph.NumZombies()
	require.NoError(t, err, "unable to query number of zombies")

	if numZombies != expZombies {
		t.Fatalf("expected %d zombies, found %d",
			expZombies, numZombies)
	}
}

// TestGraphZombieIndex ensures that we can mark edges correctly as zombie/live.
func TestGraphZombieIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// We'll start by creating our test graph along with a test edge.
	graph := MakeTestGraph(t)

	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// Swap the nodes if the second's pubkey is smaller than the first.
	// Without this, the comparisons at the end will fail probabilistically.
	if bytes.Compare(node2.PubKeyBytes[:], node1.PubKeyBytes[:]) < 0 {
		node1, node2 = node2, node1
	}

	edge, _, _ := createChannelEdge(node1, node2)
	require.NoError(t, graph.AddChannelEdge(ctx, edge))

	// Since the edge is known the graph and it isn't a zombie, IsZombieEdge
	// should not report the channel as a zombie.
	isZombie, _, _, err := graph.IsZombieEdge(edge.ChannelID)
	require.NoError(t, err)
	require.False(t, isZombie)
	assertNumZombies(t, graph, 0)

	// If we delete the edge and mark it as a zombie, then we should expect
	// to see it within the index.
	err = graph.DeleteChannelEdges(false, true, edge.ChannelID)
	require.NoError(t, err, "unable to mark edge as zombie")
	isZombie, pubKey1, pubKey2, err := graph.IsZombieEdge(edge.ChannelID)
	require.NoError(t, err)
	require.True(t, isZombie)
	require.Equal(t, node1.PubKeyBytes, pubKey1)
	require.Equal(t, node2.PubKeyBytes, pubKey2)
	assertNumZombies(t, graph, 1)

	// Similarly, if we mark the same edge as live, we should no longer see
	// it within the index.
	require.NoError(t, graph.MarkEdgeLive(edge.ChannelID))

	// Attempting to mark the edge as live again now that it is no longer
	// in the zombie index should fail.
	require.ErrorIs(
		t, graph.MarkEdgeLive(edge.ChannelID), ErrZombieEdgeNotFound,
	)

	isZombie, _, _, err = graph.IsZombieEdge(edge.ChannelID)
	require.NoError(t, err)
	require.False(t, isZombie)

	assertNumZombies(t, graph, 0)

	// If we mark the edge as a zombie manually, then it should show up as
	// being a zombie once again.
	err = graph.MarkEdgeZombie(
		edge.ChannelID, node1.PubKeyBytes, node2.PubKeyBytes,
	)
	require.NoError(t, err, "unable to mark edge as zombie")

	isZombie, _, _, err = graph.IsZombieEdge(edge.ChannelID)
	require.NoError(t, err)
	require.True(t, isZombie)
	assertNumZombies(t, graph, 1)
}

// compareNodes is used to compare two Nodes.
func compareNodes(t *testing.T, a, b *models.Node) {
	t.Helper()

	// Call the PubKey method for each node to ensure that the internal
	// `pubKey` field is set for both objects and so require.Equals can
	// then be used to compare the structs.
	_, err := a.PubKey()
	require.NoError(t, err)
	_, err = b.PubKey()
	require.NoError(t, err)

	require.Equal(t, a, b)
}

// compareEdgePolicies is used to compare two ChannelEdgePolices using
// compareNodes, so as to exclude comparisons of the Nodes' Features struct.
func compareEdgePolicies(a, b *models.ChannelEdgePolicy) error {
	if a.ChannelID != b.ChannelID {
		return fmt.Errorf("ChannelID doesn't match: expected %v, "+
			"got %v", a.ChannelID, b.ChannelID)
	}
	if !reflect.DeepEqual(a.LastUpdate, b.LastUpdate) {
		return fmt.Errorf("edge LastUpdate doesn't match: "+
			"expected %#v, got %#v", a.LastUpdate, b.LastUpdate)
	}
	if a.MessageFlags != b.MessageFlags {
		return fmt.Errorf("MessageFlags doesn't match: expected %v, "+
			"got %v", a.MessageFlags, b.MessageFlags)
	}
	if a.ChannelFlags != b.ChannelFlags {
		return fmt.Errorf("ChannelFlags doesn't match: expected %v, "+
			"got %v", a.ChannelFlags, b.ChannelFlags)
	}
	if a.TimeLockDelta != b.TimeLockDelta {
		return fmt.Errorf("TimeLockDelta doesn't match: expected %v, "+
			"got %v", a.TimeLockDelta, b.TimeLockDelta)
	}
	if a.MinHTLC != b.MinHTLC {
		return fmt.Errorf("MinHTLC doesn't match: expected %v, "+
			"got %v", a.MinHTLC, b.MinHTLC)
	}
	if a.MaxHTLC != b.MaxHTLC {
		return fmt.Errorf("MaxHTLC doesn't match: expected %v, "+
			"got %v", a.MaxHTLC, b.MaxHTLC)
	}
	if a.FeeBaseMSat != b.FeeBaseMSat {
		return fmt.Errorf("FeeBaseMSat doesn't match: expected %v, "+
			"got %v", a.FeeBaseMSat, b.FeeBaseMSat)
	}
	if a.FeeProportionalMillionths != b.FeeProportionalMillionths {
		return fmt.Errorf("FeeProportionalMillionths doesn't match: "+
			"expected %v, got %v", a.FeeProportionalMillionths,
			b.FeeProportionalMillionths)
	}
	if !bytes.Equal(a.ExtraOpaqueData, b.ExtraOpaqueData) {
		return fmt.Errorf("extra data doesn't match: %v vs %v",
			a.ExtraOpaqueData, b.ExtraOpaqueData)
	}
	if !bytes.Equal(a.ToNode[:], b.ToNode[:]) {
		return fmt.Errorf("ToNode doesn't match: expected %x, got %x",
			a.ToNode, b.ToNode)
	}

	return nil
}

// TestLightningNodeSigVerification checks that we can use the Node's
// pubkey to verify signatures.
func TestLightningNodeSigVerification(t *testing.T) {
	t.Parallel()

	// Create some dummy data to sign.
	var data [32]byte
	if _, err := prand.Read(data[:]); err != nil {
		t.Fatalf("unable to read prand: %v", err)
	}

	// Create private key and sign the data with it.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to crete priv key")

	sign := ecdsa.Sign(priv, data[:])

	// Sanity check that the signature checks out.
	if !sign.Verify(data[:], priv.PubKey()) {
		t.Fatalf("signature doesn't check out")
	}

	// Create a Node from the same private key.
	node := createNode(priv)

	// And finally check that we can verify the same signature from the
	// pubkey returned from the lightning node.
	nodePub, err := node.PubKey()
	require.NoError(t, err, "unable to get pubkey")

	if !sign.Verify(data[:], nodePub) {
		t.Fatalf("unable to verify sig")
	}
}

// TestComputeFee tests fee calculation based on the outgoing amt.
func TestComputeFee(t *testing.T) {
	var (
		policy = models.ChannelEdgePolicy{
			FeeBaseMSat:               10000,
			FeeProportionalMillionths: 30000,
		}
		outgoingAmt = lnwire.MilliSatoshi(1000000)
		expectedFee = lnwire.MilliSatoshi(40000)
	)

	fee := policy.ComputeFee(outgoingAmt)
	if fee != expectedFee {
		t.Fatalf("expected fee %v, got %v", expectedFee, fee)
	}
}

// TestBatchedAddChannelEdge asserts that BatchedAddChannelEdge properly
// executes multiple AddChannelEdge requests in a single txn.
func TestBatchedAddChannelEdge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	sourceNode := createTestVertex(t)
	require.Nil(t, graph.SetSourceNode(ctx, sourceNode))

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1 := createTestVertex(t)
	node2 := createTestVertex(t)

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	var spendOutputs []*wire.OutPoint
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))

	// Prune the graph a few times to make sure we have entries in the
	// prune log.
	_, err := graph.PruneGraph(spendOutputs, &blockHash, 155)
	require.Nil(t, err)
	var blockHash2 chainhash.Hash
	copy(blockHash2[:], bytes.Repeat([]byte{2}, 32))

	_, err = graph.PruneGraph(spendOutputs, &blockHash2, 156)
	require.Nil(t, err)

	// We'll create 3 almost identical edges, so first create a helper
	// method containing all logic for doing so.

	// Create an edge which has its block height at 156.
	height := uint32(156)
	edgeInfo, _ := createEdge(height, 0, 0, 0, node1, node2)

	// Create an edge with block height 157. We give it
	// maximum values for tx index and position, to make
	// sure our database range scan get edges from the
	// entire range.
	edgeInfo2, _ := createEdge(
		height+1, math.MaxUint32&0x00ffffff, math.MaxUint16, 1,
		node1, node2,
	)

	// Create a third edge, this with a block height of 155.
	edgeInfo3, _ := createEdge(height-1, 0, 0, 2, node1, node2)

	edges := []models.ChannelEdgeInfo{edgeInfo, edgeInfo2, edgeInfo3}
	errChan := make(chan error, len(edges))
	errTimeout := errors.New("timeout adding batched channel")

	// Now add all these new edges to the database.
	var wg sync.WaitGroup
	for _, edge := range edges {
		wg.Add(1)
		go func(edge models.ChannelEdgeInfo) {
			defer wg.Done()

			select {
			case errChan <- graph.AddChannelEdge(ctx, &edge):
			case <-time.After(2 * time.Second):
				errChan <- errTimeout
			}
		}(edge)
	}
	wg.Wait()

	for i := 0; i < len(edges); i++ {
		err := <-errChan
		require.Nil(t, err)
	}
}

// TestBatchedUpdateEdgePolicy asserts that BatchedUpdateEdgePolicy properly
// executes multiple UpdateEdgePolicy requests in a single txn.
func TestBatchedUpdateEdgePolicy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node1))
	node2 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node2))

	// Create an edge and add it to the db.
	edgeInfo, edge1, edge2 := createChannelEdge(node1, node2)

	// Make sure inserting the policy at this point, before the edge info
	// is added, will fail.
	require.ErrorIs(t, graph.UpdateEdgePolicy(ctx, edge1), ErrEdgeNotFound)

	// Add the edge info.
	require.NoError(t, graph.AddChannelEdge(ctx, edgeInfo))

	errTimeout := errors.New("timeout adding batched channel")

	updates := []*models.ChannelEdgePolicy{edge1, edge2}

	errChan := make(chan error, len(updates))

	// Now add all these new edges to the database.
	var wg sync.WaitGroup
	for _, update := range updates {
		wg.Add(1)
		go func(update *models.ChannelEdgePolicy) {
			defer wg.Done()

			select {
			case errChan <- graph.UpdateEdgePolicy(ctx, update):
			case <-time.After(2 * time.Second):
				errChan <- errTimeout
			}
		}(update)
	}
	wg.Wait()

	for i := 0; i < len(updates); i++ {
		err := <-errChan
		require.Nil(t, err)
	}
}

// BenchmarkForEachChannel is a benchmark test that measures the number of
// allocations and the total memory consumed by the full graph traversal.
func BenchmarkForEachChannel(b *testing.B) {
	graph := MakeTestGraph(b)
	ctx := b.Context()

	const numNodes = 100
	const numChannels = 4
	_, _ = fillTestGraph(b, graph, numNodes, numChannels)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var (
			totalCapacity btcutil.Amount
			maxHTLCs      lnwire.MilliSatoshi
		)

		var nodes []route.Vertex
		err := graph.ForEachNodeCacheable(ctx, func(node route.Vertex,
			vector *lnwire.FeatureVector) error {

			nodes = append(nodes, node)

			return nil
		}, func() {
			nodes = nil
		})
		require.NoError(b, err)

		for _, n := range nodes {
			cb := func(info *models.ChannelEdgeInfo,
				policy *models.ChannelEdgePolicy,
				policy2 *models.ChannelEdgePolicy) error { //nolint:ll

				// We need to do something with
				// the data here, otherwise the
				// compiler is going to optimize
				// this away, and we get bogus
				// results.
				totalCapacity += info.Capacity
				maxHTLCs += policy.MaxHTLC
				maxHTLCs += policy2.MaxHTLC

				return nil
			}

			err := graph.ForEachNodeChannel(ctx, n, cb, func() {})
			require.NoError(b, err)
		}
	}
}

// TestGraphCacheForEachNodeChannel tests that the forEachNodeDirectedChannel
// method works as expected, and is able to handle nil self edges.
func TestGraphCacheForEachNodeChannel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := MakeTestGraph(t)

	// Unset the channel graph cache to simulate the user running with the
	// option turned off.
	graph.graphCache = nil

	node1 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node1))
	node2 := createTestVertex(t)
	require.NoError(t, graph.AddNode(ctx, node2))

	// Create an edge and add it to the db.
	edgeInfo, e1, e2 := createChannelEdge(node1, node2)

	// Because of lexigraphical sorting and the usage of random node keys in
	// this test, we need to determine which edge belongs to node 1 at
	// runtime.
	var edge1 *models.ChannelEdgePolicy
	if e1.ToNode == node2.PubKeyBytes {
		edge1 = e1
	} else {
		edge1 = e2
	}

	// Add the channel, but only insert a single edge into the graph.
	require.NoError(t, graph.AddChannelEdge(ctx, edgeInfo))

	getSingleChannel := func() *DirectedChannel {
		var ch *DirectedChannel
		err := graph.ForEachNodeDirectedChannel(node1.PubKeyBytes,
			func(c *DirectedChannel) error {
				require.Nil(t, ch)
				ch = c

				return nil
			}, func() {},
		)
		require.NoError(t, err)

		return ch
	}

	// We should be able to accumulate the single channel added, even
	// though we have a nil edge policy here.
	require.NotNil(t, getSingleChannel())

	// Set an inbound fee and check that it is properly returned.
	edge1.ExtraOpaqueData = []byte{
		253, 217, 3, 8, 0, 0, 0, 10, 0, 0, 0, 20,
	}
	inboundFee := lnwire.Fee{
		BaseFee: 10,
		FeeRate: 20,
	}
	edge1.InboundFee = fn.Some(inboundFee)
	require.NoError(t, graph.UpdateEdgePolicy(ctx, edge1))
	edge1 = copyEdgePolicy(edge1) // Avoid read/write race conditions.

	directedChan := getSingleChannel()
	require.NotNil(t, directedChan)
	require.Equal(t, inboundFee, directedChan.InboundFee)

	// Set an invalid inbound fee and check that persistence fails.
	edge1.ExtraOpaqueData = []byte{
		253, 217, 3, 8, 0,
	}
	// We need to update the timestamp so that we don't hit the DB conflict
	// error when we try to update the edge policy.
	edge1.LastUpdate = edge1.LastUpdate.Add(time.Second)
	require.ErrorIs(
		t, graph.UpdateEdgePolicy(ctx, edge1), ErrParsingExtraTLVBytes,
	)

	// Since persistence of the last update failed, we should still bet
	// the previous result when we query the channel again.
	directedChan = getSingleChannel()
	require.NotNil(t, directedChan)
	require.Equal(t, inboundFee, directedChan.InboundFee)
}

// TestGraphLoading asserts that the cache is properly reconstructed after a
// restart.
func TestGraphLoading(t *testing.T) {
	t.Parallel()

	// Next, create the graph for the first time.
	graphStore := NewTestDB(t)

	graph, err := NewChannelGraph(graphStore)
	require.NoError(t, err)
	require.NoError(t, graph.Start())
	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	// Populate the graph with test data.
	const numNodes = 100
	const numChannels = 4
	_, _ = fillTestGraph(t, graph, numNodes, numChannels)

	// Recreate the graph. This should cause the graph cache to be
	// populated.
	graphReloaded, err := NewChannelGraph(graphStore)
	require.NoError(t, err)
	require.NoError(t, graphReloaded.Start())
	t.Cleanup(func() {
		require.NoError(t, graphReloaded.Stop())
	})

	// Assert that the cache content is identical.
	require.Equal(
		t, graph.graphCache.nodeChannels,
		graphReloaded.graphCache.nodeChannels,
	)

	require.Equal(
		t, graph.graphCache.nodeFeatures,
		graphReloaded.graphCache.nodeFeatures,
	)
}

// TestClosedScid tests that we can correctly insert a SCID into the index of
// closed short channel ids.
func TestClosedScid(t *testing.T) {
	t.Parallel()

	graph := MakeTestGraph(t)

	scid := lnwire.ShortChannelID{}

	// The scid should not exist in the closedScidBucket.
	exists, err := graph.IsClosedScid(scid)
	require.Nil(t, err)
	require.False(t, exists)

	// After we call PutClosedScid, the call to IsClosedScid should return
	// true.
	err = graph.PutClosedScid(scid)
	require.Nil(t, err)

	exists, err = graph.IsClosedScid(scid)
	require.Nil(t, err)
	require.True(t, exists)
}

// testNodeAnn is a serialized node announcement message which contains an
// address type (6) that LND is not aware of.
var testNodeAnn = "01012674c2e7ef68c73a086b7de2603f4ef1567358df84bb4edaa06c" +
	"f2132965b14e2434faab04170f0089216accbd79188fa3d40dbb0438bd89782cae" +
	"27cc656bf60007800088082a69a2625e7a2a024b9a1fa8e006f1e3937f65f66c40" +
	"8e6da8e1ca728ea43222a7381df1cc449605024b9a424c554549524f4e2d76302e" +
	"31312e307263332d362d67663963613934650000001d0180c7caa8260702240061" +
	"80000000d0000000005cd2a001260706204c"

// TestLightningNodePersistence takes a raw serialized node announcement
// message, converts it to our internal models.Node type, persists it
// to disk, reads it again and converts it back to a wire message and asserts
// that the two messages are equal.
func TestLightningNodePersistence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Create a new test graph instance.
	graph := MakeTestGraph(t)

	nodeAnnBytes, err := hex.DecodeString(testNodeAnn)
	require.NoError(t, err)

	// Use the raw serialized node announcement message create an
	// lnwire.NodeAnnouncement1 instance.
	msg, err := lnwire.ReadMessage(bytes.NewBuffer(nodeAnnBytes), 0)
	require.NoError(t, err)
	na, ok := msg.(*lnwire.NodeAnnouncement1)
	require.True(t, ok)

	// Convert the wire message to our internal node representation.
	node := models.NodeFromWireAnnouncement(na)

	// Persist the node to disk.
	err = graph.AddNode(ctx, node)
	require.NoError(t, err)

	// Read the node from disk.
	diskNode, err := graph.FetchNode(ctx, node.PubKeyBytes)
	require.NoError(t, err)

	// Convert it back to a wire message.
	wireMsg, err := diskNode.NodeAnnouncement(true)
	require.NoError(t, err)

	// Encode it and compare against the original.
	var b bytes.Buffer
	_, err = lnwire.WriteMessage(&b, wireMsg, 0)
	require.NoError(t, err)

	require.Equal(t, nodeAnnBytes, b.Bytes())
}
