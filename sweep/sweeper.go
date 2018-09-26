package sweep

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// UtxoSweeper provides the functionality to generate sweep txes. The plan is to
// extend UtxoSweeper in the future to also manage the actual sweeping process
// by itself.
type UtxoSweeper struct {
	cfg *UtxoSweeperConfig
}

// UtxoSweeperConfig contains dependencies of UtxoSweeper.
type UtxoSweeperConfig struct {
	// GenSweepScript generates a P2WKH script belonging to the wallet where
	// funds can be swept.
	GenSweepScript func() ([]byte, error)

	// Estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep transaction.
	Estimator lnwallet.FeeEstimator

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer lnwallet.Signer

	// ConfTarget specifies a target for the number of blocks until an
	// initial confirmation.
	ConfTarget uint32
}

// New returns a new UtxoSweeper instance.
func New(cfg *UtxoSweeperConfig) *UtxoSweeper {
	return &UtxoSweeper{
		cfg: cfg,
	}
}

// CreateSweepTx accepts a list of outputs and signs and generates a txn that
// spends from them. This method also makes an accurate fee estimate before
// generating the required witnesses.
//
// The value of currentBlockHeight argument will be set as the tx locktime. This
// function assumes that all CLTV inputs will be unlocked after
// currentBlockHeight. Reasons not to use the maximum of all actual CLTV expiry
// values of the inputs:
//
// - Make handling re-orgs easier.
// - Thwart future possible fee sniping attempts.
// - Make us blend in with the bitcoind wallet.
func (s *UtxoSweeper) CreateSweepTx(inputs []CsvSpendableOutput,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	// Create a transaction which sweeps all the newly mature outputs into
	// an output controlled by the wallet.

	// TODO(roasbeef): can be more intelligent about buffering outputs to
	// be more efficient on-chain.

	// Assemble the inputs into a slice csv spendable outputs, and also a
	// set of regular spendable outputs. The set of regular outputs are CLTV
	// locked outputs that have had their timelocks expire.
	var (
		csvOutputs     []CsvSpendableOutput
		cltvOutputs    []SpendableOutput
		weightEstimate lnwallet.TxWeightEstimator
	)

	// Allocate enough room for both types of outputs.
	csvOutputs = make([]CsvSpendableOutput, 0, len(inputs))
	cltvOutputs = make([]SpendableOutput, 0, len(inputs))

	// Our sweep transaction will pay to a single segwit p2wkh address,
	// ensure it contributes to our weight estimate.
	weightEstimate.AddP2WKHOutput()

	// For each output, use its witness type to determine the estimate
	// weight of its witness, and add it to the proper set of spendable
	// outputs.
	for i := range inputs {
		input := inputs[i]

		switch input.WitnessType() {

		// Outputs on a past commitment transaction that pay directly
		// to us.
		case lnwallet.CommitmentTimeLock:
			weightEstimate.AddWitnessInput(
				lnwallet.ToLocalTimeoutWitnessSize,
			)
			csvOutputs = append(csvOutputs, input)

		// Outgoing second layer HTLC's that have confirmed within the
		// chain, and the output they produced is now mature enough to
		// sweep.
		case lnwallet.HtlcOfferedTimeoutSecondLevel:
			weightEstimate.AddWitnessInput(
				lnwallet.ToLocalTimeoutWitnessSize,
			)
			csvOutputs = append(csvOutputs, input)

		// Incoming second layer HTLC's that have confirmed within the
		// chain, and the output they produced is now mature enough to
		// sweep.
		case lnwallet.HtlcAcceptedSuccessSecondLevel:
			weightEstimate.AddWitnessInput(
				lnwallet.ToLocalTimeoutWitnessSize,
			)
			csvOutputs = append(csvOutputs, input)

		// An HTLC on the commitment transaction of the remote party,
		// that has had its absolute timelock expire.
		case lnwallet.HtlcOfferedRemoteTimeout:
			weightEstimate.AddWitnessInput(
				lnwallet.AcceptedHtlcTimeoutWitnessSize,
			)
			cltvOutputs = append(cltvOutputs, input)

		default:
			log.Warnf("kindergarten output in nursery store "+
				"contains unexpected witness type: %v",
				input.WitnessType())
			continue
		}
	}

	log.Infof("Creating sweep transaction for %v CSV inputs, %v CLTV "+
		"inputs", len(csvOutputs), len(cltvOutputs))

	txWeight := int64(weightEstimate.Weight())
	return s.populateSweepTx(txWeight, currentBlockHeight, csvOutputs, cltvOutputs)
}

// populateSweepTx populate the final sweeping transaction with all witnesses
// in place for all inputs using the provided txn fee. The created transaction
// has a single output sending all the funds back to the source wallet, after
// accounting for the fee estimate.
func (s *UtxoSweeper) populateSweepTx(txWeight int64, currentBlockHeight uint32,
	csvInputs []CsvSpendableOutput,
	cltvInputs []SpendableOutput) (*wire.MsgTx, error) {

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := s.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	// Sum up the total value contained in the inputs.
	var totalSum btcutil.Amount
	for _, o := range csvInputs {
		totalSum += o.Amount()
	}
	for _, o := range cltvInputs {
		totalSum += o.Amount()
	}

	// Using the txn weight estimate, compute the required txn fee.
	feePerKw, err := s.cfg.Estimator.EstimateFeePerKW(s.cfg.ConfTarget)
	if err != nil {
		return nil, err
	}

	log.Debugf("Using %v sat/kw for sweep tx", int64(feePerKw))

	txFee := feePerKw.FeeForWeight(txWeight)

	// Sweep as much possible, after subtracting txn fees.
	sweepAmt := int64(totalSum - txFee)

	// Create the sweep transaction that we will be building. We use
	// version 2 as it is required for CSV. The txn will sweep the amount
	// after fees to the pkscript generated above.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    sweepAmt,
	})

	// We'll also ensure that the transaction has the required lock time if
	// we're sweeping any cltvInputs.
	if len(cltvInputs) > 0 {
		sweepTx.LockTime = currentBlockHeight
	}

	// Add all inputs to the sweep transaction. Ensure that for each
	// csvInput, we set the sequence number properly.
	for _, input := range csvInputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *input.OutPoint(),
			Sequence:         input.BlocksToMaturity(),
		})
	}
	for _, input := range cltvInputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *input.OutPoint(),
		})
	}

	// Before signing the transaction, check to ensure that it meets some
	// basic validity requirements.
	// TODO(conner): add more control to sanity checks, allowing us to delay
	// spending "problem" outputs, e.g. possibly batching with other classes
	// if fees are too low.
	btx := btcutil.NewTx(sweepTx)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	hashCache := txscript.NewTxSigHashes(sweepTx)

	// With all the inputs in place, use each output's unique witness
	// function to generate the final witness required for spending.
	addWitness := func(idx int, tso SpendableOutput) error {
		witness, err := tso.BuildWitness(
			s.cfg.Signer, sweepTx, hashCache, idx,
		)
		if err != nil {
			return err
		}

		sweepTx.TxIn[idx].Witness = witness

		return nil
	}

	// Finally we'll attach a valid witness to each csv and cltv input
	// within the sweeping transaction.
	for i, input := range csvInputs {
		if err := addWitness(i, input); err != nil {
			return nil, err
		}
	}

	// Add offset to relative indexes so cltv witnesses don't overwrite csv
	// witnesses.
	offset := len(csvInputs)
	for i, input := range cltvInputs {
		if err := addWitness(offset+i, input); err != nil {
			return nil, err
		}
	}

	return sweepTx, nil
}
