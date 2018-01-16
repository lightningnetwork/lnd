package macaroons

import (
	"crypto/rand"
	"fmt"
	"io"

	"golang.org/x/net/context"

	"github.com/boltdb/bolt"
)

const (
	// RootKeyLen is the length of a root key.
	RootKeyLen = 32
)

var (
	// rootKeyBucketName is the name of the root key store bucket.
	rootKeyBucketName = []byte("macrootkeys")

	// defaultRootKeyID is the ID of the default root key. The first is
	// just 0, to emulate the memory storage that comes with bakery.
	//
	// TODO(aakselrod): Add support for key rotation.
	defaultRootKeyID = []byte("0")

	// macaroonBucketName is the name of the macaroon store bucket.
	macaroonBucketName = []byte("macaroons")
)

// RootKeyStorage implements the bakery.RootKeyStorage interface.
type RootKeyStorage struct {
	*bolt.DB
}

// NewRootKeyStorage creates a RootKeyStorage instance.
// TODO(aakselrod): Add support for encryption of data with passphrase.
func NewRootKeyStorage(db *bolt.DB) (*RootKeyStorage, error) {
	// If the store's bucket doesn't exist, create it.
	err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(rootKeyBucketName)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Return the DB wrapped in a RootKeyStorage object.
	return &RootKeyStorage{db}, nil
}

// Get implements the Get method for the bakery.RootKeyStorage interface.
func (r *RootKeyStorage) Get(_ context.Context, id []byte) ([]byte, error) {
	var rootKey []byte
	err := r.View(func(tx *bolt.Tx) error {
		dbKey := tx.Bucket(rootKeyBucketName).Get(id)
		if len(dbKey) == 0 {
			return fmt.Errorf("root key with id %s doesn't exist",
				string(id))
		}

		rootKey = make([]byte, len(dbKey))
		copy(rootKey[:], dbKey)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return rootKey, nil
}

// RootKey implements the RootKey method for the bakery.RootKeyStorage
// interface.
// TODO(aakselrod): Add support for key rotation.
func (r *RootKeyStorage) RootKey(_ context.Context) ([]byte, []byte, error) {
	var rootKey []byte
	id := defaultRootKeyID
	err := r.Update(func(tx *bolt.Tx) error {
		ns := tx.Bucket(rootKeyBucketName)
		rootKey = ns.Get(id)

		// If there's no root key stored in the bucket yet, create one.
		if len(rootKey) != 0 {
			return nil
		}

		// Create a RootKeyLen-byte root key.
		rootKey = make([]byte, RootKeyLen)
		if _, err := io.ReadFull(rand.Reader, rootKey[:]); err != nil {
			return err
		}
		return ns.Put(id, rootKey)
	})
	if err != nil {
		return nil, nil, err
	}

	return rootKey, id, nil
}
