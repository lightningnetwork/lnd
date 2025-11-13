package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"math/rand"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const (
	// basicGraphFilePath is the file path for a basic graph used within
	// the tests. The basic graph consists of 5 nodes with 5 channels
	// connecting them.
	basicGraphFilePath = "testdata/basic_graph.json"

	testTimeout = 5 * time.Second
)

// TestAddProof checks that we can update the channel proof after channel
// info was added to the database.
func TestAddProof(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	ctx := createTestCtxSingleNode(t, 0)

	// Before creating out edge, we'll create two new nodes within the
	// network that the channel will connect.
	node1 := createTestNode(t)
	node2 := createTestNode(t)

	// In order to be able to add the edge we should have a valid funding
	// UTXO within the blockchain.
	script, fundingTx, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(), 100, 0,
	)
	require.NoError(t, err, "unable create channel edge")
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	// After utxo was recreated adding the edge without the proof.
	edge := &models.ChannelEdgeInfo{
		ChannelID:     chanID.ToUint64(),
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof:     nil,
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some(script),
	}
	copy(edge.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	require.NoError(t, ctx.builder.AddEdge(ctxb, edge))

	// Now we'll attempt to update the proof and check that it has been
	// properly updated.
	require.NoError(t, ctx.builder.AddProof(*chanID, &testAuthProof))

	info, _, _, err := ctx.builder.GetChannelByID(*chanID)
	require.NoError(t, err, "unable to get channel")
	require.NotNil(t, info.AuthProof)
}

// TestIgnoreNodeAnnouncement tests that adding a node to the router that is
// not known from any channel announcement, leads to the announcement being
// ignored.
func TestIgnoreNodeAnnouncement(t *testing.T) {
	t.Parallel()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	pub := priv1.PubKey()
	node := models.NewV1Node(
		route.NewVertex(pub), &models.NodeV1Fields{
			Addresses:    testAddrs,
			AuthSigBytes: testSig.Serialize(),
			Features:     testFeatures.RawFeatureVector,
			LastUpdate:   time.Unix(123, 0),
			Color:        color.RGBA{1, 2, 3, 0},
			Alias:        "node11",
		},
	)

	err := ctx.builder.AddNode(t.Context(), node)
	if !IsError(err, ErrIgnored) {
		t.Fatalf("expected to get ErrIgnore, instead got: %v", err)
	}
}

// TestIgnoreChannelEdgePolicyForUnknownChannel checks that a router will
// ignore a channel policy for a channel not in the graph.
func TestIgnoreChannelEdgePolicyForUnknownChannel(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101

	// Setup an initially empty network.
	var testChannels []*testChannel
	testGraph, err := createTestGraphFromChannels(
		t, true, testChannels, "roasbeef",
	)
	require.NoError(t, err, "unable to create graph")

	ctx := createTestCtxFromGraphInstance(
		t, startingBlockHeight, testGraph, false,
	)

	var pub1 [33]byte
	copy(pub1[:], priv1.PubKey().SerializeCompressed())

	var pub2 [33]byte
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	// Add the edge between the two unknown nodes to the graph, and check
	// that the nodes are found after the fact.
	script, fundingTx, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(), 10000, 500,
	)
	require.NoError(t, err, "unable to create channel edge")
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &models.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
		Features:         lnwire.EmptyFeatureVector(),
		FundingScript:    fn.Some(script),
	}
	edgePolicy := &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                testTime,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}

	// Attempt to update the edge. This should be ignored, since the edge
	// is not yet added to the router.
	err = ctx.builder.UpdateEdge(ctxb, edgePolicy)
	if !IsError(err, ErrIgnored) {
		t.Fatalf("expected to get ErrIgnore, instead got: %v", err)
	}

	// Add the edge.
	require.NoErrorf(t, ctx.builder.AddEdge(ctxb, edge),
		"expected to be able to add edge to the channel graph, even "+
			"though the vertexes were unknown: %v.", err)

	// Now updating the edge policy should succeed.
	require.NoError(t, ctx.builder.UpdateEdge(ctxb, edgePolicy))
}

