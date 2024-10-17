package sweep

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// DefaultMaxInputsPerTx specifies the default maximum number of inputs
	// allowed in a single sweep tx. If more need to be swept, multiple txes
	// are created and published.
	DefaultMaxInputsPerTx = uint32(100)

	// ErrLocktimeConflict is returned when inputs with different
	// transaction nLockTime values are included in the same transaction.
	//
	// NOTE: due the SINGLE|ANYONECANPAY sighash flag, which is used in the
	// second level success/timeout txns, only the txns sharing the same
	// nLockTime can exist in the same tx.
	ErrLocktimeConflict = errors.New("incompatible locktime")
)

// createSweepTx builds a signed tx spending the inputs to the given outputs,
// sending any leftover change to the change script.
func createSweepTx(inputs []input.Input, outputs []*wire.TxOut,
	changePkScript []byte, currentBlockHeight uint32,
	feeRate, maxFeeRate chainfee.SatPerKWeight,
	signer input.Signer) (*wire.MsgTx, btcutil.Amount, error) {

	inputs, estimator, err := getWeightEstimate(
		inputs, outputs, feeRate, maxFeeRate, [][]byte{changePkScript},
	)
	if err != nil {
		return nil, 0, err
	}

	txFee := estimator.feeWithParent()

	var (
		// Create the sweep transaction that we will be building. We
		// use version 2 as it is required for CSV.
		sweepTx = wire.NewMsgTx(2)

		// Track whether any of the inputs require a certain locktime.
		locktime = int32(-1)

		// We keep track of total input amount, and required output
		// amount to use for calculating the change amount below.
		totalInput     btcutil.Amount
		requiredOutput btcutil.Amount

		// We'll add the inputs as we go so we know the final ordering
		// of inputs to sign.
		idxs []input.Input
	)

	// We start by adding all inputs that commit to an output. We do this
	// since the input and output index must stay the same for the
	// signatures to be valid.
	for _, o := range inputs {
		if o.RequiredTxOut() == nil {
			continue
		}

		idxs = append(idxs, o)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: o.OutPoint(),
			Sequence:         o.BlocksToMaturity(),
		})
		sweepTx.AddTxOut(o.RequiredTxOut())

		if lt, ok := o.RequiredLockTime(); ok {
			// If another input commits to a different locktime,
			// they cannot be combined in the same transaction.
			if locktime != -1 && locktime != int32(lt) {
				return nil, 0, ErrLocktimeConflict
			}

			locktime = int32(lt)
		}

		totalInput += btcutil.Amount(o.SignDesc().Output.Value)
		requiredOutput += btcutil.Amount(o.RequiredTxOut().Value)
	}

	// Sum up the value contained in the remaining inputs, and add them to
	// the sweep transaction.
	for _, o := range inputs {
		if o.RequiredTxOut() != nil {
			continue
		}

		idxs = append(idxs, o)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: o.OutPoint(),
			Sequence:         o.BlocksToMaturity(),
		})

		if lt, ok := o.RequiredLockTime(); ok {
			if locktime != -1 && locktime != int32(lt) {
				return nil, 0, ErrLocktimeConflict
			}

			locktime = int32(lt)
		}

		totalInput += btcutil.Amount(o.SignDesc().Output.Value)
	}

	// Add the outputs given, if any.
	for _, o := range outputs {
		sweepTx.AddTxOut(o)
		requiredOutput += btcutil.Amount(o.Value)
	}

	if requiredOutput+txFee > totalInput {
		return nil, 0, fmt.Errorf("insufficient input to create sweep "+
			"tx: input_sum=%v, output_sum=%v", totalInput,
			requiredOutput+txFee)
	}

	// The value remaining after the required output and fees, go to
	// change. Not that this fee is what we would have to pay in case the
	// sweep tx has a change output.
	changeAmt := totalInput - requiredOutput - txFee

	// We'll calculate the dust limit for the given changePkScript since it
	// is variable.
	changeLimit := lnwallet.DustLimitForSize(len(changePkScript))

	// The txn will sweep the amount after fees to the pkscript generated
	// above.
	if changeAmt >= changeLimit {
		sweepTx.AddTxOut(&wire.TxOut{
			PkScript: changePkScript,
			Value:    int64(changeAmt),
		})
	} else {
		log.Infof("Change amt %v below dustlimit %v, not adding "+
			"change output", changeAmt, changeLimit)

		// The dust amount is added to the fee as the miner will
		// collect it.
		txFee += changeAmt
	}

	// We'll default to using the current block height as locktime, if none
	// of the inputs commits to a different locktime.
	sweepTx.LockTime = currentBlockHeight
	if locktime != -1 {
		sweepTx.LockTime = uint32(locktime)
	}

	// Before signing the transaction, check to ensure that it meets some
	// basic validity requirements.
	//
	// TODO(conner): add more control to sanity checks, allowing us to
	// delay spending "problem" outputs, e.g. possibly batching with other
	// classes if fees are too low.
	btx := btcutil.NewTx(sweepTx)
	if err := blockchain.CheckTransactionSanity(btx); err != nil {
		return nil, 0, err
	}

	prevInputFetcher, err := input.MultiPrevOutFetcher(inputs)
	if err != nil {
		return nil, 0, fmt.Errorf("error creating prev input fetcher "+
			"for hash cache: %v", err)
	}
	hashCache := txscript.NewTxSigHashes(sweepTx, prevInputFetcher)

	// With all the inputs in place, use each output's unique input script
	// function to generate the final witness required for spending.
	addInputScript := func(idx int, tso input.Input) error {
		inputScript, err := tso.CraftInputScript(
			signer, sweepTx, hashCache, prevInputFetcher, idx,
		)
		if err != nil {
			return err
		}

		sweepTx.TxIn[idx].Witness = inputScript.Witness

		if len(inputScript.SigScript) != 0 {
			sweepTx.TxIn[idx].SignatureScript =
				inputScript.SigScript
		}

		return nil
	}

	for idx, inp := range idxs {
		if err := addInputScript(idx, inp); err != nil {
			return nil, 0, err
		}
	}

	log.Debugf("Creating sweep transaction %v for %v inputs (%s) "+
		"using %v, tx_weight=%v, tx_fee=%v, parents_count=%v, "+
		"parents_fee=%v, parents_weight=%v, current_height=%v",
		sweepTx.TxHash(), len(inputs),
		inputTypeSummary(inputs), feeRate,
		estimator.weight(), txFee,
		len(estimator.parents), estimator.parentsFee,
		estimator.parentsWeight, currentBlockHeight)

	return sweepTx, txFee, nil
}

