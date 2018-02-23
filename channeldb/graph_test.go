package channeldb

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image/color"
	"math"
	"math/big"
	prand "math/rand"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr, anotherAddr}

	randSource = prand.NewSource(time.Now().Unix())
	randInts   = prand.New(randSource)
	testSig    = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	testFeatures = lnwire.NewFeatureVector(nil, lnwire.GlobalFeatures)
)

func createTestVertex(db *DB) (*LightningNode, error) {
	updateTime := prand.Int63()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

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

func TestNodeInsertionAndDeletion(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test basic insertion/deletion for vertexes from the
	// graph, so we'll create a test vertex to start with.
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	node := &LightningNode{
		HaveNodeAnnouncement: true,
		AuthSigBytes:         testSig.Serialize(),
		LastUpdate:           time.Unix(1232342, 0),
		Color:                color.RGBA{1, 2, 3, 0},
		Alias:                "kek",
		Features:             testFeatures,
		Addresses:            testAddrs,
		db:                   db,
	}
	copy(node.PubKeyBytes[:], testPub.SerializeCompressed())

	// First, insert the node into the graph DB. This should succeed
	// without any errors.
	if err := graph.AddLightningNode(node); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, fetch the node from the database to ensure everything was
	// serialized properly.
	dbNode, err := graph.FetchLightningNode(testPub)
	if err != nil {
		t.Fatalf("unable to locate node: %v", err)
	}

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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We want to be able to insert nodes into the graph that only has the
	// PubKey set.
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	node := &LightningNode{
		HaveNodeAnnouncement: false,
	}
	copy(node.PubKeyBytes[:], testPub.SerializeCompressed())

	if err := graph.AddLightningNode(node); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, fetch the node from the database to ensure everything was
	// serialized properly.
	dbNode, err := graph.FetchLightningNode(testPub)
	if err != nil {
		t.Fatalf("unable to locate node: %v", err)
	}

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
		db:                   db,
	}
	copy(node.PubKeyBytes[:], testPub.SerializeCompressed())

	if err := compareNodes(node, dbNode); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	if err := graph.DeleteLightningNode(testPub); err != nil {
		t.Fatalf("unable to delete node: %v", err)
	}

	// Finally, attempt to fetch the node again. This should fail as the
	// node should have been deleted from the database.
	_, err = graph.FetchLightningNode(testPub)
	if err != ErrGraphNodeNotFound {
		t.Fatalf("fetch after delete should fail!")
	}
}

func TestAliasLookup(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test the alias index within the database, so first
	// create a new test node.
	testNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// Add the node to the graph's database, this should also insert an
	// entry into the alias index for this node.
	if err := graph.AddLightningNode(testNode); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// Next, attempt to lookup the alias. The alias should exactly match
	// the one which the test node was assigned.
	nodePub, err := testNode.PubKey()
	if err != nil {
		t.Fatalf("unable to generate pubkey: %v", err)
	}
	dbAlias, err := graph.LookupAlias(nodePub)
	if err != nil {
		t.Fatalf("unable to find alias: %v", err)
	}
	if dbAlias != testNode.Alias {
		t.Fatalf("aliases don't match, expected %v got %v",
			testNode.Alias, dbAlias)
	}

	// Ensure that looking up a non-existent alias results in an error.
	node, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	nodePub, err = node.PubKey()
	if err != nil {
		t.Fatalf("unable to generate pubkey: %v", err)
	}
	_, err = graph.LookupAlias(nodePub)
	if err != ErrNodeAliasNotFound {
		t.Fatalf("alias lookup should fail for non-existent pubkey")
	}
}

func TestSourceNode(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test the setting/getting of the source node, so we
	// first create a fake node to use within the test.
	testNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	if err := compareNodes(testNode, sourceNode); err != nil {
		t.Fatalf("nodes don't match: %v", err)
	}
}

