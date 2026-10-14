package chainfee

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestFeeRateTypes checks that converting fee rates between the
// different types that represent fee rates and calculating fees
// work as expected.
func TestFeeRateTypes(t *testing.T) {
	t.Parallel()

	// We'll be calculating the transaction fees for the given measurements
	// using different fee rates and expecting them to match.
	const vsize = 300
	const weight = vsize * 4

	// Test the conversion from sat/kw to sat/kb.
	for feePerKw := SatPerKWeight(250); feePerKw < 10000; feePerKw += 50 {
		feePerKB := feePerKw.FeePerKVByte()
		if feePerKB != SatPerKVByte(feePerKw*4) {
			t.Fatalf("expected %d sat/kb, got %d sat/kb when "+
				"converting from %d sat/kw", feePerKw*4,
				feePerKB, feePerKw)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKw * weight / 1000)
		fee1 := feePerKw.FeeForWeight(weight)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKB.FeeForVSize(vsize)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}

	// Test the conversion from sat/kb to sat/kw.
	for feePerKB := SatPerKVByte(1000); feePerKB < 40000; feePerKB += 1000 {
		feePerKw := feePerKB.FeePerKWeight()
		if feePerKw != SatPerKWeight(feePerKB/4) {
			t.Fatalf("expected %d sat/kw, got %d sat/kw when "+
				"converting from %d sat/kb", feePerKB/4,
				feePerKw, feePerKB)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKB * vsize / 1000)
		fee1 := feePerKB.FeeForVSize(vsize)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKw.FeeForWeight(weight)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}
}

// TestStaticFeeEstimator checks that the StaticFeeEstimator returns the
// expected fee rate.
func TestStaticFeeEstimator(t *testing.T) {
	t.Parallel()

	const feePerKw = FeePerKwFloor

	feeEstimator := NewStaticEstimator(feePerKw, 0)
	if err := feeEstimator.Start(); err != nil {
		t.Fatalf("unable to start fee estimator: %v", err)
	}
	defer feeEstimator.Stop()

	feeRate, err := feeEstimator.EstimateFeePerKW(6)
	require.NoError(t, err, "unable to get fee rate")

	if feeRate != feePerKw {
		t.Fatalf("expected fee rate %v, got %v", feePerKw, feeRate)
	}
}

// TestSparseConfFeeSource checks that SparseConfFeeSource generates URLs and
// parses API responses as expected.
func TestSparseConfFeeSource(t *testing.T) {
	t.Parallel()

	// Test that GenQueryURL returns the URL as is.
	url := "test"
	feeSource := SparseConfFeeSource{URL: url}

	// Test parsing a properly formatted JSON API response.
	// First, create the response as a bytes.Reader.
	testFees := map[uint32]uint32{
		1: 12345,
		2: 42,
		3: 54321,
	}
	testMinRelayFee := SatPerKVByte(1000)
	testResp := WebAPIResponse{
		MinRelayFeerate:  testMinRelayFee,
		FeeByBlockTarget: testFees,
	}

	jsonResp, err := json.Marshal(testResp)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader := bytes.NewReader(jsonResp)

	// Finally, ensure the expected map is returned without error.
	resp, err := feeSource.parseResponse(reader)
	require.NoError(t, err, "unable to parse API response")
	require.Equal(t, testResp, resp, "unexpected resp returned")

	// Test parsing an improperly formatted JSON API response.
	badFees := map[string]uint32{"hi": 12345, "hello": 42, "satoshi": 54321}
	badJSON := map[string]map[string]uint32{"fee_by_block_target": badFees}
	jsonResp, err = json.Marshal(badJSON)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader = bytes.NewReader(jsonResp)

	// Finally, ensure the improperly formatted fees error.
	_, err = feeSource.parseResponse(reader)
	require.Error(t, err, "expected error when parsing bad JSON")
}

