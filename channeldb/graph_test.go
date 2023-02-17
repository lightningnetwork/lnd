package channeldb

import (
	"bytes"
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
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr, anotherAddr}

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar   = new(btcec.ModNScalar)
	testSScalar   = new(btcec.ModNScalar)
	_             = testRScalar.SetByteSlice(testRBytes)
	_             = testSScalar.SetByteSlice(testSBytes)
	testSig       = ecdsa.NewSignature(testRScalar, testSScalar)

	testFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.GossipQueriesRequired),
		lnwire.Features,
	)

	testPub = route.Vertex{2, 202, 4}
)

// MakeTestGraph creates a new instance of the ChannelGraph for testing purposes.
func MakeTestGraph(t testing.TB, modifiers ...OptionModifier) (*ChannelGraph, error) {
	opts := DefaultOptions()
	for _, modifier := range modifiers {
		modifier(&opts)
	}

	// Next, create channelgraph for the first time.
	backend, backendCleanup, err := kvdb.GetTestBackend(t.TempDir(), "cgr")
	if err != nil {
		backendCleanup()
		return nil, err
	}

	graph, err := NewChannelGraph(
		backend, opts.RejectCacheSize, opts.ChannelCacheSize,
		opts.BatchCommitInterval, opts.PreAllocCacheNumNodes,
		true, false,
	)
	if err != nil {
		backendCleanup()
		return nil, err
	}

	t.Cleanup(func() {
		_ = backend.Close()
		backendCleanup()
	})

	return graph, nil
}

func createLightningNode(db kvdb.Backend, priv *btcec.PrivateKey) (*LightningNode, error) {
	updateTime := prand.Int63()

	pub := priv.PubKey().SerializeCompressed()
	n := &LightningNode{
		HaveNodeAnnouncement: true,
		AuthSigBytes:         testSig.Serialize(),
		LastUpdate:           time.Unix(updateTime, 0),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "kek" + string(pub[:]),
		Features:             testFeatures,
		Addresses:            testAddrs,
		db:                   db,
	}
	copy(n.PubKeyBytes[:], priv.PubKey().SerializeCompressed())

	return n, nil
}

func createTestVertex(db kvdb.Backend) (*LightningNode, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	return createLightningNode(db, priv)
}

func TestNodeInsertionAndDeletion(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test basic insertion/deletion for vertexes from the
	// graph, so we'll create a test vertex to start with.
	node := &LightningNode{
		HaveNodeAnnouncement: true,
		AuthSigBytes:         testSig.Serialize(),
		LastUpdate:           time.Unix(1232342, 0),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "kek",
		Features:             testFeatures,
		Addresses:            testAddrs,
		ExtraOpaqueData:      []byte("extra new data"),
		PubKeyBytes:          testPub,
		db:                   graph.db,
	}

	// First, insert the node into the graph DB. This should succeed
	// without any errors.
	if err := graph.AddLightningNode(node); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node, testFeatures)

	// Next, fetch the node from the database to ensure everything was
	// serialized properly.
	dbNode, err := graph.FetchLightningNode(testPub)
	require.NoError(t, err, "unable to locate node")

	if _, exists, err := graph.HasLightningNode(dbNode.PubKeyBytes); err != nil {
		t.Fatalf("unable to query for node: %v", err)
	} else if !exists {
		t.Fatalf("node should be found but wasn't")
	}

	// The two nodes should match exactly!
	if err := compareNodes(node, dbNode); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	if err := graph.DeleteLightningNode(testPub); err != nil {
		t.Fatalf("unable to delete node; %v", err)
	}
	assertNodeNotInCache(t, graph, testPub)

	// Finally, attempt to fetch the node again. This should fail as the
	// node should have been deleted from the database.
	_, err = graph.FetchLightningNode(testPub)
	if err != ErrGraphNodeNotFound {
		t.Fatalf("fetch after delete should fail!")
	}
}

// TestPartialNode checks that we can add and retrieve a LightningNode where
// where only the pubkey is known to the database.
func TestPartialNode(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We want to be able to insert nodes into the graph that only has the
	// PubKey set.
	node := &LightningNode{
		HaveNodeAnnouncement: false,
		PubKeyBytes:          testPub,
	}

	if err := graph.AddLightningNode(node); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node, nil)

	// Next, fetch the node from the database to ensure everything was
	// serialized properly.
	dbNode, err := graph.FetchLightningNode(testPub)
	require.NoError(t, err, "unable to locate node")

	if _, exists, err := graph.HasLightningNode(dbNode.PubKeyBytes); err != nil {
		t.Fatalf("unable to query for node: %v", err)
	} else if !exists {
		t.Fatalf("node should be found but wasn't")
	}

	// The two nodes should match exactly! (with default values for
	// LastUpdate and db set to satisfy compareNodes())
	node = &LightningNode{
		HaveNodeAnnouncement: false,
		LastUpdate:           time.Unix(0, 0),
		PubKeyBytes:          testPub,
		db:                   graph.db,
	}

	if err := compareNodes(node, dbNode); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	if err := graph.DeleteLightningNode(testPub); err != nil {
		t.Fatalf("unable to delete node: %v", err)
	}
	assertNodeNotInCache(t, graph, testPub)

	// Finally, attempt to fetch the node again. This should fail as the
	// node should have been deleted from the database.
	_, err = graph.FetchLightningNode(testPub)
	if err != ErrGraphNodeNotFound {
		t.Fatalf("fetch after delete should fail!")
	}
}

func TestAliasLookup(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test the alias index within the database, so first
	// create a new test node.
	testNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	// Add the node to the graph's database, this should also insert an
	// entry into the alias index for this node.
	if err := graph.AddLightningNode(testNode); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, attempt to lookup the alias. The alias should exactly match
	// the one which the test node was assigned.
	nodePub, err := testNode.PubKey()
	require.NoError(t, err, "unable to generate pubkey")
	dbAlias, err := graph.LookupAlias(nodePub)
	require.NoError(t, err, "unable to find alias")
	if dbAlias != testNode.Alias {
		t.Fatalf("aliases don't match, expected %v got %v",
			testNode.Alias, dbAlias)
	}

	// Ensure that looking up a non-existent alias results in an error.
	node, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	nodePub, err = node.PubKey()
	require.NoError(t, err, "unable to generate pubkey")
	_, err = graph.LookupAlias(nodePub)
	if err != ErrNodeAliasNotFound {
		t.Fatalf("alias lookup should fail for non-existent pubkey")
	}
}

func TestSourceNode(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test the setting/getting of the source node, so we
	// first create a fake node to use within the test.
	testNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	// Attempt to fetch the source node, this should return an error as the
	// source node hasn't yet been set.
	if _, err := graph.SourceNode(); err != ErrSourceNodeNotSet {
		t.Fatalf("source node shouldn't be set in new graph")
	}

	// Set the source the source node, this should insert the node into the
	// database in a special way indicating it's the source node.
	if err := graph.SetSourceNode(testNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// Retrieve the source node from the database, it should exactly match
	// the one we set above.
	sourceNode, err := graph.SourceNode()
	require.NoError(t, err, "unable to fetch source node")
	if err := compareNodes(testNode, sourceNode); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}
}

