package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/ioutil"
	"math"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
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

	// noFeeLimit is the maximum value of a payment through Lightning. We
	// can use this value to signal there is no fee limit since payments
	// should never be larger than this.
	noFeeLimit = lnwire.MilliSatoshi(math.MaxUint32)
)

var (
	testSig = &btcec.Signature{
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

// parseTestGraph returns a fully populated ChannelGraph given a path to a JSON
// file which encodes a test graph.
func parseTestGraph(path string) (*testGraphInstance, error) {
	graphJSON, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// First unmarshal the JSON graph into an instance of the testGraph
	// struct. Using the struct tags created above in the struct, the JSON
	// will be properly parsed into the struct above.
	var g testGraph
	if err := json.Unmarshal(graphJSON, &g); err != nil {
		return nil, err
	}

	// We'll use this fake address for the IP address of all the nodes in
	// our tests. This value isn't needed for path finding so it doesn't
	// need to be unique.
	var testAddrs []net.Addr
	testAddr, err := net.ResolveTCPAddr("tcp", "192.0.0.1:8888")
	if err != nil {
		return nil, err
	}
	testAddrs = append(testAddrs, testAddr)

	// Next, create a temporary graph database for usage within the test.
	graph, cleanUp, err := makeTestGraph()
	if err != nil {
		return nil, err
	}

	aliasMap := make(map[string]*btcec.PublicKey)
	var source *channeldb.LightningNode

	// First we insert all the nodes within the graph as vertexes.
	for _, node := range g.Nodes {
		pubBytes, err := hex.DecodeString(node.PubKey)
		if err != nil {
			return nil, err
		}

		dbNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			AuthSigBytes:         testSig.Serialize(),
			LastUpdate:           testTime,
			Addresses:            testAddrs,
			Alias:                node.Alias,
			Features:             testFeatures,
		}
		copy(dbNode.PubKeyBytes[:], pubBytes)

		// We require all aliases within the graph to be unique for our
		// tests.
		if _, ok := aliasMap[node.Alias]; ok {
			return nil, errors.New("aliases for nodes " +
				"must be unique!")
		}

		pub, err := btcec.ParsePubKey(pubBytes, btcec.S256())
		if err != nil {
			return nil, err
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
				return nil, errors.New("JSON is invalid " +
					"multiple nodes are tagged as the source")
			}

			source = dbNode
		}

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddLightningNode(dbNode); err != nil {
			return nil, err
		}
	}

	if source != nil {
		// Set the selected source node
		if err := graph.SetSourceNode(source); err != nil {
			return nil, err
		}
	}

	// With all the vertexes inserted, we can now insert the edges into the
	// test graph.
	for _, edge := range g.Edges {
		node1Bytes, err := hex.DecodeString(edge.Node1)
		if err != nil {
			return nil, err
		}

		node2Bytes, err := hex.DecodeString(edge.Node2)
		if err != nil {
			return nil, err
		}

		fundingTXID := strings.Split(edge.ChannelPoint, ":")[0]
		txidBytes, err := chainhash.NewHashFromStr(fundingTXID)
		if err != nil {
			return nil, err
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
			return nil, err
		}

		edgePolicy := &channeldb.ChannelEdgePolicy{
			SigBytes:                  testSig.Serialize(),
			Flags:                     lnwire.ChanUpdateFlag(edge.Flags),
			ChannelID:                 edge.ChannelID,
			LastUpdate:                testTime,
			TimeLockDelta:             edge.Expiry,
			MinHTLC:                   lnwire.MilliSatoshi(edge.MinHTLC),
			FeeBaseMSat:               lnwire.MilliSatoshi(edge.FeeBaseMsat),
			FeeProportionalMillionths: lnwire.MilliSatoshi(edge.FeeRate),
		}
		if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
			return nil, err
		}
	}

	return &testGraphInstance{
		graph:    graph,
		cleanUp:  cleanUp,
		aliasMap: aliasMap,
	}, nil
}

type testChannelPolicy struct {
	Expiry      uint16
	MinHTLC     lnwire.MilliSatoshi
	FeeBaseMsat lnwire.MilliSatoshi
	FeeRate     lnwire.MilliSatoshi
}

type testChannelEnd struct {
	Alias string
	testChannelPolicy
}

func defaultTestChannelEnd(alias string) *testChannelEnd {
	return &testChannelEnd{
		Alias: alias,
		testChannelPolicy: testChannelPolicy{
			Expiry:      144,
			MinHTLC:     lnwire.MilliSatoshi(1000),
			FeeBaseMsat: lnwire.MilliSatoshi(1000),
			FeeRate:     lnwire.MilliSatoshi(1),
		},
	}
}

