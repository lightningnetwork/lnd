package sweep

import (
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
)

type MockFeePreference struct {
	mock.Mock
}

// Compile-time constraint to ensure MockFeePreference implements FeePreference.
var _ FeePreference = (*MockFeePreference)(nil)

func (m *MockFeePreference) String() string {
	return "mock fee preference"
}

func (m *MockFeePreference) Estimate(estimator chainfee.Estimator,
	maxFeeRate chainfee.SatPerKWeight) (chainfee.SatPerKWeight, error) {

	args := m.Called(estimator, maxFeeRate)

	if args.Get(0) == nil {
		return 0, args.Error(1)
	}

	return args.Get(0).(chainfee.SatPerKWeight), args.Error(1)
}

type mockUtxoAggregator struct {
	mock.Mock
}

// Compile-time constraint to ensure mockUtxoAggregator implements
// UtxoAggregator.
var _ UtxoAggregator = (*mockUtxoAggregator)(nil)

// ClusterInputs takes a list of inputs and groups them into clusters.
func (m *mockUtxoAggregator) ClusterInputs(pendingInputs) []Cluster {
	args := m.Called(pendingInputs{})

	return args.Get(0).([]Cluster)
}
