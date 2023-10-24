package sweep

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// DefaultFeeRateBucketSize is the default size of fee rate buckets
	// we'll use when clustering inputs into buckets with similar fee rates
	// within the UtxoSweeper.
	//
	// Given a minimum relay fee rate of 1 sat/vbyte, a multiplier of 10
	// would result in the following fee rate buckets up to the maximum fee
	// rate:
	//
	//   #1: min = 1 sat/vbyte, max = 10 sat/vbyte
	//   #2: min = 11 sat/vbyte, max = 20 sat/vbyte...
	DefaultFeeRateBucketSize = 10
)

var (
	// ErrRemoteSpend is returned in case an output that we try to sweep is
	// confirmed in a tx of the remote party.
	ErrRemoteSpend = errors.New("remote party swept utxo")

	// ErrTooManyAttempts is returned in case sweeping an output has failed
	// for the configured max number of attempts.
	ErrTooManyAttempts = errors.New("sweep failed after max attempts")

	// ErrFeePreferenceTooLow is returned when the fee preference gives a
	// fee rate that's below the relay fee rate.
	ErrFeePreferenceTooLow = errors.New("fee preference too low")

	// ErrExclusiveGroupSpend is returned in case a different input of the
	// same exclusive group was spent.
	ErrExclusiveGroupSpend = errors.New("other member of exclusive group " +
		"was spent")

	// ErrSweeperShuttingDown is an error returned when a client attempts to
	// make a request to the UtxoSweeper, but it is unable to handle it as
	// it is/has already been stopped.
	ErrSweeperShuttingDown = errors.New("utxo sweeper shutting down")

	// DefaultMaxSweepAttempts specifies the default maximum number of times
	// an input is included in a publish attempt before giving up and
	// returning an error to the caller.
	DefaultMaxSweepAttempts = 10
)

// Params contains the parameters that control the sweeping process.
type Params struct {
	// Fee is the fee preference of the client who requested the input to be
	// swept. If a confirmation target is specified, then we'll map it into
	// a fee rate whenever we attempt to cluster inputs for a sweep.
	Fee FeePreference

	// Force indicates whether the input should be swept regardless of
	// whether it is economical to do so.
	Force bool

	// ExclusiveGroup is an identifier that, if set, prevents other inputs
	// with the same identifier from being batched together.
	ExclusiveGroup *uint64
}

// ParamsUpdate contains a new set of parameters to update a pending sweep with.
type ParamsUpdate struct {
	// Fee is the fee preference of the client who requested the input to be
	// swept. If a confirmation target is specified, then we'll map it into
	// a fee rate whenever we attempt to cluster inputs for a sweep.
	Fee FeePreference

	// Force indicates whether the input should be swept regardless of
	// whether it is economical to do so.
	Force bool
}

// String returns a human readable interpretation of the sweep parameters.
func (p Params) String() string {
	if p.ExclusiveGroup != nil {
		return fmt.Sprintf("fee=%v, force=%v, exclusive_group=%v",
			p.Fee, p.Force, *p.ExclusiveGroup)
	}

	return fmt.Sprintf("fee=%v, force=%v, exclusive_group=nil",
		p.Fee, p.Force)
}

// pendingInput is created when an input reaches the main loop for the first
// time. It wraps the input and tracks all relevant state that is needed for
// sweeping.
type pendingInput struct {
	input.Input

	// listeners is a list of channels over which the final outcome of the
	// sweep needs to be broadcasted.
	listeners []chan Result

	// ntfnRegCancel is populated with a function that cancels the chain
	// notifier spend registration.
	ntfnRegCancel func()

	// minPublishHeight indicates the minimum block height at which this
	// input may be (re)published.
	minPublishHeight int32

	// publishAttempts records the number of attempts that have already been
	// made to sweep this tx.
	publishAttempts int

	// params contains the parameters that control the sweeping process.
	params Params

	// lastFeeRate is the most recent fee rate used for this input within a
	// transaction broadcast to the network.
	lastFeeRate chainfee.SatPerKWeight
}

// parameters returns the sweep parameters for this input.
//
// NOTE: Part of the txInput interface.
func (p *pendingInput) parameters() Params {
	return p.params
}

// pendingInputs is a type alias for a set of pending inputs.
type pendingInputs = map[wire.OutPoint]*pendingInput

// inputCluster is a helper struct to gather a set of pending inputs that should
// be swept with the specified fee rate.
type inputCluster struct {
	lockTime     *uint32
	sweepFeeRate chainfee.SatPerKWeight
	inputs       pendingInputs
}

// pendingSweepsReq is an internal message we'll use to represent an external
// caller's intent to retrieve all of the pending inputs the UtxoSweeper is
// attempting to sweep.
type pendingSweepsReq struct {
	respChan chan map[wire.OutPoint]*PendingInput
	errChan  chan error
}

// PendingInput contains information about an input that is currently being
// swept by the UtxoSweeper.
type PendingInput struct {
	// OutPoint is the identify outpoint of the input being swept.
	OutPoint wire.OutPoint

	// WitnessType is the witness type of the input being swept.
	WitnessType input.WitnessType

	// Amount is the amount of the input being swept.
	Amount btcutil.Amount

	// LastFeeRate is the most recent fee rate used for the input being
	// swept within a transaction broadcast to the network.
	LastFeeRate chainfee.SatPerKWeight

	// BroadcastAttempts is the number of attempts we've made to sweept the
	// input.
	BroadcastAttempts int

	// NextBroadcastHeight is the next height of the chain at which we'll
	// attempt to broadcast a transaction sweeping the input.
	NextBroadcastHeight uint32

	// Params contains the sweep parameters for this pending request.
	Params Params
}

// updateReq is an internal message we'll use to represent an external caller's
// intent to update the sweep parameters of a given input.
type updateReq struct {
	input        wire.OutPoint
	params       ParamsUpdate
	responseChan chan *updateResp
}

// updateResp is an internal message we'll use to hand off the response of a
// updateReq from the UtxoSweeper's main event loop back to the caller.
type updateResp struct {
	resultChan chan Result
	err        error
}

// UtxoSweeper is responsible for sweeping outputs back into the wallet
type UtxoSweeper struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *UtxoSweeperConfig

	newInputs chan *sweepInputMessage
	spendChan chan *chainntnfs.SpendDetail

	// pendingSweepsReq is a channel that will be sent requests by external
	// callers in order to retrieve the set of pending inputs the
	// UtxoSweeper is attempting to sweep.
	pendingSweepsReqs chan *pendingSweepsReq

	// updateReqs is a channel that will be sent requests by external
	// callers who wish to bump the fee rate of a given input.
	updateReqs chan *updateReq

	// pendingInputs is the total set of inputs the UtxoSweeper has been
	// requested to sweep.
	pendingInputs pendingInputs

	testSpendChan chan wire.OutPoint

	currentOutputScript []byte

	relayFeeRate chainfee.SatPerKWeight

	quit chan struct{}
	wg   sync.WaitGroup

	// currentHeight is the best known height of the main chain. This is
	// updated whenever a new block epoch is received.
	currentHeight int32
}

