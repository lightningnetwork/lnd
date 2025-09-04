package routing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	switchhop "github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const (
	// basicGraphFilePath is the file path for a basic graph used within
	// the tests. The basic graph consists of 5 nodes with 5 channels
	// connecting them.
	basicGraphFilePath = "testdata/basic_graph.json"

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
		CltvLimit:         math.MaxUint32,
	}

	testPathFindingConfig = &PathFindingConfig{}

	tlvFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
		), lnwire.Features,
	)

	payAddrFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrOptional,
		), lnwire.Features,
	)

	tlvPayAddrFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrOptional,
		), lnwire.Features,
	)

	mppFeatures = lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadRequired,
		lnwire.PaymentAddrOptional,
		lnwire.MPPOptional,
	)

	unknownRequiredFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(100), lnwire.Features,
	)
)

var (
	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar   = new(btcec.ModNScalar)
	testSScalar   = new(btcec.ModNScalar)
	_             = testRScalar.SetByteSlice(testRBytes)
	_             = testSScalar.SetByteSlice(testSBytes)
	testSig       = ecdsa.NewSignature(testRScalar, testSScalar)

	testAuthProof = models.ChannelAuthProof{
		NodeSig1Bytes:    testSig.Serialize(),
		NodeSig2Bytes:    testSig.Serialize(),
		BitcoinSig1Bytes: testSig.Serialize(),
		BitcoinSig2Bytes: testSig.Serialize(),
	}
)

// noProbabilitySource is used in testing to return the same probability 1 for
// all edges.
func noProbabilitySource(route.Vertex, route.Vertex, lnwire.MilliSatoshi,
	btcutil.Amount) float64 {

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
// for our tests. Private keys are optional. If set, they should be consistent
// with the public key. The private key is used to sign error messages
// sent from the node.
type testNode struct {
	Source  bool   `json:"source"`
	PubKey  string `json:"pubkey"`
	PrivKey string `json:"privkey"`
	Alias   string `json:"alias"`
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
// purposes.
func makeTestGraph(t *testing.T, useCache bool) (*graphdb.ChannelGraph,
	kvdb.Backend, error) {

	// Create channelgraph for the first time.
	graph := graphdb.MakeTestGraph(
		t, graphdb.WithUseGraphCache(useCache),
	)
	require.NoError(t, graph.Start())
	t.Cleanup(func() {
		require.NoError(t, graph.Stop())
	})

	mcBackend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "mission_control",
	)
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(backendCleanup)

	return graph, mcBackend, nil
}

// parseTestGraph returns a fully populated ChannelGraph given a path to a JSON
// file which encodes a test graph.
func parseTestGraph(t *testing.T, useCache bool, path string) (
	*testGraphInstance, error) {

	ctx := t.Context()

	graphJSON, err := os.ReadFile(path)
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
	graph, mcBackend, err := makeTestGraph(t, useCache)
	if err != nil {
		return nil, err
	}

	aliasMap := make(map[string]route.Vertex)
	privKeyMap := make(map[string]*btcec.PrivateKey)
	channelIDs := make(map[route.Vertex]map[route.Vertex]uint64)
	links := make(map[lnwire.ShortChannelID]htlcswitch.ChannelLink)
	var source *models.Node

	// First we insert all the nodes within the graph as vertexes.
	for _, node := range g.Nodes {
		pubBytes, err := hex.DecodeString(node.PubKey)
		if err != nil {
			return nil, err
		}

		pubKey, err := route.NewVertexFromBytes(pubBytes)
		require.NoError(t, err)

		dbNode := models.NewV1Node(pubKey, &models.NodeV1Fields{
			AuthSigBytes: testSig.Serialize(),
			LastUpdate:   testTime,
			Addresses:    testAddrs,
			Alias:        node.Alias,
			Features:     testFeatures.RawFeatureVector,
		})

		// We require all aliases within the graph to be unique for our
		// tests.
		if _, ok := aliasMap[node.Alias]; ok {
			return nil, errors.New("aliases for nodes " +
				"must be unique!")
		}

		// If the alias is unique, then add the node to the
		// alias map for easy lookup.
		aliasMap[node.Alias] = dbNode.PubKeyBytes

		// private keys are needed for signing error messages. If set
		// check the consistency with the public key.
		privBytes, err := hex.DecodeString(node.PrivKey)
		if err != nil {
			return nil, err
		}
		if len(privBytes) > 0 {
			key, derivedPub := btcec.PrivKeyFromBytes(
				privBytes,
			)

			if !bytes.Equal(
				pubBytes, derivedPub.SerializeCompressed(),
			) {

				return nil, fmt.Errorf("%s public key and "+
					"private key are inconsistent\n"+
					"got  %x\nwant %x\n",
					node.Alias,
					derivedPub.SerializeCompressed(),
					pubBytes,
				)
			}

			privKeyMap[node.Alias] = key
		}

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
					"multiple nodes are tagged as the " +
					"source")
			}

			source = dbNode

			// If this is the source node, we don't have to call
			// AddNode below since we will call
			// SetSourceNode later.
			continue
		}

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddNode(ctx, dbNode); err != nil {
			return nil, err
		}
	}

	if source != nil {
		// Set the selected source node
		if err := graph.SetSourceNode(ctx, source); err != nil {
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
		edgeInfo := models.ChannelEdgeInfo{
			ChannelID:    edge.ChannelID,
			AuthProof:    &testAuthProof,
			ChannelPoint: fundingPoint,
			Features:     lnwire.EmptyFeatureVector(),
			Capacity:     btcutil.Amount(edge.Capacity),
		}

		copy(edgeInfo.NodeKey1Bytes[:], node1Bytes)
		copy(edgeInfo.NodeKey2Bytes[:], node2Bytes)
		copy(edgeInfo.BitcoinKey1Bytes[:], node1Bytes)
		copy(edgeInfo.BitcoinKey2Bytes[:], node2Bytes)

		shortID := lnwire.NewShortChanIDFromInt(edge.ChannelID)
		links[shortID] = &mockLink{
			bandwidth: lnwire.MilliSatoshi(
				edgeInfo.Capacity * 1000,
			),
		}

		err = graph.AddChannelEdge(ctx, &edgeInfo)
		if err != nil && !errors.Is(err, graphdb.ErrEdgeAlreadyExist) {
			return nil, err
		}

		channelFlags := lnwire.ChanUpdateChanFlags(edge.ChannelFlags)
		isUpdate1 := channelFlags&lnwire.ChanUpdateDirection == 0
		targetNode := edgeInfo.NodeKey1Bytes
		if isUpdate1 {
			targetNode = edgeInfo.NodeKey2Bytes
		}

		edgePolicy := &models.ChannelEdgePolicy{
			SigBytes:                  testSig.Serialize(),
			MessageFlags:              lnwire.ChanUpdateMsgFlags(edge.MessageFlags),
			ChannelFlags:              channelFlags,
			ChannelID:                 edge.ChannelID,
			LastUpdate:                testTime,
			TimeLockDelta:             edge.Expiry,
			MinHTLC:                   lnwire.MilliSatoshi(edge.MinHTLC),
			MaxHTLC:                   lnwire.MilliSatoshi(edge.MaxHTLC),
			FeeBaseMSat:               lnwire.MilliSatoshi(edge.FeeBaseMsat),
			FeeProportionalMillionths: lnwire.MilliSatoshi(edge.FeeRate),
			ToNode:                    targetNode,
		}
		if err := graph.UpdateEdgePolicy(ctx, edgePolicy); err != nil {
			return nil, err
		}

		// We also store the channel IDs info for each of the node.
		node1Vertex, err := route.NewVertexFromBytes(node1Bytes)
		if err != nil {
			return nil, err
		}

		node2Vertex, err := route.NewVertexFromBytes(node2Bytes)
		if err != nil {
			return nil, err
		}

		if _, ok := channelIDs[node1Vertex]; !ok {
			channelIDs[node1Vertex] = map[route.Vertex]uint64{}
		}
		channelIDs[node1Vertex][node2Vertex] = edge.ChannelID

		if _, ok := channelIDs[node2Vertex]; !ok {
			channelIDs[node2Vertex] = map[route.Vertex]uint64{}
		}
		channelIDs[node2Vertex][node1Vertex] = edge.ChannelID
	}

	return &testGraphInstance{
		graph:      graph,
		mcBackend:  mcBackend,
		aliasMap:   aliasMap,
		privKeyMap: privKeyMap,
		channelIDs: channelIDs,
		links:      links,
	}, nil
}

type testChannelPolicy struct {
	Expiry             uint16
	MinHTLC            lnwire.MilliSatoshi
	MaxHTLC            lnwire.MilliSatoshi
	FeeBaseMsat        lnwire.MilliSatoshi
	FeeRate            lnwire.MilliSatoshi
	InboundFeeBaseMsat int64
	InboundFeeRate     int64
	LastUpdate         time.Time
	Disabled           bool
	Features           *lnwire.FeatureVector
}

type testChannelEnd struct {
	Alias string
	*testChannelPolicy
}

func symmetricTestChannel(alias1, alias2 string, capacity btcutil.Amount,
	policy *testChannelPolicy, chanID ...uint64) *testChannel {

	// Leaving id zero will result in auto-generation of a channel id during
	// graph construction.
	var id uint64
	if len(chanID) > 0 {
		id = chanID[0]
	}

	policy2 := *policy

	return asymmetricTestChannel(
		alias1, alias2, capacity, policy, &policy2, id,
	)
}

