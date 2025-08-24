//go:build kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqlite

import (
	"testing"
	"time"

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

	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
	})

	walletdbtest.TestInterface(t, dbType, ctx, cfg, dir, "tmp.db", "temp")
}
