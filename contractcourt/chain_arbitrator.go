package contractcourt

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/labels"
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
	// broadcast instead of just the HTLC timeout, as we want to ensure
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
	PublishTx func(*wire.MsgTx, string) error

	// DeliverResolutionMsg is a function that will append an outgoing
	// message to the "out box" for a ChannelLink. This is used to cancel
	// backwards any HTLC's that are either dust, we're timing out, or
	// settling on-chain to the incoming link.
	DeliverResolutionMsg func(...ResolutionMsg) error

	// MarkLinkInactive is a function closure that the ChainArbitrator will
	// use to mark that active HTLC's shouldn't be attempted to be routed
	// over a particular channel. This function will be called in that a
	// ChannelArbitrator decides that it needs to go to chain in order to
	// resolve contracts.
	//
	// TODO(roasbeef): rename, routing based
	MarkLinkInactive func(wire.OutPoint) error

	// ContractBreach is a function closure that the ChainArbitrator will
	// use to notify the BreachArbitrator about a contract breach. It should
	// only return a non-nil error when the BreachArbitrator has preserved
	// the necessary breach info for this channel point. Once the breach
	// resolution is persisted in the ChannelArbitrator, it will be safe
	// to mark the channel closed.
	ContractBreach func(wire.OutPoint, *lnwallet.BreachRetribution) error

	// IsOurAddress is a function that returns true if the passed address
	// is known to the underlying wallet. Otherwise, false should be
	// returned.
	IsOurAddress func(btcutil.Address) bool

	// IncubateOutputs sends either an incoming HTLC, an outgoing HTLC, or
	// both to the utxo nursery. Once this function returns, the nursery
	// should have safely persisted the outputs to disk, and should start
	// the process of incubation. This is used when a resolver wishes to
	// pass off the output to the nursery as we're only waiting on an
	// absolute/relative item block.
	IncubateOutputs func(wire.OutPoint,
		fn.Option[lnwallet.OutgoingHtlcResolution],
		fn.Option[lnwallet.IncomingHtlcResolution],
		uint32, fn.Option[int32]) error

	// PreimageDB is a global store of all known pre-images. We'll use this
	// to decide if we should broadcast a commitment transaction to claim
	// an HTLC on-chain.
	PreimageDB WitnessBeacon

	// Notifier is an instance of a chain notifier we'll use to watch for
	// certain on-chain events.
	Notifier chainntnfs.ChainNotifier

	// Mempool is the a mempool watcher that allows us to watch for events
	// happened in mempool.
	Mempool chainntnfs.MempoolWatcher

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

	// NotifyFullyResolvedChannel is a function closure that the
	// ChainArbitrator will use to notify the ChannelNotifier about a newly
	// resolved channel. The main difference to NotifyClosedChannel is that
	// in case of a local force close the NotifyClosedChannel is called when
	// the published commitment transaction confirms while
	// NotifyFullyResolvedChannel is only called when the channel is fully
	// resolved (which includes sweeping any time locked funds).
	NotifyFullyResolvedChannel func(point wire.OutPoint)

	// OnionProcessor is used to decode onion payloads for on-chain
	// resolution.
	OnionProcessor OnionProcessor

	// PaymentsExpirationGracePeriod indicates a time window we let the
	// other node to cancel an outgoing htlc that our node has initiated and
	// has timed out.
	PaymentsExpirationGracePeriod time.Duration

	// IsForwardedHTLC checks for a given htlc, identified by channel id and
	// htlcIndex, if it is a forwarded one.
	IsForwardedHTLC func(chanID lnwire.ShortChannelID, htlcIndex uint64) bool

	// Clock is the clock implementation that ChannelArbitrator uses.
	// It is useful for testing.
	Clock clock.Clock

	// SubscribeBreachComplete is used by the breachResolver to register a
	// subscription that notifies when the breach resolution process is
	// complete.
	SubscribeBreachComplete func(op *wire.OutPoint, c chan struct{}) (
		bool, error)

	// PutFinalHtlcOutcome stores the final outcome of an htlc in the
	// database.
	PutFinalHtlcOutcome func(chanId lnwire.ShortChannelID,
		htlcId uint64, settled bool) error

	// HtlcNotifier is an interface that htlc events are sent to.
	HtlcNotifier HtlcNotifier

	// Budget is the configured budget for the arbitrator.
	Budget BudgetConfig

	// QueryIncomingCircuit is used to find the outgoing HTLC's
	// corresponding incoming HTLC circuit. It queries the circuit map for
	// a given outgoing circuit key and returns the incoming circuit key.
	//
	// TODO(yy): this is a hacky way to get around the cycling import issue
	// as we cannot import `htlcswitch` here. A proper way is to define an
	// interface here that asks for method `LookupOpenCircuit`,
	// meanwhile, turn `PaymentCircuit` into an interface or bring it to a
	// lower package.
	QueryIncomingCircuit func(circuit models.CircuitKey) *models.CircuitKey

	// AuxLeafStore is an optional store that can be used to store auxiliary
	// leaves for certain custom channel types.
	AuxLeafStore fn.Option[lnwallet.AuxLeafStore]

	// AuxSigner is an optional signer that can be used to sign auxiliary
	// leaves for certain custom channel types.
	AuxSigner fn.Option[lnwallet.AuxSigner]

	// AuxResolver is an optional interface that can be used to modify the
	// way contracts are resolved.
	AuxResolver fn.Option[lnwallet.AuxContractResolver]
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

	// Embed the blockbeat consumer struct to get access to the method
	// `NotifyBlockProcessed` and the `BlockbeatChan`.
	chainio.BeatConsumer

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

	// beat is the current best known blockbeat.
	beat chainio.Blockbeat

	// resolvedChan is used to signal that the given channel outpoint has
	// been resolved onchain. Once received, chain arbitrator will perform
	// cleanups.
	resolvedChan chan wire.OutPoint

	quit chan struct{}

	wg sync.WaitGroup
}

