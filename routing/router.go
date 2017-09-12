package routing

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"

	"crypto/sha256"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lightning-onion"
)

// ChannelGraphSource represent the source of information about the topology of
// lightning network, it responsible for addition of nodes, edges
// and applying edges updates, return the current block with with out
// topology is synchronized.
type ChannelGraphSource interface {
	// AddNode is used to add information about a node to the router
	// database. If the node with this pubkey is not present in an existing
	// channel, it will be ignored.
	AddNode(node *channeldb.LightningNode) error

	// AddEdge is used to add edge/channel to the topology of the router,
	// after all information about channel will be gathered this
	// edge/channel might be used in construction of payment path.
	AddEdge(edge *channeldb.ChannelEdgeInfo) error

	// AddProof updates the channel edge info with proof which is needed to
	// properly announce the edge to the rest of the network.
	AddProof(chanID lnwire.ShortChannelID, proof *channeldb.ChannelAuthProof) error

	// UpdateEdge is used to update edge information, without this message
	// edge considered as not fully constructed.
	UpdateEdge(policy *channeldb.ChannelEdgePolicy) error

	// ForAllOutgoingChannels is used to iterate over all channels
	// eminating from the "source" node which is the center of the
	// star-graph.
	ForAllOutgoingChannels(cb func(c *channeldb.ChannelEdgeInfo,
		e *channeldb.ChannelEdgePolicy) error) error

	// CurrentBlockHeight returns the block height from POV of the router
	// subsystem.
	CurrentBlockHeight() (uint32, error)

	// GetChannelByID return the channel by the channel id.
	GetChannelByID(chanID lnwire.ShortChannelID) (*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy, error)

	// ForEachNode is used to iterate over every node in the known graph.
	ForEachNode(func(node *channeldb.LightningNode) error) error

	// ForEachChannel is used to iterate over every channel in the known
	// graph.
	ForEachChannel(func(chanInfo *channeldb.ChannelEdgeInfo,
		e1, e2 *channeldb.ChannelEdgePolicy) error) error
}

// FeeSchema is the set fee configuration for a Lighting Node on the network.
// Using the coefficients described within he schema, the required fee to
// forward outgoing payments can be derived.
type FeeSchema struct {
	// BaseFee is the base amount of milli-satoshis that will be chained
	// for ANY payment forwarded.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the rate that will be charged for forwarding payments.
	// This value should be interpreted as the numerator for a fraction
	// whose denominator is 1 million. As a result the effective fee rate
	// charged per mSAT will be: (amount * FeeRate/1,000,000)
	FeeRate uint32
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
	Chain lnwallet.BlockChainIO

	// ChainView is an instance of a FilteredChainView which is used to
	// watch the sub-set of the UTXO set (the set of active channels) that
	// we need in order to properly maintain the channel graph.
	ChainView chainview.FilteredChainView

	// SendToSwitch is a function that directs a link-layer switch to
	// forward a fully encoded payment to the first hop in the route
	// denoted by its public key. A non-nil error is to be returned if the
	// payment was unsuccessful.
	SendToSwitch func(firstHop *btcec.PublicKey, htlcAdd *lnwire.UpdateAddHTLC,
		circuit *sphinx.Circuit) ([sha256.Size]byte, error)
}

// routeTuple is an entry within the ChannelRouter's route cache. We cache
// prospective routes based on first the destination, and then the target
// amount. We required the target amount as that will influence the available
// set of paths for a payment.
type routeTuple struct {
	amt  lnwire.MilliSatoshi
	dest [33]byte
}

// newRouteTuple creates a new route tuple from the target and amount.
func newRouteTuple(amt lnwire.MilliSatoshi, dest *btcec.PublicKey) routeTuple {
	r := routeTuple{
		amt: amt,
	}
	copy(r.dest[:], dest.SerializeCompressed())

	return r
}

