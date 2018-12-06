package sweep

import (
	"fmt"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// createSweepTx builds a signed tx spending the inputs to a the output script.
func createSweepTx(inputs []Input, outputPkScript []byte,
	currentBlockHeight uint32, feePerKw lnwallet.SatPerKWeight,
	signer lnwallet.Signer) (*wire.MsgTx, error) {

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
		PkScript: outputPkScript,
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
			signer, sweepTx, hashCache, idx,
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

// getInputWitnessSizeUpperBound returns the maximum length of the witness for
// the given input if it would be included in a tx.
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
