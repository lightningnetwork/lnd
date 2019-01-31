package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
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
	noRestrictions = &RestrictParams{
		FeeLimit:          noFeeLimit,
		ProbabilitySource: noProbabilitySource,
	}
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

// noProbabilitySource is used in testing to return the same probability 1 for
// all edges.
func noProbabilitySource(route.Vertex, EdgeLocator, lnwire.MilliSatoshi) float64 {
	return 1
}

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
	ChannelFlags uint8  `json:"channel_flags"`
	MessageFlags uint8  `json:"message_flags"`
	Expiry       uint16 `json:"expiry"`
	MinHTLC      int64  `json:"min_htlc"`
	MaxHTLC      int64  `json:"max_htlc"`
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

	aliasMap := make(map[string]route.Vertex)
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

		// If the alias is unique, then add the node to the
		// alias map for easy lookup.
		aliasMap[node.Alias] = dbNode.PubKeyBytes

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

		if bytes.Compare(node1Bytes, node2Bytes) == 1 {
			return nil, fmt.Errorf(
				"channel %v node order incorrect",
				edge.ChannelID,
			)
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
			MessageFlags:              lnwire.ChanUpdateMsgFlags(edge.MessageFlags),
			ChannelFlags:              lnwire.ChanUpdateChanFlags(edge.ChannelFlags),
			ChannelID:                 edge.ChannelID,
			LastUpdate:                testTime,
			TimeLockDelta:             edge.Expiry,
			MinHTLC:                   lnwire.MilliSatoshi(edge.MinHTLC),
			MaxHTLC:                   lnwire.MilliSatoshi(edge.MaxHTLC),
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
	MaxHTLC     lnwire.MilliSatoshi
	FeeBaseMsat lnwire.MilliSatoshi
	FeeRate     lnwire.MilliSatoshi
	LastUpdate  time.Time
	Disabled    bool
}

type testChannelEnd struct {
	Alias string
	*testChannelPolicy
}

