// +build kvdb_etcd

package etcd

import (
	"github.com/btcsuite/btcwallet/walletdb"
)

// readWriteTx holds a reference to the STM transaction.
type readWriteTx struct {
	// stm is the reference to the parent STM.
	stm STM

	// rootBucketID holds the sha256 hash of the root bucket id, which is used
	// for key space spearation.
	rootBucketID [bucketIDLength]byte

	// active is true if the transaction hasn't been committed yet.
	active bool

	// dirty is true if we intent to update a value in this transaction.
	dirty bool

	// lset holds key/value set that we want to lock on. If upon commit the
	// transaction is dirty and the lset is not empty, we'll bump the mod
	// version of these key/values.
	lset map[string]string
}

// newReadWriteTx creates an rw transaction with the passed STM.
func newReadWriteTx(stm STM, prefix string) *readWriteTx {
	return &readWriteTx{
		stm:          stm,
		active:       true,
		rootBucketID: makeBucketID([]byte(prefix)),
		lset:         make(map[string]string),
	}
}

// rooBucket is a helper function to return the always present
// pseudo root bucket.
func rootBucket(tx *readWriteTx) *readWriteBucket {
	return newReadWriteBucket(tx, tx.rootBucketID[:], tx.rootBucketID[:])
}

// lock adds a key value to the lock set.
func (tx *readWriteTx) lock(key, val string) {
	tx.stm.Lock(key)
	if !tx.dirty {
		tx.lset[key] = val
	} else {
		// Bump the mod version of the key,
		// leaving the value intact.
		tx.stm.Put(key, val)
	}
}

// put updates the passed key/value.
func (tx *readWriteTx) put(key, val string) {
	tx.stm.Put(key, val)
	tx.setDirty()
}

// del marks the passed key deleted.
func (tx *readWriteTx) del(key string) {
	tx.stm.Del(key)
	tx.setDirty()
}

// setDirty marks the transaction dirty and bumps
// mod version for the existing lock set if it is
// not empty.
func (tx *readWriteTx) setDirty() {
	// Bump the lock set.
	if !tx.dirty && len(tx.lset) > 0 {
		for key, val := range tx.lset {
			// Bump the mod version of the key,
			// leaving the value intact.
			tx.stm.Put(key, val)
		}

		// Clear the lock set.
		tx.lset = make(map[string]string)
	}

	// Set dirty.
	tx.dirty = true
}

// ReadBucket opens the root bucket for read only access.  If the bucket
// described by the key does not exist, nil is returned.
func (tx *readWriteTx) ReadBucket(key []byte) walletdb.ReadBucket {
	return rootBucket(tx).NestedReadWriteBucket(key)
}

// Rollback closes the transaction, discarding changes (if any) if the
// database was modified by a write transaction.
func (tx *readWriteTx) Rollback() error {
	// If the transaction has been closed roolback will fail.
	if !tx.active {
		return walletdb.ErrTxClosed
	}

	// Rollback the STM and set the tx to inactive.
	tx.stm.Rollback()
	tx.active = false

	return nil
}

// ReadWriteBucket opens the root bucket for read/write access.  If the
// bucket described by the key does not exist, nil is returned.
func (tx *readWriteTx) ReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	return rootBucket(tx).NestedReadWriteBucket(key)
}

// CreateTopLevelBucket creates the top level bucket for a key if it
// does not exist.  The newly-created bucket it returned.
func (tx *readWriteTx) CreateTopLevelBucket(key []byte) (walletdb.ReadWriteBucket, error) {
	return rootBucket(tx).CreateBucketIfNotExists(key)
}

// DeleteTopLevelBucket deletes the top level bucket for a key.  This
// errors if the bucket can not be found or the key keys a single value
// instead of a bucket.
func (tx *readWriteTx) DeleteTopLevelBucket(key []byte) error {
	return rootBucket(tx).DeleteNestedBucket(key)
}

// Commit commits the transaction if not already committed. Will return
// error if the underlying STM fails.
func (tx *readWriteTx) Commit() error {
	// Commit will fail if the transaction is already committed.
	if !tx.active {
		return walletdb.ErrTxClosed
	}

	// Try committing the transaction.
	if err := tx.stm.Commit(); err != nil {
		return err
	}

	// Mark the transaction as not active after commit.
	tx.active = false

	return nil
}

// OnCommit sets the commit callback (overriding if already set).
func (tx *readWriteTx) OnCommit(cb func()) {
	tx.stm.OnCommit(cb)
}