// ChannelRouter is the layer 3 router within the Lightning stack. Below the
// ChannelRouter is the HtlcSwitch, and below that is the Bitcoin blockchain
// itself. The primary role of the ChannelRouter is to respond to queries for
// potential routes that can support a payment amount, and also general graph
// reachability questions. The router will prune the channel graph automatically
// as new blocks are discovered which spend certain known funding outpoints,
// thereby closing their respective channels.
type ChannelRouter struct {
	ntfnClientCounter uint64

	started uint32
	stopped uint32

	bestHeight uint32

	// cfg is a copy of the configuration struct that the ChannelRouter was
	// initialized with.
	cfg *Config

	// selfNode is the center of the star-graph centered around the
	// ChannelRouter. The ChannelRouter uses this node as a starting point
	// when doing any path finding.
	selfNode *channeldb.LightningNode

	// routeCache is a map that caches the k-shortest paths from ourselves
	// to a given target destination for a particular payment amount. This
	// map is used as an optimization to speed up subsequent payments to a
	// particular destination. This map will be cleared each time a new
	// channel announcement is accepted, or a new block arrives that
	// results in channels being closed.
	//
	// TODO(roasbeef): make LRU
	routeCacheMtx sync.RWMutex
	routeCache    map[routeTuple][]*Route

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over.
	newBlocks <-chan *chainview.FilteredBlock

	// networkUpdates is a channel that carries new topology updates
	// messages from outside the ChannelRouter to be processed by the
	// networkHandler.
	networkUpdates chan *routingMsg

	// topologyClients maps a client's unique notification ID to a
	// topologyClient client that contains its notification dispatch
	// channel.
	topologyClients map[uint64]*topologyClient

	// ntfnClientUpdates is a channel that's used to send new updates to
	// topology notification clients to the ChannelRouter. Updates either
	// add a new notification client, or cancel notifications for an
	// existing client.
	ntfnClientUpdates chan *topologyClientUpdate

	sync.RWMutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure ChannelRouter implements the ChannelGraphSource interface.
var _ ChannelGraphSource = (*ChannelRouter)(nil)

// New creates a new instance of the ChannelRouter with the specified
// configuration parameters. As part of initialization, if the router detects
// that the channel graph isn't fully in sync with the latest UTXO (since the
// channel graph is a subset of the UTXO set) set, then the router will proceed
// to fully sync to the latest state of the UTXO set.
func New(cfg Config) (*ChannelRouter, error) {
	selfNode, err := cfg.Graph.SourceNode()
	if err != nil {
		return nil, err
	}

	return &ChannelRouter{
		cfg:               &cfg,
		selfNode:          selfNode,
		networkUpdates:    make(chan *routingMsg),
		topologyClients:   make(map[uint64]*topologyClient),
		ntfnClientUpdates: make(chan *topologyClientUpdate),
		routeCache:        make(map[routeTuple][]*Route),
		quit:              make(chan struct{}),
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

	// First, we'll start the chain view instance (if it isn't already
	// started).
	if err := r.cfg.ChainView.Start(); err != nil {
		return err
	}

	// Once the instance is active, we'll fetch the channel we'll receive
	// notifications over.
	r.newBlocks = r.cfg.ChainView.FilteredBlocks()

	// Before we begin normal operation of the router, we first need to
	// synchronize the channel graph to the latest state of the UTXO set.
	if err := r.syncGraphWithChain(); err != nil {
		return err
	}

	// Once we've concluded our manual block pruning, we'll constrcut and
	// apply a fresh chain filter to the active FilteredChainView instance.
	channelView, err := r.cfg.Graph.ChannelView()
	if err != nil && err != channeldb.ErrGraphNoEdgesFound {
		return err
	}
	log.Infof("Filtering chain using %v channels active", len(channelView))
	err = r.cfg.ChainView.UpdateFilter(channelView, r.bestHeight)
	if err != nil {
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

	if err := r.cfg.ChainView.Stop(); err != nil {
		return err
	}

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
	r.bestHeight = uint32(bestHeight)
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
		// Using the next height, request a manual block pruning from
		// the chainview for the particular block hash.
		nextHash, err := r.cfg.Chain.GetBlockHash(int64(nextHeight))
		if err != nil {
			return err
		}
		filterBlock, err := r.cfg.ChainView.FilterBlock(nextHash)
		if err != nil {
			return err
		}

		// We're only interested in all prior outputs that've been
		// spent in the block, so collate all the referenced previous
		// outpoints within each tx and input.
		var spentOutputs []*wire.OutPoint
		for _, tx := range filterBlock.Transactions {
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
// network, pruning the graph on new block notification, applying network
// updates, and registering new topology clients.
//
// NOTE: This MUST be run as a goroutine.
func (r *ChannelRouter) networkHandler() {
	defer r.wg.Done()

	// TODO(roasbeef): ticker to check if should prune in two weeks or not

	for {
		select {
		// A new fully validated network update has just arrived. As a
		// result we'll modify the channel graph accordingly depending
		// on the exact type of the message.
		case updateMsg := <-r.networkUpdates:
			// Process the routing update to determine if this is
			// either a new update from our PoV or an update to a
			// prior vertex/edge we previously
			// accepted.
			err := r.processUpdate(updateMsg.msg)
			updateMsg.err <- err
			if err != nil {
				continue
			}

			// Send off a new notification for the newly
			// accepted update.
			topChange := &TopologyChange{}
			err = addToTopologyChange(r.cfg.Graph, topChange,
				updateMsg.msg)
			if err != nil {
				log.Errorf("unable to update topology "+
					"change notification: %v", err)
				continue
			}

			if !topChange.isEmpty() {
				r.notifyTopologyChange(topChange)
			}

			// TODO(roasbeef): remove all unconnected vertexes
			// after N blocks pass with no corresponding
			// announcements.

		// A new block has arrived, so we can prune the channel graph
		// of any channels which were closed in the block.
		case chainUpdate, ok := <-r.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Once a new block arrives, we update our running
			// track of the height of the chain tip.
			blockHeight := uint32(chainUpdate.Height)
			r.bestHeight = blockHeight
			log.Infof("Pruning channel graph using block %v (height=%v)",
				chainUpdate.Hash, blockHeight)

			// We're only interested in all prior outputs that've
			// been spent in the block, so collate all the
			// referenced previous outpoints within each tx and
			// input.
			var spentOutputs []*wire.OutPoint
			for _, tx := range chainUpdate.Transactions {
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
				&chainUpdate.Hash, chainUpdate.Height)
			if err != nil {
				log.Errorf("unable to prune routing table: %v", err)
				continue
			}

			log.Infof("Block %v (height=%v) closed %v channels",
				chainUpdate.Hash, blockHeight, len(chansClosed))

			// Invalidate the route cache as the block height has
			// changed which will invalidate the HTLC timeouts we
			// have crafted within each of the pre-computed routes.
			//
			// TODO(roasbeef): need to invalidate after each
			// chan ann update?
			//  * can have map of chanID to routes involved, avoids
			//    full invalidation
			r.routeCacheMtx.Lock()
			r.routeCache = make(map[routeTuple][]*Route)
			r.routeCacheMtx.Unlock()

			if len(chansClosed) == 0 {
				continue
			}

			// Notify all currently registered clients of the newly
			// closed channels.
			closeSummaries := createCloseSummaries(blockHeight, chansClosed...)
			r.notifyTopologyChange(&TopologyChange{
				ClosedChannels: closeSummaries,
			})

		// A new notification client update has arrived. We're either
		// gaining a new client, or cancelling notifications for an
		// existing client.
		case ntfnUpdate := <-r.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				if client, ok := r.topologyClients[ntfnUpdate.clientID]; ok {
					delete(r.topologyClients, clientID)

					close(client.exit)
					client.wg.Wait()

					close(client.ntfnChan)
				}

				continue
			}

			r.topologyClients[ntfnUpdate.clientID] = &topologyClient{
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

// processUpdate processes a new relate authenticated channel/edge, node or
// channel/edge update network update. If the update didn't affect the internal
// state of the draft due to either being out of date, invalid, or redundant,
// then error is returned.
func (r *ChannelRouter) processUpdate(msg interface{}) error {

	var invalidateCache bool

	switch msg := msg.(type) {
	case *channeldb.LightningNode:
		// If we are not already aware of this node, it means that we
		// don't know about any channel using this node. To avoid a DoS
		// attack by node announcements, we will ignore such nodes. If
		// we do know about this node, check that this update brings
		// info newer than what we already have.
		lastUpdate, exists, err := r.cfg.Graph.HasLightningNode(msg.PubKey)
		if err != nil {
			return errors.Errorf("unable to query for the "+
				"existence of node: %v", err)
		}
		if !exists {
			return newErrf(ErrIgnored, "Ignoring node announcement"+
				" for node not found in channel graph (%x)",
				msg.PubKey.SerializeCompressed())
		}

		// If we've reached this point then we're aware of the vertex
		// being advertised. So we now check if the new message has a
		// new time stamp, if not then we won't accept the new data as
		// it would override newer data.
		if exists && lastUpdate.After(msg.LastUpdate) ||
			lastUpdate.Equal(msg.LastUpdate) {

			return newErrf(ErrOutdated, "Ignoring outdated "+
				"announcement for %x", msg.PubKey.SerializeCompressed())
		}

		if err := r.cfg.Graph.AddLightningNode(msg); err != nil {
			return errors.Errorf("unable to add node %v to the "+
				"graph: %v", msg.PubKey.SerializeCompressed(), err)
		}

		log.Infof("Updated vertex data for node=%x",
			msg.PubKey.SerializeCompressed())

	case *channeldb.ChannelEdgeInfo:
		// Prior to processing the announcement we first check if we
		// already know of this channel, if so, then we can exit early.
		_, _, exists, err := r.cfg.Graph.HasChannelEdge(msg.ChannelID)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)
		} else if exists {
			return newErrf(ErrIgnored, "Ignoring msg for known "+
				"chan_id=%v", msg.ChannelID)
		}

		// Query the database for the existence of the two nodes in this
		// channel. If not found, add a partial node to the database,
		// containing only the node keys.
		_, exists, _ = r.cfg.Graph.HasLightningNode(msg.NodeKey1)
		if !exists {
			node1 := &channeldb.LightningNode{
				PubKey:               msg.NodeKey1,
				HaveNodeAnnouncement: false,
			}
			err := r.cfg.Graph.AddLightningNode(node1)
			if err != nil {
				return errors.Errorf("unable to add node %v to"+
					" the graph: %v",
					node1.PubKey.SerializeCompressed(), err)
			}
		}
		_, exists, _ = r.cfg.Graph.HasLightningNode(msg.NodeKey2)
		if !exists {
			node2 := &channeldb.LightningNode{
				PubKey:               msg.NodeKey2,
				HaveNodeAnnouncement: false,
			}
			err := r.cfg.Graph.AddLightningNode(node2)
			if err != nil {
				return errors.Errorf("unable to add node %v to"+
					" the graph: %v",
					node2.PubKey.SerializeCompressed(), err)
			}
		}

		// Before we can add the channel to the channel graph, we need
		// to obtain the full funding outpoint that's encoded within
		// the channel ID.
		channelID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		fundingPoint, err := r.fetchChanPoint(&channelID)
		if err != nil {
			return errors.Errorf("unable to fetch chan point for "+
				"chan_id=%v: %v", msg.ChannelID, err)
		}

		// Now that we have the funding outpoint of the channel, ensure
		// that it hasn't yet been spent. If so, then this channel has
		// been closed so we'll ignore it.
		chanUtxo, err := r.cfg.Chain.GetUtxo(fundingPoint,
			channelID.BlockHeight)
		if err != nil {
			return errors.Errorf("unable to fetch utxo for "+
				"chan_id=%v, chan_point=%v: %v", msg.ChannelID,
				fundingPoint, err)
		}

		// Recreate witness output to be sure that declared in channel
		// edge bitcoin keys and channel value corresponds to the
		// reality.
		_, witnessOutput, err := lnwallet.GenFundingPkScript(
			msg.BitcoinKey1.SerializeCompressed(),
			msg.BitcoinKey2.SerializeCompressed(),
			chanUtxo.Value,
		)
		if err != nil {
			return errors.Errorf("unable to create funding pk "+
				"script: %v", err)
		}

		// By checking the equality of witness pkscripts we checks that
		// funding witness script is multisignature lock which contains
		// both local and remote public keys which was declared in
		// channel edge and also that the announced channel value is
		// right.
		if !bytes.Equal(witnessOutput.PkScript, chanUtxo.PkScript) {
			return errors.Errorf("pkScript mismatch: expected %x, "+
				"got %x", witnessOutput.PkScript, chanUtxo.PkScript)
		}

		// TODO(roasbeef): this is a hack, needs to be removed
		// after commitment fees are dynamic.
		msg.Capacity = btcutil.Amount(chanUtxo.Value)
		msg.ChannelPoint = *fundingPoint
		if err := r.cfg.Graph.AddChannelEdge(msg); err != nil {
			return errors.Errorf("unable to add edge: %v", err)
		}

		invalidateCache = true
		log.Infof("New channel discovered! Link "+
			"connects %x and %x with ChannelPoint(%v): "+
			"chan_id=%v, capacity=%v",
			msg.NodeKey1.SerializeCompressed(),
			msg.NodeKey2.SerializeCompressed(),
			fundingPoint, msg.ChannelID, msg.Capacity)

		// As a new edge has been added to the channel graph, we'll
		// update the current UTXO filter within our active
		// FilteredChainView so we are notified if/when this channel is
		// closed.
		filterUpdate := []wire.OutPoint{*fundingPoint}
		err = r.cfg.ChainView.UpdateFilter(filterUpdate, r.bestHeight)
		if err != nil {
			return errors.Errorf("unable to update chain "+
				"view: %v", err)
		}

	case *channeldb.ChannelEdgePolicy:
		channelID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		edge1Timestamp, edge2Timestamp, exists, err := r.cfg.Graph.HasChannelEdge(
			msg.ChannelID,
		)
		if err != nil && err != channeldb.ErrGraphNoEdgesFound {
			return errors.Errorf("unable to check for edge "+
				"existence: %v", err)

		}

		// As edges are directional edge node has a unique policy for
		// the direction of the edge they control. Therefore we first
		// check if we already have the most up to date information for
		// that edge. If so, then we can exit early.
		switch msg.Flags {

		// A flag set of 0 indicates this is an announcement for the
		// "first" node in the channel.
		case 0:
			if edge1Timestamp.After(msg.LastUpdate) ||
				edge1Timestamp.Equal(msg.LastUpdate) {
				return newErrf(ErrIgnored, "Ignoring announcement "+
					"(flags=%v) for known chan_id=%v", msg.Flags,
					msg.ChannelID)

			}

		// Similarly, a flag set of 1 indicates this is an announcement
		// for the "second" node in the channel.
		case 1:
			if edge2Timestamp.After(msg.LastUpdate) ||
				edge2Timestamp.Equal(msg.LastUpdate) {

				return newErrf(ErrIgnored, "Ignoring announcement "+
					"(flags=%v) for known chan_id=%v", msg.Flags,
					msg.ChannelID)
			}
		}

		if !exists {
			// Before we can update the channel information, we'll
			// ensure that the target channel is still open by
			// querying the utxo-set for its existence.
			chanPoint, err := r.fetchChanPoint(&channelID)
			if err != nil {
				return errors.Errorf("unable to fetch chan "+
					"point for chan_id=%v: %v",
					msg.ChannelID, err)
			}
			_, err = r.cfg.Chain.GetUtxo(
				chanPoint, channelID.BlockHeight,
			)
			if err != nil {
				return errors.Errorf("unable to fetch utxo for "+
					"chan_id=%v: %v", msg.ChannelID, err)
			}
		}

		// Now that we know this isn't a stale update, we'll apply the
		// new edge policy to the proper directional edge within the
		// channel graph.
		if err = r.cfg.Graph.UpdateEdgePolicy(msg); err != nil {
			err := errors.Errorf("unable to add channel: %v", err)
			log.Error(err)
			return err
		}

		invalidateCache = true
		log.Infof("New channel update applied: %v",
			spew.Sdump(msg))

	default:
		return errors.Errorf("wrong routing update message type")
	}

	// If we've received a channel update, then invalidate the route cache
	// as channels within the graph have closed, which may affect our
	// choice of the KSP's for a particular routeTuple.
	if invalidateCache {
		r.routeCacheMtx.Lock()
		r.routeCache = make(map[routeTuple][]*Route)
		r.routeCacheMtx.Unlock()
	}

	return nil
}

// fetchChanPoint retrieves the original outpoint which is encoded within the
// channelID.
//
// TODO(roasbeef): replace iwth call to GetBlockTransaction? (woudl allow to
// later use getblocktxn)
func (r *ChannelRouter) fetchChanPoint(chanID *lnwire.ShortChannelID) (*wire.OutPoint, error) {
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

	// Finally once we have the block itself, we seek to the targeted
	// transaction index to obtain the funding output and txid.
	fundingTx := fundingBlock.Transactions[chanID.TxIndex]
	return &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: uint32(chanID.TxPosition),
	}, nil
}

// routingMsg couples a routing related routing topology update to the
// error channel.
type routingMsg struct {
	msg interface{}
	err chan error
}

// FindRoutes attempts to query the ChannelRouter for the all available paths
// to a particular target destination which is able to send `amt` after
// factoring in channel capacities and cumulative fees along each route route.
// To find all eligible paths, we use a modified version of Yen's algorithm
// which itself uses a modified version of Dijkstra's algorithm within its
// inner loop.  Once we have a set of candidate routes, we calculate the
// required fee and time lock values running backwards along the route. The
// route that will be ranked the highest is the one with the lowest cumulative
// fee along the route.
func (r *ChannelRouter) FindRoutes(target *btcec.PublicKey,
	amt lnwire.MilliSatoshi) ([]*Route, error) {

	dest := target.SerializeCompressed()
	log.Debugf("Searching for path to %x, sending %v", dest, amt)

	// We can short circuit the routing by opportunistically checking to
	// see if the target vertex event exists in the current graph.
	if _, exists, err := r.cfg.Graph.HasLightningNode(target); err != nil {
		return nil, err
	} else if !exists {
		log.Debugf("Target %x is not in known graph", dest)
		return nil, newErrf(ErrTargetNotInNetwork, "target not found")
	}

	// We'll also fetch the current block height so we can properly
	// calculate the required HTLC time locks within the route.
	_, currentHeight, err := r.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Now that we know the destination is reachable within the graph,
	// we'll execute our KSP algorithm to find the k-shortest paths from
	// our source to the destination.
	shortestPaths, err := findPaths(r.cfg.Graph, r.selfNode, target, amt)
	if err != nil {
		return nil, err
	}

	// Now that we have a set of paths, we'll need to turn them into
	// *routes* by computing the required time-lock and fee information for
	// each path. During this process, some paths may be discarded if they
	// aren't able to support the total satoshis flow once fees have been
	// factored in.
	validRoutes := make(sortableRoutes, 0, len(shortestPaths))
	for _, path := range shortestPaths {
		// Attempt to make the path into a route. We snip off the first
		// hop in the path as it contains a "self-hop" that is inserted
		// by our KSP algorithm.
		route, err := newRoute(amt, path[1:], uint32(currentHeight))
		if err != nil {
			continue
		}

		// If the path as enough total flow to support the computed
		// route, then we'll add it to our set of valid routes.
		validRoutes = append(validRoutes, route)
	}

	// If all our perspective routes were eliminating during the transition
	// from path to route, then we'll return an error to the caller
	if len(validRoutes) == 0 {
		return nil, newErr(ErrNoPathFound, "unable to find a path to "+
			"destination")
	}

	// Finally, we'll sort the set of validate routes to optimize for
	// lowest total fees, using the required time-lock within the route as
	// a tie-breaker.
	sort.Sort(validRoutes)

	log.Debugf("Obtained %v paths sending %v to %x: %v", len(validRoutes),
		amt, dest, newLogClosure(func() string {
			return spew.Sdump(validRoutes)
		}),
	)

	return validRoutes, nil
}

// generateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route. The blob returned
// from this function can immediately be included within an HTLC add packet to
// be sent to the first hop within the route.
func generateSphinxPacket(route *Route, paymentHash []byte) ([]byte,
	*sphinx.Circuit, error) {
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
	hopPayloads := route.ToHopPayloads()

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash[:], spew.Sdump(hopPayloads))

	sessionKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, nil, err
	}

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(nodes, sessionKey,
		hopPayloads, paymentHash)
	if err != nil {
		return nil, nil, err
	}

	// Finally, encode Sphinx packet using it's wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		newLogClosure(func() string {
			// We unset the internal curve here in order to keep
			// the logs from getting noisy.
			sphinxPacket.EphemeralKey.Curve = nil
			return spew.Sdump(sphinxPacket)
		}),
	)

	return onionBlob.Bytes(), &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: nodes,
	}, nil
}

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target *btcec.PublicKey

	// Amount is the value of the payment to send through the network in
	// milli-satoshis.
	Amount lnwire.MilliSatoshi

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
	log.Tracef("Dispatching route for lightning payment: %v",
		newLogClosure(func() string {
			payment.Target.Curve = nil
			return spew.Sdump(payment)
		}),
	)

	var (
		sendError error
		preImage  [32]byte
	)

	// TODO(roasbeef): consult KSP cache before dispatching

	// Before attempting to perform a series of graph traversals to find
	// the k-shortest paths to the destination, we'll first consult our
	// path cache
	rt := newRouteTuple(payment.Amount, payment.Target)

	r.routeCacheMtx.RLock()
	routes, ok := r.routeCache[rt]
	r.routeCacheMtx.RUnlock()

	// If we don't have a set of routes cached, we'll query the graph for a
	// set of potential routes to the destination node that can support our
	// payment amount. If no such routes can be found then an error will be
	// returned.
	if !ok {
		// TODO(roasbeef): put cache handling into FindRoutes
		freshRoutes, err := r.FindRoutes(payment.Target, payment.Amount)
		if err != nil {
			return preImage, nil, err
		}

		// Populate the cache with this set of fresh routes so we can
		// reuse them in the future.
		r.routeCacheMtx.Lock()
		r.routeCache[rt] = freshRoutes
		r.routeCacheMtx.Unlock()

		routes = freshRoutes
	}

	// For each eligible path, we'll attempt to successfully send our
	// target payment using the multi-hop route. We'll try each route
	// serially until either once succeeds, or we've exhausted our set of
	// available paths.
	for _, route := range routes {
		log.Tracef("Attempting to send payment %x, using route: %v",
			payment.PaymentHash, newLogClosure(func() string {
				return spew.Sdump(route)
			}),
		)

		// Generate the raw encoded sphinx packet to be included along
		// with the htlcAdd message that we send directly to the
		// switch.
		onionBlob, circuit, err := generateSphinxPacket(route,
			payment.PaymentHash[:])
		if err != nil {
			return preImage, nil, err
		}

		// Craft an HTLC packet to send to the layer 2 switch. The
		// metadata within this packet will be used to route the
		// payment through the network, starting with the first-hop.
		htlcAdd := &lnwire.UpdateAddHTLC{
			Amount:      route.TotalAmount,
			Expiry:      route.TotalTimeLock,
			PaymentHash: payment.PaymentHash,
		}
		copy(htlcAdd.OnionBlob[:], onionBlob)

		// Attempt to send this payment through the network to complete
		// the payment. If this attempt fails, then we'll continue on
		// to the next available route.
		firstHop := route.Hops[0].Channel.Node.PubKey
		preImage, sendError = r.cfg.SendToSwitch(firstHop, htlcAdd,
			circuit)
		if sendError != nil {
			log.Errorf("Attempt to send payment %x failed: %v",
				payment.PaymentHash, sendError)
			continue
		}

		return preImage, route, nil
	}

	// If we're unable to successfully make a payment using any of the
	// routes we've found, then return an error.
	return [32]byte{}, nil, sendError
}