// UtxoSweeperConfig contains dependencies of UtxoSweeper.
type UtxoSweeperConfig struct {
	// GenSweepScript generates a P2WKH script belonging to the wallet where
	// funds can be swept.
	GenSweepScript func() ([]byte, error)

	// FeeEstimator is used when crafting sweep transactions to estimate
	// the necessary fee relative to the expected size of the sweep
	// transaction.
	FeeEstimator chainfee.Estimator

	// Wallet contains the wallet functions that sweeper requires.
	Wallet Wallet

	// TickerDuration is used to create a channel that will be sent on when
	// a certain time window has passed. During this time window, new
	// inputs can still be added to the sweep tx that is about to be
	// generated.
	TickerDuration time.Duration

	// Notifier is an instance of a chain notifier we'll use to watch for
	// certain on-chain events.
	Notifier chainntnfs.ChainNotifier

	// Store stores the published sweeper txes.
	Store SweeperStore

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer input.Signer

	// MaxInputsPerTx specifies the default maximum number of inputs allowed
	// in a single sweep tx. If more need to be swept, multiple txes are
	// created and published.
	MaxInputsPerTx int

	// MaxSweepAttempts specifies the maximum number of times an input is
	// included in a publish attempt before giving up and returning an error
	// to the caller.
	MaxSweepAttempts int

	// NextAttemptDeltaFunc returns given the number of already attempted
	// sweeps, how many blocks to wait before retrying to sweep.
	NextAttemptDeltaFunc func(int) int32

	// MaxFeeRate is the maximum fee rate allowed within the
	// UtxoSweeper.
	MaxFeeRate chainfee.SatPerVByte

	// FeeRateBucketSize is the default size of fee rate buckets we'll use
	// when clustering inputs into buckets with similar fee rates within the
	// UtxoSweeper.
	//
	// Given a minimum relay fee rate of 1 sat/vbyte, a fee rate bucket size
	// of 10 would result in the following fee rate buckets up to the
	// maximum fee rate:
	//
	//   #1: min = 1 sat/vbyte, max (exclusive) = 11 sat/vbyte
	//   #2: min = 11 sat/vbyte, max (exclusive) = 21 sat/vbyte...
	FeeRateBucketSize int
}

// Result is the struct that is pushed through the result channel. Callers can
// use this to be informed of the final sweep result. In case of a remote
// spend, Err will be ErrRemoteSpend.
type Result struct {
	// Err is the final result of the sweep. It is nil when the input is
	// swept successfully by us. ErrRemoteSpend is returned when another
	// party took the input.
	Err error

	// Tx is the transaction that spent the input.
	Tx *wire.MsgTx
}

// sweepInputMessage structs are used in the internal channel between the
// SweepInput call and the sweeper main loop.
type sweepInputMessage struct {
	input      input.Input
	params     Params
	resultChan chan Result
}

// New returns a new Sweeper instance.
func New(cfg *UtxoSweeperConfig) *UtxoSweeper {
	return &UtxoSweeper{
		cfg:               cfg,
		newInputs:         make(chan *sweepInputMessage),
		spendChan:         make(chan *chainntnfs.SpendDetail),
		updateReqs:        make(chan *updateReq),
		pendingSweepsReqs: make(chan *pendingSweepsReq),
		quit:              make(chan struct{}),
		pendingInputs:     make(pendingInputs),
	}
}

// Start starts the process of constructing and publish sweep txes.
func (s *UtxoSweeper) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	log.Info("Sweeper starting")

	// Retrieve relay fee for dust limit calculation. Assume that this will
	// not change from here on.
	s.relayFeeRate = s.cfg.FeeEstimator.RelayFeePerKW()

	// We need to register for block epochs and retry sweeping every block.
	// We should get a notification with the current best block immediately
	// if we don't provide any epoch. We'll wait for that in the collector.
	blockEpochs, err := s.cfg.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %w", err)
	}

	// Start sweeper main loop.
	s.wg.Add(1)
	go func() {
		defer blockEpochs.Cancel()
		defer s.wg.Done()

		s.collector(blockEpochs.Epochs)

		// The collector exited and won't longer handle incoming
		// requests. This can happen on shutdown, when the block
		// notifier shuts down before the sweeper and its clients. In
		// order to not deadlock the clients waiting for their requests
		// being handled, we handle them here and immediately return an
		// error. When the sweeper finally is shut down we can exit as
		// the clients will be notified.
		for {
			select {
			case inp := <-s.newInputs:
				inp.resultChan <- Result{
					Err: ErrSweeperShuttingDown,
				}

			case req := <-s.pendingSweepsReqs:
				req.errChan <- ErrSweeperShuttingDown

			case req := <-s.updateReqs:
				req.responseChan <- &updateResp{
					err: ErrSweeperShuttingDown,
				}

			case <-s.quit:
				return
			}
		}
	}()

	return nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
func (s *UtxoSweeper) RelayFeePerKW() chainfee.SatPerKWeight {
	return s.relayFeeRate
}

// Stop stops sweeper from listening to block epochs and constructing sweep
// txes.
func (s *UtxoSweeper) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Info("Sweeper shutting down...")
	defer log.Debug("Sweeper shutdown complete")

	close(s.quit)
	s.wg.Wait()

	return nil
}

// SweepInput sweeps inputs back into the wallet. The inputs will be batched and
// swept after the batch time window ends. A custom fee preference can be
// provided to determine what fee rate should be used for the input. Note that
// the input may not always be swept with this exact value, as its possible for
// it to be batched under the same transaction with other similar fee rate
// inputs.
//
// NOTE: Extreme care needs to be taken that input isn't changed externally.
// Because it is an interface and we don't know what is exactly behind it, we
// cannot make a local copy in sweeper.
func (s *UtxoSweeper) SweepInput(input input.Input,
	params Params) (chan Result, error) {

	if input == nil || input.OutPoint() == nil || input.SignDesc() == nil {
		return nil, errors.New("nil input received")
	}

	// Ensure the client provided a sane fee preference.
	_, err := params.Fee.Estimate(
		s.cfg.FeeEstimator, s.cfg.MaxFeeRate.FeePerKWeight(),
	)
	if err != nil {
		return nil, err
	}

	absoluteTimeLock, _ := input.RequiredLockTime()
	log.Infof("Sweep request received: out_point=%v, witness_type=%v, "+
		"relative_time_lock=%v, absolute_time_lock=%v, amount=%v, "+
		"parent=(%v), params=(%v)", input.OutPoint(),
		input.WitnessType(), input.BlocksToMaturity(), absoluteTimeLock,
		btcutil.Amount(input.SignDesc().Output.Value),
		input.UnconfParent(), params)

	sweeperInput := &sweepInputMessage{
		input:      input,
		params:     params,
		resultChan: make(chan Result, 1),
	}

	// Deliver input to the main event loop.
	select {
	case s.newInputs <- sweeperInput:
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}

	return sweeperInput.resultChan, nil
}

