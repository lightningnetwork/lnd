package channeldb

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/kvdb"
)

// applyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func applyMigration(t *testing.T, beforeMigration, afterMigration func(d *DB),
	migrationFunc migration, shouldFail bool, dryRun bool) {

	cdb, cleanUp, err := MakeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatal(err)
	}
	cdb.dryRun = dryRun

	// Create a test node that will be our source node.
	testNode, err := createTestVertex(cdb)
	if err != nil {
		t.Fatal(err)
	}
	graph := cdb.ChannelGraph()
	if err := graph.SetSourceNode(testNode); err != nil {
		t.Fatal(err)
	}

	// beforeMigration usually used for populating the database
	// with test data.
	beforeMigration(cdb)

	// Create test meta info with zero database version and put it on disk.
	// Than creating the version list pretending that new version was added.
	meta := &Meta{DbVersionNumber: 0}
	if err := cdb.PutMeta(meta); err != nil {
		t.Fatalf("unable to store meta data: %v", err)
	}

	versions := []version{
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
			err = errors.New(r)
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

	db, cleanUp, err := MakeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatal(err)
	}

	meta, err := db.FetchMeta(nil)
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

	meta, err = db.FetchMeta(nil)
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
	versions := []version{
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
		meta, err := d.FetchMeta(nil)
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
		meta, err := d.FetchMeta(nil)
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
		meta, err := d.FetchMeta(nil)
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

	tempDirName, err := ioutil.TempDir("", "channeldb")
	defer func() {
		os.RemoveAll(tempDirName)
	}()
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	backend, cleanup, err := kvdb.GetTestBackend(tempDirName, "cdb")
	if err != nil {
		t.Fatalf("unable to get test db backend: %v", err)
	}

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

	if err != nil {
		t.Fatalf("unable to increase db version: %v", err)
	}

	backend, cleanup, err = kvdb.GetTestBackend(tempDirName, "cdb")
	if err != nil {
		t.Fatalf("unable to get test db backend: %v", err)
	}
	defer cleanup()

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
			meta, err := d.FetchMeta(nil)
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