func symmetricTestChannel(alias1 string, alias2 string, capacity btcutil.Amount,
	policy *testChannelPolicy, chanID ...uint64) *testChannel {

	// Leaving id zero will result in auto-generation of a channel id during
	// graph construction.
	var id uint64
	if len(chanID) > 0 {
		id = chanID[0]
	}

	return &testChannel{
		Capacity: capacity,
		Node1: &testChannelEnd{
			Alias:             alias1,
			testChannelPolicy: *policy,
		},
		Node2: &testChannelEnd{
			Alias:             alias2,
			testChannelPolicy: *policy,
		},
		ChannelID: id,
	}
}

type testChannel struct {
	Node1     *testChannelEnd
	Node2     *testChannelEnd
	Capacity  btcutil.Amount
	ChannelID uint64
}

type testGraphInstance struct {
	graph   *channeldb.ChannelGraph
	cleanUp func()

	// aliasMap is a map from a node's alias to its public key. This type is
	// provided in order to allow easily look up from the human memorable alias
	// to an exact node's public key.
	aliasMap map[string]*btcec.PublicKey

	// privKeyMap maps a node alias to its private key. This is used to be
	// able to mock a remote node's signing behaviour.
	privKeyMap map[string]*btcec.PrivateKey
}

// createTestGraphFromChannels returns a fully populated ChannelGraph based on a set of
// test channels. Additional required information like keys are derived in
// a deterministical way and added to the channel graph. A list of nodes is
// not required and derived from the channel data. The goal is to keep
// instantiating a test channel graph as light weight as possible.
func createTestGraphFromChannels(testChannels []*testChannel) (*testGraphInstance, error) {
	// We'll use this fake address for the IP address of all the nodes in
	// our tests. This value isn't needed for path finding so it doesn't
	// need to be unique.
	var testAddrs []net.Addr
	testAddr, err := net.ResolveTCPAddr("tcp", "192.0.0.1:8888")
	if err != nil {
		return nil, err
	}
	testAddrs = append(testAddrs, testAddr)

	// Next, create a temporary graph database for usage within the test.
	graph, cleanUp, err := makeTestGraph()
	if err != nil {
		return nil, err
	}

	aliasMap := make(map[string]*btcec.PublicKey)
	privKeyMap := make(map[string]*btcec.PrivateKey)

	nodeIndex := byte(0)
	addNodeWithAlias := func(alias string) (*channeldb.LightningNode, error) {
		keyBytes := make([]byte, 32)
		keyBytes = []byte{
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, nodeIndex + 1,
		}

		privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(),
			keyBytes)

		dbNode := &channeldb.LightningNode{
			HaveNodeAnnouncement: true,
			AuthSigBytes:         testSig.Serialize(),
			LastUpdate:           testTime,
			Addresses:            testAddrs,
			Alias:                alias,
			Features:             testFeatures,
		}

		copy(dbNode.PubKeyBytes[:], pubKey.SerializeCompressed())

		privKeyMap[alias] = privKey

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddLightningNode(dbNode); err != nil {
			return nil, err
		}

		aliasMap[alias] = pubKey
		nodeIndex++

		return dbNode, nil
	}

	var source *channeldb.LightningNode
	if source, err = addNodeWithAlias("roasbeef"); err != nil {
		return nil, err
	}

	// Set the source node
	if err := graph.SetSourceNode(source); err != nil {
		return nil, err
	}

	// Initialize variable that keeps track of the next channel id to assign
	// if none is specified.
	nextUnassignedChannelID := uint64(100000)

	for _, testChannel := range testChannels {
		for _, alias := range []string{
			testChannel.Node1.Alias, testChannel.Node2.Alias} {

			_, exists := aliasMap[alias]
			if !exists {
				addNodeWithAlias(alias)
			}
		}

		channelID := testChannel.ChannelID

		// If no channel id is specified, generate an id.
		if channelID == 0 {
			channelID = nextUnassignedChannelID
			nextUnassignedChannelID++
		}

		var hash [sha256.Size]byte
		hash[len(hash)-1] = byte(channelID)

		fundingPoint := &wire.OutPoint{
			Hash:  chainhash.Hash(hash),
			Index: 0,
		}

		// We first insert the existence of the edge between the two
		// nodes.
		edgeInfo := channeldb.ChannelEdgeInfo{
			ChannelID:    channelID,
			AuthProof:    &testAuthProof,
			ChannelPoint: *fundingPoint,
			Capacity:     testChannel.Capacity,
		}

		node1Bytes := aliasMap[testChannel.Node1.Alias].SerializeCompressed()
		node2Bytes := aliasMap[testChannel.Node2.Alias].SerializeCompressed()

		copy(edgeInfo.NodeKey1Bytes[:], node1Bytes)
		copy(edgeInfo.NodeKey2Bytes[:], node2Bytes)
		copy(edgeInfo.BitcoinKey1Bytes[:], node1Bytes)
		copy(edgeInfo.BitcoinKey2Bytes[:], node2Bytes)

		err = graph.AddChannelEdge(&edgeInfo)
		if err != nil && err != channeldb.ErrEdgeAlreadyExist {
			return nil, err
		}

		edgePolicy := &channeldb.ChannelEdgePolicy{
			SigBytes:                  testSig.Serialize(),
			Flags:                     lnwire.ChanUpdateFlag(0),
			ChannelID:                 channelID,
			LastUpdate:                testTime,
			TimeLockDelta:             testChannel.Node1.Expiry,
			MinHTLC:                   testChannel.Node1.MinHTLC,
			FeeBaseMSat:               testChannel.Node1.FeeBaseMsat,
			FeeProportionalMillionths: testChannel.Node1.FeeRate,
		}
		if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
			return nil, err
		}

		edgePolicy = &channeldb.ChannelEdgePolicy{
			SigBytes:                  testSig.Serialize(),
			Flags:                     lnwire.ChanUpdateFlag(lnwire.ChanUpdateDirection),
			ChannelID:                 channelID,
			LastUpdate:                testTime,
			TimeLockDelta:             testChannel.Node2.Expiry,
			MinHTLC:                   testChannel.Node2.MinHTLC,
			FeeBaseMSat:               testChannel.Node2.FeeBaseMsat,
			FeeProportionalMillionths: testChannel.Node2.FeeRate,
		}

		if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
			return nil, err
		}

		channelID++
	}

	return &testGraphInstance{
		graph:      graph,
		cleanUp:    cleanUp,
		aliasMap:   aliasMap,
		privKeyMap: privKeyMap,
	}, nil
}