// removeConflictSweepDescendants removes any transactions from the wallet that
// spend outputs included in the passed outpoint set. This needs to be done in
// cases where we're not the only ones that can sweep an output, but there may
// exist unconfirmed spends that spend outputs created by a sweep transaction.
// The most common case for this is when someone sweeps our anchor outputs
// after 16 blocks. Moreover this is also needed for wallets which use neutrino
// as a backend when a channel is force closed and anchor cpfp txns are
// created to bump the initial commitment transaction. In this case an anchor
// cpfp is broadcasted for up to 3 commitment transactions (local,
// remote-dangling, remote). Using neutrino all of those transactions will be
// accepted (the commitment tx will be different in all of those cases) and have
// to be removed as soon as one of them confirmes (they do have the same
// ExclusiveGroup). For neutrino backends the corresponding BIP 157 serving full
// nodes do not signal invalid transactions anymore.
func (s *UtxoSweeper) removeConflictSweepDescendants(
	outpoints map[wire.OutPoint]struct{}) error {

	// Obtain all the past sweeps that we've done so far. We'll need these
	// to ensure that if the spendingTx spends any of the same inputs, then
	// we remove any transaction that may be spending those inputs from the
	// wallet.
	//
	// TODO(roasbeef): can be last sweep here if we remove anything confirmed
	// from the store?
	pastSweepHashes, err := s.cfg.Store.ListSweeps()
	if err != nil {
		return err
	}

	// We'll now go through each past transaction we published during this
	// epoch and cross reference the spent inputs. If there're any inputs
	// in common with the inputs the spendingTx spent, then we'll remove
	// those.
	//
	// TODO(roasbeef): need to start to remove all transaction hashes after
	// every N blocks (assumed point of no return)
	for _, sweepHash := range pastSweepHashes {
		sweepTx, err := s.cfg.Wallet.FetchTx(sweepHash)
		if err != nil {
			return err
		}

		// Transaction wasn't found in the wallet, may have already
		// been replaced/removed.
		if sweepTx == nil {
			// If it was removed, then we'll play it safe and mark
			// it as no longer need to be rebroadcasted.
			s.cfg.Wallet.CancelRebroadcast(sweepHash)
			continue
		}

		// Check to see if this past sweep transaction spent any of the
		// same inputs as spendingTx.
		var isConflicting bool
		for _, txIn := range sweepTx.TxIn {
			if _, ok := outpoints[txIn.PreviousOutPoint]; ok {
				isConflicting = true
				break
			}
		}

		if !isConflicting {
			continue
		}

		// If it is conflicting, then we'll signal the wallet to remove
		// all the transactions that are descendants of outputs created
		// by the sweepTx and the sweepTx itself.
		log.Debugf("Removing sweep txid=%v from wallet: %v",
			sweepTx.TxHash(), spew.Sdump(sweepTx))

		err = s.cfg.Wallet.RemoveDescendants(sweepTx)
		if err != nil {
			log.Warnf("Unable to remove descendants: %v", err)
		}

		// If this transaction was conflicting, then we'll stop
		// rebroadcasting it in the background.
		s.cfg.Wallet.CancelRebroadcast(sweepHash)
	}

	return nil
}

// collector is the sweeper main loop. It processes new inputs, spend
// notifications and counts down to publication of the sweep tx.
func (s *UtxoSweeper) collector(blockEpochs <-chan *chainntnfs.BlockEpoch) {
	// We registered for the block epochs with a nil request. The notifier
	// should send us the current best block immediately. So we need to wait
	// for it here because we need to know the current best height.
	select {
	case bestBlock := <-blockEpochs:
		s.currentHeight = bestBlock.Height

	case <-s.quit:
		return
	}

	// Create a ticker based on the config duration.
	ticker := time.NewTicker(s.cfg.TickerDuration)
	defer ticker.Stop()

	log.Debugf("Sweep ticker started")

	for {
		select {
		// A new inputs is offered to the sweeper. We check to see if
		// we are already trying to sweep this input and if not, set up
		// a listener to spend and schedule a sweep.
		case input := <-s.newInputs:
			s.handleNewInput(input)

		// A spend of one of our inputs is detected. Signal sweep
		// results to the caller(s).
		case spend := <-s.spendChan:
			s.handleInputSpent(spend)

		// A new external request has been received to retrieve all of
		// the inputs we're currently attempting to sweep.
		case req := <-s.pendingSweepsReqs:
			req.respChan <- s.handlePendingSweepsReq(req)

		// A new external request has been received to bump the fee rate
		// of a given input.
		case req := <-s.updateReqs:
			resultChan, err := s.handleUpdateReq(req)
			req.responseChan <- &updateResp{
				resultChan: resultChan,
				err:        err,
			}

		// The timer expires and we are going to (re)sweep.
		case <-ticker.C:
			log.Debugf("Sweep ticker ticks, attempt sweeping...")
			s.handleSweep()

		// A new block comes in, update the bestHeight.
		case epoch, ok := <-blockEpochs:
			if !ok {
				return
			}

			s.currentHeight = epoch.Height

			log.Debugf("New block: height=%v, sha=%v",
				epoch.Height, epoch.Hash)

		case <-s.quit:
			return
		}
	}
}

// removeExclusiveGroup removes all inputs in the given exclusive group. This
// function is called when one of the exclusive group inputs has been spent. The
// other inputs won't ever be spendable and can be removed. This also prevents
// them from being part of future sweep transactions that would fail. In
// addition sweep transactions of those inputs will be removed from the wallet.
func (s *UtxoSweeper) removeExclusiveGroup(group uint64) {
	for outpoint, input := range s.pendingInputs {
		outpoint := outpoint

		// Skip inputs that aren't exclusive.
		if input.params.ExclusiveGroup == nil {
			continue
		}

		// Skip inputs from other exclusive groups.
		if *input.params.ExclusiveGroup != group {
			continue
		}

		// Signal result channels.
		s.signalAndRemove(&outpoint, Result{
			Err: ErrExclusiveGroupSpend,
		})

		// Remove all unconfirmed transactions from the wallet which
		// spend the passed outpoint of the same exclusive group.
		outpoints := map[wire.OutPoint]struct{}{
			outpoint: {},
		}
		err := s.removeConflictSweepDescendants(outpoints)
		if err != nil {
			log.Warnf("Unable to remove conflicting sweep tx from "+
				"wallet for outpoint %v : %v", outpoint, err)
		}
	}
}

