package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

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

	// TombstoneKey is the key under which we add a tag in the source DB
	// after we've successfully and completely migrated it to the target/
	// destination DB.
	TombstoneKey = []byte("data-migration-tombstone")

	// ErrMarkerNotPresent is the error that is returned if the queried
	// marker is not present in the given database.
	ErrMarkerNotPresent = errors.New("marker not present")

	// ErrInvalidOptionalVersion is the error that is returned if the
	// optional version persisted in the database is invalid.
	ErrInvalidOptionalVersion = errors.New("invalid optional version")
)

// Meta structure holds the database meta information.
type Meta struct {
	// DbVersionNumber is the current schema version of the database.
	DbVersionNumber uint32
}

// FetchMeta fetches the metadata from boltdb and returns filled meta structure.
func (d *DB) FetchMeta() (*Meta, error) {
	var meta *Meta

	err := kvdb.View(d, func(tx kvdb.RTx) error {
		return FetchMeta(meta, tx)
	}, func() {
		meta = &Meta{}
	})
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// FetchMeta is a helper function used in order to allow callers to re-use a
// database transaction.
func FetchMeta(meta *Meta, tx kvdb.RTx) error {
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

// String returns a string representation of the optional meta.
func (om *OptionalMeta) String() string {
	if len(om.Versions) == 0 {
		return "empty"
	}

	// Create a slice of indices to sort
	indices := make([]uint64, 0, len(om.Versions))
	for index := range om.Versions {
		indices = append(indices, index)
	}

	// Sort the indices in ascending order.
	slices.Sort(indices)

	// Create the string parts in sorted order.
	parts := make([]string, len(indices))
	for i, index := range indices {
		parts[i] = fmt.Sprintf("%d: %s", index, om.Versions[index])
	}

	return strings.Join(parts, ", ")
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

			// This check would not allow to downgrade LND software
			// to a version with an optional migration when an
			// optional migration not known to the current version
			// has already been applied.
			if version >= uint64(len(optionalVersions)) {
				return fmt.Errorf("optional version read "+
					"from db is %d, but only optional "+
					"migrations up to %d are known: %w",
					version, len(optionalVersions)-1,
					ErrInvalidOptionalVersion)
			}

			om.Versions[version] = optionalVersions[version].name
		}

		return nil
	}, func() {})
	if err != nil {
		return nil, err
	}

	return om, nil
}

// putOptionalMeta writes an optional meta to the database.
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

		// Write the version indexes of the single migrations.
		for v := range om.Versions {
			if v >= uint64(len(optionalVersions)) {
				return ErrInvalidOptionalVersion
			}

			err := tlv.WriteVarInt(&b, v, &[8]byte{})
			if err != nil {
				return err
			}
		}

		return metaBucket.Put(optionalVersionKey, b.Bytes())
	}, func() {})
}

// CheckMarkerPresent returns the marker under the requested key or
// ErrMarkerNotFound if either the root bucket or the marker key within that
// bucket does not exist.
func CheckMarkerPresent(tx kvdb.RTx, markerKey []byte) ([]byte, error) {
	markerBucket := tx.ReadBucket(markerKey)
	if markerBucket == nil {
		return nil, ErrMarkerNotPresent
	}

	val := markerBucket.Get(markerKey)

	// If we wrote the marker correctly, we created a bucket _and_ created a
	// key with a non-empty value. It doesn't matter to us whether the key
	// exists or whether its value is empty, to us, it just means the marker
	// isn't there.
	if len(val) == 0 {
		return nil, ErrMarkerNotPresent
	}

	return val, nil
}

// EnsureNoTombstone returns an error if there is a tombstone marker in the DB
// of the given transaction.
func EnsureNoTombstone(tx kvdb.RTx) error {
	marker, err := CheckMarkerPresent(tx, TombstoneKey)
	if err == ErrMarkerNotPresent {
		// No marker present, so no tombstone. The DB is still alive.
		return nil
	}
	if err != nil {
		return err
	}

	// There was no error so there is a tombstone marker/tag. We cannot use
	// this DB anymore.
	return fmt.Errorf("refusing to use db, it was marked with a tombstone "+
		"after successful data migration; tombstone reads: %s",
		string(marker))
}

// AddMarker adds the marker with the given key into a top level bucket with the
// same name. So the structure will look like:
//
//	marker-key (top level bucket)
//	    |->   marker-key:marker-value (key/value pair)
func AddMarker(tx kvdb.RwTx, markerKey, markerValue []byte) error {
	if len(markerValue) == 0 {
		return fmt.Errorf("marker value cannot be empty")
	}

	markerBucket, err := tx.CreateTopLevelBucket(markerKey)
	if err != nil {
		return err
	}

	return markerBucket.Put(markerKey, markerValue)
}