// NewChainArbitrator returns a new instance of the ChainArbitrator using the
// passed config struct, and backing persistent database.
func NewChainArbitrator(cfg ChainArbitratorConfig,
	db *channeldb.DB) *ChainArbitrator {

	c := &ChainArbitrator{
		cfg:            cfg,
		activeChannels: make(map[wire.OutPoint]*ChannelArbitrator),
		activeWatchers: make(map[wire.OutPoint]*chainWatcher),
		chanSource:     db,
		quit:           make(chan struct{}),
		resolvedChan:   make(chan wire.OutPoint),
	}

	// Mount the block consumer.
	c.BeatConsumer = chainio.NewBeatConsumer(c.quit, c.Name())

	return c
}

// Compile-time check for the chainio.Consumer interface.
var _ chainio.Consumer = (*ChainArbitrator)(nil)

// arbChannel is a wrapper around an open channel that channel arbitrators
// interact with.
type arbChannel struct {
	// channel is the in-memory channel state.
	channel *channeldb.OpenChannel

	// c references the chain arbitrator and is used by arbChannel
	// internally.
	c *ChainArbitrator
}

// NewAnchorResolutions returns the anchor resolutions for currently valid
// commitment transactions.
//
// NOTE: Part of the ArbChannel interface.
func (a *arbChannel) NewAnchorResolutions() (*lnwallet.AnchorResolutions,
	error) {

	// Get a fresh copy of the database state to base the anchor resolutions
	// on. Unfortunately the channel instance that we have here isn't the
	// same instance that is used by the link.
	chanPoint := a.channel.FundingOutpoint

	channel, err := a.c.chanSource.ChannelStateDB().FetchChannel(chanPoint)
	if err != nil {
		return nil, err
	}

	var chanOpts []lnwallet.ChannelOpt
	a.c.cfg.AuxLeafStore.WhenSome(func(s lnwallet.AuxLeafStore) {
		chanOpts = append(chanOpts, lnwallet.WithLeafStore(s))
	})
	a.c.cfg.AuxSigner.WhenSome(func(s lnwallet.AuxSigner) {
		chanOpts = append(chanOpts, lnwallet.WithAuxSigner(s))
	})
	a.c.cfg.AuxResolver.WhenSome(func(s lnwallet.AuxContractResolver) {
		chanOpts = append(chanOpts, lnwallet.WithAuxResolver(s))
	})

	chanMachine, err := lnwallet.NewLightningChannel(
		a.c.cfg.Signer, channel, nil, chanOpts...,
	)
	if err != nil {
		return nil, err
	}

	return chanMachine.NewAnchorResolutions()
}