// sweepCluster tries to sweep the given input cluster.
func (s *UtxoSweeper) sweepCluster(cluster inputCluster) error {
	// Execute the sweep within a coin select lock. Otherwise the coins
	// that we are going to spend may be selected for other transactions
	// like funding of a channel.
	return s.cfg.Wallet.WithCoinSelectLock(func() error {
		// Examine pending inputs and try to construct lists of inputs.
		allSets, newSets, err := s.getInputLists(cluster)
		if err != nil {
			return fmt.Errorf("examine pending inputs: %w", err)
		}

		// errAllSets records the error from broadcasting the sweeping
		// transactions for all input sets.
		var errAllSets error

		// allSets contains retried inputs and new inputs. To avoid
		// creating an RBF for the new inputs, we'd sweep this set
		// first.
		for _, inputs := range allSets {
			errAllSets = s.sweep(inputs, cluster.sweepFeeRate)
			// TODO(yy): we should also find out which set created
			// this error. If there are new inputs in this set, we
			// should give it a second chance by sweeping them
			// below. To enable this, we need to provide richer
			// state for each input other than just recording the
			// publishAttempts. We'd also need to refactor how we
			// create the input sets. Atm, the steps are,
			// 1. create a list of input sets.
			// 2. sweep each set by creating and publishing the tx.
			// We should change the flow as,
			// 1. create a list of input sets, and for each set,
			// 2. when created, we create and publish the tx.
			// 3. if the publish fails, find out which input is
			//    causing the failure and retry the rest of the
			//    inputs.
			if errAllSets != nil {
				log.Errorf("sweep all inputs: %w", err)
				break
			}
		}

		// If we have successfully swept all inputs, there's no need to
		// sweep the new inputs as it'd create an RBF case.
		if allSets != nil && errAllSets == nil {
			return nil
		}

		// We'd end up there if there's no retried inputs. In this
		// case, we'd sweep the new input sets. If there's an error
		// when sweeping a given set, we'd log the error and sweep the
		// next set.
		for _, inputs := range newSets {
			err := s.sweep(inputs, cluster.sweepFeeRate)
			if err != nil {
				log.Errorf("sweep new inputs: %w", err)
			}
		}

		return nil
	})
}

// bucketForFeeReate determines the proper bucket for a fee rate. This is done
// in order to batch inputs with similar fee rates together.
func (s *UtxoSweeper) bucketForFeeRate(
	feeRate chainfee.SatPerKWeight) int {

	// Create an isolated bucket for sweeps at the minimum fee rate. This is
	// to prevent very small outputs (anchors) from becoming uneconomical if
	// their fee rate would be averaged with higher fee rate inputs in a
	// regular bucket.
	if feeRate == s.relayFeeRate {
		return 0
	}

	return 1 + int(feeRate-s.relayFeeRate)/s.cfg.FeeRateBucketSize
}

// createInputClusters creates a list of input clusters from the set of pending
// inputs known by the UtxoSweeper. It clusters inputs by
// 1) Required tx locktime
// 2) Similar fee rates.
func (s *UtxoSweeper) createInputClusters() []inputCluster {
	inputs := s.pendingInputs

	// We start by getting the inputs clusters by locktime. Since the
	// inputs commit to the locktime, they can only be clustered together
	// if the locktime is equal.
	lockTimeClusters, nonLockTimeInputs := s.clusterByLockTime(inputs)

	// Cluster the remaining inputs by sweep fee rate.
	feeClusters := s.clusterBySweepFeeRate(nonLockTimeInputs)

	// Since the inputs that we clustered by fee rate don't commit to a
	// specific locktime, we can try to merge a locktime cluster with a fee
	// cluster.
	return zipClusters(lockTimeClusters, feeClusters)
}

// clusterByLockTime takes the given set of pending inputs and clusters those
// with equal locktime together. Each cluster contains a sweep fee rate, which
// is determined by calculating the average fee rate of all inputs within that
// cluster. In addition to the created clusters, inputs that did not specify a
// required lock time are returned.
func (s *UtxoSweeper) clusterByLockTime(inputs pendingInputs) ([]inputCluster,
	pendingInputs) {

	locktimes := make(map[uint32]pendingInputs)
	rem := make(pendingInputs)

	// Go through all inputs and check if they require a certain locktime.
	for op, input := range inputs {
		lt, ok := input.RequiredLockTime()
		if !ok {
			rem[op] = input
			continue
		}

		// Check if we already have inputs with this locktime.
		cluster, ok := locktimes[lt]
		if !ok {
			cluster = make(pendingInputs)
		}

		// Get the fee rate based on the fee preference. If an error is
		// returned, we'll skip sweeping this input for this round of
		// cluster creation and retry it when we create the clusters
		// from the pending inputs again.
		feeRate, err := input.params.Fee.Estimate(
			s.cfg.FeeEstimator, s.cfg.MaxFeeRate.FeePerKWeight(),
		)
		if err != nil {
			log.Warnf("Skipping input %v: %v", op, err)
			continue
		}

		log.Debugf("Adding input %v to cluster with locktime=%v, "+
			"feeRate=%v", op, lt, feeRate)

		// Attach the fee rate to the input.
		input.lastFeeRate = feeRate

		// Update the cluster about the updated input.
		cluster[op] = input
		locktimes[lt] = cluster
	}

	// We'll then determine the sweep fee rate for each set of inputs by
	// calculating the average fee rate of the inputs within each set.
	inputClusters := make([]inputCluster, 0, len(locktimes))
	for lt, cluster := range locktimes {
		lt := lt

		var sweepFeeRate chainfee.SatPerKWeight
		for _, input := range cluster {
			sweepFeeRate += input.lastFeeRate
		}

		sweepFeeRate /= chainfee.SatPerKWeight(len(cluster))
		inputClusters = append(inputClusters, inputCluster{
			lockTime:     &lt,
			sweepFeeRate: sweepFeeRate,
			inputs:       cluster,
		})
	}

	return inputClusters, rem
}