func asymmetricTestChannel(alias1, alias2 string, capacity btcutil.Amount,
	policy1, policy2 *testChannelPolicy, id uint64) *testChannel {

	return &testChannel{
		Capacity: capacity,
		Node1: &testChannelEnd{
			Alias:             alias1,
			testChannelPolicy: policy1,
		},
		Node2: &testChannelEnd{
			Alias:             alias2,
			testChannelPolicy: policy2,
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
	graph     *graphdb.ChannelGraph
	mcBackend kvdb.Backend

	// aliasMap is a map from a node's alias to its public key. This type is
	// provided in order to allow easily look up from the human memorable alias
	// to an exact node's public key.
	aliasMap map[string]route.Vertex

	// privKeyMap maps a node alias to its private key. This is used to be
	// able to mock a remote node's signing behaviour.
	privKeyMap map[string]*btcec.PrivateKey

	// channelIDs stores the channel ID for each node.
	channelIDs map[route.Vertex]map[route.Vertex]uint64

	// links maps channel ids to a mock channel update handler.
	links map[lnwire.ShortChannelID]htlcswitch.ChannelLink
}

// getLink is a mocked link lookup function which looks up links in our test
// graph.
func (g *testGraphInstance) getLink(chanID lnwire.ShortChannelID) (
	htlcswitch.ChannelLink, error) {

	link, ok := g.links[chanID]
	if !ok {
		return nil, fmt.Errorf("link not found in mock: %v", chanID)
	}

	return link, nil
}

// createTestGraphFromChannels returns a fully populated ChannelGraph based on a set of
// test channels. Additional required information like keys are derived in
// a deterministic way and added to the channel graph. A list of nodes is
// not required and derived from the channel data. The goal is to keep
// instantiating a test channel graph as light weight as possible.
func createTestGraphFromChannels(t *testing.T, useCache bool,
	testChannels []*testChannel, source string,
	sourceFeatureBits ...lnwire.FeatureBit) (*testGraphInstance, error) {

	ctx := t.Context()

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
	graph, graphBackend, err := makeTestGraph(t, useCache)
	if err != nil {
		return nil, err
	}

	aliasMap := make(map[string]route.Vertex)
	privKeyMap := make(map[string]*btcec.PrivateKey)
	channelIDs := make(map[route.Vertex]map[route.Vertex]uint64)

	nodeIndex := byte(0)
	addNodeWithAlias := func(alias string,
		features *lnwire.FeatureVector) error {

		keyBytes := []byte{
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, nodeIndex + 1,
		}

		privKey, pubKey := btcec.PrivKeyFromBytes(keyBytes)

		if features == nil {
			features = lnwire.EmptyFeatureVector()
		}

		dbNode := models.NewV1Node(
			route.NewVertex(pubKey), &models.NodeV1Fields{
				AuthSigBytes: testSig.Serialize(),
				LastUpdate:   testTime,
				Addresses:    testAddrs,
				Alias:        alias,
				Features:     features.RawFeatureVector,
			},
		)

		privKeyMap[alias] = privKey

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if alias == source {
			err = graph.SetSourceNode(ctx, dbNode)
			require.NoError(t, err)
		} else {
			err := graph.AddNode(ctx, dbNode)
			require.NoError(t, err)
		}

		aliasMap[alias] = dbNode.PubKeyBytes
		nodeIndex++

		return nil
	}

	// Add the source node.
	err = addNodeWithAlias(
		source, lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(sourceFeatureBits...),
			lnwire.Features,
		),
	)
	require.NoError(t, err)

	// Initialize variable that keeps track of the next channel id to assign
	// if none is specified.
	nextUnassignedChannelID := uint64(100000)

	links := make(map[lnwire.ShortChannelID]htlcswitch.ChannelLink)

	for _, testChannel := range testChannels {
		for _, node := range []*testChannelEnd{
			testChannel.Node1, testChannel.Node2,
		} {
			_, exists := aliasMap[node.Alias]
			if !exists {
				var features *lnwire.FeatureVector
				if node.testChannelPolicy != nil {
					features =
						node.testChannelPolicy.Features
				}
				err := addNodeWithAlias(
					node.Alias, features,
				)
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

		capacity := lnwire.MilliSatoshi(testChannel.Capacity * 1000)
		shortID := lnwire.NewShortChanIDFromInt(channelID)
		links[shortID] = &mockLink{
			bandwidth: capacity,
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

		if _, ok := channelIDs[node1Vertex]; !ok {
			channelIDs[node1Vertex] = map[route.Vertex]uint64{}
		}
		channelIDs[node1Vertex][node2Vertex] = channelID

		if _, ok := channelIDs[node2Vertex]; !ok {
			channelIDs[node2Vertex] = map[route.Vertex]uint64{}
		}
		channelIDs[node2Vertex][node1Vertex] = channelID

		// We first insert the existence of the edge between the two
		// nodes.
		edgeInfo := models.ChannelEdgeInfo{
			ChannelID:    channelID,
			AuthProof:    &testAuthProof,
			ChannelPoint: *fundingPoint,
			Capacity:     testChannel.Capacity,
			Features:     lnwire.EmptyFeatureVector(),

			NodeKey1Bytes:    node1Vertex,
			BitcoinKey1Bytes: node1Vertex,
			NodeKey2Bytes:    node2Vertex,
			BitcoinKey2Bytes: node2Vertex,
		}

		err = graph.AddChannelEdge(ctx, &edgeInfo)
		if err != nil && !errors.Is(err, graphdb.ErrEdgeAlreadyExist) {
			return nil, err
		}

		getInboundFees := func(
			end *testChannelEnd) fn.Option[lnwire.Fee] {

			inboundFee := lnwire.Fee{
				BaseFee: int32(end.InboundFeeBaseMsat),
				FeeRate: int32(end.InboundFeeRate),
			}

			return fn.Some(inboundFee)
		}
		getExtraData := func(
			end *testChannelEnd) lnwire.ExtraOpaqueData {

			var extraData lnwire.ExtraOpaqueData

			inboundFee := getInboundFees(end)
			inboundFee.WhenSome(func(fee lnwire.Fee) {
				require.NoError(t, extraData.PackRecords(&fee))
			})

			return extraData
		}

		if node1.testChannelPolicy != nil {
			var msgFlags lnwire.ChanUpdateMsgFlags
			if node1.MaxHTLC != 0 {
				msgFlags |= lnwire.ChanUpdateRequiredMaxHtlc
			}
			var channelFlags lnwire.ChanUpdateChanFlags
			if node1.Disabled {
				channelFlags |= lnwire.ChanUpdateDisabled
			}

			edgePolicy := &models.ChannelEdgePolicy{
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
				ToNode:                    node2Vertex,
				InboundFee:                getInboundFees(node1), //nolint:ll
				ExtraOpaqueData:           getExtraData(node1),
			}
			err := graph.UpdateEdgePolicy(ctx, edgePolicy)
			if err != nil {
				return nil, err
			}
		}

		if node2.testChannelPolicy != nil {
			var msgFlags lnwire.ChanUpdateMsgFlags
			if node2.MaxHTLC != 0 {
				msgFlags |= lnwire.ChanUpdateRequiredMaxHtlc
			}
			var channelFlags lnwire.ChanUpdateChanFlags
			if node2.Disabled {
				channelFlags |= lnwire.ChanUpdateDisabled
			}
			channelFlags |= lnwire.ChanUpdateDirection

			edgePolicy := &models.ChannelEdgePolicy{
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
				ToNode:                    node1Vertex,
				InboundFee:                getInboundFees(node2), //nolint:ll
				ExtraOpaqueData:           getExtraData(node2),
			}
			err := graph.UpdateEdgePolicy(ctx, edgePolicy)
			if err != nil {
				return nil, err
			}
		}

		channelID++
	}

	return &testGraphInstance{
		graph:      graph,
		mcBackend:  graphBackend,
		aliasMap:   aliasMap,
		privKeyMap: privKeyMap,
		channelIDs: channelIDs,
		links:      links,
	}, nil
}

// TestPathFinding tests all path finding related cases both with the in-memory
// graph cached turned on and off.
func TestPathFinding(t *testing.T) {
	testCases := []struct {
		name string
		fn   func(t *testing.T, useCache bool)
	}{{
		name: "lowest fee path",
		fn:   runFindLowestFeePath,
	}, {
		name: "basic graph path finding",
		fn:   runBasicGraphPathFinding,
	}, {
		name: "path finding with additional edges",
		fn:   runPathFindingWithAdditionalEdges,
	}, {
		name: "path finding with duplicate blinded hop",
		fn:   runPathFindingWithBlindedPathDuplicateHop,
	}, {
		name: "path finding with redundant additional edges",
		fn:   runPathFindingWithRedundantAdditionalEdges,
	}, {
		name: "new route path too long",
		fn:   runNewRoutePathTooLong,
	}, {
		name: "path not available",
		fn:   runPathNotAvailable,
	}, {
		name: "destination tlv graph fallback",
		fn:   runDestTLVGraphFallback,
	}, {
		name: "missing feature dependency",
		fn:   runMissingFeatureDep,
	}, {
		name: "unknown required features",
		fn:   runUnknownRequiredFeatures,
	}, {
		name: "destination payment address",
		fn:   runDestPaymentAddr,
	}, {
		name: "path insufficient capacity",
		fn:   runPathInsufficientCapacity,
	}, {
		name: "route fail min HTLC",
		fn:   runRouteFailMinHTLC,
	}, {
		name: "route fail max HTLC",
		fn:   runRouteFailMaxHTLC,
	}, {
		name: "route fail disabled edge",
		fn:   runRouteFailDisabledEdge,
	}, {
		name: "path source edges bandwidth",
		fn:   runPathSourceEdgesBandwidth,
	}, {
		name: "restrict outgoing channel",
		fn:   runRestrictOutgoingChannel,
	}, {
		name: "restrict last hop",
		fn:   runRestrictLastHop,
	}, {
		name: "CLTV limit",
		fn:   runCltvLimit,
	}, {
		name: "probability routing",
		fn:   runProbabilityRouting,
	}, {
		name: "equal cost route selection",
		fn:   runEqualCostRouteSelection,
	}, {
		name: "no cycle",
		fn:   runNoCycle,
	}, {
		name: "route to self",
		fn:   runRouteToSelf,
	}, {
		name: "with metadata",
		fn:   runFindPathWithMetadata,
	}, {
		name: "inbound fees",
		fn:   runInboundFees,
	}}

	// Run with graph cache enabled.
	for _, tc := range testCases {
		tc := tc

		t.Run("cache=true/"+tc.name, func(tt *testing.T) {
			tt.Parallel()

			tc.fn(tt, true)
		})
	}

	// And with the DB fallback to make sure everything works the same
	// still.
	for _, tc := range testCases {
		tc := tc

		t.Run("cache=false/"+tc.name, func(tt *testing.T) {
			tt.Parallel()

			tc.fn(tt, false)
		})
	}
}

// runFindPathWithMetadata tests that metadata is taken into account during
// pathfinding.
func runFindPathWithMetadata(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		symmetricTestChannel("alice", "bob", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "alice")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("bob")

	// Assert that a path is found when metadata is specified.
	ctx.restrictParams.Metadata = []byte{1, 2, 3}
	ctx.restrictParams.DestFeatures = tlvFeatures

	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err)
	require.Len(t, path, 1)

	// Assert that no path is found when metadata is too large.
	ctx.restrictParams.Metadata = make([]byte, 2000)

	_, err = ctx.findPath(target, paymentAmt)
	require.ErrorIs(t, errNoPathFound, err)
}

// runFindLowestFeePath tests that out of two routes with identical total
// time lock values, the route with the lowest total fee should be returned.
// The fee rates are chosen such that the test failed on the previous edge
// weight function where one of the terms was fee squared.
func runFindLowestFeePath(t *testing.T, useCache bool) {
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

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("target")
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	route, err := newRoute(
		ctx.source, path, startingHeight,
		finalHopParams{
			amt:       paymentAmt,
			cltvDelta: finalHopCLTV,
			records:   nil,
		}, nil,
	)
	require.NoError(t, err, "unable to create path")

	// Assert that the lowest fee route is returned.
	if route.Hops[1].PubKeyBytes != ctx.keyFromAlias("b") {
		t.Fatalf("expected route to pass through b, "+
			"but got a route through %v",
			ctx.aliasFromKey(route.Hops[1].PubKeyBytes))
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
	},
}

func runBasicGraphPathFinding(t *testing.T, useCache bool) {
	testGraphInstance, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

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

	ctx := t.Context()
	aliases := graphInstance.aliasMap
	expectedHops := test.expectedHops
	expectedHopCount := len(expectedHops)

	sourceNode, err := graphInstance.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")
	sourceVertex := route.Vertex(sourceNode.PubKeyBytes)

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(test.paymentAmt)
	target := graphInstance.aliasMap[test.target]
	path, err := dbFindPath(
		graphInstance.graph, nil, &mockBandwidthHints{},
		&RestrictParams{
			FeeLimit:          test.feeLimit,
			ProbabilitySource: noProbabilitySource,
			CltvLimit:         math.MaxUint32,
		},
		testPathFindingConfig,
		sourceNode.PubKeyBytes, target, paymentAmt, 0,
		startingHeight+finalHopCLTV,
	)
	if test.expectFailureNoPath {
		if err == nil {
			t.Fatal("expected no path to be found")
		}
		return
	}
	require.NoError(t, err, "unable to find path")

	route, err := newRoute(
		sourceVertex, path, startingHeight,
		finalHopParams{
			amt:       paymentAmt,
			cltvDelta: finalHopCLTV,
			records:   nil,
		}, nil,
	)
	require.NoError(t, err, "unable to create path")

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
	require.NoError(t, err, "unable to make sphinx path")
	if sphinxPath.TrueRouteLength() != expectedHopCount {
		t.Fatalf("incorrect number of hop payloads: expected %v, got %v",
			expectedHopCount, sphinxPath.TrueRouteLength())
	}

	// Hops should point to the next hop
	for i := 0; i < len(expectedHops)-1; i++ {
		payload, _, err := switchhop.ParseTLVPayload(
			bytes.NewReader(sphinxPath[i].HopPayload.Payload),
		)
		require.NoError(t, err)

		require.Equal(
			t, route.Hops[i+1].ChannelID,
			payload.FwdInfo.NextHop.ToUint64(),
		)
	}

	lastHopIndex := len(expectedHops) - 1

	payload, _, err := switchhop.ParseTLVPayload(
		bytes.NewReader(sphinxPath[lastHopIndex].HopPayload.Payload),
	)
	require.NoError(t, err)

	// The final hop should have a next hop value of all zeroes in order
	// to indicate it's the exit hop.
	require.Zero(t, payload.FwdInfo.NextHop.ToUint64())

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

// runPathFindingWithAdditionalEdges asserts that we are able to find paths to
// nodes that do not exist in the graph by way of hop hints. We also test that
// the path can support custom TLV records for the receiver under the
// appropriate circumstances.
func runPathFindingWithAdditionalEdges(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()

	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// In this test, we'll test that we're able to find paths through
	// private channels when providing them as additional edges in our path
	// finding algorithm. To do so, we'll create a new node, doge, and
	// create a private channel between it and songoku. We'll then attempt
	// to find a path from our source node, roasbeef, to doge.
	dogePubKeyHex := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	dogePubKeyBytes, err := hex.DecodeString(dogePubKeyHex)
	require.NoError(t, err, "unable to decode public key")

	pubKey, err := route.NewVertexFromBytes(dogePubKeyBytes)
	require.NoError(t, err)

	doge := models.NewV1Node(pubKey, &models.NodeV1Fields{
		Alias: "doge",
	})
	graph.aliasMap["doge"] = doge.PubKeyBytes

	// Create the channel edge going from songoku to doge and include it in
	// our map of additional edges.
	songokuToDogePolicy := &models.CachedEdgePolicy{
		ToNodePubKey: func() route.Vertex {
			return doge.PubKeyBytes
		},
		ToNodeFeatures:            lnwire.EmptyFeatureVector(),
		ChannelID:                 1337,
		FeeBaseMSat:               1,
		FeeProportionalMillionths: 1000,
		TimeLockDelta:             9,
	}

	additionalEdges := map[route.Vertex][]AdditionalEdge{
		graph.aliasMap["songoku"]: {&PrivateEdge{
			policy: songokuToDogePolicy,
		}},
	}

	find := func(r *RestrictParams) (
		[]*unifiedEdge, error) {

		return dbFindPath(
			graph.graph, additionalEdges, &mockBandwidthHints{},
			r, testPathFindingConfig,
			sourceNode.PubKeyBytes, doge.PubKeyBytes, paymentAmt,
			0, 0,
		)
	}

	// We should now be able to find a path from roasbeef to doge.
	path, err := find(noRestrictions)
	require.NoError(t, err, "unable to find private path to doge")

	// The path should represent the following hops:
	//	roasbeef -> songoku -> doge
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "doge")

	// Now, set custom records for the final hop. This should fail since no
	// dest features are set, and we won't have a node ann to fall back on.
	restrictions := *noRestrictions
	restrictions.DestCustomRecords = record.CustomSet{70000: []byte{}}

	// Finally, set the tlv feature in the payload and assert we found the
	// same path as before.
	restrictions.DestFeatures = tlvFeatures

	path, err = find(&restrictions)
	require.NoError(t, err, "path should have been found")
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "doge")
}

// runPathFindingWithBlindedPathDuplicateHop tests that in case a blinded path
// has duplicate hops that the path finding algorithm does not fail or behave
// incorrectly. This can happen because the creator of the blinded path can
// specify the same hop multiple times and this will only be detected at the
// forwarding nodes, so it is important that we can handle this case.
func runPathFindingWithBlindedPathDuplicateHop(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()

	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	songokuPubKeyBytes := graph.aliasMap["songoku"]
	songokuPubKey, err := btcec.ParsePubKey(songokuPubKeyBytes[:])
	require.NoError(t, err, "unable to parse public key from bytes")

	_, pkb1 := btcec.PrivKeyFromBytes([]byte{2})
	_, pkb2 := btcec.PrivKeyFromBytes([]byte{3})
	_, blindedPoint := btcec.PrivKeyFromBytes([]byte{5})

	sizeEncryptedData := 100
	cipherText := bytes.Repeat(
		[]byte{1}, sizeEncryptedData,
	)

	vb1 := route.NewVertex(pkb1)
	vb2 := route.NewVertex(pkb2)

	// Payments to blinded paths always pay to the NUMS target key.
	dummyTarget := route.NewVertex(&BlindedPathNUMSKey)

	graph.aliasMap["pkb1"] = vb1
	graph.aliasMap["pkb2"] = vb2
	graph.aliasMap["dummyTarget"] = dummyTarget

	// Create a blinded payment with duplicate hops and make sure the
	// path finding algorithm can cope with that. We add blinded hop 2
	// 3 times. The path finding algorithm should create a path with a
	// single hop to pkb2 (the first entry).
	blindedPayment := &BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: songokuPubKey,
			BlindingPoint:     blindedPoint,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					CipherText: cipherText,
				},
				{
					BlindedNodePub: pkb2,
					CipherText:     cipherText,
				},
				{
					BlindedNodePub: pkb1,
					CipherText:     cipherText,
				},
				{
					BlindedNodePub: pkb2,
					CipherText:     cipherText,
				},
				{
					BlindedNodePub: &BlindedPathNUMSKey,
					CipherText:     cipherText,
				},
				{
					BlindedNodePub: pkb2,
					CipherText:     cipherText,
				},
			},
		},
		HtlcMinimum:     1,
		HtlcMaximum:     100_000_000,
		CltvExpiryDelta: 140,
	}

	blindedPath, err := blindedPayment.toRouteHints()
	require.NoError(t, err)

	find := func(r *RestrictParams) (
		[]*unifiedEdge, error) {

		return dbFindPath(
			graph.graph, blindedPath, &mockBandwidthHints{},
			r, testPathFindingConfig,
			sourceNode.PubKeyBytes, dummyTarget, paymentAmt,
			0, 0,
		)
	}

	// We should now be able to find a path however not the chained path
	// of the blinded hops.
	path, err := find(noRestrictions)
	require.NoError(t, err, "unable to create route to blinded path")

	// The path should represent the following hops:
	//	source node -> songoku -> pkb2 -> dummyTarget
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "pkb2",
		"dummyTarget")
}