// TestFindLowestFeePath tests that out of two routes with identical total
// time lock values, the route with the lowest total fee should be returned.
// The fee rates are chosen such that the test failed on the previous edge
// weight function where one of the terms was fee squared.
func TestFindLowestFeePath(t *testing.T) {
	t.Parallel()

	// Set up a test graph with two paths from roasbeef to target. Both
	// paths have equal total time locks, but the path through b has lower
	// fees (700 compared to 800 for the path through a).
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "first", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}),
		symmetricTestChannel("first", "a", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}),
		symmetricTestChannel("first", "b", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 100,
			MinHTLC: 1,
		}),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 600,
			MinHTLC: 1,
		}),
	}

	testGraphInstance, err := createTestGraphFromChannels(testChannels)
	defer testGraphInstance.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := testGraphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := Vertex(sourceNode.PubKeyBytes)

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := testGraphInstance.aliasMap["target"]
	path, err := findPath(
		nil, testGraphInstance.graph, nil, sourceNode, target,
		ignoredVertexes, ignoredEdges, paymentAmt, noFeeLimit, nil,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	route, err := newRoute(
		paymentAmt, infinity, sourceVertex, path, startingHeight,
		finalHopCLTV)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// Assert that the lowest fee route is returned.
	if !bytes.Equal(route.Hops[1].PubKeyBytes[:],
		testGraphInstance.aliasMap["b"].SerializeCompressed()) {
		t.Fatalf("expected route to pass through b, "+
			"but got a route through %v",
			getAliasFromPubKey(route.Hops[1].PubKeyBytes[:],
				testGraphInstance.aliasMap))
	}
}

func getAliasFromPubKey(pubKey []byte,
	aliases map[string]*btcec.PublicKey) string {

	for alias, key := range aliases {
		if bytes.Equal(key.SerializeCompressed(), pubKey) {
			return alias
		}
	}
	return ""
}

type expectedHop struct {
	alias     string
	fee       lnwire.MilliSatoshi
	fwdAmount lnwire.MilliSatoshi
	timeLock  uint32
}

type basicGraphPathFindingTestCase struct {
	target                string
	paymentAmt            btcutil.Amount
	feeLimit              lnwire.MilliSatoshi
	expectedTotalAmt      lnwire.MilliSatoshi
	expectedTotalTimeLock uint32
	expectedHops          []expectedHop
	expectFailureNoPath   bool
}

