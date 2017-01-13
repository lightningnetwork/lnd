package routing

import (
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// FeeSchema is the set fee configuration for a Lighting Node on the network.
// Using the coefficients described within he schema, the required fee to
// forward outgoing payments can be derived.
//
// TODO(roasbeef): should be in switch instead?
type FeeSchema struct {
	// TODO(rosbeef): all these should be in msat instead

	// BaseFee is the base amount that will be chained for ANY payment
	// forwarded.
	BaseFee btcutil.Amount

	// FeeRate is the rate that will be charged for forwarding payments.
	// The fee rate has a granularity of 1/1000 th of a mili-satoshi, or a
	// millionth of a satoshi.
	FeeRate btcutil.Amount
}

// Config defines the configuration for the ChannelRouter. ALL elements within
// the configuration MUST be non-nil for the ChannelRouter to carry out its
// duties.
type Config struct {
	// Graph is the channel graph that the ChannelRouter will use to gather
	// metrics from and also to carry out path finding queries.
	// TODO(roasbeef): make into an interface
	Graph *channeldb.ChannelGraph

	// Chain is the router's source to the most up-to-date blockchain data.
	// All incoming advertised channels will be checked against the chain
	// to ensure that the channels advertised are still open.
	// TODO(roasbeef): remove after discovery service is in
	Chain lnwallet.BlockChainIO

	// Notifier is an instance of the ChainNotifier that the router uses to
	// received notifications of incoming blocks. With each new incoming
	// block found, the router may be able to partially prune the channel
	// graph as channels may have been pruned.
	// TODO(roasbeef): could possibly just replace this with an epoch
	// channel.
	Notifier chainntnfs.ChainNotifier

	// FeeSchema is the set fee schema that will be announced on to the
	// network.
	// TODO(roasbeef): should either be in discovery or switch
	FeeSchema *FeeSchema

	// Broadcast is a function that is used to broadcast a particular set
	// of messages to all peers that the daemon is connected to. If
	// supplied, the exclude parameter indicates that the target peer should
	// be excluded from the broadcast.
	Broadcast func(exclude *btcec.PublicKey, msg ...lnwire.Message) error

	// SendMessages is a function which allows the ChannelRouter to send a
	// set of messages to a particular peer identified by the target public
	// key.
	SendMessages func(target *btcec.PublicKey, msg ...lnwire.Message) error

	// TODO(roasbeef): need a SendToSwitch func
	//  * possibly lift switch into package?
	//  *
}

// ChannelRouter is the layer 3 router within the Lightning stack. Below the
// ChannelRouter is the HtlcSwitch, and below that is the Bitcoin blockchain
// itself. The primary role of the ChannelRouter is to respond to queries for
// potential routes that can support a payment amount, and also general graph
// reachability questions. The router will prune the channel graph
// automatically as new blocks are discovered which spend certain known funding
// outpoints, thereby closing their respective channels. Additionally, it's the
// duty of the router to sync up newly connected peers with the latest state of
// the channel graph.
type ChannelRouter struct {
	sync.RWMutex

	cfg *Config

	self *channeldb.LightningNode

	// TODO(roasbeef): make LRU, invalidate upon new block connect
	shortestPathCache map[[33]byte][]*Route
	nodeCache         map[[33]byte]*channeldb.LightningNode
	edgeCache         map[wire.OutPoint]*channeldb.ChannelEdge

	newBlocks chan *chainntnfs.BlockEpoch

	networkMsgs chan *routingMsg

	syncRequests chan *syncRequest

	fakeSig *btcec.Signature

	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// New creates a new instance of the ChannelRouter with the specified
// configuration parameters. As part of initialization, if the router detects
// that the channel graph isn't fully in sync with the latest UTXO (since the
// channel graph is a subset of the UTXO set) set, then the router will proceed
// to fully sync to the latest state of the UTXO set.
func New(cfg Config) (*ChannelRouter, error) {
	// TODO(roasbeef): remove this place holder after sigs are properly
	// stored in the graph.
	s := "30450221008ce2bc69281ce27da07e6683571319d18e949ddfa2965fb6caa" +
		"1bf0314f882d70220299105481d63e0f4bc2a88121167221b6700d72a0e" +
		"ad154c03be696a292d24ae"
	fakeSigHex, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	fakeSig, err := btcec.ParseSignature(fakeSigHex, btcec.S256())
	if err != nil {
		return nil, err
	}

	self, err := cfg.Graph.SourceNode()
	if err != nil {
		return nil, err
	}

	return &ChannelRouter{
		cfg:          &cfg,
		self:         self,
		fakeSig:      fakeSig,
		networkMsgs:  make(chan *routingMsg),
		syncRequests: make(chan *syncRequest),
		quit:         make(chan struct{}),
	}, nil
}

// Start launches all the goroutines the ChannelRouter requires to carry out
// its duties. If the router has already been started, then this method is a
// noop.
func (r *ChannelRouter) Start() error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	log.Tracef("Channel Router starting")

	// First we register for new notifications of newly discovered blocks.
	// We do this immediately so we'll later be able to consume any/all
	// blocks which were discovered as we prune the channel graph using a
	// snapshot of the chain state.
	blockEpochs, err := r.cfg.Notifier.RegisterBlockEpochNtfn()
	if err != nil {
		return err
	}
	r.newBlocks = blockEpochs.Epochs

	// Before we begin normal operation of the router, we first need to
	// synchronize the channel graph to the latest state of the UTXO set.
	if err := r.syncGraphWithChain(); err != nil {
		return err
	}

	r.wg.Add(1)
	go r.networkHandler()

	return nil
}

// Stop signals the ChannelRouter to gracefully halt all routines. This method
// will *block* until all goroutines have excited. If the channel router has
// already stopped then this method will return immediately.
func (r *ChannelRouter) Stop() error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	log.Infof("Channel Router shutting down")

	close(r.quit)
	r.wg.Wait()

	return nil
}

