package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/mock"
)

// MockSweeperStore is a mock implementation of sweeper store. This type is
// exported, because it is currently used in nursery tests too.
type MockSweeperStore struct {
	mock.Mock
}

// NewMockSweeperStore returns a new instance.
func NewMockSweeperStore() *MockSweeperStore {
	return &MockSweeperStore{}
}

// IsOurTx determines whether a tx is published by us, based on its hash.
func (s *MockSweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	args := s.Called(hash)

	return args.Bool(0), args.Error(1)
}

// StoreTx stores a tx we are about to publish.
func (s *MockSweeperStore) StoreTx(tr *TxRecord) error {
	args := s.Called(tr)
	return args.Error(0)
}

// ListSweeps lists all the sweeps we have successfully published.
func (s *MockSweeperStore) ListSweeps() ([]chainhash.Hash, error) {
	args := s.Called()

	return args.Get(0).([]chainhash.Hash), args.Error(1)
}

// GetTx queries the database to find the tx that matches the given txid.
// Returns ErrTxNotFound if it cannot be found.
func (s *MockSweeperStore) GetTx(hash chainhash.Hash) (*TxRecord, error) {
	args := s.Called(hash)

	tr := args.Get(0)
	if tr != nil {
		return args.Get(0).(*TxRecord), args.Error(1)
	}

	return nil, args.Error(1)
}

// DeleteTx removes the given tx from db.
func (s *MockSweeperStore) DeleteTx(txid chainhash.Hash) error {
	args := s.Called(txid)

	return args.Error(0)
}

// Compile-time constraint to ensure MockSweeperStore implements SweeperStore.
var _ SweeperStore = (*MockSweeperStore)(nil)
