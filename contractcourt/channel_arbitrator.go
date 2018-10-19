package contractcourt

import (
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// broadcastRedeemMultiplier is the additional factor that we'll scale
	// the normal broadcastDelta by when deciding whether or not to
	// broadcast a commitment to claim an HTLC on-chain. We use a scaled
	// value, as when redeeming we want to ensure that we have enough time
	// to redeem the HTLC, well before it times out.
	broadcastRedeemMultiplier = 2
)

// WitnessSubscription represents an intent to be notified once new witnesses
// are discovered by various active contract resolvers. A contract resolver may
// use this to be notified of when it can satisfy an incoming contract after we
// discover the witness for an outgoing contract.
type WitnessSubscription struct {
	// WitnessUpdates is a channel that newly discovered witnesses will be
	// sent over.
	//
	// TODO(roasbeef): couple with WitnessType?
	WitnessUpdates <-chan []byte

	// CancelSubscription is a function closure that should be used by a
	// client to cancel the subscription once they are no longer interested
	// in receiving new updates.
	CancelSubscription func()
}

// WitnessBeacon is a global beacon of witnesses. Contract resolvers will use
// this interface to lookup witnesses (preimages typically) of contracts
// they're trying to resolve, add new preimages they resolve, and finally
// receive new updates each new time a preimage is discovered.
//
// TODO(roasbeef): need to delete the pre-images once we've used them
// and have been sufficiently confirmed?
type WitnessBeacon interface {
	// SubscribeUpdates returns a channel that will be sent upon *each* time
	// a new preimage is discovered.
	SubscribeUpdates() *WitnessSubscription

	// LookupPreImage attempts to lookup a preimage in the global cache.
	// True is returned for the second argument if the preimage is found.
	LookupPreimage(payhash []byte) ([]byte, bool)

	// AddPreImage adds a newly discovered preimage to the global cache.
	AddPreimage(pre []byte) error
}

// ChannelArbitratorConfig contains all the functionality that the
// ChannelArbitrator needs in order to properly arbitrate any contract dispute
// on chain.
type ChannelArbitratorConfig struct {
	// ChanPoint is the channel point that uniquely identifies this
	// channel.
	ChanPoint wire.OutPoint

	// ShortChanID describes the exact location of the channel within the
	// chain. We'll use this to address any messages that we need to send
	// to the switch during contract resolution.
	ShortChanID lnwire.ShortChannelID

	// BlockEpochs is an active block epoch event stream backed by an
	// active ChainNotifier instance. We will use new block notifications
	// sent over this channel to decide when we should go on chain to
	// reclaim/redeem the funds in an HTLC sent to/from us.
	BlockEpochs *chainntnfs.BlockEpochEvent

	// ChainEvents is an active subscription to the chain watcher for this
	// channel to be notified of any on-chain activity related to this
	// channel.
	ChainEvents *ChainEventSubscription

	// ForceCloseChan should force close the contract that this attendant
	// is watching over. We'll use this when we decide that we need to go
	// to chain. It should in addition tell the switch to remove the
	// corresponding link, such that we won't accept any new updates. The
	// returned summary contains all items needed to eventually resolve all
	// outputs on chain.
	ForceCloseChan func() (*lnwallet.LocalForceCloseSummary, error)

	// MarkCommitmentBroadcasted should mark the channel as the commitment
	// being broadcast, and we are waiting for the commitment to confirm.
	MarkCommitmentBroadcasted func() error

	// MarkChannelClosed marks the channel closed in the database, with the
	// passed close summary. After this method successfully returns we can
	// no longer expect to receive chain events for this channel, and must
	// be able to recover from a failure without getting the close event
	// again.
	MarkChannelClosed func(*channeldb.ChannelCloseSummary) error

	// IsPendingClose is a boolean indicating whether the channel is marked
	// as pending close in the database.
	IsPendingClose bool

	// ClosingHeight is the height at which the channel was closed. Note
	// that this value is only valid if IsPendingClose is true.
	ClosingHeight uint32

	// CloseType is the type of the close event in case IsPendingClose is
	// true. Otherwise this value is unset.
	CloseType channeldb.ClosureType

	// MarkChannelResolved is a function closure that serves to mark a
	// channel as "fully resolved". A channel itself can be considered
	// fully resolved once all active contracts have individually been
	// fully resolved.
	//
	// TODO(roasbeef): need RPC's to combine for pendingchannels RPC
	MarkChannelResolved func() error

	ChainArbitratorConfig
}

// htlcSet represents the set of active HTLCs on a given commitment
// transaction.
type htlcSet struct {
	// incomingHTLCs is a map of all incoming HTLCs on our commitment
	// transaction. We may potentially go onchain to claim the funds sent
	// to us within this set.
	incomingHTLCs map[uint64]channeldb.HTLC

	// outgoingHTLCs is a map of all outgoing HTLCs on our commitment
	// transaction. We may potentially go onchain to reclaim the funds that
	// are currently in limbo.
	outgoingHTLCs map[uint64]channeldb.HTLC
}

// newHtlcSet constructs a new HTLC set from a slice of HTLC's.
func newHtlcSet(htlcs []channeldb.HTLC) htlcSet {
	outHTLCs := make(map[uint64]channeldb.HTLC)
	inHTLCs := make(map[uint64]channeldb.HTLC)
	for _, htlc := range htlcs {
		if htlc.Incoming {
			inHTLCs[htlc.HtlcIndex] = htlc
			continue
		}

		outHTLCs[htlc.HtlcIndex] = htlc
	}

	return htlcSet{
		incomingHTLCs: inHTLCs,
		outgoingHTLCs: outHTLCs,
	}
}

// ChannelArbitrator is the on-chain arbitrator for a particular channel. The
// struct will keep in sync with the current set of HTLCs on the commitment
// transaction. The job of the attendant is to go on-chain to either settle or
// cancel an HTLC as necessary iff: an HTLC times out, or we known the
// pre-image to an HTLC, but it wasn't settled by the link off-chain. The
// ChannelArbitrator will factor in an expected confirmation delta when
// broadcasting to ensure that we avoid any possibility of race conditions, and
// sweep the output(s) without contest.
type ChannelArbitrator struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// log is a persistent log that the attendant will use to checkpoint
	// its next action, and the state of any unresolved contracts.
	log ArbitratorLog

	// activeHTLCs is the set of active incoming/outgoing HTLC's on the
	// commitment transaction.
	activeHTLCs htlcSet

	// cfg contains all the functionality that the ChannelArbitrator requires
	// to do its duty.
	cfg ChannelArbitratorConfig

	// signalUpdates is a channel that any new live signals for the channel
	// we're watching over will be sent.
	signalUpdates chan *signalUpdateMsg

	// htlcUpdates is a channel that is sent upon with new updates from the
	// active channel. Each time a new commitment state is accepted, the
	// set of HTLC's on the new state should be sent across this channel.
	htlcUpdates <-chan []channeldb.HTLC

	// activeResolvers is a slice of any active resolvers. This is used to
	// be able to signal them for shutdown in the case that we shutdown.
	activeResolvers []ContractResolver

	// resolutionSignal is a channel that will be sent upon by contract
	// resolvers once their contract has been fully resolved. With each
	// send, we'll check to see if the contract is fully resolved.
	resolutionSignal chan struct{}

	// forceCloseReqs is a channel that requests to forcibly close the
	// contract will be sent over.
	forceCloseReqs chan *forceCloseReq

	// state is the current state of the arbitrator. This state is examined
	// upon start up to decide which actions to take.
	state ArbitratorState

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewChannelArbitrator returns a new instance of a ChannelArbitrator backed by
// the passed config struct.
func NewChannelArbitrator(cfg ChannelArbitratorConfig,
	startingHTLCs []channeldb.HTLC, log ArbitratorLog) *ChannelArbitrator {

	return &ChannelArbitrator{
		log:              log,
		signalUpdates:    make(chan *signalUpdateMsg),
		htlcUpdates:      make(<-chan []channeldb.HTLC),
		resolutionSignal: make(chan struct{}),
		forceCloseReqs:   make(chan *forceCloseReq),
		activeHTLCs:      newHtlcSet(startingHTLCs),
		cfg:              cfg,
		quit:             make(chan struct{}),
	}
}

