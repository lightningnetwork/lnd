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
	"github.com/lightningnetwork/lnd/labels"
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

// SweepState represents the current state of a pending input.
//
//nolint:revive
type SweepState uint8

const (
	// StateInit is the initial state of a pending input. This is set when
	// a new sweeping request for a given input is made.
	StateInit SweepState = iota

	// StatePendingPublish specifies an input's state where it's already
	// been included in a sweeping tx but the tx is not published yet.
	// Inputs in this state should not be used for grouping again.
	StatePendingPublish

	// StatePublished is the state where the input's sweeping tx has
	// successfully been published. Inputs in this state can only be
	// updated via RBF.
	StatePublished

	// StatePublishFailed is the state when an error is returned from
	// publishing the sweeping tx. Inputs in this state can be re-grouped
	// in to a new sweeping tx.
	StatePublishFailed

	// StateSwept is the final state of a pending input. This is set when
	// the input has been successfully swept.
	StateSwept

	// StateExcluded is the state of a pending input that has been excluded
	// and can no longer be swept. For instance, when one of the three
	// anchor sweeping transactions confirmed, the remaining two will be
	// excluded.
	StateExcluded

	// StateFailed is the state when a pending input has too many failed
	// publish atttempts or unknown broadcast error is returned.
	StateFailed
)

