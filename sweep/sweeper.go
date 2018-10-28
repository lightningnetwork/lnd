package sweep

import (
	"fmt"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// UtxoSweeper provides the functionality to generate sweep txes. The plan is
// to extend UtxoSweeper in the future to also manage the actual sweeping
// process by itself.
type UtxoSweeper struct {
	cfg *UtxoSweeperConfig
}

// UtxoSweeperConfig contains dependencies of UtxoSweeper.
type UtxoSweeperConfig struct {
	// GenSweepScript generates a P2WKH script belonging to the wallet
	// where funds can be swept.
	GenSweepScript func() ([]byte, error)

	// Estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep
	// transaction.
	Estimator lnwallet.FeeEstimator

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer lnwallet.Signer
}

// New returns a new UtxoSweeper instance.
func New(cfg *UtxoSweeperConfig) *UtxoSweeper {
	return &UtxoSweeper{
		cfg: cfg,
	}
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

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := s.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	// Using the txn weight estimate, compute the required txn fee.
	feePerKw, err := s.cfg.Estimator.EstimateFeePerKW(confTarget)
	if err != nil {
		return nil, err
	}

	inputs, txWeight, csvCount, cltvCount := getWeightEstimate(inputs)
	log.Infof("Creating sweep transaction for %v inputs (%v CSV, %v CLTV) "+
		"using %v sat/kw", len(inputs), csvCount, cltvCount,
		int64(feePerKw))

	txFee := feePerKw.FeeForWeight(txWeight)

	// Sum up the total value contained in the inputs.
	var totalSum btcutil.Amount
	for _, o := range inputs {
		totalSum += btcutil.Amount(o.SignDesc().Output.Value)
	}

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

	sweepTx.LockTime = currentBlockHeight

	// Add all inputs to the sweep transaction. Ensure that for each
	// csvInput, we set the sequence number properly.
	for _, input := range inputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *input.OutPoint(),
			Sequence:         input.BlocksToMaturity(),
		})
	}

	// Before signing the transaction, check to ensure that it meets some
	// basic validity requirements.
	//
	// TODO(conner): add more control to sanity checks, allowing us to
	// delay spending "problem" outputs, e.g. possibly batching with other
	// classes if fees are too low.
	btx := btcutil.NewTx(sweepTx)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, err
	}

	hashCache := txscript.NewTxSigHashes(sweepTx)

	// With all the inputs in place, use each output's unique witness
	// function to generate the final witness required for spending.
	addWitness := func(idx int, tso Input) error {
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
	for i, input := range inputs {
		if err := addWitness(i, input); err != nil {
			return nil, err
		}
	}

	return sweepTx, nil
}

func getInputWitnessSizeUpperBound(input Input) (int, error) {
	switch input.WitnessType() {

	// Outputs on a remote commitment transaction that pay directly
	// to us.
	case lnwallet.CommitmentNoDelay:
		return lnwallet.P2WKHWitnessSize, nil

	// Outputs on a past commitment transaction that pay directly
	// to us.
	case lnwallet.CommitmentTimeLock:
		return lnwallet.ToLocalTimeoutWitnessSize, nil

	// Outgoing second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case lnwallet.HtlcOfferedTimeoutSecondLevel:
		return lnwallet.ToLocalTimeoutWitnessSize, nil

	// Incoming second layer HTLC's that have confirmed within the
	// chain, and the output they produced is now mature enough to
	// sweep.
	case lnwallet.HtlcAcceptedSuccessSecondLevel:
		return lnwallet.ToLocalTimeoutWitnessSize, nil

	// An HTLC on the commitment transaction of the remote party,
	// that has had its absolute timelock expire.
	case lnwallet.HtlcOfferedRemoteTimeout:
		return lnwallet.AcceptedHtlcTimeoutWitnessSize, nil

	// An HTLC on the commitment transaction of the remote party,
	// that can be swept with the preimage.
	case lnwallet.HtlcAcceptedRemoteSuccess:
		return lnwallet.OfferedHtlcSuccessWitnessSize, nil

	}

	return 0, fmt.Errorf("unexpected witness type: %v", input.WitnessType())
}

// getWeightEstimate returns a weight estimate for the given inputs.
// Additionally, it returns counts for the number of csv and cltv inputs.
func getWeightEstimate(inputs []Input) ([]Input, int64, int, int) {
	// We initialize a weight estimator so we can accurately asses the
	// amount of fees we need to pay for this sweep transaction.
	//
	// TODO(roasbeef): can be more intelligent about buffering outputs to
	// be more efficient on-chain.
	var weightEstimate lnwallet.TxWeightEstimator

	// Our sweep transaction will pay to a single segwit p2wkh address,
	// ensure it contributes to our weight estimate.
	weightEstimate.AddP2WKHOutput()

	// For each output, use its witness type to determine the estimate
	// weight of its witness, and add it to the proper set of spendable
	// outputs.
	var (
		sweepInputs         []Input
		csvCount, cltvCount int
	)
	for i := range inputs {
		input := inputs[i]

		size, err := getInputWitnessSizeUpperBound(input)
		if err != nil {
			log.Warn(err)

			// Skip inputs for which no weight estimate can be
			// given.
			continue
		}
		weightEstimate.AddWitnessInput(size)

		switch input.WitnessType() {
		case lnwallet.CommitmentTimeLock,
			lnwallet.HtlcOfferedTimeoutSecondLevel,
			lnwallet.HtlcAcceptedSuccessSecondLevel:
			csvCount++
		case lnwallet.HtlcOfferedRemoteTimeout:
			cltvCount++
		}
		sweepInputs = append(sweepInputs, input)
	}

	txWeight := int64(weightEstimate.Weight())

	return sweepInputs, txWeight, csvCount, cltvCount
}
