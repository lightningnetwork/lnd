package sweep

import (
	"sync"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// mockFeeEstimator implements a mock fee estimator. It closely resembles
// lnwallet.StaticFeeEstimator with the addition that fees can be changed for
// testing purposes in a thread safe manner.
type mockFeeEstimator struct {
	feePerKW chainfee.SatPerKWeight

	relayFee chainfee.SatPerKWeight

	blocksToFee map[uint32]chainfee.SatPerKWeight

	// A closure that when set is used instead of the
	// mockFeeEstimator.EstimateFeePerKW method.
	estimateFeePerKW func(numBlocks uint32) (chainfee.SatPerKWeight, error)

	lock sync.Mutex
}

func newMockFeeEstimator(feePerKW,
	relayFee chainfee.SatPerKWeight) *mockFeeEstimator {

	return &mockFeeEstimator{
		feePerKW:    feePerKW,
		relayFee:    relayFee,
		blocksToFee: make(map[uint32]chainfee.SatPerKWeight),
	}
}

func (e *mockFeeEstimator) updateFees(feePerKW,
	relayFee chainfee.SatPerKWeight) {

	e.lock.Lock()
	defer e.lock.Unlock()

	e.feePerKW = feePerKW
	e.relayFee = relayFee
}

func (e *mockFeeEstimator) EstimateFeePerKW(numBlocks uint32) (
	chainfee.SatPerKWeight, error) {

	e.lock.Lock()
	defer e.lock.Unlock()

	if e.estimateFeePerKW != nil {
		return e.estimateFeePerKW(numBlocks)
	}

	if fee, ok := e.blocksToFee[numBlocks]; ok {
		return fee, nil
	}

	return e.feePerKW, nil
}

func (e *mockFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	e.lock.Lock()
	defer e.lock.Unlock()

	return e.relayFee
}

func (e *mockFeeEstimator) Start() error {
	return nil
}

func (e *mockFeeEstimator) Stop() error {
	return nil
}

var _ chainfee.Estimator = (*mockFeeEstimator)(nil)
