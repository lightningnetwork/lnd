package sweep

import (
	"github.com/lightningnetwork/lnd/input"
)

// weightEstimator wraps a standard weight estimator instance.
type weightEstimator struct {
	estimator input.TxWeightEstimator
}

// newWeightEstimator instantiates a new sweeper weight estimator.
func newWeightEstimator() *weightEstimator {
	return &weightEstimator{}
}

// clone returns a copy of this weight estimator.
func (w *weightEstimator) clone() *weightEstimator {
	return &weightEstimator{
		estimator: w.estimator,
	}
}

// add adds the weight of the given input to the weight estimate.
func (w *weightEstimator) add(inp input.Input) error {
	wt := inp.WitnessType()

	return wt.AddWeightEstimation(&w.estimator)
}

// addP2WKHOutput updates the weight estimate to account for an additional
// native P2WKH output.
func (w *weightEstimator) addP2WKHOutput() {
	w.estimator.AddP2WKHOutput()
}

// weight gets the estimated weight of the transaction.
func (w *weightEstimator) weight() int {
	return w.estimator.Weight()
}
