package sweep

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// ErrRemoteSpend is returned in case an output that we try to sweep is
	// confirmed in a tx of the remote party.
	ErrRemoteSpend = errors.New("remote party swept utxo")

	// ErrTooManyAttempts is returned in case sweeping an output has failed
	// for the configured max number of attempts.
	ErrTooManyAttempts = errors.New("sweep failed after max attempts")

	// DefaultMaxSweepAttempts specifies the default maximum number of times
	// an input is included in a publish attempt before giving up and
	// returning an error to the caller.
	DefaultMaxSweepAttempts = 10
)

// pendingInput is created when an input reaches the main loop for the first
// time. It tracks all relevant state that is needed for sweeping.
type pendingInput struct {
	// listeners is a list of channels over which the final outcome of the
	// sweep needs to be broadcasted.
	listeners []chan Result

	// input is the original struct that contains the input and sign
	// descriptor.
	input Input

	// ntfnRegCancel is populated with a function that cancels the chain
	// notifier spend registration.
	ntfnRegCancel func()

	// minPublishHeight indicates the minimum block height at which this
	// input may be (re)published.
	minPublishHeight int32

	// publishAttempts records the number of attempts that have already been
	// made to sweep this tx.
	publishAttempts int
}

// UtxoSweeper is responsible for sweeping outputs back into the wallet
type UtxoSweeper struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *UtxoSweeperConfig

	newInputs chan *sweepInputMessage
	spendChan chan *chainntnfs.SpendDetail

	pendingInputs map[wire.OutPoint]*pendingInput

	// timer is the channel that signals expiry of the sweep batch timer.
	timer <-chan time.Time

	testSpendChan chan wire.OutPoint

	currentOutputScript []byte

	relayFeePerKW lnwallet.SatPerKWeight

	quit chan struct{}
	wg   sync.WaitGroup
}

// UtxoSweeperConfig contains dependencies of UtxoSweeper.
type UtxoSweeperConfig struct {
	// GenSweepScript generates a P2WKH script belonging to the wallet where
	// funds can be swept.
	GenSweepScript func() ([]byte, error)

	// FeeEstimator is used when crafting sweep transactions to estimate
	// the necessary fee relative to the expected size of the sweep
	// transaction.
	FeeEstimator lnwallet.FeeEstimator

	// PublishTransaction facilitates the process of broadcasting a signed
	// transaction to the appropriate network.
	PublishTransaction func(*wire.MsgTx) error

	// NewBatchTimer creates a channel that will be sent on when a certain
	// time window has passed. During this time window, new inputs can still
	// be added to the sweep tx that is about to be generated.
	NewBatchTimer func() <-chan time.Time

	// Notifier is an instance of a chain notifier we'll use to watch for
	// certain on-chain events.
	Notifier chainntnfs.ChainNotifier

	// ChainIO is used  to determine the current block height.
	ChainIO lnwallet.BlockChainIO

	// Store stores the published sweeper txes.
	Store SweeperStore

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer lnwallet.Signer

	// SweepTxConfTarget assigns a confirmation target for sweep txes on
	// which the fee calculation will be based.
	SweepTxConfTarget uint32

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
	input      Input
	resultChan chan Result
}

// New returns a new Sweeper instance.
func New(cfg *UtxoSweeperConfig) *UtxoSweeper {

	return &UtxoSweeper{
		cfg:           cfg,
		newInputs:     make(chan *sweepInputMessage),
		spendChan:     make(chan *chainntnfs.SpendDetail),
		quit:          make(chan struct{}),
		pendingInputs: make(map[wire.OutPoint]*pendingInput),
	}
}