// TestFeeSourceCompatibility checks that when a fee source doesn't return a
// `min_relay_feerate` field in its response, the floor feerate is used.
//
// NOTE: Field `min_relay_feerate` was added in v0.18.3.
func TestFeeSourceCompatibility(t *testing.T) {
	t.Parallel()

	// Test that GenQueryURL returns the URL as is.
	url := "test"
	feeSource := SparseConfFeeSource{URL: url}

	// Test parsing a properly formatted JSON API response.
	//
	// Create the resp without the `min_relay_feerate` field.
	testFees := map[uint32]uint32{
		1: 12345,
	}
	testResp := struct {
		// FeeByBlockTarget is a map of confirmation targets to sat/kvb
		// fees.
		FeeByBlockTarget map[uint32]uint32 `json:"fee_by_block_target"`
	}{
		FeeByBlockTarget: testFees,
	}

	jsonResp, err := json.Marshal(testResp)
	require.NoError(t, err, "unable to marshal JSON API response")
	reader := bytes.NewReader(jsonResp)

	// Ensure the expected map is returned without error.
	resp, err := feeSource.parseResponse(reader)
	require.NoError(t, err, "unable to parse API response")
	require.Equal(t, testResp.FeeByBlockTarget, resp.FeeByBlockTarget,
		"unexpected resp returned")

	// Expect the floor feerate to be used.
	require.Equal(t, FeePerKwFloor.FeePerKVByte(), resp.MinRelayFeerate)
}

// TestWebAPIFeeEstimator checks that the WebAPIFeeEstimator returns fee rates
// as expected.
func TestWebAPIFeeEstimator(t *testing.T) {
	t.Parallel()

	var (
		minTarget uint32 = 2
		maxTarget uint32 = 6

		// Fee rates are in sat/kb.
		minFeeRate uint32 = 2000 // 500 sat/kw
		maxFeeRate uint32 = 4000 // 1000 sat/kw

		minFeeUpdateTimeout = 5 * time.Minute
		maxFeeUpdateTimeout = 20 * time.Minute
	)

	testCases := []struct {
		name            string
		target          uint32
		expectedFeeRate uint32
		expectedErr     string
	}{
		{
			// When requested target is below minBlockTarget, an
			// error is returned.
			name:            "target_below_min",
			target:          0,
			expectedFeeRate: 0,
			expectedErr:     "too low, minimum",
		},
		{
			// When requested target is larger than the max cached
			// target, the fee rate of the max cached target is
			// returned.
			name:            "target_w_too-low_fee",
			target:          maxTarget + 100,
			expectedFeeRate: minFeeRate,
			expectedErr:     "",
		},
		{
			// When requested target is smaller than the min cached
			// target, the fee rate of the min cached target is
			// returned.
			name:            "API-omitted_target",
			target:          minTarget - 1,
			expectedFeeRate: maxFeeRate,
			expectedErr:     "",
		},
		{
			// When the target is found, return it.
			name:            "valid_target",
			target:          maxTarget,
			expectedFeeRate: minFeeRate,
			expectedErr:     "",
		},
	}

	// Construct mock fee source for the Estimator to pull fees from.
	//
	// This will create a `feeByBlockTarget` map with the following values,
	// - 2: 4000 sat/kb
	// - 6: 2000 sat/kb.
	feeRates := map[uint32]uint32{
		minTarget: maxFeeRate,
		maxTarget: minFeeRate,
	}
	resp := WebAPIResponse{
		FeeByBlockTarget: feeRates,
	}

	// Create a mock fee source and mock its returned map.
	feeSource := &mockFeeSource{}
	feeSource.On("GetFeeInfo").Return(resp, nil)

	estimator, _ := NewWebAPIEstimator(
		feeSource, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	// Test that when the estimator is not started, an error is returned.
	feeRate, err := estimator.EstimateFeePerKW(5)
	require.Error(t, err, "expected an error")
	require.Zero(t, feeRate, "expected zero fee rate")

	// Start the estimator.
	require.NoError(t, estimator.Start(), "unable to start fee estimator")

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			est, err := estimator.EstimateFeePerKW(tc.target)

			// Test an error case.
			if tc.expectedErr != "" {
				require.Error(t, err, "expected error")
				require.ErrorContains(t, err, tc.expectedErr)

				return
			}

			// Test an non-error case.
			require.NoErrorf(t, err, "error from target %v",
				tc.target)

			exp := SatPerKVByte(tc.expectedFeeRate).FeePerKWeight()
			require.Equalf(t, exp, est, "target %v failed, fee "+
				"map is %v", tc.target, feeRate)
		})
	}

	// Stop the estimator when test ends.
	require.NoError(t, estimator.Stop(), "unable to stop fee estimator")

	// Assert the mocked fee source is called as expected.
	feeSource.AssertExpectations(t)
}

