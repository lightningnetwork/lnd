package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// MockSweeperStore is a mock implementation of sweeper store. This type is
// exported, because it is currently used in nursery tests too.
type MockSweeperStore struct {
	lastTx  *wire.MsgTx
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

// NotifyPublishTx signals that we are about to publish a tx.
func (s *MockSweeperStore) NotifyPublishTx(tx *wire.MsgTx) error {
	txHash := tx.TxHash()
	s.ourTxes[txHash] = struct{}{}
	s.lastTx = tx

	return nil
}

// GetLastPublishedTx returns the last tx that we called NotifyPublishTx
// for.
func (s *MockSweeperStore) GetLastPublishedTx() (*wire.MsgTx, error) {
	return s.lastTx, nil
}

// Compile-time constraint to ensure MockSweeperStore implements SweeperStore.
var _ SweeperStore = (*MockSweeperStore)(nil)