// syncGraphWithChain attempts to synchronize the current channel graph with
// the latest UTXO set state. This process involves pruning from the channel
// graph any channels which have been closed by spending their funding output
// since we've been down.
func (r *ChannelRouter) syncGraphWithChain() error {
	// First, we'll need to check to see if we're already in sync with the
	// latest state of the UTXO set.
	bestHash, bestHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}
	pruneHash, pruneHeight, err := r.cfg.Graph.PruneTip()
	if err != nil {
		switch {
		// If the graph has never been pruned, or hasn't fully been
		// created yet, then we don't treat this as an explicit error.
		case err == channeldb.ErrGraphNeverPruned:
		case err == channeldb.ErrGraphNotFound:
		default:
			return err
		}
	}

	log.Infof("Prune tip for Channel Graph: height=%v, hash=%v", pruneHeight,
		pruneHash)

	switch {

	// If the graph has never been pruned, then we can exit early as this
	// entails it's being created for the first time and hasn't seen any
	// block or created channels.
	case pruneHeight == 0 || pruneHash == nil:
		return nil

	// If the block hashes and heights match exactly, then we don't need to
	// prune the channel graph as we're already fully in sync.
	case bestHash.IsEqual(pruneHash) && uint32(bestHeight) == pruneHeight:
		return nil
	}

	log.Infof("Syncing channel graph from height=%v (hash=%v) to height=%v "+
		"(hash=%v)", pruneHeight, pruneHash, bestHeight, bestHash)

	// If we're not yet caught up, then we'll walk forward in the chain in
	// the chain pruning the channel graph with each new block in the chain
	// that hasn't yet been consumed by the channel graph.
	var numChansClosed uint32
	for nextHeight := pruneHeight + 1; nextHeight <= uint32(bestHeight); nextHeight++ {
		// Using the next height, fetch the next block to use in our
		// incremental graph pruning routine.
		nextHash, err := r.cfg.Chain.GetBlockHash(int64(nextHeight))
		if err != nil {
			return err
		}
		nextBlock, err := r.cfg.Chain.GetBlock(nextHash)
		if err != nil {
			return err
		}

		// We're only interested in all prior outputs that've been
		// spent in the block, so collate all the referenced previous
		// outpoints within each tx and input.
		var spentOutputs []*wire.OutPoint
		for _, tx := range nextBlock.Transactions {
			for _, txIn := range tx.TxIn {
				spentOutputs = append(spentOutputs,
					&txIn.PreviousOutPoint)
			}
		}

		// With the spent outputs gathered, attempt to prune the
		// channel graph, also passing in the hash+height of the block
		// being pruned so the prune tip can be updated.
		numClosed, err := r.cfg.Graph.PruneGraph(spentOutputs, nextHash,
			nextHeight)
		if err != nil {
			return err
		}

		log.Infof("Block %v (height=%v) closed %v channels",
			nextHash, nextHeight, numClosed)

		numChansClosed += numClosed
	}

	log.Infof("Graph pruning complete: %v channels we're closed since "+
		"height %v", numChansClosed, pruneHeight)

	return nil
}