func defaultTestChannelEnd(alias string, capacity btcutil.Amount) *testChannelEnd {
	return &testChannelEnd{
		Alias: alias,
		testChannelPolicy: &testChannelPolicy{
			Expiry:      144,
			MinHTLC:     lnwire.MilliSatoshi(1000),
			MaxHTLC:     lnwire.NewMSatFromSatoshis(capacity),
			FeeBaseMsat: lnwire.MilliSatoshi(1000),
			FeeRate:     lnwire.MilliSatoshi(1),
			LastUpdate:  testTime,
			Disabled:    false,
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
			testChannelPolicy: policy,
		},
		Node2: &testChannelEnd{
			Alias:             alias2,
			testChannelPolicy: policy,
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
	aliasMap map[string]route.Vertex

	// privKeyMap maps a node alias to its private key. This is used to be
	// able to mock a remote node's signing behaviour.
	privKeyMap map[string]*btcec.PrivateKey
}

// createTestGraphFromChannels returns a fully populated ChannelGraph based on a set of
// test channels. Additional required information like keys are derived in
// a deterministical way and added to the channel graph. A list of nodes is
// not required and derived from the channel data. The goal is to keep
// instantiating a test channel graph as light weight as possible.
func createTestGraphFromChannels(testChannels []*testChannel, source string) (
	*testGraphInstance, error) {

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

	aliasMap := make(map[string]route.Vertex)
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

		aliasMap[alias] = dbNode.PubKeyBytes
		nodeIndex++

		return dbNode, nil
	}

	// Add the source node.
	dbNode, err := addNodeWithAlias(source)
	if err != nil {
		return nil, err
	}

	if err = graph.SetSourceNode(dbNode); err != nil {
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
				_, err := addNodeWithAlias(alias)
				if err != nil {
					return nil, err
				}
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

		// Sort nodes
		node1 := testChannel.Node1
		node2 := testChannel.Node2
		node1Vertex := aliasMap[node1.Alias]
		node2Vertex := aliasMap[node2.Alias]
		if bytes.Compare(node1Vertex[:], node2Vertex[:]) == 1 {
			node1, node2 = node2, node1
			node1Vertex, node2Vertex = node2Vertex, node1Vertex
		}

		// We first insert the existence of the edge between the two
		// nodes.
		edgeInfo := channeldb.ChannelEdgeInfo{
			ChannelID:    channelID,
			AuthProof:    &testAuthProof,
			ChannelPoint: *fundingPoint,
			Capacity:     testChannel.Capacity,

			NodeKey1Bytes:    node1Vertex,
			BitcoinKey1Bytes: node1Vertex,
			NodeKey2Bytes:    node2Vertex,
			BitcoinKey2Bytes: node2Vertex,
		}

		err = graph.AddChannelEdge(&edgeInfo)
		if err != nil && err != channeldb.ErrEdgeAlreadyExist {
			return nil, err
		}

		if testChannel.Node1.testChannelPolicy != nil {
			var msgFlags lnwire.ChanUpdateMsgFlags
			if testChannel.Node1.MaxHTLC != 0 {
				msgFlags |= lnwire.ChanUpdateOptionMaxHtlc
			}
			var channelFlags lnwire.ChanUpdateChanFlags
			if testChannel.Node1.Disabled {
				channelFlags |= lnwire.ChanUpdateDisabled
			}
			edgePolicy := &channeldb.ChannelEdgePolicy{
				SigBytes:                  testSig.Serialize(),
				MessageFlags:              msgFlags,
				ChannelFlags:              channelFlags,
				ChannelID:                 channelID,
				LastUpdate:                node1.LastUpdate,
				TimeLockDelta:             node1.Expiry,
				MinHTLC:                   node1.MinHTLC,
				MaxHTLC:                   node1.MaxHTLC,
				FeeBaseMSat:               node1.FeeBaseMsat,
				FeeProportionalMillionths: node1.FeeRate,
			}
			if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
				return nil, err
			}
		}

		if testChannel.Node2.testChannelPolicy != nil {
			var msgFlags lnwire.ChanUpdateMsgFlags
			if testChannel.Node2.MaxHTLC != 0 {
				msgFlags |= lnwire.ChanUpdateOptionMaxHtlc
			}
			channelFlags := lnwire.ChanUpdateDirection
			if testChannel.Node2.Disabled {
				channelFlags |= lnwire.ChanUpdateDisabled
			}
			edgePolicy := &channeldb.ChannelEdgePolicy{
				SigBytes:                  testSig.Serialize(),
				MessageFlags:              msgFlags,
				ChannelFlags:              channelFlags,
				ChannelID:                 channelID,
				LastUpdate:                node2.LastUpdate,
				TimeLockDelta:             node2.Expiry,
				MinHTLC:                   node2.MinHTLC,
				MaxHTLC:                   node2.MaxHTLC,
				FeeBaseMSat:               node2.FeeBaseMsat,
				FeeProportionalMillionths: node2.FeeRate,
			}
			if err := graph.UpdateEdgePolicy(edgePolicy); err != nil {
				return nil, err
			}
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
			MaxHTLC: 100000000,
		}),
		symmetricTestChannel("first", "a", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}),
		symmetricTestChannel("first", "b", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 100,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 600,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}),
	}

	testGraphInstance, err := createTestGraphFromChannels(
		testChannels, "roasbeef",
	)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraphInstance.cleanUp()

	sourceNode, err := testGraphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := testGraphInstance.aliasMap["target"]
	path, err := findPath(
		&graphParams{
			graph: testGraphInstance.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, paymentAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	route, err := newRoute(
		paymentAmt, sourceVertex, path, startingHeight,
		finalHopCLTV)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// Assert that the lowest fee route is returned.
	if route.Hops[1].PubKeyBytes != testGraphInstance.aliasMap["b"] {
		t.Fatalf("expected route to pass through b, "+
			"but got a route through %v",
			getAliasFromPubKey(route.Hops[1].PubKeyBytes,
				testGraphInstance.aliasMap))
	}
}

func getAliasFromPubKey(pubKey route.Vertex,
	aliases map[string]route.Vertex) string {

	for alias, key := range aliases {
		if key == pubKey {
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
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraphInstance.cleanUp()

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
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(test.paymentAmt)
	target := graphInstance.aliasMap[test.target]
	path, err := findPath(
		&graphParams{
			graph: graphInstance.graph,
		},
		&RestrictParams{
			FeeLimit:          test.feeLimit,
			ProbabilitySource: noProbabilitySource,
		},
		sourceNode.PubKeyBytes, target, paymentAmt,
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
		paymentAmt, sourceVertex, path, startingHeight,
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
		if route.Hops[i].PubKeyBytes != aliases[expectedHops[i].alias] {

			t.Fatalf("%v-th hop should be %v, is instead: %v",
				i, expectedHops[i],
				getAliasFromPubKey(route.Hops[i].PubKeyBytes,
					aliases))
		}
	}

	// Next, we'll assert that the "next hop" field in each route payload
	// properly points to the channel ID that the HTLC should be forwarded
	// along.
	sphinxPath, err := route.ToSphinxPath()
	if err != nil {
		t.Fatalf("unable to make sphinx path: %v", err)
	}
	if sphinxPath.TrueRouteLength() != expectedHopCount {
		t.Fatalf("incorrect number of hop payloads: expected %v, got %v",
			expectedHopCount, sphinxPath.TrueRouteLength())
	}

	// Hops should point to the next hop
	for i := 0; i < len(expectedHops)-1; i++ {
		var expectedHop [8]byte
		binary.BigEndian.PutUint64(expectedHop[:], route.Hops[i+1].ChannelID)
		if !bytes.Equal(sphinxPath[i].HopData.NextAddress[:], expectedHop[:]) {
			t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
				expectedHop[:], sphinxPath[i].HopData.NextAddress)
		}
	}

	// The final hop should have a next hop value of all zeroes in order
	// to indicate it's the exit hop.
	var exitHop [8]byte
	lastHopIndex := len(expectedHops) - 1
	if !bytes.Equal(sphinxPath[lastHopIndex].HopData.NextAddress[:], exitHop[:]) {
		t.Fatalf("first hop has incorrect next hop: expected %x, got %x",
			exitHop[:], sphinxPath[lastHopIndex].HopData.NextAddress)
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
}

func TestPathFindingWithAdditionalEdges(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

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
	copy(doge.PubKeyBytes[:], dogePubKeyBytes)
	graph.aliasMap["doge"] = doge.PubKeyBytes

	// Create the channel edge going from songoku to doge and include it in
	// our map of additional edges.
	songokuToDoge := &channeldb.ChannelEdgePolicy{
		Node:                      doge,
		ChannelID:                 1337,
		FeeBaseMSat:               1,
		FeeProportionalMillionths: 1000,
		TimeLockDelta:             9,
	}

	additionalEdges := map[route.Vertex][]*channeldb.ChannelEdgePolicy{
		graph.aliasMap["songoku"]: {songokuToDoge},
	}

	// We should now be able to find a path from roasbeef to doge.
	path, err := findPath(
		&graphParams{
			graph:           graph.graph,
			additionalEdges: additionalEdges,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, doge.PubKeyBytes, paymentAmt,
	)
	if err != nil {
		t.Fatalf("unable to find private path to doge: %v", err)
	}

	// The path should represent the following hops:
	//	roasbeef -> songoku -> doge
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "doge")
}

// TestNewRoute tests whether the construction of hop payloads by newRoute
// is executed correctly.
func TestNewRoute(t *testing.T) {

	var sourceKey [33]byte
	sourceVertex := route.Vertex(sourceKey)

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
		}}

	for _, testCase := range testCases {
		assertRoute := func(t *testing.T, route *route.Route) {
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
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// We start by confirming that routing a payment 20 hops away is
	// possible. Alice should be able to find a valid route to ursula.
	target := graph.aliasMap["ursula"]
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, paymentAmt,
	)
	if err != nil {
		t.Fatalf("path should have been found")
	}

	// Vincent is 21 hops away from Alice, and thus no valid route should be
	// presented to Alice.
	target = graph.aliasMap["vincent"]
	path, err := findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, paymentAmt,
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
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// With the test graph loaded, we'll test that queries for target that
	// are either unreachable within the graph, or unknown result in an
	// error.
	unknownNodeStr := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	unknownNodeBytes, err := hex.DecodeString(unknownNodeStr)
	if err != nil {
		t.Fatalf("unable to parse bytes: %v", err)
	}
	var unknownNode route.Vertex
	copy(unknownNode[:], unknownNodeBytes)

	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, unknownNode, 100,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

func TestPathInsufficientCapacity(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

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
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
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
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// We'll not attempt to route an HTLC of 10 SAT from roasbeef to Son
	// Goku. However, the min HTLC of Son Goku is 1k SAT, as a result, this
	// attempt should fail.
	target := graph.aliasMap["songoku"]
	payAmt := lnwire.MilliSatoshi(10)
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailMaxHTLC tests that if we attempt to route an HTLC which is
// larger than the advertised max HTLC of an edge, then path finding fails.
func TestRouteFailMaxHTLC(t *testing.T) {
	t.Parallel()

	// Set up a test graph:
	// roasbeef <--> firstHop <--> secondHop <--> target
	// We will be adjusting the max HTLC of the edge between the first and
	// second hops.
	var firstToSecondID uint64 = 1
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "first", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000001,
		}),
		symmetricTestChannel("first", "second", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000002,
		}, firstToSecondID),
		symmetricTestChannel("second", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000003,
		}),
	}

	graph, err := createTestGraphFromChannels(testChannels, "roasbeef")
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// First, attempt to send a payment greater than the max HTLC we are
	// about to set, which should succeed.
	target := graph.aliasMap["target"]
	payAmt := lnwire.MilliSatoshi(100001)
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("graph should've been able to support payment: %v", err)
	}

	// Next, update the middle edge policy to only allow payments up to 100k
	// msat.
	_, midEdge, _, err := graph.graph.FetchChannelEdgesByID(firstToSecondID)
	midEdge.MessageFlags = 1
	midEdge.MaxHTLC = payAmt - 1
	if err := graph.graph.UpdateEdgePolicy(midEdge); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// We'll now attempt to route through that edge with a payment above
	// 100k msat, which should fail.
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestRouteFailDisabledEdge tests that if we attempt to route to an edge
// that's disabled, then that edge is disqualified, and the routing attempt
// will fail. We also test that this is true only for non-local edges, as we'll
// ignore the disable flags, with the assumption that the correct bandwidth is
// found among the bandwidth hints.
func TestRouteFailDisabledEdge(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// First, we'll try to route from roasbeef -> sophon. This should
	// succeed without issue, and return a single path via phamnuwen
	target := graph.aliasMap["sophon"]
	payAmt := lnwire.NewMSatFromSatoshis(105000)
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	// Disable the edge roasbeef->phamnuwen. This should not impact the
	// path finding, as we don't consider the disable flag for local
	// channels (and roasbeef is the source).
	roasToPham := uint64(999991)
	_, e1, e2, err := graph.graph.FetchChannelEdgesByID(roasToPham)
	if err != nil {
		t.Fatalf("unable to fetch edge: %v", err)
	}
	e1.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.graph.UpdateEdgePolicy(e1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	e2.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.graph.UpdateEdgePolicy(e2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	// Now, we'll modify the edge from phamnuwen -> sophon, to read that
	// it's disabled.
	phamToSophon := uint64(99999)
	_, e, _, err := graph.graph.FetchChannelEdgesByID(phamToSophon)
	if err != nil {
		t.Fatalf("unable to fetch edge: %v", err)
	}
	e.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.graph.UpdateEdgePolicy(e); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// If we attempt to route through that edge, we should get a failure as
	// it is no longer eligible.
	_, err = findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// TestPathSourceEdgesBandwidth tests that explicitly passing in a set of
// bandwidth hints is used by the path finding algorithm to consider whether to
// use a local channel.
func TestPathSourceEdgesBandwidth(t *testing.T) {
	t.Parallel()

	graph, err := parseTestGraph(basicGraphFilePath)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer graph.cleanUp()

	sourceNode, err := graph.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}

	// First, we'll try to route from roasbeef -> sophon. This should
	// succeed without issue, and return a path via songoku, as that's the
	// cheapest path.
	target := graph.aliasMap["sophon"]
	payAmt := lnwire.NewMSatFromSatoshis(50000)
	path, err := findPath(
		&graphParams{
			graph: graph.graph,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "sophon")

	// Now we'll set the bandwidth of the edge roasbeef->songoku and
	// roasbeef->phamnuwen to 0.
	roasToSongoku := uint64(12345)
	roasToPham := uint64(999991)
	bandwidths := map[uint64]lnwire.MilliSatoshi{
		roasToSongoku: 0,
		roasToPham:    0,
	}

	// Since both these edges has a bandwidth of zero, no path should be
	// found.
	_, err = findPath(
		&graphParams{
			graph:          graph.graph,
			bandwidthHints: bandwidths,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if !IsError(err, ErrNoPathFound) {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}

	// Set the bandwidth of roasbeef->phamnuwen high enough to carry the
	// payment.
	bandwidths[roasToPham] = 2 * payAmt

	// Now, if we attempt to route again, we should find the path via
	// phamnuven, as the other source edge won't be considered.
	path, err = findPath(
		&graphParams{
			graph:          graph.graph,
			bandwidthHints: bandwidths,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	assertExpectedPath(t, graph.aliasMap, path, "phamnuwen", "sophon")

	// Finally, set the roasbeef->songoku bandwidth, but also set its
	// disable flag.
	bandwidths[roasToSongoku] = 2 * payAmt
	_, e1, e2, err := graph.graph.FetchChannelEdgesByID(roasToSongoku)
	if err != nil {
		t.Fatalf("unable to fetch edge: %v", err)
	}
	e1.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.graph.UpdateEdgePolicy(e1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	e2.ChannelFlags |= lnwire.ChanUpdateDisabled
	if err := graph.graph.UpdateEdgePolicy(e2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// Since we ignore disable flags for local channels, a path should
	// still be found.
	path, err = findPath(
		&graphParams{
			graph:          graph.graph,
			bandwidthHints: bandwidths,
		},
		noRestrictions,
		sourceNode.PubKeyBytes, target, payAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "sophon")
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
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}
	defer cleanUp()

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
	bobKey, err := btcec.ParsePubKey(bob[:], btcec.S256())
	if err != nil {
		t.Fatal(err)
	}
	bobNode, err := ctx.graph.FetchLightningNode(bobKey)
	if err != nil {
		t.Fatalf("unable to find bob: %v", err)
	}
	if err := ctx.graph.SetSourceNode(bobNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// Query for a route of 4,999,999 mSAT to carol.
	carol := ctx.aliases["C"]
	const amt lnwire.MilliSatoshi = 4999999
	route, err := ctx.router.FindRoute(
		bobNode.PubKeyBytes, carol, amt, noRestrictions,
	)
	if err != nil {
		t.Fatalf("unable to find route: %v", err)
	}

	// Now we'll examine the route returned for correctness.
	//
	// It should be sending the exact payment amount as there are no
	// additional hops.
	if route.TotalAmount != amt {
		t.Fatalf("wrong total amount: got %v, expected %v",
			route.TotalAmount, amt)
	}
	if route.Hops[0].AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			route.Hops[0].AmtToForward, amt)
	}

	fee := route.HopFee(0)
	if fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 0)
	}

	// The CLTV expiry should be the current height plus 9 (the expiry for
	// the B -> C channel.
	if route.TotalTimeLock !=
		startingHeight+zpay32.DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			route.TotalTimeLock,
			startingHeight+zpay32.DefaultFinalCLTVDelta)
	}

	// Next, we'll set A as the source node so we can assert that we create
	// the proper route for any queries starting with Alice.
	alice := ctx.aliases["A"]
	aliceKey, err := btcec.ParsePubKey(alice[:], btcec.S256())
	if err != nil {
		t.Fatal(err)
	}
	aliceNode, err := ctx.graph.FetchLightningNode(aliceKey)
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
	if source.PubKeyBytes != alice {
		t.Fatalf("source node not set")
	}

	// We'll now request a route from A -> B -> C.
	route, err = ctx.router.FindRoute(
		source.PubKeyBytes, carol, amt, noRestrictions,
	)
	if err != nil {
		t.Fatalf("unable to find routes: %v", err)
	}

	// The route should be two hops.
	if len(route.Hops) != 2 {
		t.Fatalf("route should be %v hops, is instead %v", 2,
			len(route.Hops))
	}

	// The total amount should factor in a fee of 10199 and also use a CLTV
	// delta total of 29 (20 + 9),
	expectedAmt := lnwire.MilliSatoshi(5010198)
	if route.TotalAmount != expectedAmt {
		t.Fatalf("wrong amount: got %v, expected %v",
			route.TotalAmount, expectedAmt)
	}
	if route.TotalTimeLock != startingHeight+29 {
		t.Fatalf("wrong total time lock: got %v, expecting %v",
			route.TotalTimeLock, startingHeight+29)
	}

	// Ensure that the hops of the route are properly crafted.
	//
	// After taking the fee, Bob should be forwarding the remainder which
	// is the exact payment to Bob.
	if route.Hops[0].AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			route.Hops[0].AmtToForward, amt)
	}

	// We shouldn't pay any fee for the first, hop, but the fee for the
	// second hop posted fee should be exactly:

	// The fee that we pay for the second hop will be "applied to the first
	// hop, so we should get a fee of exactly:
	//
	//  * 200 + 4999999 * 2000 / 1000000 = 10199

	fee = route.HopFee(0)
	if fee != 10199 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 10199)
	}

	// While for the final hop, as there's no additional hop afterwards, we
	// pay no fee.
	fee = route.HopFee(1)
	if fee != 0 {
		t.Fatalf("wrong hop fee: got %v, expected %v", fee, 0)
	}

	// The outgoing CLTV value itself should be the current height plus 30
	// to meet Carol's requirements.
	if route.Hops[0].OutgoingTimeLock !=
		startingHeight+zpay32.DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			route.Hops[0].OutgoingTimeLock,
			startingHeight+zpay32.DefaultFinalCLTVDelta)
	}

	// For B -> C, we assert that the final hop also has the proper
	// parameters.
	lastHop := route.Hops[1]
	if lastHop.AmtToForward != amt {
		t.Fatalf("wrong forward amount: got %v, expected %v",
			lastHop.AmtToForward, amt)
	}
	if lastHop.OutgoingTimeLock !=
		startingHeight+zpay32.DefaultFinalCLTVDelta {

		t.Fatalf("wrong total time lock: got %v, expecting %v",
			lastHop.OutgoingTimeLock,
			startingHeight+zpay32.DefaultFinalCLTVDelta)
	}
}