// Start starts the process of constructing and publish sweep txes.
func (s *UtxoSweeper) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	log.Tracef("Sweeper starting")

	// Retrieve last published tx from database.
	lastTx, err := s.cfg.Store.GetLastPublishedTx()
	if err != nil {
		return fmt.Errorf("get last published tx: %v", err)
	}

	// Republish in case the previous call crashed lnd. We don't care about
	// the return value, because inputs will be re-offered and retried
	// anyway. The only reason we republish here is to prevent the corner
	// case where lnd goes into a restart loop because of a crashing publish
	// tx where we keep deriving new output script. By publishing and
	// possibly crashing already now, we haven't derived a new output script
	// yet.
	if lastTx != nil {
		log.Debugf("Publishing last tx %v", lastTx.TxHash())

		// Error can be ignored. Because we are starting up, there are
		// no pending inputs to update based on the publish result.
		err := s.cfg.PublishTransaction(lastTx)
		if err != nil && err != lnwallet.ErrDoubleSpend {
			log.Errorf("last tx publish: %v", err)
		}
	}

	// Retrieve relay fee for dust limit calculation. Assume that this will
	// not change from here on.
	s.relayFeePerKW = s.cfg.FeeEstimator.RelayFeePerKW()

	// Register for block epochs to retry sweeping every block.
	bestHash, bestHeight, err := s.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return fmt.Errorf("get best block: %v", err)
	}

	log.Debugf("Best height: %v", bestHeight)

	blockEpochs, err := s.cfg.Notifier.RegisterBlockEpochNtfn(
		&chainntnfs.BlockEpoch{
			Height: bestHeight,
			Hash:   bestHash,
		},
	)
	if err != nil {
		return fmt.Errorf("register block epoch ntfn: %v", err)
	}

	// Start sweeper main loop.
	s.wg.Add(1)
	go func() {
		defer blockEpochs.Cancel()
		defer s.wg.Done()

		err := s.collector(blockEpochs.Epochs, bestHeight)
		if err != nil {
			log.Errorf("sweeper stopped: %v", err)
		}
	}()

	return nil
}

// Stop stops sweeper from listening to block epochs and constructing sweep
// txes.
func (s *UtxoSweeper) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Debugf("Sweeper shutting down")

	close(s.quit)
	s.wg.Wait()

	log.Debugf("Sweeper shut down")

	return nil
}

// SweepInput sweeps inputs back into the wallet. The inputs will be batched and
// swept after the batch time window ends.
//
// NOTE: Extreme care needs to be taken that input isn't changed externally.
// Because it is an interface and we don't know what is exactly behind it, we
// cannot make a local copy in sweeper.
func (s *UtxoSweeper) SweepInput(input Input) (chan Result, error) {
	if input == nil || input.OutPoint() == nil || input.SignDesc() == nil {
		return nil, errors.New("nil input received")
	}

	log.Infof("Sweep request received: out_point=%v, witness_type=%v, "+
		"time_lock=%v, size=%v", input.OutPoint(), input.WitnessType(),
		input.BlocksToMaturity(),
		btcutil.Amount(input.SignDesc().Output.Value))

	sweeperInput := &sweepInputMessage{
		input:      input,
		resultChan: make(chan Result, 1),
	}

	// Deliver input to main event loop.
	select {
	case s.newInputs <- sweeperInput:
	case <-s.quit:
		return nil, fmt.Errorf("sweeper shutting down")
	}

	return sweeperInput.resultChan, nil
}

