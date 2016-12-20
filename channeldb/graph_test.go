package channeldb

import (
	"bytes"
	"fmt"
	"image/color"
	prand "math/rand"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	testAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.1:9000")
)

func createTestVertex(db *DB) (*LightningNode, error) {
	updateTime := prand.Int63()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	pub := priv.PubKey().SerializeCompressed()
	return &LightningNode{
		LastUpdate: time.Unix(updateTime, 0),
		Address:    testAddr,
		PubKey:     priv.PubKey(),
		Color:      color.RGBA{1, 2, 3, 0},
		Alias:      "kek" + string(pub[:]),
		db:         db,
	}, nil
}

func TestNodeInsertionAndDeletion(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	graph := db.ChannelGraph()

	// We'd like to test basic insertion/deletion for vertexes from the
	// graph, so we'll create a test vertex to start with.
	_, testPub := btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	node := &LightningNode{
		LastUpdate: time.Unix(1232342, 0),
		Address:    testAddr,
		PubKey:     testPub,
		Color:      color.RGBA{1, 2, 3, 0},
		Alias:      "kek",
		db:         db,
	}

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

	// The two nodes should match exactly!
	if !reflect.DeepEqual(node, dbNode) {
		t.Fatalf("retrieved node doesn't match: expected %#v\n, got %#v\n",
			node, dbNode)
	}

	// Next, delete the node from the graph, this should purge all data
	// related to the node.
	if err := graph.DeleteLightningNode(testPub); err != nil {
		t.Fatalf("unable to delete node; %v", err)
	}

	// Finally, attempt to fetch the node again. This should fail as the
	// node should've been deleted from the database.
	_, err = graph.FetchLightningNode(testPub)
	if err != ErrGraphNodeNotFound {
		t.Fatalf("fetch after delete should fail!")
	}
}

func TestAliasLookup(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
	dbAlias, err := graph.LookupAlias(testNode.PubKey)
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
	_, err = graph.LookupAlias(node.PubKey)
	if err != ErrNodeAliasNotFound {
		t.Fatalf("alias lookup should fail for non-existent pubkey")
	}
}

func TestSourceNode(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
	if !reflect.DeepEqual(testNode, sourceNode) {
		t.Fatalf("nodes don't match, expected %#v \n got %#v",
			testNode, sourceNode)
	}
}

func TestEdgeInsertionDeletion(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
	if err := graph.AddChannelEdge(node1.PubKey, node2.PubKey, &outpoint,
		chanID); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// Check for existence of the edge within the database, it should be
	// found.
	found, err := graph.HasChannelEdge(chanID)
	if err != nil {
		t.Fatalf("unable to query for edge: %v", err)
	} else if !found {
		t.Fatalf("graph should have of inserted edge")
	}

	// Next, attempt to delete the edge from the database, again this
	// should proceed without any issues.
	if err := graph.DeleteChannelEdge(&outpoint); err != nil {
		t.Fatalf("unable to delete edge: %v", err)
	}

	// Finally, attempt to delete a (now) non-existent edge within the
	// database, this should result in an error.
	err = graph.DeleteChannelEdge(&outpoint)
	if err != ErrEdgeNotFound {
		t.Fatalf("deleting a non-existent edge should fail!")
	}
}

