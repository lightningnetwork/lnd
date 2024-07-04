package chainfee

import (
	"github.com/stretchr/testify/mock"
)

type mockFeeSource struct {
	mock.Mock
}

// A compile-time assertion to ensure that mockFeeSource implements the
// WebAPIFeeSource interface.
var _ WebAPIFeeSource = (*mockFeeSource)(nil)

func (m *mockFeeSource) GetFeeInfo() (WebAPIResponse, error) {
	args := m.Called()

	return args.Get(0).(WebAPIResponse), args.Error(1)
}

// MockEstimator implements the `Estimator` interface and is used by
// other packages for mock testing.
type MockEstimator struct {
	mock.Mock
}

// Compile time assertion that MockEstimator implements Estimator.
var _ Estimator = (*MockEstimator)(nil)

// EstimateFeePerKW takes in a target for the number of blocks until an initial
// confirmation and returns the estimated fee expressed in sat/kw.
func (m *MockEstimator) EstimateFeePerKW(
	numBlocks uint32) (SatPerKWeight, error) {

	args := m.Called(numBlocks)

	if args.Get(0) == nil {
		return 0, args.Error(1)
	}

	return args.Get(0).(SatPerKWeight), args.Error(1)
}

// Start signals the Estimator to start any processes or goroutines it needs to
// perform its duty.
func (m *MockEstimator) Start() error {
	args := m.Called()

	return args.Error(0)
}

// Stop stops any spawned goroutines and cleans up the resources used by the
// fee estimator.
func (m *MockEstimator) Stop() error {
	args := m.Called()

	return args.Error(0)
}

// RelayFeePerKW returns the minimum fee rate required for transactions to be
// relayed. This is also the basis for calculation of the dust limit.
func (m *MockEstimator) RelayFeePerKW() SatPerKWeight {
	args := m.Called()

	return args.Get(0).(SatPerKWeight)
}
