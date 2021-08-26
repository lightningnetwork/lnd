package lntest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// feeServiceTarget is the confirmation target for which a fee estimate
	// is returned. Requests for higher confirmation targets will fall back
	// to this.
	feeServiceTarget = 1
)

// feeService runs a web service that provides fee estimation information.
type feeService struct {
	feeEstimates

	srv *http.Server
	wg  sync.WaitGroup

	url string

	lock sync.Mutex
}

// feeEstimates contains the current fee estimates.
type feeEstimates struct {
	Fees map[uint32]uint32 `json:"fee_by_block_target"`
}

// startFeeService spins up a go-routine to serve fee estimates.
func startFeeService() *feeService {
	port := NextAvailablePort()
	f := feeService{
		url: fmt.Sprintf("http://localhost:%v/fee-estimates.json", port),
	}

	// Initialize default fee estimate.
	f.Fees = map[uint32]uint32{feeServiceTarget: 50000}

	listenAddr := fmt.Sprintf(":%v", port)
	mux := http.NewServeMux()
	mux.HandleFunc("/fee-estimates.json", f.handleRequest)

	f.srv = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()

		if err := f.srv.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Printf("error: cannot start fee api: %v", err)
		}
	}()

	return &f
}

// handleRequest handles a client request for fee estimates.
func (f *feeService) handleRequest(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()

	bytes, err := json.Marshal(f.feeEstimates)
	if err != nil {
		fmt.Printf("error: cannot serialize "+
			"estimates: %v", err)

		return
	}

	_, err = io.WriteString(w, string(bytes))
	if err != nil {
		fmt.Printf("error: cannot send estimates: %v",
			err)
	}
}

// stop stops the web server.
func (f *feeService) stop() {
	if err := f.srv.Shutdown(context.Background()); err != nil {
		fmt.Printf("error: cannot stop fee api: %v", err)
	}

	f.wg.Wait()
}

// setFee changes the current fee estimate for the fixed confirmation target.
func (f *feeService) setFee(fee chainfee.SatPerKWeight) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Fees[feeServiceTarget] = uint32(fee.FeePerKVByte())
}

// setFeeWithConf sets a fee for the given confirmation target.
func (f *feeService) setFeeWithConf(fee chainfee.SatPerKWeight, conf uint32) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.Fees[conf] = uint32(fee.FeePerKVByte())
}
