package routing

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	prand "math/rand"
)

const (
	// basicGraphFilePath is the file path for a basic graph used within
	// the tests. The basic graph consists of 5 nodes with 5 channels
	// connecting them.
	basicGraphFilePath = "testdata/basic_graph.json"

	// excessiveHopsGraphFilePath is a file path which stores the JSON dump
	// of a graph which was previously triggering an erroneous excessive
	// hops error. The error has since been fixed, but a test case
	// exercising it is kept around to guard against regressions.
	excessiveHopsGraphFilePath = "testdata/excessive_hops.json"
)

var (
	randSource = prand.NewSource(time.Now().Unix())
	randInts   = prand.New(randSource)
	testSig    = &btcec.Signature{
		R: new(big.Int),
		S: new(big.Int),
	}
	_, _ = testSig.R.SetString("63724406601629180062774974542967536251589935445068131219452686511677818569431", 10)
	_, _ = testSig.S.SetString("18801056069249825825291287104931333862866033135609736119018462340006816851118", 10)

	testAuthProof = channeldb.ChannelAuthProof{
		NodeSig1:    testSig,
		NodeSig2:    testSig,
		BitcoinSig1: testSig,
		BitcoinSig2: testSig,
	}
)

// testGraph is the struct which corresponds to the JSON format used to encode
// graphs within the files in the testdata directory.
//
// TODO(roasbeef): add test graph auto-generator
type testGraph struct {
	Info  []string   `json:"info"`
	Nodes []testNode `json:"nodes"`
	Edges []testChan `json:"edges"`
}

// testNode represents a node within the test graph above. We skip certain
// information such as the node's IP address as that information isn't needed
// for our tests.
type testNode struct {
	Source bool   `json:"source"`
	PubKey string `json:"pubkey"`
	Alias  string `json:"alias"`
}

// testChan represents the JSON version of a payment channel. This struct
// matches the Json that's encoded under the "edges" key within the test graph.
type testChan struct {
	Node1        string  `json:"node_1"`
	Node2        string  `json:"node_2"`
	ChannelID    uint64  `json:"channel_id"`
	ChannelPoint string  `json:"channel_point"`
	Flags        uint16  `json:"flags"`
	Expiry       uint16  `json:"expiry"`
	MinHTLC      int64   `json:"min_htlc"`
	FeeBaseMsat  int64   `json:"fee_base_msat"`
	FeeRate      float64 `json:"fee_rate"`
	Capacity     int64   `json:"capacity"`
}

// makeTestGraph creates a new instance of a channeldb.ChannelGraph for testing
// purposes. A callback which cleans up the created temporary directories is
// also returned and intended to be executed after the test completes.
func makeTestGraph() (*channeldb.ChannelGraph, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb.ChannelGraph(), cleanUp, nil
}

// aliasMap is a map from a node's alias to its public key. This type is
// provided in order to allow easily look up from the human rememberable alias
// to an exact node's public key.
type aliasMap map[string]*btcec.PublicKey

