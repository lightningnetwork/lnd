package migration_01_to_11

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
)

// applyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func applyMigration(t *testing.T, beforeMigration, afterMigration func(d *DB),
	migrationFunc migration, shouldFail bool) {

	cdb, err := makeTestDB(t)
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

	defer func() {
		if r := recover(); r != nil {
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

	// Apply migration.
	err = kvdb.Update(cdb, func(tx kvdb.RwTx) error {
		return migrationFunc(tx)
	}, func() {})
	if err != nil {
		log.Error(err)
	}
}
