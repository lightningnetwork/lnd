package contractcourt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
)

var (
	// errAlreadyForceClosed is an error returned when we attempt to force
	// close a channel that's already in the process of doing so.
	errAlreadyForceClosed = errors.New("channel is already in the " +
		"process of being force closed")
)

const (
	// arbitratorBlockBufferSize is the size of the buffer we give to each
	// channel arbitrator.
	arbitratorBlockBufferSize = 20

	// AnchorOutputValue is the output value for the anchor output of an
	// anchor channel.
	// See BOLT 03 for more details:
	// https://github.com/lightning/bolts/blob/master/03-transactions.md
	AnchorOutputValue = btcutil.Amount(330)
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
	SubscribeUpdates(chanID lnwire.ShortChannelID, htlc *channeldb.HTLC,
		payload *hop.Payload,
		nextHopOnionBlob []byte) (*WitnessSubscription, error)

	// LookupPreimage attempts to lookup a preimage in the global cache.
	// True is returned for the second argument if the preimage is found.
	LookupPreimage(payhash lntypes.Hash) (lntypes.Preimage, bool)

	// AddPreimages adds a batch of newly discovered preimages to the global
	// cache, and also signals any subscribers of the newly discovered
	// witness.
	AddPreimages(preimages ...lntypes.Preimage) error
}

// ArbChannel is an abstraction that allows the channel arbitrator to interact
// with an open channel.
type ArbChannel interface {
	// ForceCloseChan should force close the contract that this attendant
	// is watching over. We'll use this when we decide that we need to go
	// to chain. It should in addition tell the switch to remove the
	// corresponding link, such that we won't accept any new updates. The
	// returned summary contains all items needed to eventually resolve all
	// outputs on chain.
	ForceCloseChan() (*wire.MsgTx, error)

	// NewAnchorResolutions returns the anchor resolutions for currently
	// valid commitment transactions.
	NewAnchorResolutions() (*lnwallet.AnchorResolutions, error)
}

// ChannelArbitratorConfig contains all the functionality that the
// ChannelArbitrator needs in order to properly arbitrate any contract dispute
// on chain.
type ChannelArbitratorConfig struct {
	// ChanPoint is the channel point that uniquely identifies this
	// channel.
	ChanPoint wire.OutPoint

	// Channel is the full channel data structure. For legacy channels, this
	// field may not always be set after a restart.
	Channel ArbChannel

	// ShortChanID describes the exact location of the channel within the
	// chain. We'll use this to address any messages that we need to send
	// to the switch during contract resolution.
	ShortChanID lnwire.ShortChannelID

	// ChainEvents is an active subscription to the chain watcher for this
	// channel to be notified of any on-chain activity related to this
	// channel.
	ChainEvents *ChainEventSubscription

	// MarkCommitmentBroadcasted should mark the channel as the commitment
	// being broadcast, and we are waiting for the commitment to confirm.
	MarkCommitmentBroadcasted func(*wire.MsgTx, lntypes.ChannelParty) error

	// MarkChannelClosed marks the channel closed in the database, with the
	// passed close summary. After this method successfully returns we can
	// no longer expect to receive chain events for this channel, and must
	// be able to recover from a failure without getting the close event
	// again. It takes an optional channel status which will update the
	// channel status in the record that we keep of historical channels.
	MarkChannelClosed func(*channeldb.ChannelCloseSummary,
		...channeldb.ChannelStatus) error

	// IsPendingClose is a boolean indicating whether the channel is marked
	// as pending close in the database.
	IsPendingClose bool

	// ClosingHeight is the height at which the channel was closed. Note
	// that this value is only valid if IsPendingClose is true.
	ClosingHeight uint32

	// CloseType is the type of the close event in case IsPendingClose is
	// true. Otherwise this value is unset.
	CloseType channeldb.ClosureType

	// NotifyChannelResolved is used by the channel arbitrator to signal
	// that a given channel has been resolved.
	NotifyChannelResolved func()

	// PutResolverReport records a resolver report for the channel. If the
	// transaction provided is nil, the function should write the report
	// in a new transaction.
	PutResolverReport func(tx kvdb.RwTx,
		report *channeldb.ResolverReport) error

	// FetchHistoricalChannel retrieves the historical state of a channel.
	// This is mostly used to supplement the ContractResolvers with
	// additional information required for proper contract resolution.
	FetchHistoricalChannel func() (*channeldb.OpenChannel, error)

	// FindOutgoingHTLCDeadline returns the deadline in absolute block
	// height for the specified outgoing HTLC. For an outgoing HTLC, its
	// deadline is defined by the timeout height of its corresponding
	// incoming HTLC - this is the expiry height the that remote peer can
	// spend his/her outgoing HTLC via the timeout path.
	FindOutgoingHTLCDeadline func(htlc channeldb.HTLC) fn.Option[int32]

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

	// ReportOutputAnchor is an anchor output on the commitment tx.
	ReportOutputAnchor
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

// resolverReport creates a resolve report using some of the information in the
// contract report.
func (c *ContractReport) resolverReport(spendTx *chainhash.Hash,
	resolverType channeldb.ResolverType,
	outcome channeldb.ResolverOutcome) *channeldb.ResolverReport {

	return &channeldb.ResolverReport{
		OutPoint:        c.Outpoint,
		Amount:          c.Amount,
		ResolverType:    resolverType,
		ResolverOutcome: outcome,
		SpendTxID:       spendTx,
	}
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

	// Embed the blockbeat consumer struct to get access to the method
	// `NotifyBlockProcessed` and the `BlockbeatChan`.
	chainio.BeatConsumer

	// startTimestamp is the time when this ChannelArbitrator was started.
	startTimestamp time.Time

	// log is a persistent log that the attendant will use to checkpoint
	// its next action, and the state of any unresolved contracts.
	log ArbitratorLog

	// activeHTLCs is the set of active incoming/outgoing HTLC's on all
	// currently valid commitment transactions.
	activeHTLCs map[HtlcSetKey]htlcSet

	// unmergedSet is used to update the activeHTLCs map in two callsites:
	// checkLocalChainActions and sweepAnchors. It contains the latest
	// updates from the link. It is not deleted from, its entries may be
	// replaced on subsequent calls to notifyContractUpdate.
	unmergedSet map[HtlcSetKey]htlcSet
	unmergedMtx sync.RWMutex

	// cfg contains all the functionality that the ChannelArbitrator requires
	// to do its duty.
	cfg ChannelArbitratorConfig

	// signalUpdates is a channel that any new live signals for the channel
	// we're watching over will be sent.
	signalUpdates chan *signalUpdateMsg

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

	// Create a new map for unmerged HTLC's as we will overwrite the values
	// and want to avoid modifying activeHTLCs directly. This soft copying
	// is done to ensure that activeHTLCs isn't reset as an empty map later
	// on.
	unmerged := make(map[HtlcSetKey]htlcSet)
	unmerged[LocalHtlcSet] = htlcSets[LocalHtlcSet]
	unmerged[RemoteHtlcSet] = htlcSets[RemoteHtlcSet]

	// If the pending set exists, write that as well.
	if _, ok := htlcSets[RemotePendingHtlcSet]; ok {
		unmerged[RemotePendingHtlcSet] = htlcSets[RemotePendingHtlcSet]
	}

	c := &ChannelArbitrator{
		log:              log,
		signalUpdates:    make(chan *signalUpdateMsg),
		resolutionSignal: make(chan struct{}),
		forceCloseReqs:   make(chan *forceCloseReq),
		activeHTLCs:      htlcSets,
		unmergedSet:      unmerged,
		cfg:              cfg,
		quit:             make(chan struct{}),
	}

	// Mount the block consumer.
	c.BeatConsumer = chainio.NewBeatConsumer(c.quit, c.Name())

	return c
}

// Compile-time check for the chainio.Consumer interface.
var _ chainio.Consumer = (*ChannelArbitrator)(nil)

// chanArbStartState contains the information from disk that we need to start
// up a channel arbitrator.
type chanArbStartState struct {
	currentState ArbitratorState
	commitSet    *CommitSet
}

// getStartState retrieves the information from disk that our channel arbitrator
// requires to start.
func (c *ChannelArbitrator) getStartState(tx kvdb.RTx) (*chanArbStartState,
	error) {

	// First, we'll read our last state from disk, so our internal state
	// machine can act accordingly.
	state, err := c.log.CurrentState(tx)
	if err != nil {
		return nil, err
	}

	// Next we'll fetch our confirmed commitment set. This will only exist
	// if the channel has been closed out on chain for modern nodes. For
	// older nodes, this won't be found at all, and will rely on the
	// existing written chain actions. Additionally, if this channel hasn't
	// logged any actions in the log, then this field won't be present.
	commitSet, err := c.log.FetchConfirmedCommitSet(tx)
	if err != nil && err != errNoCommitSet && err != errScopeBucketNoExist {
		return nil, err
	}

	return &chanArbStartState{
		currentState: state,
		commitSet:    commitSet,
	}, nil
}

// Start starts all the goroutines that the ChannelArbitrator needs to operate.
// If takes a start state, which will be looked up on disk if it is not
// provided.
func (c *ChannelArbitrator) Start(state *chanArbStartState,
	beat chainio.Blockbeat) error {

	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}
	c.startTimestamp = c.cfg.Clock.Now()

	// If the state passed in is nil, we look it up now.
	if state == nil {
		var err error
		state, err = c.getStartState(nil)
		if err != nil {
			return err
		}
	}

	log.Tracef("Starting ChannelArbitrator(%v), htlc_set=%v, state=%v",
		c.cfg.ChanPoint, lnutils.SpewLogClosure(c.activeHTLCs),
		state.currentState)

	// Set our state from our starting state.
	c.state = state.currentState

	// Get the starting height.
	bestHeight := beat.Height()

	c.wg.Add(1)
	go c.channelAttendant(bestHeight, state.commitSet)

	return nil
}

