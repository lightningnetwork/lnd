package contractcourt

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrChainArbExiting signals that the chain arbitrator is shutting down.
var ErrChainArbExiting = errors.New("ChainArbitrator exiting")

// ResolutionMsg is a message sent by resolvers to outside sub-systems once an
// outgoing contract has been fully resolved. For multi-hop contracts, if we
// resolve the outgoing contract, we'll also need to ensure that the incoming
// contract is resolved as well. We package the items required to resolve the
// incoming contracts within this message.
type ResolutionMsg struct {
	// SourceChan identifies the channel that this message is being sent
	// from. This is the channel's short channel ID.
	SourceChan lnwire.ShortChannelID

	// HtlcIndex is the index of the contract within the original
	// commitment trace.
	HtlcIndex uint64

	// Failure will be non-nil if the incoming contract should be canceled
	// all together. This can happen if the outgoing contract was dust, if
	// if the outgoing HTLC timed out.
	Failure lnwire.FailureMessage

	// PreImage will be non-nil if the incoming contract can successfully
	// be redeemed. This can happen if we learn of the preimage from the
	// outgoing HTLC on-chain.
	PreImage *[32]byte
}

// ChainArbitratorConfig is a configuration struct that contains all the
// function closures and interface that required to arbitrate on-chain
// contracts for a particular chain.
type ChainArbitratorConfig struct {
	// ChainHash is the chain that this arbitrator is to operate within.
	ChainHash chainhash.Hash

	// IncomingBroadcastDelta is the delta that we'll use to decide when to
	// broadcast our commitment transaction if we have incoming htlcs. This
	// value should be set based on our current fee estimation of the
	// commitment transaction. We use this to determine when we should
	// broadcast instead of the just the HTLC timeout, as we want to ensure
	// that the commitment transaction is already confirmed, by the time the
	// HTLC expires. Otherwise we may end up not settling the htlc on-chain
	// because the other party managed to time it out.
	IncomingBroadcastDelta uint32

	// OutgoingBroadcastDelta is the delta that we'll use to decide when to
	// broadcast our commitment transaction if there are active outgoing
	// htlcs. This value can be lower than the incoming broadcast delta.
	OutgoingBroadcastDelta uint32

	// NewSweepAddr is a function that returns a new address under control
	// by the wallet. We'll use this to sweep any no-delay outputs as a
	// result of unilateral channel closes.
	//
	// NOTE: This SHOULD return a p2wkh script.
	NewSweepAddr func() ([]byte, error)

	// PublishTx reliably broadcasts a transaction to the network. Once
	// this function exits without an error, then they transaction MUST
	// continually be rebroadcast if needed.
	PublishTx func(*wire.MsgTx) error

	// DeliverResolutionMsg is a function that will append an outgoing
	// message to the "out box" for a ChannelLink. This is used to cancel
	// backwards any HTLC's that are either dust, we're timing out, or
	// settling on-chain to the incoming link.
	DeliverResolutionMsg func(...ResolutionMsg) error

	// MarkLinkInactive is a function closure that the ChainArbitrator will
	// use to mark that active HTLC's shouldn't be attempt ted to be routed
	// over a particular channel. This function will be called in that a
	// ChannelArbitrator decides that it needs to go to chain in order to
	// resolve contracts.
	//
	// TODO(roasbeef): rename, routing based
	MarkLinkInactive func(wire.OutPoint) error

	// ContractBreach is a function closure that the ChainArbitrator will
	// use to notify the breachArbiter about a contract breach. It should
	// only return a non-nil error when the breachArbiter has preserved the
	// necessary breach info for this channel point, and it is safe to mark
	// the channel as pending close in the database.
	ContractBreach func(wire.OutPoint, *lnwallet.BreachRetribution) error

	// IsOurAddress is a function that returns true if the passed address
	// is known to the underlying wallet. Otherwise, false should be
	// returned.
	IsOurAddress func(btcutil.Address) bool

	// IncubateOutput sends either an incoming HTLC, an outgoing HTLC, or
	// both to the utxo nursery. Once this function returns, the nursery
	// should have safely persisted the outputs to disk, and should start
	// the process of incubation. This is used when a resolver wishes to
	// pass off the output to the nursery as we're only waiting on an
	// absolute/relative item block.
	IncubateOutputs func(wire.OutPoint, *lnwallet.OutgoingHtlcResolution,
		*lnwallet.IncomingHtlcResolution, uint32) error

	// PreimageDB is a global store of all known pre-images. We'll use this
	// to decide if we should broadcast a commitment transaction to claim
	// an HTLC on-chain.
	PreimageDB WitnessBeacon

	// Notifier is an instance of a chain notifier we'll use to watch for
	// certain on-chain events.
	Notifier chainntnfs.ChainNotifier

	// Signer is a signer backed by the active lnd node. This should be
	// capable of producing a signature as specified by a valid
	// SignDescriptor.
	Signer input.Signer

	// FeeEstimator will be used to return fee estimates.
	FeeEstimator chainfee.Estimator

	// ChainIO allows us to query the state of the current main chain.
	ChainIO lnwallet.BlockChainIO

	// DisableChannel disables a channel, resulting in it not being able to
	// forward payments.
	DisableChannel func(wire.OutPoint) error

	// Sweeper allows resolvers to sweep their final outputs.
	Sweeper UtxoSweeper

	// Registry is the invoice database that is used by resolvers to lookup
	// preimages and settle invoices.
	Registry Registry

	// NotifyClosedChannel is a function closure that the ChainArbitrator
	// will use to notify the ChannelNotifier about a newly closed channel.
	NotifyClosedChannel func(wire.OutPoint)

	// OnionProcessor is used to decode onion payloads for on-chain
	// resolution.
	OnionProcessor OnionProcessor
}