// collector is the sweeper main loop. It processes new inputs, spend
// notifications and counts down to publication of the sweep tx.
func (s *UtxoSweeper) collector(blockEpochs <-chan *chainntnfs.BlockEpoch,
	bestHeight int32) error {

	for {
		select {
		// A new inputs is offered to the sweeper. We check to see if we
		// are already trying to sweep this input and if not, set up a
		// listener for spend and schedule a sweep.
		case input := <-s.newInputs:
			outpoint := *input.input.OutPoint()
			pendInput, pending := s.pendingInputs[outpoint]
			if pending {
				log.Debugf("Already pending input %v received",
					outpoint)

				// Add additional result channel to signal
				// spend of this input.
				pendInput.listeners = append(
					pendInput.listeners, input.resultChan,
				)
				continue
			}

			// Create a new pendingInput and initialize the
			// listeners slice with the passed in result channel. If
			// this input is offered for sweep again, the result
			// channel will be appended to this slice.
			pendInput = &pendingInput{
				listeners:        []chan Result{input.resultChan},
				input:            input.input,
				minPublishHeight: bestHeight,
			}
			s.pendingInputs[outpoint] = pendInput

			// Start watching for spend of this input, either by us
			// or the remote party.
			cancel, err := s.waitForSpend(
				outpoint,
				input.input.SignDesc().Output.PkScript,
				input.input.HeightHint(),
			)
			if err != nil {
				err := fmt.Errorf("wait for spend: %v", err)
				s.signalAndRemove(&outpoint, Result{Err: err})
				continue
			}
			pendInput.ntfnRegCancel = cancel

			// Check to see if with this new input a sweep tx can be
			// formed.
			if err := s.scheduleSweep(bestHeight); err != nil {
				log.Errorf("schedule sweep: %v", err)
			}

		// A spend of one of our inputs is detected. Signal sweep
		// results to the caller(s).
		case spend := <-s.spendChan:
			// For testing purposes.
			if s.testSpendChan != nil {
				s.testSpendChan <- *spend.SpentOutPoint
			}

			// Query store to find out if we every published this
			// tx.
			spendHash := *spend.SpenderTxHash
			isOurTx, err := s.cfg.Store.IsOurTx(spendHash)
			if err != nil {
				log.Errorf("cannot determine if tx %v "+
					"is ours: %v", spendHash, err,
				)
				continue
			}

			log.Debugf("Detected spend related to in flight inputs "+
				"(is_ours=%v): %v",
				newLogClosure(func() string {
					return spew.Sdump(spend.SpendingTx)
				}), isOurTx,
			)

			// Signal sweep results for inputs in this confirmed
			// tx.
			for _, txIn := range spend.SpendingTx.TxIn {
				outpoint := txIn.PreviousOutPoint

				// Check if this input is known to us. It could
				// probably be unknown if we canceled the
				// registration, deleted from pendingInputs but
				// the ntfn was in-flight already. Or this could
				// be not one of our inputs.
				_, ok := s.pendingInputs[outpoint]
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
			}

			// Now that an input of ours is spent, we can try to
			// resweep the remaining inputs.
			if err := s.scheduleSweep(bestHeight); err != nil {
				log.Errorf("schedule sweep: %v", err)
			}

		// The timer expires and we are going to (re)sweep.
		case <-s.timer:
			log.Debugf("Sweep timer expired")

			// Set timer to nil so we know that a new timer needs to
			// be started when new inputs arrive.
			s.timer = nil

			// Retrieve fee estimate for input filtering and final
			// tx fee calculation.
			satPerKW, err := s.cfg.FeeEstimator.EstimateFeePerKW(
				s.cfg.SweepTxConfTarget,
			)
			if err != nil {
				log.Errorf("estimate fee: %v", err)
				continue
			}

			// Examine pending inputs and try to construct lists of
			// inputs.
			inputLists, err := s.getInputLists(bestHeight, satPerKW)
			if err != nil {
				log.Errorf("get input lists: %v", err)
				continue
			}

			// Sweep selected inputs.
			for _, inputs := range inputLists {
				err := s.sweep(inputs, satPerKW, bestHeight)
				if err != nil {
					log.Errorf("sweep: %v", err)
				}
			}

		// A new block comes in. Things may have changed, so we retry a
		// sweep.
		case epoch, ok := <-blockEpochs:
			if !ok {
				return nil
			}

			bestHeight = epoch.Height

			log.Debugf("New blocks: height=%v, sha=%v",
				epoch.Height, epoch.Hash)

			if err := s.scheduleSweep(bestHeight); err != nil {
				log.Errorf("schedule sweep: %v", err)
			}

		case <-s.quit:
			return nil
		}
	}
}