// networkHandler is the primary goroutine for the ChannelRouter. The roles of
// this goroutine include answering queries related to the state of the
// network, syncing up newly connected peers, and also periodically
// broadcasting our latest state to all connected peers.
//
// NOTE: This MUST be run as a goroutine.
func (r *ChannelRouter) networkHandler() {
	defer r.wg.Done()

	var announcementBatch []lnwire.Message

	trickleTimer := time.NewTicker(time.Millisecond * 300)
	defer trickleTimer.Stop()

	for {
		select {
		// A new fully validated network message has just arrived. As a
		// result we'll modify the channel graph accordingly depending
		// on the exact type of the message.
		case netMsg := <-r.networkMsgs:
			// TODO(roasbeef): this loop would mostly be moved to
			// the discovery service

			// Process the network announcement to determine if this
			// is either a new announcement from our PoV or an
			// update to a prior vertex/edge we previously
			// accepted.
			accepted := r.processNetworkAnnouncement(netMsg.msg)

			// If the update was accepted, then add it to our next
			// announcement batch to be broadcast once the trickle
			// timer ticks gain.
			if accepted {
				announcementBatch = append(announcementBatch, netMsg.msg)
			}

			// TODO(roasbeef): remove all unconnected vertexes
			// after N blocks pass with no corresponding
			// announcements.

		// A new block has arrived, so we can prune the channel graph
		// of any channels which were closed in the block.
		case newBlock, ok := <-r.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			block, err := r.cfg.Chain.GetBlock(newBlock.Hash)
			if err != nil {
				log.Errorf("unable to get block: %v", err)
				continue
			}

			log.Infof("Pruning channel graph using block %v (height=%v)",
				newBlock.Hash, newBlock.Height)

			// We're only interested in all prior outputs that've
			// been spent in the block, so collate all the
			// referenced previous outpoints within each tx and
			// input.
			var spentOutputs []*wire.OutPoint
			for _, tx := range block.Transactions {
				for _, txIn := range tx.TxIn {
					spentOutputs = append(spentOutputs,
						&txIn.PreviousOutPoint)
				}
			}

			// With the spent outputs gathered, attempt to prune
			// the channel graph, also passing in the hash+height
			// of the block being pruned so the prune tip can be
			// updated.
			numClosed, err := r.cfg.Graph.PruneGraph(spentOutputs,
				newBlock.Hash, uint32(newBlock.Height))
			if err != nil {
				log.Errorf("unable to prune routing table: %v", err)
				continue
			}

			log.Infof("Block %v (height=%v) closed %v channels",
				newBlock.Hash, newBlock.Height, numClosed)

		// The trickle timer has ticked, which indicates we should
		// flush to the network the pending batch of new announcements
		// we've received since the last trickle tick.
		case <-trickleTimer.C:
			// If the current announcement batch is nil, then we
			// have no further work here.
			if len(announcementBatch) == 0 {
				continue
			}

			log.Infof("Broadcasting batch of %v new announcements",
				len(announcementBatch))

			// If we have new things to announce then broadcast
			// then to all our immediately connected peers.
			err := r.cfg.Broadcast(nil, announcementBatch...)
			if err != nil {
				log.Errorf("unable to send batch announcement: %v", err)
				continue
			}

			// If we we're able to broadcast the current batch
			// successfully, then we reset the batch for a new
			// round of announcements.
			announcementBatch = nil

		// We've just received a new request to synchronize a peer with
		// our latest graph state. This indicates that a peer has just
		// connected for the first time, so for now we dump our entire
		// graph and allow them to sift through the (subjectively) new
		// information on their own.
		case syncReq := <-r.syncRequests:
			nodePub := syncReq.node.SerializeCompressed()
			log.Infof("Synchronizing channel graph with %x", nodePub)

			if err := r.syncChannelGraph(syncReq); err != nil {
				log.Errorf("unable to sync graph state with %x: %v",
					nodePub, err)
			}

		// The router has been signalled to exit, to we exit our main
		// loop so the wait group can be decremented.
		case <-r.quit:
			return
		}
	}
}