// Start starts all the goroutines that the ChannelArbitrator needs to operate.
func (c *ChannelArbitrator) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	var (
		err                 error
		unresolvedContracts []ContractResolver
	)

	log.Debugf("Starting ChannelArbitrator(%v), htlc_set=%v",
		c.cfg.ChanPoint, newLogClosure(func() string {
			return spew.Sdump(c.activeHTLCs)
		}),
	)

	// First, we'll read our last state from disk, so our internal state
	// machine can act accordingly.
	c.state, err = c.log.CurrentState()
	if err != nil {
		c.cfg.BlockEpochs.Cancel()
		return err
	}

	log.Infof("ChannelArbitrator(%v): starting state=%v", c.cfg.ChanPoint,
		c.state)

	_, bestHeight, err := c.cfg.ChainIO.GetBestBlock()
	if err != nil {
		c.cfg.BlockEpochs.Cancel()
		return err
	}

	// If the channel has been marked pending close in the database, and we
	// haven't transitioned the state machine to StateContractClosed (or a
	// succeeding state), then a state transition most likely failed. We'll
	// try to recover from this by manually advancing the state by setting
	// the corresponding close trigger.
	trigger := chainTrigger
	triggerHeight := uint32(bestHeight)
	if c.cfg.IsPendingClose {
		switch c.state {
		case StateDefault:
			fallthrough
		case StateBroadcastCommit:
			fallthrough
		case StateCommitmentBroadcasted:
			switch c.cfg.CloseType {

			case channeldb.CooperativeClose:
				trigger = coopCloseTrigger

			case channeldb.LocalForceClose:
				trigger = localCloseTrigger

			case channeldb.RemoteForceClose:
				trigger = remoteCloseTrigger
			}
			triggerHeight = c.cfg.ClosingHeight

			log.Warnf("ChannelArbitrator(%v): detected stalled "+
				"state=%v for closed channel, using "+
				"trigger=%v", c.cfg.ChanPoint, c.state, trigger)
		}
	}

	// We'll now attempt to advance our state forward based on the current
	// on-chain state, and our set of active contracts.
	startingState := c.state
	nextState, _, err := c.advanceState(triggerHeight, trigger)
	if err != nil {
		switch err {

		// If we detect that we tried to fetch resolutions, but failed,
		// this channel was marked closed in the database before
		// resolutions successfully written. In this case there is not
		// much we can do, so we don't return the error.
		case errScopeBucketNoExist:
			fallthrough
		case errNoResolutions:
			log.Warnf("ChannelArbitrator(%v): detected closed"+
				"channel with no contract resolutions written.",
				c.cfg.ChanPoint)

		default:
			c.cfg.BlockEpochs.Cancel()
			return err
		}
	}

	// If we start and ended at the awaiting full resolution state, then
	// we'll relaunch our set of unresolved contracts.
	if startingState == StateWaitingFullResolution &&
		nextState == StateWaitingFullResolution {

		// We'll now query our log to see if there are any active
		// unresolved contracts. If this is the case, then we'll
		// relaunch all contract resolvers.
		unresolvedContracts, err = c.log.FetchUnresolvedContracts()
		if err != nil {
			c.cfg.BlockEpochs.Cancel()
			return err
		}

		log.Infof("ChannelArbitrator(%v): relaunching %v contract "+
			"resolvers", c.cfg.ChanPoint, len(unresolvedContracts))

		c.activeResolvers = unresolvedContracts
		for _, contract := range unresolvedContracts {
			c.wg.Add(1)
			go c.resolveContract(contract)
		}
	}

	// TODO(roasbeef): cancel if breached

	c.wg.Add(1)
	go c.channelAttendant(bestHeight)
	return nil
}

// Stop signals the ChannelArbitrator for a graceful shutdown.
func (c *ChannelArbitrator) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	log.Debugf("Stopping ChannelArbitrator(%v)", c.cfg.ChanPoint)

	if c.cfg.ChainEvents.Cancel != nil {
		go c.cfg.ChainEvents.Cancel()
	}

	for _, activeResolver := range c.activeResolvers {
		activeResolver.Stop()
	}

	close(c.quit)
	c.wg.Wait()

	return nil
}

// transitionTrigger is an enum that denotes exactly *why* a state transition
// was initiated. This is useful as depending on the initial trigger, we may
// skip certain states as those actions are expected to have already taken
// place as a result of the external trigger.
type transitionTrigger uint8

const (
	// chainTrigger is a transition trigger that has been attempted due to
	// changing on-chain conditions such as a block which times out HTLC's
	// being attached.
	chainTrigger transitionTrigger = iota

	// userTrigger is a transition trigger driven by user action. Examples
	// of such a trigger include a user requesting a force closure of the
	// channel.
	userTrigger

	// remoteCloseTrigger is a transition trigger driven by the remote
	// peer's commitment being confirmed.
	remoteCloseTrigger

	// localCloseTrigger is a transition trigger driven by our commitment
	// being confirmed.
	localCloseTrigger

	// coopCloseTrigger is a transition trigger driven by a cooperative
	// close transaction being confirmed.
	coopCloseTrigger
)

// String returns a human readable string describing the passed
// transitionTrigger.
func (t transitionTrigger) String() string {
	switch t {
	case chainTrigger:
		return "chainTrigger"

	case remoteCloseTrigger:
		return "remoteCloseTrigger"

	case userTrigger:
		return "userTrigger"

	case localCloseTrigger:
		return "localCloseTrigger"

	case coopCloseTrigger:
		return "coopCloseTrigger"

	default:
		return "unknown trigger"
	}
}

