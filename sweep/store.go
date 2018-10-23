package sweep

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
)

// SweeperStore stores published txes.
type SweeperStore interface {
	// IsOurTx determines whether a tx is published by us, based on its
	// hash.
	IsOurTx(hash chainhash.Hash) bool

	// NotifyPublishTx signals that we are about to publish a tx.
	NotifyPublishTx(*wire.MsgTx) error

	// GetLastPublishedTx returns the last tx that we called NotifyPublishTx
	// for.
	GetLastPublishedTx() (*wire.MsgTx, error)
}

type sweeperStore struct {
	db *channeldb.DB
}

func newSweeperStore(db *channeldb.DB) (*sweeperStore, error) {
	return &sweeperStore{
		db: db,
	}, nil
}

func (s *sweeperStore) GetUnconfirmedTxes() ([]*wire.MsgTx, error) {
	return nil, nil
}

func (s *sweeperStore) RemoveTxByInput(wire.OutPoint) error {
	return nil
}

func (s *sweeperStore) IsUnconfirmedOutput(wire.OutPoint) bool {
	return false
}

func (s *sweeperStore) AddUnconfirmedTx(*wire.MsgTx) error {
	return nil
}

func (s *sweeperStore) IsOurTx(hash chainhash.Hash) bool {
	return false
}

// Compile-time constraint to ensure sweeperStore implements SweeperStore.
// var _ SweeperStore = (*sweeperStore)(nil)
