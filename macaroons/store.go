package macaroons

import (
	"crypto/rand"
	"fmt"
	"io"

	"golang.org/x/net/context"

	"github.com/coreos/bbolt"

	"github.com/roasbeef/btcwallet/snacl"
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

	// encryptedKeyID is the name of the database key that stores the
	// encryption key, encrypted with a salted + hashed password. The
	// format is 32 bytes of salt, and the rest is encrypted key.
	encryptedKeyID = []byte("enckey")

	// ErrAlreadyUnlocked specifies that the store has already been
	// unlocked.
	ErrAlreadyUnlocked = fmt.Errorf("macaroon store already unlocked")

	// ErrStoreLocked specifies that the store needs to be unlocked with
	// a password.
	ErrStoreLocked = fmt.Errorf("macaroon store is locked")

	// ErrPasswordRequired specifies that a nil password has been passed.
	ErrPasswordRequired = fmt.Errorf("a non-nil password is required")
)

// RootKeyStorage implements the bakery.RootKeyStorage interface.
type RootKeyStorage struct {
	*bolt.DB

	encKey *snacl.SecretKey
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
	return &RootKeyStorage{db, nil}, nil
}

// CreateUnlock sets an encryption key if one is not already set, otherwise it
// checks if the password is correct for the stored encryption key.
func (r *RootKeyStorage) CreateUnlock(password *[]byte) error {
	// Check if we've already unlocked the store; return an error if so.
	if r.encKey != nil {
		return ErrAlreadyUnlocked
	}

	// Check if a nil password has been passed; return an error if so.
	if password == nil {
		return ErrPasswordRequired
	}

	return r.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(rootKeyBucketName)
		dbKey := bucket.Get(encryptedKeyID)
		if len(dbKey) > 0 {
			// We've already stored a key, so try to unlock with
			// the password.
			encKey := &snacl.SecretKey{}
			err := encKey.Unmarshal(dbKey)
			if err != nil {
				return err
			}

			err = encKey.DeriveKey(password)
			if err != nil {
				return err
			}

			r.encKey = encKey
			return nil
		}

		// We haven't yet stored a key, so create a new one.
		encKey, err := snacl.NewSecretKey(password, snacl.DefaultN,
			snacl.DefaultR, snacl.DefaultP)
		if err != nil {
			return err
		}

		err = bucket.Put(encryptedKeyID, encKey.Marshal())
		if err != nil {
			return err
		}

		r.encKey = encKey
		return nil
	})
}

// Get implements the Get method for the bakery.RootKeyStorage interface.
func (r *RootKeyStorage) Get(_ context.Context, id []byte) ([]byte, error) {
	if r.encKey == nil {
		return nil, ErrStoreLocked
	}
	var rootKey []byte
	err := r.View(func(tx *bolt.Tx) error {
		dbKey := tx.Bucket(rootKeyBucketName).Get(id)
		if len(dbKey) == 0 {
			return fmt.Errorf("root key with id %s doesn't exist",
				string(id))
		}

		decKey, err := r.encKey.Decrypt(dbKey)
		if err != nil {
			return err
		}

		rootKey = make([]byte, len(decKey))
		copy(rootKey[:], decKey)
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
	if r.encKey == nil {
		return nil, nil, ErrStoreLocked
	}
	var rootKey []byte
	id := defaultRootKeyID
	err := r.Update(func(tx *bolt.Tx) error {
		ns := tx.Bucket(rootKeyBucketName)
		dbKey := ns.Get(id)

		// If there's a root key stored in the bucket, decrypt it and
		// return it.
		if len(dbKey) != 0 {
			decKey, err := r.encKey.Decrypt(dbKey)
			if err != nil {
				return err
			}

			rootKey = make([]byte, len(decKey))
			copy(rootKey[:], decKey[:])
			return nil
		}

		// Otherwise, create a RootKeyLen-byte root key, encrypt it,
		// and store it in the bucket.
		rootKey = make([]byte, RootKeyLen)
		if _, err := io.ReadFull(rand.Reader, rootKey[:]); err != nil {
			return err
		}

		encKey, err := r.encKey.Encrypt(rootKey)
		if err != nil {
			return err
		}
		return ns.Put(id, encKey)
	})
	if err != nil {
		return nil, nil, err
	}

	return rootKey, id, nil
}

// Close closes the underlying database and zeroes the encryption key stored
// in memory.
func (r *RootKeyStorage) Close() error {
	if r.encKey != nil {
		r.encKey.Zero()
	}
	return r.DB.Close()
}
