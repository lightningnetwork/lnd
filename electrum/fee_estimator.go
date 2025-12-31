package electrum

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultFeeUpdateInterval is the default interval at which the fee
	// estimator will update its cached fee rates.
	defaultFeeUpdateInterval = 5 * time.Minute

	// defaultRelayFeePerKW is the default relay fee rate in sat/kw used
	// when the server doesn't provide one.
	defaultRelayFeePerKW = chainfee.SatPerKWeight(253)
)

// FeeEstimatorConfig holds the configuration for the Electrum fee estimator.
type FeeEstimatorConfig struct {
	// FallbackFeePerKW is the fee rate (in sat/kw) to use when the server
	// fails to return a fee estimate.
	FallbackFeePerKW chainfee.SatPerKWeight

	// MinFeePerKW is the minimum fee rate (in sat/kw) that should be used.
	MinFeePerKW chainfee.SatPerKWeight

	// FeeUpdateInterval is the interval at which the fee estimator will
	// update its cached fee rates.
	FeeUpdateInterval time.Duration
}

// DefaultFeeEstimatorConfig returns a FeeEstimatorConfig with sensible
// defaults.
func DefaultFeeEstimatorConfig() *FeeEstimatorConfig {
	return &FeeEstimatorConfig{
		FallbackFeePerKW:  chainfee.SatPerKWeight(12500),
		MinFeePerKW:       chainfee.FeePerKwFloor,
		FeeUpdateInterval: defaultFeeUpdateInterval,
	}
}

// FeeEstimator is an implementation of the chainfee.Estimator interface that
// uses an Electrum server to estimate transaction fees.
type FeeEstimator struct {
	started int32
	stopped int32

	cfg *FeeEstimatorConfig

	client *Client

	// relayFeePerKW is the minimum relay fee in sat/kw.
	relayFeePerKW chainfee.SatPerKWeight

	// feeCache stores the cached fee estimates by confirmation target.
	feeCacheMtx sync.RWMutex
	feeCache    map[uint32]chainfee.SatPerKWeight

	quit chan struct{}
	wg   sync.WaitGroup
}

// Compile time check to ensure FeeEstimator implements chainfee.Estimator.
var _ chainfee.Estimator = (*FeeEstimator)(nil)

// NewFeeEstimator creates a new Electrum-based fee estimator.
func NewFeeEstimator(client *Client,
	cfg *FeeEstimatorConfig) *FeeEstimator {

	if cfg == nil {
		cfg = DefaultFeeEstimatorConfig()
	}

	return &FeeEstimator{
		cfg:           cfg,
		client:        client,
		relayFeePerKW: defaultRelayFeePerKW,
		feeCache:      make(map[uint32]chainfee.SatPerKWeight),
		quit:          make(chan struct{}),
	}
}

// Start signals the FeeEstimator to start any processes or goroutines it needs
// to perform its duty.
//
// NOTE: This is part of the chainfee.Estimator interface.
func (e *FeeEstimator) Start() error {
	if atomic.AddInt32(&e.started, 1) != 1 {
		return nil
	}

	log.Info("Starting Electrum fee estimator")

	// Fetch the relay fee from the server.
	if err := e.fetchRelayFee(); err != nil {
		log.Warnf("Failed to fetch relay fee from Electrum server: %v",
			err)
	}

	// Do an initial fee cache update.
	if err := e.updateFeeCache(); err != nil {
		log.Warnf("Failed to update initial fee cache: %v", err)
	}

	// Start the background fee update goroutine.
	e.wg.Add(1)
	go e.feeUpdateLoop()

	return nil
}