var basicGraphPathFindingTests = []basicGraphPathFindingTestCase{
	// Basic route with one intermediate hop.
	{target: "sophon", paymentAmt: 100, feeLimit: noFeeLimit,
		expectedTotalTimeLock: 102, expectedTotalAmt: 100110,
		expectedHops: []expectedHop{
			{alias: "songoku", fwdAmount: 100000, fee: 110, timeLock: 101},
			{alias: "sophon", fwdAmount: 100000, fee: 0, timeLock: 101},
		}},

	// Basic direct (one hop) route.
	{target: "luoji", paymentAmt: 100, feeLimit: noFeeLimit,
		expectedTotalTimeLock: 101, expectedTotalAmt: 100000,
		expectedHops: []expectedHop{
			{alias: "luoji", fwdAmount: 100000, fee: 0, timeLock: 101},
		}},

	// Three hop route where fees need to be added in to the forwarding amount.
	// The high fee hop phamnewun should be avoided.
	{target: "elst", paymentAmt: 50000, feeLimit: noFeeLimit,
		expectedTotalTimeLock: 103, expectedTotalAmt: 50050210,
		expectedHops: []expectedHop{
			{alias: "songoku", fwdAmount: 50000200, fee: 50010, timeLock: 102},
			{alias: "sophon", fwdAmount: 50000000, fee: 200, timeLock: 101},
			{alias: "elst", fwdAmount: 50000000, fee: 0, timeLock: 101},
		}},
	// Three hop route where fees need to be added in to the forwarding amount.
	// However this time the fwdAmount becomes too large for the roasbeef <->
	// songoku channel. Then there is no other option than to choose the
	// expensive phamnuwen channel. This test case was failing before
	// the route search was executed backwards.
	{target: "elst", paymentAmt: 100000, feeLimit: noFeeLimit,
		expectedTotalTimeLock: 103, expectedTotalAmt: 110010220,
		expectedHops: []expectedHop{
			{alias: "phamnuwen", fwdAmount: 100000200, fee: 10010020, timeLock: 102},
			{alias: "sophon", fwdAmount: 100000000, fee: 200, timeLock: 101},
			{alias: "elst", fwdAmount: 100000000, fee: 0, timeLock: 101},
		}},

	// Basic route with fee limit.
	{target: "sophon", paymentAmt: 100, feeLimit: 50,
		expectFailureNoPath: true,
	}}

func TestBasicGraphPathFinding(t *testing.T) {
	t.Parallel()

	testGraphInstance, err := parseTestGraph(basicGraphFilePath)
	defer testGraphInstance.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	// With the test graph loaded, we'll test some basic path finding using
	// the pre-generated graph. Consult the testdata/basic_graph.json file
	// to follow along with the assumptions we'll use to test the path
	// finding.

	for _, testCase := range basicGraphPathFindingTests {
		t.Run(testCase.target, func(subT *testing.T) {
			testBasicGraphPathFindingCase(subT, testGraphInstance, &testCase)
		})
	}
}