// runPathFindingWithRedundantAdditionalEdges asserts that we are able to find
// paths to nodes ignoring additional edges that are already known by self node.
func runPathFindingWithRedundantAdditionalEdges(t *testing.T, useCache bool) {
	t.Helper()

	var realChannelID uint64 = 3145
	var hintChannelID uint64 = 1618

	testChannels := []*testChannel{
		symmetricTestChannel("alice", "bob", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000000,
		}, realChannelID),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "alice")

	target := ctx.keyFromAlias("bob")
	paymentAmt := lnwire.NewMSatFromSatoshis(100)

	// Create the channel edge going from alice to bob and include it in
	// our map of additional edges.
	aliceToBobPolicy := &models.CachedEdgePolicy{
		ToNodePubKey: func() route.Vertex {
			return target
		},
		ToNodeFeatures:            lnwire.EmptyFeatureVector(),
		ChannelID:                 hintChannelID,
		FeeBaseMSat:               1,
		FeeProportionalMillionths: 1000,
		TimeLockDelta:             9,
	}

	additionalEdges := map[route.Vertex][]AdditionalEdge{
		ctx.source: {&PrivateEdge{
			policy: aliceToBobPolicy,
		}},
	}

	path, err := dbFindPath(
		ctx.graph, additionalEdges, ctx.bandwidthHints,
		&ctx.restrictParams, &ctx.pathFindingConfig, ctx.source, target,
		paymentAmt, ctx.timePref, 0,
	)

	require.NoError(t, err, "unable to find path to bob")
	require.Len(t, path, 1)

	require.Equal(t, realChannelID, path[0].policy.ChannelID,
		"additional edge for known edge wasn't ignored")
}