// TestWakeUpOnStaleBranch tests that upon startup of the ChannelRouter, if the
// chain previously reflected in the channel graph is stale (overtaken by a
// longer chain), the channel router will prune the graph for any channels
// confirmed on the stale chain, and resync to the main chain.
func TestWakeUpOnStaleBranch(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxSingleNode(t, startingBlockHeight)

	const chanValue = 10000

	// chanID1 will not be reorged out.
	var chanID1 uint64

	// chanID2 will be reorged out.
	var chanID2 uint64

	// Create 10 common blocks, confirming chanID1.
	var fundingScript1 []byte
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := startingBlockHeight + i
		if i == 5 {
			script, fundingTx, _, chanID, err := createChannelEdge(
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height,
			)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID1 = chanID.ToUint64()
			fundingScript1 = script
		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{}, t)
	}

	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	_, forkHeight, err := ctx.chain.GetBestBlock()
	require.NoError(t, err, "unable to ge best block")

	// Create 10 blocks on the minority chain, confirming chanID2.
	var fundingScript2 []byte
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		if i == 5 {
			script, fundingTx, _, chanID, err := createChannelEdge(
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID2 = chanID.ToUint64()
			fundingScript2 = script
		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{}, t)
	}
	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	// Now add the two edges to the channel graph, and check that they
	// correctly show up in the database.
	node1 := createTestNode(t)
	node2 := createTestNode(t)

	edge1 := &models.ChannelEdgeInfo{
		ChannelID:     chanID1,
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some(fundingScript1),
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.builder.AddEdge(ctxb, edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge2 := &models.ChannelEdgeInfo{
		ChannelID:     chanID2,
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some(fundingScript2),
	}
	copy(edge2.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge2.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.builder.AddEdge(ctxb, edge2); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Check that the fundingTxs are in the graph db.
	_, _, has, isZombie, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// Stop the router, so we can reorg the chain while its offline.
	if err := ctx.builder.Stop(); err != nil {
		t.Fatalf("unable to stop router: %v", err)
	}

	// Create a 15 block fork.
	for i := uint32(1); i <= 15; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
	}

	// Give time to process new blocks.
	time.Sleep(time.Millisecond * 500)

	selfNode, err := ctx.graph.SourceNode(t.Context())
	require.NoError(t, err)

	// Create new router with same graph database.
	router, err := NewBuilder(&Config{
		SelfNode:           selfNode.PubKeyBytes,
		Graph:              ctx.graph,
		Chain:              ctx.chain,
		ChainView:          ctx.chainView,
		ChannelPruneExpiry: time.Hour * 24,
		GraphPruneInterval: time.Hour * 2,

		// We'll set the delay to zero to prune immediately.
		FirstTimePruneDelay: 0,
		IsAlias: func(scid lnwire.ShortChannelID) bool {
			return false
		},
	})
	require.NoError(t, err)

	// It should resync to the longer chain on startup.
	if err := router.Start(); err != nil {
		t.Fatalf("unable to start router: %v", err)
	}

	// The channel with chanID2 should not be in the database anymore,
	// since it is not confirmed on the longest chain. chanID1 should
	// still be.
	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID1)
	require.NoError(t, err)

	if !has {
		t.Fatalf("did not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
	}
	if isZombie {
		t.Fatal("reorged edge should not be marked as zombie")
	}
}

// TestDisconnectedBlocks checks that the router handles a reorg happening when
// it is active.
func TestDisconnectedBlocks(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxSingleNode(t, startingBlockHeight)

	const chanValue = 10000

	// chanID1 will not be reorged out, while chanID2 will be reorged out.
	var chanID1, chanID2 uint64

	// Create 10 common blocks, confirming chanID1.
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := startingBlockHeight + i
		if i == 5 {
			_, fundingTx, _, chanID, err := createChannelEdge(
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height,
			)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID1 = chanID.ToUint64()
		}
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{}, t)
	}

	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	_, forkHeight, err := ctx.chain.GetBestBlock()
	require.NoError(t, err, "unable to get best block")

	// Create 10 blocks on the minority chain, confirming chanID2.
	var minorityChain []*wire.MsgBlock
	for i := uint32(1); i <= 10; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		if i == 5 {
			_, fundingTx, _, chanID, err := createChannelEdge(
				bitcoinKey1.SerializeCompressed(),
				bitcoinKey2.SerializeCompressed(),
				chanValue, height,
			)
			if err != nil {
				t.Fatalf("unable create channel edge: %v", err)
			}
			block.Transactions = append(block.Transactions,
				fundingTx)
			chanID2 = chanID.ToUint64()
		}
		minorityChain = append(minorityChain, block)
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			[]*wire.MsgTx{}, t)
	}
	// Give time to process new blocks
	time.Sleep(time.Millisecond * 500)

	// Now add the two edges to the channel graph, and check that they
	// correctly show up in the database.
	node1 := createTestNode(t)
	node2 := createTestNode(t)

	edge1 := &models.ChannelEdgeInfo{
		ChannelID:        chanID1,
		NodeKey1Bytes:    node1.PubKeyBytes,
		NodeKey2Bytes:    node2.PubKeyBytes,
		BitcoinKey1Bytes: node1.PubKeyBytes,
		BitcoinKey2Bytes: node2.PubKeyBytes,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some([]byte{}),
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.builder.AddEdge(ctxb, edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	edge2 := &models.ChannelEdgeInfo{
		ChannelID:        chanID2,
		NodeKey1Bytes:    node1.PubKeyBytes,
		NodeKey2Bytes:    node2.PubKeyBytes,
		BitcoinKey1Bytes: node1.PubKeyBytes,
		BitcoinKey2Bytes: node2.PubKeyBytes,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some([]byte{}),
	}
	copy(edge2.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge2.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())

	if err := ctx.builder.AddEdge(ctxb, edge2); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Check that the fundingTxs are in the graph db.
	_, _, has, isZombie, err := ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if !has {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// Create a 15 block fork. We first let the chainView notify the router
	// about stale blocks, before sending the now connected blocks. We do
	// this because we expect this order from the chainview.
	ctx.chainView.notifyStaleBlockAck = make(chan struct{}, 1)
	for i := len(minorityChain) - 1; i >= 0; i-- {
		block := minorityChain[i]
		height := uint32(forkHeight) + uint32(i) + 1
		ctx.chainView.notifyStaleBlock(block.BlockHash(), height,
			block.Transactions, t)
		<-ctx.chainView.notifyStaleBlockAck
	}

	time.Sleep(time.Second * 2)

	ctx.chainView.notifyBlockAck = make(chan struct{}, 1)
	for i := uint32(1); i <= 15; i++ {
		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		height := uint32(forkHeight) + i
		ctx.chain.addBlock(block, height, rand.Uint32())
		ctx.chain.setBestBlock(int32(height))
		ctx.chainView.notifyBlock(block.BlockHash(), height,
			block.Transactions, t)
		<-ctx.chainView.notifyBlockAck
	}

	time.Sleep(time.Millisecond * 500)

	// chanID2 should not be in the database anymore, since it is not
	// confirmed on the longest chain. chanID1 should still be.
	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID1)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !has {
		t.Fatalf("did not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	_, _, has, isZombie, err = ctx.graph.HasChannelEdge(chanID2)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID2)
	}
	if has {
		t.Fatalf("found edge in graph")
	}
	if isZombie {
		t.Fatal("reorged edge should not be marked as zombie")
	}
}

// TestChansClosedOfflinePruneGraph tests that if channels we know of are
// closed while we're offline, then once we resume operation of the
// ChannelRouter, then the channels are properly pruned.
func TestChansClosedOfflinePruneGraph(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxSingleNode(t, startingBlockHeight)

	const chanValue = 10000

	// First, we'll create a channel, to be mined shortly at height 102.
	block102 := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	}
	nextHeight := startingBlockHeight + 1
	script, fundingTx1, chanUTXO, chanID1, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		chanValue, uint32(nextHeight),
	)
	require.NoError(t, err, "unable create channel edge")
	block102.Transactions = append(block102.Transactions, fundingTx1)
	ctx.chain.addBlock(block102, uint32(nextHeight), rand.Uint32())
	ctx.chain.setBestBlock(int32(nextHeight))
	ctx.chainView.notifyBlock(block102.BlockHash(), uint32(nextHeight),
		[]*wire.MsgTx{}, t)

	// We'll now create the edges and nodes within the database required
	// for the ChannelRouter to properly recognize the channel we added
	// above.
	node1 := createTestNode(t)
	node2 := createTestNode(t)

	edge1 := &models.ChannelEdgeInfo{
		ChannelID:     chanID1.ToUint64(),
		NodeKey1Bytes: node1.PubKeyBytes,
		NodeKey2Bytes: node2.PubKeyBytes,
		AuthProof: &models.ChannelAuthProof{
			NodeSig1Bytes:    testSig.Serialize(),
			NodeSig2Bytes:    testSig.Serialize(),
			BitcoinSig1Bytes: testSig.Serialize(),
			BitcoinSig2Bytes: testSig.Serialize(),
		},
		ChannelPoint:  *chanUTXO,
		Capacity:      chanValue,
		Features:      lnwire.EmptyFeatureVector(),
		FundingScript: fn.Some(script),
	}
	copy(edge1.BitcoinKey1Bytes[:], bitcoinKey1.SerializeCompressed())
	copy(edge1.BitcoinKey2Bytes[:], bitcoinKey2.SerializeCompressed())
	if err := ctx.builder.AddEdge(ctxb, edge1); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// The router should now be aware of the channel we created above.
	_, _, hasChan, isZombie, err := ctx.graph.HasChannelEdge(
		chanID1.ToUint64(),
	)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if !hasChan {
		t.Fatalf("could not find edge in graph")
	}
	if isZombie {
		t.Fatal("edge was marked as zombie")
	}

	// With the transaction included, and the router's database state
	// updated, we'll now mine 5 additional blocks on top of it.
	for i := 0; i < 5; i++ {
		nextHeight++

		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}
		ctx.chain.addBlock(block, uint32(nextHeight), rand.Uint32())
		ctx.chain.setBestBlock(int32(nextHeight))
		ctx.chainView.notifyBlock(block.BlockHash(), uint32(nextHeight),
			[]*wire.MsgTx{}, t)
	}

	// At this point, our starting height should be 107.
	_, chainHeight, err := ctx.chain.GetBestBlock()
	require.NoError(t, err, "unable to get best block")
	if chainHeight != 107 {
		t.Fatalf("incorrect chain height: expected %v, got %v",
			107, chainHeight)
	}

	// Next, we'll "shut down" the router in order to simulate downtime.
	if err := ctx.builder.Stop(); err != nil {
		t.Fatalf("unable to shutdown router: %v", err)
	}

	// While the router is "offline" we'll mine 5 additional blocks, with
	// the second block closing the channel we created above.
	for i := 0; i < 5; i++ {
		nextHeight++

		block := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
		}

		if i == 2 {
			// For the second block, we'll add a transaction that
			// closes the channel we created above by spending the
			// output.
			closingTx := wire.NewMsgTx(2)
			closingTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: *chanUTXO,
			})
			block.Transactions = append(block.Transactions,
				closingTx)
		}

		ctx.chain.addBlock(block, uint32(nextHeight), rand.Uint32())
		ctx.chain.setBestBlock(int32(nextHeight))
		ctx.chainView.notifyBlock(block.BlockHash(), uint32(nextHeight),
			[]*wire.MsgTx{}, t)
	}

	// At this point, our starting height should be 112.
	_, chainHeight, err = ctx.chain.GetBestBlock()
	require.NoError(t, err, "unable to get best block")
	if chainHeight != 112 {
		t.Fatalf("incorrect chain height: expected %v, got %v",
			112, chainHeight)
	}

	// Now we'll re-start the ChannelRouter. It should recognize that it's
	// behind the main chain and prune all the blocks that it missed while
	// it was down.
	ctx.RestartBuilder(t)

	// At this point, the channel that was pruned should no longer be known
	// by the router.
	_, _, hasChan, isZombie, err = ctx.graph.HasChannelEdge(
		chanID1.ToUint64(),
	)
	if err != nil {
		t.Fatalf("error looking for edge: %v", chanID1)
	}
	if hasChan {
		t.Fatalf("channel was found in graph but shouldn't have been")
	}
	if isZombie {
		t.Fatal("closed channel should not be marked as zombie")
	}
}