// ChainArbitrator is a sub-system that oversees the on-chain resolution of all
// active, and channel that are in the "pending close" state. Within the
// contractcourt package, the ChainArbitrator manages a set of active
// ContractArbitrators. Each ContractArbitrators is responsible for watching
// the chain for any activity that affects the state of the channel, and also
// for monitoring each contract in order to determine if any on-chain activity is
// required. Outside sub-systems interact with the ChainArbitrator in order to
// forcibly exit a contract, update the set of live signals for each contract,
// and to receive reports on the state of contract resolution.
type ChainArbitrator struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	sync.Mutex

	// activeChannels is a map of all the active contracts that are still
	// open, and not fully resolved.
	activeChannels map[wire.OutPoint]*ChannelArbitrator

	// activeWatchers is a map of all the active chainWatchers for channels
	// that are still considered open.
	activeWatchers map[wire.OutPoint]*chainWatcher

	// cfg is the config struct for the arbitrator that contains all
	// methods and interface it needs to operate.
	cfg ChainArbitratorConfig

	// chanSource will be used by the ChainArbitrator to fetch all the
	// active channels that it must still watch over.
	chanSource *channeldb.DB

	quit chan struct{}

	wg sync.WaitGroup
}

// NewChainArbitrator returns a new instance of the ChainArbitrator using the
// passed config struct, and backing persistent database.
func NewChainArbitrator(cfg ChainArbitratorConfig,
	db *channeldb.DB) *ChainArbitrator {

	return &ChainArbitrator{
		cfg:            cfg,
		activeChannels: make(map[wire.OutPoint]*ChannelArbitrator),
		activeWatchers: make(map[wire.OutPoint]*chainWatcher),
		chanSource:     db,
		quit:           make(chan struct{}),
	}
}

