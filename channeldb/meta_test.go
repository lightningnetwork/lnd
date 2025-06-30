package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// applyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func applyMigration(t *testing.T, beforeMigration, afterMigration func(d *DB),
	migrationFunc migration, shouldFail bool, dryRun bool) {

	cdb, err := MakeTestDB(t)
	if err != nil {
		t.Fatal(err)
	}
	cdb.dryRun = dryRun

	// beforeMigration usually used for populating the database
	// with test data.
	beforeMigration(cdb)

	// Create test meta info with zero database version and put it on disk.
	// Than creating the version list pretending that new version was added.
	meta := &Meta{DbVersionNumber: 0}
	if err := cdb.PutMeta(meta); err != nil {
		t.Fatalf("unable to store meta data: %v", err)
	}

	versions := []mandatoryVersion{
		{
			number:    0,
			migration: nil,
		},
		{
			number:    1,
			migration: migrationFunc,
		},
	}

	defer func() {
		if r := recover(); r != nil {
			if dryRun && r != ErrDryRunMigrationOK {
				t.Fatalf("expected dry run migration OK")
			}
			err = fmt.Errorf("%v", r)
		}

		if err == nil && shouldFail {
			t.Fatal("error wasn't received on migration stage")
		} else if err != nil && !shouldFail {
			t.Fatalf("error was received on migration stage: %v", err)
		}

		// afterMigration usually used for checking the database state and
		// throwing the error if something went wrong.
		afterMigration(cdb)
	}()

	// Sync with the latest version - applying migration function.
	err = cdb.syncVersions(versions)
	if err != nil {
		log.Error(err)
	}
}

// TestVersionFetchPut checks the propernces of fetch/put methods
// and also initialization of meta data in case if don't have any in
// database.
func TestVersionFetchPut(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	if err != nil {
		t.Fatal(err)
	}

	meta, err := db.FetchMeta()
	if err != nil {
		t.Fatal(err)
	}

	if meta.DbVersionNumber != getLatestDBVersion(dbVersions) {
		t.Fatal("initialization of meta information wasn't performed")
	}

	newVersion := getLatestDBVersion(dbVersions) + 1
	meta.DbVersionNumber = newVersion

	if err := db.PutMeta(meta); err != nil {
		t.Fatalf("update of meta failed %v", err)
	}

	meta, err = db.FetchMeta()
	if err != nil {
		t.Fatal(err)
	}

	if meta.DbVersionNumber != newVersion {
		t.Fatal("update of meta information wasn't performed")
	}
}

// TestOrderOfMigrations checks that migrations are applied in proper order.
func TestOrderOfMigrations(t *testing.T) {
	t.Parallel()

	appliedMigration := -1
	versions := []mandatoryVersion{
		{0, nil},
		{1, nil},
		{2, func(tx kvdb.RwTx) error {
			appliedMigration = 2
			return nil
		}},
		{3, func(tx kvdb.RwTx) error {
			appliedMigration = 3
			return nil
		}},
	}

	// Retrieve the migration that should be applied to db, as far as
	// current version is 1, we skip zero and first versions.
	migrations, _ := getMigrationsToApply(versions, 1)

	if len(migrations) != 2 {
		t.Fatal("incorrect number of migrations to apply")
	}

	// Apply first migration.
	migrations[0](nil)

	// Check that first migration corresponds to the second version.
	if appliedMigration != 2 {
		t.Fatal("incorrect order of applying migrations")
	}

	// Apply second migration.
	migrations[1](nil)

	// Check that second migration corresponds to the third version.
	if appliedMigration != 3 {
		t.Fatal("incorrect order of applying migrations")
	}
}

// TestGlobalVersionList checks that there is no mistake in global version list
// in terms of version ordering.
func TestGlobalVersionList(t *testing.T) {
	t.Parallel()

	if dbVersions == nil {
		t.Fatal("can't find versions list")
	}

	if len(dbVersions) == 0 {
		t.Fatal("db versions list is empty")
	}

	prev := dbVersions[0].number
	for i := 1; i < len(dbVersions); i++ {
		version := dbVersions[i].number

		if version == prev {
			t.Fatal("duplicates db versions")
		}
		if version < prev {
			t.Fatal("order of db versions is wrong")
		}

		prev = version
	}
}