// TestPruneChannelGraphStaleEdges ensures that we properly prune stale edges
// from the channel graph.
func TestPruneChannelGraphStaleEdges(t *testing.T) {
	t.Parallel()

	freshTimestamp := time.Now()
	staleTimestamp := time.Unix(0, 0)

	// We'll create the following test graph so that two of the channels
	// are pruned.
	testChannels := []*testChannel{
		// No edges.
		{
			Node1:     &testChannelEnd{Alias: "a"},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Only one edge with a stale timestamp.
		{
			Node1: &testChannelEnd{
				Alias: "d",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: staleTimestamp,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 2,
		},

		// Only one edge with a stale timestamp, but it's the source
		// node so it won't get pruned.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: staleTimestamp,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 3,
		},

		// Only one edge with a fresh timestamp.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: freshTimestamp,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 4,
		},

		// One edge fresh, one edge stale. This will be pruned with
		// strict pruning activated.
		{
			Node1: &testChannelEnd{
				Alias: "c",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: freshTimestamp,
				},
			},
			Node2: &testChannelEnd{
				Alias: "d",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: staleTimestamp,
				},
			},
			Capacity:  100000,
			ChannelID: 5,
		},

		// Both edges fresh.
		symmetricTestChannel("g", "h", 100000, &testChannelPolicy{
			LastUpdate: freshTimestamp,
		}, 6),

		// Both edges stale, only one pruned. This should be pruned for
		// both normal and strict pruning.
		symmetricTestChannel("e", "f", 100000, &testChannelPolicy{
			LastUpdate: staleTimestamp,
		}, 7),
	}

	for _, strictPruning := range []bool{true, false} {
		// We'll create our test graph and router backed with these test
		// channels we've created.
		testGraph, err := createTestGraphFromChannels(
			t, true, testChannels, "a",
		)
		if err != nil {
			t.Fatalf("unable to create test graph: %v", err)
		}

		const startingHeight = 100
		ctx := createTestCtxFromGraphInstance(
			t, startingHeight, testGraph, strictPruning,
		)

		// All of the channels should exist before pruning them.
		assertChannelsPruned(t, ctx.graph, testChannels)

		// Proceed to prune the channels - only the last one should be
		// pruned.
		if err := ctx.builder.pruneZombieChans(); err != nil {
			t.Fatalf("unable to prune zombie channels: %v", err)
		}

		// We expect channels that have either both edges stale, or one
		// edge stale with both known.
		var prunedChannels []uint64
		if strictPruning {
			prunedChannels = []uint64{2, 5, 7}
		} else {
			prunedChannels = []uint64{2, 7}
		}
		assertChannelsPruned(
			t, ctx.graph, testChannels, prunedChannels...,
		)
	}
}

