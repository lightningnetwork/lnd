package channeldb

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// metaBucket stores all the meta information concerning the state of
	// the database.
	metaBucket = []byte("metadata")

	// dbVersionKey is a boltdb key and it's used for storing/retrieving
	// current database version.
	dbVersionKey = []byte("dbp")

	// dbVersionKey is a boltdb key and it's used for storing/retrieving
	// a list of optional migrations that have been applied.
	optionalVersionKey = []byte("ovk")
)

// Meta structure holds the database meta information.
type Meta struct {
	// DbVersionNumber is the current schema version of the database.
	DbVersionNumber uint32
}

// FetchMeta fetches the meta data from boltdb and returns filled meta
// structure.
func (d *DB) FetchMeta(tx kvdb.RTx) (*Meta, error) {
	var meta *Meta

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		return fetchMeta(meta, tx)
	}, func() {
		meta = &Meta{}
	})
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// fetchMeta is an internal helper function used in order to allow callers to
// re-use a database transaction. See the publicly exported FetchMeta method
// for more information.
func fetchMeta(meta *Meta, tx kvdb.RTx) error {
	metaBucket := tx.ReadBucket(metaBucket)
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
	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		return putMeta(meta, tx)
	}, func() {})
}

// putMeta is an internal helper function used in order to allow callers to
// re-use a database transaction. See the publicly exported PutMeta method for
// more information.
func putMeta(meta *Meta, tx kvdb.RwTx) error {
	metaBucket, err := tx.CreateTopLevelBucket(metaBucket)
	if err != nil {
		return err
	}

	return putDbVersion(metaBucket, meta)
}

func putDbVersion(metaBucket kvdb.RwBucket, meta *Meta) error {
	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, meta.DbVersionNumber)
	return metaBucket.Put(dbVersionKey, scratch)
}

// OptionalMeta structure holds the database optional migration information.
type OptionalMeta struct {
	// Versions is a set that contains the versions that have been applied.
	// When saved to disk, only the indexes are stored.
	Versions map[uint64]string
}

func (om *OptionalMeta) String() string {
	s := ""
	for index, name := range om.Versions {
		s += fmt.Sprintf("%d: %s", index, name)
	}
	if s == "" {
		s = "empty"
	}
	return s
}

// fetchOptionalMeta reads the optional meta from the database.
func (d *DB) fetchOptionalMeta() (*OptionalMeta, error) {
	om := &OptionalMeta{
		Versions: make(map[uint64]string),
	}

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		metaBucket := tx.ReadBucket(metaBucket)
		if metaBucket == nil {
			return ErrMetaNotFound
		}

		vBytes := metaBucket.Get(optionalVersionKey)
		// Exit early if nothing found.
		if vBytes == nil {
			return nil
		}

		// Read the versions' length.
		r := bytes.NewReader(vBytes)
		vLen, err := tlv.ReadVarInt(r, &[8]byte{})
		if err != nil {
			return err
		}

		// Write the version index.
		for i := uint64(0); i < vLen; i++ {
			version, err := tlv.ReadVarInt(r, &[8]byte{})
			if err != nil {
				return err
			}
			om.Versions[version] = optionalVersions[i].name
		}

		return nil
	}, func() {})
	if err != nil {
		return nil, err
	}

	return om, nil
}

// fetchOptionalMeta writes an optional meta to the database.
func (d *DB) putOptionalMeta(om *OptionalMeta) error {
	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		metaBucket, err := tx.CreateTopLevelBucket(metaBucket)
		if err != nil {
			return err
		}

		var b bytes.Buffer

		// Write the total length.
		err = tlv.WriteVarInt(&b, uint64(len(om.Versions)), &[8]byte{})
		if err != nil {
			return err
		}

		// Write the version indexes.
		for v := range om.Versions {
			err := tlv.WriteVarInt(&b, v, &[8]byte{})
			if err != nil {
				return err
			}
		}

		return metaBucket.Put(optionalVersionKey, b.Bytes())
	}, func() {})
}