// TestNewRoute tests whether the construction of hop payloads by newRoute
// is executed correctly.
func TestNewRoute(t *testing.T) {
	var sourceKey [33]byte
	sourceVertex := route.Vertex(sourceKey)

	testPaymentAddr := [32]byte{0x01, 0x02, 0x03}

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	createHop := func(baseFee lnwire.MilliSatoshi,
		feeRate lnwire.MilliSatoshi,
		bandwidth lnwire.MilliSatoshi,
		timeLockDelta uint16) *models.CachedEdgePolicy {

		return &models.CachedEdgePolicy{
			ToNodePubKey: func() route.Vertex {
				return route.Vertex{}
			},
			ToNodeFeatures:            lnwire.NewFeatureVector(nil, nil),
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
		hops []*models.CachedEdgePolicy

		// paymentAmount is the amount that is send into the route
		// indicated by hops.
		paymentAmount lnwire.MilliSatoshi

		// destFeatures is a feature vector, that if non-nil, will
		// overwrite the final hop's feature vector in the graph.
		destFeatures *lnwire.FeatureVector

		paymentAddr fn.Option[[32]byte]

		// metadata is the payment metadata to attach to the route.
		metadata []byte

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

		expectedMPP *record.MPP
	}{
		{
			// For a single hop payment, no fees are expected to be paid.
			name:          "single hop",
			paymentAmount: 100000,
			hops: []*models.CachedEdgePolicy{
				createHop(100, 1000, 1000000, 10),
			},
			metadata:              []byte{1, 2, 3},
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
			hops: []*models.CachedEdgePolicy{
				createHop(0, 1000, 1000000, 10),
				createHop(30, 1000, 1000000, 5),
			},
			expectedFees:          []lnwire.MilliSatoshi{130, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalAmount:   100130,
			expectedTotalTimeLock: 6,
		}, {
			// For a two hop payment, only the fee for the first hop
			// needs to be paid. The destination hop does not require
			// a fee to receive the payment.
			name:          "two hop tlv onion feature",
			destFeatures:  tlvFeatures,
			paymentAmount: 100000,
			hops: []*models.CachedEdgePolicy{
				createHop(0, 1000, 1000000, 10),
				createHop(30, 1000, 1000000, 5),
			},
			expectedFees:          []lnwire.MilliSatoshi{130, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalAmount:   100130,
			expectedTotalTimeLock: 6,
		}, {
			// For a two hop payment, only the fee for the first hop
			// needs to be paid. The destination hop does not require
			// a fee to receive the payment.
			name:          "two hop single shot mpp",
			destFeatures:  tlvPayAddrFeatures,
			paymentAddr:   fn.Some(testPaymentAddr),
			paymentAmount: 100000,
			hops: []*models.CachedEdgePolicy{
				createHop(0, 1000, 1000000, 10),
				createHop(30, 1000, 1000000, 5),
			},
			expectedFees:          []lnwire.MilliSatoshi{130, 0},
			expectedTimeLocks:     []uint32{1, 1},
			expectedTotalAmount:   100130,
			expectedTotalTimeLock: 6,
			expectedMPP: record.NewMPP(
				100000, testPaymentAddr,
			),
		}, {
			// A three hop payment where the first and second hop
			// will both charge 1 msat. The fee for the first hop
			// is actually slightly higher than 1, because the amount
			// to forward also includes the fee for the second hop. This
			// gets rounded down to 1.
			name:          "three hop",
			paymentAmount: 100000,
			hops: []*models.CachedEdgePolicy{
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
			hops: []*models.CachedEdgePolicy{
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
			hops: []*models.CachedEdgePolicy{
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
		testCase := testCase

		// Overwrite the final hop's features if the test requires a
		// custom feature vector.
		if testCase.destFeatures != nil {
			finalHop := testCase.hops[len(testCase.hops)-1]
			finalHop.ToNodeFeatures = testCase.destFeatures
		}

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

			finalHop := route.Hops[len(route.Hops)-1]

			if !reflect.DeepEqual(
				finalHop.MPP, testCase.expectedMPP,
			) {

				t.Errorf("Expected final hop mpp field: %v, "+
					" but got: %v instead",
					testCase.expectedMPP, finalHop.MPP)
			}

			if !bytes.Equal(finalHop.Metadata, testCase.metadata) {
				t.Errorf("Expected final metadata field: %v, "+
					" but got: %v instead",
					testCase.metadata, finalHop.Metadata)
			}
		}

		t.Run(testCase.name, func(t *testing.T) {
			var unifiedHops []*unifiedEdge
			for _, hop := range testCase.hops {
				unifiedHops = append(unifiedHops,
					&unifiedEdge{
						policy: hop,
					},
				)
			}

			route, err := newRoute(
				sourceVertex, unifiedHops, startingHeight,
				finalHopParams{
					amt:         testCase.paymentAmount,
					totalAmt:    testCase.paymentAmount,
					cltvDelta:   finalHopCLTV,
					records:     nil,
					paymentAddr: testCase.paymentAddr,
					metadata:    testCase.metadata,
				}, nil,
			)
			require.NoError(t, err)

			assertRoute(t, route)
		})
	}
}

func runNewRoutePathTooLong(t *testing.T, useCache bool) {
	var testChannels []*testChannel

	// Setup a linear network of 26 hops.
	fromNode := "start"
	for i := 0; i < 26; i++ {
		toNode := fmt.Sprintf("node-%v", i+1)
		c := symmetricTestChannel(fromNode, toNode, 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
			MinHTLC: 1,
			MaxHTLC: 100000001,
		})
		testChannels = append(testChannels, c)

		fromNode = toNode
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "start")

	// Assert that we can find 25 hop routes.
	node25 := ctx.keyFromAlias("node-25")
	payAmt := lnwire.MilliSatoshi(100001)
	_, err := ctx.findPath(node25, payAmt)
	require.NoError(t, err, "unexpected pathfinding failure")

	// Assert that finding a 26 hop route fails.
	node26 := ctx.keyFromAlias("node-26")
	_, err = ctx.findPath(node26, payAmt)
	require.ErrorIs(t, err, errNoPathFound)

	// Assert that we can't find a 20 hop route if custom records make it
	// exceed the maximum payload size.
	ctx.restrictParams.DestFeatures = tlvFeatures
	ctx.restrictParams.DestCustomRecords = map[uint64][]byte{
		100000: bytes.Repeat([]byte{1}, 100),
	}
	_, err = ctx.findPath(node25, payAmt)
	if err != errNoPathFound {
		t.Fatalf("not route error expected, but got %v", err)
	}
}

func runPathNotAvailable(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()

	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	// With the test graph loaded, we'll test that queries for target that
	// are either unreachable within the graph, or unknown result in an
	// error.
	unknownNodeStr := "03dd46ff29a6941b4a2607525b043ec9b020b3f318a1bf281536fd7011ec59c882"
	unknownNodeBytes, err := hex.DecodeString(unknownNodeStr)
	require.NoError(t, err, "unable to parse bytes")
	var unknownNode route.Vertex
	copy(unknownNode[:], unknownNodeBytes)

	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, unknownNode, 100, 0, 0,
	)
	if err != errNoPathFound {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

// runDestTLVGraphFallback asserts that we properly detect when we can send TLV
// records to a receiver, and also that we fallback to the receiver's node
// announcement if we don't have an invoice features.
func runDestTLVGraphFallback(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		asymmetricTestChannel("roasbeef", "luoji", 100000,
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			}, &testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			}, 0),
		asymmetricTestChannel("roasbeef", "satoshi", 100000,
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			}, &testChannelPolicy{
				Expiry:   144,
				FeeRate:  400,
				MinHTLC:  1,
				MaxHTLC:  100000000,
				Features: tlvFeatures,
			}, 0),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	sourceNode, err := ctx.graph.SourceNode(t.Context())
	require.NoError(t, err, "unable to fetch source node")

	find := func(r *RestrictParams,
		target route.Vertex) ([]*unifiedEdge, error) {

		return dbFindPath(
			ctx.graph, nil, &mockBandwidthHints{},
			r, testPathFindingConfig,
			sourceNode.PubKeyBytes, target, 100, 0, 0,
		)
	}

	// Luoji's node ann has an empty feature vector.
	luoji := ctx.testGraphInstance.aliasMap["luoji"]

	// Satoshi's node ann supports TLV.
	satoshi := ctx.testGraphInstance.aliasMap["satoshi"]

	restrictions := *noRestrictions

	// Add custom records w/o any dest features.
	restrictions.DestCustomRecords = record.CustomSet{70000: []byte{}}

	// However, path to satoshi should succeed via the fallback because his
	// node ann features have the TLV bit.
	path, err := find(&restrictions, satoshi)
	require.NoError(t, err, "path should have been found")
	assertExpectedPath(t, ctx.testGraphInstance.aliasMap, path, "satoshi")

	// Finally, set the TLV dest feature. We should succeed in finding a
	// path to luoji.
	restrictions.DestFeatures = tlvFeatures

	path, err = find(&restrictions, luoji)
	require.NoError(t, err, "path should have been found")
	assertExpectedPath(t, ctx.testGraphInstance.aliasMap, path, "luoji")
}

// runMissingFeatureDep asserts that we fail path finding when the
// destination's features are broken, in that the feature vector doesn't signal
// all transitive dependencies.
func runMissingFeatureDep(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		asymmetricTestChannel("roasbeef", "conner", 100000,
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			},
			&testChannelPolicy{
				Expiry:   144,
				FeeRate:  400,
				MinHTLC:  1,
				MaxHTLC:  100000000,
				Features: payAddrFeatures,
			}, 0,
		),
		asymmetricTestChannel("conner", "joost", 100000,
			&testChannelPolicy{
				Expiry:   144,
				FeeRate:  400,
				MinHTLC:  1,
				MaxHTLC:  100000000,
				Features: payAddrFeatures,
			},
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			}, 0,
		),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	// Conner's node in the graph has a broken feature vector, since it
	// signals payment addresses without signaling tlv onions. Pathfinding
	// should fail since we validate transitive feature dependencies for the
	// final node.
	conner := ctx.keyFromAlias("conner")
	joost := ctx.keyFromAlias("joost")

	_, err := ctx.findPath(conner, 100)
	if err != errMissingDependentFeature {
		t.Fatalf("path shouldn't have been found: %v", err)
	}

	// Now, set the TLV and payment addresses features to override the
	// broken features found in the graph. We should succeed in finding a
	// path to conner.
	ctx.restrictParams.DestFeatures = tlvPayAddrFeatures

	path, err := ctx.findPath(conner, 100)
	require.NoError(t, err, "path should have been found")
	assertExpectedPath(t, ctx.testGraphInstance.aliasMap, path, "conner")

	// Finally, try to find a route to joost through conner. The
	// destination features are set properly from the previous assertions,
	// but conner's feature vector in the graph is still broken. We expect
	// errNoPathFound and not the missing feature dep err above since
	// intermediate hops are simply skipped if they have invalid feature
	// vectors, leaving no possible route to joost.
	_, err = ctx.findPath(joost, 100)
	if err != errNoPathFound {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

// runUnknownRequiredFeatures asserts that we fail path finding when the
// destination requires an unknown required feature, and that we skip
// intermediaries that signal unknown required features.
func runUnknownRequiredFeatures(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		asymmetricTestChannel("roasbeef", "conner", 100000,
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			},
			&testChannelPolicy{
				Expiry:   144,
				FeeRate:  400,
				MinHTLC:  1,
				MaxHTLC:  100000000,
				Features: unknownRequiredFeatures,
			}, 0,
		),
		asymmetricTestChannel("conner", "joost", 100000,
			&testChannelPolicy{
				Expiry:   144,
				FeeRate:  400,
				MinHTLC:  1,
				MaxHTLC:  100000000,
				Features: unknownRequiredFeatures,
			},
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			}, 0,
		),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	conner := ctx.keyFromAlias("conner")
	joost := ctx.keyFromAlias("joost")

	// Conner's node in the graph has an unknown required feature (100).
	// Pathfinding should fail since we check the destination's features for
	// unknown required features before beginning pathfinding.
	_, err := ctx.findPath(conner, 100)
	if !reflect.DeepEqual(err, errUnknownRequiredFeature) {
		t.Fatalf("path shouldn't have been found: %v", err)
	}

	// Now, try to find a route to joost through conner. The destination
	// features are valid, but conner's feature vector in the graph still
	// requires feature 100. We expect errNoPathFound and not the error
	// above since intermediate hops are simply skipped if they have invalid
	// feature vectors, leaving no possible route to joost. This asserts
	// that we don't try to route _through_ nodes with unknown required
	// features.
	_, err = ctx.findPath(joost, 100)
	if err != errNoPathFound {
		t.Fatalf("path shouldn't have been found: %v", err)
	}
}

// runDestPaymentAddr asserts that we properly detect when we can send a
// payment address to a receiver, and also that we fallback to the receiver's
// node announcement if we don't have an invoice features.
func runDestPaymentAddr(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "luoji", 100000,
			&testChannelPolicy{
				Expiry:  144,
				FeeRate: 400,
				MinHTLC: 1,
				MaxHTLC: 100000000,
			},
		),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	luoji := ctx.keyFromAlias("luoji")

	// Add payment address w/o any invoice features.
	ctx.restrictParams.PaymentAddr = fn.Some([32]byte{1})

	// Add empty destination features. This should cause us to fail, since
	// this overrides anything in the graph.
	ctx.restrictParams.DestFeatures = lnwire.EmptyFeatureVector()

	_, err := ctx.findPath(luoji, 100)
	if err != errNoPaymentAddr {
		t.Fatalf("path shouldn't have been found: %v", err)
	}

	// Now, set the TLV and payment address features for the destination. We
	// should succeed in finding a path to luoji.
	ctx.restrictParams.DestFeatures = tlvPayAddrFeatures

	path, err := ctx.findPath(luoji, 100)
	require.NoError(t, err, "path should have been found")
	assertExpectedPath(t, ctx.testGraphInstance.aliasMap, path, "luoji")
}

