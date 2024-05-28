package sweep

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// weightEstimator wraps a standard weight estimator instance and adds to that
// support for child-pays-for-parent.
type weightEstimator struct {
	estimator     input.TxWeightEstimator
	feeRate       chainfee.SatPerKWeight
	parents       map[chainhash.Hash]struct{}
	parentsFee    btcutil.Amount
	parentsWeight lntypes.WeightUnit

	// maxFeeRate is the max allowed fee rate configured by the user.
	maxFeeRate chainfee.SatPerKWeight
}

// newWeightEstimator instantiates a new sweeper weight estimator.
func newWeightEstimator(
	feeRate, maxFeeRate chainfee.SatPerKWeight) *weightEstimator {

	return &weightEstimator{
		feeRate:    feeRate,
		maxFeeRate: maxFeeRate,
		parents:    make(map[chainhash.Hash]struct{}),
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

// addP2TROutput updates the weight estimate to account for an additional native
// SegWit v1 P2TR output.
func (w *weightEstimator) addP2TROutput() {
	w.estimator.AddP2TROutput()
}

// addP2WSHOutput updates the weight estimate to account for an additional
// segwit v0 P2WSH output.
func (w *weightEstimator) addP2WSHOutput() {
	w.estimator.AddP2WSHOutput()
}

// addOutput updates the weight estimate to account for the known
// output given.
func (w *weightEstimator) addOutput(txOut *wire.TxOut) {
	w.estimator.AddTxOutput(txOut)
}

// weight gets the estimated weight of the transaction.
func (w *weightEstimator) weight() lntypes.WeightUnit {
	return w.estimator.Weight()
}

// fee returns the tx fee to use for the aggregated inputs and outputs, which
// is different from feeWithParent as it doesn't take into account unconfirmed
// parent transactions.
func (w *weightEstimator) fee() btcutil.Amount {
	// Calculate the weight of the transaction.
	weight := w.estimator.Weight()

	// Calculate the fee.
	fee := w.feeRate.FeeForWeight(weight)

	return fee
}

// feeWithParent returns the tx fee to use for the aggregated inputs and
// outputs, taking into account unconfirmed parent transactions (cpfp).
func (w *weightEstimator) feeWithParent() btcutil.Amount {
	// Calculate fee and weight for just this tx.
	childWeight := w.estimator.Weight()

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

	// Exit early if maxFeeRate is not set.
	if w.maxFeeRate == 0 {
		return fee
	}

	// Clamp the fee to the max fee rate.
	maxFee := w.maxFeeRate.FeeForWeight(childWeight)
	if fee > maxFee {
		// Calculate the effective fee rate for logging.
		childFeeRate := chainfee.SatPerKWeight(
			fee * 1000 / btcutil.Amount(childWeight),
		)
		log.Warnf("Child fee rate %v exceeds max allowed fee rate %v, "+
			"returning fee %v instead of %v", childFeeRate,
			w.maxFeeRate, maxFee, fee)

		fee = maxFee
	}

	return fee
}