// newActiveChannelArbitrator creates a new instance of an active channel
// arbitrator given the state of the target channel.
func newActiveChannelArbitrator(channel *channeldb.OpenChannel,
	c *ChainArbitrator, chanEvents *ChainEventSubscription) (*ChannelArbitrator, error) {

	log.Tracef("Creating ChannelArbitrator for ChannelPoint(%v)",
		channel.FundingOutpoint)

	// We'll start by registering for a block epoch notifications so this
	// channel can keep track of the current state of the main chain.
	//
	// TODO(roasbeef): fetch best height (or pass in) so can ensure block
	// epoch delivers all the notifications to
	//
	// TODO(roasbeef): instead 1 block epoch that multi-plexes to the rest?
	//  * reduces the number of goroutines
	blockEpoch, err := c.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, err
	}

	chanPoint := channel.FundingOutpoint

	// Next we'll create the matching configuration struct that contains
	// all interfaces and methods the arbitrator needs to do its job.
	arbCfg := ChannelArbitratorConfig{
		ChanPoint:   chanPoint,
		ShortChanID: channel.ShortChanID(),
		BlockEpochs: blockEpoch,
		ForceCloseChan: func() (*wire.MsgTx, error) {
			// First, we mark the channel as borked, this ensure
			// that no new state transitions can happen, and also
			// that the link won't be loaded into the switch.
			if err := channel.MarkBorked(); err != nil {
				return nil, err
			}

			// With the channel marked as borked, we'll now remove
			// the link from the switch if its there. If the link
			// is active, then this method will block until it
			// exits.
			if err := c.cfg.MarkLinkInactive(chanPoint); err != nil {
				log.Errorf("unable to mark link inactive: %v", err)
			}

			// Now that we know the link can't mutate the channel
			// state, we'll read the channel from disk the target
			// channel according to its channel point.
			channel, err := c.chanSource.FetchChannel(chanPoint)
			if err != nil {
				return nil, err
			}

			// Finally, we'll force close the channel completing
			// the force close workflow.
			chanMachine, err := lnwallet.NewLightningChannel(
				c.cfg.Signer, channel, nil,
			)
			if err != nil {
				return nil, err
			}
			return chanMachine.ForceClose()
		},
		MarkCommitmentBroadcasted: channel.MarkCommitmentBroadcasted,
		MarkChannelClosed: func(summary *channeldb.ChannelCloseSummary) error {
			if err := channel.CloseChannel(summary); err != nil {
				return err
			}
			c.cfg.NotifyClosedChannel(summary.ChanPoint)
			return nil
		},
		IsPendingClose:        false,
		ChainArbitratorConfig: c.cfg,
		ChainEvents:           chanEvents,
	}

	// The final component needed is an arbitrator log that the arbitrator
	// will use to keep track of its internal state using a backed
	// persistent log.
	//
	// TODO(roasbeef); abstraction leak...
	//  * rework: adaptor method to set log scope w/ factory func
	chanLog, err := newBoltArbitratorLog(
		c.chanSource.DB, arbCfg, c.cfg.ChainHash, chanPoint,
	)
	if err != nil {
		blockEpoch.Cancel()
		return nil, err
	}

	arbCfg.MarkChannelResolved = func() error {
		return c.ResolveContract(chanPoint)
	}

	// Finally, we'll need to construct a series of htlc Sets based on all
	// currently known valid commitments.
	htlcSets := make(map[HtlcSetKey]htlcSet)
	htlcSets[LocalHtlcSet] = newHtlcSet(channel.LocalCommitment.Htlcs)
	htlcSets[RemoteHtlcSet] = newHtlcSet(channel.RemoteCommitment.Htlcs)

	pendingRemoteCommitment, err := channel.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
		blockEpoch.Cancel()
		return nil, err
	}
	if pendingRemoteCommitment != nil {
		htlcSets[RemotePendingHtlcSet] = newHtlcSet(
			pendingRemoteCommitment.Commitment.Htlcs,
		)
	}

	return NewChannelArbitrator(
		arbCfg, htlcSets, chanLog,
	), nil
}