// stateStep is a help method that examines our internal state, and attempts
// the appropriate state transition if necessary. The next state we transition
// to is returned, Additionally, if the next transition results in a commitment
// broadcast, the commitment transaction itself is returned.
func (c *ChannelArbitrator) stateStep(triggerHeight uint32,
	trigger transitionTrigger) (ArbitratorState, *wire.MsgTx, error) {

	var (
		nextState ArbitratorState
		closeTx   *wire.MsgTx
	)
	switch c.state {

	// If we're in the default state, then we'll check our set of actions
	// to see if while we were down, conditions have changed.
	case StateDefault:
		log.Debugf("ChannelArbitrator(%v): new block (height=%v) "+
			"examining active HTLC's", c.cfg.ChanPoint,
			triggerHeight)

		// As a new block has been connected to the end of the main
		// chain, we'll check to see if we need to make any on-chain
		// claims on behalf of the channel contract that we're
		// arbitrating for.
		chainActions := c.checkChainActions(triggerHeight, trigger)

		// If there are no actions to be made, then we'll remain in the
		// default state. If this isn't a self initiated event (we're
		// checking due to a chain update), then we'll exit now.
		if len(chainActions) == 0 && trigger == chainTrigger {
			log.Tracef("ChannelArbitrator(%v): no actions for "+
				"chain trigger, terminating", c.cfg.ChanPoint)

			return StateDefault, closeTx, nil
		}

		// Otherwise, we'll log that we checked the HTLC actions as the
		// commitment transaction has already been broadcast.
		log.Tracef("ChannelArbitrator(%v): logging chain_actions=%v",
			c.cfg.ChanPoint,
			newLogClosure(func() string {
				return spew.Sdump(chainActions)
			}))
		if err := c.log.LogChainActions(chainActions); err != nil {
			return StateError, closeTx, err
		}

		// Depending on the type of trigger, we'll either "tunnel"
		// through to a farther state, or just proceed linearly to the
		// next state.
		switch trigger {

		// If this is a chain trigger, then we'll go straight to the
		// next state, as we still need to broadcast the commitment
		// transaction.
		case chainTrigger:
			fallthrough
		case userTrigger:
			nextState = StateBroadcastCommit

		// If the trigger is a cooperative close being confirmed, then
		// we can go straight to StateFullyResolved, as there won't be
		// any contracts to resolve.
		case coopCloseTrigger:
			nextState = StateFullyResolved

		// Otherwise, if this state advance was triggered by a
		// commitment being confirmed on chain, then we'll jump
		// straight to the state where the contract has already been
		// closed, and we will inspect the set of unresolved contracts.
		case localCloseTrigger:
			log.Errorf("ChannelArbitrator(%v): unexpected local "+
				"commitment confirmed while in StateDefault",
				c.cfg.ChanPoint)
			fallthrough
		case remoteCloseTrigger:
			nextState = StateContractClosed
		}

	// If we're in this state, then we've decided to broadcast the
	// commitment transaction. We enter this state either due to an outside
	// sub-system, or because an on-chain action has been triggered.
	case StateBroadcastCommit:
		// Under normal operation, we can only enter
		// StateBroadcastCommit via a user or chain trigger. On restart,
		// this state may be reexecuted after closing the channel, but
		// failing to commit to StateContractClosed or
		// StateFullyResolved. In that case, one of the three close
		// triggers will be presented, signifying that we should skip
		// rebroadcasting, and go straight to resolving the on-chain
		// contract or marking the channel resolved.
		switch trigger {
		case localCloseTrigger, remoteCloseTrigger:
			log.Infof("ChannelArbitrator(%v): detected %s "+
				"close after closing channel, fast-forwarding "+
				"to %s to resolve contract",
				c.cfg.ChanPoint, trigger, StateContractClosed)
			return StateContractClosed, closeTx, nil

		case coopCloseTrigger:
			log.Infof("ChannelArbitrator(%v): detected %s "+
				"close after closing channel, fast-forwarding "+
				"to %s to resolve contract",
				c.cfg.ChanPoint, trigger, StateFullyResolved)
			return StateFullyResolved, closeTx, nil
		}

		log.Infof("ChannelArbitrator(%v): force closing "+
			"chan", c.cfg.ChanPoint)

		// Now that we have all the actions decided for the set of
		// HTLC's, we'll broadcast the commitment transaction, and
		// signal the link to exit.

		// We'll tell the switch that it should remove the link for
		// this channel, in addition to fetching the force close
		// summary needed to close this channel on chain.
		closeSummary, err := c.cfg.ForceCloseChan()
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"force close: %v", c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}
		closeTx = closeSummary.CloseTx

		// With the close transaction in hand, broadcast the
		// transaction to the network, thereby entering the post
		// channel resolution state.
		log.Infof("Broadcasting force close transaction, "+
			"ChannelPoint(%v): %v", c.cfg.ChanPoint,
			newLogClosure(func() string {
				return spew.Sdump(closeTx)
			}))

		// At this point, we'll now broadcast the commitment
		// transaction itself.
		if err := c.cfg.PublishTx(closeTx); err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to broadcast "+
				"close tx: %v", c.cfg.ChanPoint, err)
			if err != lnwallet.ErrDoubleSpend {
				return StateError, closeTx, err
			}
		}

		if err := c.cfg.MarkCommitmentBroadcasted(); err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"mark commitment broadcasted: %v",
				c.cfg.ChanPoint, err)
		}

		// We go to the StateCommitmentBroadcasted state, where we'll
		// be waiting for the commitment to be confirmed.
		nextState = StateCommitmentBroadcasted

	// In this state we have broadcasted our own commitment, and will need
	// to wait for a commitment (not necessarily the one we broadcasted!)
	// to be confirmed.
	case StateCommitmentBroadcasted:
		switch trigger {
		// We are waiting for a commitment to be confirmed, so any
		// other trigger will be ignored.
		case chainTrigger, userTrigger:
			log.Infof("ChannelArbitrator(%v): noop trigger %v",
				c.cfg.ChanPoint, trigger)
			nextState = StateCommitmentBroadcasted

		// If this state advance was triggered by any of the
		// commitments being confirmed, then we'll jump to the state
		// where the contract has been closed.
		case localCloseTrigger, remoteCloseTrigger:
			log.Infof("ChannelArbitrator(%v): trigger %v, "+
				" going to StateContractClosed",
				c.cfg.ChanPoint, trigger)
			nextState = StateContractClosed

		case coopCloseTrigger:
			log.Infof("ChannelArbitrator(%v): trigger %v, "+
				" going to StateFullyResolved",
				c.cfg.ChanPoint, trigger)
			nextState = StateFullyResolved
		}

	// If we're in this state, then the contract has been fully closed to
	// outside sub-systems, so we'll process the prior set of on-chain
	// contract actions and launch a set of resolvers.
	case StateContractClosed:
		// First, we'll fetch our chain actions, and both sets of
		// resolutions so we can process them.
		chainActions, err := c.log.FetchChainActions()
		if err != nil {
			log.Errorf("unable to fetch chain actions: %v", err)
			return StateError, closeTx, err
		}
		contractResolutions, err := c.log.FetchContractResolutions()
		if err != nil {
			log.Errorf("unable to fetch contract resolutions: %v",
				err)
			return StateError, closeTx, err
		}

		// If the resolution is empty, then we're done here. We don't
		// need to launch any resolvers, and can go straight to our
		// final state.
		if contractResolutions.IsEmpty() {
			log.Infof("ChannelArbitrator(%v): contract "+
				"resolutions empty, marking channel as fully resolved!",
				c.cfg.ChanPoint)
			nextState = StateFullyResolved
			break
		}

		// If we've have broadcast the commitment transaction, we send
		// our commitment output for incubation, but only if it wasn't
		// trimmed.  We'll need to wait for a CSV timeout before we can
		// reclaim the funds.
		commitRes := contractResolutions.CommitResolution
		if commitRes != nil && commitRes.MaturityDelay > 0 {
			log.Infof("ChannelArbitrator(%v): sending commit "+
				"output for incubation", c.cfg.ChanPoint)

			err = c.cfg.IncubateOutputs(
				c.cfg.ChanPoint, commitRes,
				nil, nil, triggerHeight,
			)
			if err != nil {
				// TODO(roasbeef): check for AlreadyExists errors
				log.Errorf("unable to incubate commitment "+
					"output: %v", err)
				return StateError, closeTx, err
			}
		}

		// Now that we know we'll need to act, we'll process the htlc
		// actions, wen create the structures we need to resolve all
		// outstanding contracts.
		htlcResolvers, pktsToSend, err := c.prepContractResolutions(
			chainActions, contractResolutions, triggerHeight,
		)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"resolve contracts: %v", c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}

		log.Debugf("ChannelArbitrator(%v): sending resolution message=%v",
			c.cfg.ChanPoint,
			newLogClosure(func() string {
				return spew.Sdump(pktsToSend)
			}))

		// With the commitment broadcast, we'll then send over all
		// messages we can send immediately.
		err = c.cfg.DeliverResolutionMsg(pktsToSend...)
		if err != nil {
			// TODO(roasbeef): make sure packet sends are idempotent
			log.Errorf("unable to send pkts: %v", err)
			return StateError, closeTx, err
		}

		log.Debugf("ChannelArbitrator(%v): inserting %v contract "+
			"resolvers", c.cfg.ChanPoint, len(htlcResolvers))

		err = c.log.InsertUnresolvedContracts(htlcResolvers...)
		if err != nil {
			return StateError, closeTx, err
		}

		// Finally, we'll launch all the required contract resolvers.
		// Once they're all resolved, we're no longer needed.
		c.activeResolvers = htlcResolvers
		for _, contract := range htlcResolvers {
			c.wg.Add(1)
			go c.resolveContract(contract)
		}

		nextState = StateWaitingFullResolution

	// This is our terminal state. We'll keep returning this state until
	// all contracts are fully resolved.
	case StateWaitingFullResolution:
		log.Infof("ChannelArbitrator(%v): still awaiting contract "+
			"resolution", c.cfg.ChanPoint)

		nextState = StateWaitingFullResolution

	// If we start as fully resolved, then we'll end as fully resolved.
	case StateFullyResolved:
		// To ensure that the state of the contract in persistent
		// storage is properly reflected, we'll mark the contract as
		// fully resolved now.
		nextState = StateFullyResolved

		log.Infof("ChannelPoint(%v) has been fully resolved "+
			"on-chain at height=%v", c.cfg.ChanPoint, triggerHeight)

		if err := c.cfg.MarkChannelResolved(); err != nil {
			log.Errorf("unable to mark channel resolved: %v", err)
			return StateError, closeTx, err
		}
	}

	log.Tracef("ChannelArbitrator(%v): next_state=%v", c.cfg.ChanPoint,
		nextState)

	return nextState, closeTx, nil
}