// progressStateMachineAfterRestart attempts to progress the state machine
// after a restart. This makes sure that if the state transition failed, we
// will try to progress the state machine again. Moreover it will relaunch
// resolvers if the channel is still in the pending close state and has not
// been fully resolved yet.
func (c *ChannelArbitrator) progressStateMachineAfterRestart(bestHeight int32,
	commitSet *CommitSet) error {

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

			log.Warnf("ChannelArbitrator(%v): detected stalled "+
				"state=%v for closed channel",
				c.cfg.ChanPoint, c.state)
		}

		triggerHeight = c.cfg.ClosingHeight
	}

	log.Infof("ChannelArbitrator(%v): starting state=%v, trigger=%v, "+
		"triggerHeight=%v", c.cfg.ChanPoint, c.state, trigger,
		triggerHeight)

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
		// receive a chain event from the chain watcher that the
		// commitment has been confirmed on chain, and before we
		// advance our state step, we call InsertConfirmedCommitSet.
		err := c.relaunchResolvers(commitSet, triggerHeight)
		if err != nil {
			return err
		}
	}

	return nil
}

// maybeAugmentTaprootResolvers will update the contract resolution information
// for taproot channels. This ensures that all the resolvers have the latest
// resolution, which may also include data such as the control block and tap
// tweaks.
func maybeAugmentTaprootResolvers(chanType channeldb.ChannelType,
	resolver ContractResolver,
	contractResolutions *ContractResolutions) {

	if !chanType.IsTaproot() {
		return
	}

	// The on disk resolutions contains all the ctrl block
	// information, so we'll set that now for the relevant
	// resolvers.
	switch r := resolver.(type) {
	case *commitSweepResolver:
		if contractResolutions.CommitResolution != nil {
			//nolint:ll
			r.commitResolution = *contractResolutions.CommitResolution
		}
	case *htlcOutgoingContestResolver:
		//nolint:ll
		htlcResolutions := contractResolutions.HtlcResolutions.OutgoingHTLCs
		for _, htlcRes := range htlcResolutions {
			htlcRes := htlcRes

			if r.htlcResolution.ClaimOutpoint ==
				htlcRes.ClaimOutpoint {

				r.htlcResolution = htlcRes
			}
		}

	case *htlcTimeoutResolver:
		//nolint:ll
		htlcResolutions := contractResolutions.HtlcResolutions.OutgoingHTLCs
		for _, htlcRes := range htlcResolutions {
			htlcRes := htlcRes

			if r.htlcResolution.ClaimOutpoint ==
				htlcRes.ClaimOutpoint {

				r.htlcResolution = htlcRes
			}
		}

	case *htlcIncomingContestResolver:
		//nolint:ll
		htlcResolutions := contractResolutions.HtlcResolutions.IncomingHTLCs
		for _, htlcRes := range htlcResolutions {
			htlcRes := htlcRes

			if r.htlcResolution.ClaimOutpoint ==
				htlcRes.ClaimOutpoint {

				r.htlcResolution = htlcRes
			}
		}
	case *htlcSuccessResolver:
		//nolint:ll
		htlcResolutions := contractResolutions.HtlcResolutions.IncomingHTLCs
		for _, htlcRes := range htlcResolutions {
			htlcRes := htlcRes

			if r.htlcResolution.ClaimOutpoint ==
				htlcRes.ClaimOutpoint {

				r.htlcResolution = htlcRes
			}
		}
	}
}