// processNetworkAnnouncement processes a new network relate authenticated
// channel or node announcement. If the update didn't affect the internal state
// of the draft due to either being out of date, invalid, or redundant, then
// false is returned. Otherwise, true is returned indicating that the caller
// may want to batch this request to be broadcast to immediate peers during th
// next announcement epoch.
func (r *ChannelRouter) processNetworkAnnouncement(msg lnwire.Message) bool {
	switch msg := msg.(type) {

	// A new node announcement has arrived which either presents a new
	// node, or a node updating previously advertised information.
	case *lnwire.NodeAnnouncement:
		// Before proceeding ensure that we aren't already away of this
		// node, and if we are then this is a newer update that we
		// known of.
		lastUpdate, exists, err := r.cfg.Graph.HasLightningNode(msg.NodeID)
		if err != nil {
			log.Errorf("Unable to query for the existence of node: %v",
				err)
			return false
		}

		// If we've reached this pint then we're aware of th vertex
		// being advertised. So we now check if the new message has a
		// new time stamp, if not then we won't accept the new data as
		// it would override newer data.
		msgTimestamp := time.Unix(int64(msg.Timestamp), 0)
		if exists && lastUpdate.After(msgTimestamp) ||
			lastUpdate.Equal(msgTimestamp) {

			log.Debugf("Ignoring outdated announcement for %x",
				msg.NodeID.SerializeCompressed())
			return false
		}

		node := &channeldb.LightningNode{
			LastUpdate: msgTimestamp,
			Address:    msg.Address,
			PubKey:     msg.NodeID,
			Alias:      msg.Alias.String(),
		}

		if err = r.cfg.Graph.AddLightningNode(node); err != nil {
			log.Errorf("unable to add node %v: %v", msg.NodeID, err)
			return false
		}

		log.Infof("Updated vertex data for node=%x",
			msg.NodeID.SerializeCompressed())

	// A new channel announcement has arrived, this indicates the
	// *creation* of a new channel within the graph. This only advertises
	// the existence of a channel and not yet the routing policies in
	// either direction of the channel.
	case *lnwire.ChannelAnnouncement:
		// Prior to processing the announcement we first check if we
		// already know of this channel, if so, then we can exit early.
		channelID := msg.ChannelID.ToUint64()
		_, _, exists, err := r.cfg.Graph.HasChannelEdge(channelID)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			log.Errorf("unable to check for edge existence: %v", err)
			return false
		} else if exists {
			log.Debugf("Ignoring announcement for known chan_id=%v",
				channelID)
			return false
		}

		// Before we can add the channel to the channel graph, we need
		// to obtain the full funding outpoint that's encoded within
		// the channel ID.
		fundingPoint, err := r.fetchChanPoint(&msg.ChannelID)
		if err != nil {
			log.Errorf("unable to fetch chan point for chan_id=%v: %v",
				channelID, err)
			return false
		}

		// Now that we have the funding outpoint of the channel, ensure
		// that it hasn't yet been spent. If so, then this channel has
		// been closed so we'll ignore it.
		if _, err := r.cfg.Chain.GetUtxo(&fundingPoint.Hash,
			fundingPoint.Index); err != nil {
			log.Errorf("unable to fetch utxo for chan_id=%v: %v",
				channelID, err)
			return false
		}

		// TODO(roasbeef): also add capacity here two instead of on the
		// directed edges.
		err = r.cfg.Graph.AddChannelEdge(msg.FirstNodeID,
			msg.SecondNodeID, fundingPoint, channelID)
		if err != nil {
			log.Errorf("unable to add channel: %v", err)
			return false
		}

		log.Infof("New channel discovered! Link "+
			"connects %x and %x with ChannelPoint(%v), chan_id=%v",
			msg.FirstNodeID.SerializeCompressed(),
			msg.SecondNodeID.SerializeCompressed(),
			fundingPoint, channelID)

	// A new authenticated channel update has has arrived, this indicates
	// that the directional information for an already known channel has
	// been updated. All updates are signed and validated before reaching
	// us, so we trust the data to be legitimate.
	case *lnwire.ChannelUpdateAnnouncement:
		chanID := msg.ChannelID.ToUint64()
		edge1Timestamp, edge2Timestamp, _, err := r.cfg.Graph.HasChannelEdge(chanID)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			log.Errorf("unable to check for edge existence: %v", err)
			return false
		}

		// As edges are directional edge node has a unique policy for
		// the direction of the edge they control. Therefore we first
		// check if we already have the most up to date information for
		// that edge. If so, then we can exit early.
		updateTimestamp := time.Unix(int64(msg.Timestamp), 0)
		switch msg.Flags {

		// A flag set of 0 indicates this is an announcement for
		// the "first" node in the channel.
		case 0:
			if edge1Timestamp.After(updateTimestamp) ||
				edge1Timestamp.Equal(updateTimestamp) {

				log.Debugf("Ignoring announcement (flags=%v) "+
					"for known chan_id=%v", msg.Flags,
					chanID)
				return false
			}

		// Similarly, a flag set of 1 indicates this is an
		// announcement for the "second" node in the channel.
		case 1:
			if edge2Timestamp.After(updateTimestamp) ||
				edge2Timestamp.Equal(updateTimestamp) {

				log.Debugf("Ignoring announcement (flags=%v) "+
					"for known chan_id=%v", msg.Flags,
					chanID)
				return false
			}
		}

		// Before we can update the channel information, we need to get
		// the UTXO itself so we can store the proper capacity.
		chanPoint, err := r.fetchChanPoint(&msg.ChannelID)
		if err != nil {
			log.Errorf("unable to fetch chan point: %v", err)
			return false
		}
		utxo, err := r.cfg.Chain.GetUtxo(&chanPoint.Hash,
			chanPoint.Index)
		if err != nil {
			log.Errorf("unable to fetch utxo for chan_id=%v: %v",
				chanID, err)
			return false
		}

		// TODO(roasbeef): should be msat here
		chanUpdate := &channeldb.ChannelEdge{
			ChannelID:                 chanID,
			ChannelPoint:              *chanPoint,
			LastUpdate:                updateTimestamp,
			Flags:                     msg.Flags,
			Expiry:                    msg.Expiry,
			MinHTLC:                   btcutil.Amount(msg.HtlcMinimumMstat),
			FeeBaseMSat:               btcutil.Amount(msg.FeeBaseMstat),
			FeeProportionalMillionths: btcutil.Amount(msg.FeeProportionalMillionths),
			// TODO(roasbeef): this is a hack, needs to be removed
			// after commitment fees are dynamic.
			Capacity: btcutil.Amount(utxo.Value) - 5000,
		}

		err = r.cfg.Graph.UpdateEdgeInfo(chanUpdate)
		if err != nil {
			log.Errorf("unable to add channel: %v", err)
			return false
		}

		log.Infof("New channel update applied: %v",
			spew.Sdump(chanUpdate))
	}

	return true
}

