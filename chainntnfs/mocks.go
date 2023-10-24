package chainntnfs

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
)

// MockMempoolWatcher is a mock implementation of the MempoolWatcher interface.
// This is used by other subsystems to mock the behavior of the mempool
// watcher.
type MockMempoolWatcher struct {
	mock.Mock
}

// NewMockMempoolWatcher returns a new instance of a mock mempool watcher.
func NewMockMempoolWatcher() *MockMempoolWatcher {
	return &MockMempoolWatcher{}
}

// Compile-time check to ensure MockMempoolWatcher implements MempoolWatcher.
var _ MempoolWatcher = (*MockMempoolWatcher)(nil)

// SubscribeMempoolSpent implements the MempoolWatcher interface.
func (m *MockMempoolWatcher) SubscribeMempoolSpent(
	op wire.OutPoint) (*MempoolSpendEvent, error) {

	args := m.Called(op)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MempoolSpendEvent), args.Error(1)
}

// CancelMempoolSpendEvent implements the MempoolWatcher interface.
func (m *MockMempoolWatcher) CancelMempoolSpendEvent(
	sub *MempoolSpendEvent) {

	m.Called(sub)
}
