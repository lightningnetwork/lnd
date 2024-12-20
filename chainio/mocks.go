package chainio

import (
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/mock"
)

// MockConsumer is a mock implementation of the Consumer interface.
type MockConsumer struct {
	mock.Mock
}

// Compile-time constraint to ensure MockConsumer implements Consumer.
var _ Consumer = (*MockConsumer)(nil)

// Name returns a human-readable string for this subsystem.
func (m *MockConsumer) Name() string {
	args := m.Called()
	return args.String(0)
}

// ProcessBlock takes a blockbeat and processes it. A receive-only error chan
// must be returned.
func (m *MockConsumer) ProcessBlock(b Blockbeat) error {
	args := m.Called(b)

	return args.Error(0)
}

// MockBlockbeat is a mock implementation of the Blockbeat interface.
type MockBlockbeat struct {
	mock.Mock
}

// Compile-time constraint to ensure MockBlockbeat implements Blockbeat.
var _ Blockbeat = (*MockBlockbeat)(nil)

// Height returns the current block height.
func (m *MockBlockbeat) Height() int32 {
	args := m.Called()

	return args.Get(0).(int32)
}

// logger returns the logger for the blockbeat.
func (m *MockBlockbeat) logger() btclog.Logger {
	args := m.Called()

	return args.Get(0).(btclog.Logger)
}
