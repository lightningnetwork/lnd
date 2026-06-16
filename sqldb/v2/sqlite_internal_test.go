//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64))

package sqldb

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/require"
)

// TestSqliteProgrammaticMigrationError verifies that SQLite migration setup
// failures are attributed to the SQLite backend.
func TestSqliteProgrammaticMigrationError(t *testing.T) {
	t.Parallel()

	store, err := NewSqliteStore(
		&SqliteConfig{}, filepath.Join(t.TempDir(), "test.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	boom := errors.New("boom")
	err = store.ExecuteMigrations(MigrationSet{
		TrackingTableName:      "migration_tracker",
		LatestMigrationVersion: 1,
		Descriptors: []MigrationDescriptor{{
			Name:    "programmatic",
			Version: 1,
		}},
		MakeProgrammaticMigrations: func(*BaseDB) (
			map[uint]migrate.ProgrammaticMigrEntry, error) {

			return nil, boom
		},
	})
	require.ErrorContains(t, err, "sqlite")
	require.NotContains(t, err.Error(), "postgres")
}