// AddNode is used to add information about a node to the router database. If
// the node with this pubkey is not present in an existing channel, it will
// be ignored.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddNode(node *channeldb.LightningNode) error {
	rMsg := &routingMsg{
		msg: node,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		return <-rMsg.err
	case <-r.quit:
		return errors.New("router has been shutted down")
	}
}

// AddEdge is used to add edge/channel to the topology of the router, after all
// information about channel will be gathered this
// edge/channel might be used in construction of payment path.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddEdge(edge *channeldb.ChannelEdgeInfo) error {
	rMsg := &routingMsg{
		msg: edge,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		return <-rMsg.err
	case <-r.quit:
		return errors.New("router has been shutted down")
	}
}

// UpdateEdge is used to update edge information, without this message edge
// considered as not fully constructed.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) UpdateEdge(update *channeldb.ChannelEdgePolicy) error {
	rMsg := &routingMsg{
		msg: update,
		err: make(chan error, 1),
	}

	select {
	case r.networkUpdates <- rMsg:
		return <-rMsg.err
	case <-r.quit:
		return errors.New("router has been shutted down")
	}
}

// CurrentBlockHeight returns the block height from POV of the router subsystem.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) CurrentBlockHeight() (uint32, error) {
	_, height, err := r.cfg.Chain.GetBestBlock()
	return uint32(height), err
}

