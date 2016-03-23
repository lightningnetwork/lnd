package channeldb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

const (
	dbName = "channel.db"
)

var (
	endian = binary.BigEndian
)

var bufPool = &sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// Store...
// TODO(roasbeef): CHECKSUMS, REDUNDANCY, etc etc.
type DB struct {
	// TODO(roasbeef): caching, etc?
	addrmgr *waddrmgr.Manager

	db *bolt.DB
}

// Wipe...
func (d *DB) Wipe() error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket(openChannelBucket)
	})
}

// New...
// TODO(roasbeef): re-visit this dependancy...
func New(dbPath string, addrmgr *waddrmgr.Manager) (*DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dbPath, 0700); err != nil {
				return nil, err
			}
		}
	}

	path := filepath.Join(dbPath, dbName)
	boltDB, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	return &DB{addrmgr, boltDB}, nil
}

// Open...
// TODO(roasbeef): create+open, ditch New, fixes above
func Open() *DB {
	return nil
}

// Create...
func Create() *DB {
	return nil
}

// Close...
func (d *DB) Close() error {
	return d.db.Close()
}

// TODO(roasbeef): SetCryptoSystem method...
//  * don't have waddrmgr up before..