// clusterBySweepFeeRate takes the set of pending inputs within the UtxoSweeper
// and clusters those together with similar fee rates. Each cluster contains a
// sweep fee rate, which is determined by calculating the average fee rate of
// all inputs within that cluster.
func (s *UtxoSweeper) clusterBySweepFeeRate(inputs pendingInputs) []inputCluster {
	bucketInputs := make(map[int]*bucketList)
	inputFeeRates := make(map[wire.OutPoint]chainfee.SatPerKWeight)

	// First, we'll group together all inputs with similar fee rates. This
	// is done by determining the fee rate bucket they should belong in.
	for op, input := range inputs {
		feeRate, err := input.params.Fee.Estimate(
			s.cfg.FeeEstimator, s.cfg.MaxFeeRate.FeePerKWeight(),
		)
		if err != nil {
			log.Warnf("Skipping input %v: %v", op, err)
			continue
		}

		// Only try to sweep inputs with an unconfirmed parent if the
		// current sweep fee rate exceeds the parent tx fee rate. This
		// assumes that such inputs are offered to the sweeper solely
		// for the purpose of anchoring down the parent tx using cpfp.
		parentTx := input.UnconfParent()
		if parentTx != nil {
			parentFeeRate :=
				chainfee.SatPerKWeight(parentTx.Fee*1000) /
					chainfee.SatPerKWeight(parentTx.Weight)

			if parentFeeRate >= feeRate {
				log.Debugf("Skipping cpfp input %v: fee_rate=%v, "+
					"parent_fee_rate=%v", op, feeRate,
					parentFeeRate)

				continue
			}
		}

		feeGroup := s.bucketForFeeRate(feeRate)

		// Create a bucket list for this fee rate if there isn't one
		// yet.
		buckets, ok := bucketInputs[feeGroup]
		if !ok {
			buckets = &bucketList{}
			bucketInputs[feeGroup] = buckets
		}

		// Request the bucket list to add this input. The bucket list
		// will take into account exclusive group constraints.
		buckets.add(input)

		input.lastFeeRate = feeRate
		inputFeeRates[op] = feeRate
	}

	// We'll then determine the sweep fee rate for each set of inputs by
	// calculating the average fee rate of the inputs within each set.
	inputClusters := make([]inputCluster, 0, len(bucketInputs))
	for _, buckets := range bucketInputs {
		for _, inputs := range buckets.buckets {
			var sweepFeeRate chainfee.SatPerKWeight
			for op := range inputs {
				sweepFeeRate += inputFeeRates[op]
			}
			sweepFeeRate /= chainfee.SatPerKWeight(len(inputs))
			inputClusters = append(inputClusters, inputCluster{
				sweepFeeRate: sweepFeeRate,
				inputs:       inputs,
			})
		}
	}

	return inputClusters
}

// zipClusters merges pairwise clusters from as and bs such that cluster a from
// as is merged with a cluster from bs that has at least the fee rate of a.
// This to ensure we don't delay confirmation by decreasing the fee rate (the
// lock time inputs are typically second level HTLC transactions, that are time
// sensitive).
func zipClusters(as, bs []inputCluster) []inputCluster {
	// Sort the clusters by decreasing fee rates.
	sort.Slice(as, func(i, j int) bool {
		return as[i].sweepFeeRate >
			as[j].sweepFeeRate
	})
	sort.Slice(bs, func(i, j int) bool {
		return bs[i].sweepFeeRate >
			bs[j].sweepFeeRate
	})

	var (
		finalClusters []inputCluster
		j             int
	)

	// Go through each cluster in as, and merge with the next one from bs
	// if it has at least the fee rate needed.
	for i := range as {
		a := as[i]

		switch {
		// If the fee rate for the next one from bs is at least a's, we
		// merge.
		case j < len(bs) && bs[j].sweepFeeRate >= a.sweepFeeRate:
			merged := mergeClusters(a, bs[j])
			finalClusters = append(finalClusters, merged...)

			// Increment j for the next round.
			j++

		// We did not merge, meaning all the remaining clusters from bs
		// have lower fee rate. Instead we add a directly to the final
		// clusters.
		default:
			finalClusters = append(finalClusters, a)
		}
	}

	// Add any remaining clusters from bs.
	for ; j < len(bs); j++ {
		b := bs[j]
		finalClusters = append(finalClusters, b)
	}

	return finalClusters
}

// mergeClusters attempts to merge cluster a and b if they are compatible. The
// new cluster will have the locktime set if a or b had a locktime set, and a
// sweep fee rate that is the maximum of a and b's. If the two clusters are not
// compatible, they will be returned unchanged.
func mergeClusters(a, b inputCluster) []inputCluster {
	newCluster := inputCluster{}

	switch {
	// Incompatible locktimes, return the sets without merging them.
	case a.lockTime != nil && b.lockTime != nil && *a.lockTime != *b.lockTime:
		return []inputCluster{a, b}

	case a.lockTime != nil:
		newCluster.lockTime = a.lockTime

	case b.lockTime != nil:
		newCluster.lockTime = b.lockTime
	}

	if a.sweepFeeRate > b.sweepFeeRate {
		newCluster.sweepFeeRate = a.sweepFeeRate
	} else {
		newCluster.sweepFeeRate = b.sweepFeeRate
	}

	newCluster.inputs = make(pendingInputs)

	for op, in := range a.inputs {
		newCluster.inputs[op] = in
	}

	for op, in := range b.inputs {
		newCluster.inputs[op] = in
	}

	return []inputCluster{newCluster}
}

// signalAndRemove notifies the listeners of the final result of the input
// sweep. It cancels any pending spend notification and removes the input from
// the list of pending inputs. When this function returns, the sweeper has
// completely forgotten about the input.
func (s *UtxoSweeper) signalAndRemove(outpoint *wire.OutPoint, result Result) {
	pendInput := s.pendingInputs[*outpoint]
	listeners := pendInput.listeners

	if result.Err == nil {
		log.Debugf("Dispatching sweep success for %v to %v listeners",
			outpoint, len(listeners),
		)
	} else {
		log.Debugf("Dispatching sweep error for %v to %v listeners: %v",
			outpoint, len(listeners), result.Err,
		)
	}

	// Signal all listeners. Channel is buffered. Because we only send once
	// on every channel, it should never block.
	for _, resultChan := range listeners {
		resultChan <- result
	}

	// Cancel spend notification with chain notifier. This is not necessary
	// in case of a success, except for that a reorg could still happen.
	if pendInput.ntfnRegCancel != nil {
		log.Debugf("Canceling spend ntfn for %v", outpoint)

		pendInput.ntfnRegCancel()
	}

	// Inputs are no longer pending after result has been sent.
	delete(s.pendingInputs, *outpoint)
}

