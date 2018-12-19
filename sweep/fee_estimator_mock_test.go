package sweep

import (
	"github.com/lightningnetwork/lnd/lnwallet"
	"sync"
)

// mockFeeEstimator implements a mock fee estimator. It closely resembles
// lnwallet.StaticFeeEstimator with the addition that fees can be changed for
// testing purposes in a thread safe manner.
type mockFeeEstimator struct {
	feePerKW lnwallet.SatPerKWeight

	relayFee lnwallet.SatPerKWeight

	lock sync.Mutex
}

func newMockFeeEstimator(feePerKW,
	relayFee lnwallet.SatPerKWeight) *mockFeeEstimator {

	return &mockFeeEstimator{
		feePerKW: feePerKW,
		relayFee: relayFee,
	}
}

func (e *mockFeeEstimator) updateFees(feePerKW,
	relayFee lnwallet.SatPerKWeight) {

	e.lock.Lock()
	defer e.lock.Unlock()

	e.feePerKW = feePerKW
	e.relayFee = relayFee
}

func (e *mockFeeEstimator) EstimateFeePerKW(numBlocks uint32) (
	lnwallet.SatPerKWeight, error) {

	e.lock.Lock()
	defer e.lock.Unlock()

	return e.feePerKW, nil
}

func (e *mockFeeEstimator) RelayFeePerKW() lnwallet.SatPerKWeight {
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

var _ lnwallet.FeeEstimator = (*mockFeeEstimator)(nil)
