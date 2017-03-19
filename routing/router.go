package routing

import (
	"bytes"
	"encoding/hex"
	"fmt"
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

	"github.com/lightningnetwork/lightning-onion"
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

	// SendToSwitch is a function that directs a link-layer switch to
	// forward a fully encoded payment to the first hop in the route
	// denoted by its public key. A non-nil error is to be returned if the
	// payment was unsuccessful.
	SendToSwitch func(firstHop *btcec.PublicKey,
		htlcAdd *lnwire.UpdateAddHTLC) ([32]byte, error)
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
	ntfnClientCounter uint64

	started uint32
	stopped uint32

	// cfg is a copy of the configuration struct that the ChannelRouter was
	// initialized with.
	cfg *Config

	// selfNode is the center of the star-graph centered around the
	// ChannelRouter. The ChannelRouter uses this node as a starting point
	// when doing any path finding.
	selfNode *channeldb.LightningNode

	// TODO(roasbeef): make LRU, invalidate upon new block connect
	shortestPathCache map[[33]byte][]*Route
	nodeCache         map[[33]byte]*channeldb.LightningNode
	edgeCache         map[wire.OutPoint]*channeldb.ChannelEdgePolicy

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over.
	newBlocks <-chan *chainntnfs.BlockEpoch

	// networkMsgs is a channel that carries new network messages from
	// outside the ChannelRouter to be processed by the networkHandler.
	networkMsgs chan *routingMsg

	// syncRequests is a channel that carries requests to synchronize newly
	// connected peers to the state of the channel graph from our PoV.
	syncRequests chan *syncRequest

	// prematureAnnouncements maps a blockheight to a set of announcements
	// which are "premature" from our PoV. An announcement is premature if
	// it claims to be anchored in a block which is beyond the current main
	// chain tip as we know it. Premature announcements will be processed
	// once the chain tip as we know it extends to/past the premature
	// height.
	//
	// TODO(roasbeef): limit premature announcements to N
	prematureAnnouncements map[uint32][]lnwire.Message

	// topologyClients maps a client's unique notification ID to a
	// topologyClient client that contains its notification dispatch
	// channel.
	topologyClients map[uint64]topologyClient

	// ntfnClientUpdates is a channel that's used to send new updates to
	// topology notification clients to the ChannelRouter. Updates either
	// add a new notification client, or cancel notifications for an
	// existing client.
	ntfnClientUpdates chan *topologyClientUpdate

	// bestHeight is the height of the block at the tip of the main chain
	// as we know it.
	bestHeight uint32

	fakeSig *btcec.Signature

	sync.RWMutex

	quit chan struct{}
	wg   sync.WaitGroup
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

	selfNode, err := cfg.Graph.SourceNode()
	if err != nil {
		return nil, err
	}

	return &ChannelRouter{
		cfg:                    &cfg,
		selfNode:               selfNode,
		fakeSig:                fakeSig,
		networkMsgs:            make(chan *routingMsg),
		syncRequests:           make(chan *syncRequest),
		prematureAnnouncements: make(map[uint32][]lnwire.Message),
		topologyClients:        make(map[uint64]topologyClient),
		ntfnClientUpdates:      make(chan *topologyClientUpdate),
		quit:                   make(chan struct{}),
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

	_, height, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}
	r.bestHeight = uint32(height)

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
		closedChans, err := r.cfg.Graph.PruneGraph(spentOutputs, nextHash,
			nextHeight)
		if err != nil {
			return err
		}

		numClosed := uint32(len(closedChans))
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

	// TODO(roasbeef): parametrize the above
	trickleTimer := time.NewTicker(time.Millisecond * 300)
	defer trickleTimer.Stop()

	retransmitTimer := time.NewTicker(time.Minute * 30)
	defer retransmitTimer.Stop()

	for {
		select {
		// A new fully validated network message has just arrived. As a
		// result we'll modify the channel graph accordingly depending
		// on the exact type of the message.
		case netMsg := <-r.networkMsgs:
			// Process the network announcement to determine if
			// this is either a new announcement from our PoV or an
			// update to a prior vertex/edge we previously
			// accepted.
			accepted := r.processNetworkAnnouncement(netMsg.msg)

			// If the update was accepted, then add it to our next
			// announcement batch to be broadcast once the trickle
			// timer ticks gain.
			if accepted {
				// TODO(roasbeef): exclude peer that sent
				announcementBatch = append(announcementBatch, netMsg.msg)

				// Send off a new notification for the newly
				// accepted announcement.
				topChange := &TopologyChange{}
				err := addToTopologyChange(r.cfg.Graph, topChange,
					netMsg.msg)
				if err != nil {
					log.Errorf("unable to update topology "+
						"change notification: %v", err)
				}

				if !topChange.isEmpty() {
					r.notifyTopologyChange(topChange)
				}
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

			// Once a new block arrives, we update our running
			// track of the height of the chain tip.
			blockHeight := uint32(newBlock.Height)
			r.bestHeight = blockHeight

			// Next we check if we have any premature announcements
			// for this height, if so, then we process them once
			// more as normal announcements.
			prematureAnns := r.prematureAnnouncements[uint32(newBlock.Height)]
			if len(prematureAnns) != 0 {
				log.Infof("Re-processing %v premature announcements for "+
					"height %v", len(prematureAnns), blockHeight)
			}

			topChange := &TopologyChange{}
			for _, ann := range prematureAnns {
				if ok := r.processNetworkAnnouncement(ann); ok {
					announcementBatch = append(announcementBatch, ann)

					// As the announcement was accepted,
					// accumulate it to the running set of
					// announcements for this block.
					err := addToTopologyChange(r.cfg.Graph,
						topChange, ann)
					if err != nil {
						log.Errorf("unable to update topology "+
							"change notification: %v", err)
					}
				}
			}
			delete(r.prematureAnnouncements, blockHeight)

			// If the pending notification generated above isn't
			// empty, then send it out to all registered clients.
			if !topChange.isEmpty() {
				r.notifyTopologyChange(topChange)
			}

			log.Infof("Pruning channel graph using block %v (height=%v)",
				newBlock.Hash, blockHeight)

			block, err := r.cfg.Chain.GetBlock(newBlock.Hash)
			if err != nil {
				log.Errorf("unable to get block: %v", err)
				continue
			}

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
			chansClosed, err := r.cfg.Graph.PruneGraph(spentOutputs,
				newBlock.Hash, blockHeight)
			if err != nil {
				log.Errorf("unable to prune routing table: %v", err)
				continue
			}

			log.Infof("Block %v (height=%v) closed %v channels",
				newBlock.Hash, blockHeight, len(chansClosed))

			if len(chansClosed) == 0 {
				continue
			}

			// Notify all currently registered clients of the newly
			// closed channels.
			closeSummaries := createCloseSummaries(blockHeight, chansClosed...)
			r.notifyTopologyChange(&TopologyChange{
				ClosedChannels: closeSummaries,
			})

		// The retransmission timer has ticked which indicates that we
		// should broadcast our personal channel to the network. This
		// addresses the case of channel advertisements whether being
		// dropped, or not properly propagated through the network.
		case <-retransmitTimer.C:
			var selfChans []lnwire.Message

			selfPub := r.selfNode.PubKey.SerializeCompressed()
			err := r.selfNode.ForEachChannel(nil, func(_ *channeldb.ChannelEdgeInfo,
				c *channeldb.ChannelEdgePolicy) error {

				chanNodePub := c.Node.PubKey.SerializeCompressed()

				// Compare our public key with that of the
				// channel peer. If our key is "less" than
				// theirs, then we're the "first" node in the
				// advertisement, otherwise we're the second.
				flags := uint16(1)
				if bytes.Compare(selfPub, chanNodePub) == -1 {
					flags = 0
				}

				selfChans = append(selfChans, &lnwire.ChannelUpdateAnnouncement{
					Signature:                 r.fakeSig,
					ChannelID:                 lnwire.NewChanIDFromInt(c.ChannelID),
					Timestamp:                 uint32(c.LastUpdate.Unix()),
					Flags:                     flags,
					TimeLockDelta:             c.TimeLockDelta,
					HtlcMinimumMsat:           uint32(c.MinHTLC),
					FeeBaseMsat:               uint32(c.FeeBaseMSat),
					FeeProportionalMillionths: uint32(c.FeeProportionalMillionths),
				})
				return nil
			})
			if err != nil {
				log.Errorf("unable to retransmit "+
					"channels: %v", err)
				continue
			}

			if len(selfChans) == 0 {
				continue
			}

			log.Debugf("Retransmitting %v outgoing channels",
				len(selfChans))

			// With all the wire messages properly crafted, we'll
			// broadcast our known outgoing channel to all our
			// immediate peers.
			if err := r.cfg.Broadcast(nil, selfChans...); err != nil {
				log.Errorf("unable to re-broadcast "+
					"channels: %v", err)
			}

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

		// A new notification client update has arrived. We're either
		// gaining a new client, or cancelling notifications for an
		// existing client.
		case ntfnUpdate := <-r.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				if client, ok := r.topologyClients[ntfnUpdate.clientID]; ok {
					delete(r.topologyClients, clientID)
					close(client.ntfnChan)
					close(client.exit)
				}

				continue
			}

			r.topologyClients[ntfnUpdate.clientID] = topologyClient{
				ntfnChan: ntfnUpdate.ntfnChan,
				exit:     make(chan struct{}),
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
// may want to batch this request to be broadcast to immediate peers during the
// next announcement epoch.
func (r *ChannelRouter) processNetworkAnnouncement(msg lnwire.Message) bool {
	isPremature := func(chanID *lnwire.ChannelID) bool {
		return chanID.BlockHeight > r.bestHeight
	}

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

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(&msg.ChannelID) {
			blockHeight := msg.ChannelID.BlockHeight
			log.Infof("Announcement for chan_id=(%v), is "+
				"premature: advertises height %v, only height "+
				"%v is known", channelID,
				msg.ChannelID.BlockHeight, r.bestHeight)

			r.prematureAnnouncements[blockHeight] = append(
				r.prematureAnnouncements[blockHeight],
				msg,
			)
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
		chanUtxo, err := r.cfg.Chain.GetUtxo(&fundingPoint.Hash,
			fundingPoint.Index)
		if err != nil {
			log.Errorf("unable to fetch utxo for chan_id=%v: %v",
				channelID, err)
			return false
		}

		edge := &channeldb.ChannelEdgeInfo{
			ChannelID:   channelID,
			NodeKey1:    msg.FirstNodeID,
			NodeKey2:    msg.SecondNodeID,
			BitcoinKey1: msg.FirstBitcoinKey,
			BitcoinKey2: msg.SecondBitcoinKey,
			AuthProof: &channeldb.ChannelAuthProof{
				NodeSig1:    msg.FirstNodeSig,
				NodeSig2:    msg.SecondNodeSig,
				BitcoinSig1: msg.FirstBitcoinSig,
				BitcoinSig2: msg.SecondBitcoinSig,
			},
			ChannelPoint: *fundingPoint,
			// TODO(roasbeef): this is a hack, needs to be removed
			// after commitment fees are dynamic.
			Capacity: btcutil.Amount(chanUtxo.Value) - btcutil.Amount(5000),
		}
		if err := r.cfg.Graph.AddChannelEdge(edge); err != nil {
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

		// If the advertised inclusionary block is beyond our knowledge
		// of the chain tip, then we'll put the announcement in limbo
		// to be fully verified once we advance forward in the chain.
		if isPremature(&msg.ChannelID) {
			blockHeight := msg.ChannelID.BlockHeight
			log.Infof("Update announcement for chan_id=(%v), is "+
				"premature: advertises height %v, only height "+
				"%v is known", chanID, blockHeight,
				r.bestHeight)

			r.prematureAnnouncements[blockHeight] = append(
				r.prematureAnnouncements[blockHeight],
				msg,
			)
			return false
		}

		// As edges are directional edge node has a unique policy for
		// the direction of the edge they control. Therefore we first
		// check if we already have the most up to date information for
		// that edge. If so, then we can exit early.
		updateTimestamp := time.Unix(int64(msg.Timestamp), 0)
		switch msg.Flags {

		// A flag set of 0 indicates this is an announcement for the
		// "first" node in the channel.
		case 0:
			if edge1Timestamp.After(updateTimestamp) ||
				edge1Timestamp.Equal(updateTimestamp) {

				log.Debugf("Ignoring announcement (flags=%v) "+
					"for known chan_id=%v", msg.Flags,
					chanID)
				return false
			}

		// Similarly, a flag set of 1 indicates this is an announcement
		// for the "second" node in the channel.
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
			log.Errorf("unable to fetch chan point for chan_id=%v: %v", chanID, err)
			return false
		}
		if _, err := r.cfg.Chain.GetUtxo(&chanPoint.Hash,
			chanPoint.Index); err != nil {
			log.Errorf("unable to fetch utxo for chan_id=%v: %v",
				chanID, err)
			return false
		}

		// TODO(roasbeef): should be msat here
		chanUpdate := &channeldb.ChannelEdgePolicy{
			ChannelID:                 chanID,
			LastUpdate:                updateTimestamp,
			Flags:                     msg.Flags,
			TimeLockDelta:             msg.TimeLockDelta,
			MinHTLC:                   btcutil.Amount(msg.HtlcMinimumMsat),
			FeeBaseMSat:               btcutil.Amount(msg.FeeBaseMsat),
			FeeProportionalMillionths: btcutil.Amount(msg.FeeProportionalMillionths),
		}
		if err = r.cfg.Graph.UpdateEdgePolicy(chanUpdate); err != nil {
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
	var numEdges uint32
	if err := r.cfg.Graph.ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error {

		chanID := lnwire.NewChanIDFromInt(chanInfo.ChannelID)

		// First, using the parameters of the channel, along with the
		// channel authentication proof, we'll create re-create the
		// original authenticated channel announcement.
		authProof := chanInfo.AuthProof
		chanAnn := &lnwire.ChannelAnnouncement{
			FirstNodeSig:     authProof.NodeSig1,
			SecondNodeSig:    authProof.NodeSig2,
			ChannelID:        chanID,
			FirstBitcoinSig:  authProof.BitcoinSig1,
			SecondBitcoinSig: authProof.BitcoinSig2,
			FirstNodeID:      chanInfo.NodeKey1,
			SecondNodeID:     chanInfo.NodeKey2,
			FirstBitcoinKey:  chanInfo.BitcoinKey1,
			SecondBitcoinKey: chanInfo.BitcoinKey2,
		}

		// We'll unconditionally queue the channel's existence proof as
		// it will need to be processed before either of the channel
		// update announcements.
		announceMessages = append(announceMessages, chanAnn)

		// Since it's up to a node's policy as to whether they
		// advertise the edge in dire direction, we don't create an
		// advertisement if the edge is nil.
		if e1 != nil {
			announceMessages = append(announceMessages, &lnwire.ChannelUpdateAnnouncement{
				Signature:                 r.fakeSig,
				ChannelID:                 chanID,
				Timestamp:                 uint32(e1.LastUpdate.Unix()),
				Flags:                     0,
				TimeLockDelta:             e1.TimeLockDelta,
				HtlcMinimumMsat:           uint32(e1.MinHTLC),
				FeeBaseMsat:               uint32(e1.FeeBaseMSat),
				FeeProportionalMillionths: uint32(e1.FeeProportionalMillionths),
			})
		}
		if e2 != nil {
			announceMessages = append(announceMessages, &lnwire.ChannelUpdateAnnouncement{
				Signature:                 r.fakeSig,
				ChannelID:                 chanID,
				Timestamp:                 uint32(e2.LastUpdate.Unix()),
				Flags:                     1,
				TimeLockDelta:             e2.TimeLockDelta,
				HtlcMinimumMsat:           uint32(e2.MinHTLC),
				FeeBaseMsat:               uint32(e2.FeeBaseMSat),
				FeeProportionalMillionths: uint32(e2.FeeProportionalMillionths),
			})
		}

		numEdges++
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

	// As a sanity check, ensure that the advertised transaction index is
	// within the bounds of the total number of transactions within a
	// block.
	numTxns := uint32(len(fundingBlock.Transactions))
	if chanID.TxIndex > numTxns-1 {
		return nil, fmt.Errorf("tx_index=#%v is out of range "+
			"(max_index=%v), network_chan_id=%v\n", chanID.TxIndex,
			numTxns-1, spew.Sdump(chanID))
	}

	// TODO(roasbeef): skipping validation here as the discovery service
	// should handle full validate

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

// ProcessRoutingMessage sends a new routing message along with the peer that
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
	route, err := findRoute(r.cfg.Graph, r.selfNode, target, amt)
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

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
//
// TODO(roasbeef): add params for the per-hop payloads
func generateSphinxPacket(route *Route, paymentHash []byte) ([]byte, error) {
	// First obtain all the public keys along the route which are contained
	// in each hop.
	nodes := make([]*btcec.PublicKey, len(route.Hops))
	for i, hop := range route.Hops {
		// We create a new instance of the public key to avoid possibly
		// mutating the curve parameters, which are unset in a higher
		// level in order to avoid spamming the logs.
		pub := btcec.PublicKey{
			Curve: btcec.S256(),
			X:     hop.Channel.Node.PubKey.X,
			Y:     hop.Channel.Node.PubKey.Y,
		}
		nodes[i] = &pub
	}

	// Next we generate the per-hop payload which gives each node within
	// the route the necessary information (fees, CLTV value, etc) to
	// properly forward the payment.
	// TODO(roasbeef): properly set CLTV value, payment amount, and chain
	// within hop payloads.
	var hopPayloads [][]byte
	for i := 0; i < len(route.Hops); i++ {
		payload := bytes.Repeat([]byte{byte('A' + i)},
			sphinx.HopPayloadSize)
		hopPayloads = append(hopPayloads, payload)
	}

	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(nodes, sessionKey,
		hopPayloads, paymentHash)
	if err != nil {
		return nil, err
	}

	// Finally, encode Sphinx packet using it's wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		newLogClosure(func() string {
			// We unset the internal curve here in order to keep
			// the logs from getting noisy.
			sphinxPacket.Header.EphemeralKey.Curve = nil
			return spew.Sdump(sphinxPacket)
		}),
	)

	return onionBlob.Bytes(), nil
}

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target *btcec.PublicKey

	// Amount is the value of the payment to send through the network in
	// satoshis.
	// TODO(roasbeef): this should be milli satoshis
	Amount btcutil.Amount

	// PaymentHash is the r-hash value to use within the HTLC extended to
	// the first hop.
	PaymentHash [32]byte

	// TODO(roasbeef): add e2e message?
}

// SendPayment attempts to send a payment as described within the passed
// LightningPayment. This function is blocking and will return either: when the
// payment is successful, or all candidates routes have been attempted and
// resulted in a failed payment. If the payment succeeds, then a non-nil Route
// will be returned which describes the path the successful payment traversed
// within the network to reach the destination. Additionally, the payment
// preimage will also be returned.
func (r *ChannelRouter) SendPayment(payment *LightningPayment) ([32]byte, *Route, error) {
	var (
		err      error
		preImage [32]byte
	)

	// Query the graph for a potential path to the destination node that
	// can support our payment amount. If a path is ultimately unavailable,
	// then an error will be returned.
	route, err := r.FindRoute(payment.Target, payment.Amount)
	if err != nil {
		return preImage, nil, err
	}
	log.Tracef("Selected route for payment: %#v", route)

	// Generate the raw encoded sphinx packet to be included along with the
	// htlcAdd message that we send directly to the switch.
	sphinxPacket, err := generateSphinxPacket(route, payment.PaymentHash[:])
	if err != nil {
		return preImage, nil, err
	}

	// Craft an HTLC packet to send to the layer 2 switch. The metadata
	// within this packet will be used to route the payment through the
	// network, starting with the first-hop.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      route.TotalAmount,
		PaymentHash: payment.PaymentHash,
	}
	copy(htlcAdd.OnionBlob[:], sphinxPacket)

	// Attempt to send this payment through the network to complete the
	// payment. If this attempt fails, then we'll bail our early.
	firstHop := route.Hops[0].Channel.Node.PubKey
	preImage, err = r.cfg.SendToSwitch(firstHop, htlcAdd)
	if err != nil {
		return preImage, nil, err
	}

	return preImage, route, nil
}