func assertExpectedPath(t *testing.T, aliasMap map[string]route.Vertex,
	path []*channeldb.ChannelEdgePolicy, nodeAliases ...string) {

	if len(path) != len(nodeAliases) {
		t.Fatal("number of hops and number of aliases do not match")
	}

	for i, hop := range path {
		if hop.Node.PubKeyBytes != aliasMap[nodeAliases[i]] {
			t.Fatalf("expected %v to be pos #%v in hop, instead "+
				"%v was", nodeAliases[i], i, hop.Node.Alias)
		}
	}
}

// TestNewRouteFromEmptyHops tests that the NewRouteFromHops function returns an
// error when the hop list is empty.
func TestNewRouteFromEmptyHops(t *testing.T) {
	t.Parallel()

	var source route.Vertex
	_, err := route.NewRouteFromHops(0, 0, source, []*route.Hop{})
	if err != route.ErrNoRouteHopsProvided {
		t.Fatalf("expected empty hops error: instead got: %v", err)
	}
}

// TestRestrictOutgoingChannel asserts that a outgoing channel restriction is
// obeyed by the path finding algorithm.
func TestRestrictOutgoingChannel(t *testing.T) {
	t.Parallel()

	// Set up a test graph with three possible paths from roasbeef to
	// target. The path through channel 2 is the highest cost path.
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "a", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}, 1),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 800,
			MinHTLC: 1,
		}, 2),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 600,
			MinHTLC: 1,
		}, 3),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
		}),
	}

	testGraphInstance, err := createTestGraphFromChannels(
		testChannels, "roasbeef",
	)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraphInstance.cleanUp()

	sourceNode, err := testGraphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := testGraphInstance.aliasMap["target"]
	outgoingChannelID := uint64(2)

	// Find the best path given the restriction to only use channel 2 as the
	// outgoing channel.
	path, err := findPath(
		&graphParams{
			graph: testGraphInstance.graph,
		},
		&RestrictParams{
			FeeLimit:          noFeeLimit,
			OutgoingChannelID: &outgoingChannelID,
			ProbabilitySource: noProbabilitySource,
		},
		sourceVertex, target, paymentAmt,
	)
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}
	route, err := newRoute(
		paymentAmt, sourceVertex, path, startingHeight,
		finalHopCLTV,
	)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// Assert that the route starts with channel 2, in line with the
	// specified restriction.
	if route.Hops[0].ChannelID != 2 {
		t.Fatalf("expected route to pass through channel 2, "+
			"but channel %v was selected instead", route.Hops[0].ChannelID)
	}
}

