package migration30

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/mock"
)

// mockStore mocks the shachain.Store.
type mockStore struct {
	mock.Mock
}

// A compile time check to ensure mockStore implements the Store interface.
var _ shachain.Store = (*mockStore)(nil)

func (m *mockStore) LookUp(height uint64) (*chainhash.Hash, error) {
	args := m.Called(height)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*chainhash.Hash), args.Error(1)
}

func (m *mockStore) AddNextEntry(preimage *chainhash.Hash) error {
	args := m.Called(preimage)

	return args.Error(0)
}

// Encode encodes a series of dummy values to pass the serialize/deserialize
// process.
func (m *mockStore) Encode(w io.Writer) error {
	err := binary.Write(w, binary.BigEndian, int8(1))
	if err != nil {
		return err
	}

	if err := binary.Write(w, binary.BigEndian, uint64(0)); err != nil {
		return err
	}
	if _, err = w.Write(preimage2); err != nil {
		return err
	}
	return binary.Write(w, binary.BigEndian, uint64(0))
}
