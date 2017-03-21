package routing

import (
	"fmt"
	"image/color"
	"net"
	"sync"
	"testing"
	"time"

	prand "math/rand"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	testAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.1:9000")

	testHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
)

func createGraphNode() (*channeldb.LightningNode, error) {
	updateTime := prand.Int63()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	pub := priv.PubKey().SerializeCompressed()
	return &channeldb.LightningNode{
		LastUpdate: time.Unix(updateTime, 0),
		Address:    testAddr,
		PubKey:     priv.PubKey(),
		Color:      color.RGBA{1, 2, 3, 0},
		Alias:      "kek" + string(pub[:]),
	}, nil
}

func createTestWireNode() (*lnwire.NodeAnnouncement, error) {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	pub := priv.PubKey().SerializeCompressed()

	alias, err := lnwire.NewAlias("kek" + string(pub[:]))
	if err != nil {
		return nil, err
	}

	return &lnwire.NodeAnnouncement{
		Timestamp: uint32(prand.Int31()),
		Address:   testAddr,
		NodeID:    priv.PubKey(),
		Alias:     alias,
	}, nil
}

func randEdgePolicyAnn(chanID lnwire.ChannelID) *lnwire.ChannelUpdateAnnouncement {

	return &lnwire.ChannelUpdateAnnouncement{
		Signature:                 testSig,
		ChannelID:                 chanID,
		Timestamp:                 uint32(prand.Int31()),
		TimeLockDelta:             uint16(prand.Int63()),
		HtlcMinimumMsat:           uint32(prand.Int31()),
		FeeBaseMsat:               uint32(prand.Int31()),
		FeeProportionalMillionths: uint32(prand.Int31()),
	}
}

func randChannelEdge(ctx *testCtx, chanValue btcutil.Amount,
	fundingHeight uint32) (*wire.MsgTx, wire.OutPoint, lnwire.ChannelID) {

	fundingTx := wire.NewMsgTx(2)
	fundingTx.TxOut = append(fundingTx.TxOut, &wire.TxOut{
		Value: int64(chanValue),
	})
	chanUtxo := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 0,
	}

	// With the utxo constructed, we'll mark it as closed.
	ctx.chain.addUtxo(chanUtxo, chanValue)

	// Our fake channel will be "confirmed" at height 101.
	chanID := lnwire.ChannelID{
		BlockHeight: fundingHeight,
		TxIndex:     0,
		TxPosition:  0,
	}

	return fundingTx, chanUtxo, chanID
}

type mockChain struct {
	blocks     map[chainhash.Hash]*wire.MsgBlock
	blockIndex map[uint32]chainhash.Hash

	utxos map[wire.OutPoint]wire.TxOut

	bestHeight int32
	bestHash   *chainhash.Hash

	sync.RWMutex
}

func newMockChain(currentHeight uint32) *mockChain {
	return &mockChain{
		bestHeight: int32(currentHeight),
		blocks:     make(map[chainhash.Hash]*wire.MsgBlock),
		utxos:      make(map[wire.OutPoint]wire.TxOut),
		blockIndex: make(map[uint32]chainhash.Hash),
	}
}

func (m *mockChain) setBestBlock(height int32) {
	m.Lock()
	defer m.Unlock()

	m.bestHeight = height
}

func (m *mockChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	m.RLock()
	defer m.RUnlock()

	return nil, m.bestHeight, nil
}

func (m *mockChain) GetTransaction(txid *chainhash.Hash) (*wire.MsgTx, error) {
	return nil, nil
}

func (m *mockChain) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	m.RLock()
	defer m.RUnlock()

	hash, ok := m.blockIndex[uint32(blockHeight)]
	if !ok {
		return nil, fmt.Errorf("can't find block hash, for "+
			"height %v", blockHeight)

	}

	return &hash, nil
}