func runPathInsufficientCapacity(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()
	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	// Next, test that attempting to find a path in which the current
	// channel graph cannot support due to insufficient capacity triggers
	// an error.

	// To test his we'll attempt to make a payment of 1 BTC, or 100 million
	// satoshis. The largest channel in the basic graph is of size 100k
	// satoshis, so we shouldn't be able to find a path to sophon even
	// though we have a 2-hop link.
	target := graph.aliasMap["sophon"]

	payAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	if err != errInsufficientBalance {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// runRouteFailMinHTLC tests that if we attempt to route an HTLC which is
// smaller than the advertised minHTLC of an edge, then path finding fails.
func runRouteFailMinHTLC(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()
	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	// We'll not attempt to route an HTLC of 10 SAT from roasbeef to Son
	// Goku. However, the min HTLC of Son Goku is 1k SAT, as a result, this
	// attempt should fail.
	target := graph.aliasMap["songoku"]
	payAmt := lnwire.MilliSatoshi(10)
	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	if err != errNoPathFound {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// runRouteFailMaxHTLC tests that if we attempt to route an HTLC which is
// larger than the advertised max HTLC of an edge, then path finding fails.
func runRouteFailMaxHTLC(t *testing.T, useCache bool) {
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

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	// First, attempt to send a payment greater than the max HTLC we are
	// about to set, which should succeed.
	target := ctx.keyFromAlias("target")
	payAmt := lnwire.MilliSatoshi(100001)
	_, err := ctx.findPath(target, payAmt)
	require.NoError(t, err, "graph should've been able to support payment")

	// Next, update the middle edge policy to only allow payments up to 100k
	// msat.
	graph := ctx.testGraphInstance.graph
	_, midEdge, _, err := graph.FetchChannelEdgesByID(firstToSecondID)
	require.NoError(t, err, "unable to fetch channel edges by ID")
	midEdge.MessageFlags = 1
	midEdge.MaxHTLC = payAmt - 1
	midEdge.LastUpdate = midEdge.LastUpdate.Add(time.Second)
	err = graph.UpdateEdgePolicy(t.Context(), midEdge)
	require.NoError(t, err)

	// We'll now attempt to route through that edge with a payment above
	// 100k msat, which should fail.
	_, err = ctx.findPath(target, payAmt)
	if err != errNoPathFound {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// runRouteFailDisabledEdge tests that if we attempt to route to an edge
// that's disabled, then that edge is disqualified, and the routing attempt
// will fail. We also test that this is true only for non-local edges, as we'll
// ignore the disable flags, with the assumption that the correct bandwidth is
// found among the bandwidth hints.
func runRouteFailDisabledEdge(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()
	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	// First, we'll try to route from roasbeef -> sophon. This should
	// succeed without issue, and return a single path via phamnuwen
	target := graph.aliasMap["sophon"]
	payAmt := lnwire.NewMSatFromSatoshis(105000)
	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")

	// Disable the edge roasbeef->phamnuwen. This should not impact the
	// path finding, as we don't consider the disable flag for local
	// channels (and roasbeef is the source).
	roasToPham := uint64(999991)
	_, e1, e2, err := graph.graph.FetchChannelEdgesByID(roasToPham)
	require.NoError(t, err, "unable to fetch edge")
	e1.ChannelFlags |= lnwire.ChanUpdateDisabled
	e1.LastUpdate = e1.LastUpdate.Add(time.Second)
	if err := graph.graph.UpdateEdgePolicy(ctx, e1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	e2.ChannelFlags |= lnwire.ChanUpdateDisabled
	e2.LastUpdate = e2.LastUpdate.Add(time.Second)
	if err := graph.graph.UpdateEdgePolicy(ctx, e2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")

	// Now, we'll modify the edge from phamnuwen -> sophon, to read that
	// it's disabled.
	phamToSophon := uint64(99999)
	_, e, _, err := graph.graph.FetchChannelEdgesByID(phamToSophon)
	require.NoError(t, err, "unable to fetch edge")
	e.ChannelFlags |= lnwire.ChanUpdateDisabled
	e.LastUpdate = e.LastUpdate.Add(time.Second)
	if err := graph.graph.UpdateEdgePolicy(ctx, e); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// If we attempt to route through that edge, we should get a failure as
	// it is no longer eligible.
	_, err = dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	if err != errNoPathFound {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}
}

// runPathSourceEdgesBandwidth tests that explicitly passing in a set of
// bandwidth hints is used by the path finding algorithm to consider whether to
// use a local channel.
func runPathSourceEdgesBandwidth(t *testing.T, useCache bool) {
	graph, err := parseTestGraph(t, useCache, basicGraphFilePath)
	require.NoError(t, err, "unable to create graph")

	ctx := t.Context()
	sourceNode, err := graph.graph.SourceNode(ctx)
	require.NoError(t, err, "unable to fetch source node")

	// First, we'll try to route from roasbeef -> sophon. This should
	// succeed without issue, and return a path via songoku, as that's the
	// cheapest path.
	target := graph.aliasMap["sophon"]
	payAmt := lnwire.NewMSatFromSatoshis(50000)
	path, err := dbFindPath(
		graph.graph, nil, &mockBandwidthHints{},
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")
	assertExpectedPath(t, graph.aliasMap, path, "songoku", "sophon")

	// Now we'll set the bandwidth of the edge roasbeef->songoku and
	// roasbeef->phamnuwen to 0.
	roasToSongoku := uint64(12345)
	roasToPham := uint64(999991)
	bandwidths := &mockBandwidthHints{
		hints: map[uint64]lnwire.MilliSatoshi{
			roasToSongoku: 0,
			roasToPham:    0,
		},
	}

	// Since both these edges has a bandwidth of zero, no path should be
	// found.
	_, err = dbFindPath(
		graph.graph, nil, bandwidths,
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	if err != errNoPathFound {
		t.Fatalf("graph shouldn't be able to support payment: %v", err)
	}

	// Set the bandwidth of roasbeef->phamnuwen high enough to carry the
	// payment.
	bandwidths.hints[roasToPham] = 2 * payAmt

	// Now, if we attempt to route again, we should find the path via
	// phamnuven, as the other source edge won't be considered.
	path, err = dbFindPath(
		graph.graph, nil, bandwidths,
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")
	assertExpectedPath(t, graph.aliasMap, path, "phamnuwen", "sophon")

	// Finally, set the roasbeef->songoku bandwidth, but also set its
	// disable flag.
	bandwidths.hints[roasToSongoku] = 2 * payAmt
	_, e1, e2, err := graph.graph.FetchChannelEdgesByID(roasToSongoku)
	require.NoError(t, err, "unable to fetch edge")
	e1.ChannelFlags |= lnwire.ChanUpdateDisabled
	e1.LastUpdate = e1.LastUpdate.Add(time.Second)
	if err := graph.graph.UpdateEdgePolicy(ctx, e1); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}
	e2.ChannelFlags |= lnwire.ChanUpdateDisabled
	e2.LastUpdate = e2.LastUpdate.Add(time.Second)
	if err := graph.graph.UpdateEdgePolicy(ctx, e2); err != nil {
		t.Fatalf("unable to update edge: %v", err)
	}

	// Since we ignore disable flags for local channels, a path should
	// still be found.
	path, err = dbFindPath(
		graph.graph, nil, bandwidths,
		noRestrictions, testPathFindingConfig,
		sourceNode.PubKeyBytes, target, payAmt, 0, 0,
	)
	require.NoError(t, err, "unable to find path")
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
	ctx := createTestCtxFromFile(t, startingHeight, specExampleFilePath)

	// We'll first exercise the scenario of a direct payment from Bob to
	// Carol, so we set "B" as the source node so path finding starts from
	// Bob.
	bob := ctx.aliases["B"]

	// Query for a route of 4,999,999 mSAT to carol.
	carol := ctx.aliases["C"]
	const amt lnwire.MilliSatoshi = 4999999
	req, err := NewRouteRequest(
		bob, &carol, amt, 0, noRestrictions, nil, nil,
		nil, MinCLTVDelta,
	)
	require.NoError(t, err, "invalid route request")

	route, _, err := ctx.router.FindRoute(req)
	require.NoError(t, err, "unable to find route")

	// Now we'll examine the route returned for correctness.
	//
	// It should be sending the exact payment amount as there are no
	// additional hops.
	require.Equal(t, amt, route.TotalAmount)
	require.Equal(t, amt, route.Hops[0].AmtToForward)
	require.Zero(t, route.HopFee(0))

	// The CLTV expiry should be the current height plus 18 (the expiry for
	// the B -> C channel.
	require.EqualValues(t, startingHeight+MinCLTVDelta, route.TotalTimeLock)

	// Next, we'll set A as the source node so we can assert that we create
	// the proper route for any queries starting with Alice.
	alice := ctx.aliases["A"]
	ctx.router.cfg.SelfNode = alice

	// We'll now request a route from A -> B -> C.
	req, err = NewRouteRequest(
		alice, &carol, amt, 0, noRestrictions, nil, nil, nil,
		MinCLTVDelta,
	)
	require.NoError(t, err, "invalid route request")

	route, _, err = ctx.router.FindRoute(req)
	require.NoError(t, err, "unable to find routes")

	// The route should be two hops.
	require.Len(t, route.Hops, 2)

	// The total amount should factor in a fee of 10199 and also use a CLTV
	// delta total of 38 (20 + 18),
	expectedAmt := lnwire.MilliSatoshi(5010198)
	require.Equal(t, expectedAmt, route.TotalAmount)

	expectedDelta := uint32(20 + MinCLTVDelta)
	require.Equal(t, startingHeight+expectedDelta, route.TotalTimeLock)

	// Ensure that the hops of the route are properly crafted.
	//
	// After taking the fee, Bob should be forwarding the remainder which
	// is the exact payment to Bob.
	require.Equal(t, amt, route.Hops[0].AmtToForward)

	// We shouldn't pay any fee for the first, hop, but the fee for the
	// second hop posted fee should be exactly:

	// The fee that we pay for the second hop will be "applied to the first
	// hop, so we should get a fee of exactly:
	//
	//  * 200 + 4999999 * 2000 / 1000000 = 10199
	require.EqualValues(t, 10199, route.HopFee(0))

	// While for the final hop, as there's no additional hop afterwards, we
	// pay no fee.
	require.Zero(t, route.HopFee(1))

	// The outgoing CLTV value itself should be the current height plus 30
	// to meet Carol's requirements.
	require.EqualValues(t, startingHeight+MinCLTVDelta,
		route.Hops[0].OutgoingTimeLock)

	// For B -> C, we assert that the final hop also has the proper
	// parameters.
	lastHop := route.Hops[1]
	require.EqualValues(t, amt, lastHop.AmtToForward)
	require.EqualValues(t, startingHeight+MinCLTVDelta,
		lastHop.OutgoingTimeLock)
}

func assertExpectedPath(t *testing.T, aliasMap map[string]route.Vertex,
	path []*unifiedEdge, nodeAliases ...string) {

	require.Len(t, path, len(nodeAliases))

	for i, hop := range path {
		require.Equal(t, aliasMap[nodeAliases[i]],
			hop.policy.ToNodePubKey())
	}
}

// TestNewRouteFromEmptyHops tests that the NewRouteFromHops function returns an
// error when the hop list is empty.
func TestNewRouteFromEmptyHops(t *testing.T) {
	t.Parallel()

	var source route.Vertex
	_, err := route.NewRouteFromHops(0, 0, source, []*route.Hop{})
	require.ErrorIs(t, err, route.ErrNoRouteHopsProvided)
}

// runRestrictOutgoingChannel asserts that a outgoing channel restriction is
// obeyed by the path finding algorithm.
func runRestrictOutgoingChannel(t *testing.T, useCache bool) {
	// Define channel id constants
	const (
		chanSourceA      = 1
		chanATarget      = 4
		chanSourceB1     = 2
		chanSourceB2     = 3
		chanBTarget      = 5
		chanSourceTarget = 6
	)

	// Set up a test graph with three possible paths from roasbeef to
	// target. The path through chanSourceB1 is the highest cost path.
	testChannels := []*testChannel{
		symmetricTestChannel("roasbeef", "a", 100000, &testChannelPolicy{
			Expiry: 144,
		}, chanSourceA),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
		}, chanATarget),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{
			Expiry: 144,
		}, chanSourceB1),
		symmetricTestChannel("roasbeef", "b", 100000, &testChannelPolicy{
			Expiry: 144,
		}, chanSourceB2),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 800,
		}, chanBTarget),
		symmetricTestChannel("roasbeef", "target", 100000, &testChannelPolicy{
			Expiry: 144,
		}, chanSourceTarget),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("target")
	outgoingChannelID := uint64(chanSourceB1)

	// Find the best path given the restriction to only use channel 2 as the
	// outgoing channel.
	ctx.restrictParams.OutgoingChannelIDs = []uint64{outgoingChannelID}
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")

	// Assert that the route starts with channel chanSourceB1, in line with
	// the specified restriction.
	if path[0].policy.ChannelID != chanSourceB1 {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", chanSourceB1,
			path[0].policy.ChannelID)
	}

	// If a direct channel to target is allowed as well, that channel is
	// expected to be selected because the routing fees are zero.
	ctx.restrictParams.OutgoingChannelIDs = []uint64{
		chanSourceB1, chanSourceTarget,
	}
	path, err = ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	if path[0].policy.ChannelID != chanSourceTarget {
		t.Fatalf("expected route to pass through channel %v",
			chanSourceTarget)
	}
}

// runRestrictLastHop asserts that a last hop restriction is obeyed by the path
// finding algorithm.
func runRestrictLastHop(t *testing.T, useCache bool) {
	// Set up a test graph with three possible paths from roasbeef to
	// target. The path via channel 1 and 2 is the lowest cost path.
	testChannels := []*testChannel{
		symmetricTestChannel("source", "a", 100000, &testChannelPolicy{
			Expiry: 144,
		}, 1),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 400,
		}, 2),
		symmetricTestChannel("source", "b", 100000, &testChannelPolicy{
			Expiry: 144,
		}, 3),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:  144,
			FeeRate: 800,
		}, 4),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "source")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("target")
	lastHop := ctx.keyFromAlias("b")

	// Find the best path given the restriction to use b as the last hop.
	// This should force pathfinding to not take the lowest cost option.
	ctx.restrictParams.LastHop = &lastHop
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	if path[0].policy.ChannelID != 3 {
		t.Fatalf("expected route to pass through channel 3, "+
			"but channel %v was selected instead",
			path[0].policy.ChannelID)
	}
}

// runCltvLimit asserts that a cltv limit is obeyed by the path finding
// algorithm.
func runCltvLimit(t *testing.T, useCache bool) {
	t.Run("no limit", func(t *testing.T) {
		testCltvLimit(t, useCache, 2016, 1)
	})
	t.Run("no path", func(t *testing.T) {
		testCltvLimit(t, useCache, 50, 0)
	})
	t.Run("force high cost", func(t *testing.T) {
		testCltvLimit(t, useCache, 80, 3)
	})
}

func testCltvLimit(t *testing.T, useCache bool, limit uint32,
	expectedChannel uint64) {

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

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("target")

	ctx.restrictParams.CltvLimit = limit
	path, err := ctx.findPath(target, paymentAmt)
	if expectedChannel == 0 {
		// Finish test if we expect no route.
		if err == errNoPathFound {
			return
		}
		t.Fatal("expected no path to be found")
	}
	require.NoError(t, err, "unable to find path")

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)
	route, err := newRoute(
		ctx.source, path, startingHeight,
		finalHopParams{
			amt:       paymentAmt,
			cltvDelta: finalHopCLTV,
			records:   nil,
		}, nil,
	)
	require.NoError(t, err, "unable to create path")

	// Assert that the route starts with the expected channel.
	if route.Hops[0].ChannelID != expectedChannel {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", expectedChannel,
			route.Hops[0].ChannelID)
	}
}

// runProbabilityRouting asserts that path finding not only takes into account
// fees but also success probability.
func runProbabilityRouting(t *testing.T, useCache bool) {
	testCases := []struct {
		name           string
		p10, p11, p20  float64
		minProbability float64
		expectedChan   uint64
		amount         btcutil.Amount
		timePref       float64
	}{
		// Test two variations with probabilities that should multiply
		// to the same total route probability. In both cases the three
		// hop route should be the best route. The three hop route has a
		// probability of 0.5 * 0.8 = 0.4. The fee is 5 (chan 10) + 8
		// (chan 11) = 13. The attempt cost is 9 + 1% * 100 = 10. Path
		// finding distance should work out to: 13 + 10 (attempt
		// penalty) / 0.4 = 38. The two hop route is 25 + 10 / 0.7 = 39.
		{
			name: "three hop 1",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   10,
			amount:         100,
		},
		{
			name: "three hop 2",
			p10:  0.5, p11: 0.8, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   10,
			amount:         100,
		},

		// If we increase the time preference, we expect the algorithm
		// to choose - with everything else being equal - the more
		// expensive, more reliable route.
		{
			name: "three hop timepref",
			p10:  0.5, p11: 0.8, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   20,
			amount:         100,
			timePref:       1,
		},

		// If a larger amount is sent, the effect of the proportional
		// attempt cost becomes more noticeable. This amount in this
		// test brings the attempt cost to 9 + 1% * 300 = 12 sat. The
		// three hop path finding distance should work out to: 13 + 12
		// (attempt penalty) / 0.4 = 43. The two hop route is 25 + 12 /
		// 0.7 = 42. For this higher amount, the two hop route is
		// expected to be selected.
		{
			name: "two hop high amount",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.1,
			expectedChan:   20,
			amount:         300,
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
			amount:         100,
		},

		// If the same probabilities are used with a probability lower bound of
		// 0.5, we expect the three hop route with probability 0.4 to be
		// excluded and the two hop route to be picked.
		{
			name: "probability limit",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.5,
			expectedChan:   20,
			amount:         100,
		},

		// With a probability limit above the probability of both routes, we
		// expect no route to be returned. This expectation is signaled by using
		// expected channel 0.
		{
			name: "probability limit no routes",
			p10:  0.8, p11: 0.5, p20: 0.7,
			minProbability: 0.8,
			expectedChan:   0,
			amount:         100,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			testProbabilityRouting(
				t, useCache, tc.amount, tc.timePref,
				tc.p10, tc.p11, tc.p20,
				tc.minProbability, tc.expectedChan,
			)
		})
	}
}