// ResolveContract marks a contract as fully resolved within the database.
// This is only to be done once all contracts which were live on the channel
// before hitting the chain have been resolved.
func (c *ChainArbitrator) ResolveContract(chanPoint wire.OutPoint) error {

	log.Infof("Marking ChannelPoint(%v) fully resolved", chanPoint)

	// First, we'll we'll mark the channel as fully closed from the PoV of
	// the channel source.
	err := c.chanSource.MarkChanFullyClosed(&chanPoint)
	if err != nil {
		log.Errorf("ChainArbitrator: unable to mark ChannelPoint(%v) "+
			"fully closed: %v", chanPoint, err)
		return err
	}

	// Now that the channel has been marked as fully closed, we'll stop
	// both the channel arbitrator and chain watcher for this channel if
	// they're still active.
	var arbLog ArbitratorLog
	c.Lock()
	chainArb := c.activeChannels[chanPoint]
	delete(c.activeChannels, chanPoint)

	chainWatcher := c.activeWatchers[chanPoint]
	delete(c.activeWatchers, chanPoint)
	c.Unlock()

	if chainArb != nil {
		arbLog = chainArb.log

		if err := chainArb.Stop(); err != nil {
			log.Warnf("unable to stop ChannelArbitrator(%v): %v",
				chanPoint, err)
		}
	}
	if chainWatcher != nil {
		if err := chainWatcher.Stop(); err != nil {
			log.Warnf("unable to stop ChainWatcher(%v): %v",
				chanPoint, err)
		}
	}

	// Once this has been marked as resolved, we'll wipe the log that the
	// channel arbitrator was using to store its persistent state. We do
	// this after marking the channel resolved, as otherwise, the
	// arbitrator would be re-created, and think it was starting from the
	// default state.
	if arbLog != nil {
		if err := arbLog.WipeHistory(); err != nil {
			return err
		}
	}

	return nil
}

