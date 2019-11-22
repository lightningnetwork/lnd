package contractcourt

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errAlreadyForceClosed is an error returned when we attempt to force
	// close a channel that's already in the process of doing so.
	errAlreadyForceClosed = errors.New("channel is already in the " +
		"process of being force closed")
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
	WitnessUpdates <-chan lntypes.Preimage

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
	LookupPreimage(payhash lntypes.Hash) (lntypes.Preimage, bool)

	// AddPreimages adds a batch of newly discovered preimages to the global
	// cache, and also signals any subscribers of the newly discovered
	// witness.
	AddPreimages(preimages ...lntypes.Preimage) error
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
	// returned transactions is our latest commitment state, which will
	// close the transaction if published.
	ForceCloseChan func() (*wire.MsgTx, error)

	// MarkCommitmentBroadcasted should mark the channel as the commitment
	// being broadcast, and we are waiting for the commitment to confirm.
	MarkCommitmentBroadcasted func(*wire.MsgTx) error

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

// ReportOutputType describes the type of output that is being reported
// on.
type ReportOutputType uint8

const (
	// ReportOutputIncomingHtlc is an incoming hash time locked contract on
	// the commitment tx.
	ReportOutputIncomingHtlc ReportOutputType = iota

	// ReportOutputOutgoingHtlc is an outgoing hash time locked contract on
	// the commitment tx.
	ReportOutputOutgoingHtlc

	// ReportOutputUnencumbered is an uncontested output on the commitment
	// transaction paying to us directly.
	ReportOutputUnencumbered
)

// ContractReport provides a summary of a commitment tx output.
type ContractReport struct {
	// Outpoint is the final output that will be swept back to the wallet.
	Outpoint wire.OutPoint

	// Type indicates the type of the reported output.
	Type ReportOutputType

	// Amount is the final value that will be swept in back to the wallet.
	Amount btcutil.Amount

	// MaturityHeight is the absolute block height that this output will
	// mature at.
	MaturityHeight uint32

	// Stage indicates whether the htlc is in the CLTV-timeout stage (1) or
	// the CSV-delay stage (2). A stage 1 htlc's maturity height will be set
	// to its expiry height, while a stage 2 htlc's maturity height will be
	// set to its confirmation height plus the maturity requirement.
	Stage uint32

	// LimboBalance is the total number of frozen coins within this
	// contract.
	LimboBalance btcutil.Amount

	// RecoveredBalance is the total value that has been successfully swept
	// back to the user's wallet.
	RecoveredBalance btcutil.Amount
}

