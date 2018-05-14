package lnwallet

import (
	"encoding/json"

	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcutil"
)

// SatPerVByte represents a fee rate in satoshis per vbyte.
type SatPerVByte btcutil.Amount

// FeeForVSize calculates the fee resulting from this fee rate and
// the given vsize in vbytes.
func (s SatPerVByte) FeeForVSize(vbytes int64) btcutil.Amount {
	return btcutil.Amount(s) * btcutil.Amount(vbytes)
}

// FeePerKWeight converts the fee rate into SatPerKWeight.
func (s SatPerVByte) FeePerKWeight() SatPerKWeight {
	return SatPerKWeight(s * 1000 / blockchain.WitnessScaleFactor)
}

// SatPerKWeight represents a fee rate in satoshis per kilo weight unit.
type SatPerKWeight btcutil.Amount

// FeeForWeight calculates the fee resulting from this fee rate and the
// given weight in weight units (wu).
func (s SatPerKWeight) FeeForWeight(wu int64) btcutil.Amount {
	// The resulting fee is rounded down, as specified in BOLT#03.
	return btcutil.Amount(s) * btcutil.Amount(wu) / 1000
}

// FeeEstimator provides the ability to estimate on-chain transaction fees for
// various combinations of transaction sizes and desired confirmation time
// (measured by number of blocks).
type FeeEstimator interface {
	// EstimateFeePerVSize takes in a target for the number of blocks until
	// an initial confirmation and returns the estimated fee expressed in
	// satoshis/vbyte.
	EstimateFeePerVSize(numBlocks uint32) (SatPerVByte, error)

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
	// FeeRate is the static fee rate in satoshis-per-vbyte that will be
	// returned by this fee estimator.
	FeeRate SatPerVByte
}

// EstimateFeePerVSize will return a static value for fee calculations.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) EstimateFeePerVSize(numBlocks uint32) (SatPerVByte, error) {
	return e.FeeRate, nil
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
// any fee estimation requests to btcd's RPC interface.
type BtcdFeeEstimator struct {
	// fallBackFeeRate is the fall back fee rate in satoshis per vbyte that
	// is returned if the fee estimator does not yet have enough data to
	// actually produce fee estimates.
	fallBackFeeRate SatPerVByte

	// minFeeRate is the minimum relay fee, in sat/vbyte, of the backend
	// node. This will be used as the default fee rate of a transaction when
	// the estimated fee rate is too low to allow the transaction to
	// propagate through the network.
	minFeeRate SatPerVByte

	btcdConn *rpcclient.Client
}

// NewBtcdFeeEstimator creates a new BtcdFeeEstimator given a fully populated
// rpc config that is able to successfully connect and authenticate with the
// btcd node, and also a fall back fee rate. The fallback fee rate is used in
// the occasion that the estimator has insufficient data, or returns zero for a
// fee estimate.
func NewBtcdFeeEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate SatPerVByte) (*BtcdFeeEstimator, error) {

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

	// Once the connection to the backend node has been established, we'll
	// query it for its minimum relay fee.
	info, err := b.btcdConn.GetInfo()
	if err != nil {
		return err
	}

	relayFee, err := btcutil.NewAmount(info.RelayFee)
	if err != nil {
		return err
	}

	// The fee rate is expressed in sat/KB, so we'll manually convert it to
	// our desired sat/vbyte rate.
	b.minFeeRate = SatPerVByte(relayFee / 1000)

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

// EstimateFeePerVSize takes in a target for the number of blocks until an
// initial confirmation and returns the estimated fee expressed in
// satoshis/vbyte.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BtcdFeeEstimator) EstimateFeePerVSize(numBlocks uint32) (SatPerVByte, error) {
	feeEstimate, err := b.fetchEstimatePerVSize(numBlocks)
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

// fetchEstimate returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/vbyte.
func (b *BtcdFeeEstimator) fetchEstimatePerVSize(
	confTarget uint32) (SatPerVByte, error) {
	// First, we'll fetch the estimate for our confirmation target.
	btcPerKB, err := b.btcdConn.EstimateFee(int64(confTarget))
	if err != nil {
		return 0, err
	}

	// Next, we'll convert the returned value to satoshis, as it's
	// currently returned in BTC.
	satPerKB, err := btcutil.NewAmount(btcPerKB)
	if err != nil {
		return 0, err
	}

	// The value returned is expressed in fees per KB, while we want
	// fee-per-byte, so we'll divide by 1000 to map to satoshis-per-byte
	// before returning the estimate.
	satPerByte := SatPerVByte(satPerKB / 1000)

	// Before proceeding, we'll make sure that this fee rate respects the
	// minimum relay fee set on the backend node.
	if satPerByte < b.minFeeRate {
		walletLog.Debugf("Using backend node's minimum relay fee rate "+
			"of %v sat/vbyte", b.minFeeRate)
		satPerByte = b.minFeeRate
	}

	walletLog.Debugf("Returning %v sat/vbyte for conf target of %v",
		int64(satPerByte), confTarget)

	return satPerByte, nil
}

// A compile-time assertion to ensure that BtcdFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*BtcdFeeEstimator)(nil)

// BitcoindFeeEstimator is an implementation of the FeeEstimator interface
// backed by the RPC interface of an active bitcoind node. This implementation
// will proxy any fee estimation requests to bitcoind's RPC interface.
type BitcoindFeeEstimator struct {
	// fallBackFeeRate is the fall back fee rate in satoshis per vbyte that
	// is returned if the fee estimator does not yet have enough data to
	// actually produce fee estimates.
	fallBackFeeRate SatPerVByte

	// minFeeRate is the minimum relay fee, in sat/vbyte, of the backend
	// node. This will be used as the default fee rate of a transaction when
	// the estimated fee rate is too low to allow the transaction to
	// propagate through the network.
	minFeeRate SatPerVByte

	bitcoindConn *rpcclient.Client
}

// NewBitcoindFeeEstimator creates a new BitcoindFeeEstimator given a fully
// populated rpc config that is able to successfully connect and authenticate
// with the bitcoind node, and also a fall back fee rate. The fallback fee rate
// is used in the occasion that the estimator has insufficient data, or returns
// zero for a fee estimate.
func NewBitcoindFeeEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate SatPerVByte) (*BitcoindFeeEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	rpcConfig.DisableTLS = true
	rpcConfig.HTTPPostMode = true
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	return &BitcoindFeeEstimator{
		fallBackFeeRate: fallBackFeeRate,
		bitcoindConn:    chainConn,
	}, nil
}