func (m *mockChain) addUtxo(op wire.OutPoint, value btcutil.Amount) {
	m.Lock()
	m.utxos[op] = wire.TxOut{
		Value: int64(value),
	}
	m.Unlock()
}
func (m *mockChain) GetUtxo(txid *chainhash.Hash, index uint32) (*wire.TxOut, error) {
	m.RLock()
	defer m.RUnlock()

	op := wire.OutPoint{
		Hash:  *txid,
		Index: index,
	}

	utxo, ok := m.utxos[op]
	if !ok {
		return nil, fmt.Errorf("utxo not found")
	}

	return &utxo, nil
}

func (m *mockChain) addBlock(block *wire.MsgBlock, height uint32) {
	m.Lock()
	block.Header.Nonce = height
	hash := block.Header.BlockHash()
	m.blocks[hash] = block
	m.blockIndex[height] = hash
	m.Unlock()
}
func (m *mockChain) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	m.RLock()
	defer m.RUnlock()

	block, ok := m.blocks[*blockHash]
	if !ok {
		return nil, fmt.Errorf("block not found")
	}

	return block, nil
}

type mockNotifier struct {
	clientCounter uint32
	epochClients  map[uint32]chan *chainntnfs.BlockEpoch

	sync.RWMutex
}

func newMockNotifier() *mockNotifier {
	return &mockNotifier{
		epochClients: make(map[uint32]chan *chainntnfs.BlockEpoch),
	}
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	numConfs uint32) (*chainntnfs.ConfirmationEvent, error) {

	return nil, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint) (*chainntnfs.SpendEvent, error) {
	return nil, nil
}

func (m *mockNotifier) notifyBlock(hash chainhash.Hash, height uint32) {
	m.RLock()
	defer m.RUnlock()

	for _, client := range m.epochClients {
		client <- &chainntnfs.BlockEpoch{
			Height: int32(height),
			Hash:   &hash,
		}
	}
}

