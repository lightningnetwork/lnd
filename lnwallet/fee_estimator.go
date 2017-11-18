package lnwallet

// FeeEstimator provides the ability to estimate on-chain transaction fees for
// various combinations of transaction sizes and desired confirmation time
// (measured by number of blocks).
type FeeEstimator interface {
	// EstimateFeePerByte takes in a target for the number of blocks until
	// an initial confirmation and returns the estimated fee expressed in
	// satoshis/byte.
	EstimateFeePerByte(numBlocks uint32) uint64

	// EstimateFeePerWeight takes in a target for the number of blocks
	// until an initial confirmation and returns the estimated fee
	// expressed in satoshis/weight.
	EstimateFeePerWeight(numBlocks uint32) uint64
}

// StaticFeeEstimator will return a static value for all fee calculation
// requests. It is designed to be replaced by a proper fee calculation
// implementation.
type StaticFeeEstimator struct {
	// FeeRate is the static fee rate in satoshis-per-byte that will be
	// returned by this fee estimator. Queries for the fee rate in weight
	// units will be scaled accordingly.
	FeeRate uint64
}

// EstimateFeePerByte will return a static value for fee calculations.
func (e StaticFeeEstimator) EstimateFeePerByte(numBlocks uint32) uint64 {
	return e.FeeRate
}

// EstimateFeePerWeight will return a static value for fee calculations.
func (e StaticFeeEstimator) EstimateFeePerWeight(numBlocks uint32) uint64 {
	return e.FeeRate / 4
}

// A compile-time assertion to ensure that StaticFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*StaticFeeEstimator)(nil)