// Start launches all goroutines that the ChainArbitrator needs to operate.
func (c *ChainArbitrator) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	log.Tracef("Starting ChainArbitrator")

	// First, we'll fetch all the channels that are still open, in order to
	// collect them within our set of active contracts.
	openChannels, err := c.chanSource.FetchAllChannels()
	if err != nil {
		return err
	}

	if len(openChannels) > 0 {
		log.Infof("Creating ChannelArbitrators for %v active channels",
			len(openChannels))
	}

	// For each open channel, we'll configure then launch a corresponding
	// ChannelArbitrator.
	for _, channel := range openChannels {
		chanPoint := channel.FundingOutpoint
		channel := channel

		// First, we'll create an active chainWatcher for this channel
		// to ensure that we detect any relevant on chain events.
		chainWatcher, err := newChainWatcher(
			chainWatcherConfig{
				chanState: channel,
				notifier:  c.cfg.Notifier,
				signer:    c.cfg.Signer,
				isOurAddr: c.cfg.IsOurAddress,
				contractBreach: func(retInfo *lnwallet.BreachRetribution) error {
					return c.cfg.ContractBreach(chanPoint, retInfo)
				},
				extractStateNumHint: lnwallet.GetStateNumHint,
			},
		)
		if err != nil {
			return err
		}

		c.activeWatchers[chanPoint] = chainWatcher
		channelArb, err := newActiveChannelArbitrator(
			channel, c, chainWatcher.SubscribeChannelEvents(),
		)
		if err != nil {
			return err
		}

		c.activeChannels[chanPoint] = channelArb

		// If the channel has had its commitment broadcasted already,
		// republish it in case it didn't propagate.
		if !channel.HasChanStatus(
			channeldb.ChanStatusCommitBroadcasted,
		) {
			continue
		}

		closeTx, err := channel.BroadcastedCommitment()
		switch {

		// This can happen for channels that had their closing tx
		// published before we started storing it to disk.
		case err == channeldb.ErrNoCloseTx:
			log.Warnf("Channel %v is in state CommitBroadcasted, "+
				"but no closing tx to re-publish...", chanPoint)
			continue

		case err != nil:
			return err
		}

		log.Infof("Re-publishing closing tx(%v) for channel %v",
			closeTx.TxHash(), chanPoint)
		err = c.cfg.PublishTx(closeTx)
		if err != nil && err != lnwallet.ErrDoubleSpend {
			log.Warnf("Unable to broadcast close tx(%v): %v",
				closeTx.TxHash(), err)
		}
	}

	// In addition to the channels that we know to be open, we'll also
	// launch arbitrators to finishing resolving any channels that are in
	// the pending close state.
	closingChannels, err := c.chanSource.FetchClosedChannels(true)
	if err != nil {
		return err
	}

	if len(closingChannels) > 0 {
		log.Infof("Creating ChannelArbitrators for %v closing channels",
			len(closingChannels))
	}

	// Next, for each channel is the closing state, we'll launch a
	// corresponding more restricted resolver, as we don't have to watch
	// the chain any longer, only resolve the contracts on the confirmed
	// commitment.
	for _, closeChanInfo := range closingChannels {
		blockEpoch, err := c.cfg.Notifier.RegisterBlockEpochNtfn(nil)
		if err != nil {
			return err
		}

		// We can leave off the CloseContract and ForceCloseChan
		// methods as the channel is already closed at this point.
		chanPoint := closeChanInfo.ChanPoint
		arbCfg := ChannelArbitratorConfig{
			ChanPoint:             chanPoint,
			ShortChanID:           closeChanInfo.ShortChanID,
			BlockEpochs:           blockEpoch,
			ChainArbitratorConfig: c.cfg,
			ChainEvents:           &ChainEventSubscription{},
			IsPendingClose:        true,
			ClosingHeight:         closeChanInfo.CloseHeight,
			CloseType:             closeChanInfo.CloseType,
		}
		chanLog, err := newBoltArbitratorLog(
			c.chanSource.DB, arbCfg, c.cfg.ChainHash, chanPoint,
		)
		if err != nil {
			blockEpoch.Cancel()
			return err
		}
		arbCfg.MarkChannelResolved = func() error {
			return c.ResolveContract(chanPoint)
		}

		// We can also leave off the set of HTLC's here as since the
		// channel is already in the process of being full resolved, no
		// new HTLC's will be added.
		c.activeChannels[chanPoint] = NewChannelArbitrator(
			arbCfg, nil, chanLog,
		)
	}

	// Now, we'll start all chain watchers in parallel to shorten start up
	// duration. In neutrino mode, this allows spend registrations to take
	// advantage of batch spend reporting, instead of doing a single rescan
	// per chain watcher.
	//
	// NOTE: After this point, we Stop the chain arb to ensure that any
	// lingering goroutines are cleaned up before exiting.
	watcherErrs := make(chan error, len(c.activeWatchers))
	var wg sync.WaitGroup
	for _, watcher := range c.activeWatchers {
		wg.Add(1)
		go func(w *chainWatcher) {
			defer wg.Done()
			select {
			case watcherErrs <- w.Start():
			case <-c.quit:
				watcherErrs <- ErrChainArbExiting
			}
		}(watcher)
	}

	// Once all chain watchers have been started, seal the err chan to
	// signal the end of the err stream.
	go func() {
		wg.Wait()
		close(watcherErrs)
	}()

	// Handle all errors returned from spawning our chain watchers. If any
	// of them failed, we will stop the chain arb to shutdown any active
	// goroutines.
	for err := range watcherErrs {
		if err != nil {
			c.Stop()
			return err
		}
	}

	// Finally, we'll launch all the goroutines for each arbitrator so they
	// can carry out their duties.
	for _, arbitrator := range c.activeChannels {
		if err := arbitrator.Start(); err != nil {
			c.Stop()
			return err
		}
	}

	// TODO(roasbeef): eventually move all breach watching here

	return nil
}

// Stop signals the ChainArbitrator to trigger a graceful shutdown. Any active
// channel arbitrators will be signalled to exit, and this method will block
// until they've all exited.
func (c *ChainArbitrator) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	log.Infof("Stopping ChainArbitrator")

	close(c.quit)

	var (
		activeWatchers = make(map[wire.OutPoint]*chainWatcher)
		activeChannels = make(map[wire.OutPoint]*ChannelArbitrator)
	)

	// Copy the current set of active watchers and arbitrators to shutdown.
	// We don't want to hold the lock when shutting down each watcher or
	// arbitrator individually, as they may need to acquire this mutex.
	c.Lock()
	for chanPoint, watcher := range c.activeWatchers {
		activeWatchers[chanPoint] = watcher
	}
	for chanPoint, arbitrator := range c.activeChannels {
		activeChannels[chanPoint] = arbitrator
	}
	c.Unlock()

	for chanPoint, watcher := range activeWatchers {
		log.Tracef("Attempting to stop ChainWatcher(%v)",
			chanPoint)

		if err := watcher.Stop(); err != nil {
			log.Errorf("unable to stop watcher for "+
				"ChannelPoint(%v): %v", chanPoint, err)
		}
	}
	for chanPoint, arbitrator := range activeChannels {
		log.Tracef("Attempting to stop ChannelArbitrator(%v)",
			chanPoint)

		if err := arbitrator.Stop(); err != nil {
			log.Errorf("unable to stop arbitrator for "+
				"ChannelPoint(%v): %v", chanPoint, err)
		}
	}

	c.wg.Wait()

	return nil
}

