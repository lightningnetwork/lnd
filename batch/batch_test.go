package batch

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

func TestRetry(t *testing.T) {
	dbDir := t.TempDir()

	dbName := filepath.Join(dbDir, "weks.db")
	db, err := walletdb.Create(
		"bdb", dbName, true, kvdb.DefaultDBTimeout,
	)
	if err != nil {
		t.Fatalf("unable to create walletdb: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})

	var (
		mu     sync.Mutex
		called int
	)
	sched := NewTimeScheduler(db, &mu, time.Second)

	// First, we construct a request that should retry individually and
	// execute it non-lazily. It should still return the error the second
	// time.
	req := &Request{
		Update: func(tx kvdb.RwTx) error {
			called++

			return errors.New("test")
		},
	}
	err = sched.Execute(req)

	// Check and reset the called counter.
	mu.Lock()
	require.Equal(t, 2, called)
	called = 0
	mu.Unlock()

	require.ErrorContains(t, err, "test")

	// Now, we construct a request that should NOT retry because it returns
	// a serialization error, which should cause the underlying postgres
	// transaction to retry. Since we aren't using postgres, this will
	// cause the transaction to not be retried at all.
	req = &Request{
		Update: func(tx kvdb.RwTx) error {
			called++

			return errors.New("could not serialize access")
		},
	}
	err = sched.Execute(req)

	// Check the called counter.
	mu.Lock()
	require.Equal(t, 1, called)
	mu.Unlock()

	require.ErrorContains(t, err, "could not serialize access")
}
