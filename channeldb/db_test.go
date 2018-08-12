package channeldb

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-errors/errors"
)

func TestOpenWithCreate(t *testing.T) {
	t.Parallel()

	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDirName)

	// Next, open thereby creating channeldb for the first time.
	dbPath := filepath.Join(tempDirName, "cdb")
	cdb, err := Open(dbPath)
	if err != nil {
		t.Fatalf("unable to create channeldb: %v", err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatalf("unable to close channeldb: %v", err)
	}

	// The path should have been successfully created.
	if !fileExists(dbPath) {
		t.Fatalf("channeldb failed to create data directory")
	}
}

// applyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func applyMigration(t *testing.T, beforeMigration, afterMigration func(d *DB),
	migrationFunc migration, shouldFail bool) {

	cdb, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
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
			err = errors.New(r)
		}

		if err == nil && shouldFail {
			t.Fatal("error wasn't received on migration stage")
		} else if err != nil && !shouldFail {
			t.Fatal("error was received on migration stage")
		}

		// afterMigration usually used for checking the database state and
		// throwing the error if something went wrong.
		afterMigration(cdb)
	}()

	// Sync with the latest version - applying migration function.
	err = cdb.syncVersions(versions)
}