func TestEdgeInsertionDeletion(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// In in addition to the fake vertexes we create some fake channel
	// identifiers.
	chanID := uint64(prand.Int63())
	outpoint := wire.OutPoint{
		Hash:  rev,
		Index: 9,
	}

	// Add the new edge to the database, this should proceed without any
	// errors.
	node1Pub, err := node1.PubKey()
	if err != nil {
		t.Fatalf("unable to generate node key: %v", err)
	}
	node2Pub, err := node2.PubKey()
	if err != nil {
		t.Fatalf("unable to generate node key: %v", err)
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

	// Next, attempt to delete the edge from the database, again this
	// should proceed without any issues.
	if err := graph.DeleteChannelEdge(&outpoint); err != nil {
		t.Fatalf("unable to delete edge: %v", err)
	}

	// Ensure that any query attempts to lookup the delete channel edge are
	// properly deleted.
	if _, _, _, err := graph.FetchChannelEdgesByOutpoint(&outpoint); err == nil {
		t.Fatalf("channel edge not deleted")
	}
	if _, _, _, err := graph.FetchChannelEdgesByID(chanID); err == nil {
		t.Fatalf("channel edge not deleted")
	}

	// Finally, attempt to delete a (now) non-existent edge within the
	// database, this should result in an error.
	err = graph.DeleteChannelEdge(&outpoint)
	if err != ErrEdgeNotFound {
		t.Fatalf("deleting a non-existent edge should fail!")
	}
}

// TestDisconnectBlockAtHeight checks that the pruned state of the channel
// database is what we expect after calling DisconnectBlockAtHeight.
func TestDisconnectBlockAtHeight(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test the insertion/deletion of edges, so we create two
	// vertexes to connect.
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// In addition to the fake vertexes we create some fake channel
	// identifiers.
	var spendOutputs []*wire.OutPoint
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))

	// Prune the graph a few times to make sure we have entries in the
	// prune log.
	_, err = graph.PruneGraph(spendOutputs, &blockHash, 155)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}
	var blockHash2 chainhash.Hash
	copy(blockHash2[:], bytes.Repeat([]byte{2}, 32))

	_, err = graph.PruneGraph(spendOutputs, &blockHash2, 156)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// We'll create 3 almost identical edges, so first create a helper
	// method containing all logic for doing so.
	createEdge := func(height uint32, txIndex uint32, txPosition uint16,
		outPointIndex uint32) ChannelEdgeInfo {
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

		return edgeInfo
	}

	// Create an edge which has its block height at 156.
	height := uint32(156)
	edgeInfo := createEdge(height, 0, 0, 0)

	// Create an edge with block height 157. We give it
	// maximum values for tx index and position, to make
	// sure our database range scan get edges from the
	// entire range.
	edgeInfo2 := createEdge(height+1, math.MaxUint32&0x00ffffff,
		math.MaxUint16, 1)

	// Create a third edge, this with a block height of 155.
	edgeInfo3 := createEdge(height-1, 0, 0, 2)

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

	// Call DisconnectBlockAtHeight, which should prune every channel
	// that has an funding height of 'height' or greater.
	removed, err := graph.DisconnectBlockAtHeight(uint32(height))
	if err != nil {
		t.Fatalf("unable to prune %v", err)
	}

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
	_, _, has, err := graph.HasChannelEdge(edgeInfo.ChannelID)
	if err != nil {
		t.Fatalf("unable to query for edge: %v", err)
	}
	if has {
		t.Fatalf("edge1 was not pruned from the graph")
	}
	_, _, has, err = graph.HasChannelEdge(edgeInfo2.ChannelID)
	if err != nil {
		t.Fatalf("unable to query for edge: %v", err)
	}
	if has {
		t.Fatalf("edge2 was not pruned from the graph")
	}

	// Edge 3 should not be removed.
	_, _, has, err = graph.HasChannelEdge(edgeInfo3.ChannelID)
	if err != nil {
		t.Fatalf("unable to query for edge: %v", err)
	}
	if !has {
		t.Fatalf("edge3 was pruned from the graph")
	}

	// PruneTip should be set to the blockHash we specified for the block
	// at height 155.
	hash, h, err := graph.PruneTip()
	if err != nil {
		t.Fatalf("unable to get prune tip: %v", err)
	}
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
}

