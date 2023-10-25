package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// MockSweeperStore is a mock implementation of sweeper store. This type is
// exported, because it is currently used in nursery tests too.
type MockSweeperStore struct {
	ourTxes map[chainhash.Hash]struct{}
}

// NewMockSweeperStore returns a new instance.
func NewMockSweeperStore() *MockSweeperStore {
	return &MockSweeperStore{
		ourTxes: make(map[chainhash.Hash]struct{}),
	}
}

// IsOurTx determines whether a tx is published by us, based on its
// hash.
func (s *MockSweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	_, ok := s.ourTxes[hash]
	return ok, nil
}

// StoreTx stores a tx we are about to publish.
func (s *MockSweeperStore) StoreTx(tr *TxRecord) error {
	s.ourTxes[tr.Txid] = struct{}{}

	return nil
}

// ListSweeps lists all the sweeps we have successfully published.
func (s *MockSweeperStore) ListSweeps() ([]chainhash.Hash, error) {
	var txns []chainhash.Hash
	for tx := range s.ourTxes {
		txns = append(txns, tx)
	}

	return txns, nil
}

// Compile-time constraint to ensure MockSweeperStore implements SweeperStore.
var _ SweeperStore = (*MockSweeperStore)(nil)