// String gives a human readable text for the sweep states.
func (s SweepState) String() string {
	switch s {
	case StateInit:
		return "Init"

	case StatePendingPublish:
		return "PendingPublish"

	case StatePublished:
		return "Published"

	case StatePublishFailed:
		return "PublishFailed"

	case StateSwept:
		return "Swept"

	case StateExcluded:
		return "Excluded"

	case StateFailed:
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

// pendingInput is created when an input reaches the main loop for the first
// time. It wraps the input and tracks all relevant state that is needed for
// sweeping.
type pendingInput struct {
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
}

// parameters returns the sweep parameters for this input.
//
// NOTE: Part of the txInput interface.
func (p *pendingInput) parameters() Params {
	return p.params
}

// terminated returns a boolean indicating whether the input has reached a
// final state.
func (p *pendingInput) terminated() bool {
	switch p.state {
	// If the input has reached a final state, that it's either
	// been swept, or failed, or excluded, we will remove it from
	// our sweeper.
	case StateFailed, StateSwept, StateExcluded:
		return true

	default:
		return false
	}
}

// pendingInputs is a type alias for a set of pending inputs.
type pendingInputs = map[wire.OutPoint]*pendingInput

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

	// MaxSweepAttempts specifies the maximum number of times an input is
	// included in a publish attempt before giving up and returning an error
	// to the caller.
	MaxSweepAttempts int

	// MaxFeeRate is the maximum fee rate allowed within the UtxoSweeper.
	MaxFeeRate chainfee.SatPerVByte

	// Aggregator is used to group inputs into clusters based on its
	// implemention-specific strategy.
	Aggregator UtxoAggregator
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
//
// TODO(yy): make sure the caller is using the Result chan.
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

	for {
		// Clean inputs, which will remove inputs that are swept,
		// failed, or excluded from the sweeper and return inputs that
		// are either new or has been published but failed back, which
		// will be retried again here.
		inputs := s.updateSweeperInputs()

		select {
		// A new inputs is offered to the sweeper. We check to see if
		// we are already trying to sweep this input and if not, set up
		// a listener to spend and schedule a sweep.
		case input := <-s.newInputs:
			s.handleNewInput(input)

			// If this input is forced, we perform an sweep
			// immediately.
			if input.params.Force {
				inputs = s.updateSweeperInputs()
				s.sweepPendingInputs(inputs)
			}

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

		// A new block comes in, update the bestHeight.
		//
		// TODO(yy): this is where we check our published transactions
		// and perform RBF if needed. We'd also like to consult our fee
		// bumper to get an updated fee rate.
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
		input.state = StateExcluded

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
func (s *UtxoSweeper) signalResult(pi *pendingInput, result Result) {
	op := pi.OutPoint()
	listeners := pi.listeners

	if result.Err == nil {
		log.Debugf("Dispatching sweep success for %v to %v listeners",
			op, len(listeners),
		)
	} else {
		log.Debugf("Dispatching sweep error for %v to %v listeners: %v",
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

// sweep takes a set of preselected inputs, creates a sweep tx and publishes the
// tx. The output address is only marked as used if the publish succeeds.
func (s *UtxoSweeper) sweep(set InputSet) error {
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
		set.Inputs(), nil, s.currentOutputScript,
		uint32(s.currentHeight), set.FeeRate(),
		s.cfg.MaxFeeRate.FeePerKWeight(), s.cfg.Signer,
	)
	if err != nil {
		return fmt.Errorf("create sweep tx: %w", err)
	}

	tr := &TxRecord{
		Txid:    tx.TxHash(),
		FeeRate: uint64(set.FeeRate()),
		Fee:     uint64(fee),
	}

	// Reschedule the inputs that we just tried to sweep. This is done in
	// case the following publish fails, we'd like to update the inputs'
	// publish attempts and rescue them in the next sweep.
	err = s.markInputsPendingPublish(tr, tx.TxIn)
	if err != nil {
		return err
	}

	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		tx.TxHash(), len(tx.TxIn), s.currentHeight)

	// Publish the sweeping tx with customized label.
	err = s.cfg.Wallet.PublishTransaction(
		tx, labels.MakeLabel(labels.LabelTypeSweepTransaction, nil),
	)
	if err != nil {
		// TODO(yy): find out which input is causing the failure.
		s.markInputsPublishFailed(tx.TxIn)

		return err
	}

	// Inputs have been successfully published so we update their states.
	err = s.markInputsPublished(tr, tx.TxIn)
	if err != nil {
		return err
	}

	// If there's no error, remove the output script. Otherwise keep it so
	// that it can be reused for the next transaction and causes no address
	// inflation.
	s.currentOutputScript = nil

	return nil
}

// markInputsPendingPublish saves the sweeping tx to db and updates the pending
// inputs with the given tx inputs. It also increments the `publishAttempts`.
func (s *UtxoSweeper) markInputsPendingPublish(tr *TxRecord,
	inputs []*wire.TxIn) error {

	// Add tx to db before publication, so that we will always know that a
	// spend by this tx is ours. Otherwise if the publish doesn't return,
	// but did publish, we'd lose track of this tx. Even republication on
	// startup doesn't prevent this, because that call returns a double
	// spend error then and would also not add the hash to the store.
	err := s.cfg.Store.StoreTx(tr)
	if err != nil {
		return fmt.Errorf("store tx: %w", err)
	}

	// Reschedule sweep.
	for _, input := range inputs {
		pi, ok := s.pendingInputs[input.PreviousOutPoint]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Debugf("Skipped marking input as pending "+
				"published: %v not found in pending inputs",
				input.PreviousOutPoint)

			continue
		}

		// If this input has already terminated, there's clearly
		// something wrong as it would have been removed. In this case
		// we log an error and skip marking this input as pending
		// publish.
		if pi.terminated() {
			log.Errorf("Expect input %v to not have terminated "+
				"state, instead it has %v",
				input.PreviousOutPoint, pi.state)

			continue
		}

		// Update the input's state.
		pi.state = StatePendingPublish

		// Record the fees and fee rate of this tx to prepare possible
		// RBF.
		pi.rbf = fn.Some(RBFInfo{
			Txid:    tr.Txid,
			FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
			Fee:     btcutil.Amount(tr.Fee),
		})

		// Record another publish attempt.
		pi.publishAttempts++
	}

	return nil
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
		pi, ok := s.pendingInputs[input.PreviousOutPoint]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Debugf("Skipped marking input as published: %v "+
				"not found in pending inputs",
				input.PreviousOutPoint)

			continue
		}

		// Valdiate that the input is in an expected state.
		if pi.state != StatePendingPublish {
			log.Errorf("Expect input %v to have %v, instead it "+
				"has %v", input.PreviousOutPoint,
				StatePendingPublish, pi.state)

			continue
		}

		// Update the input's state.
		pi.state = StatePublished
	}

	return nil
}

// markInputsPublishFailed marks the list of inputs as failed to be published.
func (s *UtxoSweeper) markInputsPublishFailed(inputs []*wire.TxIn) {
	// Reschedule sweep.
	for _, input := range inputs {
		pi, ok := s.pendingInputs[input.PreviousOutPoint]
		if !ok {
			// It could be that this input is an additional wallet
			// input that was attached. In that case there also
			// isn't a pending input to update.
			log.Debugf("Skipped marking input as publish failed: "+
				"%v not found in pending inputs",
				input.PreviousOutPoint)

			continue
		}

		// Valdiate that the input is in an expected state.
		if pi.state != StatePendingPublish {
			log.Errorf("Expect input %v to have %v, instead it "+
				"has %v", input.PreviousOutPoint,
				StatePendingPublish, pi.state)

			continue
		}

		log.Warnf("Failed to publish input %v", input.PreviousOutPoint)

		// Update the input's state.
		pi.state = StatePublishFailed
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
			LastFeeRate:       pendingInput.lastFeeRate,
			BroadcastAttempts: pendingInput.publishAttempts,
			Params:            pendingInput.params,
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

	log.Debugf("Updating parameters for %v(state=%v) from (%v) to (%v)",
		req.input, pendingInput.state, pendingInput.params, newParams)

	pendingInput.params = newParams

	// We need to reset the state so this input will be attempted again by
	// our sweeper.
	//
	// TODO(yy): a dedicated state?
	pendingInput.state = StateInit

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
//
// TODO(yy): remove this method and only allow sweeping via requests.
func (s *UtxoSweeper) CreateSweepTx(inputs []input.Input,
	feePref FeeEstimateInfo) (*wire.MsgTx, error) {

	feePerKw, err := feePref.Estimate(
		s.cfg.FeeEstimator, s.cfg.MaxFeeRate.FeePerKWeight(),
	)
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
func (s *UtxoSweeper) handleNewInput(input *sweepInputMessage) {
	outpoint := *input.input.OutPoint()
	pi, pending := s.pendingInputs[outpoint]
	if pending {
		log.Debugf("Already has pending input %v received", outpoint)

		s.handleExistingInput(input, pi)

		return
	}

	// This is a new input, and we want to query the mempool to see if this
	// input has already been spent. If so, we'll start the input with
	// state Published and attach the RBFInfo.
	state, rbfInfo := s.decideStateAndRBFInfo(*input.input.OutPoint())

	// Create a new pendingInput and initialize the listeners slice with
	// the passed in result channel. If this input is offered for sweep
	// again, the result channel will be appended to this slice.
	pi = &pendingInput{
		state:     state,
		listeners: []chan Result{input.resultChan},
		Input:     input.input,
		params:    input.params,
		rbf:       rbfInfo,
	}

	s.pendingInputs[outpoint] = pi
	log.Tracef("input %v, state=%v, added to pendingInputs", outpoint,
		pi.state)

	// Start watching for spend of this input, either by us or the remote
	// party.
	cancel, err := s.monitorSpend(
		outpoint, input.input.SignDesc().Output.PkScript,
		input.input.HeightHint(),
	)
	if err != nil {
		err := fmt.Errorf("wait for spend: %w", err)
		s.markInputFailed(pi, err)

		return
	}

	pi.ntfnRegCancel = cancel
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
		return StateInit, fn.None[RBFInfo]()
	}

	// Otherwise the input is already spent in the mempool, so eventually
	// we will return StatePublished.
	//
	// We also need to update the RBF info for this input. If the sweeping
	// transaction is broadcast by us, we can find the fee info in the
	// sweeper store.
	txid := tx.TxHash()
	tr, err := s.cfg.Store.GetTx(txid)

	// If the tx is not found in the store, it means it's not broadcast by
	// us, hence we can't find the fee info. This is fine as, later on when
	// this tx is confirmed, we will remove the input from our
	// pendingInputs.
	if errors.Is(err, ErrTxNotFound) {
		log.Warnf("Spending tx %v not found in sweeper store", txid)
		return StatePublished, fn.None[RBFInfo]()
	}

	// Exit if we get an db error.
	if err != nil {
		log.Errorf("Unable to get tx %v from sweeper store: %v",
			txid, err)

		return StatePublished, fn.None[RBFInfo]()
	}

	// Prepare the fee info and return it.
	rbf := fn.Some(RBFInfo{
		Txid:    txid,
		Fee:     btcutil.Amount(tr.Fee),
		FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
	})

	return StatePublished, rbf
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

	// We now use the spending tx to update the state of the inputs.
	s.markInputsSwept(spend.SpendingTx, isOurTx)
}

// markInputsSwept marks all inputs swept by the spending transaction as swept.
// It will also notify all the subscribers of this input.
func (s *UtxoSweeper) markInputsSwept(tx *wire.MsgTx, isOurTx bool) {
	for _, txIn := range tx.TxIn {
		outpoint := txIn.PreviousOutPoint

		// Check if this input is known to us. It could probably be
		// unknown if we canceled the registration, deleted from
		// pendingInputs but the ntfn was in-flight already. Or this
		// could be not one of our inputs.
		input, ok := s.pendingInputs[outpoint]
		if !ok {
			// It's very likely that a spending tx contains inputs
			// that we don't know.
			log.Debugf("Skipped marking input as swept: %v not "+
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

		input.state = StateSwept

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
func (s *UtxoSweeper) markInputFailed(pi *pendingInput, err error) {
	log.Errorf("Failed to sweep input: %v, error: %v", pi, err)

	pi.state = StateFailed

	// Remove all other inputs in this exclusive group.
	if pi.params.ExclusiveGroup != nil {
		s.removeExclusiveGroup(*pi.params.ExclusiveGroup)
	}

	s.signalResult(pi, Result{Err: err})
}

// updateSweeperInputs updates the sweeper's internal state and returns a map
// of inputs to be swept. It will remove the inputs that are in final states,
// and returns a map of inputs that have either StateInit or
// StatePublishFailed.
func (s *UtxoSweeper) updateSweeperInputs() pendingInputs {
	// Create a map of inputs to be swept.
	inputs := make(pendingInputs)

	// Iterate the pending inputs and update the sweeper's state.
	//
	// TODO(yy): sweeper is made to communicate via go channels, so no
	// locks are needed to access the map. However, it'd be safer if we
	// turn this pendingInputs into a SyncMap in case we wanna add
	// concurrent access to the map in the future.
	for op, input := range s.pendingInputs {
		// If the input has reached a final state, that it's either
		// been swept, or failed, or excluded, we will remove it from
		// our sweeper.
		if input.terminated() {
			log.Debugf("Removing input(State=%v) %v from sweeper",
				input.state, op)

			delete(s.pendingInputs, op)

			continue
		}

		// If this input has been included in a sweep tx that's not
		// published yet, we'd skip this input and wait for the sweep
		// tx to be published.
		if input.state == StatePendingPublish {
			continue
		}

		// If this input has already been published, we will need to
		// check the RBF condition before attempting another sweeping.
		if input.state == StatePublished {
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
func (s *UtxoSweeper) sweepPendingInputs(inputs pendingInputs) {
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
			log.Errorf("Sweep new inputs: %v", err)
		}
	}
}
