package lnwallet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcutil"
)

const (
	// FeePerKwFloor is the lowest fee rate in sat/kw that we should use for
	// determining transaction fees.
	FeePerKwFloor SatPerKWeight = 253
)

// SatPerKVByte represents a fee rate in sat/kb.
type SatPerKVByte btcutil.Amount

// FeeForVSize calculates the fee resulting from this fee rate and the given
// vsize in vbytes.
func (s SatPerKVByte) FeeForVSize(vbytes int64) btcutil.Amount {
	return btcutil.Amount(s) * btcutil.Amount(vbytes) / 1000
}

// FeePerKWeight converts the current fee rate from sat/kb to sat/kw.
func (s SatPerKVByte) FeePerKWeight() SatPerKWeight {
	return SatPerKWeight(s / blockchain.WitnessScaleFactor)
}

// SatPerKWeight represents a fee rate in sat/kw.
type SatPerKWeight btcutil.Amount

// FeeForWeight calculates the fee resulting from this fee rate and the given
// weight in weight units (wu).
func (s SatPerKWeight) FeeForWeight(wu int64) btcutil.Amount {
	// The resulting fee is rounded down, as specified in BOLT#03.
	return btcutil.Amount(s) * btcutil.Amount(wu) / 1000
}

// FeePerKVByte converts the current fee rate from sat/kw to sat/kb.
func (s SatPerKWeight) FeePerKVByte() SatPerKVByte {
	return SatPerKVByte(s * blockchain.WitnessScaleFactor)
}

// FeeEstimator provides the ability to estimate on-chain transaction fees for
// various combinations of transaction sizes and desired confirmation time
// (measured by number of blocks).
type FeeEstimator interface {
	// EstimateFeePerKW takes in a target for the number of blocks until an
	// initial confirmation and returns the estimated fee expressed in
	// sat/kw.
	EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error)

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
	// FeePerKW is the static fee rate in satoshis-per-vbyte that will be
	// returned by this fee estimator.
	FeePerKW SatPerKWeight
}

// EstimateFeePerKW will return a static value for fee calculations.
//
// NOTE: This method is part of the FeeEstimator interface.
func (e StaticFeeEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	return e.FeePerKW, nil
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
	// fallbackFeePerKW is the fall back fee rate in sat/kw that is returned
	// if the fee estimator does not yet have enough data to actually
	// produce fee estimates.
	fallbackFeePerKW SatPerKWeight

	// minFeePerKW is the minimum fee, in sat/kw, that we should enforce.
	// This will be used as the default fee rate for a transaction when the
	// estimated fee rate is too low to allow the transaction to propagate
	// through the network.
	minFeePerKW SatPerKWeight

	btcdConn *rpcclient.Client
}

