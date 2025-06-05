package decayedlog

import (
	"encoding/binary"
	"errors"

	mig1 "github.com/lightningnetwork/lnd/decayedlog/kvmigrations/migration1"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// metaBucket stores all the meta information concerning the state of
	// the database.
	metaBucket = []byte("metadata")

	// dbVersionKey is a boltdb key and it's used for storing/retrieving
	// current database version.
	dbVersionKey = []byte("dbp")

	// byteOrder is the byte order used for serializing integers to the
	// database.
	byteOrder = binary.BigEndian

	// ErrDBReversion is returned when we detect a database version that is
	// higher than our latest known version.
	ErrDBReversion = errors.New("database version is higher than latest " +
		"known version")

	// ErrMetaNotFound is returned when meta bucket hasn't been
	// created.
	ErrMetaNotFound = errors.New("unable to locate meta information")
)

// migration is a function which takes a prior outdated version of the
// database instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx kvdb.RwTx) error

// mandatoryVersion defines a db version that must be applied before the lnd
// starts.
type mandatoryVersion struct {
	number    uint32
	migration migration
}

var (
	dbVersions = []mandatoryVersion{
		{
			number:    0,
			migration: nil,
		},
		{
			number:    1,
			migration: mig1.Migrate,
		},
	}
)

// getLatestDBVersion returns the last known database version.
func getLatestDBVersion(versions []mandatoryVersion) uint32 {
	return dbVersions[len(dbVersions)-1].number
}

// getMigrationsToApply retrieves the migration functions that should be
// applied to the database.
func getMigrationsToApply(versions []mandatoryVersion,
	version uint32) ([]migration, []uint32) {

	migrations := make([]migration, 0, len(versions))
	migrationVersions := make([]uint32, 0, len(versions))

	for _, v := range versions {
		if v.number > version {
			migrations = append(migrations, v.migration)
			migrationVersions = append(migrationVersions, v.number)
		}
	}

	return migrations, migrationVersions
}

// Meta structure holds the database meta information.
type Meta struct {
	// DbVersionNumber is the current schema version of the database.
	DbVersionNumber uint32
}

// FetchMeta fetches the metadata from boltdb and returns filled meta structure.
func (d *DecayedLog) FetchMeta() (*Meta, error) {
	var meta *Meta

	err := kvdb.View(d.db, func(tx kvdb.RTx) error {
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

// syncVersions ensures the database version is consistent with the highest
// known database version, applying any migrations that have not been made. If
// the highest known version number is lower than the database's version, this
// method will fail to prevent accidental reversions.
func (d *DecayedLog) syncVersions(versions []mandatoryVersion) error {
	meta, err := d.FetchMeta()
	if err != nil {
		if err == ErrMetaNotFound {
			meta = &Meta{}
		} else {
			return err
		}
	}

	currentVersion := meta.DbVersionNumber

	latestVersion := getLatestDBVersion(versions)
	log.Infof("Checking for schema update: latest_version=%v, "+
		"db_version=%v", latestVersion, currentVersion)

	switch {
	// If the database reports a higher version that we are aware of, the
	// user is probably trying to revert to a prior version of lnd. We fail
	// here to prevent reversions and unintended corruption.
	case currentVersion > latestVersion:
		log.Errorf("Refusing to revert from db_version=%d to "+
			"lower version=%d", currentVersion, latestVersion)
		return ErrDBReversion

	// If the current database version matches the latest version number,
	// then we don't need to perform any migrations.
	case currentVersion == latestVersion:
		return nil
	}

	log.Infof("Performing database schema migration")

	// Otherwise, we fetch the migrations which need to applied, and
	// execute them serially within a single database transaction to ensure
	// the migration is atomic.
	migrations, migrationVersions := getMigrationsToApply(
		versions, currentVersion,
	)

	return kvdb.Update(d.db, func(tx kvdb.RwTx) error {
		for i, migration := range migrations {
			if migration == nil {
				continue
			}

			log.Infof("Applying migration #%v",
				migrationVersions[i])

			if err := migration(tx); err != nil {
				log.Infof("Unable to apply migration #%v",
					migrationVersions[i])
				return err
			}
		}

		// Update the database version to the latest version
		metaBucket, err := tx.CreateTopLevelBucket(metaBucket)
		if err != nil {
			return err
		}

		versionBytes := make([]byte, 4)
		byteOrder.PutUint32(versionBytes, latestVersion)
		return metaBucket.Put(dbVersionKey, versionBytes)
	}, func() {})
}
