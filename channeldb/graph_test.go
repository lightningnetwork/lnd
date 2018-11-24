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

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
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
		ExtraOpaqueData:      []byte("extra new data"),
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()
	sourceNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create source node: %v", err)
	}
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

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

	// Call DisconnectBlockAtHeight, which should prune every channel
	// that has a funding height of 'height' or greater.
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

	if !bytes.Equal(e1.ExtraOpaqueData, e2.ExtraOpaqueData) {
		t.Fatalf("extra data doesn't match: %v vs %v",
			e2.ExtraOpaqueData, e2.ExtraOpaqueData)
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
		Node:                      secondNode,
		ExtraOpaqueData:           []byte("new unknown feature2"),
		db:                        db,
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
		Node:                      firstNode,
		ExtraOpaqueData:           []byte("new unknown feature1"),
		db:                        db,
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

	return newEdgePolicy(chanID, op, db, update)
}

func newEdgePolicy(chanID uint64, op wire.OutPoint, db *DB,
	updateTime int64) *ChannelEdgePolicy {

	return &ChannelEdgePolicy{
		ChannelID:                 chanID,
		LastUpdate:                time.Unix(updateTime, 0),
		TimeLockDelta:             uint16(prand.Int63()),
		MinHTLC:                   lnwire.MilliSatoshi(prand.Int63()),
		FeeBaseMSat:               lnwire.MilliSatoshi(prand.Int63()),
		FeeProportionalMillionths: lnwire.MilliSatoshi(prand.Int63()),
		db:                        db,
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
	// again if the map is empty that indicates that all edges have
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
	err := graph.ForEachNode(nil, func(_ *bolt.Tx, _ *LightningNode) error {
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()
	sourceNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create source node: %v", err)
	}
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to get graph channel view: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to get graph channel view: %v", err)
	}
	if len(channelView) != 0 {
		t.Fatalf("channel view should be empty, instead have: %v",
			channelView)
	}
}

// TestHighestChanID tests that we're able to properly retrieve the highest
// known channel ID in the database.
func TestHighestChanID(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// If we don't yet have any channels in the database, then we should
	// get a channel ID of zero if we ask for the highest channel ID.
	bestID, err := graph.HighestChanID()
	if err != nil {
		t.Fatalf("unable to get highest ID: %v", err)
	}
	if bestID != 0 {
		t.Fatalf("best ID w/ no chan should be zero, is instead: %v",
			bestID)
	}

	// Next, we'll insert two channels into the database, with each channel
	// connecting the same two nodes.
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to get highest ID: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unable to get highest ID: %v", err)
	}

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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// If we issue an arbitrary query before any channel updates are
	// inserted in the database, we should get zero results.
	chanUpdates, err := graph.ChanUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)
	if err != nil {
		t.Fatalf("unable to updates for updates: %v", err)
	}
	if len(chanUpdates) != 0 {
		t.Fatalf("expected 0 chan updates, instead got %v",
			len(chanUpdates))
	}

	// We'll start by creating two nodes which will seed our test graph.
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

	// We'll now create 10 channels between the two nodes, with update
	// times 10 seconds after each other.
	const numChans = 10
	startTime := time.Unix(1234, 0)
	endTime := startTime
	edges := make([]ChannelEdge, 0, numChans)
	for i := 0; i < numChans; i++ {
		txHash := sha256.Sum256([]byte{byte(i)})
		op := wire.OutPoint{
			Hash:  txHash,
			Index: 0,
		}

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
			chanID.ToUint64(), op, db, edge1UpdateTime.Unix(),
		)
		edge1.Flags = 0
		edge1.Node = node2
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(
			chanID.ToUint64(), op, db, edge2UpdateTime.Unix(),
		)
		edge2.Flags = 1
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	startTime := time.Unix(1234, 0)
	endTime := startTime

	// If we issue an arbitrary query before we insert any nodes into the
	// database, then we shouldn't get any results back.
	nodeUpdates, err := graph.NodeUpdatesInHorizon(
		time.Unix(999, 0), time.Unix(9999, 0),
	)
	if err != nil {
		t.Fatalf("unable to query for node updates: %v", err)
	}
	if len(nodeUpdates) != 0 {
		t.Fatalf("expected 0 node updates, instead got %v",
			len(nodeUpdates))
	}

	// We'll create 10 node announcements, each with an update timestamp 10
	// seconds after the other.
	const numNodes = 10
	nodeAnns := make([]LightningNode, 0, numNodes)
	for i := 0; i < numNodes; i++ {
		nodeAnn, err := createTestVertex(db)
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// If we try to filter out a set of channel ID's before we even know of
	// any channels, then we should get the entire set back.
	preChanIDs := []uint64{1, 2, 3, 4}
	filteredIDs, err := graph.FilterKnownChanIDs(preChanIDs)
	if err != nil {
		t.Fatalf("unable to filter chan IDs: %v", err)
	}
	if !reflect.DeepEqual(preChanIDs, filteredIDs) {
		t.Fatalf("chan IDs shouldn't have been filtered!")
	}

	// We'll start by creating two nodes which will seed our test graph.
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

	queryCases := []struct {
		queryIDs []uint64

		resp []uint64
	}{
		// If we attempt to filter out all chanIDs we know of, the
		// response should be the empty set.
		{
			queryIDs: chanIDs,
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
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

	// If we try to filter a channel range before we have any channels
	// inserted, we should get an empty slice of results.
	resp, err := graph.FilterChannelRange(10, 100)
	if err != nil {
		t.Fatalf("unable to filter channels: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("expected zero chans, instead got %v", len(resp))
	}

	// To start, we'll create a set of channels, each mined in a block 10
	// blocks after the prior one.
	startHeight := uint32(100)
	endHeight := startHeight
	const numChans = 10
	chanIDs := make([]uint64, 0, numChans)
	for i := 0; i < numChans; i++ {
		chanHeight := endHeight
		channel, chanID := createEdge(
			uint32(chanHeight), uint32(i+1), 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		chanIDs = append(chanIDs, chanID.ToUint64())

		endHeight += 10
	}

	// With our channels inserted, we'll construct a series of queries that
	// we'll execute below in order to exercise the features of the
	// FilterKnownChanIDs method.
	queryCases := []struct {
		startHeight uint32
		endHeight   uint32

		resp []uint64
	}{
		// If we query for the entire range, then we should get the same
		// set of short channel IDs back.
		{
			startHeight: startHeight,
			endHeight:   endHeight,

			resp: chanIDs,
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

			resp: chanIDs[9:],
		},

		// If we query for just the first height, we should only get a
		// single channel back (the first one).
		{
			startHeight: startHeight,
			endHeight:   startHeight,

			resp: chanIDs[:1],
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
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

	// We'll make 5 test channels, ensuring we keep track of which channel
	// ID corresponds to a particular ChannelEdge.
	const numChans = 5
	startTime := time.Unix(1234, 0)
	endTime := startTime
	edges := make([]ChannelEdge, 0, numChans)
	edgeQuery := make([]uint64, 0, numChans)
	for i := 0; i < numChans; i++ {
		txHash := sha256.Sum256([]byte{byte(i)})
		op := wire.OutPoint{
			Hash:  txHash,
			Index: 0,
		}

		channel, chanID := createEdge(
			uint32(i*10), 0, 0, 0, node1, node2,
		)

		if err := graph.AddChannelEdge(&channel); err != nil {
			t.Fatalf("unable to create channel edge: %v", err)
		}

		updateTime := endTime
		endTime = updateTime.Add(time.Second * 10)

		edge1 := newEdgePolicy(
			chanID.ToUint64(), op, db, updateTime.Unix(),
		)
		edge1.Flags = 0
		edge1.Node = node2
		edge1.SigBytes = testSig.Serialize()
		if err := graph.UpdateEdgePolicy(edge1); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		edge2 := newEdgePolicy(
			chanID.ToUint64(), op, db, updateTime.Unix(),
		)
		edge2.Flags = 1
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

	// We'll now attempt to query for the range of channel ID's we just
	// inserted into the database. We should get the exact same set of
	// edges back.
	resp, err := graph.FetchChanInfos(edgeQuery)
	if err != nil {
		t.Fatalf("unable to fetch chan edges: %v", err)
	}
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// Create two nodes.
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

	// Create channel between nodes.
	txHash := sha256.Sum256([]byte{0})
	op := wire.OutPoint{
		Hash:  txHash,
		Index: 0,
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
		node.ForEachChannel(nil, func(_ *bolt.Tx, _ *ChannelEdgeInfo,
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

		if calls != 1 {
			t.Fatalf("Expected only one callback call")
		}
	}

	checkPolicies(node2, false, false)

	// Only create an edge policy for node1 and leave the policy for node2
	// unknown.
	updateTime := time.Unix(1234, 0)

	edgePolicy := newEdgePolicy(
		chanID.ToUint64(), op, db, updateTime.Unix(),
	)
	edgePolicy.Flags = 0
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
		chanID.ToUint64(), op, db, updateTime.Unix(),
	)
	edgePolicy.Flags = 1
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

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()
	sourceNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create source node: %v", err)
	}
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// We'll first populate our graph with two nodes. All channels created
	// below will be made between these two nodes.
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

	// With the two nodes created, we'll now create a random channel, as
	// well as two edges in the database with distinct update times.
	edgeInfo, chanID := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge1 := randEdgePolicy(chanID.ToUint64(), edgeInfo.ChannelPoint, db)
	edge1.Flags = 0
	edge1.Node = node1
	edge1.SigBytes = testSig.Serialize()
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	edge2 := randEdgePolicy(chanID.ToUint64(), edgeInfo.ChannelPoint, db)
	edge2.Flags = 1
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

		err := db.View(func(tx *bolt.Tx) error {
			edges := tx.Bucket(edgeBucket)
			if edges == nil {
				return ErrGraphNoEdgesFound
			}
			edgeUpdateIndex := edges.Bucket(edgeUpdateIndexBucket)
			if edgeUpdateIndex == nil {
				return ErrGraphNoEdgesFound
			}

			numEntries := edgeUpdateIndex.Stats().KeyN
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
		})
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
	edge1.Flags = 2
	edge1.LastUpdate = time.Now()
	if err := graph.UpdateEdgePolicy(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	edge2.Flags = 3
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
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// Finally, we'll check the database state one last time to conclude
	// that we should no longer be able to locate _any_ entries within the
	// edge update index.
	checkIndexTimestamps()
}

// TestPruneGraphNodes tests that unconnected vertexes are pruned via the
// PruneSyncState method.
func TestPruneGraphNodes(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	// We'll start off by inserting our source node, to ensure that it's
	// the only node left after we prune the graph.
	graph := db.ChannelGraph()
	sourceNode, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create source node: %v", err)
	}
	if err := graph.SetSourceNode(sourceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// With the source node inserted, we'll now add three nodes to the
	// channel graph, at the end of the scenario, only two of these nodes
	// should still be in the graph.
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
	node3, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
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
	edge1 := randEdgePolicy(chanID.ToUint64(), edgeInfo.ChannelPoint, db)
	edge1.Flags = 0
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
	node3Pub, err := node3.PubKey()
	if err != nil {
		t.Fatalf("unable to fetch the pubkey of node3: %v", err)
	}
	if _, err := graph.FetchLightningNode(node3Pub); err == nil {
		t.Fatalf("node 3 should have been deleted!")
	}
}

// TestAddChannelEdgeShellNodes tests that when we attempt to add a ChannelEdge
// to the graph, one or both of the nodes the edge involves aren't found in the
// database, then shell edges are created for each node if needed.
func TestAddChannelEdgeShellNodes(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// To start, we'll create two nodes, and only add one of them to the
	// channel graph.
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

	// We'll now create an edge between the two nodes, as a result, node2
	// should be inserted into the database as a shell node.
	edgeInfo, _ := createEdge(100, 0, 0, 0, node1, node2)
	if err := graph.AddChannelEdge(&edgeInfo); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	node1Pub, err := node1.PubKey()
	if err != nil {
		t.Fatalf("unable to parse node 1 pub: %v", err)
	}
	node2Pub, err := node2.PubKey()
	if err != nil {
		t.Fatalf("unable to parse node 2 pub: %v", err)
	}

	// Ensure that node1 was inserted as a full node, while node2 only has
	// a shell node present.
	node1, err = graph.FetchLightningNode(node1Pub)
	if err != nil {
		t.Fatalf("unable to fetch node1: %v", err)
	}
	if !node1.HaveNodeAnnouncement {
		t.Fatalf("have shell announcement for node1, shouldn't")
	}

	node2, err = graph.FetchLightningNode(node2Pub)
	if err != nil {
		t.Fatalf("unable to fetch node2: %v", err)
	}
	if node2.HaveNodeAnnouncement {
		t.Fatalf("should have shell announcement for node2, but is full")
	}
}

// TestNodePruningUpdateIndexDeletion tests that once a node has been removed
// from the channel graph, we also remove the entry from the update index as
// well.
func TestNodePruningUpdateIndexDeletion(t *testing.T) {
	t.Parallel()

	db, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}

	graph := db.ChannelGraph()

	// We'll first populate our graph with a single node that will be
	// removed shortly.
	node1, err := createTestVertex(db)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	if err := graph.AddLightningNode(node1); err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// We'll confirm that we can retrieve the node using
	// NodeUpdatesInHorizon, using a time that's slightly beyond the last
	// update time of our test node.
	startTime := time.Unix(9, 0)
	endTime := node1.LastUpdate.Add(time.Minute)
	nodesInHorizon, err := graph.NodeUpdatesInHorizon(startTime, endTime)
	if err != nil {
		t.Fatalf("unable to fetch nodes in horizon: %v", err)
	}

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
	nodePub, _ := node1.PubKey()
	if err := graph.DeleteLightningNode(nodePub); err != nil {
		t.Fatalf("unable to delete node: %v", err)
	}

	// Now that the node has been deleted, we'll again query the nodes in
	// the horizon. This time we should have no nodes at all.
	nodesInHorizon, err = graph.NodeUpdatesInHorizon(startTime, endTime)
	if err != nil {
		t.Fatalf("unable to fetch nodes in horizon: %v", err)
	}

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
	aliceDB, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	aliceNode, err := createTestVertex(aliceDB)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	aliceGraph := aliceDB.ChannelGraph()
	if err := aliceGraph.SetSourceNode(aliceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	bobDB, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	bobNode, err := createTestVertex(bobDB)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	bobGraph := bobDB.ChannelGraph()
	if err := bobGraph.SetSourceNode(bobNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	carolDB, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	carolNode, err := createTestVertex(carolDB)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	carolGraph := carolDB.ChannelGraph()
	if err := carolGraph.SetSourceNode(carolNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	aliceBobEdge, _ := createEdge(10, 0, 0, 0, aliceNode, bobNode)
	bobCarolEdge, _ := createEdge(10, 1, 0, 1, bobNode, carolNode)

	// After creating all of our nodes and edges, we'll add them to each
	// participant's graph.
	nodes := []*LightningNode{aliceNode, bobNode, carolNode}
	edges := []*ChannelEdgeInfo{&aliceBobEdge, &bobCarolEdge}
	dbs := []*DB{aliceDB, bobDB, carolDB}
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
		err := graph.DeleteChannelEdge(&aliceBobEdge.ChannelPoint)
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
		err := graph.DeleteChannelEdge(&bobCarolEdge.ChannelPoint)
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