// getInputLists goes through the given inputs and constructs multiple distinct
// sweep lists with the given fee rate, each up to the configured maximum
// number of inputs. Negative yield inputs are skipped. Transactions with an
// output below the dust limit are not published. Those inputs remain pending
// and will be bundled with future inputs if possible. It returns two list -
// one containing all inputs and the other containing only the new inputs. If
// there's no retried inputs, the first set returned will be empty.
func (s *UtxoSweeper) getInputLists(
	cluster inputCluster) ([]inputSet, []inputSet, error) {

	// Filter for inputs that need to be swept. Create two lists: all
	// sweepable inputs and a list containing only the new, never tried
	// inputs.
	//
	// We want to create as large a tx as possible, so we return a final
	// set list that starts with sets created from all inputs. However,
	// there is a chance that those txes will not publish, because they
	// already contain inputs that failed before. Therefore we also add
	// sets consisting of only new inputs to the list, to make sure that
	// new inputs are given a good, isolated chance of being published.
	//
	// TODO(yy): this would lead to conflict transactions as the same input
	// can be used in two sweeping transactions, and our rebroadcaster will
	// retry the failed one. We should instead understand why the input is
	// failed in the first place, and start tracking input states in
	// sweeper to avoid this.
	var newInputs, retryInputs []txInput
	for _, input := range cluster.inputs {
		// Skip inputs that have a minimum publish height that is not
		// yet reached.
		if input.minPublishHeight > s.currentHeight {
			continue
		}

		// Add input to the either one of the lists.
		if input.publishAttempts == 0 {
			newInputs = append(newInputs, input)
		} else {
			retryInputs = append(retryInputs, input)
		}
	}

	// Convert the max fee rate's unit from sat/vb to sat/kw.
	maxFeeRate := s.cfg.MaxFeeRate.FeePerKWeight()

	// If there is anything to retry, combine it with the new inputs and
	// form input sets.
	var allSets []inputSet
	if len(retryInputs) > 0 {
		var err error
		allSets, err = generateInputPartitionings(
			append(retryInputs, newInputs...),
			cluster.sweepFeeRate, maxFeeRate,
			s.cfg.MaxInputsPerTx, s.cfg.Wallet,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("input partitionings: %w",
				err)
		}
	}

	// Create sets for just the new inputs.
	newSets, err := generateInputPartitionings(
		newInputs, cluster.sweepFeeRate, maxFeeRate,
		s.cfg.MaxInputsPerTx, s.cfg.Wallet,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("input partitionings: %w", err)
	}

	log.Debugf("Sweep candidates at height=%v: total_num_pending=%v, "+
		"total_num_new=%v", s.currentHeight, len(allSets), len(newSets))

	return allSets, newSets, nil
}

// sweep takes a set of preselected inputs, creates a sweep tx and publishes the
// tx. The output address is only marked as used if the publish succeeds.
func (s *UtxoSweeper) sweep(inputs inputSet,
	feeRate chainfee.SatPerKWeight) error {

	// Generate an output script if there isn't an unused script available.
	if s.currentOutputScript == nil {
		pkScript, err := s.cfg.GenSweepScript()
		if err != nil {
			return fmt.Errorf("gen sweep script: %w", err)
		}
		s.currentOutputScript = pkScript
	}

	// Create sweep tx.
	tx, fee, err := createSweepTx(
		inputs, nil, s.currentOutputScript, uint32(s.currentHeight),
		feeRate, s.cfg.MaxFeeRate.FeePerKWeight(), s.cfg.Signer,
	)
	if err != nil {
		return fmt.Errorf("create sweep tx: %w", err)
	}

	tr := &TxRecord{
		Txid:    tx.TxHash(),
		FeeRate: uint64(feeRate),
		Fee:     uint64(fee),
	}

	// Add tx before publication, so that we will always know that a spend
	// by this tx is ours. Otherwise if the publish doesn't return, but did
	// publish, we loose track of this tx. Even republication on startup
	// doesn't prevent this, because that call returns a double spend error
	// then and would also not add the hash to the store.
	err = s.cfg.Store.StoreTx(tr)
	if err != nil {
		return fmt.Errorf("store tx: %w", err)
	}

	// Reschedule the inputs that we just tried to sweep. This is done in
	// case the following publish fails, we'd like to update the inputs'
	// publish attempts and rescue them in the next sweep.
	s.rescheduleInputs(tx.TxIn)

	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		tx.TxHash(), len(tx.TxIn), s.currentHeight)

	// Publish the sweeping tx with customized label.
	err = s.cfg.Wallet.PublishTransaction(
		tx, labels.MakeLabel(labels.LabelTypeSweepTransaction, nil),
	)
	if err != nil {
		return err
	}

	// Mark this tx in db once successfully published.
	//
	// NOTE: this will behave as an overwrite, which is fine as the record
	// is small.
	tr.Published = true
	err = s.cfg.Store.StoreTx(tr)
	if err != nil {
		return fmt.Errorf("store tx: %w", err)
	}

	// If there's no error, remove the output script. Otherwise keep it so
	// that it can be reused for the next transaction and causes no address
	// inflation.
	s.currentOutputScript = nil

	return nil
}

// rescheduleInputs updates the pending inputs with the given tx inputs. It
// increments the `publishAttempts` and calculates the next broadcast height
// for each input. When the publishAttempts exceeds MaxSweepAttemps(10), this
// input will be removed.
func (s *UtxoSweeper) rescheduleInputs(inputs []*wire.TxIn) {
	// Reschedule sweep.
	for _, input := range inputs {
		pi, ok := s.pendingInputs[input.PreviousOutPoint]
		if !ok {
			// It can be that the input has been removed because it
			// exceed the maximum number of attempts in a previous
			// input set. It could also be that this input is an
			// additional wallet input that was attached. In that
			// case there also isn't a pending input to update.
			continue
		}

		// Record another publish attempt.
		pi.publishAttempts++

		// We don't care what the result of the publish call was. Even
		// if it is published successfully, it can still be that it
		// needs to be retried. Call NextAttemptDeltaFunc to calculate
		// when to resweep this input.
		nextAttemptDelta := s.cfg.NextAttemptDeltaFunc(
			pi.publishAttempts,
		)

		pi.minPublishHeight = s.currentHeight + nextAttemptDelta

		log.Debugf("Rescheduling input %v after %v attempts at "+
			"height %v (delta %v)", input.PreviousOutPoint,
			pi.publishAttempts, pi.minPublishHeight,
			nextAttemptDelta)

		if pi.publishAttempts >= s.cfg.MaxSweepAttempts {
			log.Warnf("input %v: publishAttempts(%v) exceeds "+
				"MaxSweepAttempts(%v), removed",
				input.PreviousOutPoint, pi.publishAttempts,
				s.cfg.MaxSweepAttempts)

			// Signal result channels sweep result.
			s.signalAndRemove(&input.PreviousOutPoint, Result{
				Err: ErrTooManyAttempts,
			})
		}
	}
}