// ContractUpdate is a message packages the latest set of active HTLCs on a
// commitment, and also identifies which commitment received a new set of
// HTLCs.
type ContractUpdate struct {
	// HtlcKey identifies which commitment the HTLCs below are present on.
	HtlcKey HtlcSetKey

	// Htlcs are the of active HTLCs on the commitment identified by the
	// above HtlcKey.
	Htlcs []channeldb.HTLC
}

// ContractSignals wraps the two signals that affect the state of a channel
// being watched by an arbitrator. The two signals we care about are: the
// channel has a new set of HTLC's, and the remote party has just broadcast
// their version of the commitment transaction.
type ContractSignals struct {
	// HtlcUpdates is a channel that the link will use to update the
	// designated channel arbitrator when the set of HTLCs on any valid
	// commitment changes.
	HtlcUpdates chan *ContractUpdate

	// ShortChanID is the up to date short channel ID for a contract. This
	// can change either if when the contract was added it didn't yet have
	// a stable identifier, or in the case of a reorg.
	ShortChanID lnwire.ShortChannelID
}

// UpdateContractSignals sends a set of active, up to date contract signals to
// the ChannelArbitrator which is has been assigned to the channel infield by
// the passed channel point.
func (c *ChainArbitrator) UpdateContractSignals(chanPoint wire.OutPoint,
	signals *ContractSignals) error {

	log.Infof("Attempting to update ContractSignals for ChannelPoint(%v)",
		chanPoint)

	c.Lock()
	arbitrator, ok := c.activeChannels[chanPoint]
	c.Unlock()
	if !ok {
		return fmt.Errorf("unable to find arbitrator")
	}

	arbitrator.UpdateContractSignals(signals)

	return nil
}

// GetChannelArbitrator safely returns the channel arbitrator for a given
// channel outpoint.
func (c *ChainArbitrator) GetChannelArbitrator(chanPoint wire.OutPoint) (
	*ChannelArbitrator, error) {

	c.Lock()
	arbitrator, ok := c.activeChannels[chanPoint]
	c.Unlock()
	if !ok {
		return nil, fmt.Errorf("unable to find arbitrator")
	}

	return arbitrator, nil
}

// forceCloseReq is a request sent from an outside sub-system to the arbitrator
// that watches a particular channel to broadcast the commitment transaction,
// and enter the resolution phase of the channel.
type forceCloseReq struct {
	// errResp is a channel that will be sent upon either in the case of
	// force close success (nil error), or in the case on an error.
	//
	// NOTE; This channel MUST be buffered.
	errResp chan error

	// closeTx is a channel that carries the transaction which ultimately
	// closed out the channel.
	closeTx chan *wire.MsgTx
}

