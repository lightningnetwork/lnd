package sweep

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// ErrRemoteSpend is returned in case an output that we try to sweep is
	// confirmed in a tx of the remote party.
	ErrRemoteSpend = errors.New("remote party swept utxo")
)

// UtxoSweeper is responsible for sweeping outputs back into the wallet
type UtxoSweeper struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *UtxoSweeperConfig

	newInputs chan *sweepInputMessage
	spendChan chan *chainntnfs.SpendDetail

	pendingInputs map[wire.OutPoint]*pendingInput

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

	// Estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep
	// transaction.
	Estimator lnwallet.FeeEstimator

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
}

// Result is the struct that is pushed through the result channel. Callers
// can use this to be informed of the final sweep result. In case of a remote
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
		return err
	}

	// Republish in case the previous call crashed lnd. We don't care about
	// the return value, because inputs will be reoffered and retried
	// anyway. The only reason we republish here is to prevent prevent the
	// corner case where lnd goes into a restart loop because of a crashing
	// publish tx where we keep deriving new output script. By publishing
	// and possibly crashing already now, we haven't derived a new output
	// script yet.
	if lastTx != nil {
		s.cfg.PublishTransaction(lastTx)
	}

	// Retrieve relay fee for dust limit calculation. Assume that this will
	// not change from here on.
	s.relayFeePerKW = s.cfg.Estimator.RelayFeePerKW()

	// Register for block epochs to retry sweeping every block.
	bestHash, bestHeight, err := s.cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	blockEpochs, err := s.cfg.Notifier.RegisterBlockEpochNtfn(
		&chainntnfs.BlockEpoch{
			Height: bestHeight,
			Hash:   bestHash,
		},
	)
	if err != nil {
		return err
	}

	// Start sweeper main loop.
	s.wg.Add(1)
	go func() {
		err := s.collector(blockEpochs.Epochs, bestHeight)
		if err != nil {
			log.Errorf("Sweeper stopped: %v", err)
		}

		defer blockEpochs.Cancel()
		defer s.wg.Done()
	}()

	return nil
}

// Stop stops sweeper from listening to block epochs and constructing sweep
// txes.
func (s *UtxoSweeper) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	log.Tracef("Sweeper shutting down")

	close(s.quit)
	s.wg.Wait()

	log.Tracef("Sweeper shut down")

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

	log.Debugf("Sweep request received: %v", input.OutPoint())

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
		case input := <-s.newInputs:
			outpoint := *input.input.OutPoint()
			pendInput, pending := s.pendingInputs[outpoint]
			if pending {
				log.Debugf("Already pending input %v received",
					outpoint)
				// Add additional result channel to signal spend
				// of this input.
				pendInput.listeners = append(
					pendInput.listeners, input.resultChan)
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
			// or the remote party. The possibility of a remote
			// spend is the reason why just registering for conf. of
			// our sweep tx isn't enough.
			cancel, err := s.waitForSpend(
				outpoint,
				input.input.SignDesc().Output.PkScript,
				input.input.HeightHint(),
			)
			if err != nil {
				err = fmt.Errorf(
					"cannot watch for spend: %v",
					err)

				s.signalAndRemove(&outpoint,
					Result{Err: err})

				continue
			}
			pendInput.ntfnRegCancel = cancel

			// Check to see if with this new input a sweep tx can be
			// formed.
			s.scheduleSweep(bestHeight)

		case spend := <-s.spendChan:
			// For testing
			if s.testSpendChan != nil {
				s.testSpendChan <- *spend.SpentOutPoint
			}

			isOurTx := s.cfg.Store.IsOurTx(*spend.SpenderTxHash)

			// Signal sweep results for inputs in this confirmed tx.
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

				var err error
				if !isOurTx {
					err = ErrRemoteSpend
				}

				// Signal result channels sweep result.
				s.signalAndRemove(&outpoint, Result{
					Tx:  spend.SpendingTx,
					Err: err,
				})
			}

			// Don't need to schedule a sweep, because spend ntfns
			// coincide with block epochs.

		case <-s.timer:
			log.Debugf("Timer expired")

			// Set timer to nil so we know that a new timer needs to
			// be started when new inputs arrive.
			s.timer = nil

			// Retrieve fee estimate for input filtering and final
			// tx fee calculation.
			satPerKW, err := s.cfg.Estimator.EstimateFeePerKW(
				s.cfg.SweepTxConfTarget)
			if err != nil {
				return err
			}

			// Examine pending inputs and try to construct lists of
			// inputs.
			inputLists, err := s.getInputLists(bestHeight, satPerKW)
			if err != nil {
				log.Errorf("Cannot sweep on timer event: %v",
					err)
			}

			// Sweep selected inputs.
			for _, inputs := range inputLists {
				err := s.sweep(inputs, satPerKW, bestHeight)
				if err != nil {
					return err
				}
			}

		case epoch := <-blockEpochs:
			bestHeight = epoch.Height
			s.scheduleSweep(bestHeight)

		case <-s.quit:
			return nil
		}
	}
}