// monitorSpend registers a spend notification with the chain notifier. It
// returns a cancel function that can be used to cancel the registration.
func (s *UtxoSweeper) monitorSpend(outpoint wire.OutPoint,
	script []byte, heightHint uint32) (func(), error) {

	log.Tracef("Wait for spend of %v at heightHint=%v",
		outpoint, heightHint)

	spendEvent, err := s.cfg.Notifier.RegisterSpendNtfn(
		&outpoint, script, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend ntfn: %w", err)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		select {
		case spend, ok := <-spendEvent.Spend:
			if !ok {
				log.Debugf("Spend ntfn for %v canceled",
					outpoint)
				return
			}

			log.Debugf("Delivering spend ntfn for %v",
				outpoint)
			select {
			case s.spendChan <- spend:
				log.Debugf("Delivered spend ntfn for %v",
					outpoint)

			case <-s.quit:
			}
		case <-s.quit:
		}
	}()

	return spendEvent.Cancel, nil
}

// PendingInputs returns the set of inputs that the UtxoSweeper is currently
// attempting to sweep.
func (s *UtxoSweeper) PendingInputs() (map[wire.OutPoint]*PendingInput, error) {
	respChan := make(chan map[wire.OutPoint]*PendingInput, 1)
	errChan := make(chan error, 1)
	select {
	case s.pendingSweepsReqs <- &pendingSweepsReq{
		respChan: respChan,
		errChan:  errChan,
	}:
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}

	select {
	case pendingSweeps := <-respChan:
		return pendingSweeps, nil
	case err := <-errChan:
		return nil, err
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}
}

// handlePendingSweepsReq handles a request to retrieve all pending inputs the
// UtxoSweeper is attempting to sweep.
func (s *UtxoSweeper) handlePendingSweepsReq(
	req *pendingSweepsReq) map[wire.OutPoint]*PendingInput {

	pendingInputs := make(map[wire.OutPoint]*PendingInput, len(s.pendingInputs))
	for _, pendingInput := range s.pendingInputs {
		// Only the exported fields are set, as we expect the response
		// to only be consumed externally.
		op := *pendingInput.OutPoint()
		pendingInputs[op] = &PendingInput{
			OutPoint:    op,
			WitnessType: pendingInput.WitnessType(),
			Amount: btcutil.Amount(
				pendingInput.SignDesc().Output.Value,
			),
			LastFeeRate:         pendingInput.lastFeeRate,
			BroadcastAttempts:   pendingInput.publishAttempts,
			NextBroadcastHeight: uint32(pendingInput.minPublishHeight),
			Params:              pendingInput.params,
		}
	}

	return pendingInputs
}

// UpdateParams allows updating the sweep parameters of a pending input in the
// UtxoSweeper. This function can be used to provide an updated fee preference
// and force flag that will be used for a new sweep transaction of the input
// that will act as a replacement transaction (RBF) of the original sweeping
// transaction, if any. The exclusive group is left unchanged.
//
// NOTE: This currently doesn't do any fee rate validation to ensure that a bump
// is actually successful. The responsibility of doing so should be handled by
// the caller.
func (s *UtxoSweeper) UpdateParams(input wire.OutPoint,
	params ParamsUpdate) (chan Result, error) {

	// Ensure the client provided a sane fee preference.
	_, err := params.Fee.Estimate(
		s.cfg.FeeEstimator, s.cfg.MaxFeeRate.FeePerKWeight(),
	)
	if err != nil {
		return nil, err
	}

	responseChan := make(chan *updateResp, 1)
	select {
	case s.updateReqs <- &updateReq{
		input:        input,
		params:       params,
		responseChan: responseChan,
	}:
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}

	select {
	case response := <-responseChan:
		return response.resultChan, response.err
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}
}

// handleUpdateReq handles an update request by simply updating the sweep
// parameters of the pending input. Currently, no validation is done on the new
// fee preference to ensure it will properly create a replacement transaction.
//
// TODO(wilmer):
//   - Validate fee preference to ensure we'll create a valid replacement
//     transaction to allow the new fee rate to propagate throughout the
//     network.
//   - Ensure we don't combine this input with any other unconfirmed inputs that
//     did not exist in the original sweep transaction, resulting in an invalid
//     replacement transaction.
func (s *UtxoSweeper) handleUpdateReq(req *updateReq) (
	chan Result, error) {

	// If the UtxoSweeper is already trying to sweep this input, then we can
	// simply just increase its fee rate. This will allow the input to be
	// batched with others which also have a similar fee rate, creating a
	// higher fee rate transaction that replaces the original input's
	// sweeping transaction.
	pendingInput, ok := s.pendingInputs[req.input]
	if !ok {
		return nil, lnwallet.ErrNotMine
	}

	// Create the updated parameters struct. Leave the exclusive group
	// unchanged.
	newParams := pendingInput.params
	newParams.Fee = req.params.Fee
	newParams.Force = req.params.Force

	log.Debugf("Updating sweep parameters for %v from %v to %v", req.input,
		pendingInput.params, newParams)

	pendingInput.params = newParams

	// We'll reset the input's publish height to the current so that a new
	// transaction can be created that replaces the transaction currently
	// spending the input. We only do this for inputs that have been
	// broadcast at least once to ensure we don't spend an input before its
	// maturity height.
	//
	// NOTE: The UtxoSweeper is not yet offered time-locked inputs, so the
	// check for broadcast attempts is redundant at the moment.
	if pendingInput.publishAttempts > 0 {
		pendingInput.minPublishHeight = s.currentHeight
	}

	resultChan := make(chan Result, 1)
	pendingInput.listeners = append(pendingInput.listeners, resultChan)

	return resultChan, nil
}

// CreateSweepTx accepts a list of inputs and signs and generates a txn that
// spends from them. This method also makes an accurate fee estimate before
// generating the required witnesses.
//
// The created transaction has a single output sending all the funds back to
// the source wallet, after accounting for the fee estimate.
//
// The value of currentBlockHeight argument will be set as the tx locktime.
// This function assumes that all CLTV inputs will be unlocked after
// currentBlockHeight. Reasons not to use the maximum of all actual CLTV expiry
// values of the inputs:
//
// - Make handling re-orgs easier.
// - Thwart future possible fee sniping attempts.
// - Make us blend in with the bitcoind wallet.
func (s *UtxoSweeper) CreateSweepTx(inputs []input.Input,
	feePref FeePreference) (*wire.MsgTx, error) {

	feePerKw, err := DetermineFeePerKw(s.cfg.FeeEstimator, feePref)
	if err != nil {
		return nil, err
	}

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := s.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	tx, _, err := createSweepTx(
		inputs, nil, pkScript, uint32(s.currentHeight), feePerKw,
		s.cfg.MaxFeeRate.FeePerKWeight(), s.cfg.Signer,
	)

	return tx, err
}

// DefaultNextAttemptDeltaFunc is the default calculation for next sweep attempt
// scheduling. It implements exponential back-off with some randomness. This is
// to prevent a stuck tx (for example because fee is too low and can't be bumped
// in btcd) from blocking all other retried inputs in the same tx.
func DefaultNextAttemptDeltaFunc(attempts int) int32 {
	return 1 + rand.Int31n(1<<uint(attempts-1))
}