// getWeightEstimate returns a weight estimate for the given inputs.
// Additionally, it returns counts for the number of csv and cltv inputs.
func getWeightEstimate(inputs []input.Input, outputs []*wire.TxOut,
	feeRate, maxFeeRate chainfee.SatPerKWeight,
	outputPkScripts [][]byte) ([]input.Input, *weightEstimator, error) {

	// We initialize a weight estimator so we can accurately asses the
	// amount of fees we need to pay for this sweep transaction.
	//
	// TODO(roasbeef): can be more intelligent about buffering outputs to
	// be more efficient on-chain.
	weightEstimate := newWeightEstimator(feeRate, maxFeeRate)

	// Our sweep transaction will always pay to the given set of outputs.
	for _, o := range outputs {
		weightEstimate.addOutput(o)
	}

	// If there is any leftover change after paying to the given outputs
	// and required outputs, it will go to a single segwit p2wkh or p2tr
	// address. This will be our change address, so ensure it contributes
	// to our weight estimate. Note that if we have other outputs, we might
	// end up creating a sweep tx without a change output. It is okay to
	// add the change output to the weight estimate regardless, since the
	// estimated fee will just be subtracted from this already dust output,
	// and trimmed.
	for _, outputPkScript := range outputPkScripts {
		switch {
		case txscript.IsPayToTaproot(outputPkScript):
			weightEstimate.addP2TROutput()

		case txscript.IsPayToWitnessScriptHash(outputPkScript):
			weightEstimate.addP2WSHOutput()

		case txscript.IsPayToWitnessPubKeyHash(outputPkScript):
			weightEstimate.addP2WKHOutput()

		case txscript.IsPayToPubKeyHash(outputPkScript):
			weightEstimate.estimator.AddP2PKHOutput()

		case txscript.IsPayToScriptHash(outputPkScript):
			weightEstimate.estimator.AddP2SHOutput()

		default:
			// Unknown script type.
			return nil, nil, fmt.Errorf("unknown script "+
				"type: %x", outputPkScript)
		}
	}

	// For each output, use its witness type to determine the estimate
	// weight of its witness, and add it to the proper set of spendable
	// outputs.
	var sweepInputs []input.Input
	for i := range inputs {
		inp := inputs[i]

		err := weightEstimate.add(inp)
		if err != nil {
			// TODO(yy): check if this is even possible? If so, we
			// should return the error here instead of filtering!
			log.Errorf("Failed to get weight estimate for "+
				"input=%v, witnessType=%v: %v ", inp.OutPoint(),
				inp.WitnessType(), err)

			// Skip inputs for which no weight estimate can be
			// given.
			continue
		}

		// If this input comes with a committed output, add that as
		// well.
		if inp.RequiredTxOut() != nil {
			weightEstimate.addOutput(inp.RequiredTxOut())
		}

		sweepInputs = append(sweepInputs, inp)
	}

	return sweepInputs, weightEstimate, nil
}

// inputSummary returns a string containing a human readable summary about the
// witness types of a list of inputs.
func inputTypeSummary(inputs []input.Input) string {
	// Sort inputs by witness type.
	sortedInputs := make([]input.Input, len(inputs))
	copy(sortedInputs, inputs)
	sort.Slice(sortedInputs, func(i, j int) bool {
		return sortedInputs[i].WitnessType().String() <
			sortedInputs[j].WitnessType().String()
	})

	var parts []string
	for _, i := range sortedInputs {
		part := fmt.Sprintf("%v (%v)", i.OutPoint(), i.WitnessType())
		parts = append(parts, part)
	}

	return strings.Join(parts, "\n")
}