func (m *mockNotifier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	m.RLock()
	defer m.RUnlock()

	epochChan := make(chan *chainntnfs.BlockEpoch)
	clientID := m.clientCounter
	m.clientCounter++
	m.epochClients[clientID] = epochChan

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

// TestEdgeUpdateNotification tests that when edges are updated or discovered,
// a proper notification is sent of to all registered clients.
func TestEdgeUpdateNotification(t *testing.T) {
	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// First we'll create the utxo for the channel to be "closed"
	const chanValue = btcutil.Amount(10000)
	fundingTx, chanPoint, chanID := randChannelEdge(ctx, chanValue,
		startingBlockHeight)

	// We'll also add a record for the block that included our funding
	// transaction.
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight)

	// Next we'll create two test nodes that the fake channel will be open
	// between and add then as members of the channel graph.
	node1, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// Send the two node announcements to the channel router so they can be
	// validated and stored within the graph database.
	ctx.router.ProcessRoutingMessage(node1, node1.NodeID)
	ctx.router.ProcessRoutingMessage(node2, node2.NodeID)

	// Finally, to conclude our test set up, we'll create a channel
	// announcement to announce the created channel between the two nodes.
	channelAnn := &lnwire.ChannelAnnouncement{
		FirstNodeSig:     testSig,
		SecondNodeSig:    testSig,
		ChannelID:        chanID,
		FirstBitcoinSig:  testSig,
		SecondBitcoinSig: testSig,
		FirstNodeID:      node1.NodeID,
		SecondNodeID:     node2.NodeID,
		FirstBitcoinKey:  node1.NodeID,
		SecondBitcoinKey: node2.NodeID,
	}
	ctx.router.ProcessRoutingMessage(channelAnn, node1.NodeID)

	// With the channel edge now in place, we'll subscribe for topology
	// notifications.
	ntfnClient, err := ctx.router.SubscribeTopology()
	if err != nil {
		t.Fatalf("unable to subscribe for channel notifications: %v", err)
	}

	// Create random policy edges that are stemmed to the channel id
	// created above.
	edge1 := randEdgePolicyAnn(chanID)
	edge1.Flags = 0
	edge2 := randEdgePolicyAnn(chanID)
	edge2.Flags = 1

	ctx.router.ProcessRoutingMessage(edge1, node1.NodeID)
	ctx.router.ProcessRoutingMessage(edge2, node2.NodeID)

	assertEdgeAnnCorrect := func(t *testing.T, edgeUpdate *ChannelEdgeUpdate,
		edgeAnn *lnwire.ChannelUpdateAnnouncement) {
		if edgeUpdate.ChanID != edgeAnn.ChannelID.ToUint64() {
			t.Fatalf("channel ID of edge doesn't match: "+
				"expected %v, got %v", chanID.ToUint64(), edgeUpdate.ChanID)
		}
		if edgeUpdate.ChanPoint != chanPoint {
			t.Fatalf("channel don't match: expected %v, got %v",
				chanPoint, edgeUpdate.ChanPoint)
		}
		// TODO(roasbeef): this is a hack, needs to be removed
		// after commitment fees are dynamic.
		if edgeUpdate.Capacity != chanValue-5000 {
			t.Fatalf("capacity of edge doesn't match: "+
				"expected %v, got %v", chanValue, edgeUpdate.Capacity)
		}
		if edgeUpdate.MinHTLC != btcutil.Amount(edgeAnn.HtlcMinimumMsat) {
			t.Fatalf("min HTLC of edge doesn't match: "+
				"expected %v, got %v", btcutil.Amount(edgeAnn.HtlcMinimumMsat),
				edgeUpdate.MinHTLC)
		}
		if edgeUpdate.BaseFee != btcutil.Amount(edgeAnn.FeeBaseMsat) {
			t.Fatalf("base fee of edge doesn't match: "+
				"expected %v, got %v", edgeAnn.FeeBaseMsat,
				edgeUpdate.BaseFee)
		}
		if edgeUpdate.FeeRate != btcutil.Amount(edgeAnn.FeeProportionalMillionths) {
			t.Fatalf("fee rate of edge doesn't match: "+
				"expected %v, got %v", edgeAnn.FeeProportionalMillionths,
				edgeUpdate.FeeRate)
		}
		if edgeUpdate.TimeLockDelta != edgeAnn.TimeLockDelta {
			t.Fatalf("time lock delta of edge doesn't match: "+
				"expected %v, got %v", edgeAnn.TimeLockDelta,
				edgeUpdate.TimeLockDelta)
		}
	}

	const numEdgePolicies = 2
	for i := 0; i < numEdgePolicies; i++ {
		select {
		case ntfn := <-ntfnClient.TopologyChanges:
			edgeUpdate := ntfn.ChannelEdgeUpdates[0]
			if i == 0 {
				assertEdgeAnnCorrect(t, edgeUpdate, edge1)
				if !edgeUpdate.AdvertisingNode.IsEqual(node1.NodeID) {
					t.Fatalf("advertising node mismatch")
				}
				if !edgeUpdate.ConnectingNode.IsEqual(node2.NodeID) {
					t.Fatalf("connecting node mismatch")
				}

				continue
			}

			assertEdgeAnnCorrect(t, edgeUpdate, edge2)
			if !edgeUpdate.ConnectingNode.IsEqual(node1.NodeID) {
				t.Fatalf("connecting node mismatch")
			}
			if !edgeUpdate.AdvertisingNode.IsEqual(node2.NodeID) {
				t.Fatalf("advertising node mismatch")
			}
		case <-time.After(time.Second * 5):
			t.Fatalf("update not received")
		}
	}
}