// TestPruneChannelGraphDoubleDisabled test that we can properly prune channels
// with both edges disabled from our channel graph.
func TestPruneChannelGraphDoubleDisabled(t *testing.T) {
	t.Parallel()

	t.Run("no_assumechannelvalid", func(t *testing.T) {
		testPruneChannelGraphDoubleDisabled(t, false)
	})
	t.Run("assumechannelvalid", func(t *testing.T) {
		testPruneChannelGraphDoubleDisabled(t, true)
	})
}

func testPruneChannelGraphDoubleDisabled(t *testing.T, assumeValid bool) {
	timestamp := time.Now()

	// nextTimeStamp is a helper closure that will return a new
	// timestamp each time it's called, this helps us create channel updates
	// with new timestamps so that we don't run into our SQL DB constraint
	// which only allows an update to a channel edge if the last update
	// timestamp is greater than the previous one.
	nextTimeStamp := func() time.Time {
		timestamp = timestamp.Add(time.Second)

		return timestamp
	}

	// We'll create the following test graph so that only the last channel
	// is pruned. We'll use a fresh timestamp to ensure they're not pruned
	// according to that heuristic.
	testChannels := []*testChannel{
		// Channel from self shouldn't be pruned.
		symmetricTestChannel(
			"self", "a", 100000, &testChannelPolicy{
				LastUpdate: nextTimeStamp(),
				Disabled:   true,
			}, 99,
		),

		// No edges.
		{
			Node1:     &testChannelEnd{Alias: "a"},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Only one edge disabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: nextTimeStamp(),
					Disabled:   true,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 2,
		},

		// Only one edge enabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: nextTimeStamp(),
					Disabled:   false,
				},
			},
			Node2:     &testChannelEnd{Alias: "b"},
			Capacity:  100000,
			ChannelID: 3,
		},

		// One edge disabled, one edge enabled.
		{
			Node1: &testChannelEnd{
				Alias: "a",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: nextTimeStamp(),
					Disabled:   true,
				},
			},
			Node2: &testChannelEnd{
				Alias: "b",
				testChannelPolicy: &testChannelPolicy{
					LastUpdate: nextTimeStamp(),
					Disabled:   false,
				},
			},
			Capacity:  100000,
			ChannelID: 1,
		},

		// Both edges enabled.
		symmetricTestChannel("c", "d", 100000, &testChannelPolicy{
			LastUpdate: nextTimeStamp(),
			Disabled:   false,
		}, 2),

		// Both edges disabled, only one pruned.
		symmetricTestChannel("e", "f", 100000, &testChannelPolicy{
			LastUpdate: nextTimeStamp(),
			Disabled:   true,
		}, 3),
	}

	// We'll create our test graph and router backed with these test
	// channels we've created.
	testGraph, err := createTestGraphFromChannels(
		t, true, testChannels, "self",
	)
	require.NoError(t, err, "unable to create test graph")

	const startingHeight = 100
	ctx := createTestCtxFromGraphInstanceAssumeValid(
		t, startingHeight, testGraph, assumeValid, false,
	)

	// All the channels should exist within the graph before pruning them
	// when not using AssumeChannelValid, otherwise we should have pruned
	// the last channel on startup.
	if !assumeValid {
		assertChannelsPruned(t, ctx.graph, testChannels)
	} else {
		// Sleep to allow the pruning to finish.
		time.Sleep(200 * time.Millisecond)

		prunedChannel := testChannels[len(testChannels)-1].ChannelID
		assertChannelsPruned(t, ctx.graph, testChannels, prunedChannel)
	}

	if err := ctx.builder.pruneZombieChans(); err != nil {
		t.Fatalf("unable to prune zombie channels: %v", err)
	}

	// If we attempted to prune them without AssumeChannelValid being set,
	// none should be pruned. Otherwise the last channel should still be
	// pruned.
	if !assumeValid {
		assertChannelsPruned(t, ctx.graph, testChannels)
	} else {
		prunedChannel := testChannels[len(testChannels)-1].ChannelID
		assertChannelsPruned(t, ctx.graph, testChannels, prunedChannel)
	}
}

