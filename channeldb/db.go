package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
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

	netParams *chaincfg.Params

	cryptoSystem EncryptorDecryptor
}

// Open opens an existing channeldb created under the passed namespace with
// sensitive data encrypted by the passed EncryptorDecryptor implementation.
// TODO(roasbeef): versioning?
func Open(dbPath string, netParams *chaincfg.Params) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	return &DB{store: bdb, netParams: netParams}, nil
}

// RegisterCryptoSystem...
func (d *DB) RegisterCryptoSystem(ed EncryptorDecryptor) {
	d.cryptoSystem = ed
}

// Wipe...
func (d *DB) Wipe() error {
	return d.store.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(openChannelBucket); err != nil {
			return err
		}

		return tx.DeleteBucket(closedChannelBucket)
	})
}

// Close...
func (d *DB) Close() error {
	return d.store.Close()
}

// createChannelDB...
func createChannelDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return err
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
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
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

// FetchOpenChannel...
func (d *DB) FetchOpenChannels(nodeID *wire.ShaHash) ([]*OpenChannel, error) {
	var channels []*OpenChannel
	err := d.store.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChannelBucket == nil {
			return fmt.Errorf("open channel bucket does not exist")
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket := openChanBucket.Bucket(nodeID[:])
		if nodeChanBucket == nil {
			return nil
		}

		// Once we have the node's channel bucket, iterate through each
		// item in the inner chan ID bucket. This bucket acts as an
		// index for all channels we currently have open with this node.
		nodeChanIDBucket := nodeChanBucket.Bucket(chanIDBucket[:])
		err := nodeChanIDBucket.ForEach(func(k, v []byte) error {
			outBytes := bytes.NewReader(k)
			chanID := &wire.OutPoint{}
			if err := readOutpoint(outBytes, chanID); err != nil {
				return err
			}

			oChannel, err := fetchOpenChannel(openChanBucket,
				nodeChanBucket, chanID, d.cryptoSystem)
			if err != nil {
				return err
			}

			channels = append(channels, oChannel)
			return nil
		})
		if err != nil {
			return err
		}

		return nil
	})

	return channels, err
}
