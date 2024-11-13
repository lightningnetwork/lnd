package chainfee

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	prand "math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightningnetwork/lnd/lnutils"
)

const (
	// MaxBlockTarget is the highest number of blocks confirmations that
	// a WebAPIEstimator will cache fees for. This number is chosen
	// because it's the highest number of confs bitcoind will return a fee
	// estimate for.
	MaxBlockTarget uint32 = 1008

	// minBlockTarget is the lowest number of blocks confirmations that
	// a WebAPIEstimator will cache fees for. Requesting an estimate for
	// less than this will result in an error.
	minBlockTarget uint32 = 1

	// WebAPIConnectionTimeout specifies the timeout value for connecting
	// to the api source.
	WebAPIConnectionTimeout = 5 * time.Second

	// WebAPIResponseTimeout specifies the timeout value for receiving a
	// fee response from the api source.
	WebAPIResponseTimeout = 10 * time.Second

	// economicalFeeMode is a mode that bitcoind uses to serve
	// non-conservative fee estimates. These fee estimates are less
	// resistant to shocks.
	economicalFeeMode = "ECONOMICAL"

	// filterCapConfTarget is the conf target that will be used to cap our
	// minimum feerate if we used the median of our peers' feefilter
	// values.
	filterCapConfTarget = uint32(1)
)

var (
	// errNoFeeRateFound is used when a given conf target cannot be found
	// from the fee estimator.
	errNoFeeRateFound = errors.New("no fee estimation for block target")

	// errEmptyCache is used when the fee rate cache is empty.
	errEmptyCache = errors.New("fee rate cache is empty")
)

// Estimator provides the ability to estimate on-chain transaction fees for
// various combinations of transaction sizes and desired confirmation time
// (measured by number of blocks).
type Estimator interface {
	// EstimateFeePerKW takes in a target for the number of blocks until an
	// initial confirmation and returns the estimated fee expressed in
	// sat/kw.
	EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error)

	// Start signals the Estimator to start any processes or goroutines
	// it needs to perform its duty.
	Start() error

	// Stop stops any spawned goroutines and cleans up the resources used
	// by the fee estimator.
	Stop() error

	// RelayFeePerKW returns the minimum fee rate required for transactions
	// to be relayed. This is also the basis for calculation of the dust
	// limit.
	RelayFeePerKW() SatPerKWeight
}

// StaticEstimator will return a static value for all fee calculation requests.
// It is designed to be replaced by a proper fee calculation implementation.
// The fees are not accessible directly, because changing them would not be
// thread safe.
type StaticEstimator struct {
	// feePerKW is the static fee rate in satoshis-per-vbyte that will be
	// returned by this fee estimator.
	feePerKW SatPerKWeight

	// relayFee is the minimum fee rate required for transactions to be
	// relayed.
	relayFee SatPerKWeight
}

// NewStaticEstimator returns a new static fee estimator instance.
func NewStaticEstimator(feePerKW, relayFee SatPerKWeight) *StaticEstimator {
	return &StaticEstimator{
		feePerKW: feePerKW,
		relayFee: relayFee,
	}
}

