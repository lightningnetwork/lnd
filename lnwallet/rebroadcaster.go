package lnwallet

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	// ErrRebroadcastMaxAttempts is an error returned when the rebroadcaster
	// has reached its maximum number of allowed unsuccessful attempts.
	ErrRebroadcastMaxAttempts = errors.New("exhausted maximum number of " +
		"rebroadcast attempts")

	// ErrRebroadcastMaxFee is an error returned when the rebroadcaster has
	// reached the maximum fee allowed after bumping up the fee due to many
	// unsuccessful attempts
	ErrRebroadcastMaxFee = errors.New("exceeded rebroadcast max fee")

	// ErrRebroadcastDustLimit is an error returned when the rebroadcaster
	// is applying a new fee to the transaction but would result in the
	// change output dipping below the dust limit.
	ErrRebroadcastDustLimit = errors.New("change output dipped below " +
		"dust limit")

	// ErrRebroadcastHalt is an error returned when the rebroadcaster has
	// been requested to halt.
	ErrRebroadcastHalt = errors.New("rebroadcaster is exiting")
)

// WitnessSignDescriptor is a tuple of a transaction input's sign descriptor
// along with its witness type. This is needed in order to use the generic
// GenWitnessFunc in order to generate the correct witness for each type.
type WitnessSignDescriptor struct {
	WitnessType
	*SignDescriptor
}

// TxRebroadcasterCfg contains all of the required fields to be able to bump a
// transaction's current fee, re-sign it, and broadcast it to the network.
//
// NOTE: All fields are required unless otherwise noted.
type TxRebroadcasterCfg struct {
	// Tx is the transaction that should be rebroadcast.
	Tx *wire.MsgTx

	// ChangeOutputIndex is the index of the change output that should be
	// used to bump the transaction's fee.
	ChangeOutputIndex uint32

	// Signer allows us to regenerate signatures for all inputs after
	// applying the new fee to the transaction.
	Signer Signer

	// WitnessSignDescs is the sign descriptor that should be used to resign
	// the input that maps to the change output.
	//
	// NOTE: The slice must be sorted in increasing ordered by the index of
	// each input.
	WitnessSignDescs []*WitnessSignDescriptor

	// BumpPercentage is the percentage used to increase the last
	// insufficient fee tried.
	BumpPercentage int

	// MaxAttempts is the maximum number of tries we should attempt to
	// rebroadcast the transaction.
	//
	// NOTE: This field is optional if MaxFee is set.
	MaxAttempts int

	// MaxFee is the maximum fee in satoshis that we should allow to use on
	// the transaction being rebroadcast.
	//
	// NOTE: This field is optional if MaxAttempts is set.
	MaxFee btcutil.Amount

	// DustLimit is the lower threshold value for which the change output's
	// value should not dip below.
	DustLimit btcutil.Amount

	// ConsumeChangeOutput determines whether we should consume the whole
	// change output to use as fees if it ends up dipping below the dust
	// limit.
	ConsumeChangeOutput bool

	// FetchInputAmount is a callback used to fetch the amount of each of
	// the transaction's inputs in order to calculate its fee.
	FetchInputAmount func(*wire.OutPoint) (btcutil.Amount, error)

	// Broadcast allows us to broadcast the transaction to the network. This
	// will be used to broadcast the higher fee re-signed transaction to
	// ensure it's accepted into the backend's mempool.
	Broadcast func(*wire.MsgTx) error
}

// TxRebroadcaster is a helper struct that aids us in rebroadcasting
// transactions with insufficient fees in order to propagate throughout the
// network.
type TxRebroadcaster struct {
	cfg     TxRebroadcasterCfg
	errChan chan error

	mu          sync.Mutex
	numAttempts int
	lastFee     btcutil.Amount
	lastTx      *wire.MsgTx
}

// NewTxRebroadcaster creates a new rebroadcaster using the given configuration.
func NewTxRebroadcaster(cfg TxRebroadcasterCfg) (*TxRebroadcaster, error) {
	// Validate the constraints specified within the config.
	if cfg.BumpPercentage <= 0 {
		return nil, errors.New("bump percentage must be set")
	}
	if cfg.MaxAttempts <= 0 && cfg.MaxFee <= 0 {
		return nil, errors.New("either MaxAttempts or MaxFee must be set")
	}
	if cfg.DustLimit <= 0 {
		return nil, errors.New("dust limit must be set")
	}

	// Ensure this is a valid transaction.
	tx := btcutil.NewTx(cfg.Tx)
	if err := blockchain.CheckTransactionSanity(tx); err != nil {
		return nil, err
	}

	return &TxRebroadcaster{
		cfg:     cfg,
		errChan: make(chan error, 1),
	}, nil
}

// Rebroadcast kicks off the automatic rebroadcast process. The quit channel can
// be used to signal the rebroadcaster to terminate early.
func (r *TxRebroadcaster) Rebroadcast(quit <-chan struct{}) <-chan error {
	go r.rebroadcast(quit)

	return r.errChan
}