// ListSweeps returns a list of the sweeps recorded by the sweep store.
func (s *UtxoSweeper) ListSweeps() ([]chainhash.Hash, error) {
	return s.cfg.Store.ListSweeps()
}

// handleNewInput processes a new input by registering spend notification and
// scheduling sweeping for it.
func (s *UtxoSweeper) handleNewInput(input *sweepInputMessage) {

	outpoint := *input.input.OutPoint()
	pendInput, pending := s.pendingInputs[outpoint]
	if pending {
		log.Debugf("Already pending input %v received", outpoint)

		s.handleExistingInput(input, pendInput)

		return
	}

	// Create a new pendingInput and initialize the listeners slice with
	// the passed in result channel. If this input is offered for sweep
	// again, the result channel will be appended to this slice.
	pendInput = &pendingInput{
		listeners:        []chan Result{input.resultChan},
		Input:            input.input,
		minPublishHeight: s.currentHeight,
		params:           input.params,
	}
	s.pendingInputs[outpoint] = pendInput
	log.Tracef("input %v added to pendingInputs", outpoint)

	// Start watching for spend of this input, either by us or the remote
	// party.
	cancel, err := s.monitorSpend(
		outpoint, input.input.SignDesc().Output.PkScript,
		input.input.HeightHint(),
	)
	if err != nil {
		err := fmt.Errorf("wait for spend: %w", err)
		s.signalAndRemove(&outpoint, Result{Err: err})

		return
	}

	pendInput.ntfnRegCancel = cancel
}

// handleExistingInput processes an input that is already known to the sweeper.
// It will overwrite the params of the old input with the new ones.
func (s *UtxoSweeper) handleExistingInput(input *sweepInputMessage,
	oldInput *pendingInput) {

	// Before updating the input details, check if an exclusive group was
	// set. In case the same input is registered again without an exclusive
	// group set, the previous input and its sweep parameters are outdated
	// hence need to be replaced. This scenario currently only happens for
	// anchor outputs. When a channel is force closed, in the worst case 3
	// different sweeps with the same exclusive group are registered with
	// the sweeper to bump the closing transaction (cpfp) when its time
	// critical. Receiving an input which was already registered with the
	// sweeper but now without an exclusive group means non of the previous
	// inputs were used as CPFP, so we need to make sure we update the
	// sweep parameters but also remove all inputs with the same exclusive
	// group because the are outdated too.
	var prevExclGroup *uint64
	if oldInput.params.ExclusiveGroup != nil &&
		input.params.ExclusiveGroup == nil {

		prevExclGroup = new(uint64)
		*prevExclGroup = *oldInput.params.ExclusiveGroup
	}

	// Update input details and sweep parameters. The re-offered input
	// details may contain a change to the unconfirmed parent tx info.
	oldInput.params = input.params
	oldInput.Input = input.input

	// Add additional result channel to signal spend of this input.
	oldInput.listeners = append(oldInput.listeners, input.resultChan)

	if prevExclGroup != nil {
		s.removeExclusiveGroup(*prevExclGroup)
	}
}

// handleInputSpent takes a spend event of our input and updates the sweeper's
// internal state to remove the input.
func (s *UtxoSweeper) handleInputSpent(spend *chainntnfs.SpendDetail) {
	// For testing purposes.
	if s.testSpendChan != nil {
		s.testSpendChan <- *spend.SpentOutPoint
	}

	// Query store to find out if we ever published this tx.
	spendHash := *spend.SpenderTxHash
	isOurTx, err := s.cfg.Store.IsOurTx(spendHash)
	if err != nil {
		log.Errorf("cannot determine if tx %v is ours: %v",
			spendHash, err)
		return
	}

	// If this isn't our transaction, it means someone else swept outputs
	// that we were attempting to sweep. This can happen for anchor outputs
	// as well as justice transactions. In this case, we'll notify the
	// wallet to remove any spends that descent from this output.
	if !isOurTx {
		// Construct a map of the inputs this transaction spends.
		spendingTx := spend.SpendingTx
		inputsSpent := make(
			map[wire.OutPoint]struct{}, len(spendingTx.TxIn),
		)
		for _, txIn := range spendingTx.TxIn {
			inputsSpent[txIn.PreviousOutPoint] = struct{}{}
		}

		log.Debugf("Attempting to remove descendant txns invalidated "+
			"by (txid=%v): %v", spendingTx.TxHash(),
			spew.Sdump(spendingTx))

		err := s.removeConflictSweepDescendants(inputsSpent)
		if err != nil {
			log.Warnf("unable to remove descendant transactions "+
				"due to tx %v: ", spendHash)
		}

		log.Debugf("Detected third party spend related to in flight "+
			"inputs (is_ours=%v): %v", isOurTx,
			newLogClosure(func() string {
				return spew.Sdump(spend.SpendingTx)
			}),
		)
	}

	// Signal sweep results for inputs in this confirmed tx.
	for _, txIn := range spend.SpendingTx.TxIn {
		outpoint := txIn.PreviousOutPoint

		// Check if this input is known to us. It could probably be
		// unknown if we canceled the registration, deleted from
		// pendingInputs but the ntfn was in-flight already. Or this
		// could be not one of our inputs.
		input, ok := s.pendingInputs[outpoint]
		if !ok {
			continue
		}

		// Return either a nil or a remote spend result.
		var err error
		if !isOurTx {
			err = ErrRemoteSpend
		}

		// Signal result channels.
		s.signalAndRemove(&outpoint, Result{
			Tx:  spend.SpendingTx,
			Err: err,
		})

		// Remove all other inputs in this exclusive group.
		if input.params.ExclusiveGroup != nil {
			s.removeExclusiveGroup(*input.params.ExclusiveGroup)
		}
	}
}

// handleSweep is called when the ticker fires. It will create clusters and
// attempt to create and publish the sweeping transactions.
func (s *UtxoSweeper) handleSweep() {
	// We'll attempt to cluster all of our inputs with similar fee rates.
	// Before attempting to sweep them, we'll sort them in descending fee
	// rate order. We do this to ensure any inputs which have had their fee
	// rate bumped are broadcast first in order enforce the RBF policy.
	inputClusters := s.createInputClusters()
	sort.Slice(inputClusters, func(i, j int) bool {
		return inputClusters[i].sweepFeeRate >
			inputClusters[j].sweepFeeRate
	})

	for _, cluster := range inputClusters {
		err := s.sweepCluster(cluster)
		if err != nil {
			log.Errorf("input cluster sweep: %v", err)
		}
	}
}

// init initializes the random generator for random input rescheduling.
func init() {
	rand.Seed(time.Now().Unix())
}