// TestCltvLimit asserts that a cltv limit is obeyed by the path finding
// algorithm.
func TestCltvLimit(t *testing.T) {
	t.Run("no limit", func(t *testing.T) { testCltvLimit(t, 0, 1) })
	t.Run("no path", func(t *testing.T) { testCltvLimit(t, 50, 0) })
	t.Run("force high cost", func(t *testing.T) { testCltvLimit(t, 80, 3) })
}

func testCltvLimit(t *testing.T, limit uint32, expectedChannel uint64) {
	t.Parallel()

	// Set up a test graph with three possible paths to the target. The path
	// through a is the lowest cost with a high time lock (144). The path
	// through b has a higher cost but a lower time lock (100). That path
	// through c and d (two hops) has the same case as the path through b,
	// but the total time lock is lower (60).
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "a", 100000, &testChannelPolicy{}, 1),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 10000,
			MinHTLC:     1,
		}),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{}, 2),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:      100,
			FeeBaseMsat: 20000,
			MinHTLC:     1,
		}),
		symmetricTestChannel("roasbeef", "c", 100000, &testChannelPolicy{}, 3),
		symmetricTestChannel("c", "d", 100000, &testChannelPolicy{
			Expiry:      30,
			FeeBaseMsat: 10000,
			MinHTLC:     1,
		}),
		symmetricTestChannel("d", "target", 100000, &testChannelPolicy{
			Expiry:      30,
			FeeBaseMsat: 10000,
			MinHTLC:     1,
		}),
	}

	testGraphInstance, err := createTestGraphFromChannels(
		testChannels, "roasbeef",
	)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraphInstance.cleanUp()

	sourceNode, err := testGraphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := testGraphInstance.aliasMap["target"]

	// Find the best path given the cltv limit.
	var cltvLimit *uint32
	if limit != 0 {
		cltvLimit = &limit
	}

	path, err := findPath(
		&graphParams{
			graph: testGraphInstance.graph,
		},
		&RestrictParams{
			FeeLimit:          noFeeLimit,
			CltvLimit:         cltvLimit,
			ProbabilitySource: noProbabilitySource,
		},
		sourceVertex, target, paymentAmt,
	)
	if expectedChannel == 0 {
		// Finish test if we expect no route.
		if IsError(err, ErrNoPathFound) {
			return
		}
		t.Fatal("expected no path to be found")
	}
	if err != nil {
		t.Fatalf("unable to find path: %v", err)
	}

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)
	route, err := newRoute(
		paymentAmt, sourceVertex, path, startingHeight, finalHopCLTV,
	)
	if err != nil {
		t.Fatalf("unable to create path: %v", err)
	}

	// Assert that the route starts with the expected channel.
	if route.Hops[0].ChannelID != expectedChannel {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", expectedChannel,
			route.Hops[0].ChannelID)
	}
}

