package channeldb

import (
	"github.com/boltdb/bolt"
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
// structure. If transaction object is specified then it will be used rather
// than initiation creation of new one.
func (d *DB) FetchMeta(tx *bolt.Tx) (*Meta, error) {
	meta := &Meta{}
	fetchMeta := func(tx *bolt.Tx) error {
		if metaBucket := tx.Bucket(metaBucket); metaBucket != nil {
			fetchDbVersion(metaBucket, meta)
			return nil
		} else {
			return ErrMetaNotFound
		}
	}

	var err error

	if tx == nil {
		err = d.store.View(fetchMeta)
	} else {
		err = fetchMeta(tx)
	}

	if err != nil {
		return nil, err
	}

	return meta, nil
}

// PutMeta gets as input meta structure and put it into boltdb. If transaction
// object is specified then it will be used rather than initiation creation of
// new one.
func (d *DB) PutMeta(meta *Meta, tx *bolt.Tx) error {
	putMeta := func(tx *bolt.Tx) error {
		metaBucket := tx.Bucket(metaBucket)
		if metaBucket == nil {
			return ErrMetaNotFound
		}

		if err := putDbVersion(metaBucket, meta); err != nil {
			return err
		}

		return nil
	}

	if tx == nil {
		return d.store.Update(putMeta)
	} else {
		return putMeta(tx)
	}
}

func putDbVersion(metaBucket *bolt.Bucket, meta *Meta) error {
	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, meta.DbVersionNumber)
	if err := metaBucket.Put(dbVersionKey, scratch); err != nil {
		return err
	}
	return nil
}

func fetchDbVersion(metaBucket *bolt.Bucket, meta *Meta) {
	if data := metaBucket.Get(dbVersionKey); data != nil {
		meta.DbVersionNumber = byteOrder.Uint32(data)
	} else {
		meta.DbVersionNumber = getLatestDBVersion(dbVersions)
	}
}