// TestMigrationWithPanic asserts that if migration logic panics, we will return
// to the original state unaltered.
func TestMigrationWithPanic(t *testing.T) {
	t.Parallel()

	bucketPrefix := []byte("somebucket")
	keyPrefix := []byte("someprefix")
	beforeMigration := []byte("beforemigration")
	afterMigration := []byte("aftermigration")

	beforeMigrationFunc := func(d *DB) {
		// Insert data in database and in order then make sure that the
		// key isn't changes in case of panic or fail.
		err := kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			return bucket.Put(keyPrefix, beforeMigration)
		}, func() {})
		if err != nil {
			t.Fatalf("unable to insert: %v", err)
		}
	}

	// Create migration function which changes the initially created data and
	// throw the panic, in this case we pretending that something goes.
	migrationWithPanic := func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
		if err != nil {
			return err
		}

		bucket.Put(keyPrefix, afterMigration)
		panic("panic!")
	}

	// Check that version of database and data wasn't changed.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta()
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 0 {
			t.Fatal("migration panicked but version is changed")
		}

		err = kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			value := bucket.Get(keyPrefix)
			if !bytes.Equal(value, beforeMigration) {
				return errors.New("migration failed but data is " +
					"changed")
			}

			return nil
		}, func() {})
		if err != nil {
			t.Fatal(err)
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		migrationWithPanic,
		true,
		false)
}

// TestMigrationWithFatal asserts that migrations which fail do not modify the
// database.
func TestMigrationWithFatal(t *testing.T) {
	t.Parallel()

	bucketPrefix := []byte("somebucket")
	keyPrefix := []byte("someprefix")
	beforeMigration := []byte("beforemigration")
	afterMigration := []byte("aftermigration")

	beforeMigrationFunc := func(d *DB) {
		err := kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			return bucket.Put(keyPrefix, beforeMigration)
		}, func() {})
		if err != nil {
			t.Fatalf("unable to insert pre migration key: %v", err)
		}
	}

	// Create migration function which changes the initially created data and
	// return the error, in this case we pretending that something goes
	// wrong.
	migrationWithFatal := func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
		if err != nil {
			return err
		}

		bucket.Put(keyPrefix, afterMigration)
		return errors.New("some error")
	}

	// Check that version of database and initial data wasn't changed.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta()
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 0 {
			t.Fatal("migration failed but version is changed")
		}

		err = kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			value := bucket.Get(keyPrefix)
			if !bytes.Equal(value, beforeMigration) {
				return errors.New("migration failed but data is " +
					"changed")
			}

			return nil
		}, func() {})
		if err != nil {
			t.Fatal(err)
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		migrationWithFatal,
		true,
		false)
}

// TestMigrationWithoutErrors asserts that a successful migration has its
// changes applied to the database.
func TestMigrationWithoutErrors(t *testing.T) {
	t.Parallel()

	bucketPrefix := []byte("somebucket")
	keyPrefix := []byte("someprefix")
	beforeMigration := []byte("beforemigration")
	afterMigration := []byte("aftermigration")

	// Populate database with initial data.
	beforeMigrationFunc := func(d *DB) {
		err := kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			return bucket.Put(keyPrefix, beforeMigration)
		}, func() {})
		if err != nil {
			t.Fatalf("unable to update db pre migration: %v", err)
		}
	}

	// Create migration function which changes the initially created data.
	migrationWithoutErrors := func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
		if err != nil {
			return err
		}

		bucket.Put(keyPrefix, afterMigration)
		return nil
	}

	// Check that version of database and data was properly changed.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta()
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 1 {
			t.Fatal("version number isn't changed after " +
				"successfully applied migration")
		}

		err = kvdb.Update(d, func(tx kvdb.RwTx) error {
			bucket, err := tx.CreateTopLevelBucket(bucketPrefix)
			if err != nil {
				return err
			}

			value := bucket.Get(keyPrefix)
			if !bytes.Equal(value, afterMigration) {
				return errors.New("migration wasn't applied " +
					"properly")
			}

			return nil
		}, func() {})
		if err != nil {
			t.Fatal(err)
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		migrationWithoutErrors,
		false,
		false)
}

