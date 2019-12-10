package sweep

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// txInputSet is an object that accumulates tx inputs and keeps running counters
// on various properties of the tx.
type txInputSet struct {
	// weightEstimate is the (worst case) tx weight with the current set of
	// inputs.
	weightEstimate input.TxWeightEstimator

	// inputTotal is the total value of all inputs.
	inputTotal btcutil.Amount

	// outputValue is the value of the tx output.
	outputValue btcutil.Amount

	// feePerKW is the fee rate used to calculate the tx fee.
	feePerKW chainfee.SatPerKWeight

	// inputs is the set of tx inputs.
	inputs []input.Input
}

// newTxInputSet constructs a new, empty input set.
func newTxInputSet(feePerKW chainfee.SatPerKWeight) *txInputSet {
	b := txInputSet{
		feePerKW: feePerKW,
	}

	// Add the sweep tx output to the weight estimate.
	b.weightEstimate.AddP2WKHOutput()

	return &b
}

// add adds a new input to the set. It returns a bool indicating whether the
// input was added to the set. An input is rejected if it decreases the tx
// output value after paying fees.
func (b *txInputSet) add(input txInput) bool {
	// Can ignore error, because it has already been checked when
	// calculating the yields.
	size, isNestedP2SH, _ := input.WitnessType().SizeUpperBound()

	// Keep a running weight estimate of the input set.
	if isNestedP2SH {
		b.weightEstimate.AddNestedP2WSHInput(size)
	} else {
		b.weightEstimate.AddWitnessInput(size)
	}

	value := btcutil.Amount(input.SignDesc().Output.Value)
	newInputTotal := b.inputTotal + value

	weight := b.weightEstimate.Weight()
	fee := b.feePerKW.FeeForWeight(int64(weight))

	// Calculate the output value if the current input would be
	// added to the set.
	newOutputValue := newInputTotal - fee

	// If adding this input makes the total output value of the set
	// decrease, this is a negative yield input. It shouldn't be
	// added to the set. We return the current index as the number
	// of inputs, so the current input is being excluded.
	if newOutputValue <= b.outputValue {
		return false
	}

	// Update running values.
	b.inputTotal = newInputTotal
	b.outputValue = newOutputValue
	b.inputs = append(b.inputs, input)

	return true
}