// advanceState is the main driver of our state machine. This method is an
// iterative function which repeatedly attempts to advance the internal state
// of the channel arbitrator. The state will be advanced until we reach a
// redundant transition, meaning that the state transition is a noop. The final
// param is a callback that allows the caller to execute an arbitrary action
// after each state transition.
func (c *ChannelArbitrator) advanceState(triggerHeight uint32,
	trigger transitionTrigger) (ArbitratorState, *wire.MsgTx, error) {

	var (
		priorState   ArbitratorState
		forceCloseTx *wire.MsgTx
	)

	// We'll continue to advance our state forward until the state we
	// transition to is that same state that we started at.
	for {
		priorState = c.state
		log.Tracef("ChannelArbitrator(%v): attempting state step with "+
			"trigger=%v from state=%v", c.cfg.ChanPoint, trigger,
			priorState)

		nextState, closeTx, err := c.stateStep(
			triggerHeight, trigger,
		)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to advance "+
				"state: %v", c.cfg.ChanPoint, err)
			return priorState, nil, err
		}

		if forceCloseTx == nil && closeTx != nil {
			forceCloseTx = closeTx
		}

		// Our termination transition is a noop transition. If we get
		// our prior state back as the next state, then we'll
		// terminate.
		if nextState == priorState {
			log.Tracef("ChannelArbitrator(%v): terminating at "+
				"state=%v", c.cfg.ChanPoint, nextState)
			return nextState, forceCloseTx, nil
		}

		// As the prior state was successfully executed, we can now
		// commit the next state. This ensures that we will re-execute
		// the prior state if anything fails.
		if err := c.log.CommitState(nextState); err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to commit "+
				"next state(%v): %v", c.cfg.ChanPoint,
				nextState, err)
			return priorState, nil, err
		}
		c.state = nextState
	}
}

// ChainAction is an enum that encompasses all possible on-chain actions
// we'll take for a set of HTLC's.
type ChainAction uint8

const (
	// NoAction is the min chainAction type, indicating that no action
	// needs to be taken for a given HTLC.
	NoAction ChainAction = 0

	// HtlcTimeoutAction indicates that the HTLC will timeout soon. As a
	// result, we should get ready to sweep it on chain after the timeout.
	HtlcTimeoutAction = 1

	// HtlcClaimAction indicates that we should claim the HTLC on chain
	// before its timeout period.
	HtlcClaimAction = 2

	// HtlcFailNowAction indicates that we should fail an outgoing HTLC
	// immediately by cancelling it backwards as it has no corresponding
	// output in our commitment transaction.
	HtlcFailNowAction = 3

	// HtlcOutgoingWatchAction indicates that we can't yet timeout this
	// HTLC, but we had to go to chain on order to resolve an existing
	// HTLC.  In this case, we'll either: time it out once it expires, or
	// will learn the pre-image if the remote party claims the output. In
	// this case, well add the pre-image to our global store.
	HtlcOutgoingWatchAction = 4

	// HtlcIncomingWatchAction indicates that we don't yet have the
	// pre-image to claim incoming HTLC, but we had to go to chain in order
	// to resolve and existing HTLC. In this case, we'll either: let the
	// other party time it out, or eventually learn of the pre-image, in
	// which case we'll claim on chain.
	HtlcIncomingWatchAction = 5
)

// String returns a human readable string describing a chain action.
func (c ChainAction) String() string {
	switch c {
	case NoAction:
		return "NoAction"

	case HtlcTimeoutAction:
		return "HtlcTimeoutAction"

	case HtlcClaimAction:
		return "HtlcClaimAction"

	case HtlcFailNowAction:
		return "HtlcFailNowAction"

	case HtlcOutgoingWatchAction:
		return "HtlcOutgoingWatchAction"

	case HtlcIncomingWatchAction:
		return "HtlcIncomingWatchAction"

	default:
		return "<unknown action>"
	}
}

