package sweep

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrRemoteSpend is returned in case an output that we try to sweep is
	// confirmed in a tx of the remote party.
	ErrRemoteSpend = errors.New("remote party swept utxo")

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

	// DefaultDeadlineDelta defines a default deadline delta (1 week) to be
	// used when sweeping inputs with no deadline pressure.
	DefaultDeadlineDelta = int32(1008)
)

// Params contains the parameters that control the sweeping process.
type Params struct {
	// ExclusiveGroup is an identifier that, if set, prevents other inputs
	// with the same identifier from being batched together.
	ExclusiveGroup *uint64

	// DeadlineHeight specifies an absolute block height that this input
	// should be confirmed by. This value is used by the fee bumper to
	// decide its urgency and adjust its feerate used.
	DeadlineHeight fn.Option[int32]

	// Budget specifies the maximum amount of satoshis that can be spent on
	// fees for this sweep.
	Budget btcutil.Amount

	// Immediate indicates that the input should be swept immediately
	// without waiting for blocks to come to trigger the sweeping of
	// inputs.
	Immediate bool

	// StartingFeeRate is an optional parameter that can be used to specify
	// the initial fee rate to use for the fee function.
	StartingFeeRate fn.Option[chainfee.SatPerKWeight]
}

// String returns a human readable interpretation of the sweep parameters.
func (p Params) String() string {
	deadline := "none"
	p.DeadlineHeight.WhenSome(func(d int32) {
		deadline = fmt.Sprintf("%d", d)
	})

	exclusiveGroup := "none"
	if p.ExclusiveGroup != nil {
		exclusiveGroup = fmt.Sprintf("%d", *p.ExclusiveGroup)
	}

	return fmt.Sprintf("startingFeeRate=%v, immediate=%v, "+
		"exclusive_group=%v, budget=%v, deadline=%v", p.StartingFeeRate,
		p.Immediate, exclusiveGroup, p.Budget, deadline)
}

// SweepState represents the current state of a pending input.
//
//nolint:revive
type SweepState uint8

const (
	// Init is the initial state of a pending input. This is set when a new
	// sweeping request for a given input is made.
	Init SweepState = iota

	// PendingPublish specifies an input's state where it's already been
	// included in a sweeping tx but the tx is not published yet.  Inputs
	// in this state should not be used for grouping again.
	PendingPublish

	// Published is the state where the input's sweeping tx has
	// successfully been published. Inputs in this state can only be
	// updated via RBF.
	Published

	// PublishFailed is the state when an error is returned from publishing
	// the sweeping tx. Inputs in this state can be re-grouped in to a new
	// sweeping tx.
	PublishFailed

	// Swept is the final state of a pending input. This is set when the
	// input has been successfully swept.
	Swept

	// Excluded is the state of a pending input that has been excluded and
	// can no longer be swept. For instance, when one of the three anchor
	// sweeping transactions confirmed, the remaining two will be excluded.
	Excluded

	// Failed is the state when a pending input has too many failed publish
	// atttempts or unknown broadcast error is returned.
	Failed
)

// String gives a human readable text for the sweep states.
func (s SweepState) String() string {
	switch s {
	case Init:
		return "Init"

	case PendingPublish:
		return "PendingPublish"

	case Published:
		return "Published"

	case PublishFailed:
		return "PublishFailed"

	case Swept:
		return "Swept"

	case Excluded:
		return "Excluded"

	case Failed:
		return "Failed"

	default:
		return "Unknown"
	}
}

// RBFInfo stores the information required to perform a RBF bump on a pending
// sweeping tx.
type RBFInfo struct {
	// Txid is the txid of the sweeping tx.
	Txid chainhash.Hash

	// FeeRate is the fee rate of the sweeping tx.
	FeeRate chainfee.SatPerKWeight

	// Fee is the total fee of the sweeping tx.
	Fee btcutil.Amount
}

// SweeperInput is created when an input reaches the main loop for the first
// time. It wraps the input and tracks all relevant state that is needed for
// sweeping.
type SweeperInput struct {
	input.Input

	// state tracks the current state of the input.
	state SweepState

	// listeners is a list of channels over which the final outcome of the
	// sweep needs to be broadcasted.
	listeners []chan Result

	// ntfnRegCancel is populated with a function that cancels the chain
	// notifier spend registration.
	ntfnRegCancel func()

	// publishAttempts records the number of attempts that have already been
	// made to sweep this tx.
	publishAttempts int

	// params contains the parameters that control the sweeping process.
	params Params

	// lastFeeRate is the most recent fee rate used for this input within a
	// transaction broadcast to the network.
	lastFeeRate chainfee.SatPerKWeight

	// rbf records the RBF constraints.
	rbf fn.Option[RBFInfo]

	// DeadlineHeight is the deadline height for this input. This is
	// different from the DeadlineHeight in its params as it's an actual
	// value than an option.
	DeadlineHeight int32
}

