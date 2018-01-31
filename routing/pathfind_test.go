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

	// specExampleFilePath is a file path which stores an example which
	// implementations will use in order to ensure that they're calculating
	// the payload for each hop in path properly.
	specExampleFilePath = "testdata/spec_example.json"
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
		NodeSig1Bytes:    testSig.Serialize(),
		NodeSig2Bytes:    testSig.Serialize(),
		BitcoinSig1Bytes: testSig.Serialize(),
		BitcoinSig2Bytes: testSig.Serialize(),
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
	Node1        string `json:"node_1"`
	Node2        string `json:"node_2"`
	ChannelID    uint64 `json:"channel_id"`
	ChannelPoint string `json:"channel_point"`
	Flags        uint16 `json:"flags"`
	Expiry       uint16 `json:"expiry"`
	MinHTLC      int64  `json:"min_htlc"`
	FeeBaseMsat  int64  `json:"fee_base_msat"`
	FeeRate      int64  `json:"fee_rate"`
	Capacity     int64  `json:"capacity"`
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
// provided in order to allow easily look up from the human memorable alias
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

		dbNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			AuthSigBytes:         testSig.Serialize(),
			LastUpdate:           time.Now(),
			Addresses:            testAddrs,
			Alias:                node.Alias,
			Features:             testFeatures,
		}
		copy(dbNode.PubKeyBytes[:], pubBytes)

		// We require all aliases within the graph to be unique for our
		// tests.
		if _, ok := aliasMap[node.Alias]; ok {
			return nil, nil, nil, errors.New("aliases for nodes " +
				"must be unique!")
		}

		pub, err := btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return nil, nil, nil, err
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

	if source != nil {
		// Set the selected source node
		if err := graph.SetSourceNode(source); err != nil {
			return nil, nil, nil, err
		}
	}

	// With all the vertexes inserted, we can now insert the edges into the
	// test graph.
	for _, edge := range g.Edges {
		node1Bytes, err := hex.DecodeString(edge.Node1)
		if err != nil {
			return nil, nil, nil, err
		}

		node2Bytes, err := hex.DecodeString(edge.Node2)
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
			AuthProof:    &testAuthProof,
			ChannelPoint: fundingPoint,
			Capacity:     btcutil.Amount(edge.Capacity),
		}

		copy(edgeInfo.NodeKey1Bytes[:], node1Bytes)
		copy(edgeInfo.NodeKey2Bytes[:], node2Bytes)
		copy(edgeInfo.BitcoinKey1Bytes[:], node1Bytes)
		copy(edgeInfo.BitcoinKey2Bytes[:], node2Bytes)

		err = graph.AddChannelEdge(&edgeInfo)
		if err != nil && err != channeldb.ErrEdgeAlreadyExist {
			return nil, nil, nil, err
		}

		edgePolicy := &channeldb.ChannelEdgePolicy{
			SigBytes:                  testSig.Serialize(),
			Flags:                     lnwire.ChanUpdateFlag(edge.Flags),
			ChannelID:                 edge.ChannelID,
			LastUpdate:                time.Now(),
			TimeLockDelta:             edge.Expiry,
			MinHTLC:                   lnwire.MilliSatoshi(edge.MinHTLC),
			FeeBaseMSat:               lnwire.MilliSatoshi(edge.FeeBaseMsat),
			FeeProportionalMillionths: lnwire.MilliSatoshi(edge.FeeRate),
		}
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
	sourceVertex := Vertex(sourceNode.PubKeyBytes)

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	// With the test graph loaded, we'll test some basic path finding using
	// the pre-generated graph. Consult the testdata/basic_graph.json file
	// to follow along with the assumptions we'll use to test the path
	// finding.
	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := aliases["sophon"]
	path, err := findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	route, err := newRoute(paymentAmt, sourceVertex, path, startingHeight,
		finalHopCLTV)
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
	if !bytes.Equal(route.Hops[0].Channel.Node.PubKeyBytes[:],
		aliases["songoku"].SerializeCompressed()) {

		t.Fatalf("first hop should be goku, is instead: %v",
			route.Hops[0].Channel.Node.Alias)
	}

	// The second hop should be from goku to sophon.
	if !bytes.Equal(route.Hops[1].Channel.Node.PubKeyBytes[:],
		aliases["sophon"].SerializeCompressed()) {

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
	firstHopFee := computeFee(paymentAmt, route.Hops[1].Channel)
	if route.Hops[0].Fee != firstHopFee {
		t.Fatalf("first hop fee incorrect: expected %v, got %v",
			firstHopFee, route.Hops[0].Fee)
	}

	if route.TotalAmount != paymentAmt+firstHopFee {
		t.Fatalf("first hop forwarding amount incorrect: expected %v, got %v",
			paymentAmt+firstHopFee, route.TotalAmount)
	}
	if route.Hops[1].Fee != 0 {
		t.Fatalf("first hop fee incorrect: expected %v, got %v",
			firstHopFee, 0)
	}

	if route.Hops[1].AmtToForward != paymentAmt {
		t.Fatalf("second hop forwarding amount incorrect: expected %v, got %v",
			paymentAmt+firstHopFee, route.Hops[1].AmtToForward)
	}

	// Finally, the next and prev hop maps should be properly set.
	//
	// The previous hop from goku should be the channel from roasbeef, and
	// the next hop should be the channel to sophon.
	gokuPrevChan, ok := route.prevHopChannel(aliases["songoku"])
	if !ok {
		t.Fatalf("goku didn't have next chan but should have")
	}
	if gokuPrevChan.ChannelID != route.Hops[0].Channel.ChannelID {
		t.Fatalf("incorrect prev chan: expected %v, got %v",
			gokuPrevChan.ChannelID, route.Hops[0].Channel.ChannelID)
	}
	gokuNextChan, ok := route.nextHopChannel(aliases["songoku"])
	if !ok {
		t.Fatalf("goku didn't have prev chan but should have")
	}
	if gokuNextChan.ChannelID != route.Hops[1].Channel.ChannelID {
		t.Fatalf("incorrect prev chan: expected %v, got %v",
			gokuNextChan.ChannelID, route.Hops[1].Channel.ChannelID)
	}

	// Sophon shouldn't have a next chan, but she should have a prev chan.
	if _, ok := route.nextHopChannel(aliases["sophon"]); ok {
		t.Fatalf("incorrect next hop map, no vertexes should " +
			"be after sophon")
	}
	sophonPrevEdge, ok := route.prevHopChannel(aliases["sophon"])
	if !ok {
		t.Fatalf("sophon didn't have prev chan but should have")
	}
	if sophonPrevEdge.ChannelID != route.Hops[1].Channel.ChannelID {
		t.Fatalf("incorrect prev chan: expected %v, got %v",
			sophonPrevEdge.ChannelID, route.Hops[1].Channel.ChannelID)
	}

	// Next, attempt to query for a path to Luo Ji for 100 satoshis, there
	// exist two possible paths in the graph, but the shorter (1 hop) path
	// should be selected.
	target = aliases["luoji"]
	path, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}
	route, err = newRoute(paymentAmt, sourceVertex, path, startingHeight,
		finalHopCLTV)
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

	// In this test we'd like to ensure that our algorithm to find the
	// k-shortest paths from a given source node to any destination node
	// works as expected.

	// In our basic_graph.json, there exist two paths from roasbeef to luo
	// ji. Our algorithm should properly find both paths, and also rank
	// them in order of their total "distance".

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := aliases["luoji"]
	paths, err := findPaths(nil, graph, sourceNode, target, paymentAmt)
	if err != nil {
		t.Fatalf("unable to find paths between roasbeef and "+
			"luo ji: %v", err)
	}

	// The algorithm should have found two paths from roasbeef to luo ji.
	if len(paths) != 2 {
		t.Fatalf("two path shouldn't been found, instead %v were",
			len(paths))
	}

	// Additionally, the total hop length of the first path returned should
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
	t.Skip()

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
	ignoredVertexes := make(map[Vertex]struct{})

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// We start by confirming that routing a payment 20 hops away is possible.
	// Alice should be able to find a valid route to ursula.
	target := aliases["ursula"]
	_, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt)
	if err != nil {
		t.Fatalf("path should have been found")
	}

	// Vincent is 21 hops away from Alice, and thus no valid route should be
	// presented to Alice.
	target = aliases["vincent"]
	path, err := findPath(nil, graph, sourceNode, target, ignoredVertexes,
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
	ignoredVertexes := make(map[Vertex]struct{})

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

	_, err = findPath(nil, graph, sourceNode, unknownNode, ignoredVertexes,
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
	ignoredVertexes := make(map[Vertex]struct{})

	// Next, test that attempting to find a path in which the current
	// channel graph cannot support due to insufficient capacity triggers
	// an error.

	// To test his we'll attempt to make a payment of 1 BTC, or 100 million
	// satoshis. The largest channel in the basic graph is of size 100k
	// satoshis, so we shouldn't be able to find a path to sophon even
	// though we have a 2-hop link.
	target := aliases["sophon"]

	const payAmt = btcutil.SatoshiPerBitcoin
	_, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailMinHTLC tests that if we attempt to route an HTLC which is
// smaller than the advertised minHTLC of an edge, then path finding fails.
func TestRouteFailMinHTLC(t *testing.T) {
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
	ignoredVertexes := make(map[Vertex]struct{})

	// We'll not attempt to route an HTLC of 10 SAT from roasbeef to Son
	// Goku. However, the min HTLC of Son Goku is 1k SAT, as a result, this
	// attempt should fail.
	target := aliases["songoku"]
	payAmt := lnwire.MilliSatoshi(10)
	_, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailDisabledEdge tests that if we attempt to route to an edge
// that's disabled, then that edge is disqualified, and the routing attempt
// will fail.
func TestRouteFailDisabledEdge(t *testing.T) {
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
	ignoredVertexes := make(map[Vertex]struct{})

	// First, we'll try to route from roasbeef -> songoku. This should
	// succeed without issue, and return a single path.
	target := aliases["songoku"]
	payAmt := lnwire.NewMSatFromSatoshis(10000)
	_, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	// First, we'll modify the edge from roasbeef -> songoku, to read that
	// it's disabled.
	_, gokuEdge, _, err := graph.FetchChannelEdgesByID(12345)
	if err != nil {
		t.Fatalf("unable to fetch goku's edge: %v", err)
	}
	gokuEdge.Flags = lnwire.ChanUpdateDisabled
	if err := graph.UpdateEdgePolicy(gokuEdge); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// Now, if we attempt to route through that edge, we should get a
	// failure as it is no longer eligible.
	_, err = findPath(nil, graph, sourceNode, target, ignoredVertexes,
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

func TestPathFindSpecExample(t *testing.T) {
	t.Parallel()

	// All our path finding tests will assume a starting height of 100, so
	// we'll pass that in to ensure that the router uses 100 as the current
	// height.
	const startingHeight = 100
	ctx, cleanUp, err := createTestCtx(startingHeight, specExampleFilePath)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	const (
		aliceFinalCLTV = 10
		bobFinalCLTV   = 20
		carolFinalCLTV = 30
		daveFinalCLTV  = 40
	)

	// We'll first exercise the scenario of a direct payment from Bob to
	// Carol, so we set "B" as the source node so path finding starts from
	// Bob.
	bob := ctx.aliases["B"]
	bobNode, err := ctx.graph.FetchLightningNode(bob)
	if err != nil {
		t.Fatalf("unable to find bob: %v", err)
	}
	if err := ctx.graph.SetSourceNode(bobNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// Query for a route of 4,999,999 mSAT to carol.
	carol := ctx.aliases["C"]
	const amt lnwire.MilliSatoshi = 4999999
	routes, err := ctx.router.FindRoutes(carol, amt)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}

	// We should come back with _exactly_ two routes.
	if len(routes) != 2 {
		t.Fatalf("expected %v routes, instead have: %v", 2,
			len(routes))
	}

	// Now we'll examine the first route returned for correctness.
	//
	// It should be sending the exact payment amount as there are no
	// additional hops.
	firstRoute := routes[0]
	if firstRoute.TotalAmount != amt {
		t.Fatalf("wrong total amount: got %v, expected %v",
			firstRoute.TotalAmount, amt)
	}
	if firstRoute.Hops[0].AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			firstRoute.Hops[0].AmtToForward, amt)
	}
	if firstRoute.Hops[0].Fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v",
			firstRoute.Hops[0].Fee, 0)
	}

	// The CLTV expiry should be the current height plus 9 (the expiry for
	// the B -> C channel.
	if firstRoute.TotalTimeLock !=
		startingHeight+DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			firstRoute.TotalTimeLock,
			startingHeight+DefaultFinalCLTVDelta)
	}

	// Next, we'll set A as the source node so we can assert that we create
	// the proper route for any queries starting with Alice.
	alice := ctx.aliases["A"]
	aliceNode, err := ctx.graph.FetchLightningNode(alice)
	if err != nil {
		t.Fatalf("unable to find alice: %v", err)
	}
	if err := ctx.graph.SetSourceNode(aliceNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}
	ctx.router.selfNode = aliceNode
	source, err := ctx.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to retrieve source node: %v", err)
	}
	if !bytes.Equal(source.PubKeyBytes[:], alice.SerializeCompressed()) {
		t.Fatalf("source node not set")
	}

	// We'll now request a route from A -> B -> C.
	ctx.router.routeCache = make(map[routeTuple][]*Route)
	routes, err = ctx.router.FindRoutes(carol, amt)
	if err != nil {
		t.Fatalf("unable to find routes: %v", err)
	}

	// We should come back with _exactly_ two routes.
	if len(routes) != 2 {
		t.Fatalf("expected %v routes, instead have: %v", 2,
			len(routes))
	}

	// Both routes should be two hops.
	if len(routes[0].Hops) != 2 {
		t.Fatalf("route should be %v hops, is instead %v", 2,
			len(routes[0].Hops))
	}
	if len(routes[1].Hops) != 2 {
		t.Fatalf("route should be %v hops, is instead %v", 2,
			len(routes[1].Hops))
	}

	// The total amount should factor in a fee of 10199 and also use a CLTV
	// delta total of 29 (20 + 9),
	expectedAmt := lnwire.MilliSatoshi(5010198)
	if routes[0].TotalAmount != expectedAmt {
		t.Fatalf("wrong amount: got %v, expected %v",
			routes[0].TotalAmount, expectedAmt)
	}
	if routes[0].TotalTimeLock != startingHeight+29 {
		t.Fatalf("wrong total time lock: got %v, expecting %v",
			routes[0].TotalTimeLock, startingHeight+29)
	}

	// Ensure that the hops of the first route are properly crafted.
	//
	// After taking the fee, Bob should be forwarding the remainder which
	// is the exact payment to Bob.
	if routes[0].Hops[0].AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			routes[0].Hops[0].AmtToForward, amt)
	}

	// We shouldn't pay any fee for the first, hop, but the fee for the
	// second hop posted fee should be exactly:

	// The fee that we pay for the second hop will be "applied to the first
	// hop, so we should get a fee of exactly:
	//
	//  * 200 + 4999999 * 2000 / 1000000 = 10199
	if routes[0].Hops[0].Fee != 10199 {
		t.Fatalf("wrong hop fee: got %v, expected %v",
			routes[0].Hops[0].Fee, 10199)
	}

	// While for the final hop, as there's no additional hop afterwards, we
	// pay no fee.
	if routes[0].Hops[1].Fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v",
			routes[0].Hops[0].Fee, 0)
	}

	// The outgoing CLTV value itself should be the current height plus 30
	// to meet Carol's requirements.
	if routes[0].Hops[0].OutgoingTimeLock !=
		startingHeight+DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			routes[0].Hops[0].OutgoingTimeLock,
			startingHeight+DefaultFinalCLTVDelta)
	}

	// For B -> C, we assert that the final hop also has the proper
	// parameters.
	lastHop := routes[0].Hops[1]
	if lastHop.AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			lastHop.AmtToForward, amt)
	}
	if lastHop.OutgoingTimeLock !=
		startingHeight+DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			lastHop.OutgoingTimeLock,
			startingHeight+DefaultFinalCLTVDelta)
	}

	// We'll also make similar assertions for the second route from A to C
	// via D.
	secondRoute := routes[1]
	expectedAmt = 5020398
	if secondRoute.TotalAmount != expectedAmt {
		t.Fatalf("wrong amount: got %v, expected %v",
			secondRoute.TotalAmount, expectedAmt)
	}
	expectedTimeLock := startingHeight + daveFinalCLTV + DefaultFinalCLTVDelta
	if secondRoute.TotalTimeLock != uint32(expectedTimeLock) {
		t.Fatalf("wrong total time lock: got %v, expecting %v",
			secondRoute.TotalTimeLock, expectedTimeLock)
	}
	onionPayload := secondRoute.Hops[0]
	if onionPayload.AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			onionPayload.AmtToForward, amt)
	}
	expectedTimeLock = startingHeight + DefaultFinalCLTVDelta
	if onionPayload.OutgoingTimeLock != uint32(expectedTimeLock) {
		t.Fatalf("wrong outgoing time lock: got %v, expecting %v",
			onionPayload.OutgoingTimeLock,
			expectedTimeLock)
	}

	// The B -> C hop should also be identical as the prior cases.
	lastHop = secondRoute.Hops[1]
	if lastHop.AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			lastHop.AmtToForward, amt)
	}
	if lastHop.OutgoingTimeLock !=
		startingHeight+DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			lastHop.OutgoingTimeLock,
			startingHeight+DefaultFinalCLTVDelta)
	}
}
