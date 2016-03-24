package channeldb

import (
	"io/ioutil"
	"os"
	"testing"
)

func TestOpenNotCreated(t *testing.T) {
	if _, err := Open("path doesn't exist"); err != ErrNoExists {
		t.Fatalf("channeldb Open should fail due to non-existant dir")
	}
}

func TestCreateThenOpen(t *testing.T) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v")
	}
	defer os.RemoveAll(tempDirName)

	// Next, create channeldb for the first time.
	cdb, err := Create(tempDirName)
	if err != nil {
		t.Fatalf("unable to create channeldb: %v", err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatalf("unable to close channeldb: %v", err)
	}

	// Open should now succeed as the cdb was created above.
	cdb, err = Open(tempDirName)
	if err != nil {
		t.Fatalf("unable to open channeldb: %v", err)
	}
}