// TestProbabilityRouting asserts that path finding not only takes into account
// fees but also success probability.
func TestProbabilityRouting(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		p10, p11, p20  float64
		minProbability float64
		expectedChan   uint64
	}{
		// Test two variations with probabilities that should multiply
		// to the same total route probability. In both cases the three
		// hop route should be the best route. The three hop route has a
		// probability of 0.5 * 0.8 = 0.4. The fee is 5 (chan 10) + 8
		// (chan 11) = 13. Path finding distance should work out to: 13
		// + 10 (attempt penalty) / 0.4 = 38. The two hop route is 25 +
		// 10 / 0.7 = 39.
		{
			name: "three hop 1",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   10,
		},
		{
			name: "three hop 2",
			p10:  0.5, p11: 0.8, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   10,
		},

		// If the probability of the two hop route is increased, its
		// distance becomes 25 + 10 / 0.85 = 37. This is less than the
		// three hop route with its distance 38. So with an attempt
		// penalty of 10, the higher fee route is chosen because of the
		// compensation for success probability.
		{
			name: "two hop higher cost",
			p10:  0.5, p11: 0.8, p20: 0.85,
			minProbability: 0.1,
			expectedChan:   20,
		},

		// If the same probabilities are used with a probability lower bound of
		// 0.5, we expect the three hop route with probability 0.4 to be
		// excluded and the two hop route to be picked.
		{
			name: "probability limit",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.5,
			expectedChan:   20,
		},

		// With a probability limit above the probability of both routes, we
		// expect no route to be returned. This expectation is signaled by using
		// expected channel 0.
		{
			name: "probability limit no routes",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.8,
			expectedChan:   0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testProbabilityRouting(
				t, tc.p10, tc.p11, tc.p20,
				tc.minProbability, tc.expectedChan,
			)
		})
	}
}