// String returns a human readable interpretation of the pending input.
func (p *SweeperInput) String() string {
	return fmt.Sprintf("%v (%v)", p.Input.OutPoint(), p.Input.WitnessType())
}

// terminated returns a boolean indicating whether the input has reached a
// final state.
func (p *SweeperInput) terminated() bool {
	switch p.state {
	// If the input has reached a final state, that it's either
	// been swept, or failed, or excluded, we will remove it from
	// our sweeper.
	case Failed, Swept, Excluded:
		return true

	default:
		return false
	}
}

// InputsMap is a type alias for a set of pending inputs.
type InputsMap = map[wire.OutPoint]*SweeperInput

// pendingSweepsReq is an internal message we'll use to represent an external
// caller's intent to retrieve all of the pending inputs the UtxoSweeper is
// attempting to sweep.
type pendingSweepsReq struct {
	respChan chan map[wire.OutPoint]*PendingInputResponse
	errChan  chan error
}

// PendingInputResponse contains information about an input that is currently
// being swept by the UtxoSweeper.
type PendingInputResponse struct {
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

	// Params contains the sweep parameters for this pending request.
	Params Params

	// DeadlineHeight records the deadline height of this input.
	DeadlineHeight uint32
}

// updateReq is an internal message we'll use to represent an external caller's
// intent to update the sweep parameters of a given input.
type updateReq struct {
	input        wire.OutPoint
	params       Params
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

	// inputs is the total set of inputs the UtxoSweeper has been requested
	// to sweep.
	inputs InputsMap

	currentOutputScript []byte

	relayFeeRate chainfee.SatPerKWeight

	quit chan struct{}
	wg   sync.WaitGroup

	// currentHeight is the best known height of the main chain. This is
	// updated whenever a new block epoch is received.
	currentHeight int32

	// bumpResultChan is a channel that receives broadcast results from the
	// TxPublisher.
	bumpResultChan chan *BumpResult
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

	// Notifier is an instance of a chain notifier we'll use to watch for
	// certain on-chain events.
	Notifier chainntnfs.ChainNotifier

	// Mempool is the mempool watcher that will be used to query whether a
	// given input is already being spent by a transaction in the mempool.
	Mempool chainntnfs.MempoolWatcher

	// Store stores the published sweeper txes.
	Store SweeperStore

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer input.Signer

	// MaxInputsPerTx specifies the default maximum number of inputs allowed
	// in a single sweep tx. If more need to be swept, multiple txes are
	// created and published.
	MaxInputsPerTx uint32

	// MaxFeeRate is the maximum fee rate allowed within the UtxoSweeper.
	MaxFeeRate chainfee.SatPerVByte

	// Aggregator is used to group inputs into clusters based on its
	// implemention-specific strategy.
	Aggregator UtxoAggregator

	// Publisher is used to publish the sweep tx crafted here and monitors
	// it for potential fee bumps.
	Publisher Bumper

	// NoDeadlineConfTarget is the conf target to use when sweeping
	// non-time-sensitive outputs.
	NoDeadlineConfTarget uint32
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
		inputs:            make(InputsMap),
		bumpResultChan:    make(chan *BumpResult, 100),
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
//
// TODO(yy): make sure the caller is using the Result chan.
func (s *UtxoSweeper) SweepInput(inp input.Input,
	params Params) (chan Result, error) {

	if inp == nil || inp.OutPoint() == input.EmptyOutPoint ||
		inp.SignDesc() == nil {

		return nil, errors.New("nil input received")
	}

	absoluteTimeLock, _ := inp.RequiredLockTime()
	log.Infof("Sweep request received: out_point=%v, witness_type=%v, "+
		"relative_time_lock=%v, absolute_time_lock=%v, amount=%v, "+
		"parent=(%v), params=(%v)", inp.OutPoint(), inp.WitnessType(),
		inp.BlocksToMaturity(), absoluteTimeLock,
		btcutil.Amount(inp.SignDesc().Output.Value),
		inp.UnconfParent(), params)

	sweeperInput := &sweepInputMessage{
		input:      inp,
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

	for {
		// Clean inputs, which will remove inputs that are swept,
		// failed, or excluded from the sweeper and return inputs that
		// are either new or has been published but failed back, which
		// will be retried again here.
		s.updateSweeperInputs()

		select {
		// A new inputs is offered to the sweeper. We check to see if
		// we are already trying to sweep this input and if not, set up
		// a listener to spend and schedule a sweep.
		case input := <-s.newInputs:
			err := s.handleNewInput(input)
			if err != nil {
				log.Criticalf("Unable to handle new input: %v",
					err)

				return
			}

			// If this input is forced, we perform an sweep
			// immediately.
			if input.params.Immediate {
				inputs := s.updateSweeperInputs()
				s.sweepPendingInputs(inputs)
			}

		// A spend of one of our inputs is detected. Signal sweep
		// results to the caller(s).
		case spend := <-s.spendChan:
			s.handleInputSpent(spend)

		// A new external request has been received to retrieve all of
		// the inputs we're currently attempting to sweep.
		case req := <-s.pendingSweepsReqs:
			s.handlePendingSweepsReq(req)

		// A new external request has been received to bump the fee rate
		// of a given input.
		case req := <-s.updateReqs:
			resultChan, err := s.handleUpdateReq(req)
			req.responseChan <- &updateResp{
				resultChan: resultChan,
				err:        err,
			}

			// Perform an sweep immediately if asked.
			if req.params.Immediate {
				inputs := s.updateSweeperInputs()
				s.sweepPendingInputs(inputs)
			}

		case result := <-s.bumpResultChan:
			// Handle the bump event.
			err := s.handleBumpEvent(result)
			if err != nil {
				log.Errorf("Failed to handle bump event: %v",
					err)
			}

		// A new block comes in, update the bestHeight, perform a check
		// over all pending inputs and publish sweeping txns if needed.
		case epoch, ok := <-blockEpochs:
			if !ok {
				// We should stop the sweeper before stopping
				// the chain service. Otherwise it indicates an
				// error.
				log.Error("Block epoch channel closed")

				return
			}

			// Update the sweeper to the best height.
			s.currentHeight = epoch.Height

			// Update the inputs with the latest height.
			inputs := s.updateSweeperInputs()

			log.Debugf("Received new block: height=%v, attempt "+
				"sweeping %d inputs", epoch.Height, len(inputs))

			// Attempt to sweep any pending inputs.
			s.sweepPendingInputs(inputs)

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
	for outpoint, input := range s.inputs {
		outpoint := outpoint

		// Skip inputs that aren't exclusive.
		if input.params.ExclusiveGroup == nil {
			continue
		}

		// Skip inputs from other exclusive groups.
		if *input.params.ExclusiveGroup != group {
			continue
		}

		// Skip inputs that are already terminated.
		if input.terminated() {
			log.Tracef("Skipped sending error result for "+
				"input %v, state=%v", outpoint, input.state)

			continue
		}

		// Signal result channels.
		s.signalResult(input, Result{
			Err: ErrExclusiveGroupSpend,
		})

		// Update the input's state as it can no longer be swept.
		input.state = Excluded

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

// signalResult notifies the listeners of the final result of the input sweep.
// It also cancels any pending spend notification.
func (s *UtxoSweeper) signalResult(pi *SweeperInput, result Result) {
	op := pi.OutPoint()
	listeners := pi.listeners

	if result.Err == nil {
		log.Tracef("Dispatching sweep success for %v to %v listeners",
			op, len(listeners),
		)
	} else {
		log.Tracef("Dispatching sweep error for %v to %v listeners: %v",
			op, len(listeners), result.Err,
		)
	}

	// Signal all listeners. Channel is buffered. Because we only send once
	// on every channel, it should never block.
	for _, resultChan := range listeners {
		resultChan <- result
	}

	// Cancel spend notification with chain notifier. This is not necessary
	// in case of a success, except for that a reorg could still happen.
	if pi.ntfnRegCancel != nil {
		log.Debugf("Canceling spend ntfn for %v", op)

		pi.ntfnRegCancel()
	}
}

// sweep takes a set of preselected inputs, creates a sweep tx and publishes
// the tx. The output address is only marked as used if the publish succeeds.
func (s *UtxoSweeper) sweep(set InputSet) error {
	// Generate an output script if there isn't an unused script available.
	if s.currentOutputScript == nil {
		pkScript, err := s.cfg.GenSweepScript()
		if err != nil {
			return fmt.Errorf("gen sweep script: %w", err)
		}
		s.currentOutputScript = pkScript
	}

	// Create a fee bump request and ask the publisher to broadcast it. The
	// publisher will then take over and start monitoring the tx for
	// potential fee bump.
	req := &BumpRequest{
		Inputs:          set.Inputs(),
		Budget:          set.Budget(),
		DeadlineHeight:  set.DeadlineHeight(),
		DeliveryAddress: s.currentOutputScript,
		MaxFeeRate:      s.cfg.MaxFeeRate.FeePerKWeight(),
		StartingFeeRate: set.StartingFeeRate(),
		// TODO(yy): pass the strategy here.
	}

	// Reschedule the inputs that we just tried to sweep. This is done in
	// case the following publish fails, we'd like to update the inputs'
	// publish attempts and rescue them in the next sweep.
	s.markInputsPendingPublish(set)

	// Broadcast will return a read-only chan that we will listen to for
	// this publish result and future RBF attempt.
	resp, err := s.cfg.Publisher.Broadcast(req)
	if err != nil {
		outpoints := make([]wire.OutPoint, len(set.Inputs()))
		for i, inp := range set.Inputs() {
			outpoints[i] = inp.OutPoint()
		}

		log.Errorf("Initial broadcast failed: %v, inputs=\n%v", err,
			inputTypeSummary(set.Inputs()))

		// TODO(yy): find out which input is causing the failure.
		s.markInputsPublishFailed(outpoints)

		return err
	}

	// Successfully sent the broadcast attempt, we now handle the result by
	// subscribing to the result chan and listen for future updates about
	// this tx.
	s.wg.Add(1)
	go s.monitorFeeBumpResult(resp)

	return nil
}

// markInputsPendingPublish updates the pending inputs with the given tx
// inputs. It also increments the `publishAttempts`.
func (s *UtxoSweeper) markInputsPendingPublish(set InputSet) {
	// Reschedule sweep.
	for _, input := range set.Inputs() {
		pi, ok := s.inputs[input.OutPoint()]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Tracef("Skipped marking input as pending "+
				"published: %v not found in pending inputs",
				input.OutPoint())

			continue
		}

		// If this input has already terminated, there's clearly
		// something wrong as it would have been removed. In this case
		// we log an error and skip marking this input as pending
		// publish.
		if pi.terminated() {
			log.Errorf("Expect input %v to not have terminated "+
				"state, instead it has %v",
				input.OutPoint, pi.state)

			continue
		}

		// Update the input's state.
		pi.state = PendingPublish

		// Record another publish attempt.
		pi.publishAttempts++
	}
}

// markInputsPublished updates the sweeping tx in db and marks the list of
// inputs as published.
func (s *UtxoSweeper) markInputsPublished(tr *TxRecord,
	inputs []*wire.TxIn) error {

	// Mark this tx in db once successfully published.
	//
	// NOTE: this will behave as an overwrite, which is fine as the record
	// is small.
	tr.Published = true
	err := s.cfg.Store.StoreTx(tr)
	if err != nil {
		return fmt.Errorf("store tx: %w", err)
	}

	// Reschedule sweep.
	for _, input := range inputs {
		pi, ok := s.inputs[input.PreviousOutPoint]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Tracef("Skipped marking input as published: %v "+
				"not found in pending inputs",
				input.PreviousOutPoint)

			continue
		}

		// Valdiate that the input is in an expected state.
		if pi.state != PendingPublish {
			// We may get a Published if this is a replacement tx.
			log.Debugf("Expect input %v to have %v, instead it "+
				"has %v", input.PreviousOutPoint,
				PendingPublish, pi.state)

			continue
		}

		// Update the input's state.
		pi.state = Published

		// Update the input's latest fee rate.
		pi.lastFeeRate = chainfee.SatPerKWeight(tr.FeeRate)
	}

	return nil
}

// markInputsPublishFailed marks the list of inputs as failed to be published.
func (s *UtxoSweeper) markInputsPublishFailed(outpoints []wire.OutPoint) {
	// Reschedule sweep.
	for _, op := range outpoints {
		pi, ok := s.inputs[op]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Tracef("Skipped marking input as publish failed: "+
				"%v not found in pending inputs", op)

			continue
		}

		// Valdiate that the input is in an expected state.
		if pi.state != PendingPublish && pi.state != Published {
			log.Debugf("Expect input %v to have %v, instead it "+
				"has %v", op, PendingPublish, pi.state)

			continue
		}

		log.Warnf("Failed to publish input %v", op)

		// Update the input's state.
		pi.state = PublishFailed
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

			log.Debugf("Delivering spend ntfn for %v", outpoint)

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
func (s *UtxoSweeper) PendingInputs() (
	map[wire.OutPoint]*PendingInputResponse, error) {

	respChan := make(chan map[wire.OutPoint]*PendingInputResponse, 1)
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
	req *pendingSweepsReq) map[wire.OutPoint]*PendingInputResponse {

	resps := make(map[wire.OutPoint]*PendingInputResponse, len(s.inputs))
	for _, inp := range s.inputs {
		// Only the exported fields are set, as we expect the response
		// to only be consumed externally.
		op := inp.OutPoint()
		resps[op] = &PendingInputResponse{
			OutPoint:    op,
			WitnessType: inp.WitnessType(),
			Amount: btcutil.Amount(
				inp.SignDesc().Output.Value,
			),
			LastFeeRate:       inp.lastFeeRate,
			BroadcastAttempts: inp.publishAttempts,
			Params:            inp.params,
			DeadlineHeight:    uint32(inp.DeadlineHeight),
		}
	}

	select {
	case req.respChan <- resps:
	case <-s.quit:
		log.Debug("Skipped sending pending sweep response due to " +
			"UtxoSweeper shutting down")
	}

	return resps
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
	params Params) (chan Result, error) {

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
	sweeperInput, ok := s.inputs[req.input]
	if !ok {
		return nil, lnwallet.ErrNotMine
	}

	// Create the updated parameters struct. Leave the exclusive group
	// unchanged.
	newParams := Params{
		StartingFeeRate: req.params.StartingFeeRate,
		Immediate:       req.params.Immediate,
		Budget:          req.params.Budget,
		DeadlineHeight:  req.params.DeadlineHeight,
		ExclusiveGroup:  sweeperInput.params.ExclusiveGroup,
	}

	log.Debugf("Updating parameters for %v(state=%v) from (%v) to (%v)",
		req.input, sweeperInput.state, sweeperInput.params, newParams)

	sweeperInput.params = newParams

	// We need to reset the state so this input will be attempted again by
	// our sweeper.
	//
	// TODO(yy): a dedicated state?
	sweeperInput.state = Init

	// If the new input specifies a deadline, update the deadline height.
	sweeperInput.DeadlineHeight = req.params.DeadlineHeight.UnwrapOr(
		sweeperInput.DeadlineHeight,
	)

	resultChan := make(chan Result, 1)
	sweeperInput.listeners = append(sweeperInput.listeners, resultChan)

	return resultChan, nil
}

// ListSweeps returns a list of the sweeps recorded by the sweep store.
func (s *UtxoSweeper) ListSweeps() ([]chainhash.Hash, error) {
	return s.cfg.Store.ListSweeps()
}

// mempoolLookup takes an input's outpoint and queries the mempool to see
// whether it's already been spent in a transaction found in the mempool.
// Returns the transaction if found.
func (s *UtxoSweeper) mempoolLookup(op wire.OutPoint) fn.Option[wire.MsgTx] {
	// For neutrino backend, there's no mempool available, so we exit
	// early.
	if s.cfg.Mempool == nil {
		log.Debugf("Skipping mempool lookup for %v, no mempool ", op)

		return fn.None[wire.MsgTx]()
	}

	// Query this input in the mempool. If this outpoint is already spent
	// in mempool, we should get a spending event back immediately.
	return s.cfg.Mempool.LookupInputMempoolSpend(op)
}

// handleNewInput processes a new input by registering spend notification and
// scheduling sweeping for it.
func (s *UtxoSweeper) handleNewInput(input *sweepInputMessage) error {
	// Create a default deadline height, which will be used when there's no
	// DeadlineHeight specified for a given input.
	defaultDeadline := s.currentHeight + int32(s.cfg.NoDeadlineConfTarget)

	outpoint := input.input.OutPoint()
	pi, pending := s.inputs[outpoint]
	if pending {
		log.Debugf("Already has pending input %v received", outpoint)

		s.handleExistingInput(input, pi)

		return nil
	}

	// This is a new input, and we want to query the mempool to see if this
	// input has already been spent. If so, we'll start the input with
	// state Published and attach the RBFInfo.
	state, rbfInfo := s.decideStateAndRBFInfo(input.input.OutPoint())

	// Create a new pendingInput and initialize the listeners slice with
	// the passed in result channel. If this input is offered for sweep
	// again, the result channel will be appended to this slice.
	pi = &SweeperInput{
		state:     state,
		listeners: []chan Result{input.resultChan},
		Input:     input.input,
		params:    input.params,
		rbf:       rbfInfo,
		// Set the acutal deadline height.
		DeadlineHeight: input.params.DeadlineHeight.UnwrapOr(
			defaultDeadline,
		),
	}

	s.inputs[outpoint] = pi
	log.Tracef("input %v, state=%v, added to inputs", outpoint, pi.state)

	// Start watching for spend of this input, either by us or the remote
	// party.
	cancel, err := s.monitorSpend(
		outpoint, input.input.SignDesc().Output.PkScript,
		input.input.HeightHint(),
	)
	if err != nil {
		err := fmt.Errorf("wait for spend: %w", err)
		s.markInputFailed(pi, err)

		return err
	}

	pi.ntfnRegCancel = cancel

	return nil
}

// decideStateAndRBFInfo queries the mempool to see whether the given input has
// already been spent. If so, the state Published will be returned, otherwise
// state Init. When spent, it will query the sweeper store to fetch the fee
// info of the spending transction, and construct an RBFInfo based on it.
// Suppose an error occurs, fn.None is returned.
func (s *UtxoSweeper) decideStateAndRBFInfo(op wire.OutPoint) (
	SweepState, fn.Option[RBFInfo]) {

	// Check if we can find the spending tx of this input in mempool.
	txOption := s.mempoolLookup(op)

	// Extract the spending tx from the option.
	var tx *wire.MsgTx
	txOption.WhenSome(func(t wire.MsgTx) {
		tx = &t
	})

	// Exit early if it's not found.
	//
	// NOTE: this is not accurate for backends that don't support mempool
	// lookup:
	// - for neutrino we don't have a mempool.
	// - for btcd below v0.24.1 we don't have `gettxspendingprevout`.
	if tx == nil {
		return Init, fn.None[RBFInfo]()
	}

	// Otherwise the input is already spent in the mempool, so eventually
	// we will return Published.
	//
	// We also need to update the RBF info for this input. If the sweeping
	// transaction is broadcast by us, we can find the fee info in the
	// sweeper store.
	txid := tx.TxHash()
	tr, err := s.cfg.Store.GetTx(txid)

	// If the tx is not found in the store, it means it's not broadcast by
	// us, hence we can't find the fee info. This is fine as, later on when
	// this tx is confirmed, we will remove the input from our inputs.
	if errors.Is(err, ErrTxNotFound) {
		log.Warnf("Spending tx %v not found in sweeper store", txid)
		return Published, fn.None[RBFInfo]()
	}

	// Exit if we get an db error.
	if err != nil {
		log.Errorf("Unable to get tx %v from sweeper store: %v",
			txid, err)

		return Published, fn.None[RBFInfo]()
	}

	// Prepare the fee info and return it.
	rbf := fn.Some(RBFInfo{
		Txid:    txid,
		Fee:     btcutil.Amount(tr.Fee),
		FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
	})

	return Published, rbf
}

// handleExistingInput processes an input that is already known to the sweeper.
// It will overwrite the params of the old input with the new ones.
func (s *UtxoSweeper) handleExistingInput(input *sweepInputMessage,
	oldInput *SweeperInput) {

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

	// If the new input specifies a deadline, update the deadline height.
	oldInput.DeadlineHeight = input.params.DeadlineHeight.UnwrapOr(
		oldInput.DeadlineHeight,
	)

	// Add additional result channel to signal spend of this input.
	oldInput.listeners = append(oldInput.listeners, input.resultChan)

	if prevExclGroup != nil {
		s.removeExclusiveGroup(*prevExclGroup)
	}
}

// handleInputSpent takes a spend event of our input and updates the sweeper's
// internal state to remove the input.
func (s *UtxoSweeper) handleInputSpent(spend *chainntnfs.SpendDetail) {
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
			lnutils.SpewLogClosure(spend.SpendingTx))
	}

	// We now use the spending tx to update the state of the inputs.
	s.markInputsSwept(spend.SpendingTx, isOurTx)
}

// markInputsSwept marks all inputs swept by the spending transaction as swept.
// It will also notify all the subscribers of this input.
func (s *UtxoSweeper) markInputsSwept(tx *wire.MsgTx, isOurTx bool) {
	for _, txIn := range tx.TxIn {
		outpoint := txIn.PreviousOutPoint

		// Check if this input is known to us. It could probably be
		// unknown if we canceled the registration, deleted from inputs
		// map but the ntfn was in-flight already. Or this could be not
		// one of our inputs.
		input, ok := s.inputs[outpoint]
		if !ok {
			// It's very likely that a spending tx contains inputs
			// that we don't know.
			log.Tracef("Skipped marking input as swept: %v not "+
				"found in pending inputs", outpoint)

			continue
		}

		// This input may already been marked as swept by a previous
		// spend notification, which is likely to happen as one sweep
		// transaction usually sweeps multiple inputs.
		if input.terminated() {
			log.Debugf("Skipped marking input as swept: %v "+
				"state=%v", outpoint, input.state)

			continue
		}

		input.state = Swept

		// Return either a nil or a remote spend result.
		var err error
		if !isOurTx {
			log.Warnf("Input=%v was spent by remote or third "+
				"party in tx=%v", outpoint, tx.TxHash())
			err = ErrRemoteSpend
		}

		// Signal result channels.
		s.signalResult(input, Result{
			Tx:  tx,
			Err: err,
		})

		// Remove all other inputs in this exclusive group.
		if input.params.ExclusiveGroup != nil {
			s.removeExclusiveGroup(*input.params.ExclusiveGroup)
		}
	}
}

// markInputFailed marks the given input as failed and won't be retried. It
// will also notify all the subscribers of this input.
func (s *UtxoSweeper) markInputFailed(pi *SweeperInput, err error) {
	log.Errorf("Failed to sweep input: %v, error: %v", pi, err)

	pi.state = Failed

	// Remove all other inputs in this exclusive group.
	if pi.params.ExclusiveGroup != nil {
		s.removeExclusiveGroup(*pi.params.ExclusiveGroup)
	}

	s.signalResult(pi, Result{Err: err})
}

// updateSweeperInputs updates the sweeper's internal state and returns a map
// of inputs to be swept. It will remove the inputs that are in final states,
// and returns a map of inputs that have either state Init or PublishFailed.
func (s *UtxoSweeper) updateSweeperInputs() InputsMap {
	// Create a map of inputs to be swept.
	inputs := make(InputsMap)

	// Iterate the pending inputs and update the sweeper's state.
	//
	// TODO(yy): sweeper is made to communicate via go channels, so no
	// locks are needed to access the map. However, it'd be safer if we
	// turn this inputs map into a SyncMap in case we wanna add concurrent
	// access to the map in the future.
	for op, input := range s.inputs {
		// If the input has reached a final state, that it's either
		// been swept, or failed, or excluded, we will remove it from
		// our sweeper.
		if input.terminated() {
			log.Debugf("Removing input(State=%v) %v from sweeper",
				input.state, op)

			delete(s.inputs, op)

			continue
		}

		// If this input has been included in a sweep tx that's not
		// published yet, we'd skip this input and wait for the sweep
		// tx to be published.
		if input.state == PendingPublish {
			continue
		}

		// If this input has already been published, we will need to
		// check the RBF condition before attempting another sweeping.
		if input.state == Published {
			continue
		}

		// If the input has a locktime that's not yet reached, we will
		// skip this input and wait for the locktime to be reached.
		locktime, _ := input.RequiredLockTime()
		if uint32(s.currentHeight) < locktime {
			log.Warnf("Skipping input %v due to locktime=%v not "+
				"reached, current height is %v", op, locktime,
				s.currentHeight)

			continue
		}

		// If the input has a CSV that's not yet reached, we will skip
		// this input and wait for the expiry.
		locktime = input.BlocksToMaturity() + input.HeightHint()
		if s.currentHeight < int32(locktime)-1 {
			log.Infof("Skipping input %v due to CSV expiry=%v not "+
				"reached, current height is %v", op, locktime,
				s.currentHeight)

			continue
		}

		// If this input is new or has been failed to be published,
		// we'd retry it. The assumption here is that when an error is
		// returned from `PublishTransaction`, it means the tx has
		// failed to meet the policy, hence it's not in the mempool.
		inputs[op] = input
	}

	return inputs
}

// sweepPendingInputs is called when the ticker fires. It will create clusters
// and attempt to create and publish the sweeping transactions.
func (s *UtxoSweeper) sweepPendingInputs(inputs InputsMap) {
	// Cluster all of our inputs based on the specific Aggregator.
	sets := s.cfg.Aggregator.ClusterInputs(inputs)

	// sweepWithLock is a helper closure that executes the sweep within a
	// coin select lock to prevent the coins being selected for other
	// transactions like funding of a channel.
	sweepWithLock := func(set InputSet) error {
		return s.cfg.Wallet.WithCoinSelectLock(func() error {
			// Try to add inputs from our wallet.
			err := set.AddWalletInputs(s.cfg.Wallet)
			if err != nil {
				return err
			}

			// Create sweeping transaction for each set.
			err = s.sweep(set)
			if err != nil {
				return err
			}

			return nil
		})
	}

	for _, set := range sets {
		var err error
		if set.NeedWalletInput() {
			// Sweep the set of inputs that need the wallet inputs.
			err = sweepWithLock(set)
		} else {
			// Sweep the set of inputs that don't need the wallet
			// inputs.
			err = s.sweep(set)
		}

		if err != nil {
			log.Errorf("Failed to sweep %v: %v", set, err)
		}
	}
}

// monitorFeeBumpResult subscribes to the passed result chan to listen for
// future updates about the sweeping tx.
//
// NOTE: must run as a goroutine.
func (s *UtxoSweeper) monitorFeeBumpResult(resultChan <-chan *BumpResult) {
	defer s.wg.Done()

	for {
		select {
		case r := <-resultChan:
			// Validate the result is valid.
			if err := r.Validate(); err != nil {
				log.Errorf("Received invalid result: %v", err)
				continue
			}

			// Send the result back to the main event loop.
			select {
			case s.bumpResultChan <- r:
			case <-s.quit:
				log.Debug("Sweeper shutting down, skip " +
					"sending bump result")

				return
			}

			// The sweeping tx has been confirmed, we can exit the
			// monitor now.
			//
			// TODO(yy): can instead remove the spend subscription
			// in sweeper and rely solely on this event to mark
			// inputs as Swept?
			if r.Event == TxConfirmed || r.Event == TxFailed {
				log.Debugf("Received %v for sweep tx %v, exit "+
					"fee bump monitor", r.Event,
					r.Tx.TxHash())

				// Cancel the rebroadcasting of the failed tx.
				s.cfg.Wallet.CancelRebroadcast(r.Tx.TxHash())

				return
			}

		case <-s.quit:
			log.Debugf("Sweeper shutting down, exit fee " +
				"bump handler")

			return
		}
	}
}

// handleBumpEventTxFailed handles the case where the tx has been failed to
// publish.
func (s *UtxoSweeper) handleBumpEventTxFailed(r *BumpResult) error {
	tx, err := r.Tx, r.Err

	log.Errorf("Fee bump attempt failed for tx=%v: %v", tx.TxHash(), err)

	outpoints := make([]wire.OutPoint, 0, len(tx.TxIn))
	for _, inp := range tx.TxIn {
		outpoints = append(outpoints, inp.PreviousOutPoint)
	}

	// TODO(yy): should we also remove the failed tx from db?
	s.markInputsPublishFailed(outpoints)

	return err
}

// handleBumpEventTxReplaced handles the case where the sweeping tx has been
// replaced by a new one.
func (s *UtxoSweeper) handleBumpEventTxReplaced(r *BumpResult) error {
	oldTx := r.ReplacedTx
	newTx := r.Tx

	// Prepare a new record to replace the old one.
	tr := &TxRecord{
		Txid:    newTx.TxHash(),
		FeeRate: uint64(r.FeeRate),
		Fee:     uint64(r.Fee),
	}

	// Get the old record for logging purpose.
	oldTxid := oldTx.TxHash()
	record, err := s.cfg.Store.GetTx(oldTxid)
	if err != nil {
		log.Errorf("Fetch tx record for %v: %v", oldTxid, err)
		return err
	}

	// Cancel the rebroadcasting of the replaced tx.
	s.cfg.Wallet.CancelRebroadcast(oldTxid)

	log.Infof("RBFed tx=%v(fee=%v sats, feerate=%v sats/kw) with new "+
		"tx=%v(fee=%v, "+"feerate=%v)", record.Txid, record.Fee,
		record.FeeRate, tr.Txid, tr.Fee, tr.FeeRate)

	// The old sweeping tx has been replaced by a new one, we will update
	// the tx record in the sweeper db.
	//
	// TODO(yy): we may also need to update the inputs in this tx to a new
	// state. Suppose a replacing tx only spends a subset of the inputs
	// here, we'd end up with the rest being marked as `Published` and
	// won't be aggregated in the next sweep. Atm it's fine as we always
	// RBF the same input set.
	if err := s.cfg.Store.DeleteTx(oldTxid); err != nil {
		log.Errorf("Delete tx record for %v: %v", oldTxid, err)
		return err
	}

	// Mark the inputs as published using the replacing tx.
	return s.markInputsPublished(tr, r.Tx.TxIn)
}

// handleBumpEventTxPublished handles the case where the sweeping tx has been
// successfully published.
func (s *UtxoSweeper) handleBumpEventTxPublished(r *BumpResult) error {
	tx := r.Tx
	tr := &TxRecord{
		Txid:    tx.TxHash(),
		FeeRate: uint64(r.FeeRate),
		Fee:     uint64(r.Fee),
	}

	// Inputs have been successfully published so we update their
	// states.
	err := s.markInputsPublished(tr, tx.TxIn)
	if err != nil {
		return err
	}

	log.Debugf("Published sweep tx %v, num_inputs=%v, height=%v",
		tx.TxHash(), len(tx.TxIn), s.currentHeight)

	// If there's no error, remove the output script. Otherwise
	// keep it so that it can be reused for the next transaction
	// and causes no address inflation.
	s.currentOutputScript = nil

	return nil
}

// handleBumpEvent handles the result sent from the bumper based on its event
// type.
//
// NOTE: TxConfirmed event is not handled, since we already subscribe to the
// input's spending event, we don't need to do anything here.
func (s *UtxoSweeper) handleBumpEvent(r *BumpResult) error {
	log.Debugf("Received bump event [%v] for tx %v", r.Event, r.Tx.TxHash())

	switch r.Event {
	// The tx has been published, we update the inputs' state and create a
	// record to be stored in the sweeper db.
	case TxPublished:
		return s.handleBumpEventTxPublished(r)

	// The tx has failed, we update the inputs' state.
	case TxFailed:
		return s.handleBumpEventTxFailed(r)

	// The tx has been replaced, we will remove the old tx and replace it
	// with the new one.
	case TxReplaced:
		return s.handleBumpEventTxReplaced(r)
	}

	return nil
}

// IsSweeperOutpoint determines whether the outpoint was created by the sweeper.
//
// NOTE: It is enough to check the txid because the sweeper will create
// outpoints which solely belong to the internal LND wallet.
func (s *UtxoSweeper) IsSweeperOutpoint(op wire.OutPoint) bool {
	found, err := s.cfg.Store.IsOurTx(op.Hash)
	// In case there is an error fetching the transaction details from the
	// sweeper store we assume the outpoint is still used by the sweeper
	// (worst case scenario).
	//
	// TODO(ziggie): Ensure that confirmed outpoints are deleted from the
	// bucket.
	if err != nil && !errors.Is(err, errNoTxHashesBucket) {
		log.Errorf("failed to fetch info for outpoint(%v:%d) "+
			"with: %v, we assume it is still in use by the sweeper",
			op.Hash, op.Index, err)

		return true
	}

	return found
}
