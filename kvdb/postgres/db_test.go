//go:build kvdb_postgres

package postgres

import (
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/stretchr/testify/require"
)

// TestInterface performs all interfaces tests for this database driver.
func TestInterface(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer stop()

	f, err := NewFixture("")
	require.NoError(t, err)

	// dbType is the database type name for this driver.
	const dbType = "postgres"

	ctx := t.Context()
	cfg := &Config{
		Dsn: f.Dsn,
	}

	walletdbtest.TestInterface(t, dbType, ctx, cfg, prefix)
}

// TestPanic tests recovery from panic conditions.
func TestPanic(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer stop()

	f, err := NewFixture("")
	require.NoError(t, err)

	err = f.Db.Update(func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte("test"))
		require.NoError(t, err)

		// Stop database server.
		stop()

		// Keep trying to get data until Get panics because the
		// connection is lost.
		for i := 0; i < 50; i++ {
			bucket.Get([]byte("key"))
			time.Sleep(100 * time.Millisecond)
		}

		return nil
	}, func() {})

	require.Contains(t, err.Error(), "terminating connection")
}