func TestEdgeInfoUpdates(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test the update of edges inserted into the database, so
	// we create two vertexes to connect.
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}
	node2, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	if err := graph.AddLightningNode(node2); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

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

	// In in addition to the fake vertexes we create some fake channel
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
		ChannelPoint: outpoint,
		Capacity:     1000,
	}
	copy(edgeInfo.NodeKey1Bytes[:], firstNode.PubKeyBytes[:])
	copy(edgeInfo.NodeKey2Bytes[:], secondNode.PubKeyBytes[:])
	copy(edgeInfo.BitcoinKey1Bytes[:], firstNode.PubKeyBytes[:])
	copy(edgeInfo.BitcoinKey2Bytes[:], secondNode.PubKeyBytes[:])
	if err := graph.AddChannelEdge(edgeInfo); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// With the edge added, we can now create some fake edge information to
	// update for both edges.
	edge1 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(433453, 0),
		Flags:                     0,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 3452352,
		Node: secondNode,
		db:   db,
	}
	edge2 := &ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(124234, 0),
		Flags:                     1,
		TimeLockDelta:             99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 90392423,
		Node: firstNode,
		db:   db,
	}

	// Next, insert both nodes into the database, they should both be
	// inserted without any issues.
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	if err := graph.UpdateEdgePolicy(edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// Check for existence of the edge within the database, it should be
	// found.
	_, _, found, err := graph.HasChannelEdge(chanID)
	if err != nil {
		t.Fatalf("unable to query for edge: %v", err)
	} else if !found {
		t.Fatalf("graph should have of inserted edge")
	}

	// We should also be able to retrieve the channelID only knowing the
	// channel point of the channel.
	dbChanID, err := graph.ChannelID(&outpoint)
	if err != nil {
		t.Fatalf("unable to retrieve channel ID: %v", err)
	}
	if dbChanID != chanID {
		t.Fatalf("chan ID's mismatch, expected %v got %v", dbChanID,
			chanID)
	}

	// With the edges inserted, perform some queries to ensure that they've
	// been inserted properly.
	dbEdgeInfo, dbEdge1, dbEdge2, err := graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch channel by ID: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to fetch channel by ID: %v", err)
	}
	if err := compareEdgePolicies(dbEdge1, edge1); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	if err := compareEdgePolicies(dbEdge2, edge2); err != nil {
		t.Fatalf("edge doesn't match: %v", err)
	}
	assertEdgeInfoEqual(t, dbEdgeInfo, edgeInfo)
}

func randEdgePolicy(chanID uint64, op wire.OutPoint, db *DB) *ChannelEdgePolicy {
	update := prand.Int63()

	return &ChannelEdgePolicy{
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(update, 0),
		TimeLockDelta:             uint16(prand.Int63()),
		MinHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		FeeBaseMSat:               lnwire.MilliSatoshi(prand.Int63()),
		FeeProportionalMillionths: lnwire.MilliSatoshi(prand.Int63()),
		db: db,
	}
}