// relauchResolvers relaunches the set of resolvers for unresolved contracts in
// order to provide them with information that's not immediately available upon
// starting the ChannelArbitrator. This information should ideally be stored in
// the database, so this only serves as a intermediate work-around to prevent a
// migration.
func (c *ChannelArbitrator) relaunchResolvers(commitSet *CommitSet,
	heightHint uint32) error {

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
	if commitSet != nil && commitSet.ConfCommitKey.IsSome() {
		confCommitKey, err := commitSet.ConfCommitKey.UnwrapOrErr(
			fmt.Errorf("no commitKey available"),
		)
		if err != nil {
			return err
		}
		confirmedHTLCs = commitSet.HtlcSets[confCommitKey]
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

	// We'll also fetch the historical state of this channel, as it should
	// have been marked as closed by now, and supplement it to each resolver
	// such that we can properly resolve our pending contracts.
	var chanState *channeldb.OpenChannel
	chanState, err = c.cfg.FetchHistoricalChannel()
	switch {
	// If we don't find this channel, then it may be the case that it
	// was closed before we started to retain the final state
	// information for open channels.
	case err == channeldb.ErrNoHistoricalBucket:
		fallthrough
	case err == channeldb.ErrChannelNotFound:
		log.Warnf("ChannelArbitrator(%v): unable to fetch historical "+
			"state", c.cfg.ChanPoint)

	case err != nil:
		return err
	}

	log.Infof("ChannelArbitrator(%v): relaunching %v contract "+
		"resolvers", c.cfg.ChanPoint, len(unresolvedContracts))

	for i := range unresolvedContracts {
		resolver := unresolvedContracts[i]

		if chanState != nil {
			resolver.SupplementState(chanState)

			// For taproot channels, we'll need to also make sure
			// the control block information was set properly.
			maybeAugmentTaprootResolvers(
				chanState.ChanType, resolver,
				contractResolutions,
			)
		}

		unresolvedContracts[i] = resolver

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

		// If this is an outgoing HTLC, we will also need to supplement
		// the resolver with the expiry block height of its
		// corresponding incoming HTLC.
		if !htlc.Incoming {
			deadline := c.cfg.FindOutgoingHTLCDeadline(*htlc)
			htlcResolver.SupplementDeadline(deadline)
		}
	}

	// The anchor resolver is stateless and can always be re-instantiated.
	if contractResolutions.AnchorResolution != nil {
		anchorResolver := newAnchorResolver(
			contractResolutions.AnchorResolution.AnchorSignDescriptor,
			contractResolutions.AnchorResolution.CommitAnchor,
			heightHint, c.cfg.ChanPoint,
			ResolverConfig{
				ChannelArbitratorConfig: c.cfg,
			},
		)

		anchorResolver.SupplementState(chanState)

		unresolvedContracts = append(unresolvedContracts, anchorResolver)

		// TODO(roasbeef): this isn't re-launched?
	}

	c.resolveContracts(unresolvedContracts)

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
	// being confirmed. In this case the channel arbitrator will wait for
	// the BreachArbitrator to finish and then clean up gracefully.
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
		log.Debugf("ChannelArbitrator(%v): examining active HTLCs in "+
			"block %v, confCommitSet: %v", c.cfg.ChanPoint,
			triggerHeight, lnutils.LogClosure(confCommitSet.String))

		// As a new block has been connected to the end of the main
		// chain, we'll check to see if we need to make any on-chain
		// claims on behalf of the channel contract that we're
		// arbitrating for. If a commitment has confirmed, then we'll
		// use the set snapshot from the chain, otherwise we'll use our
		// current set.
		var (
			chainActions ChainActionMap
			err          error
		)

		// Normally if we force close the channel locally we will have
		// no confCommitSet. However when the remote commitment confirms
		// without us ever broadcasting our local commitment we need to
		// make sure we cancel all upstream HTLCs for outgoing dust
		// HTLCs as well hence we need to fetch the chain actions here
		// as well.
		if confCommitSet == nil {
			// Update the set of activeHTLCs so
			// checkLocalChainActions has an up-to-date view of the
			// commitments.
			c.updateActiveHTLCs()
			htlcs := c.activeHTLCs
			chainActions, err = c.checkLocalChainActions(
				triggerHeight, trigger, htlcs, false,
			)
			if err != nil {
				return StateDefault, nil, err
			}
		} else {
			chainActions, err = c.constructChainActions(
				confCommitSet, triggerHeight, trigger,
			)
			if err != nil {
				return StateDefault, nil, err
			}
		}

		// If there are no actions to be made, then we'll remain in the
		// default state. If this isn't a self initiated event (we're
		// checking due to a chain update), then we'll exit now.
		if len(chainActions) == 0 && trigger == chainTrigger {
			log.Debugf("ChannelArbitrator(%v): no actions for "+
				"chain trigger, terminating", c.cfg.ChanPoint)

			return StateDefault, closeTx, nil
		}

		// Otherwise, we'll log that we checked the HTLC actions as the
		// commitment transaction has already been broadcast.
		log.Tracef("ChannelArbitrator(%v): logging chain_actions=%v",
			c.cfg.ChanPoint, lnutils.SpewLogClosure(chainActions))

		// Cancel upstream HTLCs for all outgoing dust HTLCs available
		// either on the local or the remote/remote pending commitment
		// transaction.
		dustHTLCs := chainActions[HtlcFailDustAction]
		if len(dustHTLCs) > 0 {
			log.Debugf("ChannelArbitrator(%v): canceling %v dust "+
				"HTLCs backwards", c.cfg.ChanPoint,
				len(dustHTLCs))

			getIdx := func(htlc channeldb.HTLC) uint64 {
				return htlc.HtlcIndex
			}
			dustHTLCSet := fn.NewSet(fn.Map(dustHTLCs, getIdx)...)
			err = c.abandonForwards(dustHTLCSet)
			if err != nil {
				return StateError, closeTx, err
			}
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

		case breachCloseTrigger:
			nextContractState, err := c.checkLegacyBreach()
			if nextContractState == StateError {
				return nextContractState, nil, err
			}

			nextState = nextContractState
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

		case breachCloseTrigger:
			nextContractState, err := c.checkLegacyBreach()
			if nextContractState == StateError {
				log.Infof("ChannelArbitrator(%v): unable to "+
					"advance breach close resolution: %v",
					c.cfg.ChanPoint, nextContractState)
				return StateError, closeTx, err
			}

			log.Infof("ChannelArbitrator(%v): detected %s close "+
				"after closing channel, fast-forwarding to %s"+
				" to resolve contract", c.cfg.ChanPoint,
				trigger, nextContractState)

			return nextContractState, closeTx, nil

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
		forceCloseTx, err := c.cfg.Channel.ForceCloseChan()
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"force close: %v", c.cfg.ChanPoint, err)

			// We tried to force close (HTLC may be expiring from
			// our PoV, etc), but we think we've lost data. In this
			// case, we'll not force close, but terminate the state
			// machine here to wait to see what confirms on chain.
			if errors.Is(err, lnwallet.ErrForceCloseLocalDataLoss) {
				log.Error("ChannelArbitrator(%v): broadcast "+
					"failed due to local data loss, "+
					"waiting for on chain confimation...",
					c.cfg.ChanPoint)

				return StateBroadcastCommit, nil, nil
			}

			return StateError, closeTx, err
		}
		closeTx = forceCloseTx

		// Before publishing the transaction, we store it to the
		// database, such that we can re-publish later in case it
		// didn't propagate. We initiated the force close, so we
		// mark broadcast with local initiator set to true.
		err = c.cfg.MarkCommitmentBroadcasted(closeTx, lntypes.Local)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"mark commitment broadcasted: %v",
				c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}

		// With the close transaction in hand, broadcast the
		// transaction to the network, thereby entering the post
		// channel resolution state.
		log.Infof("Broadcasting force close transaction %v, "+
			"ChannelPoint(%v): %v", closeTx.TxHash(),
			c.cfg.ChanPoint, lnutils.SpewLogClosure(closeTx))

		// At this point, we'll now broadcast the commitment
		// transaction itself.
		label := labels.MakeLabel(
			labels.LabelTypeChannelClose, &c.cfg.ShortChanID,
		)
		if err := c.cfg.PublishTx(closeTx, label); err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to broadcast "+
				"close tx: %v", c.cfg.ChanPoint, err)

			// This makes sure we don't fail at startup if the
			// commitment transaction has too low fees to make it
			// into mempool. The rebroadcaster makes sure this
			// transaction is republished regularly until confirmed
			// or replaced.
			if !errors.Is(err, lnwallet.ErrDoubleSpend) &&
				!errors.Is(err, lnwallet.ErrMempoolFee) {

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

		// We are waiting for a commitment to be confirmed.
		case chainTrigger, userTrigger:
			// The commitment transaction has been broadcast, but it
			// doesn't necessarily need to be the commitment
			// transaction version that is going to be confirmed. To
			// be sure that any of those versions can be anchored
			// down, we now submit all anchor resolutions to the
			// sweeper. The sweeper will keep trying to sweep all of
			// them.
			//
			// Note that the sweeper is idempotent. If we ever
			// happen to end up at this point in the code again, no
			// harm is done by re-offering the anchors to the
			// sweeper.
			anchors, err := c.cfg.Channel.NewAnchorResolutions()
			if err != nil {
				return StateError, closeTx, err
			}

			err = c.sweepAnchors(anchors, triggerHeight)
			if err != nil {
				return StateError, closeTx, err
			}

			nextState = StateCommitmentBroadcasted

		// If this state advance was triggered by any of the
		// commitments being confirmed, then we'll jump to the state
		// where the contract has been closed.
		case localCloseTrigger, remoteCloseTrigger:
			nextState = StateContractClosed

		// If a coop close was confirmed, jump straight to the fully
		// resolved state.
		case coopCloseTrigger:
			nextState = StateFullyResolved

		case breachCloseTrigger:
			nextContractState, err := c.checkLegacyBreach()
			if nextContractState == StateError {
				return nextContractState, closeTx, err
			}

			nextState = nextContractState
		}

		log.Infof("ChannelArbitrator(%v): trigger %v moving from "+
			"state %v to %v", c.cfg.ChanPoint, trigger, c.state,
			nextState)

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
		// send to, then we're done here. We don't need to launch any
		// resolvers, and can go straight to our final state.
		if contractResolutions.IsEmpty() && confCommitSet.IsEmpty() {
			log.Infof("ChannelArbitrator(%v): contract "+
				"resolutions empty, marking channel as fully resolved!",
				c.cfg.ChanPoint)
			nextState = StateFullyResolved
			break
		}

		// First, we'll reconstruct a fresh set of chain actions as the
		// set of actions we need to act on may differ based on if it
		// was our commitment, or they're commitment that hit the chain.
		htlcActions, err := c.constructChainActions(
			confCommitSet, triggerHeight, trigger,
		)
		if err != nil {
			return StateError, closeTx, err
		}

		// In case its a breach transaction we fail back all upstream
		// HTLCs for their corresponding outgoing HTLCs on the remote
		// commitment set (remote and remote pending set).
		if contractResolutions.BreachResolution != nil {
			// cancelBreachedHTLCs is a set which holds HTLCs whose
			// corresponding incoming HTLCs will be failed back
			// because the peer broadcasted an old state.
			cancelBreachedHTLCs := fn.NewSet[uint64]()

			// We'll use the CommitSet, we'll fail back all
			// upstream HTLCs for their corresponding outgoing
			// HTLC that exist on either of the remote commitments.
			// The map is used to deduplicate any shared HTLC's.
			for htlcSetKey, htlcs := range confCommitSet.HtlcSets {
				if !htlcSetKey.IsRemote {
					continue
				}

				for _, htlc := range htlcs {
					// Only outgoing HTLCs have a
					// corresponding incoming HTLC.
					if htlc.Incoming {
						continue
					}

					cancelBreachedHTLCs.Add(htlc.HtlcIndex)
				}
			}

			err := c.abandonForwards(cancelBreachedHTLCs)
			if err != nil {
				return StateError, closeTx, err
			}
		} else {
			// If it's not a breach, we resolve all incoming dust
			// HTLCs immediately after the commitment is confirmed.
			err = c.failIncomingDust(
				htlcActions[HtlcIncomingDustFinalAction],
			)
			if err != nil {
				return StateError, closeTx, err
			}

			// We fail the upstream HTLCs for all remote pending
			// outgoing HTLCs as soon as the commitment is
			// confirmed. The upstream HTLCs for outgoing dust
			// HTLCs have already been resolved before we reach
			// this point.
			getIdx := func(htlc channeldb.HTLC) uint64 {
				return htlc.HtlcIndex
			}
			remoteDangling := fn.NewSet(fn.Map(
				htlcActions[HtlcFailDanglingAction], getIdx,
			)...)
			err := c.abandonForwards(remoteDangling)
			if err != nil {
				return StateError, closeTx, err
			}
		}

		// Now that we know we'll need to act, we'll process all the
		// resolvers, then create the structures we need to resolve all
		// outstanding contracts.
		resolvers, err := c.prepContractResolutions(
			contractResolutions, triggerHeight, htlcActions,
		)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): unable to "+
				"resolve contracts: %v", c.cfg.ChanPoint, err)
			return StateError, closeTx, err
		}

		log.Debugf("ChannelArbitrator(%v): inserting %v contract "+
			"resolvers", c.cfg.ChanPoint, len(resolvers))

		err = c.log.InsertUnresolvedContracts(nil, resolvers...)
		if err != nil {
			return StateError, closeTx, err
		}

		// Finally, we'll launch all the required contract resolvers.
		// Once they're all resolved, we're no longer needed.
		c.resolveContracts(resolvers)

		nextState = StateWaitingFullResolution

	// This is our terminal state. We'll keep returning this state until
	// all contracts are fully resolved.
	case StateWaitingFullResolution:
		log.Infof("ChannelArbitrator(%v): still awaiting contract "+
			"resolution", c.cfg.ChanPoint)

		unresolved, err := c.log.FetchUnresolvedContracts()
		if err != nil {
			return StateError, closeTx, err
		}

		// If we have no unresolved contracts, then we can move to the
		// final state.
		if len(unresolved) == 0 {
			nextState = StateFullyResolved
			break
		}

		// Otherwise we still have unresolved contracts, then we'll
		// stay alive to oversee their resolution.
		nextState = StateWaitingFullResolution

		// Add debug logs.
		for _, r := range unresolved {
			log.Debugf("ChannelArbitrator(%v): still have "+
				"unresolved contract: %T", c.cfg.ChanPoint, r)
		}

	// If we start as fully resolved, then we'll end as fully resolved.
	case StateFullyResolved:
		// To ensure that the state of the contract in persistent
		// storage is properly reflected, we'll mark the contract as
		// fully resolved now.
		nextState = StateFullyResolved

		log.Infof("ChannelPoint(%v) has been fully resolved "+
			"on-chain at height=%v", c.cfg.ChanPoint, triggerHeight)

		c.cfg.NotifyChannelResolved()
	}

	log.Tracef("ChannelArbitrator(%v): next_state=%v", c.cfg.ChanPoint,
		nextState)

	return nextState, closeTx, nil
}

// sweepAnchors offers all given anchor resolutions to the sweeper. It requests
// sweeping at the minimum fee rate. This fee rate can be upped manually by the
// user via the BumpFee rpc.
func (c *ChannelArbitrator) sweepAnchors(anchors *lnwallet.AnchorResolutions,
	heightHint uint32) error {

	// Update the set of activeHTLCs so that the sweeping routine has an
	// up-to-date view of the set of commitments.
	c.updateActiveHTLCs()

	// Prepare the sweeping requests for all possible versions of
	// commitments.
	sweepReqs, err := c.prepareAnchorSweeps(heightHint, anchors)
	if err != nil {
		return err
	}

	// Send out the sweeping requests to the sweeper.
	for _, req := range sweepReqs {
		_, err = c.cfg.Sweeper.SweepInput(req.input, req.params)
		if err != nil {
			return err
		}
	}

	return nil
}

