package lnwallet

import (
	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcutil"
)

// FeeEstimator provides the ability to estimate on-chain transaction fees for
// various combinations of transaction sizes and desired confirmation time
// (measured by number of blocks).
type FeeEstimator interface {
	// EstimateFeePerByte takes in a target for the number of blocks until
	// an initial confirmation and returns the estimated fee expressed in
	// satoshis/byte.
	EstimateFeePerByte(numBlocks uint32) (btcutil.Amount, error)

	// EstimateFeePerWeight takes in a target for the number of blocks
	// until an initial confirmation and returns the estimated fee
	// expressed in satoshis/weight.
	EstimateFeePerWeight(numBlocks uint32) (btcutil.Amount, error)

	// Start signals the FeeEstimator to start any processes or goroutines
	// it needs to perform its duty.
	Start() error

	// Stop stops any spawned goroutines and cleans up the resources used
	// by the fee estimator.
	Stop() error
}

// StaticFeeEstimator will return a static value for all fee calculation
// requests. It is designed to be replaced by a proper fee calculation
// implementation.
type StaticFeeEstimator struct {
	// FeeRate is the static fee rate in satoshis-per-byte that will be
	// returned by this fee estimator. Queries for the fee rate in weight
	// units will be scaled accordingly.
	FeeRate btcutil.Amount
}

// EstimateFeePerByte will return a static value for fee calculations.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) EstimateFeePerByte(numBlocks uint32) (btcutil.Amount, error) {
	return e.FeeRate, nil
}

// EstimateFeePerWeight will return a static value for fee calculations.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) EstimateFeePerWeight(numBlocks uint32) (btcutil.Amount, error) {
	return e.FeeRate / blockchain.WitnessScaleFactor, nil
}

// Start signals the FeeEstimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) Start() error {
	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) Stop() error {
	return nil
}

// A compile-time assertion to ensure that StaticFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*StaticFeeEstimator)(nil)
