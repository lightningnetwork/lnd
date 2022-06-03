//go:build kvdb_sqlite
// +build kvdb_sqlite

package sqlite

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

// TestInterface performs all interfaces tests for this database driver.
func TestInterface(t *testing.T) {
	fixture, err := NewSqliteTestFixture("test")
	require.NoError(t, err)
	defer fixture.Cleanup()

	// dbType is the database type name for this driver.
	const dbType = "sqlite"

	ctx := context.Background()

	walletdbtest.TestInterface(t, dbType, ctx, fixture.Config)
}