// ForceCloseChan should force close the contract that this attendant is
// watching over. We'll use this when we decide that we need to go to chain. It
// should in addition tell the switch to remove the corresponding link, such
// that we won't accept any new updates.
//
// NOTE: Part of the ArbChannel interface.
func (a *arbChannel) ForceCloseChan() (*wire.MsgTx, error) {
	// First, we mark the channel as borked, this ensure
	// that no new state transitions can happen, and also
	// that the link won't be loaded into the switch.
	if err := a.channel.MarkBorked(); err != nil {
		return nil, err
	}

	// With the channel marked as borked, we'll now remove
	// the link from the switch if its there. If the link
	// is active, then this method will block until it
	// exits.
	chanPoint := a.channel.FundingOutpoint

	if err := a.c.cfg.MarkLinkInactive(chanPoint); err != nil {
		log.Errorf("unable to mark link inactive: %v", err)
	}

	// Now that we know the link can't mutate the channel
	// state, we'll read the channel from disk the target
	// channel according to its channel point.
	channel, err := a.c.chanSource.ChannelStateDB().FetchChannel(chanPoint)
	if err != nil {
		return nil, err
	}

	var chanOpts []lnwallet.ChannelOpt
	a.c.cfg.AuxLeafStore.WhenSome(func(s lnwallet.AuxLeafStore) {
		chanOpts = append(chanOpts, lnwallet.WithLeafStore(s))
	})
	a.c.cfg.AuxSigner.WhenSome(func(s lnwallet.AuxSigner) {
		chanOpts = append(chanOpts, lnwallet.WithAuxSigner(s))
	})
	a.c.cfg.AuxResolver.WhenSome(func(s lnwallet.AuxContractResolver) {
		chanOpts = append(chanOpts, lnwallet.WithAuxResolver(s))
	})

	// Finally, we'll force close the channel completing
	// the force close workflow.
	chanMachine, err := lnwallet.NewLightningChannel(
		a.c.cfg.Signer, channel, nil, chanOpts...,
	)
	if err != nil {
		return nil, err
	}

	closeSummary, err := chanMachine.ForceClose(
		lnwallet.WithSkipContractResolutions(),
	)
	if err != nil {
		return nil, err
	}

	return closeSummary.CloseTx, nil
}

