package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltdb/bolt"
)

const (
	dbName = "channel.db"
)

var (
	// Big endian is the preferred byte order, due to cursor scans over integer
	// keys iterating in order.
	byteOrder = binary.BigEndian
)

var bufPool = &sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// EncryptorDecryptor...
// TODO(roasbeef): ability to rotate EncryptorDecryptor's across DB
type EncryptorDecryptor interface {
	Encrypt(in []byte) ([]byte, error)
	Decrypt(in []byte) ([]byte, error)
	OverheadSize() uint32
}

// DB...
type DB struct {
	store *bolt.DB

	cryptoSystem EncryptorDecryptor
}

// Open opens an existing channeldb created under the passed namespace with
// sensitive data encrypted by the passed EncryptorDecryptor implementation.
// TODO(roasbeef): versioning?
func Open(dbPath string) (*DB, error) {
	if !fileExists(dbPath) {
		return nil, ErrNoExists
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	return &DB{store: bdb}, nil
}

// Create...
func Create(dbPath string) (*DB, error) {
	bdb, err := createChannelDB(dbPath)
	if err != nil {
		return nil, err
	}

	return &DB{store: bdb}, nil
}

// RegisterCryptoSystem...
func (d *DB) RegisterCryptoSystem(ed EncryptorDecryptor) {
	d.cryptoSystem = ed
}

// Wipe...
func (d *DB) Wipe() error {
	return d.store.Update(func(tx *bolt.Tx) error {
		// TODO(roasbee): delete all other top-level buckets.
		return tx.DeleteBucket(openChannelBucket)
	})
}

// Close...
func (d *DB) Close() error {
	return d.store.Close()
}

// createChannelDB...
func createChannelDB(dbPath string) (*bolt.DB, error) {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return nil, err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	err = bdb.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(channelLogBucket); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create new channeldb")
	}

	return bdb, nil
}

// fileExists...
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

