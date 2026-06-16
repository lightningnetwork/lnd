//go:build test_db_postgres || test_db_sqlite || test_native_sql

package sqldb

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigrationFilesAllRegistered verifies that every .up.sql file in the
// embedded migrations filesystem has a corresponding entry in migrationConfig.
// This test requires dev build tags so that any future dev-only migrations
// added to migrationAdditions are visible — without them, such entries would
// be absent and their SQL files would trigger false failures.
func TestMigrationFilesAllRegistered(t *testing.T) {
	t.Parallel()

	migrations := GetMigrations()
	require.NotEmpty(t, migrations)

	// Collect all schema versions referenced by any entry in migrationConfig
	// (including migrationAdditions, which is only populated under dev build
	// tags).
	registeredSchemaVersions := make(map[int]string)
	for _, m := range migrations {
		registeredSchemaVersions[m.SchemaVersion] = m.Name
	}

	// Read all .up.sql files from the embedded filesystem.
	embeddedFiles, err := sqlSchemas.ReadDir("sqlc/migrations")
	require.NoError(t, err)

	for _, f := range embeddedFiles {
		if f.IsDir() {
			continue
		}

		var schemaVersion int
		_, err := fmt.Sscanf(f.Name(), "%06d_", &schemaVersion)
		require.NoError(t, err, "migration file %q has no valid "+
			"numeric prefix", f.Name())

		_, referenced := registeredSchemaVersions[schemaVersion]
		require.True(t, referenced,
			"SQL migration file %q (schema version %d) has no "+
				"corresponding entry in migrationConfig — add "+
				"an entry with SchemaVersion=%d",
			f.Name(), schemaVersion, schemaVersion)
	}
}