// TestGetCachedFee checks that the fee caching logic works as expected.
func TestGetCachedFee(t *testing.T) {
	var (
		minTarget uint32 = 2
		maxTarget uint32 = 6

		minFeeRate uint32 = 100
		maxFeeRate uint32 = 1000

		minFeeUpdateTimeout = 5 * time.Minute
		maxFeeUpdateTimeout = 20 * time.Minute
	)

	// Create a dummy estimator without WebAPIFeeSource.
	estimator, _ := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	// When the cache is empty, an error should be returned.
	cachedFee, err := estimator.getCachedFee(minTarget)
	require.Zero(t, cachedFee)
	require.ErrorIs(t, err, errEmptyCache)

	// Store a fee rate inside the cache. The cache map now looks like,
	// {2: 1000, 6: 100}
	estimator.feeByBlockTarget = map[uint32]uint32{
		minTarget: maxFeeRate,
		maxTarget: minFeeRate,
	}

	testCases := []struct {
		name        string
		confTarget  uint32
		expectedFee uint32
	}{
		{
			// When the target is cached, return it.
			name:        "return cached fee",
			confTarget:  minTarget,
			expectedFee: maxFeeRate,
		},
		{
			// When the target is not cached, return the next
			// lowest target that's cached. In this case,
			// requesting fee rate for target 7 will give the
			// result for target 6.
			name:        "return lowest cached fee",
			confTarget:  maxTarget + 1,
			expectedFee: minFeeRate,
		},
		{
			// When the target is not cached, return the next
			// lowest target that's cached. In this case,
			// requesting fee rate for target 5 will give the
			// result for target 2.
			name:        "return next cached fee",
			confTarget:  maxTarget - 1,
			expectedFee: maxFeeRate,
		},
		{
			// When the target is not cached, and the next lowest
			// target is not cached, return the nearest fee rate.
			// In this case, requesting fee rate for target 1 will
			// give the result for target 2.
			name:        "return highest cached fee",
			confTarget:  minTarget - 1,
			expectedFee: maxFeeRate,
		},
	}

	for _, tc := range testCases {

		t.Run(tc.name, func(t *testing.T) {
			cachedFee, err := estimator.getCachedFee(tc.confTarget)

			require.NoError(t, err)
			require.Equal(t, tc.expectedFee, cachedFee)
		})
	}
}

func TestRandomFeeUpdateTimeout(t *testing.T) {
	t.Parallel()

	var (
		minFeeUpdateTimeout = 1 * time.Minute
		maxFeeUpdateTimeout = 2 * time.Minute
	)

	estimator, _ := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)

	for i := 0; i < 1000; i++ {
		timeout := estimator.randomFeeUpdateTimeout()

		require.GreaterOrEqual(t, timeout, minFeeUpdateTimeout)
		require.LessOrEqual(t, timeout, maxFeeUpdateTimeout)
	}
}

func TestInvalidFeeUpdateTimeout(t *testing.T) {
	t.Parallel()

	var (
		minFeeUpdateTimeout = 2 * time.Minute
		maxFeeUpdateTimeout = 1 * time.Minute
	)

	_, err := NewWebAPIEstimator(
		nil, false, minFeeUpdateTimeout, maxFeeUpdateTimeout,
	)
	require.Error(t, err, "NewWebAPIEstimator should return an error "+
		"when minFeeUpdateTimeout > maxFeeUpdateTimeout")
}

// testFallbackFee is the 25 sat/vB cold-start rate (6,250 sat/kw) that
// chainreg supplies to the bitcoind and btcd estimators.
const testFallbackFee SatPerKWeight = 6250

const (
	// initialMinBTC is below FeePerKwFloor once converted to sat/kw, so the
	// relay fee manager clamps it.
	initialMinBTC = 0.00001

	// lowBTC, midBTC and freshBTC are successful estimates below the
	// fallback, each for a different confirmation target.
	lowBTC   = 0.00002
	midBTC   = 0.00004
	freshBTC = 0.00008

	// bumpBTC is a relay minimum above lowBTC and below the fallback.
	bumpBTC = 0.00010

	// sentinelBTC is the feefilter-cap rate. It must not be reused as a
	// channel fee estimate.
	sentinelBTC = 0.001

	testFeeMode = "CONSERVATIVE"
)

// estimateBehavior selects how the fake backend answers fee RPCs.
type estimateBehavior int

const (
	estimateServe estimateBehavior = iota
	estimateRPCErr
	estimateZero
	estimateMalformedFee
	estimateMalformedBody
	estimateAlternate
)

// rpcCall is one JSON-RPC request observed by the fake backend.
type rpcCall struct {
	method string
	params []json.RawMessage
}

