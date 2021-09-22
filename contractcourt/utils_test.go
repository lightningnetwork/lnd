package contractcourt

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
)

// timeout implements a test level timeout.
func timeout(t *testing.T) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(5 * time.Second):
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

			panic("test timeout")
		case <-done:
		}
	}()

	return func() {
		close(done)
	}
}

func copyFile(dest, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}

	return d.Close()
}

// copyChannelState copies the OpenChannel state by copying the database and
// creating a new struct from it. The copied state and a cleanup function are
// returned.
func copyChannelState(state *channeldb.OpenChannel) (
	*channeldb.OpenChannel, func(), error) {

	// Make a copy of the DB.
	dbFile := filepath.Join(state.Db.GetParentDB().Path(), "channel.db")
	tempDbPath, err := ioutil.TempDir("", "past-state")
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		os.RemoveAll(tempDbPath)
	}

	tempDbFile := filepath.Join(tempDbPath, "channel.db")
	err = copyFile(tempDbFile, dbFile)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	newDb, err := channeldb.Open(tempDbPath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	chans, err := newDb.ChannelStateDB().FetchAllChannels()
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	// We only support DBs with a single channel, for now.
	if len(chans) != 1 {
		cleanup()
		return nil, nil, fmt.Errorf("found %d chans in the db",
			len(chans))
	}

	return chans[0], cleanup, nil
}