// scheduleSweep starts the sweep timer to create an opportunity for more inputs
// to be added.
func (s *UtxoSweeper) scheduleSweep(currentHeight int32) error {
	// Retrieve fee estimate for input filtering and final
	// tx fee calculation.
	satPerKW, err := s.cfg.Estimator.EstimateFeePerKW(
		s.cfg.SweepTxConfTarget)
	if err != nil {
		return err
	}

	// Examine pending inputs and try to construct lists of
	// inputs.
	inputLists, err := s.getInputLists(currentHeight, satPerKW)
	if err != nil {
		log.Errorf("Cannot sweep on timer event: %v",
			err)
	}

	// If there are inputLists, there is something to sweep.
	sweepableInputs := len(inputLists) > 0

	// Stop timer if it is running and there is nothing to sweep anymore.
	if !sweepableInputs {
		if s.timer != nil {
			log.Debugf("Timer stopped, nothing to sweep anymore")
			s.timer = nil
		}
		return nil
	}

	// There is something to sweep. Don't restart the timer if it is already
	// ticking.
	if s.timer != nil {
		log.Debugf("Timer still ticking")
		return nil
	}

	// Start sweep timer to create opportunity for more inputs to be
	// added before a tx is constructed.
	log.Debugf("Timer started")
	s.timer = s.cfg.NewBatchTimer()

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
			outpoint, len(listeners))
	} else {
		log.Debugf("Dispatching sweep error for %v to %v listeners: %v",
			outpoint, len(listeners), result.Err)
	}

	for _, resultChan := range listeners {
		resultChan <- result
	}

	// Cancel spend notification with chain notifier. This is not necessary
	// in case of a success, except for that a reorg could still happen.
	if pendInput.ntfnRegCancel != nil {
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

	// TODO: Separate zero-attempt inputs from retries.

	// Filter for inputs that need to be swept.
	var sweepableInputs []Input
	for _, input := range s.pendingInputs {
		// Skip inputs that have a minimum publish height that is not
		// yet reached.
		if input.minPublishHeight > currentHeight {
			continue
		}

		sweepableInputs = append(sweepableInputs, input.input)
	}

	return generateInputPartitionings(
		sweepableInputs,
		s.relayFeePerKW, satPerKW,
		s.cfg.MaxInputsPerTx,
	)
}

func (s *UtxoSweeper) sweep(inputs []Input,
	satPerKW lnwallet.SatPerKWeight, currentHeight int32) error {

	var err error

	// Generate output script if no unused script available.
	if s.currentOutputScript == nil {
		s.currentOutputScript, err = s.cfg.GenSweepScript()
		if err != nil {
			return err
		}
	}

	// Create sweep tx.
	tx, err := createSweepTx(inputs,
		s.currentOutputScript,
		uint32(currentHeight), satPerKW, s.cfg.Signer,
	)
	if err != nil {
		return fmt.Errorf("cannot create sweep tx: %v", err)
	}

	// Add tx before publication, so that we will always know that a
	// spend by this tx is ours.
	err = s.cfg.Store.NotifyPublishTx(tx)
	if err != nil {
		return fmt.Errorf("cannot notify publish: %v", err)
	}

	// Publish sweep tx.
	log.Debugf("Publishing sweep tx %v", tx.TxHash())
	err = s.cfg.PublishTransaction(tx)

	// Keep outputScript in case of an error, so that it can be
	// reused for the next tx and causes no address inflation.
	if err != nil {
		log.Error(err)
	} else {
		s.currentOutputScript = nil
	}

	// Reschedule sweep.
	for _, input := range tx.TxIn {
		pi := s.pendingInputs[input.PreviousOutPoint]

		var nextAttemptDelta int32
		switch err {
		// Successful publish, but it may still not confirm.
		// Retry later.
		case nil:
			nextAttemptDelta = 6

		// In case of a double spend, retry the next block.
		case lnwallet.ErrDoubleSpend:
			nextAttemptDelta = 1

		// For other errors, back off exponentially.
		default:
			nextAttemptDelta = 1 << uint(pi.publishAttempts)
		}

		pi.minPublishHeight = currentHeight + nextAttemptDelta
		pi.publishAttempts++

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
		return nil, err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case spend, ok := <-spendEvent.Spend:
			if !ok {
				return
			}

			select {
			case s.spendChan <- spend:
				log.Debugf("Delivering spend ntfn for %v",
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
func (s *UtxoSweeper) CreateSweepTx(inputs []Input, confTarget uint32,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	feePerKw, err := s.cfg.Estimator.EstimateFeePerKW(confTarget)
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