// feeBackend is a local Bitcoin JSON-RPC backend. It is safe for concurrent
// EstimateFeePerKW calls.
type feeBackend struct {
	mu sync.Mutex

	minFeeBTC  float64
	failMinFee bool
	fees       map[uint32]float64
	behavior   estimateBehavior
	estimateN  uint64
	hook       func(method string)
	calls      []rpcCall
}

func newFeeBackend() *feeBackend {
	return &feeBackend{
		minFeeBTC: initialMinBTC,
		fees:      make(map[uint32]float64),
		behavior:  estimateServe,
	}
}

func (b *feeBackend) setFee(target uint32, btcPerKVByte float64) {
	b.mu.Lock()
	b.fees[target] = btcPerKVByte
	b.mu.Unlock()
}

func (b *feeBackend) setMinFee(btcPerKVByte float64) {
	b.mu.Lock()
	b.minFeeBTC = btcPerKVByte
	b.mu.Unlock()
}

func (b *feeBackend) setFailMinFee(fail bool) {
	b.mu.Lock()
	b.failMinFee = fail
	b.mu.Unlock()
}

func (b *feeBackend) setBehavior(behavior estimateBehavior) {
	b.mu.Lock()
	b.behavior = behavior
	b.mu.Unlock()
}

func (b *feeBackend) setHook(hook func(method string)) {
	b.mu.Lock()
	b.hook = hook
	b.mu.Unlock()
}

func (b *feeBackend) resetCalls() {
	b.mu.Lock()
	b.calls = nil
	b.mu.Unlock()
}

func (b *feeBackend) snapshotCalls() []rpcCall {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]rpcCall, len(b.calls))
	copy(out, b.calls)

	return out
}