// TestMigrationReversion tests after performing a migration to a higher
// database version, opening the database with a lower latest db version returns
// ErrDBReversion.
func TestMigrationReversion(t *testing.T) {
	t.Parallel()

	tempDirName := t.TempDir()

	backend, cleanup, err := kvdb.GetTestBackend(tempDirName, "cdb")
	require.NoError(t, err, "unable to get test db backend")

	cdb, err := CreateWithBackend(backend)
	if err != nil {
		cleanup()
		t.Fatalf("unable to open channeldb: %v", err)
	}

	// Update the database metadata to point to one more than the highest
	// known version.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		newMeta := &Meta{
			DbVersionNumber: getLatestDBVersion(dbVersions) + 1,
		}

		return putMeta(newMeta, tx)
	}, func() {})

	// Close the database. Even if we succeeded, our next step is to reopen.
	cdb.Close()
	cleanup()

	require.NoError(t, err, "unable to increase db version")

	backend, cleanup, err = kvdb.GetTestBackend(tempDirName, "cdb")
	require.NoError(t, err, "unable to get test db backend")
	t.Cleanup(cleanup)

	_, err = CreateWithBackend(backend)
	if err != ErrDBReversion {
		t.Fatalf("unexpected error when opening channeldb, "+
			"want: %v, got: %v", ErrDBReversion, err)
	}
}

// TestMigrationDryRun ensures that opening the database in dry run migration
// mode will fail and not commit the migration.
func TestMigrationDryRun(t *testing.T) {
	t.Parallel()

	// Nothing to do, will inspect version number.
	beforeMigrationFunc := func(d *DB) {}

	// Check that version of database version is not modified.
	afterMigrationFunc := func(d *DB) {
		err := kvdb.View(d, func(tx kvdb.RTx) error {
			meta, err := d.FetchMeta()
			if err != nil {
				t.Fatal(err)
			}

			if meta.DbVersionNumber != 0 {
				t.Fatal("dry run migration was not aborted")
			}

			return nil
		}, func() {})
		if err != nil {
			t.Fatalf("unable to apply after func: %v", err)
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		func(kvdb.RwTx) error { return nil },
		true,
		true)
}

// TestOptionalMeta checks the basic read and write for the optional meta.
func TestOptionalMeta(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err)

	// Test read an empty optional meta.
	om, err := db.fetchOptionalMeta()
	require.NoError(t, err, "error getting optional meta")
	require.Empty(t, om.Versions, "expected empty versions")

	// Test write an optional meta.
	om = &OptionalMeta{
		Versions: map[uint64]string{
			0: optionalVersions[0].name,
			1: optionalVersions[1].name,
		},
	}
	err = db.putOptionalMeta(om)
	require.NoError(t, err, "error putting optional meta")

	om1, err := db.fetchOptionalMeta()
	require.NoError(t, err, "error getting optional meta")
	require.Equal(t, om, om1, "unexpected empty versions")
	require.Equal(
		t, "0: prune_revocation_log, 1: gc_decayed_log",
		om1.String(),
	)
}

// TestApplyOptionalVersions checks that the optional migration is applied as
// expected based on the config.
//
// NOTE: Cannot be run in parallel because we alter the optionalVersions
// global variable which could be used by other tests.
func TestApplyOptionalVersions(t *testing.T) {
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	// migrateCount is the number of migrations that have been run. It
	// counts the number of times a migration function is called.
	var migrateCount int

	// Modify all migrations to track their execution.
	for i := range optionalVersions {
		optionalVersions[i].migration = func(_ kvdb.Backend,
			_ MigrationConfig) error {

			migrateCount++

			return nil
		}
	}

	// All migrations are disabled by default.
	cfg := NewOptionalMiragtionConfig()

	// Run the optional migrations.
	err = db.applyOptionalVersions(cfg)
	require.NoError(t, err, "failed to apply optional migration")
	require.Equal(t, 0, migrateCount, "expected no migration")

	// Check the optional meta is not updated.
	om, err := db.fetchOptionalMeta()
	require.NoError(t, err, "error getting optional meta")

	// Enable all optional migrations.
	for i := range cfg.MigrationFlags {
		cfg.MigrationFlags[i] = true
	}

	err = db.applyOptionalVersions(cfg)
	require.NoError(t, err, "failed to apply optional migration")
	require.Equal(
		t, len(optionalVersions), migrateCount,
		"expected all migrations to be run",
	)

	// Fetch the updated optional meta.
	om, err = db.fetchOptionalMeta()
	require.NoError(t, err, "error getting optional meta")

	// Verify that the optional meta is updated as expected.
	omExpected := &OptionalMeta{
		Versions: map[uint64]string{
			0: optionalVersions[0].name,
			1: optionalVersions[1].name,
		},
	}
	require.Equal(t, omExpected, om, "unexpected empty versions")

	// We make sure running the migrations again does not call the
	// migrations again because the meta data should signal that they have
	// already been run.
	err = db.applyOptionalVersions(cfg)
	require.NoError(t, err, "failed to apply optional migration")
	require.Equal(
		t, len(optionalVersions), migrateCount,
		"expected all migrations to be run",
	)
}