// TestIsStaleNode tests that the IsStaleNode method properly detects stale
// node announcements.
func TestIsStaleNode(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxSingleNode(t, startingBlockHeight)

	// Before we can insert a node in to the database, we need to create a
	// channel that it's linked to.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	script, fundingTx, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500,
	)
	require.NoError(t, err, "unable to create channel edge")
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &models.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
		Features:         lnwire.EmptyFeatureVector(),
		FundingScript:    fn.Some(script),
	}
	if err := ctx.builder.AddEdge(ctxb, edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Before we add the node, if we query for staleness, we should get
	// false, as we haven't added the full node.
	updateTimeStamp := time.Unix(123, 0)
	if ctx.builder.IsStaleNode(ctxb, pub1, updateTimeStamp) {
		t.Fatalf("incorrectly detected node as stale")
	}

	// With the node stub in the database, we'll add the fully node
	// announcement to the database.
	n1 := models.NewV1Node(
		route.NewVertex(priv1.PubKey()), &models.NodeV1Fields{
			LastUpdate:   updateTimeStamp,
			Addresses:    testAddrs,
			Color:        color.RGBA{1, 2, 3, 0},
			Alias:        "node11",
			AuthSigBytes: testSig.Serialize(),
			Features:     testFeatures.RawFeatureVector,
		},
	)
	if err := ctx.builder.AddNode(t.Context(), n1); err != nil {
		t.Fatalf("could not add node: %v", err)
	}

	// If we use the same timestamp and query for staleness, we should get
	// true.
	if !ctx.builder.IsStaleNode(ctxb, pub1, updateTimeStamp) {
		t.Fatalf("failure to detect stale node update")
	}

	// If we update the timestamp and once again query for staleness, it
	// should report false.
	newTimeStamp := time.Unix(1234, 0)
	if ctx.builder.IsStaleNode(ctxb, pub1, newTimeStamp) {
		t.Fatalf("incorrectly detected node as stale")
	}
}