// syncRequest represents a request from an outside subsystem to the wallet to
// sync a new node to the latest graph state.
type syncRequest struct {
	node *btcec.PublicKey
}

// SynchronizeNode sends a message to the ChannelRouter indicating it should
// synchronize routing state with the target node. This method is to be
// utilized when a node connections for the first time to provide it with the
// latest channel graph state.
func (r *ChannelRouter) SynchronizeNode(pub *btcec.PublicKey) {
	select {
	case r.syncRequests <- &syncRequest{
		node: pub,
	}:
	case <-r.quit:
		return
	}
}

// syncChannelGraph attempts to synchronize the target node in the syncReq to
// the latest channel graph state. In order to accomplish this, (currently) the
// entire graph is read from disk, then serialized to the format defined within
// the current wire protocol. This cache of graph data is then sent directly to
// the target node.
func (r *ChannelRouter) syncChannelGraph(syncReq *syncRequest) error {
	targetNode := syncReq.node

	// TODO(roasbeef): need to also store sig data in db
	//  * will be nice when we switch to pairing sigs would only need one ^_^

	// We'll collate all the gathered routing messages into a single slice
	// containing all the messages to be sent to the target peer.
	var announceMessages []lnwire.Message

	// First run through all the vertexes in the graph, retrieving the data
	// for the announcement we originally retrieved.
	var numNodes uint32
	if err := r.cfg.Graph.ForEachNode(func(node *channeldb.LightningNode) error {
		alias, err := lnwire.NewAlias(node.Alias)
		if err != nil {
			return err
		}

		ann := &lnwire.NodeAnnouncement{
			Signature: r.fakeSig,
			Timestamp: uint32(node.LastUpdate.Unix()),
			Address:   node.Address,
			NodeID:    node.PubKey,
			Alias:     alias,
		}
		announceMessages = append(announceMessages, ann)

		numNodes++

		return nil
	}); err != nil {
		return err
	}

	// With the vertexes gathered, we'll no retrieve the initial
	// announcement, as well as the latest channel update announcement for
	// both of the directed edges that make up the channel.
	// TODO(roasbeef): multi-sig keys should also be stored in DB
	var numEdges uint32
	if err := r.cfg.Graph.ForEachChannel(func(e1, e2 *channeldb.ChannelEdge) error {
		chanID := lnwire.NewChanIDFromInt(e1.ChannelID)
		chanAnn := &lnwire.ChannelAnnouncement{
			FirstNodeSig:     r.fakeSig,
			SecondNodeSig:    r.fakeSig,
			ChannelID:        chanID,
			FirstBitcoinSig:  r.fakeSig,
			SecondBitcoinSig: r.fakeSig,
			FirstNodeID:      e1.Node.PubKey,
			SecondNodeID:     e2.Node.PubKey,
			FirstBitcoinKey:  e1.Node.PubKey,
			SecondBitcoinKey: e2.Node.PubKey,
		}

		chanUpdate1 := &lnwire.ChannelUpdateAnnouncement{
			Signature:                 r.fakeSig,
			ChannelID:                 chanID,
			Timestamp:                 uint32(e1.LastUpdate.Unix()),
			Flags:                     0,
			Expiry:                    e1.Expiry,
			HtlcMinimumMstat:          uint32(e1.MinHTLC),
			FeeBaseMstat:              uint32(e1.FeeBaseMSat),
			FeeProportionalMillionths: uint32(e1.FeeProportionalMillionths),
		}
		chanUpdate2 := &lnwire.ChannelUpdateAnnouncement{
			Signature:                 r.fakeSig,
			ChannelID:                 chanID,
			Timestamp:                 uint32(e2.LastUpdate.Unix()),
			Flags:                     1,
			Expiry:                    e2.Expiry,
			HtlcMinimumMstat:          uint32(e2.MinHTLC),
			FeeBaseMstat:              uint32(e2.FeeBaseMSat),
			FeeProportionalMillionths: uint32(e2.FeeProportionalMillionths),
		}

		numEdges++

		announceMessages = append(announceMessages, chanAnn)
		announceMessages = append(announceMessages, chanUpdate1)
		announceMessages = append(announceMessages, chanUpdate2)
		return nil
	}); err != nil && err != channeldb.ErrGraphNoEdgesFound {
		log.Errorf("unable to sync edges w/ peer: %v", err)
		return err
	}

	log.Infof("Syncing channel graph state with %x, sending %v "+
		"nodes and %v edges", targetNode.SerializeCompressed(),
		numNodes, numEdges)

	// With all the announcement messages gathered, send them all in a
	// single batch to the target peer.
	return r.cfg.SendMessages(targetNode, announceMessages...)
}

