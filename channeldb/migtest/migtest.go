package migtest

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
)

// MakeDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func MakeDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

// ApplyMigration is a helper test function that encapsulates the general steps
// which are needed to properly check the result of applying migration function.
func ApplyMigration(t *testing.T,
	beforeMigration, afterMigration, migrationFunc func(tx *bbolt.Tx) error,
	shouldFail bool) {

	cdb, cleanUp, err := MakeDB()
	defer cleanUp()
	if err != nil {
		t.Fatal(err)
	}

	// beforeMigration usually used for populating the database
	// with test data.
	err = cdb.Update(beforeMigration)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
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
		err = cdb.Update(afterMigration)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// Apply migration.
	err = cdb.Update(migrationFunc)
	if err != nil {
		t.Fatal(err)
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