// Start signals the FeeEstimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BitcoindFeeEstimator) Start() error {
	// Once the connection to the backend node has been established, we'll
	// query it for its minimum relay fee. Since the `getinfo` RPC has been
	// deprecated for `bitcoind`, we'll need to send a `getnetworkinfo`
	// command as a raw request.
	resp, err := b.bitcoindConn.RawRequest("getnetworkinfo", nil)
	if err != nil {
		return err
	}

	// Parse the response to retrieve the relay fee in sat/KB.
	info := struct {
		RelayFee float64 `json:"relayfee"`
	}{}
	if err := json.Unmarshal(resp, &info); err != nil {
		return err
	}

	relayFee, err := btcutil.NewAmount(info.RelayFee)
	if err != nil {
		return err
	}

	// The fee rate is expressed in sat/KB, so we'll manually convert it to
	// our desired sat/vbyte rate.
	b.minFeeRate = SatPerVByte(relayFee / 1000)

	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BitcoindFeeEstimator) Stop() error {
	return nil
}

// EstimateFeePerVSize takes in a target for the number of blocks until an
// initial confirmation and returns the estimated fee expressed in
// satoshis/vbyte.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BitcoindFeeEstimator) EstimateFeePerVSize(numBlocks uint32) (SatPerVByte, error) {
	feeEstimate, err := b.fetchEstimatePerVSize(numBlocks)
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

// fetchEstimatePerVSize returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/vbyte.
func (b *BitcoindFeeEstimator) fetchEstimatePerVSize(
	confTarget uint32) (SatPerVByte, error) {
	// First, we'll send an "estimatesmartfee" command as a raw request,
	// since it isn't supported by btcd but is available in bitcoind.
	target, err := json.Marshal(uint64(confTarget))
	if err != nil {
		return 0, err
	}
	// TODO: Allow selection of economical/conservative modifiers.
	resp, err := b.bitcoindConn.RawRequest("estimatesmartfee",
		[]json.RawMessage{target})
	if err != nil {
		return 0, err
	}

	// Next, we'll parse the response to get the BTC per KB.
	feeEstimate := struct {
		Feerate float64 `json:"feerate"`
	}{}
	err = json.Unmarshal(resp, &feeEstimate)
	if err != nil {
		return 0, err
	}

	// Next, we'll convert the returned value to satoshis, as it's
	// currently returned in BTC.
	satPerKB, err := btcutil.NewAmount(feeEstimate.Feerate)
	if err != nil {
		return 0, err
	}

	// The value returned is expressed in fees per KB, while we want
	// fee-per-byte, so we'll divide by 1000 to map to satoshis-per-byte
	// before returning the estimate.
	satPerByte := SatPerVByte(satPerKB / 1000)

	// Before proceeding, we'll make sure that this fee rate respects the
	// minimum relay fee set on the backend node.
	if satPerByte < b.minFeeRate {
		walletLog.Debugf("Using backend node's minimum relay fee rate "+
			"of %v sat/vbyte", b.minFeeRate)
		satPerByte = b.minFeeRate
	}

	walletLog.Debugf("Returning %v sat/vbyte for conf target of %v",
		int64(satPerByte), confTarget)

	return satPerByte, nil
}

// A compile-time assertion to ensure that BitcoindFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*BitcoindFeeEstimator)(nil)
