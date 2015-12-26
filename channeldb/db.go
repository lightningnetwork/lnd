package channeldb

import (
	"bytes"
	"encoding/binary"
	"sync"

	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
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

	namespace walletdb.Namespace
}

// Wipe...
func (d *DB) Wipe() error {
	return d.namespace.Update(func(tx walletdb.Tx) error {
		rootBucket := tx.RootBucket()
		// TODO(roasbeef): other buckets
		return rootBucket.DeleteBucket(openChannelBucket)
	})
}

// New...
// TODO(roasbeef): re-visit this dependancy...
func New(addrmgr *waddrmgr.Manager, namespace walletdb.Namespace) *DB {
	// TODO(roasbeef): create buckets if not created?
	return &DB{addrmgr, namespace}
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
