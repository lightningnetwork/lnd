package graph

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/chainntnfs"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/multimutex"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// DefaultChannelPruneExpiry is the default duration used to determine
	// if a channel should be pruned or not.
	DefaultChannelPruneExpiry = time.Hour * 24 * 14

	// DefaultFirstTimePruneDelay is the time we'll wait after startup
	// before attempting to prune the graph for zombie channels. We don't
	// do it immediately after startup to allow lnd to start up without
	// getting blocked by this job.
	DefaultFirstTimePruneDelay = 30 * time.Second

	// defaultStatInterval governs how often the router will log non-empty
	// stats related to processing new channels, updates, or node
	// announcements.
	defaultStatInterval = time.Minute
)

var (
	// ErrGraphBuilderShuttingDown is returned if the graph builder is in
	// the process of shutting down.
	ErrGraphBuilderShuttingDown = fmt.Errorf("graph builder shutting down")
)

// Config holds the configuration required by the Builder.
type Config struct {
	// SelfNode is the public key of the node that this channel router
	// belongs to.
	SelfNode route.Vertex

	// Graph is the channel graph that the ChannelRouter will use to gather
	// metrics from and also to carry out path finding queries.
	Graph *graphdb.ChannelGraph

	// Chain is the router's source to the most up-to-date blockchain data.
	// All incoming advertised channels will be checked against the chain
	// to ensure that the channels advertised are still open.
	Chain lnwallet.BlockChainIO

	// ChainView is an instance of a FilteredChainView which is used to
	// watch the sub-set of the UTXO set (the set of active channels) that
	// we need in order to properly maintain the channel graph.
	ChainView chainview.FilteredChainView

	// Notifier is a reference to the ChainNotifier, used to grab
	// the latest blocks if the router is missing any.
	Notifier chainntnfs.ChainNotifier

	// ChannelPruneExpiry is the duration used to determine if a channel
	// should be pruned or not. If the delta between now and when the
	// channel was last updated is greater than ChannelPruneExpiry, then
	// the channel is marked as a zombie channel eligible for pruning.
	ChannelPruneExpiry time.Duration

	// GraphPruneInterval is used as an interval to determine how often we
	// should examine the channel graph to garbage collect zombie channels.
	GraphPruneInterval time.Duration

	// FirstTimePruneDelay is the time we'll wait after startup before
	// attempting to prune the graph for zombie channels. We don't do it
	// immediately after startup to allow lnd to start up without getting
	// blocked by this job.
	FirstTimePruneDelay time.Duration

	// AssumeChannelValid toggles whether the builder will prune channels
	// based on their spentness vs using the fact that they are considered
	// zombies.
	AssumeChannelValid bool

	// StrictZombiePruning determines if we attempt to prune zombie
	// channels according to a stricter criteria. If true, then we'll prune
	// a channel if only *one* of the edges is considered a zombie.
	// Otherwise, we'll only prune the channel when both edges have a very
	// dated last update.
	StrictZombiePruning bool

	// IsAlias returns whether a passed ShortChannelID is an alias. This is
	// only used for our local channels.
	IsAlias func(scid lnwire.ShortChannelID) bool
}

