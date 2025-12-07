package lntest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// WebFeeService defines an interface that's used to provide fee estimation
// service used in the integration tests. It must provide an URL so that a lnd
// node can be started with the flag `--fee.url` and uses the customized fee
// estimator.
type WebFeeService interface {
	// Start starts the service.
	Start() error

	// Stop stops the service.
	Stop() error

	// URL returns the service's endpoint.
	URL() string

	// SetFeeRate sets the estimated fee rate for a given confirmation
	// target.
	SetFeeRate(feeRate chainfee.SatPerKWeight, conf uint32)

	// SetMinRelayFeerate sets a min relay feerate.
	SetMinRelayFeerate(fee chainfee.SatPerKVByte)

	// Reset resets the fee rate map to the default value.
	Reset()
}

const (
	// feeServiceTarget is the confirmation target for which a fee estimate
	// is returned. Requests for higher confirmation targets will fall back
	// to this.
	feeServiceTarget = 1

	// DefaultFeeRateSatPerKw specifies the default fee rate used in the
	// tests.
	DefaultFeeRateSatPerKw = 12500
)

// FeeService runs a web service that provides fee estimation information.
type FeeService struct {
	*testing.T

	feeRateMap      map[uint32]uint32
	minRelayFeerate chainfee.SatPerKVByte
	url             string

	srv  *http.Server
	wg   sync.WaitGroup
	lock sync.Mutex
}

// Compile-time check for the WebFeeService interface.
var _ WebFeeService = (*FeeService)(nil)

// NewFeeService spins up a go-routine to serve fee estimates.
func NewFeeService(t *testing.T) *FeeService {
	t.Helper()

	port := port.NextAvailablePort()
	f := FeeService{
		T: t,
		url: fmt.Sprintf(
			"http://localhost:%v/fee-estimates.json", port,
		),
	}

	// Initialize default fee estimate.
	f.feeRateMap = map[uint32]uint32{
		feeServiceTarget: DefaultFeeRateSatPerKw,
	}
	f.minRelayFeerate = chainfee.FeePerKwFloor.FeePerKVByte()

	listenAddr := fmt.Sprintf(":%v", port)
	mux := http.NewServeMux()
	mux.HandleFunc("/fee-estimates.json", f.handleRequest)

	f.srv = &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: lnd.DefaultHTTPHeaderTimeout,
	}

	return &f
}

// Start starts the web server.
func (f *FeeService) Start() error {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()

		if err := f.srv.ListenAndServe(); err != http.ErrServerClosed {
			require.NoErrorf(f, err, "cannot start fee api")
		}
	}()

	return nil
}

// handleRequest handles a client request for fee estimates.
func (f *FeeService) handleRequest(w http.ResponseWriter, _ *http.Request) {
	f.lock.Lock()
	bytes, err := json.Marshal(
		chainfee.WebAPIResponse{
			FeeByBlockTarget: f.feeRateMap,
			MinRelayFeerate:  f.minRelayFeerate,
		},
	)
	f.lock.Unlock()
	require.NoErrorf(f, err, "cannot serialize estimates")

	_, err = io.WriteString(w, string(bytes))
	require.NoError(f, err, "cannot send estimates")
}

// Stop stops the web server.
func (f *FeeService) Stop() error {
	err := f.srv.Shutdown(f.Context())
	require.NoError(f, err, "cannot stop fee api")

	f.wg.Wait()

	return nil
}

// SetFeeRate sets a fee for the given confirmation target.
func (f *FeeService) SetFeeRate(fee chainfee.SatPerKWeight, conf uint32) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.feeRateMap[conf] = uint32(fee.FeePerKVByte())
}

// SetMinRelayFeerate sets a min relay feerate.
func (f *FeeService) SetMinRelayFeerate(fee chainfee.SatPerKVByte) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.minRelayFeerate = fee
}

// Reset resets the fee rate map to the default value.
func (f *FeeService) Reset() {
	f.lock.Lock()
	f.feeRateMap = make(map[uint32]uint32)
	f.minRelayFeerate = chainfee.FeePerKwFloor.FeePerKVByte()
	f.lock.Unlock()

	// Initialize default fee estimate.
	f.SetFeeRate(DefaultFeeRateSatPerKw, 1)
}

// URL returns the service endpoint.
func (f *FeeService) URL() string {
	return f.url
}