func testBasicGraphPathFindingCase(t *testing.T, graphInstance *testGraphInstance,
	test *basicGraphPathFindingTestCase) {

	aliases := graphInstance.aliasMap
	expectedHops := test.expectedHops
	expectedHopCount := len(expectedHops)

	sourceNode, err := graphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := Vertex(sourceNode.PubKeyBytes)

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(test.paymentAmt)
	target := graphInstance.aliasMap[test.target]
	path, err := findPath(
		nil, graphInstance.graph, nil, sourceNode, target,
		ignoredVertexes, ignoredEdges, paymentAmt, test.feeLimit, nil,
	)
	if test.expectFailureNoPath {
		if err == nil {
			t.Fatal("expected no path to be found")
		}
		return
	}
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	route, err := newRoute(
		paymentAmt, test.feeLimit, sourceVertex, path, startingHeight,
		finalHopCLTV,
	)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	if len(route.Hops) != len(expectedHops) {
		t.Fatalf("route is of incorrect length, expected %v got %v",
			expectedHopCount, len(route.Hops))
	}

	// Check hop nodes
	for i := 0; i < len(expectedHops); i++ {
		if !bytes.Equal(route.Hops[i].PubKeyBytes[:],
			aliases[expectedHops[i].alias].SerializeCompressed()) {

			t.Fatalf("%v-th hop should be %v, is instead: %v",
				i, expectedHops[i],
				getAliasFromPubKey(route.Hops[i].PubKeyBytes[:],
					aliases))
		}
	}

	// Next, we'll assert that the "next hop" field in each route payload
	// properly points to the channel ID that the HTLC should be forwarded
	// along.
	hopPayloads := route.ToHopPayloads()
	if len(hopPayloads) != expectedHopCount {
		t.Fatalf("incorrect number of hop payloads: expected %v, got %v",
			expectedHopCount, len(hopPayloads))
	}

	// Hops should point to the next hop
	for i := 0; i < len(expectedHops)-1; i++ {
		var expectedHop [8]byte
		binary.BigEndian.PutUint64(expectedHop[:], route.Hops[i+1].ChannelID)
		if !bytes.Equal(hopPayloads[i].NextAddress[:], expectedHop[:]) {
			t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
				expectedHop[:], hopPayloads[i].NextAddress)
		}
	}

	// The final hop should have a next hop value of all zeroes in order
	// to indicate it's the exit hop.
	var exitHop [8]byte
	lastHopIndex := len(expectedHops) - 1
	if !bytes.Equal(hopPayloads[lastHopIndex].NextAddress[:], exitHop[:]) {
		t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
			exitHop[:], hopPayloads[lastHopIndex].NextAddress)
	}

	var expectedTotalFee lnwire.MilliSatoshi
	for i := 0; i < expectedHopCount; i++ {
		// We'll ensure that the amount to forward, and fees
		// computed for each hop are correct.

		fee := route.HopFee(i)
		if fee != expectedHops[i].fee {
			t.Fatalf("fee incorrect for hop %v: expected %v, got %v",
				i, expectedHops[i].fee, fee)
		}

		if route.Hops[i].AmtToForward != expectedHops[i].fwdAmount {
			t.Fatalf("forwarding amount for hop %v incorrect: "+
				"expected %v, got %v",
				i, expectedHops[i].fwdAmount,
				route.Hops[i].AmtToForward)
		}

		// We'll also assert that the outgoing CLTV value for each
		// hop was set accordingly.
		if route.Hops[i].OutgoingTimeLock != expectedHops[i].timeLock {
			t.Fatalf("outgoing time-lock for hop %v is incorrect: "+
				"expected %v, got %v", i,
				expectedHops[i].timeLock,
				route.Hops[i].OutgoingTimeLock)
		}

		expectedTotalFee += expectedHops[i].fee
	}

	if route.TotalAmount != test.expectedTotalAmt {
		t.Fatalf("total amount incorrect: "+
			"expected %v, got %v",
			test.expectedTotalAmt, route.TotalAmount)
	}

	if route.TotalTimeLock != test.expectedTotalTimeLock {
		t.Fatalf("expected time lock of %v, instead have %v", 2,
			route.TotalTimeLock)
	}

	// The next and prev hop maps should be properly set.
	for i := 0; i < expectedHopCount; i++ {
		prevChan, ok := route.prevHopChannel(aliases[expectedHops[i].alias])
		if !ok {
			t.Fatalf("hop didn't have prev chan but should have")
		}
		if prevChan.ChannelID != route.Hops[i].ChannelID {
			t.Fatalf("incorrect prev chan: expected %v, got %v",
				prevChan.ChannelID, route.Hops[i].ChannelID)
		}
	}

	for i := 0; i < expectedHopCount-1; i++ {
		nextChan, ok := route.nextHopChannel(aliases[expectedHops[i].alias])
		if !ok {
			t.Fatalf("hop didn't have prev chan but should have")
		}
		if nextChan.ChannelID != route.Hops[i+1].ChannelID {
			t.Fatalf("incorrect prev chan: expected %v, got %v",
				nextChan.ChannelID, route.Hops[i+1].ChannelID)
		}
	}

	// Final hop shouldn't have a next chan
	if _, ok := route.nextHopChannel(aliases[expectedHops[lastHopIndex].alias]); ok {
		t.Fatalf("incorrect next hop map, no vertexes should " +
			"be after sophon")
	}
}

func TestPathFindingWithAdditionalEdges(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// In this test, we'll test that we're able to find paths through
	// private channels when providing them as additional edges in our path
	// finding algorithm. To do so, we'll create a new node, doge, and
	// create a private channel between it and songoku. We'll then attempt
	// to find a path from our source node, roasbeef, to doge.
	dogePubKeyHex := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	dogePubKeyBytes, err := hex.DecodeString(dogePubKeyHex)
	if err != nil {
		t.Fatalf("unable to decode public key: %v", err)
	}
	dogePubKey, err := btcec.ParsePubKey(dogePubKeyBytes, btcec.S256())
	if err != nil {
		t.Fatalf("unable to parse public key from bytes: %v", err)
	}

	doge := &channeldb.LightningNode{}
	doge.AddPubKey(dogePubKey)
	doge.Alias = "doge"

	// Create the channel edge going from songoku to doge and include it in
	// our map of additional edges.
	songokuToDoge := &channeldb.ChannelEdgePolicy{
		Node:                      doge,
		ChannelID:                 1337,
		FeeBaseMSat:               1,
		FeeProportionalMillionths: 1000,
		TimeLockDelta:             9,
	}

	additionalEdges := map[Vertex][]*channeldb.ChannelEdgePolicy{
		NewVertex(graph.aliasMap["songoku"]): {songokuToDoge},
	}

	// We should now be able to find a path from roasbeef to doge.
	path, err := findPath(
		nil, graph.graph, additionalEdges, sourceNode, dogePubKey, nil, nil,
		paymentAmt, noFeeLimit, nil,
	)
	if err != nil {
		t.Fatalf("unable to find private path to doge: %v", err)
	}

	// The path should represent the following hops:
	//	roasbeef -> songoku -> doge
	assertExpectedPath(t, path, "songoku", "doge")
}