// Builder builds and maintains a view of the Lightning Network graph.
type Builder struct {
	started atomic.Bool
	stopped atomic.Bool

	bestHeight atomic.Uint32

	cfg *Config

	// newBlocks is a channel in which new blocks connected to the end of
	// the main chain are sent over, and blocks updated after a call to
	// UpdateFilter.
	newBlocks <-chan *chainview.FilteredBlock

	// staleBlocks is a channel in which blocks disconnected from the end
	// of our currently known best chain are sent over.
	staleBlocks <-chan *chainview.FilteredBlock

	// channelEdgeMtx is a mutex we use to make sure we process only one
	// ChannelEdgePolicy at a time for a given channelID, to ensure
	// consistency between the various database accesses.
	channelEdgeMtx *multimutex.Mutex[uint64]

	// statTicker is a resumable ticker that logs the router's progress as
	// it discovers channels or receives updates.
	statTicker ticker.Ticker

	// stats tracks newly processed channels, updates, and node
	// announcements over a window of defaultStatInterval.
	stats *builderStats

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure Builder implements the
// ChannelGraphSource interface.
var _ ChannelGraphSource = (*Builder)(nil)

// NewBuilder constructs a new Builder.
func NewBuilder(cfg *Config) (*Builder, error) {
	return &Builder{
		cfg:            cfg,
		channelEdgeMtx: multimutex.NewMutex[uint64](),
		statTicker:     ticker.New(defaultStatInterval),
		stats:          new(builderStats),
		quit:           make(chan struct{}),
	}, nil
}

// Start launches all the goroutines the Builder requires to carry out its
// duties. If the builder has already been started, then this method is a noop.
func (b *Builder) Start() error {
	if !b.started.CompareAndSwap(false, true) {
		return nil
	}

	log.Info("Builder starting")

	bestHash, bestHeight, err := b.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}

	// If the graph has never been pruned, or hasn't fully been created yet,
	// then we don't treat this as an explicit error.
	if _, _, err := b.cfg.Graph.PruneTip(); err != nil {
		switch {
		case errors.Is(err, graphdb.ErrGraphNeverPruned):
			fallthrough

		case errors.Is(err, graphdb.ErrGraphNotFound):
			// If the graph has never been pruned, then we'll set
			// the prune height to the current best height of the
			// chain backend.
			_, err = b.cfg.Graph.PruneGraph(
				nil, bestHash, uint32(bestHeight),
			)
			if err != nil {
				return err
			}

		default:
			return err
		}
	}

	// If AssumeChannelValid is present, then we won't rely on pruning
	// channels from the graph based on their spentness, but whether they
	// are considered zombies or not. We will start zombie pruning after a
	// small delay, to avoid slowing down startup of lnd.
	if b.cfg.AssumeChannelValid { //nolint:nestif
		time.AfterFunc(b.cfg.FirstTimePruneDelay, func() {
			select {
			case <-b.quit:
				return
			default:
			}

			log.Info("Initial zombie prune starting")
			if err := b.pruneZombieChans(); err != nil {
				log.Errorf("Unable to prune zombies: %v", err)
			}
		})
	} else {
		// Otherwise, we'll use our filtered chain view to prune
		// channels as soon as they are detected as spent on-chain.
		if err := b.cfg.ChainView.Start(); err != nil {
			return err
		}

		// Once the instance is active, we'll fetch the channel we'll
		// receive notifications over.
		b.newBlocks = b.cfg.ChainView.FilteredBlocks()
		b.staleBlocks = b.cfg.ChainView.DisconnectedBlocks()

		// Before we perform our manual block pruning, we'll construct
		// and apply a fresh chain filter to the active
		// FilteredChainView instance.  We do this before, as otherwise
		// we may miss on-chain events as the filter hasn't properly
		// been applied.
		channelView, err := b.cfg.Graph.ChannelView()
		if err != nil && !errors.Is(
			err, graphdb.ErrGraphNoEdgesFound,
		) {

			return err
		}

		log.Infof("Filtering chain using %v channels active",
			len(channelView))

		if len(channelView) != 0 {
			err = b.cfg.ChainView.UpdateFilter(
				channelView, uint32(bestHeight),
			)
			if err != nil {
				return err
			}
		}

		// The graph pruning might have taken a while and there could be
		// new blocks available.
		_, bestHeight, err = b.cfg.Chain.GetBestBlock()
		if err != nil {
			return err
		}
		b.bestHeight.Store(uint32(bestHeight))

		// Before we begin normal operation of the router, we first need
		// to synchronize the channel graph to the latest state of the
		// UTXO set.
		if err := b.syncGraphWithChain(); err != nil {
			return err
		}

		// Finally, before we proceed, we'll prune any unconnected nodes
		// from the graph in order to ensure we maintain a tight graph
		// of "useful" nodes.
		err = b.cfg.Graph.PruneGraphNodes()
		if err != nil &&
			!errors.Is(err, graphdb.ErrGraphNodesNotFound) {

			return err
		}
	}

	b.wg.Add(1)
	go b.networkHandler()

	log.Debug("Builder started")

	return nil
}

// Stop signals to the Builder that it should halt all routines. This method
// will *block* until all goroutines have excited. If the builder has already
// stopped then this method will return immediately.
func (b *Builder) Stop() error {
	if !b.stopped.CompareAndSwap(false, true) {
		return nil
	}

	log.Info("Builder shutting down...")

	// Our filtered chain view could've only been started if
	// AssumeChannelValid isn't present.
	if !b.cfg.AssumeChannelValid {
		if err := b.cfg.ChainView.Stop(); err != nil {
			return err
		}
	}

	close(b.quit)
	b.wg.Wait()

	log.Debug("Builder shutdown complete")

	return nil
}

// syncGraphWithChain attempts to synchronize the current channel graph with
// the latest UTXO set state. This process involves pruning from the channel
// graph any channels which have been closed by spending their funding output
// since we've been down.
func (b *Builder) syncGraphWithChain() error {
	// First, we'll need to check to see if we're already in sync with the
	// latest state of the UTXO set.
	bestHash, bestHeight, err := b.cfg.Chain.GetBestBlock()
	if err != nil {
		return err
	}
	b.bestHeight.Store(uint32(bestHeight))

	pruneHash, pruneHeight, err := b.cfg.Graph.PruneTip()
	if err != nil {
		switch {
		// If the graph has never been pruned, or hasn't fully been
		// created yet, then we don't treat this as an explicit error.
		case errors.Is(err, graphdb.ErrGraphNeverPruned):
		case errors.Is(err, graphdb.ErrGraphNotFound):
		default:
			return err
		}
	}

	log.Infof("Prune tip for Channel Graph: height=%v, hash=%v",
		pruneHeight, pruneHash)

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

	// If the main chain blockhash at prune height is different from the
	// prune hash, this might indicate the database is on a stale branch.
	mainBlockHash, err := b.cfg.Chain.GetBlockHash(int64(pruneHeight))
	if err != nil {
		return err
	}

	// While we are on a stale branch of the chain, walk backwards to find
	// first common block.
	for !pruneHash.IsEqual(mainBlockHash) {
		log.Infof("channel graph is stale. Disconnecting block %v "+
			"(hash=%v)", pruneHeight, pruneHash)
		// Prune the graph for every channel that was opened at height
		// >= pruneHeight.
		_, err := b.cfg.Graph.DisconnectBlockAtHeight(pruneHeight)
		if err != nil {
			return err
		}

		pruneHash, pruneHeight, err = b.cfg.Graph.PruneTip()
		switch {
		// If at this point the graph has never been pruned, we can exit
		// as this entails we are back to the point where it hasn't seen
		// any block or created channels, alas there's nothing left to
		// prune.
		case errors.Is(err, graphdb.ErrGraphNeverPruned):
			return nil

		case errors.Is(err, graphdb.ErrGraphNotFound):
			return nil

		case err != nil:
			return err

		default:
		}

		mainBlockHash, err = b.cfg.Chain.GetBlockHash(
			int64(pruneHeight),
		)
		if err != nil {
			return err
		}
	}

	log.Infof("Syncing channel graph from height=%v (hash=%v) to "+
		"height=%v (hash=%v)", pruneHeight, pruneHash, bestHeight,
		bestHash)

	// If we're not yet caught up, then we'll walk forward in the chain
	// pruning the channel graph with each new block that hasn't yet been
	// consumed by the channel graph.
	var spentOutputs []*wire.OutPoint
	for nextHeight := pruneHeight + 1; nextHeight <= uint32(bestHeight); nextHeight++ { //nolint:ll
		// Break out of the rescan early if a shutdown has been
		// requested, otherwise long rescans will block the daemon from
		// shutting down promptly.
		select {
		case <-b.quit:
			return ErrGraphBuilderShuttingDown
		default:
		}

		// Using the next height, request a manual block pruning from
		// the chainview for the particular block hash.
		log.Infof("Filtering block for closed channels, at height: %v",
			int64(nextHeight))
		nextHash, err := b.cfg.Chain.GetBlockHash(int64(nextHeight))
		if err != nil {
			return err
		}
		log.Tracef("Running block filter on block with hash: %v",
			nextHash)
		filterBlock, err := b.cfg.ChainView.FilterBlock(nextHash)
		if err != nil {
			return err
		}

		// We're only interested in all prior outputs that have been
		// spent in the block, so collate all the referenced previous
		// outpoints within each tx and input.
		for _, tx := range filterBlock.Transactions {
			for _, txIn := range tx.TxIn {
				spentOutputs = append(spentOutputs,
					&txIn.PreviousOutPoint)
			}
		}
	}

	// With the spent outputs gathered, attempt to prune the channel graph,
	// also passing in the best hash+height so the prune tip can be updated.
	closedChans, err := b.cfg.Graph.PruneGraph(
		spentOutputs, bestHash, uint32(bestHeight),
	)
	if err != nil {
		return err
	}

	log.Infof("Graph pruning complete: %v channels were closed since "+
		"height %v", len(closedChans), pruneHeight)

	return nil
}