// TestNodeUpdateNotification tests that notifications are sent out when nodes
// either join the network for the first time, or update their authenticated
// attributes with new data.
func TestNodeUpdateNotification(t *testing.T) {
	ctx, cleanUp, err := createTestCtx(1)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Create a new client to receive notifications.
	ntfnClient, err := ctx.router.SubscribeTopology()
	if err != nil {
		t.Fatalf("unable to subscribe for channel notifications: %v", err)
	}

	// Create two random nodes to add to send as node announcement messages
	// to trigger notifications.
	node1Ann, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2Ann, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// Send both announcement message to the channel router.
	ctx.router.ProcessRoutingMessage(node1Ann, node1Ann.NodeID)
	ctx.router.ProcessRoutingMessage(node2Ann, node2Ann.NodeID)

	assertNodeNtfnCorrect := func(t *testing.T, ann *lnwire.NodeAnnouncement,
		ntfns []*NetworkNodeUpdate) {

		// For each processed announcement we should only receive a
		// single announcement in a batch.
		if len(ntfns) != 1 {
			t.Fatalf("expected 1 notification, instead have %v",
				len(ntfns))
		}

		// The notification received should directly map the
		// announcement originally sent.
		nodeNtfn := ntfns[0]
		if nodeNtfn.Addresses[0] != ann.Address {
			t.Fatalf("node address doesn't match: expected %v, got %v",
				nodeNtfn.Addresses[0], ann.Address)
		}
		if !nodeNtfn.IdentityKey.IsEqual(ann.NodeID) {
			t.Fatalf("node identity keys don't match: expected %x, "+
				"got %x", ann.NodeID.SerializeCompressed(),
				nodeNtfn.IdentityKey.SerializeCompressed())
		}
		if nodeNtfn.Alias != ann.Alias.String() {
			t.Fatalf("node alias doesn't match: expected %v, got %v",
				ann.Alias.String(), nodeNtfn.Alias)
		}
	}

	// Exactly two notifications should be sent, each corresponding to the
	// node announcement messages sent above.
	const numAnns = 2
	for i := 0; i < numAnns; i++ {
		select {
		case ntfn := <-ntfnClient.TopologyChanges:
			if i == 0 {
				assertNodeNtfnCorrect(t, node1Ann, ntfn.NodeUpdates)
				continue
			}

			assertNodeNtfnCorrect(t, node2Ann, ntfn.NodeUpdates)
		case <-time.After(time.Second * 5):
		}
	}

	// If we receive a new update from a node (with a higher timestamp),
	// then it should trigger a new notification.
	// TODO(roasbeef): assume monotonic time.
	nodeUpdateAnn := *node1Ann
	nodeUpdateAnn.Timestamp = node1Ann.Timestamp + 300

	// Send off the new node announcement to the channel router.
	ctx.router.ProcessRoutingMessage(&nodeUpdateAnn, node1Ann.NodeID)

	// Once again a notification should be received reflecting the up to
	// date node announcement.
	select {
	case ntfn := <-ntfnClient.TopologyChanges:
		assertNodeNtfnCorrect(t, &nodeUpdateAnn, ntfn.NodeUpdates)
	case <-time.After(time.Second * 5):
	}
}

// TestNotificationCancellation tests that notifications are properly cancelled
// when the client wishes to exit.
func TestNotificationCancellation(t *testing.T) {
	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// Create a new client to receive notifications.
	ntfnClient, err := ctx.router.SubscribeTopology()
	if err != nil {
		t.Fatalf("unable to subscribe for channel notifications: %v", err)
	}

	// We'll create a fresh new node announcement to feed to the channel
	// router.
	node1Ann, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// Before we send the message to the channel router, we'll cancel the
	// notifications for this client. As a result, the notification
	// triggered by accepting this announcement shouldn't be sent to the
	// client.
	ntfnClient.Cancel()

	ctx.router.ProcessRoutingMessage(node1Ann, node1Ann.NodeID)

	select {
	// The notification shouldn't be sent, however, the channel should be
	// closed, causing the second read-value to be false.
	case _, ok := <-ntfnClient.TopologyChanges:
		if !ok {
			return
		}

		t.Fatalf("notification sent but shouldn't have been")

	case <-time.After(time.Second * 5):
		t.Fatalf("notification client never cancelled")
	}
}