func TestKShortestPathFinding(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
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
	target := graph.aliasMap["luoji"]
	paths, err := findPaths(
		nil, graph.graph, sourceNode, target, paymentAmt, noFeeLimit, 100,
		nil,
	)
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

	// The first route should be a direct route to luo ji.
	assertExpectedPath(t, paths[0], "roasbeef", "luoji")

	// The second route should be a route to luo ji via satoshi.
	assertExpectedPath(t, paths[1], "roasbeef", "satoshi", "luoji")
}

// TestNewRoute tests whether the construction of hop payloads by newRoute
// is executed correctly.
func TestNewRoute(t *testing.T) {

	var sourceKey [33]byte
	sourceVertex := Vertex(sourceKey)

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	createHop := func(baseFee lnwire.MilliSatoshi,
		feeRate lnwire.MilliSatoshi,
		bandwidth lnwire.MilliSatoshi,
		timeLockDelta uint16) *channeldb.ChannelEdgePolicy {

		return &channeldb.ChannelEdgePolicy{
			Node:                      &channeldb.LightningNode{},
			FeeProportionalMillionths: feeRate,
			FeeBaseMSat:               baseFee,
			TimeLockDelta:             timeLockDelta,
		}
	}

	testCases := []struct {
		// name identifies the test case in the test output.
		name string

		// hops is the list of hops (the route) that gets passed into
		// the call to newRoute.
		hops []*channeldb.ChannelEdgePolicy

		// paymentAmount is the amount that is send into the route
		// indicated by hops.
		paymentAmount lnwire.MilliSatoshi

		// expectedFees is a list of fees that every hop is expected
		// to charge for forwarding.
		expectedFees []lnwire.MilliSatoshi

		// expectedTimeLocks is a list of time lock values that every
		// hop is expected to specify in its outgoing HTLC. The time
		// lock values in this list are relative to the current block
		// height.
		expectedTimeLocks []uint32

		// expectedTotalAmount is the total amount that is expected to
		// be returned from newRoute. This amount should include all
		// the fees to be paid to intermediate hops.
		expectedTotalAmount lnwire.MilliSatoshi

		// expectedTotalTimeLock is the time lock that is expected to
		// be returned from newRoute. This is the time lock that should
		// be specified in the HTLC that is sent by the source node.
		// expectedTotalTimeLock is relative to the current block height.
		expectedTotalTimeLock uint32

		// expectError indicates whether the newRoute call is expected
		// to fail or succeed.
		expectError bool

		// expectedErrorCode indicates the expected error code when
		// expectError is true.
		expectedErrorCode errorCode

		feeLimit lnwire.MilliSatoshi
	}{
		{
			// For a single hop payment, no fees are expected to be paid.
			name:          "single hop",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(100, 1000, 1000000, 10),
			},
			expectedFees:          []lnwire.MilliSatoshi{0},
			expectedTimeLocks:     []uint32{1},
			expectedTotalAmount:   100000,
			expectedTotalTimeLock: 1,
			feeLimit:              noFeeLimit,
		}, {
			// For a two hop payment, only the fee for the first hop
			// needs to be paid. The destination hop does not require
			// a fee to receive the payment.
			name:          "two hop",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 1000, 1000000, 10),
				createHop(30, 1000, 1000000, 5),
			},
			expectedFees:          []lnwire.MilliSatoshi{130, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalAmount:   100130,
			expectedTotalTimeLock: 6,
			feeLimit:              noFeeLimit,
		}, {
			// A three hop payment where the first and second hop
			// will both charge 1 msat. The fee for the first hop
			// is actually slightly higher than 1, because the amount
			// to forward also includes the fee for the second hop. This
			// gets rounded down to 1.
			name:          "three hop",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 10, 1000000, 10),
				createHop(0, 10, 1000000, 5),
				createHop(0, 10, 1000000, 3),
			},
			expectedFees:          []lnwire.MilliSatoshi{1, 1, 0},
			expectedTotalAmount:   100002,
			expectedTimeLocks:     []uint32{4, 1, 1},
			expectedTotalTimeLock: 9,
			feeLimit:              noFeeLimit,
		}, {
			// A three hop payment where the fee of the first hop
			// is slightly higher (11) than the fee at the second hop,
			// because of the increase amount to forward.
			name:          "three hop with fee carry over",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 10000, 1000000, 10),
				createHop(0, 10000, 1000000, 5),
				createHop(0, 10000, 1000000, 3),
			},
			expectedFees:          []lnwire.MilliSatoshi{1010, 1000, 0},
			expectedTotalAmount:   102010,
			expectedTimeLocks:     []uint32{4, 1, 1},
			expectedTotalTimeLock: 9,
			feeLimit:              noFeeLimit,
		}, {
			// A three hop payment where the fee policies of the first and
			// second hop are just high enough to show the fee carry over
			// effect.
			name:          "three hop with minimal fees for carry over",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 10000, 1000000, 10),

				// First hop charges 0.1% so the second hop fee
				// should show up in the first hop fee as 1 msat
				// extra.
				createHop(0, 1000, 1000000, 5),

				// Second hop charges a fixed 1000 msat.
				createHop(1000, 0, 1000000, 3),
			},
			expectedFees:          []lnwire.MilliSatoshi{101, 1000, 0},
			expectedTotalAmount:   101101,
			expectedTimeLocks:     []uint32{4, 1, 1},
			expectedTotalTimeLock: 9,
			feeLimit:              noFeeLimit,
		},
		// Check fee limit behaviour
		{
			name:          "two hop success with fee limit (greater)",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 1000, 1000000, 144),
				createHop(0, 1000, 1000000, 144),
			},
			expectedTotalAmount:   100100,
			expectedFees:          []lnwire.MilliSatoshi{100, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalTimeLock: 145,
			feeLimit:              150,
		}, {
			name:          "two hop success with fee limit (equal)",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 1000, 1000000, 144),
				createHop(0, 1000, 1000000, 144),
			},
			expectedTotalAmount:   100100,
			expectedFees:          []lnwire.MilliSatoshi{100, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalTimeLock: 145,
			feeLimit:              100,
		}, {
			name:          "two hop failure with fee limit (smaller)",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 1000, 1000000, 144),
				createHop(0, 1000, 1000000, 144),
			},
			feeLimit:          50,
			expectError:       true,
			expectedErrorCode: ErrFeeLimitExceeded,
		}, {
			name:          "two hop failure with fee limit (zero)",
			paymentAmount: 100000,
			hops: []*channeldb.ChannelEdgePolicy{
				createHop(0, 1000, 1000000, 144),
				createHop(0, 1000, 1000000, 144),
			},
			feeLimit:          0,
			expectError:       true,
			expectedErrorCode: ErrFeeLimitExceeded,
		}}

	for _, testCase := range testCases {
		assertRoute := func(t *testing.T, route *Route) {
			if route.TotalAmount != testCase.expectedTotalAmount {
				t.Errorf("Expected total amount is be %v"+
					", but got %v instead",
					testCase.expectedTotalAmount,
					route.TotalAmount)
			}

			for i := 0; i < len(testCase.expectedFees); i++ {
				fee := route.HopFee(i)
				if testCase.expectedFees[i] != fee {

					t.Errorf("Expected fee for hop %v to "+
						"be %v, but got %v instead",
						i, testCase.expectedFees[i],
						fee)
				}
			}

			expectedTimeLockHeight := startingHeight +
				testCase.expectedTotalTimeLock

			if route.TotalTimeLock != expectedTimeLockHeight {

				t.Errorf("Expected total time lock to be %v"+
					", but got %v instead",
					expectedTimeLockHeight,
					route.TotalTimeLock)
			}

			for i := 0; i < len(testCase.expectedTimeLocks); i++ {
				expectedTimeLockHeight := startingHeight +
					testCase.expectedTimeLocks[i]

				if expectedTimeLockHeight !=
					route.Hops[i].OutgoingTimeLock {

					t.Errorf("Expected time lock for hop "+
						"%v to be %v, but got %v instead",
						i, expectedTimeLockHeight,
						route.Hops[i].OutgoingTimeLock)
				}
			}
		}

		t.Run(testCase.name, func(t *testing.T) {
			route, err := newRoute(testCase.paymentAmount,
				testCase.feeLimit,
				sourceVertex, testCase.hops, startingHeight,
				finalHopCLTV)

			if testCase.expectError {
				expectedCode := testCase.expectedErrorCode
				if err == nil || !IsError(err, expectedCode) {
					t.Fatalf("expected newRoute to fail "+
						"with error code %v but got "+
						"%v instead",
						expectedCode, err)
				}
			} else {
				if err != nil {
					t.Errorf("unable to create path: %v", err)
					return
				}

				assertRoute(t, route)
			}
		})
	}
}