// isZombieChannel takes two edge policy updates and determines if the
// corresponding channel should be considered a zombie. The first boolean is
// true if the policy update from node 1 is considered a zombie, the second
// boolean is that of node 2, and the final boolean is true if the channel
// is considered a zombie.
func (b *Builder) isZombieChannel(e1,
	e2 *models.ChannelEdgePolicy) (bool, bool, bool) {

	chanExpiry := b.cfg.ChannelPruneExpiry

	e1Zombie := e1 == nil || time.Since(e1.LastUpdate) >= chanExpiry
	e2Zombie := e2 == nil || time.Since(e2.LastUpdate) >= chanExpiry

	var e1Time, e2Time time.Time
	if e1 != nil {
		e1Time = e1.LastUpdate
	}
	if e2 != nil {
		e2Time = e2.LastUpdate
	}

	return e1Zombie, e2Zombie, b.IsZombieChannel(e1Time, e2Time)
}

// IsZombieChannel takes the timestamps of the latest channel updates for a
// channel and returns true if the channel should be considered a zombie based
// on these timestamps.
func (b *Builder) IsZombieChannel(updateTime1,
	updateTime2 time.Time) bool {

	chanExpiry := b.cfg.ChannelPruneExpiry

	e1Zombie := updateTime1.IsZero() ||
		time.Since(updateTime1) >= chanExpiry

	e2Zombie := updateTime2.IsZero() ||
		time.Since(updateTime2) >= chanExpiry

	// If we're using strict zombie pruning, then a channel is only
	// considered live if both edges have a recent update we know of.
	if b.cfg.StrictZombiePruning {
		return e1Zombie || e2Zombie
	}

	// Otherwise, if we're using the less strict variant, then a channel is
	// considered live if either of the edges have a recent update.
	return e1Zombie && e2Zombie
}