// parseTestGraph returns a fully populated ChannelGraph given a path to a JSON
// file which encodes a test graph.
func parseTestGraph(path string) (*channeldb.ChannelGraph, func(), aliasMap, error) {
	graphJSON, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}

	// First unmarshal the JSON graph into an instance of the testGraph
	// struct. Using the struct tags created above in the struct, the JSON
	// will be properly parsed into the struct above.
	var g testGraph
	if err := json.Unmarshal(graphJSON, &g); err != nil {
		return nil, nil, nil, err
	}

	// We'll use this fake address for the IP address of all the nodes in
	// our tests. This value isn't needed for path finding so it doesn't
	// need to be unique.
	var testAddrs []net.Addr
	testAddr, err := net.ResolveTCPAddr("tcp", "192.0.0.1:8888")
	if err != nil {
		return nil, nil, nil, err
	}
	testAddrs = append(testAddrs, testAddr)

	// Next, create a temporary graph database for usage within the test.
	graph, cleanUp, err := makeTestGraph()
	if err != nil {
		return nil, nil, nil, err
	}

	aliasMap := make(map[string]*btcec.PublicKey)
	var source *channeldb.LightningNode

	// First we insert all the nodes within the graph as vertexes.
	for _, node := range g.Nodes {
		pubBytes, err := hex.DecodeString(node.PubKey)
		if err != nil {
			return nil, nil, nil, err
		}
		pub, err := btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		dbNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			AuthSig:              testSig,
			LastUpdate:           time.Now(),
			Addresses:            testAddrs,
			PubKey:               pub,
			Alias:                node.Alias,
			Features:             testFeatures,
		}

		// We require all aliases within the graph to be unique for our
		// tests.
		if _, ok := aliasMap[node.Alias]; ok {
			return nil, nil, nil, errors.New("aliases for nodes " +
				"must be unique!")
		}

		// If the alias is unique, then add the node to the
		// alias map for easy lookup.
		aliasMap[node.Alias] = pub

		// If the node is tagged as the source, then we create a
		// pointer to is so we can mark the source in the graph
		// properly.
		if node.Source {
			// If we come across a node that's marked as the
			// source, and we've already set the source in a prior
			// iteration, then the JSON has an error as only ONE
			// node can be the source in the graph.
			if source != nil {
				return nil, nil, nil, errors.New("JSON is invalid " +
					"multiple nodes are tagged as the source")
			}

			source = dbNode
		}

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddLightningNode(dbNode); err != nil {
			return nil, nil, nil, err
		}
	}

	// Set the selected source node
	if err := graph.SetSourceNode(source); err != nil {
		return nil, nil, nil, err
	}

	// With all the vertexes inserted, we can now insert the edges into the
	// test graph.
	for _, edge := range g.Edges {
		node1Bytes, err := hex.DecodeString(edge.Node1)
		if err != nil {
			return nil, nil, nil, err
		}
		node1Pub, err := btcec.ParsePubKey(node1Bytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		node2Bytes, err := hex.DecodeString(edge.Node2)
		if err != nil {
			return nil, nil, nil, err
		}
		node2Pub, err := btcec.ParsePubKey(node2Bytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
		}

		fundingTXID := strings.Split(edge.ChannelPoint, ":")[0]
		txidBytes, err := chainhash.NewHashFromStr(fundingTXID)
		if err != nil {
			return nil, nil, nil, err
		}
		fundingPoint := wire.OutPoint{
			Hash:  *txidBytes,
			Index: 0,
		}

		// We first insert the existence of the edge between the two
		// nodes.
		edgeInfo := channeldb.ChannelEdgeInfo{
			ChannelID:    edge.ChannelID,
			NodeKey1:     node1Pub,
			NodeKey2:     node2Pub,
			BitcoinKey1:  node1Pub,
			BitcoinKey2:  node2Pub,
			AuthProof:    &testAuthProof,
			ChannelPoint: fundingPoint,
			Capacity:     btcutil.Amount(edge.Capacity),
		}
		if err := graph.AddChannelEdge(&edgeInfo); err != nil {
			return nil, nil, nil, err
		}

		edgePolicy := &channeldb.ChannelEdgePolicy{
			Signature:                 testSig,
			ChannelID:                 edge.ChannelID,
			LastUpdate:                time.Now(),
			TimeLockDelta:             edge.Expiry,
			MinHTLC:                   lnwire.MilliSatoshi(edge.MinHTLC),
			FeeBaseMSat:               lnwire.MilliSatoshi(edge.FeeBaseMsat),
			FeeProportionalMillionths: lnwire.MilliSatoshi(edge.FeeRate),
		}

		// As the graph itself is directed, we need to insert two edges
		// into the graph: one from node1->node2 and one from
		// node2->node1. A flag of 0 indicates this is the routing
		// policy for the first node, and a flag of 1 indicates its the
		// information for the second node.
		edgePolicy.Flags = 0
		if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
			return nil, nil, nil, err
		}

		edgePolicy.Flags = 1
		if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
			return nil, nil, nil, err
		}
	}

	return graph, cleanUp, aliasMap, nil
}