// ForceCloseContract attempts to force close the channel infield by the passed
// channel point. A force close will immediately terminate the contract,
// causing it to enter the resolution phase. If the force close was successful,
// then the force close transaction itself will be returned.
//
// TODO(roasbeef): just return the summary itself?
func (c *ChainArbitrator) ForceCloseContract(chanPoint wire.OutPoint) (*wire.MsgTx, error) {
	c.Lock()
	arbitrator, ok := c.activeChannels[chanPoint]
	c.Unlock()
	if !ok {
		return nil, fmt.Errorf("unable to find arbitrator")
	}

	log.Infof("Attempting to force close ChannelPoint(%v)", chanPoint)

	// Before closing, we'll attempt to send a disable update for the
	// channel. We do so before closing the channel as otherwise the current
	// edge policy won't be retrievable from the graph.
	if err := c.cfg.DisableChannel(chanPoint); err != nil {
		log.Warnf("Unable to disable channel %v on "+
			"close: %v", chanPoint, err)
	}

	errChan := make(chan error, 1)
	respChan := make(chan *wire.MsgTx, 1)

	// With the channel found, and the request crafted, we'll send over a
	// force close request to the arbitrator that watches this channel.
	select {
	case arbitrator.forceCloseReqs <- &forceCloseReq{
		errResp: errChan,
		closeTx: respChan,
	}:
	case <-c.quit:
		return nil, ErrChainArbExiting
	}

	// We'll await two responses: the error response, and the transaction
	// that closed out the channel.
	select {
	case err := <-errChan:
		if err != nil {
			return nil, err
		}
	case <-c.quit:
		return nil, ErrChainArbExiting
	}

	var closeTx *wire.MsgTx
	select {
	case closeTx = <-respChan:
	case <-c.quit:
		return nil, ErrChainArbExiting
	}

	return closeTx, nil
}

// WatchNewChannel sends the ChainArbitrator a message to create a
// ChannelArbitrator tasked with watching over a new channel. Once a new
// channel has finished its final funding flow, it should be registered with
// the ChainArbitrator so we can properly react to any on-chain events.
func (c *ChainArbitrator) WatchNewChannel(newChan *channeldb.OpenChannel) error {
	c.Lock()
	defer c.Unlock()

	log.Infof("Creating new ChannelArbitrator for ChannelPoint(%v)",
		newChan.FundingOutpoint)

	// If we're already watching this channel, then we'll ignore this
	// request.
	chanPoint := newChan.FundingOutpoint
	if _, ok := c.activeChannels[chanPoint]; ok {
		return nil
	}

	// First, also create an active chainWatcher for this channel to ensure
	// that we detect any relevant on chain events.
	chainWatcher, err := newChainWatcher(
		chainWatcherConfig{
			chanState: newChan,
			notifier:  c.cfg.Notifier,
			signer:    c.cfg.Signer,
			isOurAddr: c.cfg.IsOurAddress,
			contractBreach: func(retInfo *lnwallet.BreachRetribution) error {
				return c.cfg.ContractBreach(chanPoint, retInfo)
			},
			extractStateNumHint: lnwallet.GetStateNumHint,
		},
	)
	if err != nil {
		return err
	}

	c.activeWatchers[newChan.FundingOutpoint] = chainWatcher

	// We'll also create a new channel arbitrator instance using this new
	// channel, and our internal state.
	channelArb, err := newActiveChannelArbitrator(
		newChan, c, chainWatcher.SubscribeChannelEvents(),
	)
	if err != nil {
		return err
	}

	// With the arbitrator created, we'll add it to our set of active
	// arbitrators, then launch it.
	c.activeChannels[chanPoint] = channelArb

	if err := channelArb.Start(); err != nil {
		return err
	}

	return chainWatcher.Start()
}

// SubscribeChannelEvents returns a new active subscription for the set of
// possible on-chain events for a particular channel. The struct can be used by
// callers to be notified whenever an event that changes the state of the
// channel on-chain occurs.
func (c *ChainArbitrator) SubscribeChannelEvents(
	chanPoint wire.OutPoint) (*ChainEventSubscription, error) {

	// First, we'll attempt to look up the active watcher for this channel.
	// If we can't find it, then we'll return an error back to the caller.
	watcher, ok := c.activeWatchers[chanPoint]
	if !ok {
		return nil, fmt.Errorf("unable to find watcher for: %v",
			chanPoint)
	}

	// With the watcher located, we'll request for it to create a new chain
	// event subscription client.
	return watcher.SubscribeChannelEvents(), nil
}

// TODO(roasbeef): arbitration reports
//  * types: contested, waiting for success conf, etc
