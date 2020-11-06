package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// weightEstimator wraps a standard weight estimator instance and adds to that
// support for child-pays-for-parent.
type weightEstimator struct {
	estimator     input.TxWeightEstimator
	feeRate       chainfee.SatPerKWeight
	parents       map[chainhash.Hash]struct{}
	parentsFee    btcutil.Amount
	parentsWeight int64
}

// newWeightEstimator instantiates a new sweeper weight estimator.
func newWeightEstimator(feeRate chainfee.SatPerKWeight) *weightEstimator {
	return &weightEstimator{
		feeRate: feeRate,
		parents: make(map[chainhash.Hash]struct{}),
	}
}

// clone returns a copy of this weight estimator.
func (w *weightEstimator) clone() *weightEstimator {
	parents := make(map[chainhash.Hash]struct{}, len(w.parents))
	for hash := range w.parents {
		parents[hash] = struct{}{}
	}

	return &weightEstimator{
		estimator:     w.estimator,
		feeRate:       w.feeRate,
		parents:       parents,
		parentsFee:    w.parentsFee,
		parentsWeight: w.parentsWeight,
	}
}

// add adds the weight of the given input to the weight estimate.
func (w *weightEstimator) add(inp input.Input) error {
	// If there is a parent tx, add the parent's fee and weight.
	w.tryAddParent(inp)

	wt := inp.WitnessType()

	return wt.AddWeightEstimation(&w.estimator)
}

// tryAddParent examines the input and updates parent tx totals if required for
// cpfp.
func (w *weightEstimator) tryAddParent(inp input.Input) {
	// Get unconfirmed parent info from the input.
	unconfParent := inp.UnconfParent()

	// If there is no parent, there is nothing to add.
	if unconfParent == nil {
		return
	}

	// If we've already accounted for the parent tx, don't do it
	// again. This can happens when two outputs of the parent tx are
	// included in the same sweep tx.
	parentHash := inp.OutPoint().Hash
	if _, ok := w.parents[parentHash]; ok {
		return
	}

	// Calculate parent fee rate.
	parentFeeRate := chainfee.SatPerKWeight(unconfParent.Fee) * 1000 /
		chainfee.SatPerKWeight(unconfParent.Weight)

	// Ignore parents that pay at least the fee rate of this transaction.
	// Parent pays for child is not happening.
	if parentFeeRate >= w.feeRate {
		return
	}

	// Include parent.
	w.parents[parentHash] = struct{}{}
	w.parentsFee += unconfParent.Fee
	w.parentsWeight += unconfParent.Weight
}

// addP2WKHOutput updates the weight estimate to account for an additional
// native P2WKH output.
func (w *weightEstimator) addP2WKHOutput() {
	w.estimator.AddP2WKHOutput()
}

// addOutput updates the weight estimate to account for the known
// output given.
func (w *weightEstimator) addOutput(txOut *wire.TxOut) {
	w.estimator.AddTxOutput(txOut)
}

// weight gets the estimated weight of the transaction.
func (w *weightEstimator) weight() int {
	return w.estimator.Weight()
}

// fee returns the tx fee to use for the aggregated inputs and outputs, taking
// into account unconfirmed parent transactions (cpfp).
func (w *weightEstimator) fee() btcutil.Amount {
	// Calculate fee and weight for just this tx.
	childWeight := int64(w.estimator.Weight())

	// Add combined weight of unconfirmed parent txes.
	totalWeight := childWeight + w.parentsWeight

	// Subtract fee already paid by parents.
	fee := w.feeRate.FeeForWeight(totalWeight) - w.parentsFee

	// Clamp the fee to what would be required if no parent txes were paid
	// for. This is to make sure no rounding errors can get us into trouble.
	childFee := w.feeRate.FeeForWeight(childWeight)
	if childFee > fee {
		fee = childFee
	}

	return fee
}