func TestBasicGraphPathFinding(t *testing.T) {
	t.Parallel()

	graph, cleanUp, aliases, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[vertex]struct{})

	// With the test graph loaded, we'll test some basic path finding using
	// the pre-generated graph. Consult the testdata/basic_graph.json file
	// to follow along with the assumptions we'll use to test the path
	// finding.
	const startingHeight = 100

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := aliases["sophon"]
	path, err := findPath(graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	route, err := newRoute(paymentAmt, path, startingHeight)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// The length of the route selected should be of exactly length two.
	if len(route.Hops) != 2 {
		t.Fatalf("route is of incorrect length, expected %v got %v", 2,
			len(route.Hops))
	}

	// As each hop only decrements a single block from the time-lock, the
	// total time lock value should two more than our starting block
	// height.
	if route.TotalTimeLock != 102 {
		t.Fatalf("expected time lock of %v, instead have %v", 2,
			route.TotalTimeLock)
	}

	// The first hop in the path should be an edge from roasbeef to goku.
	if !route.Hops[0].Channel.Node.PubKey.IsEqual(aliases["songoku"]) {
		t.Fatalf("first hop should be goku, is instead: %v",
			route.Hops[0].Channel.Node.Alias)
	}

	// The second hop should be from goku to sophon.
	if !route.Hops[1].Channel.Node.PubKey.IsEqual(aliases["sophon"]) {
		t.Fatalf("second hop should be sophon, is instead: %v",
			route.Hops[0].Channel.Node.Alias)
	}

	// Next, we'll assert that the "next hop" field in each route payload
	// properly points to the channel ID that the HTLC should be forwarded
	// along.
	hopPayloads := route.ToHopPayloads()
	if len(hopPayloads) != 2 {
		t.Fatalf("incorrect number of hop payloads: expected %v, got %v",
			2, len(hopPayloads))
	}

	// The first hop should point to the second hop.
	var expectedHop [8]byte
	binary.BigEndian.PutUint64(expectedHop[:], route.Hops[1].Channel.ChannelID)
	if !bytes.Equal(hopPayloads[0].NextAddress[:], expectedHop[:]) {
		t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
			expectedHop[:], hopPayloads[0].NextAddress)
	}

	// The second hop should have a next hop value of all zeroes in order
	// to indicate it's the exit hop.
	var exitHop [8]byte
	if !bytes.Equal(hopPayloads[1].NextAddress[:], exitHop[:]) {
		t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
			exitHop[:], hopPayloads[0].NextAddress)
	}

	// We'll also assert that the outgoing CLTV value for each hop was set
	// accordingly.
	if route.Hops[0].OutgoingTimeLock != 101 {
		t.Fatalf("expected outgoing time-lock of %v, instead have %v",
			1, route.Hops[0].OutgoingTimeLock)
	}
	if route.Hops[1].OutgoingTimeLock != 101 {
		t.Fatalf("outgoing time-lock for final hop is incorrect: "+
			"expected %v, got %v", 1, route.Hops[1].OutgoingTimeLock)
	}

	// Additionally, we'll ensure that the amount to forward, and fees
	// computed for each hop are correct.
	firstHopFee := route.Hops[0].Channel.FeeBaseMSat
	if route.TotalAmount != paymentAmt+firstHopFee {
		t.Fatalf("first hop forwarding amount incorrect: expected %v, got %v",
			paymentAmt+firstHopFee, route.Hops[0].AmtToForward)
	}
	if route.Hops[0].Fee != firstHopFee {
		t.Fatalf("first hop fee incorrect: expected %v, got %v",
			firstHopFee, route.Hops[0].Fee)
	}
	if route.Hops[1].AmtToForward != paymentAmt {
		t.Fatalf("second hop forwarding amount incorrect: expected %v, got %v",
			paymentAmt+firstHopFee, route.Hops[0].AmtToForward)
	}
	if route.Hops[1].Fee != 0 {
		t.Fatalf("second hop fee incorrect: expected %v, got %v",
			0, route.Hops[1].Fee)
	}

	// Next, attempt to query for a path to Luo Ji for 100 satoshis, there
	// exist two possible paths in the graph, but the shorter (1 hop) path
	// should be selected.
	target = aliases["luoji"]
	path, err = findPath(graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}
	route, err = newRoute(paymentAmt, path, startingHeight)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// The length of the path should be exactly one hop as it's the
	// "shortest" known path in the graph.
	if len(route.Hops) != 1 {
		t.Fatalf("shortest path not selected, should be of length 1, "+
			"is instead: %v", len(route.Hops))
	}

	// As we have a direct path, the total time lock value should be
	// exactly the current block height plus one.
	if route.TotalTimeLock != 101 {
		t.Fatalf("expected time lock of %v, instead have %v", 1,
			route.TotalTimeLock)
	}

	// Additionally, since this is a single-hop payment, we shouldn't have
	// to pay any fees in total, so the total amount should be the payment
	// amount.
	if route.TotalAmount != paymentAmt {
		t.Fatalf("incorrect total amount, expected %v got %v",
			paymentAmt, route.TotalAmount)
	}
}