func TestGraphTraversal(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'd like to test some of the graph traversal capabilities within
	// the DB, so we'll create a series of fake nodes to insert into the
	// graph.
	const numNodes = 20
	nodes := make([]*LightningNode, numNodes)
	nodeIndex := map[string]struct{}{}
	for i := 0; i < numNodes; i++ {
		node, err := createTestVertex(db)
		if err != nil {
			t.Fatalf("unable to create node: %v", err)
		}

		nodes[i] = node
		nodeIndex[node.Alias] = struct{}{}
	}

	// Add each of the nodes into the graph, they should be inserted
	// without error.
	for _, node := range nodes {
		if err := graph.AddLightningNode(node); err != nil {
			t.Fatalf("unable to add node: %v", err)
		}
	}

	// Iterate over each node as returned by the graph, if all nodes are
	// reached, then the map created above should be empty.
	err = graph.ForEachNode(nil, func(_ *bolt.Tx, node *LightningNode) error {
		delete(nodeIndex, node.Alias)
		return nil
	})
	if err != nil {
		t.Fatalf("for each failure: %v", err)
	}
	if len(nodeIndex) != 0 {
		t.Fatalf("all nodes not reached within ForEach")
	}

	// Determine which node is "smaller", we'll need this in order to
	// properly create the edges for the graph.
	var firstNode, secondNode *LightningNode
	if bytes.Compare(nodes[0].PubKeyBytes[:], nodes[1].PubKeyBytes[:]) == -1 {
		firstNode = nodes[0]
		secondNode = nodes[1]
	} else {
		firstNode = nodes[0]
		secondNode = nodes[1]
	}

	// Create 5 channels between the first two nodes we generated above.
	const numChannels = 5
	chanIndex := map[uint64]struct{}{}
	for i := 0; i < numChannels; i++ {
		txHash := sha256.Sum256([]byte{byte(i)})
		chanID := uint64(i + 1)
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
		copy(edgeInfo.NodeKey1Bytes[:], nodes[0].PubKeyBytes[:])
		copy(edgeInfo.NodeKey2Bytes[:], nodes[1].PubKeyBytes[:])
		copy(edgeInfo.BitcoinKey1Bytes[:], nodes[0].PubKeyBytes[:])
		copy(edgeInfo.BitcoinKey2Bytes[:], nodes[1].PubKeyBytes[:])
		err := graph.AddChannelEdge(&edgeInfo)
		if err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		// Create and add an edge with random data that points from
		// node1 -> node2.
		edge := randEdgePolicy(chanID, op, db)
		edge.Flags = 0
		edge.Node = secondNode
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node2 -> node1
		// this time.
		edge = randEdgePolicy(chanID, op, db)
		edge.Flags = 1
		edge.Node = firstNode
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		chanIndex[chanID] = struct{}{}
	}

	// Iterate through all the known channels within the graph DB, once
	// again if the map is empty that that indicates that all edges have
	// properly been reached.
	err = graph.ForEachChannel(func(ei *ChannelEdgeInfo, _ *ChannelEdgePolicy,
		_ *ChannelEdgePolicy) error {

		delete(chanIndex, ei.ChannelID)
		return nil
	})
	if err != nil {
		t.Fatalf("for each failure: %v", err)
	}
	if len(chanIndex) != 0 {
		t.Fatalf("all edges not reached within ForEach")
	}

	// Finally, we want to test the ability to iterate over all the
	// outgoing channels for a particular node.
	numNodeChans := 0
	err = firstNode.ForEachChannel(nil, func(_ *bolt.Tx, _ *ChannelEdgeInfo,
		outEdge, inEdge *ChannelEdgePolicy) error {

		// Each each should indicate that it's outgoing (pointed
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
	if err != nil {
		t.Fatalf("for each failure: %v", err)
	}
	if numNodeChans != numChannels {
		t.Fatalf("all edges for node not reached within ForEach: "+
			"expected %v, got %v", numChannels, numNodeChans)
	}
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
		t.Fatalf("line %v:unable to scan channels: %v", line, err)
	}
	if numChans != n {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: expected %v chans instead have %v", line,
			n, numChans)
	}
}

func assertChanViewEqual(t *testing.T, a []wire.OutPoint, b []*wire.OutPoint) {
	if len(a) != len(b) {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: chan views don't match", line)
	}

	chanViewSet := make(map[wire.OutPoint]struct{})
	for _, op := range a {
		chanViewSet[op] = struct{}{}
	}

	for _, op := range b {
		if _, ok := chanViewSet[*op]; !ok {
			_, _, line, _ := runtime.Caller(1)
			t.Fatalf("line %v: chanPoint(%v) not found in first view",
				line, op)
		}
	}
}