func testProbabilityRouting(t *testing.T, useCache bool,
	paymentAmt btcutil.Amount, timePref float64,
	p10, p11, p20, minProbability float64,
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

	ctx := newPathFindingTestContext(t, useCache, testChannels, "roasbeef")

	alias := ctx.testGraphInstance.aliasMap

	target := ctx.testGraphInstance.aliasMap["target"]

	// Configure a probability source with the test parameters.
	ctx.restrictParams.ProbabilitySource = func(fromNode,
		toNode route.Vertex, amt lnwire.MilliSatoshi,
		capacity btcutil.Amount) float64 {

		if amt == 0 {
			t.Fatal("expected non-zero amount")
		}

		switch {
		case fromNode == alias["a1"] && toNode == alias["a2"]:
			return p10
		case fromNode == alias["a2"] && toNode == alias["target"]:
			return p11
		case fromNode == alias["b"] && toNode == alias["target"]:
			return p20
		default:
			return 1
		}
	}

	ctx.pathFindingConfig = PathFindingConfig{
		AttemptCost:    lnwire.NewMSatFromSatoshis(9),
		AttemptCostPPM: 10000,
		MinProbability: minProbability,
	}

	ctx.timePref = timePref

	path, err := ctx.findPath(
		target, lnwire.NewMSatFromSatoshis(paymentAmt),
	)
	if expectedChan == 0 {
		if err != errNoPathFound {
			t.Fatalf("expected no path found, but got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}

	// Assert that the route passes through the expected channel.
	if path[1].policy.ChannelID != expectedChan {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", expectedChan,
			path[1].policy.ChannelID)
	}
}

// runEqualCostRouteSelection asserts that route probability will be used as a
// tie breaker in case the path finding probabilities are equal.
func runEqualCostRouteSelection(t *testing.T, useCache bool) {
	// Set up a test graph with two possible paths to the target: via a and
	// via b. The routing fees and probabilities are chosen such that the
	// algorithm will first explore target->a->source (backwards search).
	// This route has fee 6 and a penalty of 4 for the 25% success
	// probability. The algorithm will then proceed with evaluating
	// target->b->source, which has a fee of 8 and a penalty of 2 for the
	// 50% success probability. Both routes have the same path finding cost
	// of 10. It is expected that in that case, the highest probability
	// route (through b) is chosen.
	testChannels := []*testChannel{
		symmetricTestChannel("source", "a", 100000, &testChannelPolicy{}),
		symmetricTestChannel("source", "b", 100000, &testChannelPolicy{}),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: lnwire.NewMSatFromSatoshis(6),
			MinHTLC:     1,
		}, 1),
		symmetricTestChannel("b", "target", 100000, &testChannelPolicy{
			Expiry:      100,
			FeeBaseMsat: lnwire.NewMSatFromSatoshis(8),
			MinHTLC:     1,
		}, 2),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "source")

	alias := ctx.testGraphInstance.aliasMap

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.testGraphInstance.aliasMap["target"]

	ctx.restrictParams.ProbabilitySource = func(fromNode,
		toNode route.Vertex, amt lnwire.MilliSatoshi,
		capacity btcutil.Amount) float64 {

		switch {
		case fromNode == alias["source"] && toNode == alias["a"]:
			return 0.25
		case fromNode == alias["source"] && toNode == alias["b"]:
			return 0.5
		default:
			return 1
		}
	}

	ctx.pathFindingConfig = PathFindingConfig{
		AttemptCost: lnwire.NewMSatFromSatoshis(1),
	}

	path, err := ctx.findPath(target, paymentAmt)
	if err != nil {
		t.Fatal(err)
	}

	if path[1].policy.ChannelID != 2 {
		t.Fatalf("expected route to pass through channel %v, "+
			"but channel %v was selected instead", 2,
			path[1].policy.ChannelID)
	}
}

// runNoCycle tries to guide the path finding algorithm into reconstructing an
// endless route. It asserts that the algorithm is able to handle this properly.
func runNoCycle(t *testing.T, useCache bool) {
	// Set up a test graph with two paths: source->a->target and
	// source->b->c->target. The fees are setup such that, searching
	// backwards, the algorithm will evaluate the following end of the route
	// first: ->target->c->target. This does not make sense, because if
	// target is reached, there is no need to continue to c. A proper
	// implementation will then go on with alternative routes. It will then
	// consider ->a->target because its cost is lower than the alternative
	// ->b->c->target and finally find source->a->target as the best route.
	testChannels := []*testChannel{
		symmetricTestChannel("source", "a", 100000, &testChannelPolicy{
			Expiry: 144,
		}, 1),
		symmetricTestChannel("source", "b", 100000, &testChannelPolicy{
			Expiry: 144,
		}, 2),
		symmetricTestChannel("b", "c", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 2000,
		}, 3),
		symmetricTestChannel("c", "target", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 0,
		}, 4),
		symmetricTestChannel("a", "target", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 600,
		}, 5),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "source")

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.keyFromAlias("target")

	// Find the best path given the restriction to only use channel 2 as the
	// outgoing channel.
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	route, err := newRoute(
		ctx.source, path, startingHeight,
		finalHopParams{
			amt:       paymentAmt,
			cltvDelta: finalHopCLTV,
			records:   nil,
		}, nil,
	)
	require.NoError(t, err, "unable to create path")

	if len(route.Hops) != 2 {
		t.Fatalf("unexpected route")
	}
	if route.Hops[0].ChannelID != 1 {
		t.Fatalf("unexpected first hop")
	}
	if route.Hops[1].ChannelID != 5 {
		t.Fatalf("unexpected second hop")
	}
}

// runRouteToSelf tests that it is possible to find a route to the self node.
func runRouteToSelf(t *testing.T, useCache bool) {
	testChannels := []*testChannel{
		symmetricTestChannel("source", "a", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 500,
		}, 1),
		symmetricTestChannel("source", "b", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 1000,
		}, 2),
		symmetricTestChannel("a", "b", 100000, &testChannelPolicy{
			Expiry:      144,
			FeeBaseMsat: 1000,
		}, 3),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "source")

	paymentAmt := lnwire.NewMSatFromSatoshis(100)
	target := ctx.source

	// Find the best path to self. We expect this to be source->a->source,
	// because a charges the lowest forwarding fee.
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	ctx.assertPath(path, []uint64{1, 1})

	outgoingChanID := uint64(1)
	lastHop := ctx.keyFromAlias("b")
	ctx.restrictParams.OutgoingChannelIDs = []uint64{outgoingChanID}
	ctx.restrictParams.LastHop = &lastHop

	// Find the best path to self given that we want to go out via channel 1
	// and return through node b.
	path, err = ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")
	ctx.assertPath(path, []uint64{1, 3, 2})
}

// runInboundFees tests whether correct routes are built when inbound fees
// apply.
func runInboundFees(t *testing.T, useCache bool) {
	// Setup a test network.
	chanCapSat := btcutil.Amount(100000)
	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.PaymentAddrOptional,
			lnwire.TLVOnionPayloadRequired,
		),
		lnwire.Features,
	)

	testChannels := []*testChannel{
		asymmetricTestChannel("a", "b", chanCapSat,
			&testChannelPolicy{
				MinHTLC:  lnwire.NewMSatFromSatoshis(2),
				Features: features,
			},
			&testChannelPolicy{
				Features:           features,
				InboundFeeRate:     -60000,
				InboundFeeBaseMsat: -5000,
			}, 10,
		),
		asymmetricTestChannel("b", "c", chanCapSat,
			&testChannelPolicy{
				Expiry:      20,
				FeeRate:     50000,
				FeeBaseMsat: 1000,
				MinHTLC:     lnwire.NewMSatFromSatoshis(2),
				Features:    features,
			},
			&testChannelPolicy{
				Features:           features,
				InboundFeeRate:     0,
				InboundFeeBaseMsat: 5000,
			}, 11,
		),
		asymmetricTestChannel("c", "d", chanCapSat,
			&testChannelPolicy{
				Expiry:      20,
				FeeRate:     50000,
				FeeBaseMsat: 0,
				MinHTLC:     lnwire.NewMSatFromSatoshis(2),
				Features:    features,
			},
			&testChannelPolicy{
				Features:           features,
				InboundFeeRate:     -50000,
				InboundFeeBaseMsat: -8000,
			}, 12,
		),
		asymmetricTestChannel("d", "e", chanCapSat,
			&testChannelPolicy{
				Expiry:      20,
				FeeRate:     100000,
				FeeBaseMsat: 9000,
				MinHTLC:     lnwire.NewMSatFromSatoshis(2),
				Features:    features,
			},
			&testChannelPolicy{
				Features:           features,
				InboundFeeRate:     80000,
				InboundFeeBaseMsat: 2000,
			}, 13,
		),
	}

	ctx := newPathFindingTestContext(t, useCache, testChannels, "a")

	payAddr := [32]byte{1}
	ctx.restrictParams.PaymentAddr = fn.Some(payAddr)
	ctx.restrictParams.DestFeatures = tlvPayAddrFeatures

	const (
		startingHeight = 100
		finalHopCLTV   = 1
	)

	paymentAmt := lnwire.MilliSatoshi(100_000)
	target := ctx.keyFromAlias("e")
	path, err := ctx.findPath(target, paymentAmt)
	require.NoError(t, err, "unable to find path")

	rt, err := newRoute(
		ctx.source, path, startingHeight,
		finalHopParams{
			amt:         paymentAmt,
			cltvDelta:   finalHopCLTV,
			records:     nil,
			paymentAddr: fn.Some(payAddr),
			totalAmt:    paymentAmt,
		},
		nil,
	)
	require.NoError(t, err, "unable to create path")

	expectedHops := []*route.Hop{
		{
			PubKeyBytes: ctx.keyFromAlias("b"),
			ChannelID:   10,
			// The amount that c forwards (105_050) plus the out fee
			// (5_252) and in fee (5_000) of c.
			AmtToForward:     115_302,
			OutgoingTimeLock: 141,
		},
		{
			PubKeyBytes: ctx.keyFromAlias("c"),
			ChannelID:   11,
			// The amount that d forwards (100_000) plus the out fee
			// (19_000) and in fee (-13_950) of d.
			AmtToForward:     105_050,
			OutgoingTimeLock: 121,
		},
		{
			PubKeyBytes:      ctx.keyFromAlias("d"),
			ChannelID:        12,
			AmtToForward:     100_000,
			OutgoingTimeLock: 101,
		},
		{
			PubKeyBytes:      ctx.keyFromAlias("e"),
			ChannelID:        13,
			AmtToForward:     100_000,
			OutgoingTimeLock: 101,
			MPP:              record.NewMPP(100_000, payAddr),
		},
	}

	expectedRt := &route.Route{
		// The amount that b forwards (115_302) plus the out fee (6_765)
		// and in fee (-12324) of b. The total fee is floored at zero.
		TotalAmount:   115_302,
		TotalTimeLock: 161,
		SourcePubKey:  ctx.keyFromAlias("a"),
		Hops:          expectedHops,
	}
	require.Equal(t, expectedRt, rt)
}

type pathFindingTestContext struct {
	t                 *testing.T
	graph             *graphdb.ChannelGraph
	restrictParams    RestrictParams
	bandwidthHints    bandwidthHints
	pathFindingConfig PathFindingConfig
	testGraphInstance *testGraphInstance
	source            route.Vertex
	timePref          float64
}

func newPathFindingTestContext(t *testing.T, useCache bool,
	testChannels []*testChannel, source string,
	sourceFeatureBits ...lnwire.FeatureBit) *pathFindingTestContext {

	testGraphInstance, err := createTestGraphFromChannels(
		t, useCache, testChannels, source, sourceFeatureBits...,
	)
	require.NoError(t, err, "unable to create graph")

	sourceNode, err := testGraphInstance.graph.SourceNode(
		t.Context(),
	)
	require.NoError(t, err, "unable to fetch source node")

	ctx := &pathFindingTestContext{
		t:                 t,
		testGraphInstance: testGraphInstance,
		source:            route.Vertex(sourceNode.PubKeyBytes),
		pathFindingConfig: *testPathFindingConfig,
		graph:             testGraphInstance.graph,
		restrictParams:    *noRestrictions,
		bandwidthHints:    &mockBandwidthHints{},
	}

	return ctx
}