// pruneZombieChans is a method that will be called periodically to prune out
// any "zombie" channels. We consider channels zombies if *both* edges haven't
// been updated since our zombie horizon. If AssumeChannelValid is present,
// we'll also consider channels zombies if *both* edges are disabled. This
// usually signals that a channel has been closed on-chain. We do this
// periodically to keep a healthy, lively routing table.
func (b *Builder) pruneZombieChans() error {
	chansToPrune := make(map[uint64]struct{})
	chanExpiry := b.cfg.ChannelPruneExpiry

	log.Infof("Examining channel graph for zombie channels")

	// A helper method to detect if the channel belongs to this node
	isSelfChannelEdge := func(info *models.ChannelEdgeInfo) bool {
		return info.NodeKey1Bytes == b.cfg.SelfNode ||
			info.NodeKey2Bytes == b.cfg.SelfNode
	}

	// First, we'll collect all the channels which are eligible for garbage
	// collection due to being zombies.
	filterPruneChans := func(info *models.ChannelEdgeInfo,
		e1, e2 *models.ChannelEdgePolicy) error {

		// Exit early in case this channel is already marked to be
		// pruned
		_, markedToPrune := chansToPrune[info.ChannelID]
		if markedToPrune {
			return nil
		}

		// We'll ensure that we don't attempt to prune our *own*
		// channels from the graph, as in any case this should be
		// re-advertised by the sub-system above us.
		if isSelfChannelEdge(info) {
			return nil
		}

		e1Zombie, e2Zombie, isZombieChan := b.isZombieChannel(e1, e2)

		if e1Zombie {
			log.Tracef("Node1 pubkey=%x of chan_id=%v is zombie",
				info.NodeKey1Bytes, info.ChannelID)
		}

		if e2Zombie {
			log.Tracef("Node2 pubkey=%x of chan_id=%v is zombie",
				info.NodeKey2Bytes, info.ChannelID)
		}

		// If either edge hasn't been updated for a period of
		// chanExpiry, then we'll mark the channel itself as eligible
		// for graph pruning.
		if !isZombieChan {
			return nil
		}

		log.Debugf("ChannelID(%v) is a zombie, collecting to prune",
			info.ChannelID)

		// TODO(roasbeef): add ability to delete single directional edge
		chansToPrune[info.ChannelID] = struct{}{}

		return nil
	}

	// If AssumeChannelValid is present we'll look at the disabled bit for
	// both edges. If they're both disabled, then we can interpret this as
	// the channel being closed and can prune it from our graph.
	if b.cfg.AssumeChannelValid {
		disabledChanIDs, err := b.cfg.Graph.DisabledChannelIDs()
		if err != nil {
			return fmt.Errorf("unable to get disabled channels "+
				"ids chans: %v", err)
		}

		disabledEdges, err := b.cfg.Graph.FetchChanInfos(
			disabledChanIDs,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch disabled channels "+
				"edges chans: %v", err)
		}

		// Ensuring we won't prune our own channel from the graph.
		for _, disabledEdge := range disabledEdges {
			if !isSelfChannelEdge(disabledEdge.Info) {
				chansToPrune[disabledEdge.Info.ChannelID] =
					struct{}{}
			}
		}
	}

	startTime := time.Unix(0, 0)
	endTime := time.Now().Add(-1 * chanExpiry)
	oldEdgesIter := b.cfg.Graph.ChanUpdatesInHorizon(startTime, endTime)

	for u, err := range oldEdgesIter {
		if err != nil {
			return fmt.Errorf("unable to fetch expired "+
				"channel updates chans: %v", err)
		}

		err = filterPruneChans(u.Info, u.Policy1, u.Policy2)
		if err != nil {
			return fmt.Errorf("error filtering channels to "+
				"prune: %w", err)
		}
	}

	log.Infof("Pruning %v zombie channels", len(chansToPrune))
	if len(chansToPrune) == 0 {
		return nil
	}

	// With the set of zombie-like channels obtained, we'll do another pass
	// to delete them from the channel graph.
	toPrune := make([]uint64, 0, len(chansToPrune))
	for chanID := range chansToPrune {
		toPrune = append(toPrune, chanID)
		log.Tracef("Pruning zombie channel with ChannelID(%v)", chanID)
	}
	err := b.cfg.Graph.DeleteChannelEdges(
		b.cfg.StrictZombiePruning, true, toPrune...,
	)
	if err != nil {
		return fmt.Errorf("unable to delete zombie channels: %w", err)
	}

	// With the channels pruned, we'll also attempt to prune any nodes that
	// were a part of them.
	err = b.cfg.Graph.PruneGraphNodes()
	if err != nil && !errors.Is(err, graphdb.ErrGraphNodesNotFound) {
		return fmt.Errorf("unable to prune graph nodes: %w", err)
	}

	return nil
}

// networkHandler is the primary goroutine for the Builder. The roles of
// this goroutine include answering queries related to the state of the
// network, pruning the graph on new block notification, applying network
// updates, and registering new topology clients.
//
// NOTE: This MUST be run as a goroutine.
func (b *Builder) networkHandler() {
	defer b.wg.Done()

	graphPruneTicker := time.NewTicker(b.cfg.GraphPruneInterval)
	defer graphPruneTicker.Stop()

	defer b.statTicker.Stop()

	b.stats.Reset()

	for {
		// If there are stats, resume the statTicker.
		if !b.stats.Empty() {
			b.statTicker.Resume()
		}

		select {
		case chainUpdate, ok := <-b.staleBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// Since this block is stale, we update our best height
			// to the previous block.
			blockHeight := chainUpdate.Height
			b.bestHeight.Store(blockHeight - 1)

			// Update the channel graph to reflect that this block
			// was disconnected.
			_, err := b.cfg.Graph.DisconnectBlockAtHeight(
				blockHeight,
			)
			if err != nil {
				log.Errorf("unable to prune graph with stale "+
					"block: %v", err)
				continue
			}

			// TODO(halseth): notify client about the reorg?

		// A new block has arrived, so we can prune the channel graph
		// of any channels which were closed in the block.
		case chainUpdate, ok := <-b.newBlocks:
			// If the channel has been closed, then this indicates
			// the daemon is shutting down, so we exit ourselves.
			if !ok {
				return
			}

			// We'll ensure that any new blocks received attach
			// directly to the end of our main chain. If not, then
			// we've somehow missed some blocks. Here we'll catch
			// up the chain with the latest blocks.
			currentHeight := b.bestHeight.Load()
			switch {
			case chainUpdate.Height == currentHeight+1:
				err := b.updateGraphWithClosedChannels(
					chainUpdate,
				)
				if err != nil {
					log.Errorf("unable to prune graph "+
						"with closed channels: %v", err)
				}

			case chainUpdate.Height > currentHeight+1:
				log.Errorf("out of order block: expecting "+
					"height=%v, got height=%v",
					currentHeight+1, chainUpdate.Height)

				err := b.getMissingBlocks(
					currentHeight, chainUpdate,
				)
				if err != nil {
					log.Errorf("unable to retrieve missing"+
						"blocks: %v", err)
				}

			case chainUpdate.Height < currentHeight+1:
				log.Errorf("out of order block: expecting "+
					"height=%v, got height=%v",
					currentHeight+1, chainUpdate.Height)

				log.Infof("Skipping channel pruning since "+
					"received block height %v was already"+
					" processed.", chainUpdate.Height)
			}

		// The graph prune ticker has ticked, so we'll examine the
		// state of the known graph to filter out any zombie channels
		// for pruning.
		case <-graphPruneTicker.C:
			if err := b.pruneZombieChans(); err != nil {
				log.Errorf("Unable to prune zombies: %v", err)
			}

		// Log any stats if we've processed a non-empty number of
		// channels, updates, or nodes. We'll only pause the ticker if
		// the last window contained no updates to avoid resuming and
		// pausing while consecutive windows contain new info.
		case <-b.statTicker.Ticks():
			if !b.stats.Empty() {
				log.Infof(b.stats.String())
			} else {
				b.statTicker.Pause()
			}
			b.stats.Reset()

		// The router has been signalled to exit, to we exit our main
		// loop so the wait group can be decremented.
		case <-b.quit:
			return
		}
	}
}