func TestNewRoutePathTooLong(t *testing.T) {
	t.Skip()

	// Ensure that potential paths which are over the maximum hop-limit are
	// rejected.
	graph, err := parseTestGraph(excessiveHopsGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// We start by confirming that routing a payment 20 hops away is possible.
	// Alice should be able to find a valid route to ursula.
	target := graph.aliasMap["ursula"]
	_, err = findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt, noFeeLimit, nil,
	)
	if err != nil {
		t.Fatalf("path should have been found")
	}

	// Vincent is 21 hops away from Alice, and thus no valid route should be
	// presented to Alice.
	target = graph.aliasMap["vincent"]
	path, err := findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, paymentAmt, noFeeLimit, nil,
	)
	if err == nil {
		t.Fatalf("should not have been able to find path, supposed to be "+
			"greater than 20 hops, found route with %v hops",
			len(path))
	}

}

func TestPathNotAvailable(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
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

	_, err = findPath(
		nil, graph.graph, nil, sourceNode, unknownNode, ignoredVertexes,
		ignoredEdges, 100, noFeeLimit, nil,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

func TestPathInsufficientCapacity(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
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
	target := graph.aliasMap["sophon"]

	payAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	_, err = findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt, noFeeLimit, nil,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailMinHTLC tests that if we attempt to route an HTLC which is
// smaller than the advertised minHTLC of an edge, then path finding fails.
func TestRouteFailMinHTLC(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	// We'll not attempt to route an HTLC of 10 SAT from roasbeef to Son
	// Goku. However, the min HTLC of Son Goku is 1k SAT, as a result, this
	// attempt should fail.
	target := graph.aliasMap["songoku"]
	payAmt := lnwire.MilliSatoshi(10)
	_, err = findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt, noFeeLimit, nil,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailDisabledEdge tests that if we attempt to route to an edge
// that's disabled, then that edge is disqualified, and the routing attempt
// will fail.
func TestRouteFailDisabledEdge(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	defer graph.cleanUp()
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	ignoredEdges := make(map[uint64]struct{})
	ignoredVertexes := make(map[Vertex]struct{})

	// First, we'll try to route from roasbeef -> sophon. This should
	// succeed without issue, and return a single path via phamnuwen
	target := graph.aliasMap["sophon"]
	payAmt := lnwire.NewMSatFromSatoshis(105000)
	_, err = findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt, noFeeLimit, nil,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	// First, we'll modify the edge from roasbeef -> phamnuwen, to read that
	// it's disabled.
	_, _, phamnuwenEdge, err := graph.graph.FetchChannelEdgesByID(999991)
	if err != nil {
		t.Fatalf("unable to fetch goku's edge: %v", err)
	}
	phamnuwenEdge.Flags = lnwire.ChanUpdateDisabled | lnwire.ChanUpdateDirection
	if err := graph.graph.UpdateEdgePolicy(phamnuwenEdge); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// Now, if we attempt to route through that edge, we should get a
	// failure as it is no longer eligible.
	_, err = findPath(
		nil, graph.graph, nil, sourceNode, target, ignoredVertexes,
		ignoredEdges, payAmt, noFeeLimit, nil,
	)
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
	ctx, cleanUp, err := createTestCtxFromFile(startingHeight, specExampleFilePath)
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
	routes, err := ctx.router.FindRoutes(carol, amt, noFeeLimit, 100)
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

	fee := firstRoute.HopFee(0)
	if fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 0)
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
	routes, err = ctx.router.FindRoutes(carol, amt, noFeeLimit, 100)
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

	fee = routes[0].HopFee(0)
	if fee != 10199 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 10199)
	}

	// While for the final hop, as there's no additional hop afterwards, we
	// pay no fee.
	fee = routes[0].HopFee(1)
	if fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 0)
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

func assertExpectedPath(t *testing.T, path []*channeldb.ChannelEdgePolicy,
	nodeAliases ...string) {

	if len(path) != len(nodeAliases) {
		t.Fatal("number of hops and number of aliases do not match")
	}

	for i, hop := range path {
		if hop.Node.Alias != nodeAliases[i] {
			t.Fatalf("expected %v to be pos #%v in hop, instead "+
				"%v was", nodeAliases[i], i, hop.Node.Alias)
		}
	}
}
