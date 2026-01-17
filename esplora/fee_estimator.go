package esplora

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultFeeUpdateInterval is the default interval at which the fee
	// estimator will update its cached fee rates.
	defaultFeeUpdateInterval = 5 * time.Minute

	// defaultRelayFeePerKW is the default relay fee rate in sat/kw used
	// when the API doesn't provide one.
	defaultRelayFeePerKW = chainfee.SatPerKWeight(253)
)

// FeeEstimatorConfig holds the configuration for the Esplora fee estimator.
type FeeEstimatorConfig struct {
	// FallbackFeePerKW is the fee rate (in sat/kw) to use when the API
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
// uses an Esplora HTTP API to estimate transaction fees.
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

// NewFeeEstimator creates a new Esplora-based fee estimator.
func NewFeeEstimator(client *Client, cfg *FeeEstimatorConfig) *FeeEstimator {
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

	log.Info("Starting Esplora fee estimator")

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

	log.Info("Stopping Esplora fee estimator")

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

	if numBlocks > chainfee.MaxBlockTarget {
		log.Debugf("conf target %d exceeds the max value, use %d instead.",
			numBlocks, chainfee.MaxBlockTarget)
		numBlocks = chainfee.MaxBlockTarget
	}

	// Try to get from cache first.
	if feeRate, ok := e.getCachedFee(numBlocks); ok {
		return e.clampFee(feeRate), nil
	}

	// No cached data available, try to fetch fresh data.
	if err := e.updateFeeCache(); err != nil {
		log.Debugf("Failed to fetch fee estimates: %v", err)
		return e.clampFee(e.cfg.FallbackFeePerKW), nil
	}

	// Try cache again after update.
	if feeRate, ok := e.getCachedFee(numBlocks); ok {
		return e.clampFee(feeRate), nil
	}

	return e.clampFee(e.cfg.FallbackFeePerKW), nil
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed.
//
// NOTE: This is part of the chainfee.Estimator interface.
func (e *FeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	return e.relayFeePerKW
}

// updateFeeCache fetches fee estimates from the Esplora API and caches them.
func (e *FeeEstimator) updateFeeCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	estimates, err := e.client.GetFeeEstimates(ctx)
	if err != nil {
		return fmt.Errorf("failed to get fee estimates: %w", err)
	}

	newFeeCache := make(map[uint32]chainfee.SatPerKWeight)
	for targetStr, feeRate := range estimates {
		target, err := strconv.ParseUint(targetStr, 10, 32)
		if err != nil {
			continue
		}

		// Esplora returns fee rates in sat/vB, convert to sat/kw.
		// 1 vB = 4 weight units, so sat/kw = sat/vB * 1000 / 4 = sat/vB * 250
		feePerKW := satPerVBToSatPerKW(feeRate)

		// Ensure we don't go below the minimum.
		if feePerKW < e.cfg.MinFeePerKW {
			feePerKW = e.cfg.MinFeePerKW
		}

		newFeeCache[uint32(target)] = feePerKW
	}

	e.feeCacheMtx.Lock()
	e.feeCache = newFeeCache
	e.feeCacheMtx.Unlock()

	log.Debugf("Updated fee cache with %d entries", len(estimates))

	return nil
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

		case <-e.quit:
			return
		}
	}
}

// satPerVBToSatPerKW converts a fee rate from sat/vB to sat/kw.
// 1 vB = 4 weight units
// 1 kw = 1000 weight units
// So: sat/kw = sat/vB * 1000 / 4 = sat/vB * 250
func satPerVBToSatPerKW(satPerVB float64) chainfee.SatPerKWeight {
	return chainfee.SatPerKWeight(satPerVB * 250)
}

// getCachedFee finds the best cached fee for a target. It will return the exact
// target if present, otherwise the closest lower target. If no lower target
// exists, it returns the minimum cached target (cheaper than requested).
func (e *FeeEstimator) getCachedFee(numBlocks uint32) (
	chainfee.SatPerKWeight, bool) {

	e.feeCacheMtx.RLock()
	defer e.feeCacheMtx.RUnlock()

	if len(e.feeCache) == 0 {
		return 0, false
	}

	if feeRate, ok := e.feeCache[numBlocks]; ok {
		return feeRate, true
	}

	closestTarget := uint32(0)
	var closestFee chainfee.SatPerKWeight
	minTarget := uint32(math.MaxUint32)
	var minFee chainfee.SatPerKWeight
	hasMin := false

	for target, fee := range e.feeCache {
		if target <= numBlocks && target > closestTarget {
			closestTarget = target
			closestFee = fee
		}

		if target < minTarget {
			minTarget = target
			minFee = fee
			hasMin = true
		}
	}

	if closestTarget > 0 {
		log.Warnf("Esplora fee cache missing target=%d, using target=%d instead",
			numBlocks, closestTarget)
		return closestFee, true
	}

	if hasMin {
		log.Errorf("Esplora fee cache missing target=%d, using target=%d instead",
			numBlocks, minTarget)
		return minFee, true
	}

	return 0, false
}

// clampFee enforces a minimum fee floor using relay and configured floors.
func (e *FeeEstimator) clampFee(
	fee chainfee.SatPerKWeight) chainfee.SatPerKWeight {

	floor := e.relayFeePerKW
	if e.cfg.MinFeePerKW > floor {
		floor = e.cfg.MinFeePerKW
	}

	if fee < floor {
		return floor
	}

	return fee
}