// getMissingBlocks walks through all missing blocks and updates the graph
// closed channels accordingly.
func (b *Builder) getMissingBlocks(currentHeight uint32,
	chainUpdate *chainview.FilteredBlock) error {

	outdatedHash, err := b.cfg.Chain.GetBlockHash(int64(currentHeight))
	if err != nil {
		return err
	}

	outdatedBlock := &chainntnfs.BlockEpoch{
		Height: int32(currentHeight),
		Hash:   outdatedHash,
	}

	epochClient, err := b.cfg.Notifier.RegisterBlockEpochNtfn(
		outdatedBlock,
	)
	if err != nil {
		return err
	}
	defer epochClient.Cancel()

	blockDifference := int(chainUpdate.Height - currentHeight)

	// We'll walk through all the outdated blocks and make sure we're able
	// to update the graph with any closed channels from them.
	for i := 0; i < blockDifference; i++ {
		var (
			missingBlock *chainntnfs.BlockEpoch
			ok           bool
		)

		select {
		case missingBlock, ok = <-epochClient.Epochs:
			if !ok {
				return nil
			}

		case <-b.quit:
			return nil
		}

		filteredBlock, err := b.cfg.ChainView.FilterBlock(
			missingBlock.Hash,
		)
		if err != nil {
			return err
		}

		err = b.updateGraphWithClosedChannels(
			filteredBlock,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// updateGraphWithClosedChannels prunes the channel graph of closed channels
// that are no longer needed.
func (b *Builder) updateGraphWithClosedChannels(
	chainUpdate *chainview.FilteredBlock) error {

	// Once a new block arrives, we update our running track of the height
	// of the chain tip.
	blockHeight := chainUpdate.Height

	b.bestHeight.Store(blockHeight)
	log.Infof("Pruning channel graph using block %v (height=%v)",
		chainUpdate.Hash, blockHeight)

	// We're only interested in all prior outputs that have been spent in
	// the block, so collate all the referenced previous outpoints within
	// each tx and input.
	var spentOutputs []*wire.OutPoint
	for _, tx := range chainUpdate.Transactions {
		for _, txIn := range tx.TxIn {
			spentOutputs = append(spentOutputs,
				&txIn.PreviousOutPoint)
		}
	}

	// With the spent outputs gathered, attempt to prune the channel graph,
	// also passing in the hash+height of the block being pruned so the
	// prune tip can be updated.
	chansClosed, err := b.cfg.Graph.PruneGraph(spentOutputs,
		&chainUpdate.Hash, chainUpdate.Height)
	if err != nil {
		log.Errorf("unable to prune routing table: %v", err)
		return err
	}

	log.Infof("Block %v (height=%v) closed %v channels", chainUpdate.Hash,
		blockHeight, len(chansClosed))

	return nil
}

// assertNodeAnnFreshness returns a non-nil error if we have an announcement in
// the database for the passed node with a timestamp newer than the passed
// timestamp. ErrIgnored will be returned if we already have the node, and
// ErrOutdated will be returned if we have a timestamp that's after the new
// timestamp.
func (b *Builder) assertNodeAnnFreshness(ctx context.Context, node route.Vertex,
	msgTimestamp time.Time) error {

	// If we are not already aware of this node, it means that we don't
	// know about any channel using this node. To avoid a DoS attack by
	// node announcements, we will ignore such nodes. If we do know about
	// this node, check that this update brings info newer than what we
	// already have.
	lastUpdate, exists, err := b.cfg.Graph.HasNode(ctx, node)
	if err != nil {
		return fmt.Errorf("unable to query for the "+
			"existence of node: %w", err)
	}
	if !exists {
		return NewErrf(ErrIgnored, "Ignoring node announcement"+
			" for node not found in channel graph (%x)",
			node[:])
	}

	// If we've reached this point then we're aware of the vertex being
	// advertised. So we now check if the new message has a new time stamp,
	// if not then we won't accept the new data as it would override newer
	// data.
	if !lastUpdate.Before(msgTimestamp) {
		return NewErrf(ErrOutdated, "Ignoring outdated "+
			"announcement for %x", node[:])
	}

	return nil
}

// MarkZombieEdge adds a channel that failed complete validation into the zombie
// index so we can avoid having to re-validate it in the future.
func (b *Builder) MarkZombieEdge(chanID uint64) error {
	// If the edge fails validation we'll mark the edge itself as a zombie
	// so we don't continue to request it. We use the "zero key" for both
	// node pubkeys so this edge can't be resurrected.
	var zeroKey [33]byte
	err := b.cfg.Graph.MarkEdgeZombie(chanID, zeroKey, zeroKey)
	if err != nil {
		return fmt.Errorf("unable to mark spent chan(id=%v) as a "+
			"zombie: %w", chanID, err)
	}

	return nil
}

// ApplyChannelUpdate validates a channel update and if valid, applies it to the
// database. It returns a bool indicating whether the updates were successful.
func (b *Builder) ApplyChannelUpdate(msg *lnwire.ChannelUpdate1) bool {
	ctx := context.TODO()

	ch, _, _, err := b.GetChannelByID(msg.ShortChannelID)
	if err != nil {
		log.Errorf("Unable to retrieve channel by id: %v", err)
		return false
	}

	var pubKey *btcec.PublicKey

	switch msg.ChannelFlags & lnwire.ChanUpdateDirection {
	case 0:
		pubKey, _ = ch.NodeKey1()

	case 1:
		pubKey, _ = ch.NodeKey2()
	}

	// Exit early if the pubkey cannot be decided.
	if pubKey == nil {
		log.Errorf("Unable to decide pubkey with ChannelFlags=%v",
			msg.ChannelFlags)
		return false
	}

	err = netann.ValidateChannelUpdateAnn(pubKey, ch.Capacity, msg)
	if err != nil {
		log.Errorf("Unable to validate channel update: %v", err)
		return false
	}

	update := &models.ChannelEdgePolicy{
		SigBytes:                  msg.Signature.ToSignatureBytes(),
		ChannelID:                 msg.ShortChannelID.ToUint64(),
		LastUpdate:                time.Unix(int64(msg.Timestamp), 0),
		MessageFlags:              msg.MessageFlags,
		ChannelFlags:              msg.ChannelFlags,
		TimeLockDelta:             msg.TimeLockDelta,
		MinHTLC:                   msg.HtlcMinimumMsat,
		MaxHTLC:                   msg.HtlcMaximumMsat,
		FeeBaseMSat:               lnwire.MilliSatoshi(msg.BaseFee),
		FeeProportionalMillionths: lnwire.MilliSatoshi(msg.FeeRate),
		InboundFee:                msg.InboundFee.ValOpt(),
		ExtraOpaqueData:           msg.ExtraOpaqueData,
	}

	err = b.UpdateEdge(ctx, update)
	if err != nil && !IsError(err, ErrIgnored, ErrOutdated) {
		log.Errorf("Unable to apply channel update: %v", err)
		return false
	}

	return true
}

// AddNode is used to add information about a node to the router database. If
// the node with this pubkey is not present in an existing channel, it will
// be ignored.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) AddNode(ctx context.Context, node *models.Node,
	op ...batch.SchedulerOption) error {

	err := b.addNode(ctx, node, op...)
	if err != nil {
		logNetworkMsgProcessError(err)

		return err
	}

	return nil
}

// addNode does some basic checks on the given Node against what we
// currently have persisted in the graph, and then adds it to the graph. If we
// already know about the node, then we only update our DB if the new update
// has a newer timestamp than the last one we received.
func (b *Builder) addNode(ctx context.Context, node *models.Node,
	op ...batch.SchedulerOption) error {

	// Before we add the node to the database, we'll check to see if the
	// announcement is "fresh" or not. If it isn't, then we'll return an
	// error.
	err := b.assertNodeAnnFreshness(ctx, node.PubKeyBytes, node.LastUpdate)
	if err != nil {
		return err
	}

	if err := b.cfg.Graph.AddNode(ctx, node, op...); err != nil {
		return fmt.Errorf("unable to add node %x to the "+
			"graph: %w", node.PubKeyBytes, err)
	}

	log.Tracef("Updated vertex data for node=%x", node.PubKeyBytes)
	b.stats.incNumNodeUpdates()

	return nil
}

// AddEdge is used to add edge/channel to the topology of the router, after all
// information about channel will be gathered this edge/channel might be used
// in construction of payment path.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) AddEdge(ctx context.Context, edge *models.ChannelEdgeInfo,
	op ...batch.SchedulerOption) error {

	err := b.addEdge(ctx, edge, op...)
	if err != nil {
		logNetworkMsgProcessError(err)

		return err
	}

	return nil
}

// addEdge does some validation on the new channel edge against what we
// currently have persisted in the graph, and then adds it to the graph. The
// Chain View is updated with the new edge if it is successfully added to the
// graph. We only persist the channel if we currently dont have it at all in
// our graph.
func (b *Builder) addEdge(ctx context.Context, edge *models.ChannelEdgeInfo,
	op ...batch.SchedulerOption) error {

	log.Debugf("Received ChannelEdgeInfo for channel %v", edge.ChannelID)

	// Prior to processing the announcement we first check if we
	// already know of this channel, if so, then we can exit early.
	_, _, exists, isZombie, err := b.cfg.Graph.HasChannelEdge(
		edge.ChannelID,
	)
	if err != nil && !errors.Is(err, graphdb.ErrGraphNoEdgesFound) {
		return fmt.Errorf("unable to check for edge existence: %w",
			err)
	}
	if isZombie {
		return NewErrf(ErrIgnored, "ignoring msg for zombie chan_id=%v",
			edge.ChannelID)
	}
	if exists {
		return NewErrf(ErrIgnored, "ignoring msg for known chan_id=%v",
			edge.ChannelID)
	}

	if err := b.cfg.Graph.AddChannelEdge(ctx, edge, op...); err != nil {
		return fmt.Errorf("unable to add edge: %w", err)
	}

	b.stats.incNumEdgesDiscovered()

	// If AssumeChannelValid is present, of if the SCID is an alias, then
	// the gossiper would not have done the expensive work of fetching
	// a funding transaction and validating it. So we won't have the channel
	// capacity nor the funding script. So we just log and return here.
	scid := lnwire.NewShortChanIDFromInt(edge.ChannelID)
	if b.cfg.AssumeChannelValid || b.cfg.IsAlias(scid) {
		log.Tracef("New channel discovered! Link connects %x and %x "+
			"with ChannelID(%v)", edge.NodeKey1Bytes,
			edge.NodeKey2Bytes, edge.ChannelID)

		return nil
	}

	log.Debugf("New channel discovered! Link connects %x and %x with "+
		"ChannelPoint(%v): chan_id=%v, capacity=%v", edge.NodeKey1Bytes,
		edge.NodeKey2Bytes, edge.ChannelPoint, edge.ChannelID,
		edge.Capacity)

	// Otherwise, then we expect the funding script to be present on the
	// edge since it would have been fetched when the gossiper validated the
	// announcement.
	fundingPkScript, err := edge.FundingScript.UnwrapOrErr(fmt.Errorf(
		"expected the funding transaction script to be set",
	))
	if err != nil {
		return err
	}

	// As a new edge has been added to the channel graph, we'll update the
	// current UTXO filter within our active FilteredChainView so we are
	// notified if/when this channel is closed.
	filterUpdate := []graphdb.EdgePoint{
		{
			FundingPkScript: fundingPkScript,
			OutPoint:        edge.ChannelPoint,
		},
	}

	err = b.cfg.ChainView.UpdateFilter(filterUpdate, b.bestHeight.Load())
	if err != nil {
		return fmt.Errorf("unable to update chain view: %w", err)
	}

	return nil
}

// UpdateEdge is used to update edge information, without this message edge
// considered as not fully constructed.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) UpdateEdge(ctx context.Context,
	update *models.ChannelEdgePolicy, op ...batch.SchedulerOption) error {

	err := b.updateEdge(ctx, update, op...)
	if err != nil {
		logNetworkMsgProcessError(err)

		return err
	}

	return nil
}

