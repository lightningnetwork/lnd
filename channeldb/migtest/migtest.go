package migtest

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
)

// MakeDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func MakeDB() (kvdb.Backend, func(), error) {
	// Create temporary database for mission control.
	file, err := ioutil.TempFile("", "*.db")
	if err != nil {
		return nil, nil, err
	}

	dbPath := file.Name()
	db, err := kvdb.Open(
		kvdb.BoltBackendName, dbPath, true, kvdb.DefaultDBTimeout,
	)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		db.Close()
		os.RemoveAll(dbPath)
	}

	return db, cleanUp, nil
}

// ApplyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func ApplyMigration(t *testing.T,
	beforeMigration, afterMigration, migrationFunc func(tx kvdb.RwTx) error,
	shouldFail bool) {

	t.Helper()

	cdb, cleanUp, err := MakeDB()
	defer cleanUp()
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
