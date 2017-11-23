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

// BtcdFeeEstimator is an implementation of the FeeEstimator interface backed
// by the RPC interface of an active btcd node. This implementation will proxy
// any fee estimation requests to btcd's RPC interace.
type BtcdFeeEstimator struct {
	// fallBackFeeRate is the fall back fee rate in satoshis per byte that
	// is returned if the fee estimator does not yet have enough data to
	// actually produce fee estimates.
	fallBackFeeRate btcutil.Amount

	btcdConn *rpcclient.Client
}

// NewBtcdFeeEstimator creates a new BtcdFeeEstimator given a fully populated
// rpc config that is able to successfully connect and authenticate with the
// btcd node, and also a fall back fee rate. The fallback fee rate is used in
// the occasion that the estimator has insufficient data, or returns zero for a
// fee estimate.
func NewBtcdFeeEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate btcutil.Amount) (*BtcdFeeEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	return &BtcdFeeEstimator{
		fallBackFeeRate: fallBackFeeRate,
		btcdConn:        chainConn,
	}, nil
}

// Start signals the FeeEstimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BtcdFeeEstimator) Start() error {
	if err := b.btcdConn.Connect(20); err != nil {
		return err
	}

	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BtcdFeeEstimator) Stop() error {
	b.btcdConn.Shutdown()

	return nil
}

// EstimateFeePerByte takes in a target for the number of blocks until an
// initial confirmation and returns the estimated fee expressed in
// satoshis/byte.
func (b *BtcdFeeEstimator) EstimateFeePerByte(numBlocks uint32) (btcutil.Amount, error) {
	feeEstimate, err := b.fetchEstimatePerByte(numBlocks)
	switch {
	// If the estimator doesn't have enough data, or returns an error, then
	// to return a proper value, then we'll return the default fall back
	// fee rate.
	case err != nil:
		walletLog.Errorf("unable to query estimator: %v", err)
		fallthrough

	case feeEstimate == 0:
		return b.fallBackFeeRate, nil
	}

	return feeEstimate, nil
}

// EstimateFeePerWeight takes in a target for the number of blocks until an
// initial confirmation and returns the estimated fee expressed in
// satoshis/weight.
func (b *BtcdFeeEstimator) EstimateFeePerWeight(numBlocks uint32) (btcutil.Amount, error) {
	feePerByte, err := b.EstimateFeePerByte(numBlocks)
	if err != nil {
		return 0, err
	}

	// We'll scale down the fee per byte to fee per weight, as for each raw
	// byte, there's 1/4 unit of weight mapped to it.
	return btcutil.Amount(feePerByte / blockchain.WitnessScaleFactor), nil
}

// fetchEstimate returns a fee estimate for a transaction be be confirmed in
// confTarget blocks. The estimate is returned in sat/byte.
func (b *BtcdFeeEstimator) fetchEstimatePerByte(confTarget uint32) (btcutil.Amount, error) {
	// First, we'll fetch the estimate for our confirmation target.
	btcPerKB, err := b.btcdConn.EstimateFee(int64(confTarget))
	if err != nil {
		return 0, err
	}

	// Next, we'll convert the returned value to satoshis, as it's
	// currently returned in BTC.
	satPerKB := uint64(btcPerKB * 10e8)

	// The value returned is expressed in fees per KB, while we want
	// fee-per-byte, so we'll divide by 1024 to map to satoshis-per-byte
	// before returning the estimate.
	satPerByte := btcutil.Amount(satPerKB / 1024)

	walletLog.Debugf("Returning %v sat/byte for conf target of %v",
		int64(satPerByte), confTarget)

	return satPerByte, nil
}

// A compile-time assertion to ensure that BtcdFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*BtcdFeeEstimator)(nil)
