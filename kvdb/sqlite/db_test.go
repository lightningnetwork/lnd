//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/stretchr/testify/require"
)

// TestInterface performs all interfaces tests for this database driver.
func TestInterface(t *testing.T) {
	// dbType is the database type name for this driver.
	dir := t.TempDir()
	ctx := t.Context()

	sqlbase.Init(0)

	cfg := &Config{
		BusyTimeout: time.Second * 5,
	}

	sqlDB, err := NewSqliteBackend(ctx, cfg, dir, "tmp.db", "table")
	require.NoError(t, err)

	// The regular SQLite backend must not expose migration-only
	// capabilities. Migration backends must be explicitly selected.
	_, ok := sqlDB.(sqlbase.MigrationBulkKVStore)
	require.False(t, ok)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
	})

	walletdbtest.TestInterface(t, dbType, ctx, cfg, dir, "tmp.db", "temp")
}

// TestForAllPreservesEmptyValues verifies that ForAll distinguishes the SQL
// NULL used for nested buckets from empty leaf values.
func TestForAllPreservesEmptyValues(t *testing.T) {
	db := newTestDB(t, t.Context())

	err := db.Update(func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte("top"))
		if err != nil {
			return err
		}

		if _, err := bucket.CreateBucket([]byte("bucket")); err != nil {
			return err
		}

		return bucket.Put([]byte("empty"), []byte{})
	}, func() {})
	require.NoError(t, err)

	err = db.View(func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte("top"))
		forAllBucket, ok := bucket.(interface {
			ForAll(func(k, v []byte) error) error
		})
		require.True(t, ok)

		return forAllBucket.ForAll(func(key, value []byte) error {
			switch string(key) {
			case "bucket":
				require.Nil(t, value)

			case "empty":
				require.NotNil(t, value)
				require.Empty(t, value)
			}

			return nil
		})
	}, func() {})
	require.NoError(t, err)
}

// TestForAllReturnsIterationError verifies that a failure encountered after a
// query starts is returned instead of being mistaken for normal exhaustion.
func TestForAllReturnsIterationError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	db := newTestDB(t, ctx)

	err := db.Update(func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket([]byte("top"))
		if err != nil {
			return err
		}

		for _, key := range []string{"one", "two", "three"} {
			err := bucket.Put([]byte(key), []byte("value"))
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	require.NoError(t, err)

	var count int
	err = db.View(func(tx walletdb.ReadTx) error {
		bucket := tx.ReadBucket([]byte("top"))
		forAllBucket, ok := bucket.(interface {
			ForAll(func(k, v []byte) error) error
		})
		require.True(t, ok)

		return forAllBucket.ForAll(func(_, _ []byte) error {
			count++
			cancel()
			time.Sleep(10 * time.Millisecond)

			return nil
		})
	}, func() {
		count = 0
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, count, 3)
}

func newTestDB(t *testing.T, ctx context.Context) walletdb.DB {
	t.Helper()

	sqlbase.Init(0)
	db, err := NewSqliteBackend(ctx, &Config{
		BusyTimeout: 5 * time.Second,
	}, t.TempDir(), "test.db", "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}