// GetChannelByID return the channel by the channel id.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) GetChannelByID(chanID lnwire.ShortChannelID) (
	*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	return r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
}

// ForEachNode is used to iterate over every node in router topology.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachNode(cb func(*channeldb.LightningNode) error) error {
	return r.cfg.Graph.ForEachNode(nil, func(_ *bolt.Tx, n *channeldb.LightningNode) error {
		return cb(n)
	})
}

// ForAllOutgoingChannels is used to iterate over all outgiong channel owned by
// the router.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForAllOutgoingChannels(cb func(*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy) error) error {

	return r.selfNode.ForEachChannel(nil, func(_ *bolt.Tx, c *channeldb.ChannelEdgeInfo,
		e, _ *channeldb.ChannelEdgePolicy) error {

		return cb(c, e)
	})
}

// ForEachChannel is used to iterate over every known edge (channel) within our
// view of the channel graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) ForEachChannel(cb func(chanInfo *channeldb.ChannelEdgeInfo,
	e1, e2 *channeldb.ChannelEdgePolicy) error) error {

	return r.cfg.Graph.ForEachChannel(cb)
}

// AddProof updates the channel edge info with proof which is needed to
// properly announce the edge to the rest of the network.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (r *ChannelRouter) AddProof(chanID lnwire.ShortChannelID,
	proof *channeldb.ChannelAuthProof) error {

	info, _, _, err := r.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
	if err != nil {
		return err
	}

	info.AuthProof = proof
	return r.cfg.Graph.UpdateChannelEdge(info)
}
