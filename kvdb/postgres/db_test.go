//go:build kvdb_postgres

package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

// TestInterface performs all interfaces tests for this database driver.
func TestInterface(t *testing.T) {
	stop, err := StartEmbeddedPostgres()
	require.NoError(t, err)
	defer stop()

	f := NewTestPgFixture(t, "")

	// dbType is the database type name for this driver.
	const dbType = "postgres"

	ctx := context.Background()
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

	f := NewTestPgFixture(t, "")
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

	hasErr := err != nil &&
		strings.Contains(err.Error(), "unexpected EOF") ||
		strings.Contains(err.Error(), "terminating connection")

	require.True(t, hasErr)
}