// htlcSet represents the set of active HTLCs on a given commitment
// transaction.
type htlcSet struct {
	// incomingHTLCs is a map of all incoming HTLCs on the target
	// commitment transaction. We may potentially go onchain to claim the
	// funds sent to us within this set.
	incomingHTLCs map[uint64]channeldb.HTLC

	// outgoingHTLCs is a map of all outgoing HTLCs on the target
	// commitment transaction. We may potentially go onchain to reclaim the
	// funds that are currently in limbo.
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

// HtlcSetKey is a two-tuple that uniquely identifies a set of HTLCs on a
// commitment transaction.
type HtlcSetKey struct {
	// IsRemote denotes if the HTLCs are on the remote commitment
	// transaction.
	IsRemote bool

	// IsPending denotes if the commitment transaction that HTLCS are on
	// are pending (the higher of two unrevoked commitments).
	IsPending bool
}

var (
	// LocalHtlcSet is the HtlcSetKey used for local commitments.
	LocalHtlcSet = HtlcSetKey{IsRemote: false, IsPending: false}

	// RemoteHtlcSet is the HtlcSetKey used for remote commitments.
	RemoteHtlcSet = HtlcSetKey{IsRemote: true, IsPending: false}

	// RemotePendingHtlcSet is the HtlcSetKey used for dangling remote
	// commitment transactions.
	RemotePendingHtlcSet = HtlcSetKey{IsRemote: true, IsPending: true}
)

// String returns a human readable string describing the target HtlcSetKey.
func (h HtlcSetKey) String() string {
	switch h {
	case LocalHtlcSet:
		return "LocalHtlcSet"
	case RemoteHtlcSet:
		return "RemoteHtlcSet"
	case RemotePendingHtlcSet:
		return "RemotePendingHtlcSet"
	default:
		return "unknown HtlcSetKey"
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

	// activeHTLCs is the set of active incoming/outgoing HTLC's on all
	// currently valid commitment transactions.
	activeHTLCs map[HtlcSetKey]htlcSet

	// cfg contains all the functionality that the ChannelArbitrator requires
	// to do its duty.
	cfg ChannelArbitratorConfig

	// signalUpdates is a channel that any new live signals for the channel
	// we're watching over will be sent.
	signalUpdates chan *signalUpdateMsg

	// htlcUpdates is a channel that is sent upon with new updates from the
	// active channel. Each time a new commitment state is accepted, the
	// set of HTLC's on the new state should be sent across this channel.
	htlcUpdates <-chan *ContractUpdate

	// activeResolvers is a slice of any active resolvers. This is used to
	// be able to signal them for shutdown in the case that we shutdown.
	activeResolvers []ContractResolver

	// activeResolversLock prevents simultaneous read and write to the
	// resolvers slice.
	activeResolversLock sync.RWMutex

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
	htlcSets map[HtlcSetKey]htlcSet, log ArbitratorLog) *ChannelArbitrator {

	return &ChannelArbitrator{
		log:              log,
		signalUpdates:    make(chan *signalUpdateMsg),
		htlcUpdates:      make(<-chan *ContractUpdate),
		resolutionSignal: make(chan struct{}),
		forceCloseReqs:   make(chan *forceCloseReq),
		activeHTLCs:      htlcSets,
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
		err error
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

			case channeldb.BreachClose:
				trigger = breachCloseTrigger

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

	// Next we'll fetch our confirmed commitment set. This will only exist
	// if the channel has been closed out on chain for modern nodes. For
	// older nodes, this won't be found at all, and will rely on the
	// existing written chain actions. Additionally, if this channel hasn't
	// logged any actions in the log, then this field won't be present.
	commitSet, err := c.log.FetchConfirmedCommitSet()
	if err != nil && err != errNoCommitSet && err != errScopeBucketNoExist {
		return err
	}

	// We'll now attempt to advance our state forward based on the current
	// on-chain state, and our set of active contracts.
	startingState := c.state
	nextState, _, err := c.advanceState(
		triggerHeight, trigger, commitSet,
	)
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

		// In order to relaunch the resolvers, we'll need to fetch the
		// set of HTLCs that were present in the commitment transaction
		// at the time it was confirmed. commitSet.ConfCommitKey can't
		// be nil at this point since we're in
		// StateWaitingFullResolution. We can only be in
		// StateWaitingFullResolution after we've transitioned from
		// StateContractClosed which can only be triggered by the local
		// or remote close trigger. This trigger is only fired when we
		// receive a chain event from the chain watcher than the
		// commitment has been confirmed on chain, and before we
		// advance our state step, we call InsertConfirmedCommitSet.
		if err := c.relaunchResolvers(commitSet); err != nil {
			c.cfg.BlockEpochs.Cancel()
			return err
		}
	}

	c.wg.Add(1)
	go c.channelAttendant(bestHeight)
	return nil
}

// relauchResolvers relaunches the set of resolvers for unresolved contracts in
// order to provide them with information that's not immediately available upon
// starting the ChannelArbitrator. This information should ideally be stored in
// the database, so this only serves as a intermediate work-around to prevent a
// migration.
func (c *ChannelArbitrator) relaunchResolvers(commitSet *CommitSet) error {
	// We'll now query our log to see if there are any active unresolved
	// contracts. If this is the case, then we'll relaunch all contract
	// resolvers.
	unresolvedContracts, err := c.log.FetchUnresolvedContracts()
	if err != nil {
		return err
	}

	// Retrieve the commitment tx hash from the log.
	contractResolutions, err := c.log.FetchContractResolutions()
	if err != nil {
		log.Errorf("unable to fetch contract resolutions: %v",
			err)
		return err
	}
	commitHash := contractResolutions.CommitHash

	// In prior versions of lnd, the information needed to supplement the
	// resolvers (in most cases, the full amount of the HTLC) was found in
	// the chain action map, which is now deprecated.  As a result, if the
	// commitSet is nil (an older node with unresolved HTLCs at time of
	// upgrade), then we'll use the chain action information in place. The
	// chain actions may exclude some information, but we cannot recover it
	// for these older nodes at the moment.
	var confirmedHTLCs []channeldb.HTLC
	if commitSet != nil {
		confirmedHTLCs = commitSet.HtlcSets[*commitSet.ConfCommitKey]
	} else {
		chainActions, err := c.log.FetchChainActions()
		if err != nil {
			log.Errorf("unable to fetch chain actions: %v", err)
			return err
		}
		for _, htlcs := range chainActions {
			confirmedHTLCs = append(confirmedHTLCs, htlcs...)
		}
	}

	// Reconstruct the htlc outpoints and data from the chain action log.
	// The purpose of the constructed htlc map is to supplement to
	// resolvers restored from database with extra data. Ideally this data
	// is stored as part of the resolver in the log. This is a workaround
	// to prevent a db migration. We use all available htlc sets here in
	// order to ensure we have complete coverage.
	htlcMap := make(map[wire.OutPoint]*channeldb.HTLC)
	for _, htlc := range confirmedHTLCs {
		htlc := htlc
		outpoint := wire.OutPoint{
			Hash:  commitHash,
			Index: uint32(htlc.OutputIndex),
		}
		htlcMap[outpoint] = &htlc
	}

	log.Infof("ChannelArbitrator(%v): relaunching %v contract "+
		"resolvers", c.cfg.ChanPoint, len(unresolvedContracts))

	for _, resolver := range unresolvedContracts {
		htlcResolver, ok := resolver.(htlcContractResolver)
		if !ok {
			continue
		}

		htlcPoint := htlcResolver.HtlcPoint()
		htlc, ok := htlcMap[htlcPoint]
		if !ok {
			return fmt.Errorf(
				"htlc resolver %T unavailable", resolver,
			)
		}

		htlcResolver.Supplement(*htlc)
	}

	c.launchResolvers(unresolvedContracts)

	return nil
}

// Report returns htlc reports for the active resolvers.
func (c *ChannelArbitrator) Report() []*ContractReport {
	c.activeResolversLock.RLock()
	defer c.activeResolversLock.RUnlock()

	var reports []*ContractReport
	for _, resolver := range c.activeResolvers {
		r, ok := resolver.(reportingContractResolver)
		if !ok {
			continue
		}

		if r.IsResolved() {
			continue
		}

		report := r.report()
		if report == nil {
			continue
		}

		reports = append(reports, report)
	}

	return reports
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

	c.activeResolversLock.RLock()
	for _, activeResolver := range c.activeResolvers {
		activeResolver.Stop()
	}
	c.activeResolversLock.RUnlock()

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

	// breachCloseTrigger is a transition trigger driven by a remote breach
	// being confirmed. In this case the channel arbitrator won't have to
	// do anything, so we'll just clean up and exit gracefully.
	breachCloseTrigger
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

	case breachCloseTrigger:
		return "breachCloseTrigger"

	default:
		return "unknown trigger"
	}
}

// stateStep is a help method that examines our internal state, and attempts
// the appropriate state transition if necessary. The next state we transition
// to is returned, Additionally, if the next transition results in a commitment
// broadcast, the commitment transaction itself is returned.
func (c *ChannelArbitrator) stateStep(
	triggerHeight uint32, trigger transitionTrigger,
	confCommitSet *CommitSet) (ArbitratorState, *wire.MsgTx, error) {

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
		// arbitrating for. If a commitment has confirmed, then we'll
		// use the set snapshot from the chain, otherwise we'll use our
		// current set.
		var htlcs map[HtlcSetKey]htlcSet
		if confCommitSet != nil {
			htlcs = confCommitSet.toActiveHTLCSets()
		} else {
			htlcs = c.activeHTLCs
		}
		chainActions, err := c.checkLocalChainActions(
			triggerHeight, trigger, htlcs, false,
		)
		if err != nil {
			return StateDefault, nil, err
		}

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
		// any contracts to resolve. The same is true in the case of a
		// breach.
		case coopCloseTrigger, breachCloseTrigger:
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
		// StateFullyResolved. In that case, one of the four close
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

		case coopCloseTrigger, breachCloseTrigger:
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
		var err error
		closeTx, err = c.cfg.ForceCloseChan()
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"force close: %v", c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}

		// Before publishing the transaction, we store it to the
		// database, such that we can re-publish later in case it
		// didn't propagate.
		if err := c.cfg.MarkCommitmentBroadcasted(closeTx); err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"mark commitment broadcasted: %v",
				c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}

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

		// If a coop close or breach was confirmed, jump straight to
		// the fully resolved state.
		case coopCloseTrigger, breachCloseTrigger:
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
		contractResolutions, err := c.log.FetchContractResolutions()
		if err != nil {
			log.Errorf("unable to fetch contract resolutions: %v",
				err)
			return StateError, closeTx, err
		}

		// If the resolution is empty, and we have no HTLCs at all to
		// tend to, then we're done here. We don't need to launch any
		// resolvers, and can go straight to our final state.
		if contractResolutions.IsEmpty() && confCommitSet.IsEmpty() {
			log.Infof("ChannelArbitrator(%v): contract "+
				"resolutions empty, marking channel as fully resolved!",
				c.cfg.ChanPoint)
			nextState = StateFullyResolved
			break
		}

		// Now that we know we'll need to act, we'll process the htlc
		// actions, wen create the structures we need to resolve all
		// outstanding contracts.
		htlcResolvers, pktsToSend, err := c.prepContractResolutions(
			contractResolutions, triggerHeight, trigger,
			confCommitSet,
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
		if len(pktsToSend) != 0 {
			err := c.cfg.DeliverResolutionMsg(pktsToSend...)
			if err != nil {
				// TODO(roasbeef): make sure packet sends are
				// idempotent
				log.Errorf("unable to send pkts: %v", err)
				return StateError, closeTx, err
			}
		}

		log.Debugf("ChannelArbitrator(%v): inserting %v contract "+
			"resolvers", c.cfg.ChanPoint, len(htlcResolvers))

		err = c.log.InsertUnresolvedContracts(htlcResolvers...)
		if err != nil {
			return StateError, closeTx, err
		}

		// Finally, we'll launch all the required contract resolvers.
		// Once they're all resolved, we're no longer needed.
		c.launchResolvers(htlcResolvers)

		nextState = StateWaitingFullResolution

	// This is our terminal state. We'll keep returning this state until
	// all contracts are fully resolved.
	case StateWaitingFullResolution:
		log.Infof("ChannelArbitrator(%v): still awaiting contract "+
			"resolution", c.cfg.ChanPoint)

		numUnresolved, err := c.log.FetchUnresolvedContracts()
		if err != nil {
			return StateError, closeTx, err
		}

		// If we still have unresolved contracts, then we'll stay alive
		// to oversee their resolution.
		if len(numUnresolved) != 0 {
			nextState = StateWaitingFullResolution
			break
		}

		nextState = StateFullyResolved

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

// launchResolvers updates the activeResolvers list and starts the resolvers.
func (c *ChannelArbitrator) launchResolvers(resolvers []ContractResolver) {
	c.activeResolversLock.Lock()
	defer c.activeResolversLock.Unlock()

	c.activeResolvers = resolvers
	for _, contract := range resolvers {
		c.wg.Add(1)
		go c.resolveContract(contract)
	}
}

// advanceState is the main driver of our state machine. This method is an
// iterative function which repeatedly attempts to advance the internal state
// of the channel arbitrator. The state will be advanced until we reach a
// redundant transition, meaning that the state transition is a noop. The final
// param is a callback that allows the caller to execute an arbitrary action
// after each state transition.
func (c *ChannelArbitrator) advanceState(
	triggerHeight uint32, trigger transitionTrigger,
	confCommitSet *CommitSet) (ArbitratorState, *wire.MsgTx, error) {

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
			triggerHeight, trigger, confCommitSet,
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

// Merge merges the passed chain actions with the target chain action map.
func (c ChainActionMap) Merge(actions ChainActionMap) {
	for chainAction, htlcs := range actions {
		c[chainAction] = append(c[chainAction], htlcs...)
	}
}

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

// checkCommitChainActions is called for each new block connected to the end of
// the main chain. Given the new block height, this new method will examine all
// active HTLC's, and determine if we need to go on-chain to claim any of them.
// A map of action -> []htlc is returned, detailing what action (if any) should
// be performed for each HTLC. For timed out HTLC's, once the commitment has
// been sufficiently confirmed, the HTLC's should be canceled backwards. For
// redeemed HTLC's, we should send the pre-image back to the incoming link.
func (c *ChannelArbitrator) checkCommitChainActions(height uint32,
	trigger transitionTrigger, htlcs htlcSet) (ChainActionMap, error) {

	// TODO(roasbeef): would need to lock channel? channel totem?
	//  * race condition if adding and we broadcast, etc
	//  * or would make each instance sync?

	log.Debugf("ChannelArbitrator(%v): checking commit chain actions at "+
		"height=%v, in_htlc_count=%v, out_htlc_count=%v",
		c.cfg.ChanPoint, height,
		len(htlcs.incomingHTLCs), len(htlcs.outgoingHTLCs))

	actionMap := make(ChainActionMap)

	// First, we'll make an initial pass over the set of incoming and
	// outgoing HTLC's to decide if we need to go on chain at all.
	haveChainActions := false
	for _, htlc := range htlcs.outgoingHTLCs {
		// We'll need to go on-chain for an outgoing HTLC if it was
		// never resolved downstream, and it's "close" to timing out.
		toChain := c.shouldGoOnChain(
			htlc.RefundTimeout, c.cfg.OutgoingBroadcastDelta,
			height,
		)

		if toChain {
			log.Debugf("ChannelArbitrator(%v): go to chain for "+
				"outgoing htlc %x: timeout=%v, "+
				"blocks_until_expiry=%v, broadcast_delta=%v",
				c.cfg.ChanPoint, htlc.RHash[:],
				htlc.RefundTimeout, htlc.RefundTimeout-height,
				c.cfg.OutgoingBroadcastDelta,
			)
		}

		haveChainActions = haveChainActions || toChain
	}

	for _, htlc := range htlcs.incomingHTLCs {
		// We'll need to go on-chain to pull an incoming HTLC iff we
		// know the pre-image and it's close to timing out. We need to
		// ensure that we claim the funds that our rightfully ours
		// on-chain.
		preimageAvailable, err := c.isPreimageAvailable(htlc.RHash)
		if err != nil {
			return nil, err
		}

		if !preimageAvailable {
			continue
		}

		toChain := c.shouldGoOnChain(
			htlc.RefundTimeout, c.cfg.IncomingBroadcastDelta,
			height,
		)

		if toChain {
			log.Debugf("ChannelArbitrator(%v): go to chain for "+
				"incoming htlc %x: timeout=%v, "+
				"blocks_until_expiry=%v, broadcast_delta=%v",
				c.cfg.ChanPoint, htlc.RHash[:],
				htlc.RefundTimeout, htlc.RefundTimeout-height,
				c.cfg.IncomingBroadcastDelta,
			)
		}

		haveChainActions = haveChainActions || toChain
	}

	// If we don't have any actions to make, then we'll return an empty
	// action map. We only do this if this was a chain trigger though, as
	// if we're going to broadcast the commitment (or the remote party did)
	// we're *forced* to act on each HTLC.
	if !haveChainActions && trigger == chainTrigger {
		log.Tracef("ChannelArbitrator(%v): no actions to take at "+
			"height=%v", c.cfg.ChanPoint, height)
		return actionMap, nil
	}

	// Now that we know we'll need to go on-chain, we'll examine all of our
	// active outgoing HTLC's to see if we either need to: sweep them after
	// a timeout (then cancel backwards), cancel them backwards
	// immediately, or watch them as they're still active contracts.
	for _, htlc := range htlcs.outgoingHTLCs {
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
			htlc.RefundTimeout, c.cfg.OutgoingBroadcastDelta,
			height,
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
	for _, htlc := range htlcs.incomingHTLCs {
		// If the HTLC is dust, there is no action to be taken.
		if htlc.OutputIndex < 0 {
			log.Debugf("ChannelArbitrator(%v): no resolution "+
				"needed for incoming dust htlc=%x",
				c.cfg.ChanPoint, htlc.RHash[:])

			continue
		}

		log.Tracef("ChannelArbitrator(%v): watching chain to decide "+
			"action for incoming htlc=%x", c.cfg.ChanPoint,
			htlc.RHash[:])

		actionMap[HtlcIncomingWatchAction] = append(
			actionMap[HtlcIncomingWatchAction], htlc,
		)
	}

	return actionMap, nil
}

// isPreimageAvailable returns whether the hash preimage is available in either
// the preimage cache or the invoice database.
func (c *ChannelArbitrator) isPreimageAvailable(hash lntypes.Hash) (bool,
	error) {

	// Start by checking the preimage cache for preimages of
	// forwarded HTLCs.
	_, preimageAvailable := c.cfg.PreimageDB.LookupPreimage(
		hash,
	)
	if preimageAvailable {
		return true, nil
	}

	// Then check if we have an invoice that can be settled by this HTLC.
	//
	// TODO(joostjager): Check that there are still more blocks remaining
	// than the invoice cltv delta. We don't want to go to chain only to
	// have the incoming contest resolver decide that we don't want to
	// settle this invoice.
	invoice, err := c.cfg.Registry.LookupInvoice(hash)
	switch err {
	case nil:
	case channeldb.ErrInvoiceNotFound, channeldb.ErrNoInvoicesCreated:
		return false, nil
	default:
		return false, err
	}

	preimageAvailable = invoice.Terms.PaymentPreimage !=
		channeldb.UnknownPreimage

	return preimageAvailable, nil
}

// checkLocalChainActions is similar to checkCommitChainActions, but it also
// examines the set of HTLCs on the remote party's commitment. This allows us
// to ensure we're able to satisfy the HTLC timeout constraints for incoming vs
// outgoing HTLCs.
func (c *ChannelArbitrator) checkLocalChainActions(
	height uint32, trigger transitionTrigger,
	activeHTLCs map[HtlcSetKey]htlcSet,
	commitsConfirmed bool) (ChainActionMap, error) {

	// First, we'll check our local chain actions as normal. This will only
	// examine HTLCs on our local commitment (timeout or settle).
	localCommitActions, err := c.checkCommitChainActions(
		height, trigger, activeHTLCs[LocalHtlcSet],
	)
	if err != nil {
		return nil, err
	}

	// Next, we'll examine the remote commitment (and maybe a dangling one)
	// to see if the set difference of our HTLCs is non-empty. If so, then
	// we may need to cancel back some HTLCs if we decide go to chain.
	remoteDanglingActions := c.checkRemoteDanglingActions(
		height, activeHTLCs, commitsConfirmed,
	)

	// Finally, we'll merge the two set of chain actions.
	localCommitActions.Merge(remoteDanglingActions)

	return localCommitActions, nil
}

// checkRemoteDanglingActions examines the set of remote commitments for any
// HTLCs that are close to timing out. If we find any, then we'll return a set
// of chain actions for HTLCs that are on our commitment, but not theirs to
// cancel immediately.
func (c *ChannelArbitrator) checkRemoteDanglingActions(
	height uint32, activeHTLCs map[HtlcSetKey]htlcSet,
	commitsConfirmed bool) ChainActionMap {

	var (
		pendingRemoteHTLCs []channeldb.HTLC
		localHTLCs         = make(map[uint64]struct{})
		remoteHTLCs        = make(map[uint64]channeldb.HTLC)
		actionMap          = make(ChainActionMap)
	)

	// First, we'll construct two sets of the outgoing HTLCs: those on our
	// local commitment, and those that are on the remote commitment(s).
	for htlcSetKey, htlcs := range activeHTLCs {
		if htlcSetKey.IsRemote {
			for _, htlc := range htlcs.outgoingHTLCs {
				remoteHTLCs[htlc.HtlcIndex] = htlc
			}
		} else {
			for _, htlc := range htlcs.outgoingHTLCs {
				localHTLCs[htlc.HtlcIndex] = struct{}{}
			}
		}
	}

	// With both sets constructed, we'll now compute the set difference of
	// our two sets of HTLCs. This'll give us the HTLCs that exist on the
	// remote commitment transaction, but not on ours.
	for htlcIndex, htlc := range remoteHTLCs {
		if _, ok := localHTLCs[htlcIndex]; ok {
			continue
		}

		pendingRemoteHTLCs = append(pendingRemoteHTLCs, htlc)
	}

	// Finally, we'll examine all the pending remote HTLCs for those that
	// have expired. If we find any, then we'll recommend that they be
	// failed now so we can free up the incoming HTLC.
	for _, htlc := range pendingRemoteHTLCs {
		// We'll now check if we need to go to chain in order to cancel
		// the incoming HTLC.
		goToChain := c.shouldGoOnChain(
			htlc.RefundTimeout, c.cfg.OutgoingBroadcastDelta,
			height,
		)

		// If we don't need to go to chain, and no commitments have
		// been confirmed, then we can move on. Otherwise, if
		// commitments have been confirmed, then we need to cancel back
		// *all* of the pending remote HTLCS.
		if !goToChain && !commitsConfirmed {
			continue
		}

		log.Tracef("ChannelArbitrator(%v): immediately failing "+
			"htlc=%x from remote commitment",
			c.cfg.ChanPoint, htlc.RHash[:])

		actionMap[HtlcFailNowAction] = append(
			actionMap[HtlcFailNowAction], htlc,
		)
	}

	return actionMap
}

// checkRemoteChainActions examines the two possible remote commitment chains
// and returns the set of chain actions we need to carry out if the remote
// commitment (non pending) confirms. The pendingConf indicates if the pending
// remote commitment confirmed. This is similar to checkCommitChainActions, but
// we'll immediately fail any HTLCs on the pending remote commit, but not the
// remote commit (or the other way around).
func (c *ChannelArbitrator) checkRemoteChainActions(
	height uint32, trigger transitionTrigger,
	activeHTLCs map[HtlcSetKey]htlcSet,
	pendingConf bool) (ChainActionMap, error) {

	// First, we'll examine all the normal chain actions on the remote
	// commitment that confirmed.
	confHTLCs := activeHTLCs[RemoteHtlcSet]
	if pendingConf {
		confHTLCs = activeHTLCs[RemotePendingHtlcSet]
	}
	remoteCommitActions, err := c.checkCommitChainActions(
		height, trigger, confHTLCs,
	)
	if err != nil {
		return nil, err
	}

	// With this actions computed, we'll now check the diff of the HTLCs on
	// the commitments, and cancel back any that are on the pending but not
	// the non-pending.
	remoteDiffActions := c.checkRemoteDiffActions(
		height, activeHTLCs, pendingConf,
	)

	// Finally, we'll merge all the chain actions and the final set of
	// chain actions.
	remoteCommitActions.Merge(remoteDiffActions)
	return remoteCommitActions, nil
}

// checkRemoteDiffActions checks the set difference of the HTLCs on the remote
// confirmed commit and remote dangling commit for HTLCS that we need to cancel
// back. If we find any HTLCs on the remote pending but not the remote, then
// we'll mark them to be failed immediately.
func (c *ChannelArbitrator) checkRemoteDiffActions(height uint32,
	activeHTLCs map[HtlcSetKey]htlcSet,
	pendingConf bool) ChainActionMap {

	// First, we'll partition the HTLCs into those that are present on the
	// confirmed commitment, and those on the dangling commitment.
	confHTLCs := activeHTLCs[RemoteHtlcSet]
	danglingHTLCs := activeHTLCs[RemotePendingHtlcSet]
	if pendingConf {
		confHTLCs = activeHTLCs[RemotePendingHtlcSet]
		danglingHTLCs = activeHTLCs[RemoteHtlcSet]
	}

	// Next, we'll create a set of all the HTLCs confirmed commitment.
	remoteHtlcs := make(map[uint64]struct{})
	for _, htlc := range confHTLCs.outgoingHTLCs {
		remoteHtlcs[htlc.HtlcIndex] = struct{}{}
	}

	// With the remote HTLCs assembled, we'll mark any HTLCs only on the
	// remote dangling commitment to be failed asap.
	actionMap := make(ChainActionMap)
	for _, htlc := range danglingHTLCs.outgoingHTLCs {
		if _, ok := remoteHtlcs[htlc.HtlcIndex]; ok {
			continue
		}

		actionMap[HtlcFailNowAction] = append(
			actionMap[HtlcFailNowAction], htlc,
		)

		log.Tracef("ChannelArbitrator(%v): immediately failing "+
			"htlc=%x from remote commitment",
			c.cfg.ChanPoint, htlc.RHash[:])
	}

	return actionMap
}

// constructChainActions returns the set of actions that should be taken for
// confirmed HTLCs at the specified height. Our actions will depend on the set
// of HTLCs that were active across all channels at the time of channel
// closure.
func (c *ChannelArbitrator) constructChainActions(confCommitSet *CommitSet,
	height uint32, trigger transitionTrigger) (ChainActionMap, error) {

	// If we've reached this point and have not confirmed commitment set,
	// then this is an older node that had a pending close channel before
	// the CommitSet was introduced. In this case, we'll just return the
	// existing ChainActionMap they had on disk.
	if confCommitSet == nil {
		return c.log.FetchChainActions()
	}

	// Otherwise we have the full commitment set written to disk, and can
	// proceed as normal.
	htlcSets := confCommitSet.toActiveHTLCSets()
	switch *confCommitSet.ConfCommitKey {

	// If the local commitment transaction confirmed, then we'll examine
	// that as well as their commitments to the set of chain actions.
	case LocalHtlcSet:
		return c.checkLocalChainActions(
			height, trigger, htlcSets, true,
		)

	// If the remote commitment confirmed, then we'll grab all the chain
	// actions for the remote commit, and check the pending commit for any
	// HTLCS we need to handle immediately (dust).
	case RemoteHtlcSet:
		return c.checkRemoteChainActions(
			height, trigger, htlcSets, false,
		)

	// Otherwise, the remote pending commitment confirmed, so we'll examine
	// the HTLCs on that unrevoked dangling commitment.
	case RemotePendingHtlcSet:
		return c.checkRemoteChainActions(
			height, trigger, htlcSets, true,
		)
	}

	return nil, fmt.Errorf("unable to locate chain actions")
}

// prepContractResolutions is called either int he case that we decide we need
// to go to chain, or the remote party goes to chain. Given a set of actions we
// need to take for each HTLC, this method will return a set of contract
// resolvers that will resolve the contracts on-chain if needed, and also a set
// of packets to send to the htlcswitch in order to ensure all incoming HTLC's
// are properly resolved.
func (c *ChannelArbitrator) prepContractResolutions(
	contractResolutions *ContractResolutions, height uint32,
	trigger transitionTrigger,
	confCommitSet *CommitSet) ([]ContractResolver, []ResolutionMsg, error) {

	// First, we'll reconstruct a fresh set of chain actions as the set of
	// actions we need to act on may differ based on if it was our
	// commitment, or they're commitment that hit the chain.
	htlcActions, err := c.constructChainActions(
		confCommitSet, height, trigger,
	)
	if err != nil {
		return nil, nil, err
	}

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
		inResolutionMap[inRes.HtlcPoint()] = inRes
	}
	for i := 0; i < len(outgoingResolutions); i++ {
		outRes := outgoingResolutions[i]
		outResolutionMap[outRes.HtlcPoint()] = outRes
	}

	// We'll create the resolver kit that we'll be cloning for each
	// resolver so they each can do their duty.
	resolverCfg := ResolverConfig{
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
				htlc := htlc

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

				resolver := newSuccessResolver(
					resolution, height, htlc, resolverCfg,
				)
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// If we can timeout the HTLC directly, then we'll create the
		// proper resolver to do so, who will then cancel the packet
		// backwards.
		case HtlcTimeoutAction:
			for _, htlc := range htlcs {
				htlc := htlc

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

				resolver := newTimeoutResolver(
					resolution, height, htlc, resolverCfg,
				)
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// If this is an incoming HTLC, but we can't act yet, then
		// we'll create an incoming resolver to redeem the HTLC if we
		// learn of the pre-image, or let the remote party time out.
		case HtlcIncomingWatchAction:
			for _, htlc := range htlcs {
				htlc := htlc

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

				resolver := newIncomingContestResolver(
					resolution, height, htlc,
					resolverCfg,
				)
				htlcResolvers = append(htlcResolvers, resolver)
			}

		// Finally, if this is an outgoing HTLC we've sent, then we'll
		// launch a resolver to watch for the pre-image (and settle
		// backwards), or just timeout.
		case HtlcOutgoingWatchAction:
			for _, htlc := range htlcs {
				htlc := htlc

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

				resolver := newOutgoingContestResolver(
					resolution, height, htlc, resolverCfg,
				)
				htlcResolvers = append(htlcResolvers, resolver)
			}
		}
	}

	// Finally, if this is was a unilateral closure, then we'll also create
	// a resolver to sweep our commitment output (but only if it wasn't
	// trimmed).
	if contractResolutions.CommitResolution != nil {
		resolver := newCommitSweepResolver(
			*contractResolutions.CommitResolution,
			height, c.cfg.ChanPoint, resolverCfg,
		)
		htlcResolvers = append(htlcResolvers, resolver)
	}

	return htlcResolvers, msgsToSend, nil
}

// replaceResolver replaces a in the list of active resolvers. If the resolver
// to be replaced is not found, it returns an error.
func (c *ChannelArbitrator) replaceResolver(oldResolver,
	newResolver ContractResolver) error {

	c.activeResolversLock.Lock()
	defer c.activeResolversLock.Unlock()

	oldKey := oldResolver.ResolverKey()
	for i, r := range c.activeResolvers {
		if bytes.Equal(r.ResolverKey(), oldKey) {
			c.activeResolvers[i] = newResolver
			return nil
		}
	}

	return errors.New("resolver to be replaced not found")
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
				if err == errResolverShuttingDown {
					return
				}

				log.Errorf("ChannelArbitrator(%v): unable to "+
					"progress %T: %v",
					c.cfg.ChanPoint, currentContract, err)
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

				// Swap contract in log.
				err := c.log.SwapContract(
					currentContract, nextContract,
				)
				if err != nil {
					log.Errorf("unable to add recurse "+
						"contract: %v", err)
				}

				// Swap contract in resolvers list. This is to
				// make sure that reports are queried from the
				// new resolver.
				err = c.replaceResolver(
					currentContract, nextContract,
				)
				if err != nil {
					log.Errorf("unable to replace "+
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
				uint32(bestHeight), chainTrigger, nil,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
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
		case htlcUpdate := <-c.htlcUpdates:
			// We'll wipe out our old set of HTLC's for each
			// htlcSetKey type included in this update in order to
			// only monitor the HTLCs that are still active on this
			// target commitment.
			c.activeHTLCs[htlcUpdate.HtlcKey] = newHtlcSet(
				htlcUpdate.Htlcs,
			)

			log.Tracef("ChannelArbitrator(%v): fresh set of htlcs=%v",
				c.cfg.ChanPoint,
				newLogClosure(func() string {
					return spew.Sdump(htlcUpdate)
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
				log.Errorf("Unable to mark channel closed: "+
					"%v", err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state, and the channel is marked resolved.
			_, _, err = c.advanceState(
				closeInfo.CloseHeight, coopCloseTrigger, nil,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
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
			// available to fetch in that state, we'll also write
			// the commit set so we can reconstruct our chain
			// actions on restart.
			err := c.log.LogContractResolutions(contractRes)
			if err != nil {
				log.Errorf("Unable to write resolutions: %v",
					err)
				return
			}
			err = c.log.InsertConfirmedCommitSet(
				&closeInfo.CommitSet,
			)
			if err != nil {
				log.Errorf("Unable to write commit set: %v",
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
				log.Errorf("Unable to mark "+
					"channel closed: %v", err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state.
			_, _, err = c.advanceState(
				uint32(closeInfo.SpendingHeight),
				localCloseTrigger, &closeInfo.CommitSet,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
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

			// When processing a unilateral close event, we'll
			// transition to the ContractClosed state. We'll log
			// out the set of resolutions such that they are
			// available to fetch in that state, we'll also write
			// the commit set so we can reconstruct our chain
			// actions on restart.
			err := c.log.LogContractResolutions(contractRes)
			if err != nil {
				log.Errorf("Unable to write resolutions: %v",
					err)
				return
			}
			err = c.log.InsertConfirmedCommitSet(
				&uniClosure.CommitSet,
			)
			if err != nil {
				log.Errorf("Unable to write commit set: %v",
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
				log.Errorf("Unable to mark channel closed: %v",
					err)
				return
			}

			// We'll now advance our state machine until it reaches
			// a terminal state.
			_, _, err = c.advanceState(
				uint32(uniClosure.SpendingHeight),
				remoteCloseTrigger, &uniClosure.CommitSet,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
			}

		// The remote has breached the channel. As this is handled by
		// the ChainWatcher and BreachArbiter, we don't have to do
		// anything in particular, so just advance our state and
		// gracefully exit.
		case <-c.cfg.ChainEvents.ContractBreach:
			log.Infof("ChannelArbitrator(%v): remote party has "+
				"breached channel!", c.cfg.ChanPoint)

			// We'll advance our state machine until it reaches a
			// terminal state.
			_, _, err := c.advanceState(
				uint32(bestHeight), breachCloseTrigger, nil,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
			}

		// A new contract has just been resolved, we'll now check our
		// log to see if all contracts have been resolved. If so, then
		// we can exit as the contract is fully resolved.
		case <-c.resolutionSignal:
			log.Infof("ChannelArbitrator(%v): a contract has been "+
				"fully resolved!", c.cfg.ChanPoint)

			nextState, _, err := c.advanceState(
				uint32(bestHeight), chainTrigger, nil,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
			}

			// If we don't have anything further to do after
			// advancing our state, then we'll exit.
			if nextState == StateFullyResolved {
				log.Infof("ChannelArbitrator(%v): all "+
					"contracts fully resolved, exiting",
					c.cfg.ChanPoint)

				return
			}

		// We've just received a request to forcibly close out the
		// channel. We'll
		case closeReq := <-c.forceCloseReqs:
			if c.state != StateDefault {
				select {
				case closeReq.closeTx <- nil:
				case <-c.quit:
				}

				select {
				case closeReq.errResp <- errAlreadyForceClosed:
				case <-c.quit:
				}

				continue
			}

			nextState, closeTx, err := c.advanceState(
				uint32(bestHeight), userTrigger, nil,
			)
			if err != nil {
				log.Errorf("Unable to advance state: %v", err)
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