// NewBtcdFeeEstimator creates a new BtcdFeeEstimator given a fully populated
// rpc config that is able to successfully connect and authenticate with the
// btcd node, and also a fall back fee rate. The fallback fee rate is used in
// the occasion that the estimator has insufficient data, or returns zero for a
// fee estimate.
func NewBtcdFeeEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate SatPerKWeight) (*BtcdFeeEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	return &BtcdFeeEstimator{
		fallbackFeePerKW: fallBackFeeRate,
		btcdConn:         chainConn,
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

	// The fee rate is expressed in sat/kb, so we'll manually convert it to
	// our desired sat/kw rate.
	minRelayFeePerKw := SatPerKVByte(relayFee).FeePerKWeight()

	// By default, we'll use the backend node's minimum relay fee as the
	// minimum fee rate we'll propose for transacations. However, if this
	// happens to be lower than our fee floor, we'll enforce that instead.
	b.minFeePerKW = minRelayFeePerKw
	if b.minFeePerKW < FeePerKwFloor {
		b.minFeePerKW = FeePerKwFloor
	}

	walletLog.Debugf("Using minimum fee rate of %v sat/kw",
		int64(b.minFeePerKW))

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

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BtcdFeeEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	feeEstimate, err := b.fetchEstimate(numBlocks)
	switch {
	// If the estimator doesn't have enough data, or returns an error, then
	// to return a proper value, then we'll return the default fall back
	// fee rate.
	case err != nil:
		walletLog.Errorf("unable to query estimator: %v", err)
		fallthrough

	case feeEstimate == 0:
		return b.fallbackFeePerKW, nil
	}

	return feeEstimate, nil
}

// fetchEstimate returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/kw.
func (b *BtcdFeeEstimator) fetchEstimate(confTarget uint32) (SatPerKWeight, error) {
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

	// Since we use fee rates in sat/kw internally, we'll convert the
	// estimated fee rate from its sat/kb representation to sat/kw.
	satPerKw := SatPerKVByte(satPerKB).FeePerKWeight()

	// Finally, we'll enforce our fee floor.
	if satPerKw < b.minFeePerKW {
		walletLog.Debugf("Estimated fee rate of %v sat/kw is too low, "+
			"using fee floor of %v sat/kw instead", b.minFeePerKW)
		satPerKw = b.minFeePerKW
	}

	walletLog.Debugf("Returning %v sat/kw for conf target of %v",
		int64(satPerKw), confTarget)

	return satPerKw, nil
}

// A compile-time assertion to ensure that BtcdFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*BtcdFeeEstimator)(nil)

// BitcoindFeeEstimator is an implementation of the FeeEstimator interface
// backed by the RPC interface of an active bitcoind node. This implementation
// will proxy any fee estimation requests to bitcoind's RPC interface.
type BitcoindFeeEstimator struct {
	// fallbackFeePerKW is the fallback fee rate in sat/kw that is returned
	// if the fee estimator does not yet have enough data to actually
	// produce fee estimates.
	fallbackFeePerKW SatPerKWeight

	// minFeePerKW is the minimum fee, in sat/kw, that we should enforce.
	// This will be used as the default fee rate for a transaction when the
	// estimated fee rate is too low to allow the transaction to propagate
	// through the network.
	minFeePerKW SatPerKWeight

	bitcoindConn *rpcclient.Client
}

// NewBitcoindFeeEstimator creates a new BitcoindFeeEstimator given a fully
// populated rpc config that is able to successfully connect and authenticate
// with the bitcoind node, and also a fall back fee rate. The fallback fee rate
// is used in the occasion that the estimator has insufficient data, or returns
// zero for a fee estimate.
func NewBitcoindFeeEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate SatPerKWeight) (*BitcoindFeeEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	rpcConfig.DisableTLS = true
	rpcConfig.HTTPPostMode = true
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	return &BitcoindFeeEstimator{
		fallbackFeePerKW: fallBackFeeRate,
		bitcoindConn:     chainConn,
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

	// The fee rate is expressed in sat/kb, so we'll manually convert it to
	// our desired sat/kw rate.
	minRelayFeePerKw := SatPerKVByte(relayFee).FeePerKWeight()

	// By default, we'll use the backend node's minimum relay fee as the
	// minimum fee rate we'll propose for transacations. However, if this
	// happens to be lower than our fee floor, we'll enforce that instead.
	b.minFeePerKW = minRelayFeePerKw
	if b.minFeePerKW < FeePerKwFloor {
		b.minFeePerKW = FeePerKwFloor
	}

	walletLog.Debugf("Using minimum fee rate of %v sat/kw",
		int64(b.minFeePerKW))

	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BitcoindFeeEstimator) Stop() error {
	return nil
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the FeeEstimator interface.
func (b *BitcoindFeeEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	feeEstimate, err := b.fetchEstimate(numBlocks)
	switch {
	// If the estimator doesn't have enough data, or returns an error, then
	// to return a proper value, then we'll return the default fall back
	// fee rate.
	case err != nil:
		walletLog.Errorf("unable to query estimator: %v", err)
		fallthrough

	case feeEstimate == 0:
		return b.fallbackFeePerKW, nil
	}

	return feeEstimate, nil
}

// fetchEstimate returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/kw.
func (b *BitcoindFeeEstimator) fetchEstimate(confTarget uint32) (SatPerKWeight, error) {
	// First, we'll send an "estimatesmartfee" command as a raw request,
	// since it isn't supported by btcd but is available in bitcoind.
	target, err := json.Marshal(uint64(confTarget))
	if err != nil {
		return 0, err
	}
	// TODO: Allow selection of economical/conservative modifiers.
	resp, err := b.bitcoindConn.RawRequest(
		"estimatesmartfee", []json.RawMessage{target},
	)
	if err != nil {
		return 0, err
	}

	// Next, we'll parse the response to get the BTC per KB.
	feeEstimate := struct {
		FeeRate float64 `json:"feerate"`
	}{}
	err = json.Unmarshal(resp, &feeEstimate)
	if err != nil {
		return 0, err
	}

	// Next, we'll convert the returned value to satoshis, as it's currently
	// returned in BTC.
	satPerKB, err := btcutil.NewAmount(feeEstimate.FeeRate)
	if err != nil {
		return 0, err
	}

	// Since we use fee rates in sat/kw internally, we'll convert the
	// estimated fee rate from its sat/kb representation to sat/kw.
	satPerKw := SatPerKVByte(satPerKB).FeePerKWeight()

	// Finally, we'll enforce our fee floor.
	if satPerKw < b.minFeePerKW {
		walletLog.Debugf("Estimated fee rate of %v sat/kw is too low, "+
			"using fee floor of %v sat/kw instead", satPerKw,
			b.minFeePerKW)

		satPerKw = b.minFeePerKW
	}

	walletLog.Debugf("Returning %v sat/kw for conf target of %v",
		int64(satPerKw), confTarget)

	return satPerKw, nil
}

// A compile-time assertion to ensure that BitcoindFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*BitcoindFeeEstimator)(nil)

// WebApiFeeSource is an interface allows the WebApiFeeEstimator to query an
// arbitrary HTTP-based fee estimator. Each new set/network will gain an
// implementation of this interface in order to allow the WebApiFeeEstimator to
// be fully generic in its logic.
type WebApiFeeSource interface {
	// GenQueryURL generates the full query URL given a number of blocks to
	// query for fee estimation for. The value returned by this method
	// should be able to be used directly as a path for an HTTP GET
	// request.
	GenQueryURL(numBlocks uint32) string

	// ParseResponse attempts to parse the body of the response generated
	// by the above query URL. Typically this will be JSON, but the
	// specifics are left to the WebApiFeeSource implementation.
	ParseResponse(r io.Reader) (SatPerKWeight, error)
}

// TestnetBitGoFeeSource is an implemtatnion of the WebApiFeeSource that access
// the BitGo testnet fee estimation API for Bitcoin.
type TestnetBitGoFeeSource struct{}

// GenQueryURL generates the full query URL given a number of blocks to query
// for fee estimation for. The value returned by this method should be able to
// be used directly as a path for an HTTP GET request.
//
// NOTE: Part of the WebApiFeeSource interface.
func (t TestnetBitGoFeeSource) GenQueryURL(numBlocks uint32) string {
	urlTemplate := "https://test.bitgo.com/api/v2/tbtc/tx/fee?numBlocks=%v"
	return fmt.Sprintf(urlTemplate, numBlocks)
}

// ParseResponse attempts to parse the body of the response generated by the
// above query URL. Typically this will be JSON, but the specifics are left to
// the WebApiFeeSource implementation.
//
// NOTE: Part of the WebApiFeeSource interface.
func (t TestnetBitGoFeeSource) ParseResponse(r io.Reader) (SatPerKWeight, error) {
	// jsonResp is a struct that we'll use to decode the response sent by
	// the API. We only capture the fields that we need for our response.
	type jsonResp struct {
		FeePerKb     uint64  `json:"feePerKb"`
		CpfpFeePerKb uint64  `json:"cpfpFeePerKb"`
		NumBlocks    uint64  `json:"numBlocks"`
		Confidence   uint64  `json:"confidence"`
		Multiplier   float64 `json:"multiplier"`

		FeeByBlockTarget map[string]uint32 `json:feeByBlockTarget"`

		// TODO(roasbeef): can add rest of fees if needed
	}

	kresp, err := ioutil.ReadAll(r)
	if err != nil {
		return 0, err
	}

	// We expect the response to be in JSON, so we'll a new decoder that
	// can parse the JSON directly from the response reader.
	var resp jsonResp
	dec := bytes.NewReader(kresp)
	jsonReader := json.NewDecoder(dec)
	if err := jsonReader.Decode(&resp); err != nil {
		return 0, err
	}

	return SatPerKVByte(resp.FeePerKb).FeePerKWeight(), nil
}

// A compile-time assertion to ensure that TestnetBitGoFeeSource implements the
// WebApiFeeEstimator interface.
var _ WebApiFeeSource = (*TestnetBitGoFeeSource)(nil)

// WebApiFeeEstimator is an implementation of the FeeEstimator interface that
// queries an HTTP-based fee estimation from an existing web API.
type WebApiFeeEstimator struct {
	// apiSource is the backing web API source we'll use for our queries.
	apiSource WebApiFeeSource

	// defaultFeePerkw is a fallback value that we'll use if we're unable
	// to query the API for w/e reason.
	defaultFeePerkw SatPerKWeight
}

// NewWebApiFeeSource creates a new WebApiFeeSource from a given
// WebApiFeeSource and a fall back default fee.
func NewWebApiFeeSource(api WebApiFeeSource, defaultFee SatPerKWeight) *WebApiFeeEstimator {
	return &WebApiFeeEstimator{
		apiSource:       api,
		defaultFeePerkw: defaultFee,
	}
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the FeeEstimator interface.
func (w WebApiFeeEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	// Rather than use the default http.Client, we'll make a custom one
	// which will allow us to control how long we'll wait to read the
	// response from the service. This way, if the service is down or
	// overloaded, we can exit early and use our default fee.
	netTransport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	netClient := &http.Client{
		Timeout:   time.Second * 10,
		Transport: netTransport,
	}

	// With the client created, we'll query the API source to fetch the URL
	// that we should use to query for the fee estimation.
	targetURL := w.apiSource.GenQueryURL(numBlocks)
	resp, err := netClient.Get(targetURL)
	if err != nil {
		// If we're unable to dial for w/e reason, then we'll log the
		// error, but return the default static fee.
		walletLog.Errorf("unable to query web api for fee response: %v", err)
		return w.defaultFeePerkw, nil
	}
	defer resp.Body.Close()

	// Once we've obtained the response, we'll instruct the WebApiFeeSource
	// to parse out the body to obtain our final result.
	satPerKw, err := w.apiSource.ParseResponse(resp.Body)
	if err != nil {
		// If we're unable to dial for w/e reason, then we'll log the
		// error, but return the default static fee.
		walletLog.Errorf("unable to query web api for fee response: %v", err)
		return w.defaultFeePerkw, nil
	}

	// If the result is too low, then we'll clamp it to our current fee
	// floor.
	if satPerKw < FeePerKwFloor {
		satPerKw = FeePerKwFloor
	}

	walletLog.Debugf("Web API returning %v sat/kw for conf target of %v",
		int64(satPerKw), numBlocks)

	return satPerKw, nil
}

// Start signals the FeeEstimator to start any processes or goroutines it needs
// to perform its duty.
//
// NOTE: This method is part of the FeeEstimator interface.
func (w WebApiFeeEstimator) Start() error {
	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used by the
// fee estimator.
//
// NOTE: This method is part of the FeeEstimator interface.
func (w WebApiFeeEstimator) Stop() error {
	return nil
}

// A compile-time assertion to ensure that WebApiFeeEstimator implements the
// FeeEstimator interface.
var _ FeeEstimator = (*WebApiFeeEstimator)(nil)
