package migration_01_to_11

import (
	"testing"

	"github.com/go-errors/errors"
)

// applyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func applyMigration(t *testing.T, beforeMigration, afterMigration func(d *DB),
	migrationFunc migration, shouldFail bool) {

	cdb, cleanUp, err := makeTestDB()
	defer cleanUp()
	if err != nil {
		t.Fatal(err)
	}

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