// TestFetchMeta tests that the FetchMeta returns the latest DB version for a
// freshly created DB instance.
func TestFetchMeta(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err)

	meta := &Meta{}
	err = db.View(func(tx walletdb.ReadTx) error {
		return FetchMeta(meta, tx)
	}, func() {
		meta = &Meta{}
	})
	require.NoError(t, err)

	require.Equal(t, LatestDBVersion(), meta.DbVersionNumber)
}

// TestMarkerAndTombstone tests that markers like a tombstone can be added to a
// DB.
func TestMarkerAndTombstone(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err)

	// Test that a generic marker is not present in a fresh DB.
	var marker []byte
	err = db.View(func(tx walletdb.ReadTx) error {
		var err error
		marker, err = CheckMarkerPresent(tx, []byte("foo"))
		return err
	}, func() {
		marker = nil
	})
	require.ErrorIs(t, err, ErrMarkerNotPresent)
	require.Nil(t, marker)

	// Only adding the marker bucket should not be enough to be counted as
	// a marker, we explicitly also want the value to be set.
	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket([]byte("foo"))
		return err
	}, func() {})
	require.NoError(t, err)

	err = db.View(func(tx walletdb.ReadTx) error {
		var err error
		marker, err = CheckMarkerPresent(tx, []byte("foo"))
		return err
	}, func() {
		marker = nil
	})
	require.ErrorIs(t, err, ErrMarkerNotPresent)
	require.Nil(t, marker)

	// Test that a tombstone marker is not present in a fresh DB.
	err = db.View(EnsureNoTombstone, func() {})
	require.NoError(t, err)

	// Add a generic marker now and assert that it can be read.
	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		return AddMarker(tx, []byte("foo"), []byte("bar"))
	}, func() {})
	require.NoError(t, err)

	err = db.View(func(tx walletdb.ReadTx) error {
		var err error
		marker, err = CheckMarkerPresent(tx, []byte("foo"))
		return err
	}, func() {
		marker = nil
	})
	require.NoError(t, err)
	require.Equal(t, []byte("bar"), marker)

	// A tombstone should still not be present.
	err = db.View(EnsureNoTombstone, func() {})
	require.NoError(t, err)

	// Finally, add a tombstone.
	tombstoneText := []byte("RIP test DB")
	err = db.Update(func(tx walletdb.ReadWriteTx) error {
		return AddMarker(tx, TombstoneKey, tombstoneText)
	}, func() {})
	require.NoError(t, err)

	// We can read it as a normal marker.
	err = db.View(func(tx walletdb.ReadTx) error {
		var err error
		marker, err = CheckMarkerPresent(tx, TombstoneKey)
		return err
	}, func() {
		marker = nil
	})
	require.NoError(t, err)
	require.Equal(t, tombstoneText, marker)

	// But also as a tombstone, and now we should get an error that the DB
	// cannot be used anymore.
	err = db.View(EnsureNoTombstone, func() {})
	require.ErrorContains(t, err, string(tombstoneText))

	// Now that the DB has a tombstone, we should no longer be able to open
	// it once we close it.
	_, err = CreateWithBackend(db.Backend)
	require.ErrorContains(t, err, string(tombstoneText))
}