func TestKShortestPathFinding(t *testing.T) {
	t.Parallel()

	graph, cleanUp, aliases, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// In this test we'd like to ensure that our algoirthm to find the
	// k-shortest paths from a given source node to any destination node
	// works as exepcted.

	// In our basic_graph.json, there exist two paths from roasbeef to luo
	// ji. Our algorithm should properly find both paths, and also rank
	// them in order of their total "distance".

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := aliases["luoji"]
	paths, err := findPaths(graph, sourceNode, target, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find paths between roasbeef and "+
			"luo ji: %v", err)
	}

	// The algorithm should've found two paths from roasbeef to luo ji.
	if len(paths) != 2 {
		t.Fatalf("two path shouldn't been found, instead %v were",
			len(paths))
	}

	// Additinoally, the total hop length of the first path returned should
	// be _less_ than that of the second path returned.
	if len(paths[0]) > len(paths[1]) {
		t.Fatalf("paths found not ordered properly")
	}

	// Finally, we'll assert the exact expected ordering of both paths
	// found.
	assertExpectedPath := func(path []*ChannelHop, nodeAliases ...string) {
		for i, hop := range path {
			if hop.Node.Alias != nodeAliases[i] {
				t.Fatalf("expected %v to be pos #%v in hop, "+
					"instead %v was", nodeAliases[i], i,
					hop.Node.Alias)
			}
		}
	}

	// The first route should be a direct route to luo ji.
	assertExpectedPath(paths[0], "roasbeef", "luoji")

	// The second route should be a route to luo ji via satoshi.
	assertExpectedPath(paths[1], "roasbeef", "satoshi", "luoji")
}

func TestNewRoutePathTooLong(t *testing.T) {
	t.Parallel()

	// Ensure that potential paths which are over the maximum hop-limit are
	// rejected.
	graph, cleanUp, aliases, err := parseTestGraph(excessiveHopsGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[vertex]struct{})

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// We start by confirminig that routing a payment 20 hops away is possible.
	// Alice should be able to find a valid route to ursula.
	target := aliases["ursula"]
	_, err = findPath(graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("path should have been found")
	}

	// Vincent is 21 hops away from Alice, and thus no valid route should be
	// presented to Alice.
	target = aliases["vincent"]
	path, err := findPath(graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err == nil {
		t.Fatalf("should not have been able to find path, supposed to be "+
			"greater than 20 hops, found route with %v hops",
			len(path))
	}

}

func TestPathNotAvailable(t *testing.T) {
	t.Parallel()

	graph, cleanUp, _, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[vertex]struct{})

	// With the test graph loaded, we'll test that queries for target that
	// are either unreachable within the graph, or unknown result in an
	// error.
	unknownNodeStr := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	unknownNodeBytes, err := hex.DecodeString(unknownNodeStr)
	if err != nil {
		t.Fatalf("unable to parse bytes: %v", err)
	}
	unknownNode, err := btcec.ParsePubKey(unknownNodeBytes, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse pubkey: %v", err)
	}

	_, err = findPath(graph, sourceNode, unknownNode, ignoredVertexes,
		ignoredEdges, 100)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

func TestPathInsufficientCapacity(t *testing.T) {
	t.Parallel()

	graph, cleanUp, aliases, err := parseTestGraph(basicGraphFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[vertex]struct{})

	// Next, test that attempting to find a path in which the current
	// channel graph cannot support due to insufficient capacity triggers
	// an error.

	// To test his we'll attempt to make a payment of 1 BTC, or 100 million
	// satoshis. The largest channel in the basic graph is of size 100k
	// satoshis, so we shouldn't be able to find a path to sophon even
	// though we have a 2-hop link.
	target := aliases["sophon"]

	const payAmt = btcutil.SatoshiPerBitcoin
	_, err = findPath(graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

func TestPathInsufficientCapacityWithFee(t *testing.T) {
	t.Parallel()

	// TODO(roasbeef): encode live graph to json

	// TODO(roasbeef): need to add a case, or modify the fee ratio for one
	// to ensure that has going forward, but when fees are applied doesn't
	// work
}

// TODO(roasbeef): more time-lock calvulation tests