// TestIsKnownEdge tests that the IsKnownEdge method properly detects stale
// channel announcements.
func TestIsKnownEdge(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxSingleNode(t, startingBlockHeight)

	// First, we'll create a new channel edge (just the info) and insert it
	// into the database.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	script, fundingTx, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500,
	)
	require.NoError(t, err, "unable to create channel edge")
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	edge := &models.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
		FundingScript:    fn.Some(script),
		Features:         lnwire.EmptyFeatureVector(),
	}
	if err := ctx.builder.AddEdge(ctxb, edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// Now that the edge has been inserted, query is the router already
	// knows of the edge should return true.
	if !ctx.builder.IsKnownEdge(*chanID) {
		t.Fatalf("router should detect edge as known")
	}
}

// TestIsStaleEdgePolicy tests that the IsStaleEdgePolicy properly detects
// stale channel edge update announcements.
func TestIsStaleEdgePolicy(t *testing.T) {
	t.Parallel()
	ctxb := t.Context()

	const startingBlockHeight = 101
	ctx := createTestCtxFromFile(t, startingBlockHeight, basicGraphFilePath)

	// First, we'll create a new channel edge (just the info) and insert it
	// into the database.
	var (
		pub1 [33]byte
		pub2 [33]byte
	)
	copy(pub1[:], priv1.PubKey().SerializeCompressed())
	copy(pub2[:], priv2.PubKey().SerializeCompressed())

	script, fundingTx, _, chanID, err := createChannelEdge(
		bitcoinKey1.SerializeCompressed(),
		bitcoinKey2.SerializeCompressed(),
		10000, 500,
	)
	require.NoError(t, err, "unable to create channel edge")
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight, chanID.BlockHeight)

	// If we query for staleness before adding the edge, we should get
	// false.
	updateTimeStamp := time.Unix(123, 0)
	if ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
	if ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect fresh edge policy")
	}

	edge := &models.ChannelEdgeInfo{
		ChannelID:        chanID.ToUint64(),
		NodeKey1Bytes:    pub1,
		NodeKey2Bytes:    pub2,
		BitcoinKey1Bytes: pub1,
		BitcoinKey2Bytes: pub2,
		AuthProof:        nil,
		Features:         lnwire.EmptyFeatureVector(),
		FundingScript:    fn.Some(script),
	}
	if err := ctx.builder.AddEdge(ctxb, edge); err != nil {
		t.Fatalf("unable to add edge: %v", err)
	}

	// We'll also add two edge policies, one for each direction.
	edgePolicy := &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                updateTimeStamp,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 0
	if err := ctx.builder.UpdateEdge(ctxb, edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	edgePolicy = &models.ChannelEdgePolicy{
		SigBytes:                  testSig.Serialize(),
		ChannelID:                 edge.ChannelID,
		LastUpdate:                updateTimeStamp,
		TimeLockDelta:             10,
		MinHTLC:                   1,
		FeeBaseMSat:               10,
		FeeProportionalMillionths: 10000,
	}
	edgePolicy.ChannelFlags = 1
	if err := ctx.builder.UpdateEdge(ctxb, edgePolicy); err != nil {
		t.Fatalf("unable to update edge policy: %v", err)
	}

	// Now that the edges have been added, an identical (chanID, flag,
	// timestamp) tuple for each edge should be detected as a stale edge.
	if !ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect stale edge policy")
	}
	if !ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect stale edge policy")
	}

	// If we now update the timestamp for both edges, the router should
	// detect that this tuple represents a fresh edge.
	updateTimeStamp = time.Unix(9999, 0)
	if ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 0) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
	if ctx.builder.IsStaleEdgePolicy(*chanID, updateTimeStamp, 1) {
		t.Fatalf("router failed to detect fresh edge policy")
	}
}