// TestChannelCloseNotification tests that channel closure notifications are
// properly dispatched to all registered clients.
func TestChannelCloseNotification(t *testing.T) {
	const startingBlockHeight = 101
	ctx, cleanUp, err := createTestCtx(startingBlockHeight)
	defer cleanUp()
	if err != nil {
		t.Fatalf("unable to create router: %v", err)
	}

	// First we'll create the utxo for the channel to be "closed"
	const chanValue = btcutil.Amount(10000)
	fundingTx, chanUtxo, chanID := randChannelEdge(ctx, chanValue,
		startingBlockHeight)

	// We'll also add a record for the block that included our funding
	// transaction.
	fundingBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{fundingTx},
	}
	ctx.chain.addBlock(fundingBlock, chanID.BlockHeight)

	// Next we'll create two test nodes that the fake channel will be open
	// between and add then as members of the channel graph.
	node1, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	node2, err := createTestWireNode()
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}

	// Finally, to conclude our test set up, we'll create a channel
	// announcement to announce the created channel between the two nodes.
	channelAnn := lnwire.ChannelAnnouncement{
		FirstNodeSig:     testSig,
		SecondNodeSig:    testSig,
		ChannelID:        chanID,
		FirstBitcoinSig:  testSig,
		SecondBitcoinSig: testSig,
		FirstNodeID:      node1.NodeID,
		SecondNodeID:     node2.NodeID,
		FirstBitcoinKey:  node1.NodeID,
		SecondBitcoinKey: node2.NodeID,
	}
	ctx.router.ProcessRoutingMessage(&channelAnn, node1.NodeID)

	// With the channel edge now in place, we'll subscribe for topology
	// notifications.
	ntfnClient, err := ctx.router.SubscribeTopology()
	if err != nil {
		t.Fatalf("unable to subscribe for channel notifications: %v", err)
	}

	// Next, we'll simulate the closure of our channel by generating a new
	// block at height 102 which spends the original multi-sig output of
	// the channel.
	blockHeight := uint32(102)
	newBlock := &wire.MsgBlock{
		Transactions: []*wire.MsgTx{
			{
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: chanUtxo,
					},
				},
			},
		},
	}
	ctx.chain.addBlock(newBlock, blockHeight)
	ctx.notifier.notifyBlock(newBlock.Header.BlockHash(), blockHeight)

	// The notification registered above should be sent, if not we'll time
	// out and mark the test as failed.
	select {
	case ntfn := <-ntfnClient.TopologyChanges:
		// We should have exactly a single notification for the channel
		// "closed" above.
		closedChans := ntfn.ClosedChannels
		if len(closedChans) == 0 {
			t.Fatalf("close channel ntfn not populated")
		} else if len(closedChans) != 1 {
			t.Fatalf("only one should've been detected as closed, "+
				"instead %v were", len(closedChans))
		}

		// Ensure that the notification we received includes the proper
		// update the for the channel that was closed in the generated
		// block.
		closedChan := closedChans[0]
		if closedChan.ChanID != chanID.ToUint64() {
			t.Fatalf("channel ID of closed channel doesn't match: "+
				"expected %v, got %v", chanID.ToUint64(), closedChan.ChanID)
		}
		// TODO(roasbeef): this is a hack, needs to be removed
		// after commitment fees are dynamic.
		if closedChan.Capacity != chanValue-5000 {
			t.Fatalf("capacity of closed channel doesn't match: "+
				"expected %v, got %v", chanValue, closedChan.Capacity)
		}
		if closedChan.ClosedHeight != blockHeight {
			t.Fatalf("close height of closed channel doesn't match: "+
				"expected %v, got %v", blockHeight, closedChan.ClosedHeight)
		}
		if closedChan.ChanPoint != chanUtxo {
			t.Fatalf("chan point of closed channel doesn't match: "+
				"expected %v, got %v", chanUtxo, closedChan.ChanPoint)
		}

	case <-time.After(time.Second * 5):
		t.Fatalf("notification not sent")
	}
}