// findCommitmentDeadlineAndValue finds the deadline (relative block height)
// for a commitment transaction by extracting the minimum CLTV from its HTLCs.
// From our PoV, the deadline delta is defined to be the smaller of,
//   - half of the least CLTV from outgoing HTLCs' corresponding incoming
//     HTLCs,  or,
//   - half of the least CLTV from incoming HTLCs if the preimage is available.
//
// We use half of the CTLV value to ensure that we have enough time to sweep
// the second-level HTLCs.
//
// It also finds the total value that are time-sensitive, which is the sum of
// all the outgoing HTLCs plus incoming HTLCs whose preimages are known. It
// then returns the value left after subtracting the budget used for sweeping
// the time-sensitive HTLCs.
//
// NOTE: when the deadline turns out to be 0 blocks, we will replace it with 1
// block because our fee estimator doesn't allow a 0 conf target. This also
// means we've left behind and should increase our fee to make the transaction
// confirmed asap.
func (c *ChannelArbitrator) findCommitmentDeadlineAndValue(heightHint uint32,
	htlcs htlcSet) (fn.Option[int32], btcutil.Amount, error) {

	deadlineMinHeight := uint32(math.MaxUint32)
	totalValue := btcutil.Amount(0)

	// First, iterate through the outgoingHTLCs to find the lowest CLTV
	// value.
	for _, htlc := range htlcs.outgoingHTLCs {
		// Skip if the HTLC is dust.
		if htlc.OutputIndex < 0 {
			log.Debugf("ChannelArbitrator(%v): skipped deadline "+
				"for dust htlc=%x",
				c.cfg.ChanPoint, htlc.RHash[:])

			continue
		}

		value := htlc.Amt.ToSatoshis()

		// Find the expiry height for this outgoing HTLC's incoming
		// HTLC.
		deadlineOpt := c.cfg.FindOutgoingHTLCDeadline(htlc)

		// The deadline is default to the current deadlineMinHeight,
		// and it's overwritten when it's not none.
		deadline := deadlineMinHeight
		deadlineOpt.WhenSome(func(d int32) {
			deadline = uint32(d)

			// We only consider the value is under protection when
			// it's time-sensitive.
			totalValue += value
		})

		if deadline < deadlineMinHeight {
			deadlineMinHeight = deadline

			log.Tracef("ChannelArbitrator(%v): outgoing HTLC has "+
				"deadline=%v, value=%v", c.cfg.ChanPoint,
				deadlineMinHeight, value)
		}
	}

	// Then going through the incomingHTLCs, and update the minHeight when
	// conditions met.
	for _, htlc := range htlcs.incomingHTLCs {
		// Skip if the HTLC is dust.
		if htlc.OutputIndex < 0 {
			log.Debugf("ChannelArbitrator(%v): skipped deadline "+
				"for dust htlc=%x",
				c.cfg.ChanPoint, htlc.RHash[:])

			continue
		}

		// Since it's an HTLC sent to us, check if we have preimage for
		// this HTLC.
		preimageAvailable, err := c.isPreimageAvailable(htlc.RHash)
		if err != nil {
			return fn.None[int32](), 0, err
		}

		if !preimageAvailable {
			continue
		}

		value := htlc.Amt.ToSatoshis()
		totalValue += value

		if htlc.RefundTimeout < deadlineMinHeight {
			deadlineMinHeight = htlc.RefundTimeout

			log.Tracef("ChannelArbitrator(%v): incoming HTLC has "+
				"deadline=%v, amt=%v", c.cfg.ChanPoint,
				deadlineMinHeight, value)
		}
	}

	// Calculate the deadline. There are two cases to be handled here,
	//   - when the deadlineMinHeight never gets updated, which could
	//     happen when we have no outgoing HTLCs, and, for incoming HTLCs,
	//       * either we have none, or,
	//       * none of the HTLCs are preimageAvailable.
	//   - when our deadlineMinHeight is no greater than the heightHint,
	//     which means we are behind our schedule.
	var deadline uint32
	switch {
	// When we couldn't find a deadline height from our HTLCs, we will fall
	// back to the default value as there's no time pressure here.
	case deadlineMinHeight == math.MaxUint32:
		return fn.None[int32](), 0, nil

	// When the deadline is passed, we will fall back to the smallest conf
	// target (1 block).
	case deadlineMinHeight <= heightHint:
		log.Warnf("ChannelArbitrator(%v): deadline is passed with "+
			"deadlineMinHeight=%d, heightHint=%d",
			c.cfg.ChanPoint, deadlineMinHeight, heightHint)
		deadline = 1

	// Use half of the deadline delta, and leave the other half to be used
	// to sweep the HTLCs.
	default:
		deadline = (deadlineMinHeight - heightHint) / 2
	}

	// Calculate the value left after subtracting the budget used for
	// sweeping the time-sensitive HTLCs.
	valueLeft := totalValue - calculateBudget(
		totalValue, c.cfg.Budget.DeadlineHTLCRatio,
		c.cfg.Budget.DeadlineHTLC,
	)

	log.Debugf("ChannelArbitrator(%v): calculated valueLeft=%v, "+
		"deadline=%d, using deadlineMinHeight=%d, heightHint=%d",
		c.cfg.ChanPoint, valueLeft, deadline, deadlineMinHeight,
		heightHint)

	return fn.Some(int32(deadline)), valueLeft, nil
}

// resolveContracts updates the activeResolvers list and starts to resolve each
// contract concurrently, and launches them.
func (c *ChannelArbitrator) resolveContracts(resolvers []ContractResolver) {
	c.activeResolversLock.Lock()
	c.activeResolvers = resolvers
	c.activeResolversLock.Unlock()

	// Launch all resolvers.
	c.launchResolvers()

	for _, contract := range resolvers {
		c.wg.Add(1)
		go c.resolveContract(contract)
	}
}