func TestEdgeInsertionDeletion(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

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
	edgeInfo := ChannelEdgeInfo{
		ChannelID: chanID,
		ChainHash: key,
		AuthProof: &ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		ChannelPoint: outpoint,
		Capacity:     9000,
	}
	copy(edgeInfo.NodeKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.NodeKey2Bytes[:], node2Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey1Bytes[:], node1Pub.SerializeCompressed())
	copy(edgeInfo.BitcoinKey2Bytes[:], node2Pub.SerializeCompressed())

	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	assertEdgeWithNoPoliciesInCache(t, graph, &edgeInfo)

	// Ensure that both policies are returned as unknown (nil).
	_, e1, e2, err := graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch channel edge")
	}
	if e1 != nil || e2 != nil {
		t.Fatalf("channel edges not unknown")
	}

	// Next, attempt to delete the edge from the database, again this
	// should proceed without any issues.
	if err := graph.DeleteChannelEdges(false, true, chanID); err != nil {
		t.Fatalf("unable to delete edge: %v", err)
	}
	assertNoEdge(t, graph, chanID)

	// Ensure that any query attempts to lookup the delete channel edge are
	// properly deleted.
	if _, _, _, err := graph.FetchChannelEdgesByOutpoint(&outpoint); err == nil {
		t.Fatalf("channel edge not deleted")
	}
	if _, _, _, err := graph.FetchChannelEdgesByID(chanID); err == nil {
		t.Fatalf("channel edge not deleted")
	}
	isZombie, _, _ := graph.IsZombieEdge(chanID)
	if !isZombie {
		t.Fatal("channel edge not marked as zombie")
	}

	// Finally, attempt to delete a (now) non-existent edge within the
	// database, this should result in an error.
	err = graph.DeleteChannelEdges(false, true, chanID)
	if err != ErrEdgeNotFound {
		t.Fatalf("deleting a non-existent edge should fail!")
	}
}

func createEdge(height, txIndex uint32, txPosition uint16, outPointIndex uint32,
	node1, node2 *LightningNode) (ChannelEdgeInfo, lnwire.ShortChannelID) {

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
	edgeInfo := ChannelEdgeInfo{
		ChannelID: shortChanID.ToUint64(),
		ChainHash: key,
		AuthProof: &ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		ChannelPoint: outpoint,
		Capacity:     9000,
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	sourceNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create source node")
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	var spendOutputs []*wire.OutPoint
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))

	// Prune the graph a few times to make sure we have entries in the
	// prune log.
	_, err = graph.PruneGraph(spendOutputs, &blockHash, 155)
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
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	if err := graph.AddChannelEdge(&edgeInfo2); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	if err := graph.AddChannelEdge(&edgeInfo3); err != nil {
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
	if !blockHash.IsEqual(hash) {
		t.Fatalf("expected best block to be %x, was %x", blockHash, hash)
	}
	if h != height-1 {
		t.Fatalf("expected best block height to be %d, was %d", height-1, h)
	}
}

func assertEdgeInfoEqual(t *testing.T, e1 *ChannelEdgeInfo,
	e2 *ChannelEdgeInfo) {

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

	if !bytes.Equal(e1.Features, e2.Features) {
		t.Fatalf("features doesn't match: %x vs %x", e1.Features,
			e2.Features)
	}

	if !bytes.Equal(e1.AuthProof.NodeSig1Bytes, e2.AuthProof.NodeSig1Bytes) {
		t.Fatalf("nodesig1 doesn't match: %v vs %v",
			spew.Sdump(e1.AuthProof.NodeSig1Bytes),
			spew.Sdump(e2.AuthProof.NodeSig1Bytes))
	}
	if !bytes.Equal(e1.AuthProof.NodeSig2Bytes, e2.AuthProof.NodeSig2Bytes) {
		t.Fatalf("nodesig2 doesn't match")
	}
	if !bytes.Equal(e1.AuthProof.BitcoinSig1Bytes, e2.AuthProof.BitcoinSig1Bytes) {
		t.Fatalf("bitcoinsig1 doesn't match")
	}
	if !bytes.Equal(e1.AuthProof.BitcoinSig2Bytes, e2.AuthProof.BitcoinSig2Bytes) {
		t.Fatalf("bitcoinsig2 doesn't match")
	}

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

func createChannelEdge(db kvdb.Backend, node1, node2 *LightningNode) (*ChannelEdgeInfo,
	*ChannelEdgePolicy, *ChannelEdgePolicy) {

	var (
		firstNode  *LightningNode
		secondNode *LightningNode
	)
	if bytes.Compare(node1.PubKeyBytes[:], node2.PubKeyBytes[:]) == -1 {
		firstNode = node1
		secondNode = node2
	} else {
		firstNode = node2
		secondNode = node1
	}

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	chanID := uint64(prand.Int63())
	outpoint := wire.OutPoint{
		Hash:  rev,
		Index: 9,
	}

	// Add the new edge to the database, this should proceed without any
	// errors.
	edgeInfo := &ChannelEdgeInfo{
		ChannelID: chanID,
		ChainHash: key,
		AuthProof: &ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		ChannelPoint:    outpoint,
		Capacity:        1000,
		ExtraOpaqueData: []byte("new unknown feature"),
	}
	copy(edgeInfo.NodeKey1Bytes[:], firstNode.PubKeyBytes[:])
	copy(edgeInfo.NodeKey2Bytes[:], secondNode.PubKeyBytes[:])
	copy(edgeInfo.BitcoinKey1Bytes[:], firstNode.PubKeyBytes[:])
	copy(edgeInfo.BitcoinKey2Bytes[:], secondNode.PubKeyBytes[:])

	edge1 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(433453, 0),
		MessageFlags:              1,
		ChannelFlags:              0,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		MaxHTLC:                   13928598,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 3452352,
		Node:                      secondNode,
		ExtraOpaqueData:           []byte("new unknown feature2"),
		db:                        db,
	}
	edge2 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(124234, 0),
		MessageFlags:              1,
		ChannelFlags:              1,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		MaxHTLC:                   13928598,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 90392423,
		Node:                      firstNode,
		ExtraOpaqueData:           []byte("new unknown feature1"),
		db:                        db,
	}

	return edgeInfo, edge1, edge2
}

func TestEdgeInfoUpdates(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node1, testFeatures)
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	assertNodeInCache(t, graph, node2, testFeatures)

	// Create an edge and add it to the db.
	edgeInfo, edge1, edge2 := createChannelEdge(graph.db, node1, node2)

	// Make sure inserting the policy at this point, before the edge info
	// is added, will fail.
	if err := graph.UpdateEdgePolicy(edge1); err != ErrEdgeNotFound {
		t.Fatalf("expected ErrEdgeNotFound, got: %v", err)
	}
	require.Len(t, graph.graphCache.nodeChannels, 0)

	// Add the edge info.
	if err := graph.AddChannelEdge(edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	assertEdgeWithNoPoliciesInCache(t, graph, edgeInfo)

	chanID := edgeInfo.ChannelID
	outpoint := edgeInfo.ChannelPoint

	// Next, insert both edge policies into the database, they should both
	// be inserted without any issues.
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	assertEdgeWithPolicyInCache(t, graph, edgeInfo, edge1, true)
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
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
	dbEdgeInfo, dbEdge1, dbEdge2, err = graph.FetchChannelEdgesByOutpoint(&outpoint)
	require.NoError(t, err, "unable to fetch channel by ID")
	if err := compareEdgePolicies(dbEdge1, edge1); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)
}