func (b *feeBackend) handle(body []byte) []byte {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
		ID     json.RawMessage   `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return rpcError(nil, "parse error")
	}

	target := callTarget(req.Params)

	b.mu.Lock()
	b.calls = append(b.calls, rpcCall{
		method: req.Method,
		params: req.Params,
	})
	hook := b.hook
	failMinFee := b.failMinFee
	minFee := b.minFeeBTC
	behavior := b.behavior
	fee, hasFee := b.fees[target]
	if isEstimateMethod(req.Method) {
		b.estimateN++
		if behavior == estimateAlternate && b.estimateN%2 == 0 {
			behavior = estimateRPCErr
		}
	}
	b.mu.Unlock()

	if hook != nil {
		hook(req.Method)
	}

	switch req.Method {
	case "getmempoolinfo":
		if failMinFee {
			return rpcError(req.ID, "mempool unavailable")
		}

		return rpcResult(req.ID, map[string]any{
			"mempoolminfee": minFee,
		})

	case "getinfo":
		if failMinFee {
			return rpcError(req.ID, "info unavailable")
		}

		return rpcResult(req.ID, map[string]any{
			"relayfee": minFee,
		})

	case "getpeerinfo":
		return rpcResult(req.ID, []any{})

	case "estimatesmartfee", "estimatefee":
		return estimateResponse(
			req.Method, req.ID, target, fee, hasFee, behavior,
		)

	default:
		return rpcError(req.ID, "method not found")
	}
}

func estimateResponse(method string, id json.RawMessage, target uint32,
	fee float64, hasFee bool, behavior estimateBehavior) []byte {

	switch behavior {
	case estimateMalformedBody:
		return []byte("not-json")

	case estimateRPCErr:
		return rpcError(id, "backend unavailable")

	case estimateMalformedFee:
		return rpcResult(id, map[string]any{
			"feerate": "bad",
			"blocks":  target,
		})

	case estimateZero:
		if method == "estimatefee" {
			return rpcResult(id, 0.0)
		}

		// Bitcoin Core omits feerate when it has no data. The estimator
		// treats that missing/zero rate as unusable.
		return rpcResult(id, map[string]any{
			"errors": []string{
				"Insufficient data or no feerate found",
			},
			"blocks": target,
		})
	}

	if !hasFee {
		return rpcError(id, "no feerate configured")
	}

	if method == "estimatefee" {
		return rpcResult(id, fee)
	}

	return rpcResult(id, map[string]any{
		"feerate": fee,
		"blocks":  target,
	})
}

func isEstimateMethod(method string) bool {
	return method == "estimatesmartfee" || method == "estimatefee"
}

func callTarget(params []json.RawMessage) uint32 {
	if len(params) == 0 {
		return 0
	}

	var target uint32
	if err := json.Unmarshal(params[0], &target); err != nil {
		return 0
	}

	return target
}

func callMode(params []json.RawMessage) string {
	if len(params) < 2 {
		return ""
	}

	var mode string
	if err := json.Unmarshal(params[1], &mode); err != nil {
		return ""
	}

	return mode
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	Result any             `json:"result"`
	Error  *jsonRPCError   `json:"error"`
	ID     json.RawMessage `json:"id"`
}

func rpcResult(id json.RawMessage, result any) []byte {
	resp, err := json.Marshal(jsonRPCResponse{
		Result: result,
		ID:     id,
	})
	if err != nil {
		return rpcMarshalFallback()
	}

	return resp
}

func rpcError(id json.RawMessage, message string) []byte {
	resp, err := json.Marshal(jsonRPCResponse{
		Error: &jsonRPCError{Code: -1, Message: message},
		ID:    id,
	})
	if err != nil {
		return rpcMarshalFallback()
	}

	return resp
}

func rpcMarshalFallback() []byte {
	return []byte(`{"result":null,"error":{"code":-32603,` +
		`"message":"marshal"},"id":null}`)
}

func (b *feeBackend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b.handle(body))
}

func (b *feeBackend) serveWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	var (
		writeMu sync.Mutex
		wg      sync.WaitGroup
	)
	defer func() {
		wg.Wait()
		_ = conn.Close()
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}

		wg.Add(1)
		go func(msg []byte) {
			defer wg.Done()

			resp := b.handle(msg)

			writeMu.Lock()
			_ = conn.WriteMessage(
				websocket.TextMessage, resp,
			)
			writeMu.Unlock()
		}(msg)
	}
}

func newLocalFeeServer(t *testing.T, btcd bool,
	backend *feeBackend) *httptest.Server {

	t.Helper()

	handler := http.HandlerFunc(backend.serveHTTP)
	if btcd {
		handler = backend.serveWS
	}

	return httptest.NewServer(handler)
}

// rpcEstimatorHarness is one started RPC estimator backed by feeBackend.
type rpcEstimatorHarness struct {
	t       *testing.T
	btcd    bool
	backend *feeBackend

	btcdEst     *BtcdEstimator
	bitcoindEst *BitcoindEstimator
}

func newRPCEstimatorHarness(t *testing.T, btcd bool) *rpcEstimatorHarness {
	t.Helper()

	backend := newFeeBackend()
	srv := newLocalFeeServer(t, btcd, backend)
	t.Cleanup(srv.Close)

	cfg := rpcclient.ConnConfig{
		Host:       srv.Listener.Addr().String(),
		Endpoint:   "ws",
		User:       "user",
		Pass:       "pass",
		DisableTLS: true,
	}

	h := &rpcEstimatorHarness{
		t:       t,
		btcd:    btcd,
		backend: backend,
	}

	if btcd {
		est, err := NewBtcdEstimator(cfg, testFallbackFee)
		require.NoError(t, err)
		require.NoError(t, est.Start())

		h.btcdEst = est
		t.Cleanup(func() {
			require.NoError(t, est.Stop())
		})

		return h
	}

	est, err := NewBitcoindEstimator(cfg, testFeeMode, testFallbackFee)
	require.NoError(t, err)
	require.NoError(t, est.Start())

	h.bitcoindEst = est
	t.Cleanup(func() {
		require.NoError(t, est.Stop())
		est.bitcoindConn.Shutdown()
		est.bitcoindConn.WaitForShutdown()
	})

	return h
}

func (h *rpcEstimatorHarness) estimate(target uint32) (SatPerKWeight, error) {
	if h.btcd {
		return h.btcdEst.EstimateFeePerKW(target)
	}

	return h.bitcoindEst.EstimateFeePerKW(target)
}

func (h *rpcEstimatorHarness) mustEstimate(target uint32) SatPerKWeight {
	h.t.Helper()

	fee, err := h.estimate(target)
	require.NoError(h.t, err)

	return fee
}

func (h *rpcEstimatorHarness) cache() *feeEstimateCache {
	if h.btcd {
		return h.btcdEst.feeCache
	}

	return h.bitcoindEst.feeCache
}

func (h *rpcEstimatorHarness) minFeeMgr() *minFeeManager {
	if h.btcd {
		return h.btcdEst.minFeeManager
	}

	return h.bitcoindEst.minFeeManager
}

func (h *rpcEstimatorHarness) expireMinFee() {
	h.minFeeMgr().lastUpdatedTime = time.Now().Add(
		-defaultUpdateInterval - time.Second,
	)
}

func (h *rpcEstimatorHarness) setMedian(fee SatPerKWeight) {
	filters := []SatPerKWeight{fee}
	if h.btcd {
		h.btcdEst.filterManager.updateMedian(filters)
		return
	}

	h.bitcoindEst.filterManager.updateMedian(filters)
}

func mustSatPerKW(t *testing.T, btcPerKVByte float64) SatPerKWeight {
	t.Helper()

	amt, err := btcutil.NewAmount(btcPerKVByte)
	require.NoError(t, err)

	return SatPerKVByte(amt).FeePerKWeight()
}

func expectedRelayFloor(t *testing.T, btcPerKVByte float64) SatPerKWeight {
	t.Helper()

	fee := mustSatPerKW(t, btcPerKVByte)
	if fee < FeePerKwFloor {
		return FeePerKwFloor
	}

	return fee
}

func hasEstimateCall(calls []rpcCall, method string, target uint32,
	mode string) bool {

	for _, call := range calls {
		if call.method != method || callTarget(call.params) != target {
			continue
		}

		if mode != "" && callMode(call.params) != mode {
			continue
		}

		return true
	}

	return false
}

// TestRPCEstimatorRetainsFeeDuringOutage checks that both RPC estimators keep
// the last successful per-target rate across backend failures and only use the
// configured fallback before any success for that target.
func TestRPCEstimatorRetainsFeeDuringOutage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		btcd bool
	}{
		{name: "bitcoind"},
		{name: "btcd", btcd: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testRPCEstimatorRetainsFee(t, tc.btcd)
		})
	}
}

func testRPCEstimatorRetainsFee(t *testing.T, btcd bool) {
	low := mustSatPerKW(t, lowBTC)
	mid := mustSatPerKW(t, midBTC)
	fresh := mustSatPerKW(t, freshBTC)
	bumped := expectedRelayFloor(t, bumpBTC)
	sentinel := mustSatPerKW(t, sentinelBTC)
	relayFloor := expectedRelayFloor(t, initialMinBTC)

	require.Greater(t, testFallbackFee, low)
	require.Greater(t, testFallbackFee, mid)
	require.Greater(t, testFallbackFee, fresh)
	require.Greater(t, bumped, low)
	require.Greater(t, testFallbackFee, bumped)
	require.NotEqual(t, sentinel, testFallbackFee)

	h := newRPCEstimatorHarness(t, btcd)

	// No successful estimate yet, and a target that stays uncached, both
	// keep the configured fallback.
	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, testFallbackFee, h.mustEstimate(3))
	require.Equal(t, testFallbackFee, h.mustEstimate(6))

	// A low target-3 rate is cached. Repeated RPC failures return it, not
	// the fallback, and do not clear it.
	h.backend.setBehavior(estimateServe)
	h.backend.setFee(3, lowBTC)
	h.backend.setFee(6, midBTC)
	h.backend.resetCalls()

	require.Equal(t, low, h.mustEstimate(3))
	require.Equal(t, mid, h.mustEstimate(6))

	estimateMethod := "estimatesmartfee"
	if btcd {
		estimateMethod = "estimatefee"
	}
	require.True(t, hasEstimateCall(
		h.backend.snapshotCalls(), estimateMethod, 3,
		modeIfBitcoind(btcd, testFeeMode),
	), "estimate RPC was not issued for target 3")
	require.True(t, hasEstimateCall(
		h.backend.snapshotCalls(), estimateMethod, 6,
		modeIfBitcoind(btcd, testFeeMode),
	), "estimate RPC was not issued for target 6")

	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, low, h.mustEstimate(3))
	require.Equal(t, low, h.mustEstimate(3))
	require.Equal(t, mid, h.mustEstimate(6))

	// A recovered backend replaces the cached target-3 rate, and the next
	// outage serves that new rate. Target 6 is left alone.
	h.backend.setBehavior(estimateServe)
	h.backend.setFee(3, freshBTC)
	require.Equal(t, fresh, h.mustEstimate(3))

	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, fresh, h.mustEstimate(3))
	require.Equal(t, fresh, h.mustEstimate(3))
	require.Equal(t, mid, h.mustEstimate(6))

	if !btcd {
		testBitcoindUnusableResponses(t, h, fresh)
		testBitcoindMaxBlockTargetCache(t, h, low)
	}

	// Cached rates are raised to the current relay fee. A failed minimum
	// refresh keeps the last known minimum, and the higher floor is not
	// written back over the cached estimate.
	h.backend.setBehavior(estimateServe)
	h.backend.setFailMinFee(false)
	h.backend.setMinFee(initialMinBTC)
	h.backend.setFee(3, lowBTC)
	h.expireMinFee()
	require.Equal(t, low, h.mustEstimate(3))

	h.backend.setMinFee(bumpBTC)
	h.backend.setBehavior(estimateRPCErr)
	h.expireMinFee()
	require.Equal(t, bumped, h.mustEstimate(3))

	h.backend.setFailMinFee(true)
	h.expireMinFee()
	require.Equal(t, bumped, h.mustEstimate(3))

	h.backend.setFailMinFee(false)
	h.backend.setMinFee(initialMinBTC)
	h.expireMinFee()
	require.Equal(t, low, h.mustEstimate(3))
	require.NotEqual(t, testFallbackFee, h.mustEstimate(3))

	if btcd {
		// A successful zero result is floored to the relay fee, which
		// replaces the cached estimate. It is not the outage path.
		h.backend.setBehavior(estimateZero)
		h.backend.setFailMinFee(false)
		h.backend.setMinFee(initialMinBTC)
		require.Equal(t, relayFloor, h.mustEstimate(3))

		h.backend.setBehavior(estimateRPCErr)
		require.Equal(t, relayFloor, h.mustEstimate(3))
		require.NotEqual(t, low, h.mustEstimate(3))
	}

	// A feefilter-cap query for target 1 must not populate the estimate
	// cache. Median is below the relay fee, so the cap does not change the
	// floor, but it is still fetched.
	h.backend.setBehavior(estimateServe)
	h.backend.setFailMinFee(false)
	h.backend.setMinFee(initialMinBTC)
	h.backend.setFee(3, lowBTC)
	h.backend.setFee(filterCapConfTarget, sentinelBTC)
	h.setMedian(1)
	h.backend.resetCalls()

	require.Equal(t, low, h.mustEstimate(3))

	capMode := ""
	if !btcd {
		capMode = economicalFeeMode
	}
	require.True(t, hasEstimateCall(
		h.backend.snapshotCalls(), estimateMethod, filterCapConfTarget,
		capMode,
	), "feefilter cap was not queried")

	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, testFallbackFee, h.mustEstimate(filterCapConfTarget))
	require.NotEqual(t, sentinel, h.mustEstimate(filterCapConfTarget))
}

func modeIfBitcoind(btcd bool, mode string) string {
	if btcd {
		return ""
	}

	return mode
}

func testBitcoindUnusableResponses(t *testing.T, h *rpcEstimatorHarness,
	cached SatPerKWeight) {

	t.Helper()

	// Missing/zero feerate, a non-JSON body, and a feerate of the wrong
	// JSON type are all query failures. None of them may replace the good
	// cached rate with the fallback or with zero.
	for _, behavior := range []estimateBehavior{
		estimateZero,
		estimateMalformedBody,
		estimateMalformedFee,
	} {
		h.backend.setBehavior(behavior)
		require.Equal(t, cached, h.mustEstimate(3))
		require.NotEqual(t, testFallbackFee, h.mustEstimate(3))
	}
}

func testBitcoindMaxBlockTargetCache(t *testing.T, h *rpcEstimatorHarness,
	fee SatPerKWeight) {

	t.Helper()

	h.backend.setBehavior(estimateServe)
	h.backend.setFee(MaxBlockTarget, lowBTC)
	h.backend.resetCalls()

	require.Equal(t, fee, h.mustEstimate(MaxBlockTarget+25))

	calls := h.backend.snapshotCalls()
	require.True(t, hasEstimateCall(
		calls, "estimatesmartfee", MaxBlockTarget, testFeeMode,
	))
	require.False(t, hasEstimateCall(
		calls, "estimatesmartfee", MaxBlockTarget+25, testFeeMode,
	))

	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, fee, h.mustEstimate(MaxBlockTarget))
	require.Equal(t, fee, h.mustEstimate(MaxBlockTarget+25))
}

// TestEstimatorCacheLockNotHeldAcrossRPC blocks inside backend RPCs and checks
// that the estimate cache lock is not held across that I/O.
func TestEstimatorCacheLockNotHeldAcrossRPC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		btcd bool
	}{
		{name: "bitcoind"},
		{name: "btcd", btcd: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testCacheLockNotHeldAcrossRPC(t, tc.btcd)
		})
	}
}

type callGate struct {
	ch   chan struct{}
	once sync.Once
}

func newCallGate() *callGate {
	return &callGate{ch: make(chan struct{})}
}

func (g *callGate) wait() {
	<-g.ch
}

func (g *callGate) unblock() {
	g.once.Do(func() {
		close(g.ch)
	})
}

func testCacheLockNotHeldAcrossRPC(t *testing.T, btcd bool) {
	low := mustSatPerKW(t, lowBTC)
	bumped := expectedRelayFloor(t, bumpBTC)
	require.Greater(t, bumped, low)
	require.Greater(t, testFallbackFee, bumped)

	h := newRPCEstimatorHarness(t, btcd)
	h.backend.setFee(3, lowBTC)

	estimateMethod := "estimatesmartfee"
	minFeeMethod := "getmempoolinfo"
	if btcd {
		estimateMethod = "estimatefee"
		minFeeMethod = "getinfo"
	}

	var current *callGate
	t.Cleanup(func() {
		if current != nil {
			current.unblock()
		}
	})

	// Success path blocks in the estimate RPC, before the cache is taken.
	release := newCallGate()
	current = release
	entered := make(chan struct{})
	var enteredOnce sync.Once
	h.backend.setHook(func(method string) {
		if method != estimateMethod {
			return
		}

		enteredOnce.Do(func() {
			close(entered)
		})
		release.wait()
	})

	type result struct {
		fee SatPerKWeight
		err error
	}
	done := make(chan result, 1)
	go func() {
		fee, err := h.estimate(3)
		done <- result{fee: fee, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		release.unblock()
		t.Fatalf("timed out waiting for %s; calls=%v",
			estimateMethod, h.backend.snapshotCalls())
	}

	locked := h.cache().mu.TryLock()
	if locked {
		h.cache().mu.Unlock()
	}
	release.unblock()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.True(t, locked, "cache lock held during estimate RPC")
		require.Equal(t, low, res.fee)
	case <-time.After(5 * time.Second):
		t.Fatal("estimate did not return after estimate RPC unblocked")
	}

	// Outage path blocks in the relay-fee refresh, after the cache hit.
	h.backend.setHook(nil)
	h.backend.setBehavior(estimateRPCErr)
	h.backend.setMinFee(bumpBTC)
	h.expireMinFee()

	release = newCallGate()
	current = release
	entered = make(chan struct{})
	enteredOnce = sync.Once{}
	h.backend.setHook(func(method string) {
		if method != minFeeMethod {
			return
		}

		enteredOnce.Do(func() {
			close(entered)
		})
		release.wait()
	})

	go func() {
		fee, err := h.estimate(3)
		done <- result{fee: fee, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		release.unblock()
		t.Fatalf("timed out waiting for %s; calls=%v",
			minFeeMethod, h.backend.snapshotCalls())
	}

	locked = h.cache().mu.TryLock()
	if locked {
		h.cache().mu.Unlock()
	}
	release.unblock()

	select {
	case res := <-done:
		require.NoError(t, res.err)
		require.True(t, locked, "cache lock held during relay-fee RPC")
		require.Equal(t, bumped, res.fee)
	case <-time.After(5 * time.Second):
		t.Fatal("estimate did not return after relay-fee RPC unblocked")
	}
}

// TestRPCEstimatorFeeCacheConcurrent runs overlapping queries for repeated and
// distinct targets, with successes and failures mixed together.
func TestRPCEstimatorFeeCacheConcurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		btcd bool
	}{
		{name: "bitcoind"},
		{name: "btcd", btcd: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testFeeCacheConcurrent(t, tc.btcd)
		})
	}
}

func testFeeCacheConcurrent(t *testing.T, btcd bool) {
	h := newRPCEstimatorHarness(t, btcd)

	const (
		goroutines = 8
		iterations = 25
	)

	for target := uint32(1); target <= 3; target++ {
		h.backend.setFee(target, lowBTC+float64(target)*0.00002)
	}
	h.backend.setBehavior(estimateAlternate)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*iterations)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				target := uint32((i+j)%3 + 1)
				_, err := h.estimate(target)
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// After the mixed traffic, a fresh success replaces the cached rate and
	// is what the next outage returns.
	fresh := mustSatPerKW(t, freshBTC)
	h.backend.setBehavior(estimateServe)
	h.backend.setFee(3, freshBTC)
	require.Equal(t, fresh, h.mustEstimate(3))

	h.backend.setBehavior(estimateRPCErr)
	require.Equal(t, fresh, h.mustEstimate(3))
	require.NotEqual(t, testFallbackFee, h.mustEstimate(3))

	// A target that the final success did not touch still falls back when
	// it has no cached rate. Target 4 was never queried.
	require.Equal(t, testFallbackFee, h.mustEstimate(4))
}