// updateEdge validates the new edge policy against what we currently have
// persisted in the graph, and then applies it to the graph if the update is
// considered fresh enough and if we actually have a channel persisted for the
// given update.
func (b *Builder) updateEdge(ctx context.Context,
	policy *models.ChannelEdgePolicy, op ...batch.SchedulerOption) error {

	log.Debugf("Received ChannelEdgePolicy for channel %v",
		policy.ChannelID)

	// We make sure to hold the mutex for this channel ID, such that no
	// other goroutine is concurrently doing database accesses for the same
	// channel ID.
	b.channelEdgeMtx.Lock(policy.ChannelID)
	defer b.channelEdgeMtx.Unlock(policy.ChannelID)

	edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
		b.cfg.Graph.HasChannelEdge(policy.ChannelID)
	if err != nil && !errors.Is(err, graphdb.ErrGraphNoEdgesFound) {
		return fmt.Errorf("unable to check for edge existence: %w", err)
	}

	// If the channel is marked as a zombie in our database, and
	// we consider this a stale update, then we should not apply the
	// policy.
	isStaleUpdate := time.Since(policy.LastUpdate) >
		b.cfg.ChannelPruneExpiry

	if isZombie && isStaleUpdate {
		return NewErrf(ErrIgnored, "ignoring stale update "+
			"(flags=%v|%v) for zombie chan_id=%v",
			policy.MessageFlags, policy.ChannelFlags,
			policy.ChannelID)
	}

	// If the channel doesn't exist in our database, we cannot apply the
	// updated policy.
	if !exists {
		return NewErrf(ErrIgnored, "ignoring update (flags=%v|%v) for "+
			"unknown chan_id=%v", policy.MessageFlags,
			policy.ChannelFlags, policy.ChannelID)
	}

	log.Debugf("Found edge1Timestamp=%v, edge2Timestamp=%v",
		edge1Timestamp, edge2Timestamp)

	// As edges are directional edge node has a unique policy for the
	// direction of the edge they control. Therefore, we first check if we
	// already have the most up-to-date information for that edge. If this
	// message has a timestamp not strictly newer than what we already know
	// of we can exit early.
	switch policy.ChannelFlags & lnwire.ChanUpdateDirection {
	// A flag set of 0 indicates this is an announcement for the "first"
	// node in the channel.
	case 0:
		// Ignore outdated message.
		if !edge1Timestamp.Before(policy.LastUpdate) {
			return NewErrf(ErrOutdated, "Ignoring "+
				"outdated update (flags=%v|%v) for "+
				"known chan_id=%v", policy.MessageFlags,
				policy.ChannelFlags, policy.ChannelID)
		}

	// Similarly, a flag set of 1 indicates this is an announcement
	// for the "second" node in the channel.
	case 1:
		// Ignore outdated message.
		if !edge2Timestamp.Before(policy.LastUpdate) {
			return NewErrf(ErrOutdated, "Ignoring "+
				"outdated update (flags=%v|%v) for "+
				"known chan_id=%v", policy.MessageFlags,
				policy.ChannelFlags, policy.ChannelID)
		}
	}

	// Now that we know this isn't a stale update, we'll apply the new edge
	// policy to the proper directional edge within the channel graph.
	if err = b.cfg.Graph.UpdateEdgePolicy(ctx, policy, op...); err != nil {
		err := fmt.Errorf("unable to add channel: %w", err)
		log.Error(err)
		return err
	}

	log.Tracef("New channel update applied: %v",
		lnutils.SpewLogClosure(policy))
	b.stats.incNumChannelUpdates()

	return nil
}

