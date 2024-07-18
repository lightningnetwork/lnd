package chainio

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
)

// MockBeat is a mock implementation of the Beat interface.
type MockBeat struct {
	mock.Mock
}

// Compile-time check to ensure MockBeat satisfies the Blockbeat interface.
var _ Blockbeat = (*MockBeat)(nil)

// Height returns the height of the block epoch.
func (m *MockBeat) Height() int32 {
	args := m.Called()

	return args.Get(0).(int32)
}

func (m *MockBeat) NotifyBlockProcessed(err error, quitChan chan struct{}) {
	m.Called(err, quitChan)
}

// DispatchSequential takes a list of consumers and notify them about the new
// epoch sequentially.
func (m *MockBeat) DispatchSequential(consumers []Consumer) error {
	args := m.Called(consumers)

	return args.Error(0)
}

// DispatchConcurrent notifies each consumer concurrently about the blockbeat.
func (m *MockBeat) DispatchConcurrent(consumers []Consumer) error {
	args := m.Called(consumers)

	return args.Error(0)
}

// HasOutpointSpentByScript queries the block to find a spending tx that spends
// the given outpoint using the pkScript.
func (m *MockBeat) HasOutpointSpentByScript(outpoint wire.OutPoint,
	pkScript txscript.PkScript) (*chainntnfs.SpendDetail, error) {

	args := m.Called(outpoint, pkScript)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*chainntnfs.SpendDetail), args.Error(1)
}

// HasOutpointSpent queries the block to find a spending tx that spends the
// given outpoint. Returns the spend details if found, otherwise nil.
func (m *MockBeat) HasOutpointSpent(
	outpoint wire.OutPoint) *chainntnfs.SpendDetail {

	args := m.Called(outpoint)

	if args.Get(0) == nil {
		return nil
	}

	return args.Get(0).(*chainntnfs.SpendDetail)
}