// Stop stops any spawned goroutines and cleans up the resources used by the
// fee estimator.
//
// NOTE: This is part of the chainfee.Estimator interface.
func (e *FeeEstimator) Stop() error {
	if atomic.AddInt32(&e.stopped, 1) != 1 {
		return nil
	}

	log.Info("Stopping Electrum fee estimator")

	close(e.quit)
	e.wg.Wait()

	return nil
}

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
//
// NOTE: This is part of the chainfee.Estimator interface.
func (e *FeeEstimator) EstimateFeePerKW(
	numBlocks uint32) (chainfee.SatPerKWeight, error) {

	// Try to get from cache first.
	e.feeCacheMtx.RLock()
	if feeRate, ok := e.feeCache[numBlocks]; ok {
		e.feeCacheMtx.RUnlock()
		return feeRate, nil
	}
	e.feeCacheMtx.RUnlock()

	// Not in cache, fetch from server.
	feeRate, err := e.fetchFeeEstimate(numBlocks)
	if err != nil {
		log.Debugf("Failed to fetch fee estimate for %d blocks: %v",
			numBlocks, err)

		return e.cfg.FallbackFeePerKW, nil
	}

	// Cache the result.
	e.feeCacheMtx.Lock()
	e.feeCache[numBlocks] = feeRate
	e.feeCacheMtx.Unlock()

	return feeRate, nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This is part of the chainfee.Estimator interface.
func (e *FeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return e.relayFeePerKW
}

// fetchRelayFee fetches the relay fee from the Electrum server.
func (e *FeeEstimator) fetchRelayFee() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// GetRelayFee returns the fee in BTC/kB.
	relayFeeBTCPerKB, err := e.client.GetRelayFee(ctx)
	if err != nil {
		return fmt.Errorf("failed to get relay fee: %w", err)
	}

	// Convert from BTC/kB to sat/kw.
	relayFeePerKW := btcPerKBToSatPerKW(float64(relayFeeBTCPerKB))

	if relayFeePerKW < e.cfg.MinFeePerKW {
		relayFeePerKW = e.cfg.MinFeePerKW
	}

	e.relayFeePerKW = relayFeePerKW

	log.Debugf("Electrum relay fee: %v sat/kw", relayFeePerKW)

	return nil
}

// fetchFeeEstimate fetches a fee estimate for the given confirmation target
// from the Electrum server.
func (e *FeeEstimator) fetchFeeEstimate(
	numBlocks uint32) (chainfee.SatPerKWeight, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// EstimateFee returns the fee rate in BTC/kB.
	feeRateBTCPerKB, err := e.client.EstimateFee(ctx, int(numBlocks))
	if err != nil {
		return 0, fmt.Errorf("failed to estimate fee: %w", err)
	}

	// A negative fee rate means the server couldn't estimate.
	if feeRateBTCPerKB < 0 {
		return 0, fmt.Errorf("server returned negative fee rate")
	}

	// Convert from BTC/kB to sat/kw.
	feePerKW := btcPerKBToSatPerKW(float64(feeRateBTCPerKB))

	// Ensure we don't go below the minimum.
	if feePerKW < e.cfg.MinFeePerKW {
		feePerKW = e.cfg.MinFeePerKW
	}

	return feePerKW, nil
}

// updateFeeCache updates the cached fee estimates for common confirmation
// targets.
func (e *FeeEstimator) updateFeeCache() error {
	// Common confirmation targets to cache.
	targets := []uint32{1, 2, 3, 6, 12, 25, 144}

	var lastErr error

	for _, target := range targets {
		feeRate, err := e.fetchFeeEstimate(target)
		if err != nil {
			lastErr = err
			continue
		}

		e.feeCacheMtx.Lock()
		e.feeCache[target] = feeRate
		e.feeCacheMtx.Unlock()
	}

	return lastErr
}

// feeUpdateLoop periodically updates the fee cache.
func (e *FeeEstimator) feeUpdateLoop() {
	defer e.wg.Done()

	ticker := time.NewTicker(e.cfg.FeeUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := e.updateFeeCache(); err != nil {
				log.Debugf("Failed to update fee cache: %v", err)
			}

			if err := e.fetchRelayFee(); err != nil {
				log.Debugf("Failed to update relay fee: %v", err)
			}

		case <-e.quit:
			return
		}
	}
}

// btcPerKBToSatPerKW converts a fee rate from BTC/kB to sat/kw.
// 1 BTC = 100,000,000 satoshis
// 1 kB = 1000 bytes
// 1 kw = 1000 weight units
// For segwit, 1 vbyte = 4 weight units, so 1 kB = 4 kw.
// Therefore: sat/kw = (BTC/kB * 100,000,000) / 4
func btcPerKBToSatPerKW(btcPerKB float64) chainfee.SatPerKWeight {
	satPerKB := btcutil.Amount(btcPerKB * btcutil.SatoshiPerBitcoin)
	satPerKW := satPerKB / 4

	return chainfee.SatPerKWeight(satPerKW)
}
