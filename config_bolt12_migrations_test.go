//go:build test_db_postgres || test_db_sqlite || test_native_sql

package lnd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBolt12MigrationsExist verifies that every migration that the BOLT 12
// startup check names exists in a build with the development migrations. A
// renumbered migration would otherwise make lnd refuse the flag in every
// build.
func TestBolt12MigrationsExist(t *testing.T) {
	t.Parallel()

	name, missing := missingMigration(bolt12Migrations)
	require.False(t, missing, "migration %s does not exist", name)
}
