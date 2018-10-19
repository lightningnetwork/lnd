package channeldb

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
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

// TestWipe tests that the database wipe operation completes successfully
// and that the buckets are deleted. It also checks that attempts to fetch
// information while the buckets are not set return the correct errors.
func TestWipe(t *testing.T) {
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
	defer cdb.Close()

	if err := cdb.Wipe(); err != nil {
		t.Fatalf("unable to wipe channeldb: %v", err)
	}
	// Check correct errors are returned
	_, err = cdb.FetchAllOpenChannels()
	if err != ErrNoActiveChannels {
		t.Fatalf("fetching open channels: expected '%v' instead got '%v'",
			ErrNoActiveChannels, err)
	}
	_, err = cdb.FetchClosedChannels(false)
	if err != ErrNoClosedChannels {
		t.Fatalf("fetching closed channels: expected '%v' instead got '%v'",
			ErrNoClosedChannels, err)
	}
}