// scheduleSweep starts the sweep timer to create an opportunity for more inputs
// to be added.
func (s *UtxoSweeper) scheduleSweep(currentHeight int32) error {
	// The timer is already ticking, no action needed for the sweep to
	// happen.
	if s.timer != nil {
		log.Debugf("Timer still ticking")
		return nil
	}

	// Retrieve fee estimate for input filtering and final tx fee
	// calculation.
	satPerKW, err := s.cfg.FeeEstimator.EstimateFeePerKW(
		s.cfg.SweepTxConfTarget,
	)
	if err != nil {
		return fmt.Errorf("estimate fee: %v", err)
	}

	// Examine pending inputs and try to construct lists of inputs.
	inputLists, err := s.getInputLists(currentHeight, satPerKW)
	if err != nil {
		return fmt.Errorf("get input lists: %v", err)
	}

	log.Infof("Sweep candidates at height=%v, yield %v distinct txns",
		currentHeight, len(inputLists))

	// If there are no input sets, there is nothing sweepable and we can
	// return without starting the timer.
	if len(inputLists) == 0 {
		return nil
	}

	// Start sweep timer to create opportunity for more inputs to be added
	// before a tx is constructed.
	s.timer = s.cfg.NewBatchTimer()

	log.Debugf("Sweep timer started")

	return nil
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

// getInputLists goes through all pending inputs and constructs sweep lists,
// each up to the configured maximum number of inputs. Negative yield inputs are
// skipped. Transactions with an output below the dust limit are not published.
// Those inputs remain pending and will be bundled with future inputs if
// possible.
func (s *UtxoSweeper) getInputLists(currentHeight int32,
	satPerKW lnwallet.SatPerKWeight) ([]inputSet, error) {

	// Filter for inputs that need to be swept. Create two lists: all
	// sweepable inputs and a list containing only the new, never tried
	// inputs.
	//
	// We want to create as large a tx as possible, so we return a final set
	// list that starts with sets created from all inputs. However, there is
	// a chance that those txes will not publish, because they already
	// contain inputs that failed before. Therefore we also add sets
	// consisting of only new inputs to the list, to make sure that new
	// inputs are given a good, isolated chance of being published.
	var newInputs, retryInputs []Input
	for _, input := range s.pendingInputs {
		// Skip inputs that have a minimum publish height that is not
		// yet reached.
		if input.minPublishHeight > currentHeight {
			continue
		}

		// Add input to the either one of the lists.
		if input.publishAttempts == 0 {
			newInputs = append(newInputs, input.input)
		} else {
			retryInputs = append(retryInputs, input.input)
		}
	}

	// If there is anything to retry, combine it with the new inputs and
	// form input sets.
	var allSets []inputSet
	if len(retryInputs) > 0 {
		var err error
		allSets, err = generateInputPartitionings(
			append(retryInputs, newInputs...),
			s.relayFeePerKW, satPerKW,
			s.cfg.MaxInputsPerTx,
		)
		if err != nil {
			return nil, fmt.Errorf("input partitionings: %v", err)
		}
	}

	// Create sets for just the new inputs.
	newSets, err := generateInputPartitionings(
		newInputs,
		s.relayFeePerKW, satPerKW,
		s.cfg.MaxInputsPerTx,
	)
	if err != nil {
		return nil, fmt.Errorf("input partitionings: %v", err)
	}

	log.Debugf("Sweep candidates at height=%v: total_num_pending=%v, "+
		"total_num_new=%v", currentHeight, len(allSets), len(newSets))

	// Append the new sets at the end of the list, because those tx likely
	// have a higher fee per input.
	return append(allSets, newSets...), nil
}

// sweep takes a set of preselected inputs, creates a sweep tx and publishes the
// tx. The output address is only marked as used if the publish succeeds.
func (s *UtxoSweeper) sweep(inputs inputSet,
	satPerKW lnwallet.SatPerKWeight, currentHeight int32) error {

	var err error

	// Generate output script if no unused script available.
	if s.currentOutputScript == nil {
		s.currentOutputScript, err = s.cfg.GenSweepScript()
		if err != nil {
			return fmt.Errorf("gen sweep script: %v", err)
		}
	}

	// Create sweep tx.
	tx, err := createSweepTx(
		inputs, s.currentOutputScript,
		uint32(currentHeight), satPerKW, s.cfg.Signer,
	)
	if err != nil {
		return fmt.Errorf("create sweep tx: %v", err)
	}

	// Add tx before publication, so that we will always know that a spend
	// by this tx is ours. Otherwise if the publish doesn't return, but did
	// publish, we loose track of this tx. Even republication on startup
	// doesn't prevent this, because that call returns a double spend error
	// then and would also not add the hash to the store.
	err = s.cfg.Store.NotifyPublishTx(tx)
	if err != nil {
		return fmt.Errorf("notify publish tx: %v", err)
	}

	// Publish sweep tx.
	log.Debugf("Publishing sweep tx %v, num_inputs=%v, height=%v",
		tx.TxHash(), len(tx.TxIn), currentHeight)

	log.Tracef("Sweep tx at height=%v: %v", currentHeight,
		newLogClosure(func() string {
			return spew.Sdump(tx)
		}),
	)

	err = s.cfg.PublishTransaction(tx)

	// In case of an unexpected error, don't try to recover.
	if err != nil && err != lnwallet.ErrDoubleSpend {
		return fmt.Errorf("publish tx: %v", err)
	}

	// Keep outputScript in case of an error, so that it can be reused for
	// the next tx and causes no address inflation.
	if err == nil {
		s.currentOutputScript = nil
	}

	// Reschedule sweep.
	for _, input := range tx.TxIn {
		pi, ok := s.pendingInputs[input.PreviousOutPoint]
		if !ok {
			// It can be that the input has been removed because it
			// exceed the maximum number of attempts in a previous
			// input set.
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

		pi.minPublishHeight = currentHeight + nextAttemptDelta

		log.Debugf("Rescheduling input %v after %v attempts at "+
			"height %v (delta %v)", input.PreviousOutPoint,
			pi.publishAttempts, pi.minPublishHeight,
			nextAttemptDelta)

		if pi.publishAttempts >= s.cfg.MaxSweepAttempts {
			// Signal result channels sweep result.
			s.signalAndRemove(&input.PreviousOutPoint, Result{
				Err: ErrTooManyAttempts,
			})
		}
	}
	return nil
}

// waitForSpend registers a spend notification with the chain notifier. It
// returns a cancel function that can be used to cancel the registration.
func (s *UtxoSweeper) waitForSpend(outpoint wire.OutPoint,
	script []byte, heightHint uint32) (func(), error) {

	log.Debugf("Wait for spend of %v", outpoint)

	spendEvent, err := s.cfg.Notifier.RegisterSpendNtfn(
		&outpoint, script, heightHint,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend ntfn: %v", err)
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
func (s *UtxoSweeper) CreateSweepTx(inputs []Input, feePref FeePreference,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	feePerKw, err := DetermineFeePerKw(s.cfg.FeeEstimator, feePref)
	if err != nil {
		return nil, err
	}

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := s.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	return createSweepTx(
		inputs, pkScript, currentBlockHeight, feePerKw, s.cfg.Signer,
	)
}

// DefaultNextAttemptDeltaFunc is the default calculation for next sweep attempt
// scheduling. It implements exponential back-off with some randomness. This is
// to prevent a stuck tx (for example because fee is too low and can't be bumped
// in btcd) from blocking all other retried inputs in the same tx.
func DefaultNextAttemptDeltaFunc(attempts int) int32 {
	return 1 + rand.Int31n(1<<uint(attempts-1))
}

// init initializes the random generator for random input rescheduling.
func init() {
	rand.Seed(time.Now().Unix())
}