// newActiveChannelArbitrator creates a new instance of an active channel
// arbitrator given the state of the target channel.
func newActiveChannelArbitrator(channel *channeldb.OpenChannel,
	c *ChainArbitrator, chanEvents *ChainEventSubscription) (*ChannelArbitrator, error) {

	// TODO(roasbeef): fetch best height (or pass in) so can ensure block
	// epoch delivers all the notifications to

	chanPoint := channel.FundingOutpoint

	log.Tracef("Creating ChannelArbitrator for ChannelPoint(%v)", chanPoint)

	// Next we'll create the matching configuration struct that contains
	// all interfaces and methods the arbitrator needs to do its job.
	arbCfg := ChannelArbitratorConfig{
		ChanPoint:   chanPoint,
		Channel:     c.getArbChannel(channel),
		ShortChanID: channel.ShortChanID(),

		MarkCommitmentBroadcasted: channel.MarkCommitmentBroadcasted,
		MarkChannelClosed: func(summary *channeldb.ChannelCloseSummary,
			statuses ...channeldb.ChannelStatus) error {

			err := channel.CloseChannel(summary, statuses...)
			if err != nil {
				return err
			}
			c.cfg.NotifyClosedChannel(summary.ChanPoint)
			return nil
		},
		IsPendingClose:        false,
		ChainArbitratorConfig: c.cfg,
		ChainEvents:           chanEvents,
		PutResolverReport: func(tx kvdb.RwTx,
			report *channeldb.ResolverReport) error {

			return c.chanSource.PutResolverReport(
				tx, c.cfg.ChainHash, &chanPoint, report,
			)
		},
		FetchHistoricalChannel: func() (*channeldb.OpenChannel, error) {
			chanStateDB := c.chanSource.ChannelStateDB()
			return chanStateDB.FetchHistoricalChannel(&chanPoint)
		},
		FindOutgoingHTLCDeadline: func(
			htlc channeldb.HTLC) fn.Option[int32] {

			return c.FindOutgoingHTLCDeadline(
				channel.ShortChanID(), htlc,
			)
		},
		NotifyChannelResolved: func() {
			c.notifyChannelResolved(chanPoint)
		},
	}

	// The final component needed is an arbitrator log that the arbitrator
	// will use to keep track of its internal state using a backed
	// persistent log.
	//
	// TODO(roasbeef); abstraction leak...
	//  * rework: adaptor method to set log scope w/ factory func
	chanLog, err := newBoltArbitratorLog(
		c.chanSource.Backend, arbCfg, c.cfg.ChainHash, chanPoint,
	)
	if err != nil {
		return nil, err
	}

	// Finally, we'll need to construct a series of htlc Sets based on all
	// currently known valid commitments.
	htlcSets := make(map[HtlcSetKey]htlcSet)
	htlcSets[LocalHtlcSet] = newHtlcSet(channel.LocalCommitment.Htlcs)
	htlcSets[RemoteHtlcSet] = newHtlcSet(channel.RemoteCommitment.Htlcs)

	pendingRemoteCommitment, err := channel.RemoteCommitChainTip()
	if err != nil && err != channeldb.ErrNoPendingCommit {
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

// getArbChannel returns an open channel wrapper for use by channel arbitrators.
func (c *ChainArbitrator) getArbChannel(
	channel *channeldb.OpenChannel) *arbChannel {

	return &arbChannel{
		channel: channel,
		c:       c,
	}
}

// ResolveContract marks a contract as fully resolved within the database.
// This is only to be done once all contracts which were live on the channel
// before hitting the chain have been resolved.
func (c *ChainArbitrator) ResolveContract(chanPoint wire.OutPoint) error {
	log.Infof("Marking ChannelPoint(%v) fully resolved", chanPoint)

	// First, we'll we'll mark the channel as fully closed from the PoV of
	// the channel source.
	err := c.chanSource.ChannelStateDB().MarkChanFullyClosed(&chanPoint)
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
func (c *ChainArbitrator) Start(beat chainio.Blockbeat) error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	// Set the current beat.
	c.beat = beat

	// Start the goroutine which listens for signals to mark the channel as
	// resolved.
	//
	// NOTE: We must start this goroutine here we won't block the following
	// channel loading.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.resolveContracts()
	}()

	// First, we'll fetch all the channels that are still open, in order to
	// collect them within our set of active contracts.
	if err := c.loadOpenChannels(); err != nil {
		return err
	}

	// In addition to the channels that we know to be open, we'll also
	// launch arbitrators to finishing resolving any channels that are in
	// the pending close state.
	if err := c.loadPendingCloseChannels(); err != nil {
		return err
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

	// stopAndLog is a helper function which shuts down the chain arb and
	// logs errors if they occur.
	stopAndLog := func() {
		if err := c.Stop(); err != nil {
			log.Errorf("ChainArbitrator could not shutdown: %v", err)
		}
	}

	// Handle all errors returned from spawning our chain watchers. If any
	// of them failed, we will stop the chain arb to shutdown any active
	// goroutines.
	for err := range watcherErrs {
		if err != nil {
			stopAndLog()
			return err
		}
	}

	// Before we start all of our arbitrators, we do a preliminary state
	// lookup so that we can combine all of these lookups in a single db
	// transaction.
	var startStates map[wire.OutPoint]*chanArbStartState

	err := kvdb.View(c.chanSource, func(tx walletdb.ReadTx) error {
		for _, arbitrator := range c.activeChannels {
			startState, err := arbitrator.getStartState(tx)
			if err != nil {
				return err
			}

			startStates[arbitrator.cfg.ChanPoint] = startState
		}

		return nil
	}, func() {
		startStates = make(
			map[wire.OutPoint]*chanArbStartState,
			len(c.activeChannels),
		)
	})
	if err != nil {
		stopAndLog()
		return err
	}

	// Launch all the goroutines for each arbitrator so they can carry out
	// their duties.
	for _, arbitrator := range c.activeChannels {
		startState, ok := startStates[arbitrator.cfg.ChanPoint]
		if !ok {
			stopAndLog()
			return fmt.Errorf("arbitrator: %v has no start state",
				arbitrator.cfg.ChanPoint)
		}

		if err := arbitrator.Start(startState, c.beat); err != nil {
			stopAndLog()
			return err
		}
	}

	// Start our goroutine which will dispatch blocks to each arbitrator.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.dispatchBlocks()
	}()

	log.Infof("ChainArbitrator starting at height %d with %d chain "+
		"watchers, %d channel arbitrators, and budget config=[%v]",
		c.beat.Height(), len(c.activeWatchers), len(c.activeChannels),
		&c.cfg.Budget)

	// TODO(roasbeef): eventually move all breach watching here

	return nil
}

// resolveContracts listens to the `resolvedChan` to mark a given channel as
// fully resolved.
func (c *ChainArbitrator) resolveContracts() {
	for {
		select {
		// The channel arbitrator signals that a given channel has been
		// resolved, we now update chain arbitrator's internal state for
		// this channel.
		case cp := <-c.resolvedChan:
			if c.cfg.NotifyFullyResolvedChannel != nil {
				c.cfg.NotifyFullyResolvedChannel(cp)
			}

			err := c.ResolveContract(cp)
			if err != nil {
				log.Errorf("Failed to resolve contract for "+
					"channel %v", cp)
			}

		// Exit if the chain arbitrator is shutting down.
		case <-c.quit:
			return
		}
	}
}

// dispatchBlocks consumes a block epoch notification stream and dispatches
// blocks to each of the chain arb's active channel arbitrators. This function
// must be run in a goroutine.
func (c *ChainArbitrator) dispatchBlocks() {
	// Consume block epochs until we receive the instruction to shutdown.
	for {
		select {
		// Consume block epochs, exiting if our subscription is
		// terminated.
		case beat := <-c.BlockbeatChan:
			// Set the current blockbeat.
			c.beat = beat

			// Send this blockbeat to all the active channels and
			// wait for them to finish processing it.
			c.handleBlockbeat(beat)

		// Exit if the chain arbitrator is shutting down.
		case <-c.quit:
			return
		}
	}
}

// handleBlockbeat sends the blockbeat to all active channel arbitrator in
// parallel and wait for them to finish processing it.
func (c *ChainArbitrator) handleBlockbeat(beat chainio.Blockbeat) {
	// Read the active channels in a lock.
	c.Lock()

	// Create a slice to record active channel arbitrator.
	channels := make([]chainio.Consumer, 0, len(c.activeChannels))
	watchers := make([]chainio.Consumer, 0, len(c.activeWatchers))

	// Copy the active channels to the slice.
	for _, channel := range c.activeChannels {
		channels = append(channels, channel)
	}

	for _, watcher := range c.activeWatchers {
		watchers = append(watchers, watcher)
	}

	c.Unlock()

	// Iterate all the copied watchers and send the blockbeat to them.
	err := chainio.DispatchConcurrent(beat, watchers)
	if err != nil {
		log.Errorf("Notify blockbeat for chainWatcher failed: %v", err)
	}

	// Iterate all the copied channels and send the blockbeat to them.
	//
	// NOTE: This method will timeout if the processing of blocks of the
	// subsystems is too long (60s).
	err = chainio.DispatchConcurrent(beat, channels)
	if err != nil {
		log.Errorf("Notify blockbeat for ChannelArbitrator failed: %v",
			err)
	}

	// Notify the chain arbitrator has processed the block.
	c.NotifyBlockProcessed(beat, err)
}

// notifyChannelResolved is used by the channel arbitrator to signal that a
// given channel has been resolved.
func (c *ChainArbitrator) notifyChannelResolved(cp wire.OutPoint) {
	select {
	case c.resolvedChan <- cp:
	case <-c.quit:
		return
	}
}

// republishClosingTxs will load any stored cooperative or unilateral closing
// transactions and republish them. This helps ensure propagation of the
// transactions in the event that prior publications failed.
func (c *ChainArbitrator) republishClosingTxs(
	channel *channeldb.OpenChannel) error {

	// If the channel has had its unilateral close broadcasted already,
	// republish it in case it didn't propagate.
	if channel.HasChanStatus(channeldb.ChanStatusCommitBroadcasted) {
		err := c.rebroadcast(
			channel, channeldb.ChanStatusCommitBroadcasted,
		)
		if err != nil {
			return err
		}
	}

	// If the channel has had its cooperative close broadcasted
	// already, republish it in case it didn't propagate.
	if channel.HasChanStatus(channeldb.ChanStatusCoopBroadcasted) {
		err := c.rebroadcast(
			channel, channeldb.ChanStatusCoopBroadcasted,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// rebroadcast is a helper method which will republish the unilateral or
// cooperative close transaction or a channel in a particular state.
//
// NOTE: There is no risk to calling this method if the channel isn't in either
// CommitmentBroadcasted or CoopBroadcasted, but the logs will be misleading.
func (c *ChainArbitrator) rebroadcast(channel *channeldb.OpenChannel,
	state channeldb.ChannelStatus) error {

	chanPoint := channel.FundingOutpoint

	var (
		closeTx *wire.MsgTx
		kind    string
		err     error
	)
	switch state {
	case channeldb.ChanStatusCommitBroadcasted:
		kind = "force"
		closeTx, err = channel.BroadcastedCommitment()

	case channeldb.ChanStatusCoopBroadcasted:
		kind = "coop"
		closeTx, err = channel.BroadcastedCooperative()

	default:
		return fmt.Errorf("unknown closing state: %v", state)
	}

	switch {
	// This can happen for channels that had their closing tx published
	// before we started storing it to disk.
	case err == channeldb.ErrNoCloseTx:
		log.Warnf("Channel %v is in state %v, but no %s closing tx "+
			"to re-publish...", chanPoint, state, kind)
		return nil

	case err != nil:
		return err
	}

	log.Infof("Re-publishing %s close tx(%v) for channel %v",
		kind, closeTx.TxHash(), chanPoint)

	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &channel.ShortChannelID,
	)
	err = c.cfg.PublishTx(closeTx, label)
	if err != nil && err != lnwallet.ErrDoubleSpend {
		log.Warnf("Unable to broadcast %s close tx(%v): %v",
			kind, closeTx.TxHash(), err)
	}

	return nil
}

// Stop signals the ChainArbitrator to trigger a graceful shutdown. Any active
// channel arbitrators will be signalled to exit, and this method will block
// until they've all exited.
func (c *ChainArbitrator) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	log.Info("ChainArbitrator shutting down...")
	defer log.Debug("ChainArbitrator shutdown complete")

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

// ContractSignals is used by outside subsystems to notify a channel arbitrator
// of its ShortChannelID.
type ContractSignals struct {
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

// NotifyContractUpdate lets a channel arbitrator know that a new
// ContractUpdate is available. This calls the ChannelArbitrator's internal
// method NotifyContractUpdate which waits for a response on a done chan before
// returning. This method will return an error if the ChannelArbitrator is not
// in the activeChannels map. However, this only happens if the arbitrator is
// resolved and the related link would already be shut down.
func (c *ChainArbitrator) NotifyContractUpdate(chanPoint wire.OutPoint,
	update *ContractUpdate) error {

	c.Lock()
	arbitrator, ok := c.activeChannels[chanPoint]
	c.Unlock()
	if !ok {
		return fmt.Errorf("can't find arbitrator for %v", chanPoint)
	}

	arbitrator.notifyContractUpdate(update)
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

	chanPoint := newChan.FundingOutpoint

	log.Infof("Creating new chainWatcher and ChannelArbitrator for "+
		"ChannelPoint(%v)", chanPoint)

	// If we're already watching this channel, then we'll ignore this
	// request.
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
			contractBreach: func(
				retInfo *lnwallet.BreachRetribution) error {

				return c.cfg.ContractBreach(
					chanPoint, retInfo,
				)
			},
			extractStateNumHint: lnwallet.GetStateNumHint,
			auxLeafStore:        c.cfg.AuxLeafStore,
			auxResolver:         c.cfg.AuxResolver,
		},
	)
	if err != nil {
		return err
	}

	c.activeWatchers[chanPoint] = chainWatcher

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

	if err := channelArb.Start(nil, c.beat); err != nil {
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
	c.Lock()
	watcher, ok := c.activeWatchers[chanPoint]
	c.Unlock()

	if !ok {
		return nil, fmt.Errorf("unable to find watcher for: %v",
			chanPoint)
	}

	// With the watcher located, we'll request for it to create a new chain
	// event subscription client.
	return watcher.SubscribeChannelEvents(), nil
}

// FindOutgoingHTLCDeadline returns the deadline in absolute block height for
// the specified outgoing HTLC. For an outgoing HTLC, its deadline is defined
// by the timeout height of its corresponding incoming HTLC - this is the
// expiry height the that remote peer can spend his/her outgoing HTLC via the
// timeout path.
func (c *ChainArbitrator) FindOutgoingHTLCDeadline(scid lnwire.ShortChannelID,
	outgoingHTLC channeldb.HTLC) fn.Option[int32] {

	// Find the outgoing HTLC's corresponding incoming HTLC in the circuit
	// map.
	rHash := outgoingHTLC.RHash
	circuit := models.CircuitKey{
		ChanID: scid,
		HtlcID: outgoingHTLC.HtlcIndex,
	}
	incomingCircuit := c.cfg.QueryIncomingCircuit(circuit)

	// If there's no incoming circuit found, we will use the default
	// deadline.
	if incomingCircuit == nil {
		log.Warnf("ChannelArbitrator(%v): incoming circuit key not "+
			"found for rHash=%x, using default deadline instead",
			scid, rHash)

		return fn.None[int32]()
	}

	// If this is a locally initiated HTLC, it means we are the first hop.
	// In this case, we can relax the deadline.
	if incomingCircuit.ChanID.IsDefault() {
		log.Infof("ChannelArbitrator(%v): using default deadline for "+
			"locally initiated HTLC for rHash=%x", scid, rHash)

		return fn.None[int32]()
	}

	log.Debugf("Found incoming circuit %v for rHash=%x using outgoing "+
		"circuit %v", incomingCircuit, rHash, circuit)

	c.Lock()
	defer c.Unlock()

	// Iterate over all active channels to find the incoming HTLC specified
	// by its circuit key.
	for cp, channelArb := range c.activeChannels {
		// Skip if the SCID doesn't match.
		if channelArb.cfg.ShortChanID != incomingCircuit.ChanID {
			continue
		}

		// Make sure the channel arbitrator has the latest view of its
		// active HTLCs.
		channelArb.updateActiveHTLCs()

		// Iterate all the known HTLCs to find the targeted incoming
		// HTLC.
		for _, htlcs := range channelArb.activeHTLCs {
			for _, htlc := range htlcs.incomingHTLCs {
				// Skip if the index doesn't match.
				if htlc.HtlcIndex != incomingCircuit.HtlcID {
					continue
				}

				log.Debugf("ChannelArbitrator(%v): found "+
					"incoming HTLC in channel=%v using "+
					"rHash=%x, refundTimeout=%v", scid,
					cp, rHash, htlc.RefundTimeout)

				return fn.Some(int32(htlc.RefundTimeout))
			}
		}
	}

	// If there's no incoming HTLC found, yet we have the incoming circuit,
	// something is wrong - in this case, we return the none deadline.
	log.Errorf("ChannelArbitrator(%v): incoming HTLC not found for "+
		"rHash=%x, using default deadline instead", scid, rHash)

	return fn.None[int32]()
}

// TODO(roasbeef): arbitration reports
//  * types: contested, waiting for success conf, etc

// NOTE: part of the `chainio.Consumer` interface.
func (c *ChainArbitrator) Name() string {
	return "ChainArbitrator"
}

// loadOpenChannels loads all channels that are currently open in the database
// and registers them with the chainWatcher for future notification.
func (c *ChainArbitrator) loadOpenChannels() error {
	openChannels, err := c.chanSource.ChannelStateDB().FetchAllChannels()
	if err != nil {
		return err
	}

	if len(openChannels) == 0 {
		return nil
	}

	log.Infof("Creating ChannelArbitrators for %v active channels",
		len(openChannels))

	// For each open channel, we'll configure then launch a corresponding
	// ChannelArbitrator.
	for _, channel := range openChannels {
		chanPoint := channel.FundingOutpoint
		channel := channel

		// First, we'll create an active chainWatcher for this channel
		// to ensure that we detect any relevant on chain events.
		breachClosure := func(ret *lnwallet.BreachRetribution) error {
			return c.cfg.ContractBreach(chanPoint, ret)
		}

		chainWatcher, err := newChainWatcher(
			chainWatcherConfig{
				chanState:           channel,
				notifier:            c.cfg.Notifier,
				signer:              c.cfg.Signer,
				isOurAddr:           c.cfg.IsOurAddress,
				contractBreach:      breachClosure,
				extractStateNumHint: lnwallet.GetStateNumHint,
				auxLeafStore:        c.cfg.AuxLeafStore,
				auxResolver:         c.cfg.AuxResolver,
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

		// Republish any closing transactions for this channel.
		err = c.republishClosingTxs(channel)
		if err != nil {
			log.Errorf("Failed to republish closing txs for "+
				"channel %v", chanPoint)
		}
	}

	return nil
}

// loadPendingCloseChannels loads all channels that are currently pending
// closure in the database and registers them with the ChannelArbitrator to
// continue the resolution process.
func (c *ChainArbitrator) loadPendingCloseChannels() error {
	chanStateDB := c.chanSource.ChannelStateDB()

	closingChannels, err := chanStateDB.FetchClosedChannels(true)
	if err != nil {
		return err
	}

	if len(closingChannels) == 0 {
		return nil
	}

	log.Infof("Creating ChannelArbitrators for %v closing channels",
		len(closingChannels))

	// Next, for each channel is the closing state, we'll launch a
	// corresponding more restricted resolver, as we don't have to watch
	// the chain any longer, only resolve the contracts on the confirmed
	// commitment.
	//nolint:ll
	for _, closeChanInfo := range closingChannels {
		// We can leave off the CloseContract and ForceCloseChan
		// methods as the channel is already closed at this point.
		chanPoint := closeChanInfo.ChanPoint
		arbCfg := ChannelArbitratorConfig{
			ChanPoint:             chanPoint,
			ShortChanID:           closeChanInfo.ShortChanID,
			ChainArbitratorConfig: c.cfg,
			ChainEvents:           &ChainEventSubscription{},
			IsPendingClose:        true,
			ClosingHeight:         closeChanInfo.CloseHeight,
			CloseType:             closeChanInfo.CloseType,
			PutResolverReport: func(tx kvdb.RwTx,
				report *channeldb.ResolverReport) error {

				return c.chanSource.PutResolverReport(
					tx, c.cfg.ChainHash, &chanPoint, report,
				)
			},
			FetchHistoricalChannel: func() (*channeldb.OpenChannel, error) {
				return chanStateDB.FetchHistoricalChannel(&chanPoint)
			},
			FindOutgoingHTLCDeadline: func(
				htlc channeldb.HTLC) fn.Option[int32] {

				return c.FindOutgoingHTLCDeadline(
					closeChanInfo.ShortChanID, htlc,
				)
			},
			NotifyChannelResolved: func() {
				c.notifyChannelResolved(chanPoint)
			},
		}
		chanLog, err := newBoltArbitratorLog(
			c.chanSource.Backend, arbCfg, c.cfg.ChainHash, chanPoint,
		)
		if err != nil {
			return err
		}

		// We create an empty map of HTLC's here since it's possible
		// that the channel is in StateDefault and updateActiveHTLCs is
		// called. We want to avoid writing to an empty map. Since the
		// channel is already in the process of being resolved, no new
		// HTLCs will be added.
		c.activeChannels[chanPoint] = NewChannelArbitrator(
			arbCfg, make(map[HtlcSetKey]htlcSet), chanLog,
		)
	}

	return nil
}

// RedispatchBlockbeat resends the current blockbeat to the channels specified
// by the chanPoints. It is used when a channel is added to the chain
// arbitrator after it has been started, e.g., during the channel restore
// process.
func (c *ChainArbitrator) RedispatchBlockbeat(chanPoints []wire.OutPoint) {
	// Get the current blockbeat.
	beat := c.beat

	// Prepare two sets of consumers.
	channels := make([]chainio.Consumer, 0, len(chanPoints))
	watchers := make([]chainio.Consumer, 0, len(chanPoints))

	// Read the active channels in a lock.
	c.Lock()
	for _, op := range chanPoints {
		if channel, ok := c.activeChannels[op]; ok {
			channels = append(channels, channel)
		}

		if watcher, ok := c.activeWatchers[op]; ok {
			watchers = append(watchers, watcher)
		}
	}
	c.Unlock()

	// Iterate all the copied watchers and send the blockbeat to them.
	err := chainio.DispatchConcurrent(beat, watchers)
	if err != nil {
		log.Errorf("Notify blockbeat for chainWatcher failed: %v", err)
	}

	// Iterate all the copied channels and send the blockbeat to them.
	err = chainio.DispatchConcurrent(beat, channels)
	if err != nil {
		// Shutdown lnd if there's an error processing the block.
		log.Errorf("Notify blockbeat for ChannelArbitrator failed: %v",
			err)
	}
}
