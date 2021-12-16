package chainfee

import (
	"sync"
	"time"
)

const defaultUpdateInterval = 10 * time.Minute

// minFeeManager is used to store and update the minimum fee that is required
// by a transaction to be accepted to the mempool. The minFeeManager ensures
// that the backend used to fetch the fee is not queried too regularly.
type minFeeManager struct {
	mu                sync.Mutex
	minFeePerKW       SatPerKWeight
	lastUpdatedTime   time.Time
	minUpdateInterval time.Duration
	fetchFeeFunc      fetchFee
}

// fetchFee represents a function that can be used to fetch a fee.
type fetchFee func() (SatPerKWeight, error)

// newMinFeeManager creates a new minFeeManager and uses the
// given fetchMinFee function to set the minFeePerKW of the minFeeManager.
// This function requires the fetchMinFee function to succeed.
func newMinFeeManager(minUpdateInterval time.Duration,
	fetchMinFee fetchFee) (*minFeeManager, error) {

	minFee, err := fetchMinFee()
	if err != nil {
		return nil, err
	}

	// Ensure that the minimum fee we use is always clamped by our fee
	// floor.
	if minFee < FeePerKwFloor {
		minFee = FeePerKwFloor
	}

	return &minFeeManager{
		minFeePerKW:       minFee,
		lastUpdatedTime:   time.Now(),
		minUpdateInterval: minUpdateInterval,
		fetchFeeFunc:      fetchMinFee,
	}, nil
}

// fetchMinFee returns the stored minFeePerKW if it has been updated recently
// or if the call to the chain backend fails. Otherwise, it sets the stored
// minFeePerKW to the fee returned from the backend and floors it based on
// our fee floor.
func (m *minFeeManager) fetchMinFee() SatPerKWeight {
	m.mu.Lock()
	defer m.mu.Unlock()

	if time.Since(m.lastUpdatedTime) < m.minUpdateInterval {
		return m.minFeePerKW
	}

	newMinFee, err := m.fetchFeeFunc()
	if err != nil {
		log.Errorf("Unable to fetch updated min fee from chain "+
			"backend. Using last known min fee instead: %v", err)

		return m.minFeePerKW
	}

	// By default, we'll use the backend node's minimum fee as the
	// minimum fee rate we'll propose for transactions. However, if this
	// happens to be lower than our fee floor, we'll enforce that instead.
	m.minFeePerKW = newMinFee
	if m.minFeePerKW < FeePerKwFloor {
		m.minFeePerKW = FeePerKwFloor
	}
	m.lastUpdatedTime = time.Now()

	log.Debugf("Using minimum fee rate of %v sat/kw",
		int64(m.minFeePerKW))

	return m.minFeePerKW
}