// fetchChanPoint retrieves the original outpoint which is encoded within the
// channelID.
func (r *ChannelRouter) fetchChanPoint(chanID *lnwire.ChannelID) (*wire.OutPoint, error) {
	// First fetch the block hash by the block number encoded, then use
	// that hash to fetch the block itself.
	blockNum := int64(chanID.BlockHeight)
	blockHash, err := r.cfg.Chain.GetBlockHash(blockNum)
	if err != nil {
		return nil, err
	}
	fundingBlock, err := r.cfg.Chain.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): skipping validation here as
	// the discovery service should handle full
	// validate

	// Finally once we have the block itself, we seek to the targeted
	// transaction index to obtain the funding output and txid.
	fundingTx := fundingBlock.Transactions[chanID.TxIndex]
	return &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: uint32(chanID.TxPosition),
	}, nil
}

// routingMsg couples a routing related wire message with the peer that
// originally sent it.
type routingMsg struct {
	msg  lnwire.Message
	peer *btcec.PublicKey
}

// ProcessRoutingMessags sends a new routing message along with the peer that
// sent the routing message to the ChannelRouter. The announcement will be
// processed then added to a queue for batched tickled announcement to all
// connected peers.
//
// TODO(roasbeef): need to move to discovery package
func (r *ChannelRouter) ProcessRoutingMessage(msg lnwire.Message, src *btcec.PublicKey) {
	// TODO(roasbeef): msg wrappers to add a doneChan

	rMsg := &routingMsg{
		msg:  msg,
		peer: src,
	}

	select {
	case r.networkMsgs <- rMsg:
	case <-r.quit:
		return
	}
}

