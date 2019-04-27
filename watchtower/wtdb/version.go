package wtdb

import "github.com/coreos/bbolt"

// migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx *bbolt.Tx) error

// version pairs a version number with the migration that would need to be
// applied from the prior version to upgrade.
type version struct {
	number    uint32
	migration migration
}

// dbVersions stores all versions and migrations of the database. This list will
// be used when opening the database to determine if any migrations must be
// applied.
var dbVersions = []version{
	{
		// Initial version requires no migration.
		number:    0,
		migration: nil,
	},
}

// getLatestDBVersion returns the last known database version.
func getLatestDBVersion(versions []version) uint32 {
	return versions[len(versions)-1].number
}

// getMigrations returns a slice of all updates with a greater number that
// curVersion that need to be applied to sync up with the latest version.
func getMigrations(versions []version, curVersion uint32) []version {
	var updates []version
	for _, v := range versions {
		if v.number > curVersion {
			updates = append(updates, v)
		}
	}

	return updates
}

// getDBVersion retrieves the current database version from the metadata bucket
// using the dbVersionKey.
func getDBVersion(tx *bbolt.Tx) (uint32, error) {
	metadata := tx.Bucket(metadataBkt)
	if metadata == nil {
		return 0, ErrUninitializedDB
	}

	versionBytes := metadata.Get(dbVersionKey)
	if len(versionBytes) != 4 {
		return 0, ErrNoDBVersion
	}

	return byteOrder.Uint32(versionBytes), nil
}

// initDBVersion initializes the top-level metadata bucket and writes the passed
// version number as the current version.
func initDBVersion(tx *bbolt.Tx, version uint32) error {
	_, err := tx.CreateBucketIfNotExists(metadataBkt)
	if err != nil {
		return err
	}

	return putDBVersion(tx, version)
}

// putDBVersion stores the passed database version in the metadata bucket under
// the dbVersionKey.
func putDBVersion(tx *bbolt.Tx, version uint32) error {
	metadata := tx.Bucket(metadataBkt)
	if metadata == nil {
		return ErrUninitializedDB
	}

	versionBytes := make([]byte, 4)
	byteOrder.PutUint32(versionBytes, version)
	return metadata.Put(dbVersionKey, versionBytes)
}
