package peer

import (
	"errors"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// peerStorage is the key used in kvdb to store backup data received
	// from peers.
	peerStorage = []byte("peer-storage")

	// ErrUninitializedDB signifies an error encountered when attempting
	// to access the database before it has been set up.
	ErrUninitializedDB = errors.New("uninitialized Kvdb peer data store")
)

// PeerStorageProducer creates a backup storage instance for a peer.
//
//nolint:revive
type PeerStorageProducer struct {
	DB kvdb.Backend
}

// NewPeerStorageProducer initializes a PeerStorageProducer with a top-level
// peer storage bucket in kvdb.Backend. It returns an error if bucket creation
// or kvdb operations fails.
func NewPeerStorageProducer(dB kvdb.Backend) (*PeerStorageProducer, error) {
	if err := kvdb.Update(dB, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(peerStorage)
		if err != nil {
			return err
		}

		return nil
	}, func() {}); err != nil {
		return nil, err
	}

	return &PeerStorageProducer{
		DB: dB,
	}, nil
}

// PeerStorageDB is the kvdb implementation of the PeerDataStore interface.
//
//nolint:revive
type PeerStorageDB struct {
	*PeerStorageProducer
	pubKey []byte
}

// NewPeerStorageDB creates a new PeerStorageDB instance associated with the
// given public key.
func (p *PeerStorageProducer) NewPeerStorageDB(key []byte) *PeerStorageDB {
	return &PeerStorageDB{
		pubKey:              key,
		PeerStorageProducer: p,
	}
}

// Store persists the peer's backup in the storage layer.
func (k *PeerStorageDB) Store(data []byte) error {
	return kvdb.Update(k.DB, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(peerStorage)

		if bucket == nil {
			return ErrUninitializedDB
		}

		return bucket.Put(k.pubKey, data)
	}, func() {})
}

// Retrieve fetches the peer's backup from the storage layer.
func (k *PeerStorageDB) Retrieve() ([]byte, error) {
	var data []byte
	if err := kvdb.View(k.DB, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(peerStorage)
		if bucket == nil {
			return ErrUninitializedDB
		}

		blob := bucket.Get(k.pubKey)
		data = append([]byte(nil), blob...)

		return nil
	}, func() {
		data = nil
	}); err != nil {
		return nil, err
	}

	return data, nil
}

// Delete removes the peer's backup from the storage layer.
func (k *PeerStorageDB) Delete() error {
	return kvdb.Update(k.DB, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(peerStorage)
		if bucket == nil {
			return ErrUninitializedDB
		}

		return bucket.Delete(k.pubKey)
	}, func() {})
}