// FindRoute attempts to query the ChannelRouter for the "best" path to a
// particular target destination which is able to send `amt` after factoring in
// channel capacities and cumulative fees along the route.
func (r *ChannelRouter) FindRoute(target *btcec.PublicKey, amt btcutil.Amount) (*Route, error) {
	dest := target.SerializeCompressed()

	log.Debugf("Searching for path to %x, sending %v", dest, amt)

	// We can short circuit the routing by opportunistically checking to
	// see if the target vertex event exists in the current graph.
	if _, exists, err := r.cfg.Graph.HasLightningNode(target); err != nil {
		return nil, err
	} else if !exists {
		log.Debugf("Target %x is not in known graph", dest)
		return nil, ErrTargetNotInNetwork
	}

	// TODO(roasbeef): add k-shortest paths
	route, err := findRoute(r.cfg.Graph, target, amt)
	if err != nil {
		log.Errorf("Unable to find path: %v", err)
		return nil, err
	}

	// TODO(roabseef): also create the Sphinx packet and add in the route

	log.Debugf("Obtained path sending %v to %x: %v", amt, dest,
		newLogClosure(func() string {
			return spew.Sdump(route)
		}),
	)

	return route, nil
}

// SendPayment...
//
// TODO(roasbeef): pipe through the htlcSwitch, move the payment storage info
// to the router, add interface for payment storage
// TODO(roasbeef): add version that takes a route object
func (r *ChannelRouter) SendPayment() error {
	return nil
}

// TopologyClient...
// TODO(roasbeef): put in discovery package?
type TopologyClient struct {
}

// TopologyChange...
type TopologyChange struct {
	NewNodes    []*channeldb.LinkNode
	NewChannels []*channeldb.ChannelEdge
}

// notifyTopologyChange...
func (r *ChannelRouter) notifyTopologyChange() {
}

// SubscribeTopology....
func (r *ChannelRouter) SubscribeTopology() (*TopologyClient, error) {
	return nil, nil
}