// TestBlockDifferenceFix tests if when the router is behind on blocks, the
// router catches up to the best block head.
func TestBlockDifferenceFix(t *testing.T) {
	t.Parallel()

	initialBlockHeight := uint32(0)

	// Starting height here is set to 0, which is behind where we want to
	// be.
	ctx := createTestCtxSingleNode(t, initialBlockHeight)

	// Add initial block to our mini blockchain.
	block := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	}
	ctx.chain.addBlock(block, initialBlockHeight, rand.Uint32())

	// Let's generate a new block of height 5, 5 above where our node is at.
	newBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{},
	}
	newBlockHeight := uint32(5)

	blockDifference := newBlockHeight - initialBlockHeight

	ctx.chainView.notifyBlockAck = make(chan struct{}, 1)

	ctx.chain.addBlock(newBlock, newBlockHeight, rand.Uint32())
	ctx.chain.setBestBlock(int32(newBlockHeight))
	ctx.chainView.notifyBlock(block.BlockHash(), newBlockHeight,
		[]*wire.MsgTx{}, t)

	<-ctx.chainView.notifyBlockAck

	// At this point, the chain notifier should have noticed that we're
	// behind on blocks, and will send the n missing blocks that we
	// need to the client's epochs channel. Let's replicate this
	// functionality.
	for i := 0; i < int(blockDifference); i++ {
		currBlockHeight := int32(i + 1)

		nonce := rand.Uint32()

		newBlock := &wire.MsgBlock{
			Transactions: []*wire.MsgTx{},
			Header:       wire.BlockHeader{Nonce: nonce},
		}
		ctx.chain.addBlock(newBlock, uint32(currBlockHeight), nonce)
		currHash := newBlock.Header.BlockHash()

		newEpoch := &chainntnfs.BlockEpoch{
			Height: currBlockHeight,
			Hash:   &currHash,
		}

		ctx.notifier.EpochChan <- newEpoch

		ctx.chainView.notifyBlock(currHash,
			uint32(currBlockHeight), block.Transactions, t)

		<-ctx.chainView.notifyBlockAck
	}

	err := wait.NoError(func() error {
		// Then router height should be updated to the latest block.
		if ctx.builder.bestHeight.Load() != newBlockHeight {
			return fmt.Errorf("height should have been updated "+
				"to %v, instead got %v", newBlockHeight,
				ctx.builder.bestHeight.Load())
		}

		return nil
	}, testTimeout)
	require.NoError(t, err, "block height wasn't updated")
}

func createTestCtxFromFile(t *testing.T,
	startingHeight uint32, testGraph string) *testCtx {

	// We'll attempt to locate and parse out the file
	// that encodes the graph that our tests should be run against.
	graphInstance, err := parseTestGraph(t, true, testGraph)
	require.NoError(t, err, "unable to create test graph")

	return createTestCtxFromGraphInstance(
		t, startingHeight, graphInstance, false,
	)
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
	graph := graphdb.MakeTestGraph(
		t, graphdb.WithUseGraphCache(useCache),
	)

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

			// Set the selected source node.
			if err := graph.SetSourceNode(ctx, source); err != nil {
				return nil, err
			}

			continue
		}

		// With the node fully parsed, add it as a vertex within the
		// graph.
		if err := graph.AddNode(ctx, dbNode); err != nil {
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
			Capacity:     btcutil.Amount(edge.Capacity),
			Features:     lnwire.EmptyFeatureVector(),
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
			SigBytes: testSig.Serialize(),
			MessageFlags: lnwire.ChanUpdateMsgFlags(
				edge.MessageFlags,
			),
			ChannelFlags:  channelFlags,
			ChannelID:     edge.ChannelID,
			LastUpdate:    testTime,
			TimeLockDelta: edge.Expiry,
			MinHTLC: lnwire.MilliSatoshi(
				edge.MinHTLC,
			),
			MaxHTLC: lnwire.MilliSatoshi(
				edge.MaxHTLC,
			),
			FeeBaseMSat: lnwire.MilliSatoshi(
				edge.FeeBaseMsat,
			),
			FeeProportionalMillionths: lnwire.MilliSatoshi(
				edge.FeeRate,
			),
			ToNode: targetNode,
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
		aliasMap:   aliasMap,
		privKeyMap: privKeyMap,
		channelIDs: channelIDs,
		links:      links,
	}, nil
}