// ChainActionMap is a map of a chain action, to the set of HTLC's that need to
// be acted upon for a given action type. The channel
type ChainActionMap map[ChainAction][]channeldb.HTLC

// shouldGoOnChain takes into account the absolute timeout of the HTLC, if the
// confirmation delta that we need is close, and returns a bool indicating if
// we should go on chain to claim.  We do this rather than waiting up until the
// last minute as we want to ensure that when we *need* (HTLC is timed out) to
// sweep, the commitment is already confirmed.
func (c *ChannelArbitrator) shouldGoOnChain(htlcExpiry, broadcastDelta,
	currentHeight uint32) bool {

	// We'll calculate the broadcast cut off for this HTLC. This is the
	// height that (based on our current fee estimation) we should
	// broadcast in order to ensure the commitment transaction is confirmed
	// before the HTLC fully expires.
	broadcastCutOff := htlcExpiry - broadcastDelta

	log.Tracef("ChannelArbitrator(%v): examining outgoing contract: "+
		"expiry=%v, cutoff=%v, height=%v", c.cfg.ChanPoint, htlcExpiry,
		broadcastCutOff, currentHeight)

	// TODO(roasbeef): take into account default HTLC delta, don't need to
	// broadcast immediately
	//  * can then batch with SINGLE | ANYONECANPAY

	// We should on-chain for this HTLC, iff we're within out broadcast
	// cutoff window.
	return currentHeight >= broadcastCutOff
}

// checkChainActions is called for each new block connected to the end of the
// main chain. Given the new block height, this new method will examine all
// active HTLC's, and determine if we need to go on-chain to claim any of them.
// A map of action -> []htlc is returned, detailing what action (if any) should
// be performed for each HTLC. For timed out HTLC's, once the commitment has
// been sufficiently confirmed, the HTLC's should be canceled backwards. For
// redeemed HTLC's, we should send the pre-image back to the incoming link.
func (c *ChannelArbitrator) checkChainActions(height uint32,
	trigger transitionTrigger) ChainActionMap {

	// TODO(roasbeef): would need to lock channel? channel totem?
	//  * race condition if adding and we broadcast, etc
	//  * or would make each instance sync?

	log.Debugf("ChannelArbitrator(%v): checking chain actions at "+
		"height=%v", c.cfg.ChanPoint, height)

	actionMap := make(ChainActionMap)
	redeemCutoff := c.cfg.BroadcastDelta * broadcastRedeemMultiplier

	// First, we'll make an initial pass over the set of incoming and
	// outgoing HTLC's to decide if we need to go on chain at all.
	haveChainActions := false
	for _, htlc := range c.activeHTLCs.outgoingHTLCs {
		// If any of our HTLC's triggered an on-chain action, then we
		// can break early.
		if haveChainActions {
			break
		}

		// We'll need to go on-chain for an outgoing HTLC if it was
		// never resolved downstream, and it's "close" to timing out.
		haveChainActions = haveChainActions || c.shouldGoOnChain(
			htlc.RefundTimeout, c.cfg.BroadcastDelta, height,
		)
	}
	for _, htlc := range c.activeHTLCs.incomingHTLCs {
		// If any of our HTLC's triggered an on-chain action, then we
		// can break early.
		if haveChainActions {
			break
		}

		// We'll need to go on-chain to pull an incoming HTLC iff we
		// know the pre-image and it's close to timing out. We need to
		// ensure that we claim the funds that our rightfully ours
		// on-chain.
		if _, ok := c.cfg.PreimageDB.LookupPreimage(htlc.RHash[:]); !ok {
			continue
		}
		haveChainActions = haveChainActions || c.shouldGoOnChain(
			htlc.RefundTimeout, redeemCutoff, height,
		)
	}

	// If we don't have any actions to make, then we'll return an empty
	// action map. We only do this if this was a chain trigger though, as
	// if we're going to broadcast the commitment (or the remote party) did
	// we're *forced* to act on each HTLC.
	if !haveChainActions && trigger == chainTrigger {
		log.Tracef("ChannelArbitrator(%v): no actions to take at "+
			"height=%v", c.cfg.ChanPoint, height)
		return actionMap
	}

	// Now that we know we'll need to go on-chain, we'll examine all of our
	// active outgoing HTLC's to see if we either need to: sweep them after
	// a timeout (then cancel backwards), cancel them backwards
	// immediately, or watch them as they're still active contracts.
	for _, htlc := range c.activeHTLCs.outgoingHTLCs {
		switch {
		// If the HTLC is dust, then we can cancel it backwards
		// immediately as there's no matching contract to arbitrate
		// on-chain. We know the HTLC is dust, if the OutputIndex
		// negative.
		case htlc.OutputIndex < 0:
			log.Tracef("ChannelArbitrator(%v): immediately "+
				"failing dust htlc=%x", c.cfg.ChanPoint,
				htlc.RHash[:])

			actionMap[HtlcFailNowAction] = append(
				actionMap[HtlcFailNowAction], htlc,
			)

		// If we don't need to immediately act on this HTLC, then we'll
		// mark it still "live". After we broadcast, we'll monitor it
		// until the HTLC times out to see if we can also redeem it
		// on-chain.
		case !c.shouldGoOnChain(
			htlc.RefundTimeout, c.cfg.BroadcastDelta, height,
		):
			// TODO(roasbeef): also need to be able to query
			// circuit map to see if HTLC hasn't been fully
			// resolved
			//
			//  * can't fail incoming until if outgoing not yet
			//  failed

			log.Tracef("ChannelArbitrator(%v): watching chain to "+
				"decide action for outgoing htlc=%x",
				c.cfg.ChanPoint, htlc.RHash[:])

			actionMap[HtlcOutgoingWatchAction] = append(
				actionMap[HtlcOutgoingWatchAction], htlc,
			)

		// Otherwise, we'll update our actionMap to mark that we need
		// to sweep this HTLC on-chain
		default:
			log.Tracef("ChannelArbitrator(%v): going on-chain to "+
				"timeout htlc=%x", c.cfg.ChanPoint, htlc.RHash[:])

			actionMap[HtlcTimeoutAction] = append(
				actionMap[HtlcTimeoutAction], htlc,
			)
		}
	}

	// Similarly, for each incoming HTLC, now that we need to go on-chain,
	// we'll either: sweep it immediately if we know the pre-image, or
	// observe the output on-chain if we don't In this last, case we'll
	// either learn of it eventually from the outgoing HTLC, or the sender
	// will timeout the HTLC.
	for _, htlc := range c.activeHTLCs.incomingHTLCs {
		payHash := htlc.RHash

		// If we have the pre-image, then we should go on-chain to
		// redeem the HTLC immediately.
		if _, ok := c.cfg.PreimageDB.LookupPreimage(payHash[:]); ok {
			log.Tracef("ChannelArbitrator(%v): preimage for "+
				"htlc=%x is known!", c.cfg.ChanPoint, payHash[:])

			actionMap[HtlcClaimAction] = append(
				actionMap[HtlcClaimAction], htlc,
			)
			continue
		}

		log.Tracef("ChannelArbitrator(%v): watching chain to decide "+
			"action for incoming htlc=%x", c.cfg.ChanPoint,
			payHash[:])

		// Otherwise, we don't yet have the pre-image, but should watch
		// on-chain to see if either: the remote party times out the
		// HTLC, or we learn of the pre-image.
		actionMap[HtlcIncomingWatchAction] = append(
			actionMap[HtlcIncomingWatchAction], htlc,
		)
	}

	return actionMap
}