// logNetworkMsgProcessError logs the error received from processing a network
// message. It logs as a debug message if the error is not critical.
func logNetworkMsgProcessError(err error) {
	if IsError(err, ErrIgnored, ErrOutdated) {
		log.Debugf("process network updates got: %v", err)

		return
	}

	log.Errorf("process network updates got: %v", err)
}

// CurrentBlockHeight returns the block height from POV of the router subsystem.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) CurrentBlockHeight() (uint32, error) {
	_, height, err := b.cfg.Chain.GetBestBlock()
	return uint32(height), err
}

// SyncedHeight returns the block height to which the router subsystem currently
// is synced to. This can differ from the above chain height if the goroutine
// responsible for processing the blocks isn't yet up to speed.
func (b *Builder) SyncedHeight() uint32 {
	return b.bestHeight.Load()
}

// GetChannelByID return the channel by the channel id.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) GetChannelByID(chanID lnwire.ShortChannelID) (
	*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return b.cfg.Graph.FetchChannelEdgesByID(chanID.ToUint64())
}

// FetchNode attempts to look up a target node by its identity public
// key. graphdb.ErrGraphNodeNotFound is returned if the node doesn't exist
// within the graph.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) FetchNode(ctx context.Context,
	node route.Vertex) (*models.Node, error) {

	return b.cfg.Graph.FetchNode(ctx, node)
}