// testGraph is the struct which corresponds to the JSON format used to encode
// graphs within the files in the testdata directory.
//
// TODO(roasbeef): add test graph auto-generator.
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

type testChannel struct {
	Node1     *testChannelEnd
	Node2     *testChannelEnd
	Capacity  btcutil.Amount
	ChannelID uint64
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

// assertChannelsPruned ensures that only the given channels are pruned from the
// graph out of the set of all channels.
func assertChannelsPruned(t *testing.T, graph *graphdb.ChannelGraph,
	channels []*testChannel, prunedChanIDs ...uint64) {

	t.Helper()

	pruned := make(map[uint64]struct{}, len(channels))
	for _, chanID := range prunedChanIDs {
		pruned[chanID] = struct{}{}
	}

	for _, channel := range channels {
		_, shouldPrune := pruned[channel.ChannelID]
		_, _, exists, isZombie, err := graph.HasChannelEdge(
			channel.ChannelID,
		)
		if err != nil {
			t.Fatalf("unable to determine existence of "+
				"channel=%v in the graph: %v",
				channel.ChannelID, err)
		}
		if !shouldPrune && !exists {
			t.Fatalf("expected channel=%v to exist within "+
				"the graph", channel.ChannelID)
		}
		if shouldPrune && exists {
			t.Fatalf("expected channel=%v to not exist "+
				"within the graph", channel.ChannelID)
		}
		if !shouldPrune && isZombie {
			t.Fatalf("expected channel=%v to not be marked "+
				"as zombie", channel.ChannelID)
		}
		if shouldPrune && !isZombie {
			t.Fatalf("expected channel=%v to be marked as "+
				"zombie", channel.ChannelID)
		}
	}
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

// createTestGraphFromChannels returns a fully populated ChannelGraph based on a
// set of test channels. Additional required information like keys are derived
// in a deterministic way and added to the channel graph. A list of nodes is not
// required and derived from the channel data. The goal is to keep instantiating
// a test channel graph as light weight as possible.
func createTestGraphFromChannels(t *testing.T, useCache bool,
	testChannels []*testChannel, source string) (*testGraphInstance,
	error) {

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
	graph := graphdb.MakeTestGraph(
		t, graphdb.WithUseGraphCache(useCache),
	)

	aliasMap := make(map[string]route.Vertex)
	privKeyMap := make(map[string]*btcec.PrivateKey)

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
	err = addNodeWithAlias(source, lnwire.EmptyFeatureVector())
	if err != nil {
		return nil, err
	}

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

		// We first insert the existence of the edge between the two
		// nodes.
		edgeInfo := models.ChannelEdgeInfo{
			ChannelID:    channelID,
			AuthProof:    &testAuthProof,
			ChannelPoint: *fundingPoint,
			Capacity:     testChannel.Capacity,

			NodeKey1Bytes:    node1Vertex,
			BitcoinKey1Bytes: node1Vertex,
			NodeKey2Bytes:    node2Vertex,
			BitcoinKey2Bytes: node2Vertex,
			Features:         lnwire.EmptyFeatureVector(),
		}

		err = graph.AddChannelEdge(ctx, &edgeInfo)
		if err != nil &&
			!errors.Is(err, graphdb.ErrEdgeAlreadyExist) {

			return nil, err
		}

		getExtraData := func(
			end *testChannelEnd) lnwire.ExtraOpaqueData {

			var extraData lnwire.ExtraOpaqueData
			inboundFee := lnwire.Fee{
				BaseFee: int32(end.InboundFeeBaseMsat),
				FeeRate: int32(end.InboundFeeRate),
			}
			require.NoError(t, extraData.PackRecords(&inboundFee))

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
				ExtraOpaqueData:           getExtraData(node2),
			}
			err := graph.UpdateEdgePolicy(ctx, edgePolicy)
			if err != nil {
				return nil, err
			}
		}

		channelID++ //nolint:ineffassign
	}

	return &testGraphInstance{
		graph:      graph,
		aliasMap:   aliasMap,
		privKeyMap: privKeyMap,
		links:      links,
	}, nil
}

type mockLink struct {
	htlcswitch.ChannelLink
	bandwidth         lnwire.MilliSatoshi
	mayAddOutgoingErr error
	ineligible        bool
}

// Bandwidth returns the bandwidth the mock was configured with.
func (m *mockLink) Bandwidth() lnwire.MilliSatoshi {
	return m.bandwidth
}

// EligibleToForward returns the mock's configured eligibility.
func (m *mockLink) EligibleToForward() bool {
	return !m.ineligible
}

// MayAddOutgoingHtlc returns the error configured in our mock.
func (m *mockLink) MayAddOutgoingHtlc(_ lnwire.MilliSatoshi) error {
	return m.mayAddOutgoingErr
}
