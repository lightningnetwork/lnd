package migtest

import (
	"fmt"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// MakeDB creates a new instance of the ChannelDB for testing purposes.
func MakeDB(t testing.TB) (kvdb.Backend, error) {
	// Create temporary database for mission control.
	file, err := os.CreateTemp("", "*.db")
	if err != nil {
		return nil, err
	}

	dbPath := file.Name()
	t.Cleanup(func() {
		require.NoError(t, file.Close())
		require.NoError(t, os.Remove(dbPath))
	})

	db, err := kvdb.Open(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
		false,
	)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db, nil
}

// ApplyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func ApplyMigration(t *testing.T,
	beforeMigration, afterMigration, migrationFunc func(tx kvdb.RwTx) error,
	shouldFail bool) {

	t.Helper()

	cdb, err := MakeDB(t)
	if err != nil {
		t.Fatal(err)
	}

	// beforeMigration usually used for populating the database
	// with test data.
	err = kvdb.Update(cdb, beforeMigration, func() {})
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		t.Helper()

		if r := recover(); r != nil {
			err = newError(r)
		}

		if err == nil && shouldFail {
			t.Fatal("error wasn't received on migration stage")
		} else if err != nil && !shouldFail {
			t.Fatalf("error was received on migration stage: %v", err)
		}

		// afterMigration usually used for checking the database state and
		// throwing the error if something went wrong.
		err = kvdb.Update(cdb, afterMigration, func() {})
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Apply migration.
	err = kvdb.Update(cdb, migrationFunc, func() {})
	if err != nil {
		t.Logf("migration error: %v", err)
	}
}

// ApplyMigrationWithDB is a helper test function that encapsulates the general
// steps which are needed to properly check the result of applying migration
// function. This function differs from ApplyMigration as it requires the
// supplied migration functions to take a db instance and construct their own
// database transactions.
func ApplyMigrationWithDB(t testing.TB, beforeMigration,
	afterMigration func(db kvdb.Backend) error,
	migrationFunc func(db kvdb.Backend) error, shouldFail bool) {

	t.Helper()

	cdb, err := MakeDB(t)
	if err != nil {
		t.Fatal(err)
	}

	// beforeMigration usually used for populating the database
	// with test data.
	if err := beforeMigration(cdb); err != nil {
		t.Fatalf("beforeMigration error: %v", err)
	}

	// Apply migration.
	err = migrationFunc(cdb)
	if shouldFail {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}

	// If there's no afterMigration, exit here.
	if afterMigration == nil {
		return
	}

	// afterMigration usually used for checking the database state
	// and throwing the error if something went wrong.
	if err := afterMigration(cdb); err != nil {
		t.Fatalf("afterMigration error: %v", err)
	}
}

func newError(e interface{}) error {
	var err error
	switch e := e.(type) {
	case error:
		err = e
	default:
		err = fmt.Errorf("%v", e)
	}

	return err
}