func TestGraphPruning(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// As initial set up for the test, we'll create a graph with 5 vertexes
	// and enough edges to create a fully connected graph. The graph will
	// be rather simple, representing a straight line.
	const numNodes = 5
	graphNodes := make([]*LightningNode, numNodes)
	for i := 0; i < numNodes; i++ {
		node, err := createTestVertex(db)
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

		// Create and add an edge with random data that points from
		// node_i -> node_i+1
		edge := randEdgePolicy(chanID, op, db)
		edge.Flags = 0
		edge.Node = graphNodes[i]
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node_i+1 ->
		// node_i this time.
		edge = randEdgePolicy(chanID, op, db)
		edge.Flags = 1
		edge.Node = graphNodes[i]
		edge.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}
	}

	// With all the channel points added, we'll consult the graph to ensure
	// it has the same channel view as the one we just constructed.
	channelView, err := graph.ChannelView()
	if err != nil {
		t.Fatalf("unable to get graph channel view: %v", err)
	}
	assertChanViewEqual(t, channelView, channelPoints)

	// Now with our test graph created, we can test the pruning
	// capabilities of the channel graph.

	// First we create a mock block that ends up closing the first two
	// channels.
	var blockHash chainhash.Hash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))
	blockHeight := uint32(1)
	block := channelPoints[:2]
	prunedChans, err := graph.PruneGraph(block, &blockHash, blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}
	if len(prunedChans) != 2 {
		t.Fatalf("incorrect number of channels pruned: expected %v, got %v",
			2, prunedChans)
	}

	// Now ensure that the prune tip has been updated.
	assertPruneTip(t, graph, &blockHash, blockHeight)

	// Count up the number of channels known within the graph, only 2
	// should be remaining.
	assertNumChans(t, graph, 2)

	// Those channels should also be missing from the channel view.
	channelView, err = graph.ChannelView()
	if err != nil {
		t.Fatalf("unable to get graph channel view: %v", err)
	}
	assertChanViewEqual(t, channelView, channelPoints[2:])

	// Next we'll create a block that doesn't close any channels within the
	// graph to test the negative error case.
	fakeHash := sha256.Sum256([]byte("test prune"))
	nonChannel := &wire.OutPoint{
		Hash:  fakeHash,
		Index: 9,
	}
	blockHash = sha256.Sum256(blockHash[:])
	blockHeight = 2
	prunedChans, err = graph.PruneGraph([]*wire.OutPoint{nonChannel},
		&blockHash, blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// No channels should have been detected as pruned.
	if len(prunedChans) != 0 {
		t.Fatalf("channels were pruned but shouldn't have been")
	}

	// Once again, the prune tip should have been updated.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	assertNumChans(t, graph, 2)

	// Finally, create a block that prunes the remainder of the channels
	// from the graph.
	blockHash = sha256.Sum256(blockHash[:])
	blockHeight = 3
	prunedChans, err = graph.PruneGraph(channelPoints[2:], &blockHash,
		blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// The remainder of the channels should have been pruned from the graph.
	if len(prunedChans) != 2 {
		t.Fatalf("incorrect number of channels pruned: expected %v, got %v",
			2, len(prunedChans))
	}

	// The prune tip should be updated, and no channels should be found
	// within the current graph.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	assertNumChans(t, graph, 0)

	// Finally, the channel view at this point in the graph should now be
	// completely empty.
	// Those channels should also be missing from the channel view.
	channelView, err = graph.ChannelView()
	if err != nil {
		t.Fatalf("unable to get graph channel view: %v", err)
	}
	if len(channelView) != 0 {
		t.Fatalf("channel view should be empty, instead have: %v",
			channelView)
	}
}

// compareNodes is used to compare two LightningNodes while excluding the
// Features struct, which cannot be compared as the semantics for reserializing
// the featuresMap have not been defined.
func compareNodes(a, b *LightningNode) error {
	if !reflect.DeepEqual(a.LastUpdate, b.LastUpdate) {
		return fmt.Errorf("LastUpdate doesn't match: expected %#v, \n"+
			"got %#v", a.LastUpdate, b.LastUpdate)
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
		return fmt.Errorf("LastUpdate doesn't match: expected %#v, \n "+
			"got %#v", a.LastUpdate, b.LastUpdate)
	}
	if a.Flags != b.Flags {
		return fmt.Errorf("Flags doesn't match: expected %v, "+
			"got %v", a.Flags, b.Flags)
	}
	if a.TimeLockDelta != b.TimeLockDelta {
		return fmt.Errorf("TimeLockDelta doesn't match: expected %v, "+
			"got %v", a.TimeLockDelta, b.TimeLockDelta)
	}
	if a.MinHTLC != b.MinHTLC {
		return fmt.Errorf("MinHTLC doesn't match: expected %v, "+
			"got %v", a.MinHTLC, b.MinHTLC)
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
	if err := compareNodes(a.Node, b.Node); err != nil {
		return err
	}
	if !reflect.DeepEqual(a.db, b.db) {
		return fmt.Errorf("db doesn't match: expected %#v, \n "+
			"got %#v", a.db, b.db)
	}
	return nil
}