// ForAllOutgoingChannels is used to iterate over all outgoing channels owned by
// the router.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) ForAllOutgoingChannels(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy) error,
	reset func()) error {

	return b.cfg.Graph.ForEachNodeChannel(
		ctx, b.cfg.SelfNode,
		func(c *models.ChannelEdgeInfo, e *models.ChannelEdgePolicy,
			_ *models.ChannelEdgePolicy) error {

			if e == nil {
				return fmt.Errorf("channel from self node " +
					"has no policy")
			}

			return cb(c, e)
		}, reset,
	)
}

// AddProof updates the channel edge info with proof which is needed to
// properly announce the edge to the rest of the network.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) AddProof(chanID lnwire.ShortChannelID,
	proof *models.ChannelAuthProof) error {

	return b.cfg.Graph.AddEdgeProof(chanID, proof)
}

// IsStaleNode returns true if the graph source has a node announcement for the
// target node with a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) IsStaleNode(ctx context.Context, node route.Vertex,
	timestamp time.Time) bool {

	// If our attempt to assert that the node announcement is fresh fails,
	// then we know that this is actually a stale announcement.
	err := b.assertNodeAnnFreshness(ctx, node, timestamp)
	if err != nil {
		log.Debugf("Checking stale node %s got %v", node, err)
		return true
	}

	return false
}

// IsPublicNode determines whether the given vertex is seen as a public node in
// the graph from the graph's source node's point of view.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) IsPublicNode(node route.Vertex) (bool, error) {
	return b.cfg.Graph.IsPublicNode(node)
}

// IsKnownEdge returns true if the graph source already knows of the passed
// channel ID either as a live or zombie edge.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) IsKnownEdge(chanID lnwire.ShortChannelID) bool {
	_, _, exists, isZombie, _ := b.cfg.Graph.HasChannelEdge(
		chanID.ToUint64(),
	)

	return exists || isZombie
}

// IsZombieEdge returns true if the graph source has marked the given channel ID
// as a zombie edge.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) IsZombieEdge(chanID lnwire.ShortChannelID) (bool, error) {
	_, _, _, isZombie, err := b.cfg.Graph.HasChannelEdge(chanID.ToUint64())

	return isZombie, err
}

// IsStaleEdgePolicy returns true if the graph source has a channel edge for
// the passed channel ID (and flags) that have a more recent timestamp.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) IsStaleEdgePolicy(chanID lnwire.ShortChannelID,
	timestamp time.Time, flags lnwire.ChanUpdateChanFlags) bool {

	edge1Timestamp, edge2Timestamp, exists, isZombie, err :=
		b.cfg.Graph.HasChannelEdge(chanID.ToUint64())
	if err != nil {
		log.Debugf("Check stale edge policy got error: %v", err)
		return false
	}

	// If we know of the edge as a zombie, then we'll make some additional
	// checks to determine if the new policy is fresh.
	if isZombie {
		// When running with AssumeChannelValid, we also prune channels
		// if both of their edges are disabled. We'll mark the new
		// policy as stale if it remains disabled.
		if b.cfg.AssumeChannelValid {
			isDisabled := flags&lnwire.ChanUpdateDisabled ==
				lnwire.ChanUpdateDisabled
			if isDisabled {
				return true
			}
		}

		// Otherwise, we'll fall back to our usual ChannelPruneExpiry.
		return time.Since(timestamp) > b.cfg.ChannelPruneExpiry
	}

	// If we don't know of the edge, then it means it's fresh (thus not
	// stale).
	if !exists {
		return false
	}

	// As edges are directional edge node has a unique policy for the
	// direction of the edge they control. Therefore, we first check if we
	// already have the most up-to-date information for that edge. If so,
	// then we can exit early.
	switch {
	// A flag set of 0 indicates this is an announcement for the "first"
	// node in the channel.
	case flags&lnwire.ChanUpdateDirection == 0:
		return !edge1Timestamp.Before(timestamp)

	// Similarly, a flag set of 1 indicates this is an announcement for the
	// "second" node in the channel.
	case flags&lnwire.ChanUpdateDirection == 1:
		return !edge2Timestamp.Before(timestamp)
	}

	return false
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
//
// NOTE: This method is part of the ChannelGraphSource interface.
func (b *Builder) MarkEdgeLive(chanID lnwire.ShortChannelID) error {
	return b.cfg.Graph.MarkEdgeLive(chanID.ToUint64())
}
