package channeldb

import (
	"github.com/coreos/bbolt"
)

var (
	// metaBucket stores all the meta information concerning the state of
	// the database.
	metaBucket = []byte("metadata")

	// dbVersionKey is a boltdb key and it's used for storing/retrieving
	// current database version.
	dbVersionKey = []byte("dbp")
)

// Meta structure holds the database meta information.
type Meta struct {
	// DbVersionNumber is the current schema version of the database.
	DbVersionNumber uint32
}

// FetchMeta fetches the meta data from boltdb and returns filled meta
// structure.
func (d *DB) FetchMeta(tx *bolt.Tx) (*Meta, error) {
	meta := &Meta{}

	err := d.View(func(tx *bolt.Tx) error {
		return fetchMeta(meta, tx)
	})
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// fetchMeta is an internal helper function used in order to allow callers to
// re-use a database transaction. See the publicly exported FetchMeta method
// for more information.
func fetchMeta(meta *Meta, tx *bolt.Tx) error {
	metaBucket := tx.Bucket(metaBucket)
	if metaBucket == nil {
		return ErrMetaNotFound
	}

	data := metaBucket.Get(dbVersionKey)
	if data == nil {
		meta.DbVersionNumber = getLatestDBVersion(dbVersions)
	} else {
		meta.DbVersionNumber = byteOrder.Uint32(data)
	}

	return nil
}

// PutMeta writes the passed instance of the database met-data struct to disk.
func (d *DB) PutMeta(meta *Meta) error {
	return d.Update(func(tx *bolt.Tx) error {
		return putMeta(meta, tx)
	})
}

// putMeta is an internal helper function used in order to allow callers to
// re-use a database transaction. See the publicly exported PutMeta method for
// more information.
func putMeta(meta *Meta, tx *bolt.Tx) error {
	metaBucket, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}

	return putDbVersion(metaBucket, meta)
}

func putDbVersion(metaBucket *bolt.Bucket, meta *Meta) error {
	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, meta.DbVersionNumber)
	return metaBucket.Put(dbVersionKey, scratch)
}