// rebroadcast is the main handler of the rebroadcaster. For every attempt, the
// current fee is bumped by the specified percentage. Each new transaction is
// then signed and broadcast to the network. If the fee is not sufficient, the
// whole process will be done again until either we can read from the quit
// channel, we've reached the maximum fee allowed, or we've reached the maximum
// number of attempts allowed.
func (r *TxRebroadcaster) rebroadcast(quit <-chan struct{}) {
	// We'll start off by computing the current fee of the transaction.
	// We do this by subtracting the total value of the outputs from the
	// total value of the inputs.
	inputAmt := int64(0)
	for _, input := range r.cfg.Tx.TxIn {
		amount, err := r.cfg.FetchInputAmount(&input.PreviousOutPoint)
		if err != nil {
			r.errChan <- fmt.Errorf("unable to fetch utxo used as "+
				"input: %v", err)
			return
		}

		inputAmt += int64(amount)
	}

	outputAmt := int64(0)
	for _, output := range r.cfg.Tx.TxOut {
		outputAmt += output.Value
	}

	currentFee := inputAmt - outputAmt
	changeOutput := r.cfg.Tx.TxOut[r.cfg.ChangeOutputIndex]

	r.mu.Lock()
	r.lastFee = btcutil.Amount(currentFee)
	r.mu.Unlock()

	// With the current fee computed, we can begin our rebroadcast attempts
	// by bumping up the transaction's fee at every iteration.
	for {
		r.mu.Lock()
		numAttempts := r.numAttempts
		currentFee := r.lastFee
		r.mu.Unlock()

		if r.cfg.MaxAttempts > 0 && numAttempts >= r.cfg.MaxAttempts {
			// TODO(wilmer): output should be moved to pool to be
			// swept later on.
			r.errChan <- ErrRebroadcastMaxAttempts
			return
		}

		// Calculate the new fee that should be used for the transaction
		// by taking into account the specified bump and ensure it
		// respects the maximum fee allowed.
		bump := currentFee * btcutil.Amount(r.cfg.BumpPercentage) / 100
		newFee := currentFee + bump
		if r.cfg.MaxFee > 0 && newFee > r.cfg.MaxFee {
			// TODO(wilmer): output should be moved to pool to be
			// swept later on.
			r.errChan <- ErrRebroadcastMaxFee
			return
		}

		// Apply the new fee to the transaction by subtracting the
		// amount increased and ensure our change output hasn't dipped
		// below the dust limit,
		changeOutput.Value -= int64(bump)
		if changeOutput.Value < int64(r.cfg.DustLimit) {
			// If it has, then we'll also check whether this output
			// shouldn't be consumed.
			if !r.cfg.ConsumeChangeOutput {
				r.errChan <- ErrRebroadcastDustLimit
				return
			}

			// Otherwise, it should, so we'll remove it from the
			// transaction.
			outputs := r.cfg.Tx.TxOut
			r.cfg.Tx.TxOut = append(
				outputs[:r.cfg.ChangeOutputIndex],
				outputs[r.cfg.ChangeOutputIndex+1:]...,
			)
		}

		// Before signing the transaction, check to ensure that it meets
		// some basic validity requirements.
		tx := btcutil.NewTx(r.cfg.Tx)
		if err := blockchain.CheckTransactionSanity(tx); err != nil {
			r.errChan <- err
			return
		}

		// With the new fee applied, we'll re-sign the transaction.
		for i := range r.cfg.Tx.TxIn {
			// Retrieve the sign descriptor for the current input.
			if i >= len(r.cfg.WitnessSignDescs) {
				r.errChan <- fmt.Errorf("missing sign "+
					"descriptor for input with index %v", i)
				return
			}

			signDesc := r.cfg.WitnessSignDescs[i]
			signDesc.SigHashes = txscript.NewTxSigHashes(r.cfg.Tx)

			// Determine the type of witness required for this input
			// and generate it.
			witnessFunc := signDesc.GenWitnessFunc(
				r.cfg.Signer, signDesc.SignDescriptor,
			)
			newWitness, err := witnessFunc(
				r.cfg.Tx, signDesc.SigHashes, i,
			)
			if err != nil {
				r.errChan <- fmt.Errorf("unable to sign "+
					"output: %v", err)
				return
			}
			r.cfg.Tx.TxIn[i].Witness = newWitness
		}

		// We'll make sure we haven't been requested to stop right
		// before attempting to rebroadcast the transaction.
		if quit != nil {
			select {
			case <-quit:
				r.errChan <- ErrRebroadcastHalt
				return
			default:
			}
		}

		// Now that the transaction has had its new fee and signatures
		// applied, we can attempt to rebroadcast it.
		err := r.cfg.Broadcast(r.cfg.Tx)

		r.mu.Lock()
		r.numAttempts++
		r.lastFee = newFee
		r.lastTx = r.cfg.Tx
		r.mu.Unlock()

		switch err {
		// If the transaction still has an insufficient fee, we'll bump
		// it up once more.
		case ErrInsufficientFee:
			continue

		// If a previous version of the transaction has already been
		// seen in the network, then we should halt the rebroadcaster in
		// order to avoid bumping up the fee once again.
		case ErrDoubleSpend:
			r.errChan <- nil
			return

		case nil:
			r.errChan <- nil
			return

		default:
			r.errChan <- fmt.Errorf("unable to rebroadcast "+
				"transaction with higher fee: %v", err)
			return
		}
	}
}

// NumAttempts returns the number of attempts the transaction was rebroadcast.
func (r *TxRebroadcaster) NumAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.numAttempts
}

// LastFeeUsed returned the fee used in satoshis for the last rebroadcast
// attempt.
func (r *TxRebroadcaster) LastFeeUsed() btcutil.Amount {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastFee
}

// LastTxBroadcast returns the last version of the transaction that was
// rebroadcast.
//
// NOTE: This may be nil if the tranasaction hasn't yet been rebroadcasted.
func (r *TxRebroadcaster) LastTxBroadcast() *wire.MsgTx {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastTx
}
