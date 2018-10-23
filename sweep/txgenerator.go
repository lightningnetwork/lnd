package sweep

import (
	"fmt"
	"github.com/btcsuite/btcwallet/wallet/txrules"

	"sort"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// DefaultMaxInputsPerTx specifies the default maximum number of inputs
	// allowed in a single sweep tx. If more need to be swept, multiple txes
	// are created and published.
	DefaultMaxInputsPerTx = 100
)

type inputSet []Input

// generateInputPartitionings goes through all given inputs and constructs sets
// of inputs that can be used to generate a sensible transaction. Each set
// contains up to the configured maximum number of inputs. Negative yield inputs
// are skipped. No input sets with a total value after fees below the dust limit
// are not returned.
func generateInputPartitionings(sweepableInputs []Input,
	relayFeePerKW, feePerKW lnwallet.SatPerKWeight,
	maxInputsPerTx int) ([]inputSet, error) {

	// Calculate dust limit based on the P2WPKH output script of the sweep
	// txes.
	dustLimit := int64(txrules.GetDustThreshold(
		lnwallet.P2WPKHSize,
		btcutil.Amount(relayFeePerKW.FeePerKVByte())))

	log.Tracef("Sweepable inputs count: %v", len(sweepableInputs))

	// Sort input by yield. We will start constructing input sets starting
	// with the highest yield inputs. This is to prevent the construction of
	// a set with an output below the dust limit, causing the sweep process
	// to stop, while there are still higher value inputs available. It also
	// allows us to stop evaluating more inputs when the first input in this
	// ordering is encountered with a negative yield.
	//
	// Yield is calculated as the difference between value and added fee for
	// this input. The fee calculation excludes fee components that are
	// common to all inputs, as those wouldn't influence the order. The
	// single component that is differentiating is witness size.
	//
	// For witness size, the upper limit is taken. The actual size depends
	// on the signature length, which is not known yet at this point.
	yields := make(map[wire.OutPoint]int64)
	for _, input := range sweepableInputs {
		size, err := getInputWitnessSizeUpperBound(input)
		if err != nil {
			return nil, fmt.Errorf(
				"failed adding input weight: %v", err)
		}

		yields[*input.OutPoint()] = input.SignDesc().Output.Value -
			int64(feePerKW.FeeForWeight(int64(size)))
	}

	sort.Slice(sweepableInputs, func(i, j int) bool {
		return yields[*sweepableInputs[i].OutPoint()] >
			yields[*sweepableInputs[j].OutPoint()]
	})

	// Select blocks of inputs up to the configured maximum number.
	var sets []inputSet
	stopSweep := false
	for len(sweepableInputs) > 0 && !stopSweep {
		var weightEstimate lnwallet.TxWeightEstimator

		// Add the sweep tx output to the weight estimate.
		weightEstimate.AddP2WKHOutput()

		var inputList inputSet
		var total, outputValue int64
		for len(sweepableInputs) > 0 {
			input := sweepableInputs[0]
			sweepableInputs = sweepableInputs[1:]

			// Can ignore error, because it has already been checked
			// when calculating the yields.
			size, _ := getInputWitnessSizeUpperBound(input)

			weightEstimate.AddWitnessInput(size)

			newTotal := total +
				input.SignDesc().Output.Value

			weight := weightEstimate.Weight()
			fee := feePerKW.FeeForWeight(int64(weight))
			newOutputValue := newTotal - int64(fee)

			// If adding this input makes the total output value of
			// the set decrease, this is a negative yield input. It
			// shouldn't be added to the set. We also don't need to
			// consider further inputs. Because of the sorting by
			// value, subsequent inputs almost certainly have an
			// even higher negative yield.
			if newOutputValue <= outputValue {
				stopSweep = true
				break
			}

			inputList = append(inputList, input)
			total = newTotal
			outputValue = newOutputValue
			if len(inputList) >= maxInputsPerTx {
				break
			}
		}

		// We can get an empty input list if all of the inputs had a
		// negative yield. In that case, we can stop here.
		if len(inputList) == 0 {
			return sets, nil
		}

		// If the output value of this block of inputs does not reach
		// the dust limit, stop sweeping. Because of the sorting,
		// continuing with remaining inputs will only lead to sets with
		// a even lower output value.
		if outputValue < dustLimit {
			log.Debugf("Set value %v below dust limit of %v",
				outputValue, dustLimit)
			return sets, nil
		}

		sets = append(sets, inputList)
	}

	return sets, nil
}

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