// EstimateFeePerKW will return a static value for fee calculations.
//
// NOTE: This method is part of the Estimator interface.
func (e StaticEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	return e.feePerKW, nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This method is part of the Estimator interface.
func (e StaticEstimator) RelayFeePerKW() SatPerKWeight {
	return e.relayFee
}

// Start signals the Estimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the Estimator interface.
func (e StaticEstimator) Start() error {
	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the Estimator interface.
func (e StaticEstimator) Stop() error {
	return nil
}

// A compile-time assertion to ensure that StaticFeeEstimator implements the
// Estimator interface.
var _ Estimator = (*StaticEstimator)(nil)

// BtcdEstimator is an implementation of the Estimator interface backed
// by the RPC interface of an active btcd node. This implementation will proxy
// any fee estimation requests to btcd's RPC interface.
type BtcdEstimator struct {
	// fallbackFeePerKW is the fall back fee rate in sat/kw that is returned
	// if the fee estimator does not yet have enough data to actually
	// produce fee estimates.
	fallbackFeePerKW SatPerKWeight

	// minFeeManager is used to query the current minimum fee, in sat/kw,
	// that we should enforce. This will be used to determine fee rate for
	// a transaction when the estimated fee rate is too low to allow the
	// transaction to propagate through the network.
	minFeeManager *minFeeManager

	btcdConn *rpcclient.Client

	// filterManager uses our peer's feefilter values to determine a
	// suitable feerate to use that will allow successful transaction
	// propagation.
	filterManager *filterManager
}

// NewBtcdEstimator creates a new BtcdEstimator given a fully populated
// rpc config that is able to successfully connect and authenticate with the
// btcd node, and also a fall back fee rate. The fallback fee rate is used in
// the occasion that the estimator has insufficient data, or returns zero for a
// fee estimate.
func NewBtcdEstimator(rpcConfig rpcclient.ConnConfig,
	fallBackFeeRate SatPerKWeight) (*BtcdEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	fetchCb := func() ([]SatPerKWeight, error) {
		return fetchBtcdFilters(chainConn)
	}

	return &BtcdEstimator{
		fallbackFeePerKW: fallBackFeeRate,
		btcdConn:         chainConn,
		filterManager:    newFilterManager(fetchCb),
	}, nil
}

// Start signals the Estimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the Estimator interface.
func (b *BtcdEstimator) Start() error {
	if err := b.btcdConn.Connect(20); err != nil {
		return err
	}

	// Once the connection to the backend node has been established, we
	// can initialise the minimum relay fee manager which queries the
	// chain backend for the minimum relay fee on construction.
	minRelayFeeManager, err := newMinFeeManager(
		defaultUpdateInterval, b.fetchMinRelayFee,
	)
	if err != nil {
		return err
	}
	b.minFeeManager = minRelayFeeManager

	b.filterManager.Start()

	return nil
}

// fetchMinRelayFee fetches and returns the minimum relay fee in sat/kb from
// the btcd backend.
func (b *BtcdEstimator) fetchMinRelayFee() (SatPerKWeight, error) {
	info, err := b.btcdConn.GetInfo()
	if err != nil {
		return 0, err
	}

	relayFee, err := btcutil.NewAmount(info.RelayFee)
	if err != nil {
		return 0, err
	}

	// The fee rate is expressed in sat/kb, so we'll manually convert it to
	// our desired sat/kw rate.
	return SatPerKVByte(relayFee).FeePerKWeight(), nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the Estimator interface.
func (b *BtcdEstimator) Stop() error {
	b.filterManager.Stop()

	b.btcdConn.Shutdown()
	b.btcdConn.WaitForShutdown()

	return nil
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the Estimator interface.
func (b *BtcdEstimator) EstimateFeePerKW(numBlocks uint32) (SatPerKWeight, error) {
	feeEstimate, err := b.fetchEstimate(numBlocks)
	switch {
	// If the estimator doesn't have enough data, or returns an error, then
	// to return a proper value, then we'll return the default fall back
	// fee rate.
	case err != nil:
		log.Errorf("unable to query estimator: %v", err)
		fallthrough

	case feeEstimate == 0:
		return b.fallbackFeePerKW, nil
	}

	return feeEstimate, nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This method is part of the Estimator interface.
func (b *BtcdEstimator) RelayFeePerKW() SatPerKWeight {
	// Get a suitable minimum feerate to use. This may optionally use the
	// median of our peers' feefilter values.
	feeCapClosure := func() (SatPerKWeight, error) {
		return b.fetchEstimateInner(filterCapConfTarget)
	}

	return chooseMinFee(
		b.minFeeManager.fetchMinFee, b.filterManager.FetchMedianFilter,
		feeCapClosure,
	)
}

// fetchEstimate returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/kw.
func (b *BtcdEstimator) fetchEstimate(confTarget uint32) (SatPerKWeight, error) {
	satPerKw, err := b.fetchEstimateInner(confTarget)
	if err != nil {
		return 0, err
	}

	// Finally, we'll enforce our fee floor by choosing the higher of the
	// minimum relay fee and the feerate returned by the filterManager.
	absoluteMinFee := b.RelayFeePerKW()

	if satPerKw < absoluteMinFee {
		log.Debugf("Estimated fee rate of %v sat/kw is too low, "+
			"using fee floor of %v sat/kw instead", satPerKw,
			absoluteMinFee)

		satPerKw = absoluteMinFee
	}

	log.Debugf("Returning %v sat/kw for conf target of %v",
		int64(satPerKw), confTarget)

	return satPerKw, nil
}

func (b *BtcdEstimator) fetchEstimateInner(confTarget uint32) (SatPerKWeight,
	error) {

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
	return SatPerKVByte(satPerKB).FeePerKWeight(), nil
}

// A compile-time assertion to ensure that BtcdEstimator implements the
// Estimator interface.
var _ Estimator = (*BtcdEstimator)(nil)

// BitcoindEstimator is an implementation of the Estimator interface backed by
// the RPC interface of an active bitcoind node. This implementation will proxy
// any fee estimation requests to bitcoind's RPC interface.
type BitcoindEstimator struct {
	// fallbackFeePerKW is the fallback fee rate in sat/kw that is returned
	// if the fee estimator does not yet have enough data to actually
	// produce fee estimates.
	fallbackFeePerKW SatPerKWeight

	// minFeeManager is used to keep track of the minimum fee, in sat/kw,
	// that we should enforce. This will be used as the default fee rate
	// for a transaction when the estimated fee rate is too low to allow
	// the transaction to propagate through the network.
	minFeeManager *minFeeManager

	// feeMode is the estimate_mode to use when calling "estimatesmartfee".
	// It can be either "ECONOMICAL" or "CONSERVATIVE", and it's default
	// to "CONSERVATIVE".
	feeMode string

	// TODO(ziggie): introduce an interface for the client to enhance
	// testability of the estimator.
	bitcoindConn *rpcclient.Client

	// filterManager uses our peer's feefilter values to determine a
	// suitable feerate to use that will allow successful transaction
	// propagation.
	filterManager *filterManager
}

// NewBitcoindEstimator creates a new BitcoindEstimator given a fully populated
// rpc config that is able to successfully connect and authenticate with the
// bitcoind node, and also a fall back fee rate. The fallback fee rate is used
// in the occasion that the estimator has insufficient data, or returns zero
// for a fee estimate.
func NewBitcoindEstimator(rpcConfig rpcclient.ConnConfig, feeMode string,
	fallBackFeeRate SatPerKWeight) (*BitcoindEstimator, error) {

	rpcConfig.DisableConnectOnNew = true
	rpcConfig.DisableAutoReconnect = false
	rpcConfig.DisableTLS = true
	rpcConfig.HTTPPostMode = true
	chainConn, err := rpcclient.New(&rpcConfig, nil)
	if err != nil {
		return nil, err
	}

	fetchCb := func() ([]SatPerKWeight, error) {
		return fetchBitcoindFilters(chainConn)
	}

	return &BitcoindEstimator{
		fallbackFeePerKW: fallBackFeeRate,
		bitcoindConn:     chainConn,
		feeMode:          feeMode,
		filterManager:    newFilterManager(fetchCb),
	}, nil
}

// Start signals the Estimator to start any processes or goroutines
// it needs to perform its duty.
//
// NOTE: This method is part of the Estimator interface.
func (b *BitcoindEstimator) Start() error {
	// Once the connection to the backend node has been established, we'll
	// initialise the minimum relay fee manager which will query
	// the backend node for its minimum mempool fee.
	relayFeeManager, err := newMinFeeManager(
		defaultUpdateInterval,
		b.fetchMinMempoolFee,
	)
	if err != nil {
		return err
	}
	b.minFeeManager = relayFeeManager

	b.filterManager.Start()

	return nil
}

// fetchMinMempoolFee is used to fetch the minimum fee that the backend node
// requires for a tx to enter its mempool. The returned fee will be the
// maximum of the minimum relay fee and the minimum mempool fee.
func (b *BitcoindEstimator) fetchMinMempoolFee() (SatPerKWeight, error) {
	resp, err := b.bitcoindConn.RawRequest("getmempoolinfo", nil)
	if err != nil {
		return 0, err
	}

	// Parse the response to retrieve the min mempool fee in sat/KB.
	// mempoolminfee is the maximum of minrelaytxfee and
	// minimum mempool fee
	info := struct {
		MempoolMinFee float64 `json:"mempoolminfee"`
	}{}
	if err := json.Unmarshal(resp, &info); err != nil {
		return 0, err
	}

	minMempoolFee, err := btcutil.NewAmount(info.MempoolMinFee)
	if err != nil {
		return 0, err
	}

	// The fee rate is expressed in sat/kb, so we'll manually convert it to
	// our desired sat/kw rate.
	return SatPerKVByte(minMempoolFee).FeePerKWeight(), nil
}

// Stop stops any spawned goroutines and cleans up the resources used
// by the fee estimator.
//
// NOTE: This method is part of the Estimator interface.
func (b *BitcoindEstimator) Stop() error {
	b.filterManager.Stop()
	return nil
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the Estimator interface.
func (b *BitcoindEstimator) EstimateFeePerKW(
	numBlocks uint32) (SatPerKWeight, error) {

	if numBlocks > MaxBlockTarget {
		log.Debugf("conf target %d exceeds the max value, "+
			"use %d instead.", numBlocks, MaxBlockTarget,
		)
		numBlocks = MaxBlockTarget
	}

	feeEstimate, err := b.fetchEstimate(numBlocks, b.feeMode)
	switch {
	// If the estimator doesn't have enough data, or returns an error, then
	// to return a proper value, then we'll return the default fall back
	// fee rate.
	case err != nil:
		log.Errorf("unable to query estimator: %v", err)
		fallthrough

	case feeEstimate == 0:
		return b.fallbackFeePerKW, nil
	}

	return feeEstimate, nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This method is part of the Estimator interface.
func (b *BitcoindEstimator) RelayFeePerKW() SatPerKWeight {
	// Get a suitable minimum feerate to use. This may optionally use the
	// median of our peers' feefilter values.
	feeCapClosure := func() (SatPerKWeight, error) {
		return b.fetchEstimateInner(
			filterCapConfTarget, economicalFeeMode,
		)
	}

	return chooseMinFee(
		b.minFeeManager.fetchMinFee, b.filterManager.FetchMedianFilter,
		feeCapClosure,
	)
}

// fetchEstimate returns a fee estimate for a transaction to be confirmed in
// confTarget blocks. The estimate is returned in sat/kw.
func (b *BitcoindEstimator) fetchEstimate(confTarget uint32, feeMode string) (
	SatPerKWeight, error) {

	satPerKw, err := b.fetchEstimateInner(confTarget, feeMode)
	if err != nil {
		return 0, err
	}

	// Finally, we'll enforce our fee floor by choosing the higher of the
	// minimum relay fee and the feerate returned by the filterManager.
	absoluteMinFee := b.RelayFeePerKW()

	if satPerKw < absoluteMinFee {
		log.Debugf("Estimated fee rate of %v sat/kw is too low, "+
			"using fee floor of %v sat/kw instead", satPerKw,
			absoluteMinFee)

		satPerKw = absoluteMinFee
	}

	log.Debugf("Returning %v sat/kw for conf target of %v",
		int64(satPerKw), confTarget)

	return satPerKw, nil
}

func (b *BitcoindEstimator) fetchEstimateInner(confTarget uint32,
	feeMode string) (SatPerKWeight, error) {

	// First, we'll send an "estimatesmartfee" command as a raw request,
	// since it isn't supported by btcd but is available in bitcoind.
	target, err := json.Marshal(uint64(confTarget))
	if err != nil {
		return 0, err
	}

	// The mode must be either ECONOMICAL or CONSERVATIVE.
	mode, err := json.Marshal(feeMode)
	if err != nil {
		return 0, err
	}

	resp, err := b.bitcoindConn.RawRequest(
		"estimatesmartfee", []json.RawMessage{target, mode},
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

	// Bitcoind will not report any fee estimation if it has not enough
	// data available hence the fee will remain zero. We return an error
	// here to make sure that we do not use the min relay fee instead.
	if satPerKB == 0 {
		return 0, fmt.Errorf("fee estimation data not available yet")
	}

	// Since we use fee rates in sat/kw internally, we'll convert the
	// estimated fee rate from its sat/kb representation to sat/kw.
	return SatPerKVByte(satPerKB).FeePerKWeight(), nil
}

// chooseMinFee takes the minimum relay fee and the median of our peers'
// feefilter values and takes the higher of the two. It then compares the value
// against a maximum fee and caps it if the value is higher than the maximum
// fee. This function is only called if we have data for our peers' feefilter.
// The returned value will be used as the fee floor for calls to
// RelayFeePerKW.
func chooseMinFee(minRelayFeeFunc func() SatPerKWeight,
	medianFilterFunc func() (SatPerKWeight, error),
	feeCapFunc func() (SatPerKWeight, error)) SatPerKWeight {

	minRelayFee := minRelayFeeFunc()

	medianFilter, err := medianFilterFunc()
	if err != nil {
		// If we don't have feefilter data, we fallback to using our
		// minimum relay fee.
		return minRelayFee
	}

	feeCap, err := feeCapFunc()
	if err != nil {
		// If we encountered an error, don't use the medianFilter and
		// instead fallback to using our minimum relay fee.
		return minRelayFee
	}

	// If the median feefilter is higher than our minimum relay fee, use it
	// instead.
	if medianFilter > minRelayFee {
		// Only apply the cap if the median filter was used. This is
		// to prevent an adversary from taking up the majority of our
		// outbound peer slots and forcing us to use a high median
		// filter value.
		if medianFilter > feeCap {
			return feeCap
		}

		return medianFilter
	}

	return minRelayFee
}

// A compile-time assertion to ensure that BitcoindEstimator implements the
// Estimator interface.
var _ Estimator = (*BitcoindEstimator)(nil)

// WebAPIFeeSource is an interface allows the WebAPIEstimator to query an
// arbitrary HTTP-based fee estimator. Each new set/network will gain an
// implementation of this interface in order to allow the WebAPIEstimator to
// be fully generic in its logic.
type WebAPIFeeSource interface {
	// GetFeeInfo will query the web API, parse the response into a
	// WebAPIResponse which contains a map of confirmation targets to
	// sat/kw fees and min relay feerate.
	GetFeeInfo() (WebAPIResponse, error)
}

// SparseConfFeeSource is an implementation of the WebAPIFeeSource that utilizes
// a user-specified fee estimation API for Bitcoin. It expects the response
// to be in the JSON format: `fee_by_block_target: { ... }` where the value maps
// block targets to fee estimates (in sat per kilovbyte).
type SparseConfFeeSource struct {
	// URL is the fee estimation API specified by the user.
	URL string
}

// WebAPIResponse is the response returned by the fee estimation API.
type WebAPIResponse struct {
	// FeeByBlockTarget is a map of confirmation targets to sat/kvb fees.
	FeeByBlockTarget map[uint32]uint32 `json:"fee_by_block_target"`

	// MinRelayFeerate is the minimum relay fee in sat/kvb.
	MinRelayFeerate SatPerKVByte `json:"min_relay_feerate"`
}

// parseResponse attempts to parse the body of the response generated by the
// above query URL. Typically this will be JSON, but the specifics are left to
// the WebAPIFeeSource implementation.
func (s SparseConfFeeSource) parseResponse(r io.Reader) (
	WebAPIResponse, error) {

	resp := WebAPIResponse{
		FeeByBlockTarget: make(map[uint32]uint32),
		MinRelayFeerate:  0,
	}
	jsonReader := json.NewDecoder(r)
	if err := jsonReader.Decode(&resp); err != nil {
		return WebAPIResponse{}, err
	}

	if resp.MinRelayFeerate == 0 {
		log.Errorf("No min relay fee rate available, using default %v",
			FeePerKwFloor)
		resp.MinRelayFeerate = FeePerKwFloor.FeePerKVByte()
	}

	return resp, nil
}

// GetFeeInfo will query the web API, parse the response and return a map of
// confirmation targets to sat/kw fees and min relay feerate in a parsed
// response.
func (s SparseConfFeeSource) GetFeeInfo() (WebAPIResponse, error) {
	// Rather than use the default http.Client, we'll make a custom one
	// which will allow us to control how long we'll wait to read the
	// response from the service. This way, if the service is down or
	// overloaded, we can exit early and use our default fee.
	netTransport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: WebAPIConnectionTimeout,
		}).Dial,
		TLSHandshakeTimeout: WebAPIConnectionTimeout,
	}
	netClient := &http.Client{
		Timeout:   WebAPIResponseTimeout,
		Transport: netTransport,
	}

	// With the client created, we'll query the API source to fetch the URL
	// that we should use to query for the fee estimation.
	targetURL := s.URL
	resp, err := netClient.Get(targetURL)
	if err != nil {
		log.Errorf("unable to query web api for fee response: %v",
			err)
		return WebAPIResponse{}, err
	}
	defer resp.Body.Close()

	// Once we've obtained the response, we'll instruct the WebAPIFeeSource
	// to parse out the body to obtain our final result.
	parsedResp, err := s.parseResponse(resp.Body)
	if err != nil {
		log.Errorf("unable to parse fee api response: %v", err)

		return WebAPIResponse{}, err
	}

	return parsedResp, nil
}

// A compile-time assertion to ensure that SparseConfFeeSource implements the
// WebAPIFeeSource interface.
var _ WebAPIFeeSource = (*SparseConfFeeSource)(nil)

// WebAPIEstimator is an implementation of the Estimator interface that
// queries an HTTP-based fee estimation from an existing web API.
type WebAPIEstimator struct {
	started atomic.Bool
	stopped atomic.Bool

	// apiSource is the backing web API source we'll use for our queries.
	apiSource WebAPIFeeSource

	// updateFeeTicker is the ticker responsible for updating the Estimator's
	// fee estimates every time it fires.
	updateFeeTicker *time.Ticker

	// feeByBlockTarget is our cache for fees pulled from the API. When a
	// fee estimate request comes in, we pull the estimate from this array
	// rather than re-querying the API, to prevent an inadvertent DoS attack.
	feesMtx          sync.Mutex
	feeByBlockTarget map[uint32]uint32
	minRelayFeerate  SatPerKVByte

	// noCache determines whether the web estimator should cache fee
	// estimates.
	noCache bool

	// minFeeUpdateTimeout represents the minimum interval in which the
	// web estimator will request fresh fees from its API.
	minFeeUpdateTimeout time.Duration

	// minFeeUpdateTimeout represents the maximum interval in which the
	// web estimator will request fresh fees from its API.
	maxFeeUpdateTimeout time.Duration

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewWebAPIEstimator creates a new WebAPIEstimator from a given URL and a
// fallback default fee. The fees are updated whenever a new block is mined.
func NewWebAPIEstimator(api WebAPIFeeSource, noCache bool,
	minFeeUpdateTimeout time.Duration,
	maxFeeUpdateTimeout time.Duration) (*WebAPIEstimator, error) {

	if minFeeUpdateTimeout == 0 || maxFeeUpdateTimeout == 0 {
		return nil, fmt.Errorf("minFeeUpdateTimeout and " +
			"maxFeeUpdateTimeout must be greater than 0")
	}

	if minFeeUpdateTimeout >= maxFeeUpdateTimeout {
		return nil, fmt.Errorf("minFeeUpdateTimeout target of %v "+
			"cannot be greater than maxFeeUpdateTimeout of %v",
			minFeeUpdateTimeout, maxFeeUpdateTimeout)
	}

	return &WebAPIEstimator{
		apiSource:           api,
		feeByBlockTarget:    make(map[uint32]uint32),
		noCache:             noCache,
		quit:                make(chan struct{}),
		minFeeUpdateTimeout: minFeeUpdateTimeout,
		maxFeeUpdateTimeout: maxFeeUpdateTimeout,
	}, nil
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This method is part of the Estimator interface.
func (w *WebAPIEstimator) EstimateFeePerKW(numBlocks uint32) (
	SatPerKWeight, error) {

	// If the estimator hasn't been started yet, we'll return an error as
	// we can't provide a fee estimate.
	if !w.started.Load() {
		return 0, fmt.Errorf("estimator not started")
	}

	if numBlocks > MaxBlockTarget {
		numBlocks = MaxBlockTarget
	} else if numBlocks < minBlockTarget {
		return 0, fmt.Errorf("conf target of %v is too low, minimum "+
			"accepted is %v", numBlocks, minBlockTarget)
	}

	// Get fee estimates now if we don't refresh periodically.
	if w.noCache {
		w.updateFeeEstimates()
	}

	feePerKb, err := w.getCachedFee(numBlocks)

	// If the estimator returns an error, a zero value fee rate will be
	// returned. We will log the error and return the fall back fee rate
	// instead.
	if err != nil {
		log.Errorf("Unable to query estimator: %v", err)
	}

	// If the result is too low, then we'll clamp it to our current fee
	// floor.
	satPerKw := SatPerKVByte(feePerKb).FeePerKWeight()
	if satPerKw < FeePerKwFloor {
		satPerKw = FeePerKwFloor
	}

	log.Debugf("Web API returning %v sat/kw for conf target of %v",
		int64(satPerKw), numBlocks)

	return satPerKw, nil
}

// Start signals the Estimator to start any processes or goroutines it needs
// to perform its duty.
//
// NOTE: This method is part of the Estimator interface.
func (w *WebAPIEstimator) Start() error {
	log.Infof("Starting Web API fee estimator...")

	// Return an error if it's already been started.
	if w.started.Load() {
		return fmt.Errorf("web API fee estimator already started")
	}
	defer w.started.Store(true)

	// During startup we'll query the API to initialize the fee map.
	w.updateFeeEstimates()

	// No update loop is needed when we don't cache.
	if w.noCache {
		return nil
	}

	feeUpdateTimeout := w.randomFeeUpdateTimeout()

	log.Infof("Web API fee estimator using update timeout of %v",
		feeUpdateTimeout)

	w.updateFeeTicker = time.NewTicker(feeUpdateTimeout)

	w.wg.Add(1)
	go w.feeUpdateManager()

	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used by the
// fee estimator.
//
// NOTE: This method is part of the Estimator interface.
func (w *WebAPIEstimator) Stop() error {
	log.Infof("Stopping web API fee estimator")

	if w.stopped.Swap(true) {
		return fmt.Errorf("web API fee estimator already stopped")
	}

	// Update loop is not running when we don't cache.
	if w.noCache {
		return nil
	}

	if w.updateFeeTicker != nil {
		w.updateFeeTicker.Stop()
	}

	close(w.quit)
	w.wg.Wait()

	return nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This method is part of the Estimator interface.
func (w *WebAPIEstimator) RelayFeePerKW() SatPerKWeight {
	if !w.started.Load() {
		log.Error("WebAPIEstimator not started")
	}

	// Get fee estimates now if we don't refresh periodically.
	if w.noCache {
		w.updateFeeEstimates()
	}

	log.Infof("Web API returning %v for min relay feerate",
		w.minRelayFeerate)

	return w.minRelayFeerate.FeePerKWeight()
}

// randomFeeUpdateTimeout returns a random timeout between minFeeUpdateTimeout
// and maxFeeUpdateTimeout that will be used to determine how often the Estimator
// should retrieve fresh fees from its API.
func (w *WebAPIEstimator) randomFeeUpdateTimeout() time.Duration {
	lower := int64(w.minFeeUpdateTimeout)
	upper := int64(w.maxFeeUpdateTimeout)
	return time.Duration(
		prand.Int63n(upper-lower) + lower, //nolint:gosec
	).Round(time.Second)
}

// getCachedFee takes a conf target and returns the cached fee rate. When the
// fee rate cannot be found, it will search the cache by decrementing the conf
// target until a fee rate is found. If still not found, it will return the fee
// rate of the minimum conf target cached, in other words, the most expensive
// fee rate it knows of.
func (w *WebAPIEstimator) getCachedFee(numBlocks uint32) (uint32, error) {
	w.feesMtx.Lock()
	defer w.feesMtx.Unlock()

	// If the cache is empty, return an error.
	if len(w.feeByBlockTarget) == 0 {
		return 0, fmt.Errorf("web API error: %w", errEmptyCache)
	}

	// Search the conf target from the cache. We expect a query to the web
	// API has been made and the result has been cached at this point.
	fee, ok := w.feeByBlockTarget[numBlocks]

	// If the conf target can be found, exit early.
	if ok {
		return fee, nil
	}

	// The conf target cannot be found. We will first search the cache
	// using a lower conf target. This is a conservative approach as the
	// fee rate returned will be larger than what's requested.
	for target := numBlocks; target >= minBlockTarget; target-- {
		fee, ok := w.feeByBlockTarget[target]
		if !ok {
			continue
		}

		log.Warnf("Web API does not have a fee rate for target=%d, "+
			"using the fee rate for target=%d instead",
			numBlocks, target)

		// Return the fee rate found, which will be more expensive than
		// requested. We will not cache the fee rate here in the hope
		// that the web API will later populate this value.
		return fee, nil
	}

	// There are no lower conf targets cached, which is likely when the
	// requested conf target is 1. We will search the cache using a higher
	// conf target, which gives a fee rate that's cheaper than requested.
	//
	// NOTE: we can only get here iff the requested conf target is smaller
	// than the minimum conf target cached, so we return the minimum conf
	// target from the cache.
	minTargetCached := uint32(math.MaxUint32)
	for target := range w.feeByBlockTarget {
		if target < minTargetCached {
			minTargetCached = target
		}
	}

	fee, ok = w.feeByBlockTarget[minTargetCached]
	if !ok {
		// We should never get here, just a vanity check.
		return 0, fmt.Errorf("web API error: %w, conf target: %d",
			errNoFeeRateFound, numBlocks)
	}

	// Log an error instead of a warning as a cheaper fee rate may delay
	// the confirmation for some important transactions.
	log.Errorf("Web API does not have a fee rate for target=%d, "+
		"using the fee rate for target=%d instead",
		numBlocks, minTargetCached)

	return fee, nil
}

// updateFeeEstimates re-queries the API for fresh fees and caches them.
func (w *WebAPIEstimator) updateFeeEstimates() {
	// Once we've obtained the response, we'll instruct the WebAPIFeeSource
	// to parse out the body to obtain our final result.
	resp, err := w.apiSource.GetFeeInfo()
	if err != nil {
		log.Errorf("unable to get fee response: %v", err)
		return
	}

	log.Debugf("Received response from source: %s", lnutils.NewLogClosure(
		func() string {
			resp, _ := json.Marshal(resp)
			return string(resp)
		}))

	w.feesMtx.Lock()
	w.feeByBlockTarget = resp.FeeByBlockTarget
	w.minRelayFeerate = resp.MinRelayFeerate
	w.feesMtx.Unlock()
}

// feeUpdateManager updates the fee estimates whenever a new block comes in.
func (w *WebAPIEstimator) feeUpdateManager() {
	defer w.wg.Done()

	for {
		select {
		case <-w.updateFeeTicker.C:
			w.updateFeeEstimates()
		case <-w.quit:
			return
		}
	}
}

// A compile-time assertion to ensure that WebAPIEstimator implements the
// Estimator interface.
var _ Estimator = (*WebAPIEstimator)(nil)