func (c *pathFindingTestContext) nodePairChannel(alias1, alias2 string) uint64 {
	node1 := c.keyFromAlias(alias1)
	node2 := c.keyFromAlias(alias2)

	channel, ok := c.testGraphInstance.channelIDs[node1][node2]
	require.True(c.t, ok)

	return channel
}

func (c *pathFindingTestContext) keyFromAlias(alias string) route.Vertex {
	return c.testGraphInstance.aliasMap[alias]
}

func (c *pathFindingTestContext) aliasFromKey(pubKey route.Vertex) string {
	for alias, key := range c.testGraphInstance.aliasMap {
		if key == pubKey {
			return alias
		}
	}
	return ""
}

func (c *pathFindingTestContext) findPath(target route.Vertex,
	amt lnwire.MilliSatoshi) ([]*unifiedEdge,
	error) {

	return dbFindPath(
		c.graph, nil, c.bandwidthHints, &c.restrictParams,
		&c.pathFindingConfig, c.source, target, amt, c.timePref, 0,
	)
}

func (c *pathFindingTestContext) findBlindedPaths(
	restrictions *blindedPathRestrictions) ([][]blindedHop, error) {

	return dbFindBlindedPaths(c.graph, restrictions)
}

func (c *pathFindingTestContext) assertPath(path []*unifiedEdge,
	expected []uint64) {

	if len(path) != len(expected) {
		c.t.Fatalf("expected path of length %v, but got %v",
			len(expected), len(path))
	}

	for i, edge := range path {
		if edge.policy.ChannelID != expected[i] {
			c.t.Fatalf("expected hop %v to be channel %v, "+
				"but got %v", i, expected[i],
				edge.policy.ChannelID)
		}
	}
}

// dbFindPath calls findPath after getting a db transaction from the database
// graph.
func dbFindPath(graph *graphdb.ChannelGraph,
	additionalEdges map[route.Vertex][]AdditionalEdge,
	bandwidthHints bandwidthHints,
	r *RestrictParams, cfg *PathFindingConfig,
	source, target route.Vertex, amt lnwire.MilliSatoshi, timePref float64,
	finalHtlcExpiry int32) ([]*unifiedEdge, error) {

	ctx := context.Background()
	sourceNode, err := graph.SourceNode(ctx)
	if err != nil {
		return nil, err
	}

	var route []*unifiedEdge
	err = graph.GraphSession(func(graph graphdb.NodeTraverser) error {
		route, _, err = findPath(
			&graphParams{
				additionalEdges: additionalEdges,
				bandwidthHints:  bandwidthHints,
				graph:           graph,
			},
			r, cfg, sourceNode.PubKeyBytes, source, target, amt,
			timePref, finalHtlcExpiry,
		)

		return err
	}, func() {
		route = nil
	})
	if err != nil {
		return nil, err
	}

	return route, nil
}

// dbFindBlindedPaths calls findBlindedPaths after getting a db transaction from
// the database graph.
func dbFindBlindedPaths(graph *graphdb.ChannelGraph,
	restrictions *blindedPathRestrictions) ([][]blindedHop, error) {

	sourceNode, err := graph.SourceNode(context.Background())
	if err != nil {
		return nil, err
	}

	return findBlindedPaths(
		graph, sourceNode.PubKeyBytes, restrictions,
	)
}

// TestBlindedRouteConstruction tests creation of a blinded route with the
// following topology:
//
// A -- B -- C (introduction point) - D (blinded) - E (blinded).
func TestBlindedRouteConstruction(t *testing.T) {
	t.Parallel()

	var (
		// We need valid pubkeys for the blinded portion of our route,
		// so we just produce all of our pubkeys in the same way.
		_, alicePk       = btcec.PrivKeyFromBytes([]byte{1})
		_, bobPk         = btcec.PrivKeyFromBytes([]byte{2})
		_, carolPk       = btcec.PrivKeyFromBytes([]byte{3})
		_, daveBlindedPk = btcec.PrivKeyFromBytes([]byte{4})
		_, eveBlindedPk  = btcec.PrivKeyFromBytes([]byte{5})

		_, blindingPk = btcec.PrivKeyFromBytes([]byte{9})

		// Convenient type conversions for the pieces of code that use
		// vertexes.
		sourceVertex      = route.NewVertex(alicePk)
		bobVertex         = route.NewVertex(bobPk)
		carolVertex       = route.NewVertex(carolPk)
		daveBlindedVertex = route.NewVertex(daveBlindedPk)
		eveBlindedVertex  = route.NewVertex(eveBlindedPk)

		currentHeight uint32 = 100

		// Create arbitrary encrypted data for each hop (we don't need
		// to actually read this data to test route construction, since
		// it's only used by forwarding nodes) and metadata for the
		// final hop.
		metadata           = []byte{1, 2, 3}
		carolEncryptedData = []byte{4, 5, 6}
		daveEncryptedData  = []byte{7, 8, 9}
		eveEncryptedData   = []byte{10, 11, 12}

		// Create a blinded route with carol as the introduction point.
		blindedPath = &sphinx.BlindedPath{
			IntroductionPoint: carolPk,
			BlindingPoint:     blindingPk,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					// Note: no pubkey is provided for
					// Carol because we use the pubkey
					// given by IntroductionPoint.
					CipherText: carolEncryptedData,
				},
				{
					BlindedNodePub: daveBlindedPk,
					CipherText:     daveEncryptedData,
				},
				{
					BlindedNodePub: eveBlindedPk,
					CipherText:     eveEncryptedData,
				},
			},
		}

		// Create a blinded payment, which contains the aggregate relay
		// information and constraints for the blinded portion of the
		// path.
		blindedPayment = &BlindedPayment{
			BlindedPath:     blindedPath,
			CltvExpiryDelta: 120,
			// Set only a base fee for easier calculations.
			BaseFee:  5000,
			Features: tlvFeatures,
		}

		// Create channel edges for the unblinded portion of our
		// route. Proportional fees are omitted for easy test
		// calculations, but non-zero base fees ensure our fee is
		// still accounted for.
		aliceBobEdge = &models.CachedEdgePolicy{
			ChannelID: 1,
			// We won't actually use this timelock / fee (since
			// it's the sender's outbound channel), but we include
			// it with different values so that the test will trip
			// up if we were to include the fee/delay.
			TimeLockDelta: 10,
			FeeBaseMSat:   50,
			ToNodePubKey: func() route.Vertex {
				return bobVertex
			},
			ToNodeFeatures: tlvFeatures,
		}

		bobCarolEdge = &models.CachedEdgePolicy{
			ChannelID:     2,
			TimeLockDelta: 15,
			FeeBaseMSat:   20,
			ToNodePubKey: func() route.Vertex {
				return carolVertex
			},
			ToNodeFeatures: tlvFeatures,
		}

		// Create final hop parameters for payment amount = 110.
		totalAmt       lnwire.MilliSatoshi = 110
		finalHopParams                     = finalHopParams{
			amt:      totalAmt,
			totalAmt: totalAmt,
			metadata: metadata,

			// We set a CLTV delta here just to test that this will
			// be ignored by newRoute since this is a blinded path
			// where the accumulated CLTV delta for the route
			// communicated in the blinded path should be assumed to
			// include the CLTV delta of the final hop.
			cltvDelta: MaxCLTVDelta,
		}
	)

	require.NoError(t, blindedPayment.Validate())

	blindedPathSet, err := NewBlindedPaymentPathSet(
		[]*BlindedPayment{blindedPayment},
	)
	require.NoError(t, err)

	// Generate route hints from our blinded payment and a set of edges
	// that make up the graph we'll give to route construction. The hints
	// map is keyed by source node, so we can retrieve our blinded edges
	// accordingly.
	blindedEdges, err := blindedPayment.toRouteHints()
	require.NoError(t, err)

	carolDaveEdge := blindedEdges[carolVertex][0]
	daveEveEdge := blindedEdges[daveBlindedVertex][0]

	edges := []*unifiedEdge{
		{
			policy: aliceBobEdge,
		},
		{
			policy:         bobCarolEdge,
			blindedPayment: blindedPayment,
		},
		{
			policy:         carolDaveEdge.EdgePolicy(),
			blindedPayment: blindedPayment,
		},
		{
			policy:         daveEveEdge.EdgePolicy(),
			blindedPayment: blindedPayment,
		},
	}

	// Total timelock for the route should include:
	// - Starting block height
	// - CLTV delta for Bob -> Carol (unblinded hop)
	// - Aggregate cltv from blinded path, which includes
	//   - CLTV delta for Carol -> Dave -> Eve (blinded route)
	//   - Eve's chosen final CLTV delta
	totalTimelock := currentHeight +
		uint32(bobCarolEdge.TimeLockDelta) +
		uint32(blindedPayment.CltvExpiryDelta)

	// Total amount for the route should include:
	// - Total amount being sent
	// - Fee for Bob -> Carol (unblinded hop)
	// - Fee for Carol -> Dave -> Eve (blinded route)
	totalAmount := totalAmt + bobCarolEdge.FeeBaseMSat +
		lnwire.MilliSatoshi(blindedPayment.BaseFee)

	// Bob's outgoing timelock and amount are the total less his own
	// outgoing channel's delta and fee.
	bobTimelock := currentHeight + uint32(blindedPayment.CltvExpiryDelta)
	bobAmount := totalAmt + lnwire.MilliSatoshi(
		blindedPayment.BaseFee,
	)

	aliceBobRouteHop := &route.Hop{
		PubKeyBytes: bobVertex,
		ChannelID:   aliceBobEdge.ChannelID,
		// Alice -> Bob is a regular hop, so it should include all the
		// regular forwarding values for Bob to send an outgoing HTLC
		// to Carol.
		OutgoingTimeLock: bobTimelock,
		AmtToForward:     bobAmount,
	}

	bobCarolRouteHop := &route.Hop{
		PubKeyBytes: carolVertex,
		ChannelID:   bobCarolEdge.ChannelID,
		// Bob -> Carol sends the HTLC to the introduction node, so
		// it should not have forwarding values for Carol (these will
		// be obtained from the encrypted data) and should include both
		// the blinding point and encrypted data to be passed to Carol.
		OutgoingTimeLock: 0,
		AmtToForward:     0,
		BlindingPoint:    blindingPk,
		EncryptedData:    carolEncryptedData,
	}

	carolDaveRouteHop := &route.Hop{
		PubKeyBytes: daveBlindedVertex,
		// Carol -> Dave is within the blinded route, so should not
		// set any outgoing values but must include Dave's encrypted
		// data.
		OutgoingTimeLock: 0,
		AmtToForward:     0,
		EncryptedData:    daveEncryptedData,
	}

	daveEveRouteHop := &route.Hop{
		PubKeyBytes: eveBlindedVertex,
		// Dave -> Eve is the final hop in a blinded route, so it
		// should include outgoing values for the final value, along
		// with any encrypted data for Eve. Since we have not added
		// any block padding to the final hop, this value is _just_
		// the current height (as the final CTLV delta is covered in
		// the blinded path.
		OutgoingTimeLock: currentHeight,
		AmtToForward:     totalAmt,
		EncryptedData:    eveEncryptedData,
		// The last hop should also contain final-hop fields such as
		// metadata and total amount.
		Metadata:     metadata,
		TotalAmtMsat: totalAmt,
	}

	expectedRoute := &route.Route{
		SourcePubKey: sourceVertex,
		Hops: []*route.Hop{
			aliceBobRouteHop,
			bobCarolRouteHop,
			carolDaveRouteHop,
			daveEveRouteHop,
		},
		TotalTimeLock: totalTimelock,
		TotalAmount:   totalAmount,
	}

	route, err := newRoute(
		sourceVertex, edges, currentHeight, finalHopParams,
		blindedPathSet,
	)
	require.NoError(t, err)
	require.Equal(t, expectedRoute, route)
}

