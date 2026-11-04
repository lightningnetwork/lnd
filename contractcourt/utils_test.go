package contractcourt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/lnwallet"
)

func init() {
	lnwallet.SetTestChannelDBRegistrar(registerTestChannelDB)
}

func testChannelStore(t testing.TB,
	state *chanstate.OpenChannel) chainArbitratorChannelStore {

	t.Helper()

	return testChannelDB(t, state).ChannelStateStore()
}

var testChannelDBs sync.Map

func registerTestChannelDB(state *chanstate.OpenChannel, db *channeldb.DB) {
	testChannelDBs.Store(state, db)
}

func testChannelDB(t testing.TB, state *chanstate.OpenChannel) *channeldb.DB {
	t.Helper()

	db, ok := testChannelDBs.Load(state)
	if !ok {
		t.Fatalf("expected DB for test channel state %p", state)
	}

	cdb, ok := db.(*channeldb.DB)
	if !ok {
		t.Fatalf("expected channeldb.DB, got %T", db)
	}

	return cdb
}

// timeout implements a test level timeout.
func timeout() func() {
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
// creating a new struct from it. The copied state is returned.
func copyChannelState(t *testing.T, state *chanstate.OpenChannel) (
	*chanstate.OpenChannel, error) {

	// Make a copy of the DB.
	dbFile := filepath.Join(
		testChannelDB(t, state).Path(), "channel.db",
	)
	tempDbPath := t.TempDir()

	tempDbFile := filepath.Join(tempDbPath, "channel.db")
	err := copyFile(tempDbFile, dbFile)
	if err != nil {
		return nil, err
	}

	newDB := channeldb.OpenForTesting(t, tempDbPath)

	chans, err := newDB.ChannelStateDB().FetchAllChannels()
	if err != nil {
		return nil, err
	}

	// We only support DBs with a single channel, for now.
	if len(chans) != 1 {
		return nil, fmt.Errorf("found %d chans in the db",
			len(chans))
	}

	registerTestChannelDB(chans[0], newDB)

	return chans[0], nil
}