// launchResolvers launches all the active resolvers concurrently.
func (c *ChannelArbitrator) launchResolvers() {
	c.activeResolversLock.Lock()
	resolvers := c.activeResolvers
	c.activeResolversLock.Unlock()

	// errChans is a map of channels that will be used to receive errors
	// returned from launching the resolvers.
	errChans := make(map[ContractResolver]chan error, len(resolvers))

	// Launch each resolver in goroutines.
	for _, r := range resolvers {
		// If the contract is already resolved, there's no need to
		// launch it again.
		if r.IsResolved() {
			log.Debugf("ChannelArbitrator(%v): skipping resolver "+
				"%T as it's already resolved", c.cfg.ChanPoint,
				r)

			continue
		}

		// Create a signal chan.
		errChan := make(chan error, 1)
		errChans[r] = errChan

		go func() {
			err := r.Launch()
			errChan <- err
		}()
	}

	// Wait for all resolvers to finish launching.
	for r, errChan := range errChans {
		select {
		case err := <-errChan:
			if err == nil {
				continue
			}

			log.Errorf("ChannelArbitrator(%v): unable to launch "+
				"contract resolver(%T): %v", c.cfg.ChanPoint, r,
				err)

		case <-c.quit:
			log.Debugf("ChannelArbitrator quit signal received, " +
				"exit launchResolvers")

			return
		}
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
		log.Debugf("ChannelArbitrator(%v): attempting state step with "+
			"trigger=%v from state=%v at height=%v",
			c.cfg.ChanPoint, trigger, priorState, triggerHeight)

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
			log.Debugf("ChannelArbitrator(%v): terminating at "+
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

	// HtlcFailDustAction indicates that we should fail the upstream HTLC
	// for an outgoing dust HTLC immediately (even before the commitment
	// transaction is confirmed) because it has no output on the commitment
	// transaction. This also includes remote pending outgoing dust HTLCs.
	HtlcFailDustAction = 3

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

	// HtlcIncomingDustFinalAction indicates that we should mark an incoming
	// dust htlc as final because it can't be claimed on-chain.
	HtlcIncomingDustFinalAction = 6

	// HtlcFailDanglingAction indicates that we should fail the upstream
	// HTLC for an outgoing HTLC immediately after the commitment
	// transaction has confirmed because it has no corresponding output on
	// the commitment transaction. This category does NOT include any dust
	// HTLCs which are mapped in the "HtlcFailDustAction" category.
	HtlcFailDanglingAction = 7
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

	case HtlcFailDustAction:
		return "HtlcFailDustAction"

	case HtlcOutgoingWatchAction:
		return "HtlcOutgoingWatchAction"

	case HtlcIncomingWatchAction:
		return "HtlcIncomingWatchAction"

	case HtlcIncomingDustFinalAction:
		return "HtlcIncomingDustFinalAction"

	case HtlcFailDanglingAction:
		return "HtlcFailDanglingAction"

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
func (c *ChannelArbitrator) shouldGoOnChain(htlc channeldb.HTLC,
	broadcastDelta, currentHeight uint32) bool {

	// We'll calculate the broadcast cut off for this HTLC. This is the
	// height that (based on our current fee estimation) we should
	// broadcast in order to ensure the commitment transaction is confirmed
	// before the HTLC fully expires.
	broadcastCutOff := htlc.RefundTimeout - broadcastDelta

	log.Tracef("ChannelArbitrator(%v): examining outgoing contract: "+
		"expiry=%v, cutoff=%v, height=%v", c.cfg.ChanPoint, htlc.RefundTimeout,
		broadcastCutOff, currentHeight)

	// TODO(roasbeef): take into account default HTLC delta, don't need to
	// broadcast immediately
	//  * can then batch with SINGLE | ANYONECANPAY

	// We should on-chain for this HTLC, iff we're within out broadcast
	// cutoff window.
	if currentHeight < broadcastCutOff {
		return false
	}

	// In case of incoming htlc we should go to chain.
	if htlc.Incoming {
		return true
	}

	// For htlcs that are result of our initiated payments we give some grace
	// period before force closing the channel. During this time we expect
	// both nodes to connect and give a chance to the other node to send its
	// updates and cancel the htlc.
	// This shouldn't add any security risk as there is no incoming htlc to
	// fulfill at this case and the expectation is that when the channel is
	// active the other node will send update_fail_htlc to remove the htlc
	// without closing the channel. It is up to the user to force close the
	// channel if the peer misbehaves and doesn't send the update_fail_htlc.
	// It is useful when this node is most of the time not online and is
	// likely to miss the time slot where the htlc may be cancelled.
	isForwarded := c.cfg.IsForwardedHTLC(c.cfg.ShortChanID, htlc.HtlcIndex)
	upTime := c.cfg.Clock.Now().Sub(c.startTimestamp)
	return isForwarded || upTime > c.cfg.PaymentsExpirationGracePeriod
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
		//
		// TODO(yy): If there's no corresponding incoming HTLC, it
		// means we are the first hop, hence the payer. This is a
		// tricky case - unlike a forwarding hop, we don't have an
		// incoming HTLC that will time out, which means as long as we
		// can learn the preimage, we can settle the invoice (before it
		// expires?).
		toChain := c.shouldGoOnChain(
			htlc, c.cfg.OutgoingBroadcastDelta, height,
		)

		if toChain {
			// Convert to int64 in case of overflow.
			remainingBlocks := int64(htlc.RefundTimeout) -
				int64(height)

			log.Infof("ChannelArbitrator(%v): go to chain for "+
				"outgoing htlc %x: timeout=%v, amount=%v, "+
				"blocks_until_expiry=%v, broadcast_delta=%v",
				c.cfg.ChanPoint, htlc.RHash[:],
				htlc.RefundTimeout, htlc.Amt, remainingBlocks,
				c.cfg.OutgoingBroadcastDelta,
			)
		}

		haveChainActions = haveChainActions || toChain
	}

	for _, htlc := range htlcs.incomingHTLCs {
		// We'll need to go on-chain to pull an incoming HTLC iff we
		// know the pre-image and it's close to timing out. We need to
		// ensure that we claim the funds that are rightfully ours
		// on-chain.
		preimageAvailable, err := c.isPreimageAvailable(htlc.RHash)
		if err != nil {
			return nil, err
		}

		if !preimageAvailable {
			continue
		}

		toChain := c.shouldGoOnChain(
			htlc, c.cfg.IncomingBroadcastDelta, height,
		)

		if toChain {
			// Convert to int64 in case of overflow.
			remainingBlocks := int64(htlc.RefundTimeout) -
				int64(height)

			log.Infof("ChannelArbitrator(%v): go to chain for "+
				"incoming htlc %x: timeout=%v, amount=%v, "+
				"blocks_until_expiry=%v, broadcast_delta=%v",
				c.cfg.ChanPoint, htlc.RHash[:],
				htlc.RefundTimeout, htlc.Amt, remainingBlocks,
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

			actionMap[HtlcFailDustAction] = append(
				actionMap[HtlcFailDustAction], htlc,
			)

		// If we don't need to immediately act on this HTLC, then we'll
		// mark it still "live". After we broadcast, we'll monitor it
		// until the HTLC times out to see if we can also redeem it
		// on-chain.
		case !c.shouldGoOnChain(htlc, c.cfg.OutgoingBroadcastDelta,
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

			actionMap[HtlcIncomingDustFinalAction] = append(
				actionMap[HtlcIncomingDustFinalAction], htlc,
			)

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
	invoice, err := c.cfg.Registry.LookupInvoice(context.Background(), hash)
	switch {
	case err == nil:
	case errors.Is(err, invoices.ErrInvoiceNotFound) ||
		errors.Is(err, invoices.ErrNoInvoicesCreated):

		return false, nil
	default:
		return false, err
	}

	preimageAvailable = invoice.Terms.PaymentPreimage != nil

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
		goToChain := c.shouldGoOnChain(htlc, c.cfg.OutgoingBroadcastDelta,
			height,
		)

		// If we don't need to go to chain, and no commitments have
		// been confirmed, then we can move on. Otherwise, if
		// commitments have been confirmed, then we need to cancel back
		// *all* of the pending remote HTLCS.
		if !goToChain && !commitsConfirmed {
			continue
		}

		preimageAvailable, err := c.isPreimageAvailable(htlc.RHash)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): failed to query "+
				"preimage for dangling htlc=%x from remote "+
				"commitments diff", c.cfg.ChanPoint,
				htlc.RHash[:])

			continue
		}

		if preimageAvailable {
			continue
		}

		// Dust htlcs can be canceled back even before the commitment
		// transaction confirms. Dust htlcs are not enforceable onchain.
		// If another version of the commit tx would confirm we either
		// gain or lose those dust amounts but there is no other way
		// than cancelling the incoming back because we will never learn
		// the preimage.
		if htlc.OutputIndex < 0 {
			log.Infof("ChannelArbitrator(%v): fail dangling dust "+
				"htlc=%x from local/remote commitments diff",
				c.cfg.ChanPoint, htlc.RHash[:])

			actionMap[HtlcFailDustAction] = append(
				actionMap[HtlcFailDustAction], htlc,
			)

			continue
		}

		log.Infof("ChannelArbitrator(%v): fail dangling htlc=%x from "+
			"local/remote commitments diff",
			c.cfg.ChanPoint, htlc.RHash[:])

		actionMap[HtlcFailDanglingAction] = append(
			actionMap[HtlcFailDanglingAction], htlc,
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

	// With these actions computed, we'll now check the diff of the HTLCs on
	// the commitments, and cancel back any that are on the pending but not
	// the non-pending.
	remoteDiffActions := c.checkRemoteDiffActions(
		activeHTLCs, pendingConf,
	)

	// Finally, we'll merge all the chain actions and the final set of
	// chain actions.
	remoteCommitActions.Merge(remoteDiffActions)
	return remoteCommitActions, nil
}

// checkRemoteDiffActions checks the set difference of the HTLCs on the remote
// confirmed commit and remote pending commit for HTLCS that we need to cancel
// back. If we find any HTLCs on the remote pending but not the remote, then
// we'll mark them to be failed immediately.
func (c *ChannelArbitrator) checkRemoteDiffActions(
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
	// remote pending commitment to be failed asap.
	actionMap := make(ChainActionMap)
	for _, htlc := range danglingHTLCs.outgoingHTLCs {
		if _, ok := remoteHtlcs[htlc.HtlcIndex]; ok {
			continue
		}

		preimageAvailable, err := c.isPreimageAvailable(htlc.RHash)
		if err != nil {
			log.Errorf("ChannelArbitrator(%v): failed to query "+
				"preimage for dangling htlc=%x from remote "+
				"commitments diff", c.cfg.ChanPoint,
				htlc.RHash[:])

			continue
		}

		if preimageAvailable {
			continue
		}

		// Dust HTLCs on the remote commitment can be failed back.
		if htlc.OutputIndex < 0 {
			log.Infof("ChannelArbitrator(%v): fail dangling dust "+
				"htlc=%x from remote commitments diff",
				c.cfg.ChanPoint, htlc.RHash[:])

			actionMap[HtlcFailDustAction] = append(
				actionMap[HtlcFailDustAction], htlc,
			)

			continue
		}

		actionMap[HtlcFailDanglingAction] = append(
			actionMap[HtlcFailDanglingAction], htlc,
		)

		log.Infof("ChannelArbitrator(%v): fail dangling htlc=%x from "+
			"remote commitments diff",
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
	if confCommitSet == nil || confCommitSet.ConfCommitKey.IsNone() {
		return c.log.FetchChainActions()
	}

	// Otherwise, we have the full commitment set written to disk, and can
	// proceed as normal.
	htlcSets := confCommitSet.toActiveHTLCSets()
	confCommitKey, err := confCommitSet.ConfCommitKey.UnwrapOrErr(
		fmt.Errorf("no commitKey available"),
	)
	if err != nil {
		return nil, err
	}

	switch confCommitKey {
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

// prepContractResolutions is called either in the case that we decide we need
// to go to chain, or the remote party goes to chain. Given a set of actions we
// need to take for each HTLC, this method will return a set of contract
// resolvers that will resolve the contracts on-chain if needed, and also a set
// of packets to send to the htlcswitch in order to ensure all incoming HTLC's
// are properly resolved.
func (c *ChannelArbitrator) prepContractResolutions(
	contractResolutions *ContractResolutions, height uint32,
	htlcActions ChainActionMap) ([]ContractResolver, error) {

	// We'll also fetch the historical state of this channel, as it should
	// have been marked as closed by now, and supplement it to each resolver
	// such that we can properly resolve our pending contracts.
	var chanState *channeldb.OpenChannel
	chanState, err := c.cfg.FetchHistoricalChannel()
	switch {
	// If we don't find this channel, then it may be the case that it
	// was closed before we started to retain the final state
	// information for open channels.
	case err == channeldb.ErrNoHistoricalBucket:
		fallthrough
	case err == channeldb.ErrChannelNotFound:
		log.Warnf("ChannelArbitrator(%v): unable to fetch historical "+
			"state", c.cfg.ChanPoint)

	case err != nil:
		return nil, err
	}

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
		Checkpoint: func(res ContractResolver,
			reports ...*channeldb.ResolverReport) error {

			return c.log.InsertUnresolvedContracts(reports, res)
		},
	}

	commitHash := contractResolutions.CommitHash

	var htlcResolvers []ContractResolver

	// We instantiate an anchor resolver if the commitment tx has an
	// anchor.
	if contractResolutions.AnchorResolution != nil {
		anchorResolver := newAnchorResolver(
			contractResolutions.AnchorResolution.AnchorSignDescriptor,
			contractResolutions.AnchorResolution.CommitAnchor,
			height, c.cfg.ChanPoint, resolverCfg,
		)
		anchorResolver.SupplementState(chanState)

		htlcResolvers = append(htlcResolvers, anchorResolver)
	}

	// If this is a breach close, we'll create a breach resolver, determine
	// the htlc's to fail back, and exit. This is done because the other
	// steps taken for non-breach-closes do not matter for breach-closes.
	if contractResolutions.BreachResolution != nil {
		breachResolver := newBreachResolver(resolverCfg)
		htlcResolvers = append(htlcResolvers, breachResolver)

		return htlcResolvers, nil
	}

	// For each HTLC, we'll either act immediately, meaning we'll instantly
	// fail the HTLC, or we'll act only once the transaction has been
	// confirmed, in which case we'll need an HTLC resolver.
	for htlcAction, htlcs := range htlcActions {
		switch htlcAction {
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
				if chanState != nil {
					resolver.SupplementState(chanState)
				}
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
				if chanState != nil {
					resolver.SupplementState(chanState)
				}

				// For outgoing HTLCs, we will also need to
				// supplement the resolver with the expiry
				// block height of its corresponding incoming
				// HTLC.
				deadline := c.cfg.FindOutgoingHTLCDeadline(htlc)
				resolver.SupplementDeadline(deadline)

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
				if chanState != nil {
					resolver.SupplementState(chanState)
				}
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
					log.Errorf("ChannelArbitrator(%v) "+
						"unable to find outgoing "+
						"resolution: %v",
						c.cfg.ChanPoint, htlcOp)

					continue
				}

				resolver := newOutgoingContestResolver(
					resolution, height, htlc, resolverCfg,
				)
				if chanState != nil {
					resolver.SupplementState(chanState)
				}

				// For outgoing HTLCs, we will also need to
				// supplement the resolver with the expiry
				// block height of its corresponding incoming
				// HTLC.
				deadline := c.cfg.FindOutgoingHTLCDeadline(htlc)
				resolver.SupplementDeadline(deadline)

				htlcResolvers = append(htlcResolvers, resolver)
			}
		}
	}

	// If this is was an unilateral closure, then we'll also create a
	// resolver to sweep our commitment output (but only if it wasn't
	// trimmed).
	if contractResolutions.CommitResolution != nil {
		resolver := newCommitSweepResolver(
			*contractResolutions.CommitResolution, height,
			c.cfg.ChanPoint, resolverCfg,
		)
		if chanState != nil {
			resolver.SupplementState(chanState)
		}
		htlcResolvers = append(htlcResolvers, resolver)
	}

	return htlcResolvers, nil
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

	log.Tracef("ChannelArbitrator(%v): attempting to resolve %T",
		c.cfg.ChanPoint, currentContract)

	// Until the contract is fully resolved, we'll continue to iteratively
	// resolve the contract one step at a time.
	for !currentContract.IsResolved() {
		log.Tracef("ChannelArbitrator(%v): contract %T not yet "+
			"resolved", c.cfg.ChanPoint, currentContract)

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
			// resolved in this step. So we'll do a contract swap
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

				// Launch the new contract.
				err = currentContract.Launch()
				if err != nil {
					log.Errorf("Failed to launch %T: %v",
						currentContract, err)
				}

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

// notifyContractUpdate updates the ChannelArbitrator's unmerged mappings such
// that it can later be merged with activeHTLCs when calling
// checkLocalChainActions or sweepAnchors. These are the only two places that
// activeHTLCs is used.
func (c *ChannelArbitrator) notifyContractUpdate(upd *ContractUpdate) {
	c.unmergedMtx.Lock()
	defer c.unmergedMtx.Unlock()

	// Update the mapping.
	c.unmergedSet[upd.HtlcKey] = newHtlcSet(upd.Htlcs)

	log.Tracef("ChannelArbitrator(%v): fresh set of htlcs=%v",
		c.cfg.ChanPoint, lnutils.SpewLogClosure(upd))
}

// updateActiveHTLCs merges the unmerged set of HTLCs from the link with
// activeHTLCs.
func (c *ChannelArbitrator) updateActiveHTLCs() {
	c.unmergedMtx.RLock()
	defer c.unmergedMtx.RUnlock()

	// Update the mapping.
	c.activeHTLCs[LocalHtlcSet] = c.unmergedSet[LocalHtlcSet]
	c.activeHTLCs[RemoteHtlcSet] = c.unmergedSet[RemoteHtlcSet]

	// If the pending set exists, update that as well.
	if _, ok := c.unmergedSet[RemotePendingHtlcSet]; ok {
		pendingSet := c.unmergedSet[RemotePendingHtlcSet]
		c.activeHTLCs[RemotePendingHtlcSet] = pendingSet
	}
}

// channelAttendant is the primary goroutine that acts at the judicial
// arbitrator between our channel state, the remote channel peer, and the
// blockchain (Our judge). This goroutine will ensure that we faithfully execute
// all clauses of our contract in the case that we need to go on-chain for a
// dispute. Currently, two such conditions warrant our intervention: when an
// outgoing HTLC is about to timeout, and when we know the pre-image for an
// incoming HTLC, but it hasn't yet been settled off-chain. In these cases,
// we'll: broadcast our commitment, cancel/settle any HTLC's backwards after
// sufficient confirmation, and finally send our set of outputs to the UTXO
// Nursery for incubation, and ultimate sweeping.
//
// NOTE: This MUST be run as a goroutine.
func (c *ChannelArbitrator) channelAttendant(bestHeight int32,
	commitSet *CommitSet) {

	// TODO(roasbeef): tell top chain arb we're done
	defer func() {
		c.wg.Done()
	}()

	err := c.progressStateMachineAfterRestart(bestHeight, commitSet)
	if err != nil {
		// In case of an error, we return early but we do not shutdown
		// LND, because there might be other channels that still can be
		// resolved and we don't want to interfere with that.
		// We continue to run the channel attendant in case the channel
		// closes via other means for example the remote pary force
		// closes the channel. So we log the error and continue.
		log.Errorf("Unable to progress state machine after "+
			"restart: %v", err)
	}

	for {
		select {

		// A new block has arrived, we'll examine all the active HTLC's
		// to see if any of them have expired, and also update our
		// track of the best current height.
		case beat := <-c.BlockbeatChan:
			bestHeight = beat.Height()

			log.Debugf("ChannelArbitrator(%v): received new "+
				"block: height=%v, processing...",
				c.cfg.ChanPoint, bestHeight)

			err := c.handleBlockbeat(beat)
			if err != nil {
				log.Errorf("Handle block=%v got err: %v",
					bestHeight, err)
			}

			// If as a result of this trigger, the contract is
			// fully resolved, then well exit.
			if c.state == StateFullyResolved {
				return
			}

		// A new signal update was just sent. This indicates that the
		// channel under watch is now live, and may modify its internal
		// state, so we'll get the most up to date signals to we can
		// properly do our job.
		case signalUpdate := <-c.signalUpdates:
			log.Tracef("ChannelArbitrator(%v): got new signal "+
				"update!", c.cfg.ChanPoint)

			// We'll update the ShortChannelID.
			c.cfg.ShortChanID = signalUpdate.newSignals.ShortChanID

			// Now that the signal has been updated, we'll now
			// close the done channel to signal to the caller we've
			// registered the new ShortChannelID.
			close(signalUpdate.doneChan)

		// We've cooperatively closed the channel, so we're no longer
		// needed. We'll mark the channel as resolved and exit.
		case closeInfo := <-c.cfg.ChainEvents.CooperativeClosure:
			err := c.handleCoopCloseEvent(closeInfo)
			if err != nil {
				log.Errorf("Failed to handle coop close: %v",
					err)

				return
			}

		// We have broadcasted our commitment, and it is now confirmed
		// on-chain.
		case closeInfo := <-c.cfg.ChainEvents.LocalUnilateralClosure:
			if c.state != StateCommitmentBroadcasted {
				log.Errorf("ChannelArbitrator(%v): unexpected "+
					"local on-chain channel close",
					c.cfg.ChanPoint)
			}

			err := c.handleLocalForceCloseEvent(closeInfo)
			if err != nil {
				log.Errorf("Failed to handle local force "+
					"close: %v", err)

				return
			}

		// The remote party has broadcast the commitment on-chain.
		// We'll examine our state to determine if we need to act at
		// all.
		case uniClosure := <-c.cfg.ChainEvents.RemoteUnilateralClosure:
			err := c.handleRemoteForceCloseEvent(uniClosure)
			if err != nil {
				log.Errorf("Failed to handle remote force "+
					"close: %v", err)

				return
			}

		// The remote has breached the channel. As this is handled by
		// the ChainWatcher and BreachArbitrator, we don't have to do
		// anything in particular, so just advance our state and
		// gracefully exit.
		case breachInfo := <-c.cfg.ChainEvents.ContractBreach:
			err := c.handleContractBreach(breachInfo)
			if err != nil {
				log.Errorf("Failed to handle contract breach: "+
					"%v", err)

				return
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
			log.Infof("ChannelArbitrator(%v): received force "+
				"close request", c.cfg.ChanPoint)

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

// handleBlockbeat processes a newly received blockbeat by advancing the
// arbitrator's internal state using the received block height.
func (c *ChannelArbitrator) handleBlockbeat(beat chainio.Blockbeat) error {
	// Notify we've processed the block.
	defer c.NotifyBlockProcessed(beat, nil)

	// If the state is StateContractClosed, StateWaitingFullResolution, or
	// StateFullyResolved, there's no need to read the close event channel
	// since the arbitrator can only get to this state after processing a
	// previous close event and launched all its resolvers.
	if c.state.IsContractClosed() {
		log.Infof("ChannelArbitrator(%v): skipping reading close "+
			"events in state=%v", c.cfg.ChanPoint, c.state)

		// Launch all active resolvers when a new blockbeat is
		// received, even when the contract is closed, we still need
		// this as the resolvers may transform into new ones. For
		// already launched resolvers this will be NOOP as they track
		// their own `launched` states.
		c.launchResolvers()

		return nil
	}

	// Perform a non-blocking read on the close events in case the channel
	// is closed in this blockbeat.
	c.receiveAndProcessCloseEvent()

	// Try to advance the state if we are in StateDefault.
	if c.state == StateDefault {
		// Now that a new block has arrived, we'll attempt to advance
		// our state forward.
		_, _, err := c.advanceState(
			uint32(beat.Height()), chainTrigger, nil,
		)
		if err != nil {
			return fmt.Errorf("unable to advance state: %w", err)
		}
	}

	// Launch all active resolvers when a new blockbeat is received.
	c.launchResolvers()

	return nil
}

// receiveAndProcessCloseEvent does a non-blocking read on all the channel
// close event channels. If an event is received, it will be further processed.
func (c *ChannelArbitrator) receiveAndProcessCloseEvent() {
	select {
	// Received a coop close event, we now mark the channel as resolved and
	// exit.
	case closeInfo := <-c.cfg.ChainEvents.CooperativeClosure:
		err := c.handleCoopCloseEvent(closeInfo)
		if err != nil {
			log.Errorf("Failed to handle coop close: %v", err)
			return
		}

	// We have broadcast our commitment, and it is now confirmed onchain.
	case closeInfo := <-c.cfg.ChainEvents.LocalUnilateralClosure:
		if c.state != StateCommitmentBroadcasted {
			log.Errorf("ChannelArbitrator(%v): unexpected "+
				"local on-chain channel close", c.cfg.ChanPoint)
		}

		err := c.handleLocalForceCloseEvent(closeInfo)
		if err != nil {
			log.Errorf("Failed to handle local force close: %v",
				err)

			return
		}

	// The remote party has broadcast the commitment. We'll examine our
	// state to determine if we need to act at all.
	case uniClosure := <-c.cfg.ChainEvents.RemoteUnilateralClosure:
		err := c.handleRemoteForceCloseEvent(uniClosure)
		if err != nil {
			log.Errorf("Failed to handle remote force close: %v",
				err)

			return
		}

	// The remote has breached the channel! We now launch the breach
	// contract resolvers.
	case breachInfo := <-c.cfg.ChainEvents.ContractBreach:
		err := c.handleContractBreach(breachInfo)
		if err != nil {
			log.Errorf("Failed to handle contract breach: %v", err)
			return
		}

	default:
		log.Infof("ChannelArbitrator(%v) no close event",
			c.cfg.ChanPoint)
	}
}

// Name returns a human-readable string for this subsystem.
//
// NOTE: Part of chainio.Consumer interface.
func (c *ChannelArbitrator) Name() string {
	return fmt.Sprintf("ChannelArbitrator(%v)", c.cfg.ChanPoint)
}

// checkLegacyBreach returns StateFullyResolved if the channel was closed with
// a breach transaction before the channel arbitrator launched its own breach
// resolver. StateContractClosed is returned if this is a modern breach close
// with a breach resolver. StateError is returned if the log lookup failed.
func (c *ChannelArbitrator) checkLegacyBreach() (ArbitratorState, error) {
	// A previous version of the channel arbitrator would make the breach
	// close skip to StateFullyResolved. If there are no contract
	// resolutions in the bolt arbitrator log, then this is an older breach
	// close. Otherwise, if there are resolutions, the state should advance
	// to StateContractClosed.
	_, err := c.log.FetchContractResolutions()
	if err == errNoResolutions {
		// This is an older breach close still in the database.
		return StateFullyResolved, nil
	} else if err != nil {
		return StateError, err
	}

	// This is a modern breach close with resolvers.
	return StateContractClosed, nil
}

// sweepRequest wraps the arguments used when calling `SweepInput`.
type sweepRequest struct {
	// input is the input to be swept.
	input input.Input

	// params holds the sweeping parameters.
	params sweep.Params
}

// createSweepRequest creates an anchor sweeping request for a particular
// version (local/remote/remote pending) of the commitment.
func (c *ChannelArbitrator) createSweepRequest(
	anchor *lnwallet.AnchorResolution, htlcs htlcSet, anchorPath string,
	heightHint uint32) (sweepRequest, error) {

	// Use the chan id as the exclusive group. This prevents any of the
	// anchors from being batched together.
	exclusiveGroup := c.cfg.ShortChanID.ToUint64()

	// Find the deadline for this specific anchor.
	deadline, value, err := c.findCommitmentDeadlineAndValue(
		heightHint, htlcs,
	)
	if err != nil {
		return sweepRequest{}, err
	}

	// If we cannot find a deadline, it means there's no HTLCs at stake,
	// which means we can relax our anchor sweeping conditions as we don't
	// have any time sensitive outputs to sweep. However we need to
	// register the anchor output with the sweeper so we are later able to
	// bump the close fee.
	if deadline.IsNone() {
		log.Infof("ChannelArbitrator(%v): no HTLCs at stake, "+
			"sweeping anchor with default deadline",
			c.cfg.ChanPoint)
	}

	witnessType := input.CommitmentAnchor

	// For taproot channels, we need to use the proper witness type.
	if txscript.IsPayToTaproot(
		anchor.AnchorSignDescriptor.Output.PkScript,
	) {

		witnessType = input.TaprootAnchorSweepSpend
	}

	// Prepare anchor output for sweeping.
	anchorInput := input.MakeBaseInput(
		&anchor.CommitAnchor,
		witnessType,
		&anchor.AnchorSignDescriptor,
		heightHint,
		&input.TxInfo{
			Fee:    anchor.CommitFee,
			Weight: anchor.CommitWeight,
		},
	)

	// If we have a deadline, we'll use it to calculate the deadline
	// height, otherwise default to none.
	deadlineDesc := "None"
	deadlineHeight := fn.MapOption(func(d int32) int32 {
		deadlineDesc = fmt.Sprintf("%d", d)

		return d + int32(heightHint)
	})(deadline)

	// Calculate the budget based on the value under protection, which is
	// the sum of all HTLCs on this commitment subtracted by their budgets.
	// The anchor output in itself has a small output value of 330 sats so
	// we also include it in the budget to pay for the cpfp transaction.
	budget := calculateBudget(
		value, c.cfg.Budget.AnchorCPFPRatio, c.cfg.Budget.AnchorCPFP,
	) + AnchorOutputValue

	log.Infof("ChannelArbitrator(%v): offering anchor from %s commitment "+
		"%v to sweeper with deadline=%v, budget=%v", c.cfg.ChanPoint,
		anchorPath, anchor.CommitAnchor, deadlineDesc, budget)

	// Sweep anchor output with a confirmation target fee preference.
	// Because this is a cpfp-operation, the anchor will only be attempted
	// to sweep when the current fee estimate for the confirmation target
	// exceeds the commit fee rate.
	return sweepRequest{
		input: &anchorInput,
		params: sweep.Params{
			ExclusiveGroup: &exclusiveGroup,
			Budget:         budget,
			DeadlineHeight: deadlineHeight,
		},
	}, nil
}

// prepareAnchorSweeps creates a list of requests to be used by the sweeper for
// all possible commitment versions.
func (c *ChannelArbitrator) prepareAnchorSweeps(heightHint uint32,
	anchors *lnwallet.AnchorResolutions) ([]sweepRequest, error) {

	// requests holds all the possible anchor sweep requests. We can have
	// up to 3 different versions of commitments (local/remote/remote
	// dangling) to be CPFPed by the anchors.
	requests := make([]sweepRequest, 0, 3)

	// remotePendingReq holds the request for sweeping the anchor output on
	// the remote pending commitment. It's only set when there's an actual
	// pending remote commitment and it's used to decide whether we need to
	// update the fee budget when sweeping the anchor output on the local
	// commitment.
	remotePendingReq := fn.None[sweepRequest]()

	// First we check on the remote pending commitment and optionally
	// create an anchor sweeping request.
	htlcs, ok := c.activeHTLCs[RemotePendingHtlcSet]
	if ok && anchors.RemotePending != nil {
		req, err := c.createSweepRequest(
			anchors.RemotePending, htlcs, "remote pending",
			heightHint,
		)
		if err != nil {
			return nil, err
		}

		// Save the request.
		requests = append(requests, req)

		// Set the optional variable.
		remotePendingReq = fn.Some(req)
	}

	// Check the local commitment and optionally create an anchor sweeping
	// request. The params used in this request will be influenced by the
	// anchor sweeping request made from the pending remote commitment.
	htlcs, ok = c.activeHTLCs[LocalHtlcSet]
	if ok && anchors.Local != nil {
		req, err := c.createSweepRequest(
			anchors.Local, htlcs, "local", heightHint,
		)
		if err != nil {
			return nil, err
		}

		// If there's an anchor sweeping request from the pending
		// remote commitment, we will compare its budget against the
		// budget used here and choose the params that has a larger
		// budget. The deadline when choosing the remote pending budget
		// instead of the local one will always be earlier or equal to
		// the local deadline because outgoing HTLCs are resolved on
		// the local commitment first before they are removed from the
		// remote one.
		remotePendingReq.WhenSome(func(s sweepRequest) {
			if s.params.Budget <= req.params.Budget {
				return
			}

			log.Infof("ChannelArbitrator(%v): replaced local "+
				"anchor(%v) sweep params with pending remote "+
				"anchor sweep params, \nold:[%v], \nnew:[%v]",
				c.cfg.ChanPoint, anchors.Local.CommitAnchor,
				req.params, s.params)

			req.params = s.params
		})

		// Save the request.
		requests = append(requests, req)
	}

	// Check the remote commitment and create an anchor sweeping request if
	// needed.
	htlcs, ok = c.activeHTLCs[RemoteHtlcSet]
	if ok && anchors.Remote != nil {
		req, err := c.createSweepRequest(
			anchors.Remote, htlcs, "remote", heightHint,
		)
		if err != nil {
			return nil, err
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// failIncomingDust resolves the incoming dust HTLCs because they do not have
// an output on the commitment transaction and cannot be resolved onchain. We
// mark them as failed here.
func (c *ChannelArbitrator) failIncomingDust(
	incomingDustHTLCs []channeldb.HTLC) error {

	for _, htlc := range incomingDustHTLCs {
		if !htlc.Incoming || htlc.OutputIndex >= 0 {
			return fmt.Errorf("htlc with index %v is not incoming "+
				"dust", htlc.OutputIndex)
		}

		key := models.CircuitKey{
			ChanID: c.cfg.ShortChanID,
			HtlcID: htlc.HtlcIndex,
		}

		// Mark this dust htlc as final failed.
		chainArbCfg := c.cfg.ChainArbitratorConfig
		err := chainArbCfg.PutFinalHtlcOutcome(
			key.ChanID, key.HtlcID, false,
		)
		if err != nil {
			return err
		}

		// Send notification.
		chainArbCfg.HtlcNotifier.NotifyFinalHtlcEvent(
			key,
			channeldb.FinalHtlcInfo{
				Settled:  false,
				Offchain: false,
			},
		)
	}

	return nil
}

// abandonForwards cancels back the incoming HTLCs for their corresponding
// outgoing HTLCs. We use a set here to avoid sending duplicate failure messages
// for the same HTLC. This also needs to be done for locally initiated outgoing
// HTLCs they are special cased in the switch.
func (c *ChannelArbitrator) abandonForwards(htlcs fn.Set[uint64]) error {
	log.Debugf("ChannelArbitrator(%v): cancelling back %v incoming "+
		"HTLC(s)", c.cfg.ChanPoint,
		len(htlcs))

	msgsToSend := make([]ResolutionMsg, 0, len(htlcs))
	failureMsg := &lnwire.FailPermanentChannelFailure{}

	for idx := range htlcs {
		failMsg := ResolutionMsg{
			SourceChan: c.cfg.ShortChanID,
			HtlcIndex:  idx,
			Failure:    failureMsg,
		}

		msgsToSend = append(msgsToSend, failMsg)
	}

	// Send the msges to the switch, if there are any.
	if len(msgsToSend) == 0 {
		return nil
	}

	log.Debugf("ChannelArbitrator(%v): sending resolution message=%v",
		c.cfg.ChanPoint, lnutils.SpewLogClosure(msgsToSend))

	err := c.cfg.DeliverResolutionMsg(msgsToSend...)
	if err != nil {
		log.Errorf("Unable to send resolution msges to switch: %v", err)
		return err
	}

	return nil
}

// handleCoopCloseEvent takes a coop close event from ChainEvents, marks the
// channel as closed and advances the state.
func (c *ChannelArbitrator) handleCoopCloseEvent(
	closeInfo *CooperativeCloseInfo) error {

	log.Infof("ChannelArbitrator(%v) marking channel cooperatively closed "+
		"at height %v", c.cfg.ChanPoint, closeInfo.CloseHeight)

	err := c.cfg.MarkChannelClosed(
		closeInfo.ChannelCloseSummary,
		channeldb.ChanStatusCoopBroadcasted,
	)
	if err != nil {
		return fmt.Errorf("unable to mark channel closed: %w", err)
	}

	// We'll now advance our state machine until it reaches a terminal
	// state, and the channel is marked resolved.
	_, _, err = c.advanceState(closeInfo.CloseHeight, coopCloseTrigger, nil)
	if err != nil {
		log.Errorf("Unable to advance state: %v", err)
	}

	return nil
}

// handleLocalForceCloseEvent takes a local force close event from ChainEvents,
// saves the contract resolutions to disk, mark the channel as closed and
// advance the state.
func (c *ChannelArbitrator) handleLocalForceCloseEvent(
	closeInfo *LocalUnilateralCloseInfo) error {

	closeTx := closeInfo.CloseTx

	resolutions, err := closeInfo.ContractResolutions.
		UnwrapOrErr(
			fmt.Errorf("resolutions not found"),
		)
	if err != nil {
		return fmt.Errorf("unable to get resolutions: %w", err)
	}

	// We make sure that the htlc resolutions are present
	// otherwise we would panic dereferencing the pointer.
	//
	// TODO(ziggie): Refactor ContractResolutions to use
	// options.
	if resolutions.HtlcResolutions == nil {
		return fmt.Errorf("htlc resolutions is nil")
	}

	log.Infof("ChannelArbitrator(%v): local force close tx=%v confirmed",
		c.cfg.ChanPoint, closeTx.TxHash())

	contractRes := &ContractResolutions{
		CommitHash:       closeTx.TxHash(),
		CommitResolution: resolutions.CommitResolution,
		HtlcResolutions:  *resolutions.HtlcResolutions,
		AnchorResolution: resolutions.AnchorResolution,
	}

	// When processing a unilateral close event, we'll transition to the
	// ContractClosed state. We'll log out the set of resolutions such that
	// they are available to fetch in that state, we'll also write the
	// commit set so we can reconstruct our chain actions on restart.
	err = c.log.LogContractResolutions(contractRes)
	if err != nil {
		return fmt.Errorf("unable to write resolutions: %w", err)
	}

	err = c.log.InsertConfirmedCommitSet(&closeInfo.CommitSet)
	if err != nil {
		return fmt.Errorf("unable to write commit set: %w", err)
	}

	// After the set of resolutions are successfully logged, we can safely
	// close the channel. After this succeeds we won't be getting chain
	// events anymore, so we must make sure we can recover on restart after
	// it is marked closed. If the next state transition fails, we'll start
	// up in the prior state again, and we won't be longer getting chain
	// events. In this case we must manually re-trigger the state
	// transition into StateContractClosed based on the close status of the
	// channel.
	err = c.cfg.MarkChannelClosed(
		closeInfo.ChannelCloseSummary,
		channeldb.ChanStatusLocalCloseInitiator,
	)
	if err != nil {
		return fmt.Errorf("unable to mark channel closed: %w", err)
	}

	// We'll now advance our state machine until it reaches a terminal
	// state.
	_, _, err = c.advanceState(
		uint32(closeInfo.SpendingHeight),
		localCloseTrigger, &closeInfo.CommitSet,
	)
	if err != nil {
		log.Errorf("Unable to advance state: %v", err)
	}

	return nil
}

// handleRemoteForceCloseEvent takes a remote force close event from
// ChainEvents, saves the contract resolutions to disk, mark the channel as
// closed and advance the state.
func (c *ChannelArbitrator) handleRemoteForceCloseEvent(
	closeInfo *RemoteUnilateralCloseInfo) error {

	log.Infof("ChannelArbitrator(%v): remote party has force closed "+
		"channel at height %v", c.cfg.ChanPoint,
		closeInfo.SpendingHeight)

	// If we don't have a self output, and there are no active HTLC's, then
	// we can immediately mark the contract as fully resolved and exit.
	contractRes := &ContractResolutions{
		CommitHash:       *closeInfo.SpenderTxHash,
		CommitResolution: closeInfo.CommitResolution,
		HtlcResolutions:  *closeInfo.HtlcResolutions,
		AnchorResolution: closeInfo.AnchorResolution,
	}

	// When processing a unilateral close event, we'll transition to the
	// ContractClosed state. We'll log out the set of resolutions such that
	// they are available to fetch in that state, we'll also write the
	// commit set so we can reconstruct our chain actions on restart.
	err := c.log.LogContractResolutions(contractRes)
	if err != nil {
		return fmt.Errorf("unable to write resolutions: %w", err)
	}

	err = c.log.InsertConfirmedCommitSet(&closeInfo.CommitSet)
	if err != nil {
		return fmt.Errorf("unable to write commit set: %w", err)
	}

	// After the set of resolutions are successfully logged, we can safely
	// close the channel. After this succeeds we won't be getting chain
	// events anymore, so we must make sure we can recover on restart after
	// it is marked closed. If the next state transition fails, we'll start
	// up in the prior state again, and we won't be longer getting chain
	// events. In this case we must manually re-trigger the state
	// transition into StateContractClosed based on the close status of the
	// channel.
	closeSummary := &closeInfo.ChannelCloseSummary
	err = c.cfg.MarkChannelClosed(
		closeSummary,
		channeldb.ChanStatusRemoteCloseInitiator,
	)
	if err != nil {
		return fmt.Errorf("unable to mark channel closed: %w", err)
	}

	// We'll now advance our state machine until it reaches a terminal
	// state.
	_, _, err = c.advanceState(
		uint32(closeInfo.SpendingHeight),
		remoteCloseTrigger, &closeInfo.CommitSet,
	)
	if err != nil {
		log.Errorf("Unable to advance state: %v", err)
	}

	return nil
}

// handleContractBreach takes a breach close event from ChainEvents, saves the
// contract resolutions to disk, mark the channel as closed and advance the
// state.
func (c *ChannelArbitrator) handleContractBreach(
	breachInfo *BreachCloseInfo) error {

	closeSummary := &breachInfo.CloseSummary

	log.Infof("ChannelArbitrator(%v): remote party has breached channel "+
		"at height %v!", c.cfg.ChanPoint, closeSummary.CloseHeight)

	// In the breach case, we'll only have anchor and breach resolutions.
	contractRes := &ContractResolutions{
		CommitHash:       breachInfo.CommitHash,
		BreachResolution: breachInfo.BreachResolution,
		AnchorResolution: breachInfo.AnchorResolution,
	}

	// We'll transition to the ContractClosed state and log the set of
	// resolutions such that they can be turned into resolvers later on.
	// We'll also insert the CommitSet of the latest set of commitments.
	err := c.log.LogContractResolutions(contractRes)
	if err != nil {
		return fmt.Errorf("unable to write resolutions: %w", err)
	}

	err = c.log.InsertConfirmedCommitSet(&breachInfo.CommitSet)
	if err != nil {
		return fmt.Errorf("unable to write commit set: %w", err)
	}

	// The channel is finally marked pending closed here as the
	// BreachArbitrator and channel arbitrator have persisted the relevant
	// states.
	err = c.cfg.MarkChannelClosed(
		closeSummary, channeldb.ChanStatusRemoteCloseInitiator,
	)
	if err != nil {
		return fmt.Errorf("unable to mark channel closed: %w", err)
	}

	log.Infof("Breached channel=%v marked pending-closed",
		breachInfo.BreachResolution.FundingOutPoint)

	// We'll advance our state machine until it reaches a terminal state.
	_, _, err = c.advanceState(
		closeSummary.CloseHeight, breachCloseTrigger,
		&breachInfo.CommitSet,
	)
	if err != nil {
		log.Errorf("Unable to advance state: %v", err)
	}

	return nil
}