// prepContractResolutions is called either int he case that we decide we need
// to go to chain, or the remote party goes to chain. Given a set of actions we
// need to take for each HTLC, this method will return a set of contract
// resolvers that will resolve the contracts on-chain if needed, and also a set
// of packets to send to the htlcswitch in order to ensure all incoming HTLC's
// are properly resolved.
func (c *ChannelArbitrator) prepContractResolutions(htlcActions ChainActionMap,
	contractResolutions *ContractResolutions, height uint32,
) ([]ContractResolver, []ResolutionMsg, error) {

	// There may be a class of HTLC's which we can fail back immediately,
	// for those we'll prepare a slice of packets to add to our outbox. Any
	// packets we need to send, will be cancels.
	var (
		msgsToSend []ResolutionMsg
	)

	incomingResolutions := contractResolutions.HtlcResolutions.IncomingHTLCs
	outgoingResolutions := contractResolutions.HtlcResolutions.OutgoingHTLCs

	// We'll use these two maps to quickly look up an active HTLC with its
	// matching HTLC resolution.
	outResolutionMap := make(map[wire.OutPoint]lnwallet.OutgoingHtlcResolution)
	inResolutionMap := make(map[wire.OutPoint]lnwallet.IncomingHtlcResolution)
	for i := 0; i < len(incomingResolutions); i++ {
		inRes := incomingResolutions[i]

		// If we have a success transaction, then the htlc's outpoint
		// is the transaction's only input. Otherwise, it's the claim
		// point.
		var htlcPoint wire.OutPoint
		if inRes.SignedSuccessTx != nil {
			htlcPoint = inRes.SignedSuccessTx.TxIn[0].PreviousOutPoint
		} else {
			htlcPoint = inRes.ClaimOutpoint
		}

		inResolutionMap[htlcPoint] = inRes
	}
	for i := 0; i < len(outgoingResolutions); i++ {
		outRes := outgoingResolutions[i]

		// If we have a timeout transaction, then the htlc's outpoint
		// is the transaction's only input. Otherwise, it's the claim
		// point.
		var htlcPoint wire.OutPoint
		if outRes.SignedTimeoutTx != nil {
			htlcPoint = outRes.SignedTimeoutTx.TxIn[0].PreviousOutPoint
		} else {
			htlcPoint = outRes.ClaimOutpoint
		}

		outResolutionMap[htlcPoint] = outRes
	}

	// We'll create the resolver kit that we'll be cloning for each
	// resolver so they each can do their duty.
	resKit := ResolverKit{
		ChannelArbitratorConfig: c.cfg,
		Checkpoint: func(res ContractResolver) error {
			return c.log.InsertUnresolvedContracts(res)
		},
	}

	commitHash := contractResolutions.CommitHash
	failureMsg := &lnwire.FailPermanentChannelFailure{}

	// For each HTLC, we'll either act immediately, meaning we'll instantly
	// fail the HTLC, or we'll act only once the transaction has been
	// confirmed, in which case we'll need an HTLC resolver.
	var htlcResolvers []ContractResolver
	for htlcAction, htlcs := range htlcActions {
		switch htlcAction {

		// If we can fail an HTLC immediately (an outgoing HTLC with no
		// contract), then we'll assemble an HTLC fail packet to send.
		case HtlcFailNowAction:
			for _, htlc := range htlcs {
				failMsg := ResolutionMsg{
					SourceChan: c.cfg.ShortChanID,
					HtlcIndex:  htlc.HtlcIndex,
					Failure:    failureMsg,
				}

				msgsToSend = append(msgsToSend, failMsg)
			}

		// If we can claim this HTLC, we'll create an HTLC resolver to
		// claim the HTLC (second-level or directly), then add the pre
		case HtlcClaimAction:
			for _, htlc := range htlcs {
				htlcOp := wire.OutPoint{
					Hash:  commitHash,
					Index: uint32(htlc.OutputIndex),
				}

				resolution, ok := inResolutionMap[htlcOp]
				if !ok {
					// TODO(roasbeef): panic?
					log.Errorf("ChannelArbitrator(%v) unable to find "+
						"incoming resolution: %v",
						c.cfg.ChanPoint, htlcOp)
					continue
				}

				resKit.Quit = make(chan struct{})
				resolver := &htlcSuccessResolver{
					htlcResolution:  resolution,
					broadcastHeight: height,
					payHash:         htlc.RHash,
					ResolverKit:     resKit,
				}
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// If we can timeout the HTLC directly, then we'll create the
		// proper resolver to do so, who will then cancel the packet
		// backwards.
		case HtlcTimeoutAction:
			for _, htlc := range htlcs {
				htlcOp := wire.OutPoint{
					Hash:  commitHash,
					Index: uint32(htlc.OutputIndex),
				}

				resolution, ok := outResolutionMap[htlcOp]
				if !ok {
					log.Errorf("ChannelArbitrator(%v) unable to find "+
						"outgoing resolution: %v", c.cfg.ChanPoint, htlcOp)
					continue
				}

				resKit.Quit = make(chan struct{})
				resolver := &htlcTimeoutResolver{
					htlcResolution:  resolution,
					broadcastHeight: height,
					htlcIndex:       htlc.HtlcIndex,
					ResolverKit:     resKit,
				}
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// If this is an incoming HTLC, but we can't act yet, then
		// we'll create an incoming resolver to redeem the HTLC if we
		// learn of the pre-image, or let the remote party time out.
		case HtlcIncomingWatchAction:
			for _, htlc := range htlcs {
				htlcOp := wire.OutPoint{
					Hash:  commitHash,
					Index: uint32(htlc.OutputIndex),
				}

				// TODO(roasbeef): need to handle incoming dust...

				// TODO(roasbeef): can't be negative!!!
				resolution, ok := inResolutionMap[htlcOp]
				if !ok {
					log.Errorf("ChannelArbitrator(%v) unable to find "+
						"incoming resolution: %v",
						c.cfg.ChanPoint, htlcOp)
					continue
				}

				resKit.Quit = make(chan struct{})
				resolver := &htlcIncomingContestResolver{
					htlcExpiry: htlc.RefundTimeout,
					htlcSuccessResolver: htlcSuccessResolver{
						htlcResolution:  resolution,
						broadcastHeight: height,
						payHash:         htlc.RHash,
						ResolverKit:     resKit,
					},
				}
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// Finally, if this is an outgoing HTLC we've sent, then we'll
		// launch a resolver to watch for the pre-image (and settle
		// backwards), or just timeout.
		case HtlcOutgoingWatchAction:
			for _, htlc := range htlcs {
				htlcOp := wire.OutPoint{
					Hash:  commitHash,
					Index: uint32(htlc.OutputIndex),
				}

				resolution, ok := outResolutionMap[htlcOp]
				if !ok {
					log.Errorf("ChannelArbitrator(%v) unable to find "+
						"outgoing resolution: %v",
						c.cfg.ChanPoint, htlcOp)
					continue
				}

				resKit.Quit = make(chan struct{})
				resolver := &htlcOutgoingContestResolver{
					htlcTimeoutResolver{
						htlcResolution:  resolution,
						broadcastHeight: height,
						htlcIndex:       htlc.HtlcIndex,
						ResolverKit:     resKit,
					},
				}
				htlcResolvers = append(htlcResolvers, resolver)
			}
		}
	}

	// Finally, if this is was a unilateral closure, then we'll also create
	// a resolver to sweep our commitment output (but only if it wasn't
	// trimmed).
	if contractResolutions.CommitResolution != nil {
		resKit.Quit = make(chan struct{})
		resolver := &commitSweepResolver{
			commitResolution: *contractResolutions.CommitResolution,
			broadcastHeight:  height,
			chanPoint:        c.cfg.ChanPoint,
			ResolverKit:      resKit,
		}

		htlcResolvers = append(htlcResolvers, resolver)
	}

	return htlcResolvers, msgsToSend, nil
}

// resolveContract is a goroutine tasked with fully resolving an unresolved
// contract. Either the initial contract will be resolved after a single step,
// or the contract will itself create another contract to be resolved. In
// either case, one the contract has been fully resolved, we'll signal back to
// the main goroutine so it can properly keep track of the set of unresolved
// contracts.
//
// NOTE: This MUST be run as a goroutine.
func (c *ChannelArbitrator) resolveContract(currentContract ContractResolver) {
	defer c.wg.Done()

	log.Debugf("ChannelArbitrator(%v): attempting to resolve %T",
		c.cfg.ChanPoint, currentContract)

	// Until the contract is fully resolved, we'll continue to iteratively
	// resolve the contract one step at a time.
	for !currentContract.IsResolved() {
		log.Debugf("ChannelArbitrator(%v): contract %T not yet resolved",
			c.cfg.ChanPoint, currentContract)

		select {

		// If we've been signalled to quit, then we'll exit early.
		case <-c.quit:
			return

		default:
			// Otherwise, we'll attempt to resolve the current
			// contract.
			nextContract, err := currentContract.Resolve()
			if err != nil {
				log.Errorf("ChannelArbitrator(%v): unable to "+
					"progress resolver: %v",
					c.cfg.ChanPoint, err)
				return
			}

			switch {
			// If this contract produced another, then this means
			// the current contract was only able to be partially
			// resolved in this step. So we'll not a contract swap
			// within our logs: the new contract will take the
			// place of the old one.
			case nextContract != nil:
				log.Debugf("ChannelArbitrator(%v): swapping "+
					"out contract %T for %T ",
					c.cfg.ChanPoint, currentContract,
					nextContract)

				err := c.log.SwapContract(
					currentContract, nextContract,
				)
				if err != nil {
					log.Errorf("unable to add recurse "+
						"contract: %v", err)
				}

				// As this contract produced another, we'll
				// re-assign, so we can continue our resolution
				// loop.
				currentContract = nextContract

			// If this contract is actually fully resolved, then
			// we'll mark it as such within the database.
			case currentContract.IsResolved():
				log.Debugf("ChannelArbitrator(%v): marking "+
					"contract %T fully resolved",
					c.cfg.ChanPoint, currentContract)

				err := c.log.ResolveContract(currentContract)
				if err != nil {
					log.Errorf("unable to resolve contract: %v",
						err)
				}

				// Now that the contract has been resolved,
				// well signal to the main goroutine.
				select {
				case c.resolutionSignal <- struct{}{}:
				case <-c.quit:
					return
				}
			}

		}
	}
}

// signalUpdateMsg is a struct that carries fresh signals to the
// ChannelArbitrator. We need to receive a message like this each time the
// channel becomes active, as it's internal state may change.
type signalUpdateMsg struct {
	// newSignals is the set of new active signals to be sent to the
	// arbitrator.
	newSignals *ContractSignals

	// doneChan is a channel that will be closed on the arbitrator has
	// attached the new signals.
	doneChan chan struct{}
}

// UpdateContractSignals updates the set of signals the ChannelArbitrator needs
// to receive from a channel in real-time in order to keep in sync with the
// latest state of the contract.
func (c *ChannelArbitrator) UpdateContractSignals(newSignals *ContractSignals) {
	done := make(chan struct{})

	select {
	case c.signalUpdates <- &signalUpdateMsg{
		newSignals: newSignals,
		doneChan:   done,
	}:
	case <-c.quit:
	}

	select {
	case <-done:
	case <-c.quit:
	}
}

// channelAttendant is the primary goroutine that acts at the judicial
// arbitrator between our channel state, the remote channel peer, and the
// blockchain Our judge). This goroutine will ensure that we faithfully execute
// all clauses of our contract in the case that we need to go on-chain for a
// dispute. Currently, two such conditions warrant our intervention: when an
// outgoing HTLC is about to timeout, and when we know the pre-image for an
// incoming HTLC, but it hasn't yet been settled off-chain. In these cases,
// we'll: broadcast our commitment, cancel/settle any HTLC's backwards after
// sufficient confirmation, and finally send our set of outputs to the UTXO
// Nursery for incubation, and ultimate sweeping.
//
// NOTE: This MUST be run as a goroutine.
func (c *ChannelArbitrator) channelAttendant(bestHeight int32) {

	// TODO(roasbeef): tell top chain arb we're done
	defer func() {
		c.cfg.BlockEpochs.Cancel()
		c.wg.Done()
	}()

	for {
		select {

		// A new block has arrived, we'll examine all the active HTLC's
		// to see if any of them have expired, and also update our
		// track of the best current height.
		case blockEpoch, ok := <-c.cfg.BlockEpochs.Epochs:
			if !ok {
				return
			}
			bestHeight = blockEpoch.Height

			// If we're not in the default state, then we can
			// ignore this signal as we're waiting for contract
			// resolution.
			if c.state != StateDefault {
				continue
			}

			// Now that a new block has arrived, we'll attempt to
			// advance our state forward.
			nextState, _, err := c.advanceState(
				uint32(bestHeight), chainTrigger,
			)
			if err != nil {
				log.Errorf("unable to advance state: %v", err)
			}

			// If as a result of this trigger, the contract is
			// fully resolved, then well exit.
			if nextState == StateFullyResolved {
				return
			}

		// A new signal update was just sent. This indicates that the
		// channel under watch is now live, and may modify its internal
		// state, so we'll get the most up to date signals to we can
		// properly do our job.
		case signalUpdate := <-c.signalUpdates:
			log.Tracef("ChannelArbitrator(%v) got new signal "+
				"update!", c.cfg.ChanPoint)

			// First, we'll update our set of signals.
			c.htlcUpdates = signalUpdate.newSignals.HtlcUpdates
			c.cfg.ShortChanID = signalUpdate.newSignals.ShortChanID

			// Now that the signals have been updated, we'll now
			// close the done channel to signal to the caller we've
			// registered the new contracts.
			close(signalUpdate.doneChan)

		// A new set of HTLC's has been added or removed from the
		// commitment transaction. So we'll update our activeHTLCs map
		// accordingly.
		case newStateHTLCs := <-c.htlcUpdates:
			// We'll wipe out our old set of HTLC's and instead
			// monitor only the HTLC's that are still active on the
			// current commitment state.
			c.activeHTLCs = newHtlcSet(newStateHTLCs)

			log.Tracef("ChannelArbitrator(%v): fresh set of "+
				"htlcs=%v", c.cfg.ChanPoint,
				newLogClosure(func() string {
					return spew.Sdump(c.activeHTLCs)
				}),
			)

		// We've cooperatively closed the channel, so we're no longer
		// needed. We'll mark the channel as resolved and exit.
		case closeInfo := <-c.cfg.ChainEvents.CooperativeClosure:
			log.Infof("ChannelArbitrator(%v) marking channel "+
				"cooperatively closed", c.cfg.ChanPoint)

			err := c.cfg.MarkChannelClosed(
				closeInfo.ChannelCloseSummary,
			)
			if err != nil {
				log.Errorf("unable to mark channel closed: "+
					"%v", err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state, and the channel is marked resolved.
			_, _, err = c.advanceState(
				closeInfo.CloseHeight, coopCloseTrigger,
			)
			if err != nil {
				log.Errorf("unable to advance state: %v", err)
				return
			}

		// We have broadcasted our commitment, and it is now confirmed
		// on-chain.
		case closeInfo := <-c.cfg.ChainEvents.LocalUnilateralClosure:
			log.Infof("ChannelArbitrator(%v): local on-chain "+
				"channel close", c.cfg.ChanPoint)

			if c.state != StateCommitmentBroadcasted {
				log.Errorf("ChannelArbitrator(%v): unexpected "+
					"local on-chain channel close",
					c.cfg.ChanPoint)
			}
			closeTx := closeInfo.CloseTx

			contractRes := &ContractResolutions{
				CommitHash:       closeTx.TxHash(),
				CommitResolution: closeInfo.CommitResolution,
				HtlcResolutions:  *closeInfo.HtlcResolutions,
			}

			// When processing a unilateral close event, we'll
			// transition to the ContractClosed state. We'll log
			// out the set of resolutions such that they are
			// available to fetch in that state.
			err := c.log.LogContractResolutions(contractRes)
			if err != nil {
				log.Errorf("unable to write resolutions: %v",
					err)
				return
			}

			// After the set of resolutions are successfully
			// logged, we can safely close the channel. After this
			// succeeds we won't be getting chain events anymore,
			// so we must make sure we can recover on restart after
			// it is marked closed. If the next state transition
			// fails, we'll start up in the prior state again, and
			// we won't be longer getting chain events. In this
			// case we must manually re-trigger the state
			// transition into StateContractClosed based on the
			// close status of the channel.
			err = c.cfg.MarkChannelClosed(
				closeInfo.ChannelCloseSummary,
			)
			if err != nil {
				log.Errorf("unable to mark "+
					"channel closed: %v", err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state.
			_, _, err = c.advanceState(
				uint32(closeInfo.SpendingHeight),
				localCloseTrigger,
			)
			if err != nil {
				log.Errorf("unable to advance state: %v", err)
			}

		// The remote party has broadcast the commitment on-chain.
		// We'll examine our state to determine if we need to act at
		// all.
		case uniClosure := <-c.cfg.ChainEvents.RemoteUnilateralClosure:

			log.Infof("ChannelArbitrator(%v): remote party has "+
				"closed channel out on-chain", c.cfg.ChanPoint)

			// If we don't have a self output, and there are no
			// active HTLC's, then we can immediately mark the
			// contract as fully resolved and exit.
			contractRes := &ContractResolutions{
				CommitHash:       *uniClosure.SpenderTxHash,
				CommitResolution: uniClosure.CommitResolution,
				HtlcResolutions:  *uniClosure.HtlcResolutions,
			}

			// As we're now acting upon an event triggered by the
			// broadcast of the remote commitment transaction,
			// we'll swap out our active HTLC set with the set
			// present on their commitment.
			c.activeHTLCs = newHtlcSet(uniClosure.RemoteCommit.Htlcs)

			// When processing a unilateral close event, we'll
			// transition to the ContractClosed state. We'll log
			// out the set of resolutions such that they are
			// available to fetch in that state.
			err := c.log.LogContractResolutions(contractRes)
			if err != nil {
				log.Errorf("unable to write resolutions: %v",
					err)
				return
			}

			// After the set of resolutions are successfully
			// logged, we can safely close the channel. After this
			// succeeds we won't be getting chain events anymore,
			// so we must make sure we can recover on restart after
			// it is marked closed. If the next state transition
			// fails, we'll start up in the prior state again, and
			// we won't be longer getting chain events. In this
			// case we must manually re-trigger the state
			// transition into StateContractClosed based on the
			// close status of the channel.
			closeSummary := &uniClosure.ChannelCloseSummary
			err = c.cfg.MarkChannelClosed(closeSummary)
			if err != nil {
				log.Errorf("unable to mark channel closed: %v",
					err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state.
			_, _, err = c.advanceState(
				uint32(uniClosure.SpendingHeight),
				remoteCloseTrigger,
			)
			if err != nil {
				log.Errorf("unable to advance state: %v", err)
			}

		// A new contract has just been resolved, we'll now check our
		// log to see if all contracts have been resolved. If so, then
		// we can exit as the contract is fully resolved.
		case <-c.resolutionSignal:
			log.Infof("ChannelArbitrator(%v): a contract has been "+
				"fully resolved!", c.cfg.ChanPoint)

			numUnresolved, err := c.log.FetchUnresolvedContracts()
			if err != nil {
				log.Errorf("unable to query resolved "+
					"contracts: %v", err)
			}

			// If we still have unresolved contracts, then we'll
			// stay alive to oversee their resolution.
			if len(numUnresolved) != 0 {
				continue
			}

			log.Infof("ChannelArbitrator(%v): all contracts fully "+
				"resolved, exiting", c.cfg.ChanPoint)

			// Otherwise, our job is finished here, the contract is
			// now fully resolved! We'll mark it as such, then exit
			// ourselves.
			if err := c.cfg.MarkChannelResolved(); err != nil {
				log.Errorf("unable to mark contract "+
					"resolved: %v", err)
			}
			return

		// We've just received a request to forcibly close out the
		// channel. We'll
		case closeReq := <-c.forceCloseReqs:
			if c.state != StateDefault {
				continue
			}

			nextState, closeTx, err := c.advanceState(
				uint32(bestHeight), userTrigger,
			)
			if err != nil {
				log.Errorf("unable to advance state: %v", err)
			}

			select {
			case closeReq.closeTx <- closeTx:
			case <-c.quit:
				return
			}

			select {
			case closeReq.errResp <- err:
			case <-c.quit:
				return
			}

			// If we don't have anything further to do after
			// advancing our state, then we'll exit.
			if nextState == StateFullyResolved {
				log.Infof("ChannelArbitrator(%v): all "+
					"contracts resolved, exiting",
					c.cfg.ChanPoint)
				return
			}

		case <-c.quit:
			return
		}
	}
}