// TestLastHopPayloadSize tests the final hop payload size. The final hop
// payload structure differes from the intermediate hop payload for both the
// non-blinded and blinded case.
func TestLastHopPayloadSize(t *testing.T) {
	t.Parallel()

	var (
		metadata      = []byte{21, 22}
		customRecords = map[uint64][]byte{
			record.CustomTypeStart: {1, 2, 3},
		}

		encrypedDataSmall = bytes.Repeat(
			[]byte{1}, 5,
		)
		encrypedDataLarge = bytes.Repeat(
			[]byte{1}, 100,
		)
		_, blindedPoint          = btcec.PrivKeyFromBytes([]byte{5})
		paymentAddr              = &[32]byte{1}
		ampOptions               = &AMPOptions{}
		amtToForward             = lnwire.MilliSatoshi(10000)
		emptyEncryptedData       = []byte{}
		finalHopExpiry     int32 = 144

		oneHopPath = &sphinx.BlindedPath{
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					CipherText: emptyEncryptedData,
				},
			},
			BlindingPoint: blindedPoint,
		}

		twoHopPathSmallHopSize = &sphinx.BlindedPath{
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					CipherText: encrypedDataLarge,
				},
				{
					CipherText: encrypedDataLarge,
				},
			},
			BlindingPoint: blindedPoint,
		}

		twoHopPathLargeHopSize = &sphinx.BlindedPath{
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					CipherText: encrypedDataSmall,
				},
				{
					CipherText: encrypedDataSmall,
				},
			},
			BlindingPoint: blindedPoint,
		}
	)

	oneHopBlindedPayment, err := NewBlindedPaymentPathSet(
		[]*BlindedPayment{{BlindedPath: oneHopPath}},
	)
	require.NoError(t, err)

	twoHopBlindedPayment, err := NewBlindedPaymentPathSet(
		[]*BlindedPayment{
			{BlindedPath: twoHopPathLargeHopSize},
			{BlindedPath: twoHopPathSmallHopSize},
		},
	)
	require.NoError(t, err)

	testCases := []struct {
		name                  string
		restrictions          *RestrictParams
		finalHopExpiry        int32
		amount                lnwire.MilliSatoshi
		expectedEncryptedData []byte
	}{
		{
			name: "Non blinded final hop",
			restrictions: &RestrictParams{
				PaymentAddr:       fn.Some(*paymentAddr),
				DestCustomRecords: customRecords,
				Metadata:          metadata,
				Amp:               ampOptions,
			},
			amount:         amtToForward,
			finalHopExpiry: finalHopExpiry,
		},
		{
			name: "Blinded final hop introduction point",
			restrictions: &RestrictParams{
				BlindedPaymentPathSet: oneHopBlindedPayment,
			},
			amount:                amtToForward,
			finalHopExpiry:        finalHopExpiry,
			expectedEncryptedData: emptyEncryptedData,
		},
		{
			name: "Blinded final hop of a two hop payment",
			restrictions: &RestrictParams{
				BlindedPaymentPathSet: twoHopBlindedPayment,
			},
			amount:                amtToForward,
			finalHopExpiry:        finalHopExpiry,
			expectedEncryptedData: encrypedDataLarge,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mpp := fn.MapOptionZ(tc.restrictions.PaymentAddr,
				func(addr [32]byte) *record.MPP {
					return record.NewMPP(tc.amount, addr)
				})

			// In case it's an AMP payment we use the max AMP record
			// size to estimate the final hop size.
			var amp *record.AMP
			if tc.restrictions.Amp != nil {
				amp = &record.MaxAmpPayLoadSize
			}

			var finalHop route.Hop
			if tc.restrictions.BlindedPaymentPathSet != nil {
				bPSet := tc.restrictions.BlindedPaymentPathSet
				path, err := bPSet.LargestLastHopPayloadPath()
				require.NotNil(t, path)

				require.NoError(t, err)

				blindedPath := path.BlindedPath.BlindedHops
				blindedPoint := path.BlindedPath.BlindingPoint
				lastHop := blindedPath[len(blindedPath)-1]
				require.Equal(t, lastHop.CipherText,
					tc.expectedEncryptedData)

				//nolint:ll
				finalHop = route.Hop{
					AmtToForward:     tc.amount,
					OutgoingTimeLock: uint32(tc.finalHopExpiry),
					EncryptedData:    lastHop.CipherText,
				}
				if len(blindedPath) == 1 {
					finalHop.BlindingPoint = blindedPoint
				}
			} else {
				//nolint:ll
				finalHop = route.Hop{
					AmtToForward:     tc.amount,
					OutgoingTimeLock: uint32(tc.finalHopExpiry),
					Metadata:         tc.restrictions.Metadata,
					MPP:              mpp,
					AMP:              amp,
					CustomRecords:    tc.restrictions.DestCustomRecords,
				}
			}

			payLoad, err := createHopPayload(finalHop, 0, true)
			require.NoErrorf(t, err, "failed to create hop payload")

			expectedPayloadSize, err := lastHopPayloadSize(
				tc.restrictions, tc.finalHopExpiry,
				tc.amount,
			)
			require.NoError(t, err)
			require.Equal(
				t, expectedPayloadSize,
				uint64(payLoad.NumBytes()),
			)
		})
	}
}

// TestFindBlindedPaths tests that the findBlindedPaths function correctly
// selects a set of blinded paths to a destination node given various
// restrictions.
func TestFindBlindedPaths(t *testing.T) {
	featuresWithRouteBlinding := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.RouteBlindingOptional),
		lnwire.Features,
	)

	policyWithRouteBlinding := &testChannelPolicy{
		Expiry:   144,
		FeeRate:  400,
		MinHTLC:  1,
		MaxHTLC:  100000000,
		Features: featuresWithRouteBlinding,
	}

	policyWithoutRouteBlinding := &testChannelPolicy{
		Expiry:  144,
		FeeRate: 400,
		MinHTLC: 1,
		MaxHTLC: 100000000,
	}

	// Set up the following graph where Dave will be our destination node.
	// All the nodes except for A will signal the Route Blinding feature
	// bit.
	//
	// 	      A --- F
	// 	      |	    |
	//	G --- D --- B --- E
	// 	      |		  |
	// 	      C-----------/
	//
	testChannels := []*testChannel{
		symmetricTestChannel(
			"dave", "alice", 100000, policyWithoutRouteBlinding, 1,
		),
		symmetricTestChannel(
			"dave", "bob", 100000, policyWithRouteBlinding, 2,
		),
		symmetricTestChannel(
			"dave", "charlie", 100000, policyWithRouteBlinding, 3,
		),
		symmetricTestChannel(
			"alice", "frank", 100000, policyWithRouteBlinding, 4,
		),
		symmetricTestChannel(
			"bob", "frank", 100000, policyWithRouteBlinding, 5,
		),
		symmetricTestChannel(
			"eve", "charlie", 100000, policyWithRouteBlinding, 6,
		),
		symmetricTestChannel(
			"bob", "eve", 100000, policyWithRouteBlinding, 7,
		),
		symmetricTestChannel(
			"dave", "george", 100000, policyWithRouteBlinding, 8,
		),
	}

	ctx := newPathFindingTestContext(
		t, true, testChannels, "dave", lnwire.RouteBlindingOptional,
	)

	// assertPaths checks that the set of selected paths contains all the
	// expected paths.
	assertPaths := func(paths [][]blindedHop, expectedPaths []string) {
		require.Len(t, paths, len(expectedPaths))

		actualPaths := make(map[string]bool)

		for _, path := range paths {
			var label string
			for _, hop := range path {
				label += ctx.aliasFromKey(hop.vertex) + ","
			}

			actualPaths[strings.TrimRight(label, ",")] = true
		}

		for _, path := range expectedPaths {
			require.True(t, actualPaths[path])
		}
	}

	// 1) Restrict the min & max path length such that we only include paths
	// with one hop other than the destination hop.
	paths, err := ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 1,
	})
	require.NoError(t, err)

	// We expect the B->D and C->D paths to be chosen.
	// The A->D path is not chosen since A does not advertise the route
	// blinding feature bit. The G->D path is not chosen since G does not
	// have any other known channels.
	assertPaths(paths, []string{
		"bob,dave",
		"charlie,dave",
	})

	// 2) Extend the search to include 2 hops other than the destination.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 2,
	})
	require.NoError(t, err)

	// We expect the following paths:
	// 	- B, D
	//	- F, B, D
	// 	- E, B, D
	// 	- C, D
	// 	- E, C, D
	assertPaths(paths, []string{
		"bob,dave",
		"frank,bob,dave",
		"eve,bob,dave",
		"charlie,dave",
		"eve,charlie,dave",
	})

	// 3) Extend the search even further and also increase the minimum path
	// length.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 2,
		maxNumHops: 3,
	})
	require.NoError(t, err)

	// We expect the following paths:
	//	- F, B, D
	// 	- E, B, D
	// 	- E, C, D
	// 	- B, E, C, D
	// 	- C, E, B, D
	assertPaths(paths, []string{
		"frank,bob,dave",
		"eve,bob,dave",
		"eve,charlie,dave",
		"bob,eve,charlie,dave",
		"charlie,eve,bob,dave",
	})

	// 4) Repeat the above test but instruct the function to never use
	// charlie.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 2,
		maxNumHops: 3,
		nodeOmissionSet: fn.NewSet[route.Vertex](
			ctx.keyFromAlias("charlie"),
		),
	})
	require.NoError(t, err)

	// We expect the following paths:
	//	- F, B, D
	// 	- E, B, D
	assertPaths(paths, []string{
		"frank,bob,dave",
		"eve,bob,dave",
	})

	// 5) We will also test the special case where the destination node
	// is also the recipient.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 0,
		maxNumHops: 0,
	})
	require.NoError(t, err)

	assertPaths(paths, []string{
		"dave",
	})

	// 6) Now, we will test some cases where the user manually specifies
	// the first few incoming channels of a route.
	//
	// 6.1) Let the user specify the B-D channel as the last hop with a
	// max of 1 hop.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 1,
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("bob", "dave"),
		},
	})
	require.NoError(t, err)

	// If the max number of hops is 1, then only the B->D path is chosen
	assertPaths(paths, []string{
		"bob,dave",
	})

	// 6.2) Extend the search to include 2 hops along with the B-D channel
	// restriction.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 2,
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("bob", "dave"),
		},
	})
	require.NoError(t, err)

	// We expect the following paths:
	//	- B, D
	//	- F, B, D
	// 	- E, B, D
	assertPaths(paths, []string{
		"bob,dave",
		"frank,bob,dave",
		"eve,bob,dave",
	})

	// 6.3) Repeat the above test but instruct the function to never use
	// bob. This should fail since bob owns one of the channels in the
	// partially specified path.
	_, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops:      1,
		maxNumHops:      2,
		nodeOmissionSet: fn.NewSet(ctx.keyFromAlias("bob")),
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("bob", "dave"),
		},
	})
	require.ErrorContains(t, err, "cannot simultaneously be included in "+
		"the omission set and in the partially specified path")

	// 6.4) Repeat it again but this time omit frank and demonstrate that
	// the resulting set contains all the results from 6.2 except for the
	// frank path.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops:      1,
		maxNumHops:      2,
		nodeOmissionSet: fn.NewSet(ctx.keyFromAlias("frank")),
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("bob", "dave"),
		},
	})
	require.NoError(t, err)

	// We expect the following paths:
	//	- B, D
	// 	- E, B, D
	assertPaths(paths, []string{
		"bob,dave",
		"eve,bob,dave",
	})

	// 6.5) Users may specify channels to nodes that do not signal route
	// blinding (like A). So if we specify the A-D channel, we should get
	// valid paths.
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 4,
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("dave", "alice"),
		},
	})
	require.NoError(t, err)

	// We expect the following paths:
	// 	- A, D
	// 	- F, A, D
	// 	- B, F, A, D
	// 	- E, B, F, A, D
	assertPaths(paths, []string{
		"alice,dave",
		"frank,alice,dave",
		"bob,frank,alice,dave",
		"eve,bob,frank,alice,dave",
	})

	// 6.6) Assert that an error is returned if a user accidentally tries
	// to force a circular path.
	_, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 2,
		maxNumHops: 3,
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("dave", "alice"),
			ctx.nodePairChannel("alice", "frank"),
			ctx.nodePairChannel("frank", "bob"),
			ctx.nodePairChannel("bob", "dave"),
		},
	})
	require.ErrorContains(t, err, "circular route")

	// 6.7) Test specifying a chain of incoming channels. We specify
	// the following incoming list: [A->D, F->A].
	paths, err = ctx.findBlindedPaths(&blindedPathRestrictions{
		minNumHops: 1,
		maxNumHops: 4,
		incomingChainedChannels: []uint64{
			ctx.nodePairChannel("dave", "alice"),
			ctx.nodePairChannel("alice", "frank"),
		},
	})
	require.NoError(t, err)

	// We expect the following paths:
	// 	- F, A, D
	// 	- B, F, A, D
	// 	- E, B, F, A, D
	assertPaths(paths, []string{
		"frank,alice,dave",
		"bob,frank,alice,dave",
		"eve,bob,frank,alice,dave",
	})
}