func assertNodeInCache(t *testing.T, g *ChannelGraph, n *LightningNode,
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
	e *ChannelEdgeInfo) {

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
	e *ChannelEdgeInfo, p *ChannelEdgePolicy, policy1 bool) {

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

func randEdgePolicy(chanID uint64, db kvdb.Backend) *ChannelEdgePolicy {
	update := prand.Int63()

	return newEdgePolicy(chanID, db, update)
}

func newEdgePolicy(chanID uint64, db kvdb.Backend,
	updateTime int64) *ChannelEdgePolicy {

	return &ChannelEdgePolicy{
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(updateTime, 0),
		MessageFlags:              1,
		ChannelFlags:              0,
		TimeLockDelta:             uint16(prand.Int63()),
		MinHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		MaxHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		FeeBaseMSat:               lnwire.MilliSatoshi(prand.Int63()),
		FeeProportionalMillionths: lnwire.MilliSatoshi(prand.Int63()),
		db:                        db,
	}
}

func TestGraphTraversal(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

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
	err = graph.ForEachNodeCached(func(node route.Vertex,
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
	})
	require.NoError(t, err)

	// Iterate through all the known channels within the graph DB, once
	// again if the map is empty that indicates that all edges have
	// properly been reached.
	err = graph.ForEachChannel(func(ei *ChannelEdgeInfo, _ *ChannelEdgePolicy,
		_ *ChannelEdgePolicy) error {

		delete(chanIndex, ei.ChannelID)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, chanIndex, 0)

	// Finally, we want to test the ability to iterate over all the
	// outgoing channels for a particular node.
	numNodeChans := 0
	firstNode, secondNode := nodeList[0], nodeList[1]
	err = firstNode.ForEachChannel(nil, func(_ kvdb.RTx, _ *ChannelEdgeInfo,
		outEdge, inEdge *ChannelEdgePolicy) error {

		// All channels between first and second node should have fully
		// (both sides) specified policies.
		if inEdge == nil || outEdge == nil {
			return fmt.Errorf("channel policy not present")
		}

		// Each should indicate that it's outgoing (pointed
		// towards the second node).
		if !bytes.Equal(outEdge.Node.PubKeyBytes[:], secondNode.PubKeyBytes[:]) {
			return fmt.Errorf("wrong outgoing edge")
		}

		// The incoming edge should also indicate that it's pointing to
		// the origin node.
		if !bytes.Equal(inEdge.Node.PubKeyBytes[:], firstNode.PubKeyBytes[:]) {
			return fmt.Errorf("wrong outgoing edge")
		}

		numNodeChans++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, numChannels, numNodeChans)
}

// TestGraphTraversalCacheable tests that the memory optimized node traversal is
// working correctly.
func TestGraphTraversalCacheable(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test some of the graph traversal capabilities within
	// the DB, so we'll create a series of fake nodes to insert into the
	// graph. And we'll create 5 channels between the first two nodes.
	const numNodes = 20
	const numChannels = 5
	chanIndex, _ := fillTestGraph(t, graph, numNodes, numChannels)

	// Create a map of all nodes with the iteration we know works (because
	// it is tested in another test).
	nodeMap := make(map[route.Vertex]struct{})
	err = graph.ForEachNode(func(tx kvdb.RTx, n *LightningNode) error {
		nodeMap[n.PubKeyBytes] = struct{}{}

		return nil
	})
	require.NoError(t, err)
	require.Len(t, nodeMap, numNodes)

	// Iterate through all the known channels within the graph DB by
	// iterating over each node, once again if the map is empty that
	// indicates that all edges have properly been reached.
	var nodes []GraphCacheNode
	err = graph.ForEachNodeCacheable(
		func(tx kvdb.RTx, node GraphCacheNode) error {
			delete(nodeMap, node.PubKey())

			nodes = append(nodes, node)

			return nil
		},
	)
	require.NoError(t, err)
	require.Len(t, nodeMap, 0)

	err = graph.db.View(func(tx kvdb.RTx) error {
		for _, node := range nodes {
			err := node.ForEachChannel(
				tx, func(tx kvdb.RTx, info *ChannelEdgeInfo,
					policy *ChannelEdgePolicy,
					policy2 *ChannelEdgePolicy) error {

					delete(chanIndex, info.ChannelID)
					return nil
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})

	require.NoError(t, err)
	require.Len(t, chanIndex, 0)
}

func TestGraphCacheTraversal(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err)

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

		err = graph.graphCache.ForEachChannel(
			node.PubKeyBytes, func(d *DirectedChannel) error {
				delete(chanIndex, d.ChannelID)

				if !d.OutPolicySet || d.InPolicy == nil {
					return fmt.Errorf("channel policy not " +
						"present")
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

func fillTestGraph(t require.TestingT, graph *ChannelGraph, numNodes,
	numChannels int) (map[uint64]struct{}, []*LightningNode) {

	nodes := make([]*LightningNode, numNodes)
	nodeIndex := map[string]struct{}{}
	for i := 0; i < numNodes; i++ {
		node, err := createTestVertex(graph.db)
		require.NoError(t, err)

		nodes[i] = node
		nodeIndex[node.Alias] = struct{}{}
	}

	// Add each of the nodes into the graph, they should be inserted
	// without error.
	for _, node := range nodes {
		require.NoError(t, graph.AddLightningNode(node))
	}

	// Iterate over each node as returned by the graph, if all nodes are
	// reached, then the map created above should be empty.
	err := graph.ForEachNode(func(_ kvdb.RTx, node *LightningNode) error {
		delete(nodeIndex, node.Alias)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, nodeIndex, 0)

	// Create a number of channels between each of the node pairs generated
	// above. This will result in numChannels*(numNodes-1) channels.
	chanIndex := map[uint64]struct{}{}
	for n := 0; n < numNodes-1; n++ {
		node1 := nodes[n]
		node2 := nodes[n+1]
		if bytes.Compare(node1.PubKeyBytes[:], node2.PubKeyBytes[:]) == -1 {
			node1, node2 = node2, node1
		}

		for i := 0; i < numChannels; i++ {
			txHash := sha256.Sum256([]byte{byte(i)})
			chanID := uint64((n << 8) + i + 1)
			op := wire.OutPoint{
				Hash:  txHash,
				Index: 0,
			}

			edgeInfo := ChannelEdgeInfo{
				ChannelID: chanID,
				ChainHash: key,
				AuthProof: &ChannelAuthProof{
					NodeSig1Bytes:    testSig.Serialize(),
					NodeSig2Bytes:    testSig.Serialize(),
					BitcoinSig1Bytes: testSig.Serialize(),
					BitcoinSig2Bytes: testSig.Serialize(),
				},
				ChannelPoint: op,
				Capacity:     1000,
			}
			copy(edgeInfo.NodeKey1Bytes[:], node1.PubKeyBytes[:])
			copy(edgeInfo.NodeKey2Bytes[:], node2.PubKeyBytes[:])
			copy(edgeInfo.BitcoinKey1Bytes[:], node1.PubKeyBytes[:])
			copy(edgeInfo.BitcoinKey2Bytes[:], node2.PubKeyBytes[:])
			err := graph.AddChannelEdge(&edgeInfo)
			require.NoError(t, err)

			// Create and add an edge with random data that points
			// from node1 -> node2.
			edge := randEdgePolicy(chanID, graph.db)
			edge.ChannelFlags = 0
			edge.Node = node2
			edge.SigBytes = testSig.Serialize()
			require.NoError(t, graph.UpdateEdgePolicy(edge))

			// Create another random edge that points from
			// node2 -> node1 this time.
			edge = randEdgePolicy(chanID, graph.db)
			edge.ChannelFlags = 1
			edge.Node = node1
			edge.SigBytes = testSig.Serialize()
			require.NoError(t, graph.UpdateEdgePolicy(edge))

			chanIndex[chanID] = struct{}{}
		}
	}

	return chanIndex, nodes
}

func assertPruneTip(t *testing.T, graph *ChannelGraph, blockHash *chainhash.Hash,
	blockHeight uint32) {

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
	if err := graph.ForEachChannel(func(*ChannelEdgeInfo, *ChannelEdgePolicy,
		*ChannelEdgePolicy) error {

		numChans++
		return nil
	}); err != nil {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: unable to scan channels: %v", line, err)
	}
	if numChans != n {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: expected %v chans instead have %v", line,
			n, numChans)
	}
}

func assertNumNodes(t *testing.T, graph *ChannelGraph, n int) {
	numNodes := 0
	err := graph.ForEachNode(func(_ kvdb.RTx, _ *LightningNode) error {
		numNodes++
		return nil
	})
	if err != nil {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: unable to scan nodes: %v", line, err)
	}

	if numNodes != n {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: expected %v nodes, got %v", line, n, numNodes)
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

func assertChanViewEqualChanPoints(t *testing.T, a []EdgePoint, b []*wire.OutPoint) {
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	sourceNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create source node")
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// As initial set up for the test, we'll create a graph with 5 vertexes
	// and enough edges to create a fully connected graph. The graph will
	// be rather simple, representing a straight line.
	const numNodes = 5
	graphNodes := make([]*LightningNode, numNodes)
	for i := 0; i < numNodes; i++ {
		node, err := createTestVertex(graph.db)
		if err != nil {
			t.Fatalf("unable to create node: %v", err)
		}

		if err := graph.AddLightningNode(node); err != nil {
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

		edgeInfo := ChannelEdgeInfo{
			ChannelID: chanID,
			ChainHash: key,
			AuthProof: &ChannelAuthProof{
				NodeSig1Bytes:    testSig.Serialize(),
				NodeSig2Bytes:    testSig.Serialize(),
				BitcoinSig1Bytes: testSig.Serialize(),
				BitcoinSig2Bytes: testSig.Serialize(),
			},
			ChannelPoint: op,
			Capacity:     1000,
		}
		copy(edgeInfo.NodeKey1Bytes[:], graphNodes[i].PubKeyBytes[:])
		copy(edgeInfo.NodeKey2Bytes[:], graphNodes[i+1].PubKeyBytes[:])
		copy(edgeInfo.BitcoinKey1Bytes[:], graphNodes[i].PubKeyBytes[:])
		copy(edgeInfo.BitcoinKey2Bytes[:], graphNodes[i+1].PubKeyBytes[:])
		if err := graph.AddChannelEdge(&edgeInfo); err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		pkScript, err := genMultiSigP2WSH(
			edgeInfo.BitcoinKey1Bytes[:], edgeInfo.BitcoinKey2Bytes[:],
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
		edge := randEdgePolicy(chanID, graph.db)
		edge.ChannelFlags = 0
		edge.Node = graphNodes[i]
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node_i+1 ->
		// node_i this time.
		edge = randEdgePolicy(chanID, graph.db)
		edge.ChannelFlags = 1
		edge.Node = graphNodes[i]
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// If we don't yet have any channels in the database, then we should
	// get a channel ID of zero if we ask for the highest channel ID.
	bestID, err := graph.HighestChanID()
	require.NoError(t, err, "unable to get highest ID")
	if bestID != 0 {
		t.Fatalf("best ID w/ no chan should be zero, is instead: %v",
			bestID)
	}

	// Next, we'll insert two channels into the database, with each channel
	// connecting the same two nodes.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	// The first channel with be at height 10, while the other will be at
	// height 100.
	edge1, _ := createEdge(10, 0, 0, 0, node1, node2)
	edge2, chanID2 := createEdge(100, 0, 0, 0, node1, node2)

	if err := graph.AddChannelEdge(&edge1); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	if err := graph.AddChannelEdge(&edge2); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Now that the edges has been inserted, we'll query for the highest
	// known channel ID in the database.
	bestID, err = graph.HighestChanID()
	require.NoError(t, err, "unable to get highest ID")

	if bestID != chanID2.ToUint64() {
		t.Fatalf("expected %v got %v for best chan ID: ",
			chanID2.ToUint64(), bestID)
	}

	// If we add another edge, then the current best chan ID should be
	// updated as well.
	edge3, chanID3 := createEdge(1000, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edge3); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	bestID, err = graph.HighestChanID()
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// If we issue an arbitrary query before any channel updates are
	// inserted in the database, we should get zero results.
	chanUpdates, err := graph.ChanUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)
	require.NoError(t, err, "unable to updates for updates")
	if len(chanUpdates) != 0 {
		t.Fatalf("expected 0 chan updates, instead got %v",
			len(chanUpdates))
	}

	// We'll start by creating two nodes which will seed our test graph.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
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

		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		edge1UpdateTime := endTime
		edge2UpdateTime := edge1UpdateTime.Add(time.Second)
		endTime = endTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(
			chanID.ToUint64(), graph.db, edge1UpdateTime.Unix(),
		)
		edge1.ChannelFlags = 0
		edge1.Node = node2
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(
			chanID.ToUint64(), graph.db, edge2UpdateTime.Unix(),
		)
		edge2.ChannelFlags = 1
		edge2.Node = node1
		edge2.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge2); err != nil {
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
		resp, err := graph.ChanUpdatesInHorizon(
			queryCase.start, queryCase.end,
		)
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

			err := compareEdgePolicies(chanExp.Policy1, chanRet.Policy1)
			if err != nil {
				t.Fatal(err)
			}
			compareEdgePolicies(chanExp.Policy2, chanRet.Policy2)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestNodeUpdatesInHorizon tests that we're able to properly scan and retrieve
// the most recent node updates within a particular time horizon.
func TestNodeUpdatesInHorizon(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	startTime := time.Unix(1234, 0)
	endTime := startTime

	// If we issue an arbitrary query before we insert any nodes into the
	// database, then we shouldn't get any results back.
	nodeUpdates, err := graph.NodeUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)
	require.NoError(t, err, "unable to query for node updates")
	if len(nodeUpdates) != 0 {
		t.Fatalf("expected 0 node updates, instead got %v",
			len(nodeUpdates))
	}

	// We'll create 10 node announcements, each with an update timestamp 10
	// seconds after the other.
	const numNodes = 10
	nodeAnns := make([]LightningNode, 0, numNodes)
	for i := 0; i < numNodes; i++ {
		nodeAnn, err := createTestVertex(graph.db)
		if err != nil {
			t.Fatalf("unable to create test vertex: %v", err)
		}

		// The node ann will use the current end time as its last
		// update them, then we'll add 10 seconds in order to create
		// the proper update time for the next node announcement.
		updateTime := endTime
		endTime = updateTime.Add(time.Second * 10)

		nodeAnn.LastUpdate = updateTime

		nodeAnns = append(nodeAnns, *nodeAnn)

		if err := graph.AddLightningNode(nodeAnn); err != nil {
			t.Fatalf("unable to add lightning node: %v", err)
		}
	}

	queryCases := []struct {
		start time.Time
		end   time.Time

		resp []LightningNode
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

		// If we reduce the ending time by 10 seconds, then we should
		// get all but the last node we inserted.
		{
			start: startTime,
			end:   endTime.Add(-time.Second * 10),

			resp: nodeAnns[:9],
		},
	}
	for _, queryCase := range queryCases {
		resp, err := graph.NodeUpdatesInHorizon(queryCase.start, queryCase.end)
		if err != nil {
			t.Fatalf("unable to query for nodes: %v", err)
		}

		if len(resp) != len(queryCase.resp) {
			t.Fatalf("expected %v nodes, got %v nodes",
				len(queryCase.resp), len(resp))

		}

		for i := 0; i < len(resp); i++ {
			err := compareNodes(&queryCase.resp[i], &resp[i])
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestFilterKnownChanIDs tests that we're able to properly perform the set
// differences of an incoming set of channel ID's, and those that we already
// know of on disk.
func TestFilterKnownChanIDs(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// If we try to filter out a set of channel ID's before we even know of
	// any channels, then we should get the entire set back.
	preChanIDs := []uint64{1, 2, 3, 4}
	filteredIDs, err := graph.FilterKnownChanIDs(preChanIDs)
	require.NoError(t, err, "unable to filter chan IDs")
	if !reflect.DeepEqual(preChanIDs, filteredIDs) {
		t.Fatalf("chan IDs shouldn't have been filtered!")
	}

	// We'll start by creating two nodes which will seed our test graph.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, we'll add 5 channel ID's to the graph, each of them having a
	// block height 10 blocks after the previous.
	const numChans = 5
	chanIDs := make([]uint64, 0, numChans)
	for i := 0; i < numChans; i++ {
		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		chanIDs = append(chanIDs, chanID.ToUint64())
	}

	const numZombies = 5
	zombieIDs := make([]uint64, 0, numZombies)
	for i := 0; i < numZombies; i++ {
		channel, chanID := createEdge(
			uint32(i*10+1), 0, 0, 0, node1, node2,
		)
		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}
		err := graph.DeleteChannelEdges(false, true, channel.ChannelID)
		if err != nil {
			t.Fatalf("unable to mark edge zombie: %v", err)
		}

		zombieIDs = append(zombieIDs, chanID.ToUint64())
	}

	queryCases := []struct {
		queryIDs []uint64

		resp []uint64
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
			queryIDs: []uint64{99, 100},
			resp:     []uint64{99, 100},
		},

		// If we query for a super-set of our the chan ID's inserted,
		// we should only get those new chanIDs back.
		{
			queryIDs: append(chanIDs, []uint64{99, 101}...),
			resp:     []uint64{99, 101},
		},
	}

	for _, queryCase := range queryCases {
		resp, err := graph.FilterKnownChanIDs(queryCase.queryIDs)
		if err != nil {
			t.Fatalf("unable to filter chan IDs: %v", err)
		}

		if !reflect.DeepEqual(resp, queryCase.resp) {
			t.Fatalf("expected %v, got %v", spew.Sdump(queryCase.resp),
				spew.Sdump(resp))
		}
	}
}

// TestFilterChannelRange tests that we're able to properly retrieve the full
// set of short channel ID's for a given block range.
func TestFilterChannelRange(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// If we try to filter a channel range before we have any channels
	// inserted, we should get an empty slice of results.
	resp, err := graph.FilterChannelRange(10, 100)
	require.NoError(t, err, "unable to filter channels")
	if len(resp) != 0 {
		t.Fatalf("expected zero chans, instead got %v", len(resp))
	}

	// To start, we'll create a set of channels, two mined in a block 10
	// blocks after the prior one.
	startHeight := uint32(100)
	endHeight := startHeight
	const numChans = 10
	channelRanges := make([]BlockChannelRange, 0, numChans/2)
	for i := 0; i < numChans/2; i++ {
		chanHeight := endHeight
		channel1, chanID1 := createEdge(
			chanHeight, uint32(i+1), 0, 0, node1, node2,
		)
		if err := graph.AddChannelEdge(&channel1); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		channel2, chanID2 := createEdge(
			chanHeight, uint32(i+2), 0, 0, node1, node2,
		)
		if err := graph.AddChannelEdge(&channel2); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		channelRanges = append(channelRanges, BlockChannelRange{
			Height:   chanHeight,
			Channels: []lnwire.ShortChannelID{chanID1, chanID2},
		})
		endHeight += 10
	}

	// With our channels inserted, we'll construct a series of queries that
	// we'll execute below in order to exercise the features of the
	// FilterKnownChanIDs method.
	queryCases := []struct {
		startHeight uint32
		endHeight   uint32

		resp []BlockChannelRange
	}{
		// If we query for the entire range, then we should get the same
		// set of short channel IDs back.
		{
			startHeight: startHeight,
			endHeight:   endHeight,

			resp: channelRanges,
		},

		// If we query for a range of channels right before our range, we
		// shouldn't get any results back.
		{
			startHeight: 0,
			endHeight:   10,
		},

		// If we only query for the last height (range wise), we should
		// only get that last channel.
		{
			startHeight: endHeight - 10,
			endHeight:   endHeight - 10,

			resp: channelRanges[4:],
		},

		// If we query for just the first height, we should only get a
		// single channel back (the first one).
		{
			startHeight: startHeight,
			endHeight:   startHeight,

			resp: channelRanges[:1],
		},

		{
			startHeight: startHeight + 10,
			endHeight:   endHeight - 10,

			resp: channelRanges[1:5],
		},
	}
	for i, queryCase := range queryCases {
		resp, err := graph.FilterChannelRange(
			queryCase.startHeight, queryCase.endHeight,
		)
		if err != nil {
			t.Fatalf("unable to issue range query: %v", err)
		}

		if !reflect.DeepEqual(resp, queryCase.resp) {
			t.Fatalf("case #%v: expected %v, got %v", i,
				queryCase.resp, resp)
		}
	}
}

// TestFetchChanInfos tests that we're able to properly retrieve the full set
// of ChannelEdge structs for a given set of short channel ID's.
func TestFetchChanInfos(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
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

		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		updateTime := endTime
		endTime = updateTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(
			chanID.ToUint64(), graph.db, updateTime.Unix(),
		)
		edge1.ChannelFlags = 0
		edge1.Node = node2
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(
			chanID.ToUint64(), graph.db, updateTime.Unix(),
		)
		edge2.ChannelFlags = 1
		edge2.Node = node1
		edge2.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge2); err != nil {
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
	if err := graph.AddChannelEdge(&zombieChan); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}
	err = graph.DeleteChannelEdges(false, true, zombieChan.ChannelID)
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// Create two nodes.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	channel, chanID := createEdge(
		uint32(0), 0, 0, 0, node1, node2,
	)

	if err := graph.AddChannelEdge(&channel); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Ensure that channel is reported with unknown policies.
	checkPolicies := func(node *LightningNode, expectedIn, expectedOut bool) {
		calls := 0
		err := node.ForEachChannel(nil, func(_ kvdb.RTx, _ *ChannelEdgeInfo,
			outEdge, inEdge *ChannelEdgePolicy) error {

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
		})
		if err != nil {
			t.Fatalf("unable to scan channels: %v", err)
		}

		if calls != 1 {
			t.Fatalf("Expected only one callback call")
		}
	}

	checkPolicies(node2, false, false)

	// Only create an edge policy for node1 and leave the policy for node2
	// unknown.
	updateTime := time.Unix(1234, 0)

	edgePolicy := newEdgePolicy(
		chanID.ToUint64(), graph.db, updateTime.Unix(),
	)
	edgePolicy.ChannelFlags = 0
	edgePolicy.Node = node2
	edgePolicy.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	checkPolicies(node1, false, true)
	checkPolicies(node2, true, false)

	// Create second policy and assert that both policies are reported
	// as present.
	edgePolicy = newEdgePolicy(
		chanID.ToUint64(), graph.db, updateTime.Unix(),
	)
	edgePolicy.ChannelFlags = 1
	edgePolicy.Node = node1
	edgePolicy.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	sourceNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create source node")
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// With the two nodes created, we'll now create a random channel, as
	// well as two edges in the database with distinct update times.
	edgeInfo, chanID := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge1 := randEdgePolicy(chanID.ToUint64(), graph.db)
	edge1.ChannelFlags = 0
	edge1.Node = node1
	edge1.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	edge2 := randEdgePolicy(chanID.ToUint64(), graph.db)
	edge2.ChannelFlags = 1
	edge2.Node = node2
	edge2.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// checkIndexTimestamps is a helper function that checks the edge update
	// index only includes the given timestamps.
	checkIndexTimestamps := func(timestamps ...uint64) {
		timestampSet := make(map[uint64]struct{})
		for _, t := range timestamps {
			timestampSet[t] = struct{}{}
		}

		err := kvdb.View(graph.db, func(tx kvdb.RTx) error {
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
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	edge2.ChannelFlags = 3
	edge2.LastUpdate = edge1.LastUpdate.Add(time.Hour)
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
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
	_, err = graph.PruneGraph(
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

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'll start off by inserting our source node, to ensure that it's
	// the only node left after we prune the graph.
	sourceNode, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create source node")
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// With the source node inserted, we'll now add three nodes to the
	// channel graph, at the end of the scenario, only two of these nodes
	// should still be in the graph.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node3, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node3); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll now add a new edge to the graph, but only actually advertise
	// the edge of *one* of the nodes.
	edgeInfo, chanID := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// We'll now insert an advertised edge, but it'll only be the edge that
	// points from the first to the second node.
	edge1 := randEdgePolicy(chanID.ToUint64(), graph.db)
	edge1.ChannelFlags = 0
	edge1.Node = node1
	edge1.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
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
	_, err = graph.FetchLightningNode(node3.PubKeyBytes)
	if err == nil {
		t.Fatalf("node 3 should have been deleted!")
	}
}

// TestAddChannelEdgeShellNodes tests that when we attempt to add a ChannelEdge
// to the graph, one or both of the nodes the edge involves aren't found in the
// database, then shell edges are created for each node if needed.
func TestAddChannelEdgeShellNodes(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// To start, we'll create two nodes, and only add one of them to the
	// channel graph.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	// We'll now create an edge between the two nodes, as a result, node2
	// should be inserted into the database as a shell node.
	edgeInfo, _ := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Ensure that node1 was inserted as a full node, while node2 only has
	// a shell node present.
	node1, err = graph.FetchLightningNode(node1.PubKeyBytes)
	require.NoError(t, err, "unable to fetch node1")
	if !node1.HaveNodeAnnouncement {
		t.Fatalf("have shell announcement for node1, shouldn't")
	}

	node2, err = graph.FetchLightningNode(node2.PubKeyBytes)
	require.NoError(t, err, "unable to fetch node2")
	if node2.HaveNodeAnnouncement {
		t.Fatalf("should have shell announcement for node2, but is full")
	}
}

// TestNodePruningUpdateIndexDeletion tests that once a node has been removed
// from the channel graph, we also remove the entry from the update index as
// well.
func TestNodePruningUpdateIndexDeletion(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'll first populate our graph with a single node that will be
	// removed shortly.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll confirm that we can retrieve the node using
	// NodeUpdatesInHorizon, using a time that's slightly beyond the last
	// update time of our test node.
	startTime := time.Unix(9, 0)
	endTime := node1.LastUpdate.Add(time.Minute)
	nodesInHorizon, err := graph.NodeUpdatesInHorizon(startTime, endTime)
	require.NoError(t, err, "unable to fetch nodes in horizon")

	// We should only have a single node, and that node should exactly
	// match the node we just inserted.
	if len(nodesInHorizon) != 1 {
		t.Fatalf("should have 1 nodes instead have: %v",
			len(nodesInHorizon))
	}
	if err := compareNodes(node1, &nodesInHorizon[0]); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}

	// We'll now delete the node from the graph, this should result in it
	// being removed from the update index as well.
	if err := graph.DeleteLightningNode(node1.PubKeyBytes); err != nil {
		t.Fatalf("unable to delete node: %v", err)
	}

	// Now that the node has been deleted, we'll again query the nodes in
	// the horizon. This time we should have no nodes at all.
	nodesInHorizon, err = graph.NodeUpdatesInHorizon(startTime, endTime)
	require.NoError(t, err, "unable to fetch nodes in horizon")

	if len(nodesInHorizon) != 0 {
		t.Fatalf("should have zero nodes instead have: %v",
			len(nodesInHorizon))
	}
}

// TestNodeIsPublic ensures that we properly detect nodes that are seen as
// public within the network graph.
func TestNodeIsPublic(t *testing.T) {
	t.Parallel()

	// We'll start off the test by creating a small network of 3
	// participants with the following graph:
	//
	//	Alice <-> Bob <-> Carol
	//
	// We'll need to create a separate database and channel graph for each
	// participant to replicate real-world scenarios (private edges being in
	// some graphs but not others, etc.).
	aliceGraph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")
	aliceNode, err := createTestVertex(aliceGraph.db)
	require.NoError(t, err, "unable to create test node")
	if err := aliceGraph.SetSourceNode(aliceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	bobGraph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")
	bobNode, err := createTestVertex(bobGraph.db)
	require.NoError(t, err, "unable to create test node")
	if err := bobGraph.SetSourceNode(bobNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	carolGraph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")
	carolNode, err := createTestVertex(carolGraph.db)
	require.NoError(t, err, "unable to create test node")
	if err := carolGraph.SetSourceNode(carolNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	aliceBobEdge, _ := createEdge(10, 0, 0, 0, aliceNode, bobNode)
	bobCarolEdge, _ := createEdge(10, 1, 0, 1, bobNode, carolNode)

	// After creating all of our nodes and edges, we'll add them to each
	// participant's graph.
	nodes := []*LightningNode{aliceNode, bobNode, carolNode}
	edges := []*ChannelEdgeInfo{&aliceBobEdge, &bobCarolEdge}
	dbs := []kvdb.Backend{aliceGraph.db, bobGraph.db, carolGraph.db}
	graphs := []*ChannelGraph{aliceGraph, bobGraph, carolGraph}
	for i, graph := range graphs {
		for _, node := range nodes {
			node.db = dbs[i]
			if err := graph.AddLightningNode(node); err != nil {
				t.Fatalf("unable to add node: %v", err)
			}
		}
		for _, edge := range edges {
			edge.db = dbs[i]
			if err := graph.AddChannelEdge(edge); err != nil {
				t.Fatalf("unable to add edge: %v", err)
			}
		}
	}

	// checkNodes is a helper closure that will be used to assert that the
	// given nodes are seen as public/private within the given graphs.
	checkNodes := func(nodes []*LightningNode, graphs []*ChannelGraph,
		public bool) {

		t.Helper()

		for _, node := range nodes {
			for _, graph := range graphs {
				isPublic, err := graph.IsPublicNode(node.PubKeyBytes)
				if err != nil {
					t.Fatalf("unable to determine if pivot "+
						"is public: %v", err)
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
		[]*LightningNode{aliceNode},
		[]*ChannelGraph{bobGraph, carolGraph},
		false,
	)

	// We'll also make the edge between Bob and Carol private. Within Bob's
	// and Carol's graph, the edge will exist, but it will not have a proof
	// that allows it to be advertised. Within Alice's graph, we'll
	// completely remove the edge as it is not possible for her to know of
	// it without it being advertised.
	for i, graph := range graphs {
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
		bobCarolEdge.db = dbs[i]
		if err := graph.AddChannelEdge(&bobCarolEdge); err != nil {
			t.Fatalf("unable to add edge: %v", err)
		}
	}

	// With the modifications above, Bob should now be seen as a private
	// node from both Alice's and Carol's perspective.
	checkNodes(
		[]*LightningNode{bobNode},
		[]*ChannelGraph{aliceGraph, carolGraph},
		false,
	)
}

// TestDisabledChannelIDs ensures that the disabled channels within the
// disabledEdgePolicyBucket are managed properly and the list returned from
// DisabledChannelIDs is correct.
func TestDisabledChannelIDs(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// Create first node and add it to the graph.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Create second node and add it to the graph.
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Adding a new channel edge to the graph.
	edgeInfo, edge1, edge2 := createChannelEdge(graph.db, node1, node2)
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	if err := graph.AddChannelEdge(edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Ensure no disabled channels exist in the bucket on start.
	disabledChanIds, err := graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled channels",
			len(disabledChanIds))
	}

	// Add one disabled policy and ensure the channel is still not in the
	// disabled list.
	edge1.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled channels",
			len(disabledChanIds))
	}

	// Add second disabled policy and ensure the channel is now in the
	// disabled list.
	edge2.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) != 1 || disabledChanIds[0] != edgeInfo.ChannelID {
		t.Fatalf("expected disabled channel with id %v, "+
			"got %v", edgeInfo.ChannelID, disabledChanIds)
	}

	// Delete the channel edge and ensure it is removed from the disabled list.
	if err = graph.DeleteChannelEdges(
		false, true, edgeInfo.ChannelID,
	); err != nil {
		t.Fatalf("unable to delete channel edge: %v", err)
	}
	disabledChanIds, err = graph.DisabledChannelIDs()
	require.NoError(t, err, "unable to get disabled channel ids")
	if len(disabledChanIds) > 0 {
		t.Fatalf("expected empty disabled channels, got %v disabled channels",
			len(disabledChanIds))
	}
}

// TestEdgePolicyMissingMaxHtcl tests that if we find a ChannelEdgePolicy in
// the DB that indicates that it should support the htlc_maximum_value_msat
// field, but it is not part of the opaque data, then we'll handle it as it is
// unknown. It also checks that we are correctly able to overwrite it when we
// receive the proper update.
func TestEdgePolicyMissingMaxHtcl(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test node")

	edgeInfo, edge1, edge2 := createChannelEdge(graph.db, node1, node2)
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	if err := graph.AddChannelEdge(edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	chanID := edgeInfo.ChannelID
	from := edge2.Node.PubKeyBytes[:]
	to := edge1.Node.PubKeyBytes[:]

	// We'll remove the no max_htlc field from the first edge policy, and
	// all other opaque data, and serialize it.
	edge1.MessageFlags = 0
	edge1.ExtraOpaqueData = nil

	var b bytes.Buffer
	err = serializeChanEdgePolicy(&b, edge1, to)
	if err != nil {
		t.Fatalf("unable to serialize policy")
	}

	// Set the max_htlc field. The extra bytes added to the serialization
	// will be the opaque data containing the serialized field.
	edge1.MessageFlags = lnwire.ChanUpdateRequiredMaxHtlc
	edge1.MaxHTLC = 13928598
	var b2 bytes.Buffer
	err = serializeChanEdgePolicy(&b2, edge1, to)
	if err != nil {
		t.Fatalf("unable to serialize policy")
	}

	withMaxHtlc := b2.Bytes()

	// Remove the opaque data from the serialization.
	stripped := withMaxHtlc[:len(b.Bytes())]

	// Attempting to deserialize these bytes should return an error.
	r := bytes.NewReader(stripped)
	err = kvdb.View(graph.db, func(tx kvdb.RTx) error {
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		_, err = deserializeChanEdgePolicy(r, nodes)
		if err != ErrEdgePolicyOptionalFieldNotFound {
			t.Fatalf("expected "+
				"ErrEdgePolicyOptionalFieldNotFound, got %v",
				err)
		}

		return nil
	}, func() {})
	require.NoError(t, err, "error reading db")

	// Put the stripped bytes in the DB.
	err = kvdb.Update(graph.db, func(tx kvdb.RwTx) error {
		edges := tx.ReadWriteBucket(edgeBucket)
		if edges == nil {
			return ErrEdgeNotFound
		}

		edgeIndex := edges.NestedReadWriteBucket(edgeIndexBucket)
		if edgeIndex == nil {
			return ErrEdgeNotFound
		}

		var edgeKey [33 + 8]byte
		copy(edgeKey[:], from)
		byteOrder.PutUint64(edgeKey[33:], edge1.ChannelID)

		var scratch [8]byte
		var indexKey [8 + 8]byte
		copy(indexKey[:], scratch[:])
		byteOrder.PutUint64(indexKey[8:], edge1.ChannelID)

		updateIndex, err := edges.CreateBucketIfNotExists(edgeUpdateIndexBucket)
		if err != nil {
			return err
		}

		if err := updateIndex.Put(indexKey[:], nil); err != nil {
			return err
		}

		return edges.Put(edgeKey[:], stripped)
	}, func() {})
	require.NoError(t, err, "error writing db")

	// And add the second, unmodified edge.
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

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
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
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

	// We'll start by creating our test graph along with a test edge.
	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to create test database")

	node1, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test vertex")
	node2, err := createTestVertex(graph.db)
	require.NoError(t, err, "unable to create test vertex")

	// Swap the nodes if the second's pubkey is smaller than the first.
	// Without this, the comparisons at the end will fail probabilistically.
	if bytes.Compare(node2.PubKeyBytes[:], node1.PubKeyBytes[:]) < 0 {
		node1, node2 = node2, node1
	}

	edge, _, _ := createChannelEdge(graph.db, node1, node2)
	if err := graph.AddChannelEdge(edge); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Since the edge is known the graph and it isn't a zombie, IsZombieEdge
	// should not report the channel as a zombie.
	isZombie, _, _ := graph.IsZombieEdge(edge.ChannelID)
	if isZombie {
		t.Fatal("expected edge to not be marked as zombie")
	}
	assertNumZombies(t, graph, 0)

	// If we delete the edge and mark it as a zombie, then we should expect
	// to see it within the index.
	err = graph.DeleteChannelEdges(false, true, edge.ChannelID)
	require.NoError(t, err, "unable to mark edge as zombie")
	isZombie, pubKey1, pubKey2 := graph.IsZombieEdge(edge.ChannelID)
	if !isZombie {
		t.Fatal("expected edge to be marked as zombie")
	}
	if pubKey1 != node1.PubKeyBytes {
		t.Fatalf("expected pubKey1 %x, got %x", node1.PubKeyBytes,
			pubKey1)
	}
	if pubKey2 != node2.PubKeyBytes {
		t.Fatalf("expected pubKey2 %x, got %x", node2.PubKeyBytes,
			pubKey2)
	}
	assertNumZombies(t, graph, 1)

	// Similarly, if we mark the same edge as live, we should no longer see
	// it within the index.
	if err := graph.MarkEdgeLive(edge.ChannelID); err != nil {
		t.Fatalf("unable to mark edge as live: %v", err)
	}
	isZombie, _, _ = graph.IsZombieEdge(edge.ChannelID)
	if isZombie {
		t.Fatal("expected edge to not be marked as zombie")
	}
	assertNumZombies(t, graph, 0)

	// If we mark the edge as a zombie manually, then it should show up as
	// being a zombie once again.
	err = graph.MarkEdgeZombie(
		edge.ChannelID, node1.PubKeyBytes, node2.PubKeyBytes,
	)
	require.NoError(t, err, "unable to mark edge as zombie")
	isZombie, _, _ = graph.IsZombieEdge(edge.ChannelID)
	if !isZombie {
		t.Fatal("expected edge to be marked as zombie")
	}
	assertNumZombies(t, graph, 1)
}

// compareNodes is used to compare two LightningNodes while excluding the
// Features struct, which cannot be compared as the semantics for reserializing
// the featuresMap have not been defined.
func compareNodes(a, b *LightningNode) error {
	if a.LastUpdate != b.LastUpdate {
		return fmt.Errorf("node LastUpdate doesn't match: expected %v, \n"+
			"got %v", a.LastUpdate, b.LastUpdate)
	}
	if !reflect.DeepEqual(a.Addresses, b.Addresses) {
		return fmt.Errorf("Addresses doesn't match: expected %#v, \n "+
			"got %#v", a.Addresses, b.Addresses)
	}
	if !reflect.DeepEqual(a.PubKeyBytes, b.PubKeyBytes) {
		return fmt.Errorf("PubKey doesn't match: expected %#v, \n "+
			"got %#v", a.PubKeyBytes, b.PubKeyBytes)
	}
	if !reflect.DeepEqual(a.Color, b.Color) {
		return fmt.Errorf("Color doesn't match: expected %#v, \n "+
			"got %#v", a.Color, b.Color)
	}
	if !reflect.DeepEqual(a.Alias, b.Alias) {
		return fmt.Errorf("Alias doesn't match: expected %#v, \n "+
			"got %#v", a.Alias, b.Alias)
	}
	if !reflect.DeepEqual(a.db, b.db) {
		return fmt.Errorf("db doesn't match: expected %#v, \n "+
			"got %#v", a.db, b.db)
	}
	if !reflect.DeepEqual(a.HaveNodeAnnouncement, b.HaveNodeAnnouncement) {
		return fmt.Errorf("HaveNodeAnnouncement doesn't match: expected %#v, \n "+
			"got %#v", a.HaveNodeAnnouncement, b.HaveNodeAnnouncement)
	}
	if !bytes.Equal(a.ExtraOpaqueData, b.ExtraOpaqueData) {
		return fmt.Errorf("extra data doesn't match: %v vs %v",
			a.ExtraOpaqueData, b.ExtraOpaqueData)
	}

	return nil
}

// compareEdgePolicies is used to compare two ChannelEdgePolices using
// compareNodes, so as to exclude comparisons of the Nodes' Features struct.
func compareEdgePolicies(a, b *ChannelEdgePolicy) error {
	if a.ChannelID != b.ChannelID {
		return fmt.Errorf("ChannelID doesn't match: expected %v, "+
			"got %v", a.ChannelID, b.ChannelID)
	}
	if !reflect.DeepEqual(a.LastUpdate, b.LastUpdate) {
		return fmt.Errorf("edge LastUpdate doesn't match: expected %#v, \n "+
			"got %#v", a.LastUpdate, b.LastUpdate)
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
	if err := compareNodes(a.Node, b.Node); err != nil {
		return err
	}
	if !reflect.DeepEqual(a.db, b.db) {
		return fmt.Errorf("db doesn't match: expected %#v, \n "+
			"got %#v", a.db, b.db)
	}
	return nil
}

// TestLightningNodeSigVerification checks that we can use the LightningNode's
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

	// Create a LightningNode from the same private key.
	graph, err := MakeTestGraph(t)
	require.NoError(t, err, "unable to make test database")

	node, err := createLightningNode(graph.db, priv)
	require.NoError(t, err, "unable to create node")

	// And finally check that we can verify the same signature from the
	// pubkey returned from the lightning node.
	nodePub, err := node.PubKey()
	require.NoError(t, err, "unable to get pubkey")

	if !sign.Verify(data[:], nodePub) {
		t.Fatalf("unable to verify sig")
	}
}

// TestComputeFee tests fee calculation based on both in- and outgoing amt.
func TestComputeFee(t *testing.T) {
	var (
		policy = ChannelEdgePolicy{
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

	fwdFee := policy.ComputeFeeFromIncoming(outgoingAmt + fee)
	if fwdFee != expectedFee {
		t.Fatalf("expected fee %v, but got %v", fee, fwdFee)
	}
}

// TestBatchedAddChannelEdge asserts that BatchedAddChannelEdge properly
// executes multiple AddChannelEdge requests in a single txn.
func TestBatchedAddChannelEdge(t *testing.T) {
	t.Parallel()

	graph, err := MakeTestGraph(t)
	require.Nil(t, err)

	sourceNode, err := createTestVertex(graph.db)
	require.Nil(t, err)
	err = graph.SetSourceNode(sourceNode)
	require.Nil(t, err)

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.Nil(t, err)
	node2, err := createTestVertex(graph.db)
	require.Nil(t, err)

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	var spendOutputs []*wire.OutPoint
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))

	// Prune the graph a few times to make sure we have entries in the
	// prune log.
	_, err = graph.PruneGraph(spendOutputs, &blockHash, 155)
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

	edges := []ChannelEdgeInfo{edgeInfo, edgeInfo2, edgeInfo3}
	errChan := make(chan error, len(edges))
	errTimeout := errors.New("timeout adding batched channel")

	// Now add all these new edges to the database.
	var wg sync.WaitGroup
	for _, edge := range edges {
		wg.Add(1)
		go func(edge ChannelEdgeInfo) {
			defer wg.Done()

			select {
			case errChan <- graph.AddChannelEdge(&edge):
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

	graph, err := MakeTestGraph(t)
	require.Nil(t, err)

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1, err := createTestVertex(graph.db)
	require.Nil(t, err)
	err = graph.AddLightningNode(node1)
	require.Nil(t, err)
	node2, err := createTestVertex(graph.db)
	require.Nil(t, err)
	err = graph.AddLightningNode(node2)
	require.Nil(t, err)

	// Create an edge and add it to the db.
	edgeInfo, edge1, edge2 := createChannelEdge(graph.db, node1, node2)

	// Make sure inserting the policy at this point, before the edge info
	// is added, will fail.
	err = graph.UpdateEdgePolicy(edge1)
	require.Error(t, ErrEdgeNotFound, err)

	// Add the edge info.
	err = graph.AddChannelEdge(edgeInfo)
	require.Nil(t, err)

	errTimeout := errors.New("timeout adding batched channel")

	updates := []*ChannelEdgePolicy{edge1, edge2}

	errChan := make(chan error, len(updates))

	// Now add all these new edges to the database.
	var wg sync.WaitGroup
	for _, update := range updates {
		wg.Add(1)
		go func(update *ChannelEdgePolicy) {
			defer wg.Done()

			select {
			case errChan <- graph.UpdateEdgePolicy(update):
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
	graph, err := MakeTestGraph(b)
	require.Nil(b, err)

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

		var nodes []GraphCacheNode
		err = graph.ForEachNodeCacheable(
			func(tx kvdb.RTx, node GraphCacheNode) error {
				nodes = append(nodes, node)

				return nil
			},
		)
		require.NoError(b, err)

		err = graph.db.View(func(tx kvdb.RTx) error {
			for _, n := range nodes {
				err := n.ForEachChannel(
					tx, func(tx kvdb.RTx,
						info *ChannelEdgeInfo,
						policy *ChannelEdgePolicy,
						policy2 *ChannelEdgePolicy) error {

						// We need to do something with
						// the data here, otherwise the
						// compiler is going to optimize
						// this away, and we get bogus
						// results.
						totalCapacity += info.Capacity
						maxHTLCs += policy.MaxHTLC
						maxHTLCs += policy2.MaxHTLC

						return nil
					},
				)
				if err != nil {
					return err
				}
			}

			return nil
		}, func() {})
		require.NoError(b, err)
	}
}

// TestGraphCacheForEachNodeChannel tests that the ForEachNodeChannel method
// works as expected, and is able to handle nil self edges.
func TestGraphCacheForEachNodeChannel(t *testing.T) {
	graph, err := MakeTestGraph(t)
	require.NoError(t, err)

	// Unset the channel graph cache to simulate the user running with the
	// option turned off.
	graph.graphCache = nil

	node1, err := createTestVertex(graph.db)
	require.Nil(t, err)
	err = graph.AddLightningNode(node1)
	require.Nil(t, err)
	node2, err := createTestVertex(graph.db)
	require.Nil(t, err)
	err = graph.AddLightningNode(node2)
	require.Nil(t, err)

	// Create an edge and add it to the db.
	edgeInfo, _, _ := createChannelEdge(graph.db, node1, node2)

	// Add the channel, but only insert a single edge into the graph.
	require.NoError(t, graph.AddChannelEdge(edgeInfo))

	// We should be able to accumulate the single channel added, even
	// though we have a nil edge policy here.
	var numChans int
	err = graph.ForEachNodeChannel(nil, node1.PubKeyBytes,
		func(channel *DirectedChannel) error {
			numChans++
			return nil
		})
	require.NoError(t, err)

	require.Equal(t, numChans, 1)
}

// TestGraphLoading asserts that the cache is properly reconstructed after a
// restart.
func TestGraphLoading(t *testing.T) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName := t.TempDir()

	// Next, create the graph for the first time.
	backend, backendCleanup, err := kvdb.GetTestBackend(tempDirName, "cgr")
	require.NoError(t, err)
	defer backend.Close()
	defer backendCleanup()

	opts := DefaultOptions()
	graph, err := NewChannelGraph(
		backend, opts.RejectCacheSize, opts.ChannelCacheSize,
		opts.BatchCommitInterval, opts.PreAllocCacheNumNodes,
		true, false,
	)
	require.NoError(t, err)

	// Populate the graph with test data.
	const numNodes = 100
	const numChannels = 4
	_, _ = fillTestGraph(t, graph, numNodes, numChannels)

	// Recreate the graph. This should cause the graph cache to be
	// populated.
	graphReloaded, err := NewChannelGraph(
		backend, opts.RejectCacheSize, opts.ChannelCacheSize,
		opts.BatchCommitInterval, opts.PreAllocCacheNumNodes,
		true, false,
	)
	require.NoError(t, err)

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