func testProbabilityRouting(t *testing.T, p10, p11, p20, minProbability float64,
	expectedChan uint64) {

	t.Parallel()

	// Set up a test graph with two possible paths to the target: a three
	// hop path (via channels 10 and 11) and a two hop path (via channel
	// 20).
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "a1", 100000, &testChannelPolicy{}),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{}),
		symmetricTestChannel("a1", "a2", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: lnwire.NewMSatFromSatoshis(5),
			MinHTLC:     1,
		}, 10),
		symmetricTestChannel("a2", "target", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: lnwire.NewMSatFromSatoshis(8),
			MinHTLC:     1,
		}, 11),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:      100,
			FeeBaseMsat: lnwire.NewMSatFromSatoshis(25),
			MinHTLC:     1,
		}, 20),
	}

	testGraphInstance, err := createTestGraphFromChannels(
		testChannels, "roasbeef",
	)
	if err != nil {
		t.Fatalf("unable to create graph: %v", err)
	}
	defer testGraphInstance.cleanUp()

	sourceNode, err := testGraphInstance.graph.SourceNode()
	if err != nil {
		t.Fatalf("unable to fetch source node: %v", err)
	}
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := testGraphInstance.aliasMap["target"]

	// Configure a probability source with the test parameters.
	probabilitySource := func(node route.Vertex, edge EdgeLocator,
		amt lnwire.MilliSatoshi) float64 {

		if amt == 0 {
			t.Fatal("expected non-zero amount")
		}

		switch edge.ChannelID {
		case 10:
			return p10
		case 11:
			return p11
		case 20:
			return p20
		default:
			return 1
		}
	}

	path, err := findPath(
		&graphParams{
			graph: testGraphInstance.graph,
		},
		&RestrictParams{
			FeeLimit:              noFeeLimit,
			ProbabilitySource:     probabilitySource,
			PaymentAttemptPenalty: lnwire.NewMSatFromSatoshis(10),
			MinProbability:        minProbability,
		},
		sourceVertex, target, paymentAmt,
	)
	if expectedChan == 0 {
		if err == nil || !IsError(err, ErrNoPathFound) {
			t.Fatalf("expected no path found, but got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}

	// Assert that the route passes through the expected channel.
	if path[1].ChannelID != expectedChan {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", expectedChan,
			path[1].ChannelID)
	}
}