func TestEdgeInfoUpdates(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
	node1Bytes := node1.PubKey.SerializeCompressed()
	node2Bytes := node2.PubKey.SerializeCompressed()
	if bytes.Compare(node1Bytes, node2Bytes) == -1 {
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
	if err := graph.AddChannelEdge(node1.PubKey, node2.PubKey, &outpoint,
		chanID); err != nil {
		t.Fatalf("unable to create channel edge: %v", err)
	}

	// With the edge added, we can now create some fake edge information to
	// update for both edges.
	edge1 := &ChannelEdge{
		ChannelID:                 chanID,
		ChannelPoint:              outpoint,
		LastUpdate:                time.Unix(433453, 0),
		Flags:                     0,
		Expiry:                    99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 3452352,
		Capacity:                  9903453,
		Node:                      secondNode,
		db:                        db,
	}
	edge2 := &ChannelEdge{
		ChannelID:                 chanID,
		ChannelPoint:              outpoint,
		LastUpdate:                time.Unix(124234, 0),
		Flags:                     1,
		Expiry:                    99,
		MinHTLC:                   2342135,
		FeeBaseMSat:               4352345,
		FeeProportionalMillionths: 90392423,
		Capacity:                  324523,
		Node:                      firstNode,
		db:                        db,
	}

	// Next, insert both nodes into the database, they should both be
	// inserted without any issues.
	if err := graph.UpdateEdgeInfo(edge1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	if err := graph.UpdateEdgeInfo(edge2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// With the edges inserted, perform some queries to ensure that they've
	// been inserted properly.
	dbEdge1, dbEdge2, err := graph.FetchChannelEdgesByID(chanID)
	if err != nil {
		t.Fatalf("unable to fetch channel by ID: %v", err)
	}
	if !reflect.DeepEqual(dbEdge1, edge1) {
		t.Fatalf("edge doesn't match: expected %#v, \n got %#v", edge1,
			dbEdge1)
	}
	if !reflect.DeepEqual(dbEdge2, edge2) {
		t.Fatalf("edge doesn't match: expected %#v, \n got %#v", edge2,
			dbEdge2)
	}

	// Next, attempt to query the channel edges according to the outpoint
	// of the channel.
	dbEdge1, dbEdge2, err = graph.FetchChannelEdgesByOutpoint(&outpoint)
	if err != nil {
		t.Fatalf("unable to fetch channel by ID: %v", err)
	}
	if !reflect.DeepEqual(dbEdge1, edge1) {
		t.Fatalf("edge doesn't match: expected %#v, \n got %#v", edge1,
			dbEdge1)
	}
	if !reflect.DeepEqual(dbEdge2, edge2) {
		t.Fatalf("edge doesn't match: expected %#v, \n got %#v", edge2,
			dbEdge2)
	}
}

func randEdge(chanID uint64, op wire.OutPoint, db *DB) *ChannelEdge {
	update := prand.Int63()

	return &ChannelEdge{
		ChannelID:                 chanID,
		ChannelPoint:              op,
		LastUpdate:                time.Unix(update, 0),
		Expiry:                    uint16(prand.Int63()),
		MinHTLC:                   btcutil.Amount(prand.Int63()),
		FeeBaseMSat:               btcutil.Amount(prand.Int63()),
		FeeProportionalMillionths: btcutil.Amount(prand.Int63()),
		Capacity:                  btcutil.Amount(prand.Int63()),
		db:                        db,
	}
}

func TestGraphTraversal(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
	err = graph.ForEachNode(func(node *LightningNode) error {
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
	node1Bytes := nodes[0].PubKey.SerializeCompressed()
	node2Bytes := nodes[1].PubKey.SerializeCompressed()
	if bytes.Compare(node1Bytes, node2Bytes) == -1 {
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
		txHash := fastsha256.Sum256([]byte{byte(i)})
		chanID := uint64(i + 1)
		op := wire.OutPoint{
			Hash:  txHash,
			Index: 0,
		}

		err := graph.AddChannelEdge(nodes[0].PubKey, nodes[1].PubKey,
			&op, chanID)
		if err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		// Create and add an edge with random data that points from
		// node1 -> node2.
		edge := randEdge(chanID, op, db)
		edge.Flags = 0
		edge.Node = secondNode
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node2 -> node1
		// this time.
		edge = randEdge(chanID, op, db)
		edge.Flags = 1
		edge.Node = firstNode
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		chanIndex[chanID] = struct{}{}
	}

	// Iterate through all the known channels within the graph DB, once
	// again if the map is empty that that indicates that all edges have
	// properly been reached.
	err = graph.ForEachChannel(func(_, e *ChannelEdge) error {
		delete(chanIndex, e.ChannelID)
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
	err = firstNode.ForEachChannel(nil, func(c *ChannelEdge) error {
		// Each each should indicate that it's outgoing (pointed
		// towards the second node).
		if !c.Node.PubKey.IsEqual(secondNode.PubKey) {
			return fmt.Errorf("wrong outgoing edge")
		}
		numNodeChans += 1
		return nil
	})
	if err != nil {
		t.Fatalf("for each failure: %v", err)
	}
	if numNodeChans != numChannels {
		t.Fatalf("all edges for node reached within ForEach")
	}
}

func assertPruneTip(t *testing.T, graph *ChannelGraph, blockHash *wire.ShaHash,
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

func asserNumChans(t *testing.T, graph *ChannelGraph, n int) {
	numChans := 0
	if err := graph.ForEachChannel(func(*ChannelEdge, *ChannelEdge) error {
		numChans += 1
		return nil
	}); err != nil {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v:unable to scan channels: %v", line, err)
	}
	if numChans != n {
		_, _, line, _ := runtime.Caller(1)
		t.Fatalf("line %v: expected %v chans instead have %v", line, n, numChans)
	}
}

func TestGraphPruning(t *testing.T) {
	db, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

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
		txHash := fastsha256.Sum256([]byte{byte(i)})
		chanID := uint64(i + 1)
		op := wire.OutPoint{
			Hash:  txHash,
			Index: 0,
		}

		channelPoints = append(channelPoints, &op)

		err := graph.AddChannelEdge(graphNodes[i].PubKey,
			graphNodes[i+1].PubKey, &op, chanID)
		if err != nil {
			t.Fatalf("unable to add node: %v", err)
		}

		// Create and add an edge with random data that points from
		// node_i -> node_i+1
		edge := randEdge(chanID, op, db)
		edge.Flags = 0
		edge.Node = graphNodes[i]
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}

		// Create another random edge that points from node_i+1 ->
		// node_i this time.
		edge = randEdge(chanID, op, db)
		edge.Flags = 1
		edge.Node = graphNodes[i]
		if err := graph.UpdateEdgeInfo(edge); err != nil {
			t.Fatalf("unable to update edge: %v", err)
		}
	}

	// Now with our test graph created, we can test the pruning
	// capabilities of the channel graph.

	// First we create a mock block that ends up closing the first two
	// channels.
	var blockHash wire.ShaHash
	copy(blockHash[:], bytes.Repeat([]byte{1}, 32))
	blockHeight := uint32(1)
	block := channelPoints[:2]
	numPruned, err := graph.PruneGraph(block, &blockHash, blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}
	if numPruned != 2 {
		t.Fatalf("incorrect number of channels pruned: expected %v, got %v",
			2, numPruned)
	}

	// Now ensure that the prune tip has been updated.
	assertPruneTip(t, graph, &blockHash, blockHeight)

	// Count up the number of channels known within the graph, only 2
	// should be remaining.
	asserNumChans(t, graph, 2)

	// Next we'll create a block that doesn't close any channels within the
	// graph to test the negative error case.
	fakeHash := fastsha256.Sum256([]byte("test prune"))
	nonChannel := &wire.OutPoint{
		Hash:  fakeHash,
		Index: 9,
	}
	blockHash = fastsha256.Sum256(blockHash[:])
	blockHeight = 2
	numPruned, err = graph.PruneGraph([]*wire.OutPoint{nonChannel},
		&blockHash, blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// No channels should've been detected as pruned.
	if numPruned != 0 {
		t.Fatalf("channels were pruned but shouldn't have been")
	}

	// Once again, the prune tip should've been updated.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	asserNumChans(t, graph, 2)

	// Finally, create a block that prunes the remainder of the channels
	// from the graph.
	blockHash = fastsha256.Sum256(blockHash[:])
	blockHeight = 3
	numPruned, err = graph.PruneGraph(channelPoints[2:], &blockHash,
		blockHeight)
	if err != nil {
		t.Fatalf("unable to prune graph: %v", err)
	}

	// The remainder of the channels should've been pruned from the graph.
	if numPruned != 2 {
		t.Fatalf("incorrect number of channels pruned: expected %v, got %v",
			2, numPruned)
	}

	// The prune tip should be updated, and no channels should be found
	// within the current graph.
	assertPruneTip(t, graph, &blockHash, blockHeight)
	asserNumChans(t, graph, 0)
}
