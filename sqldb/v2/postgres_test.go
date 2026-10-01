//go:build test_db_postgres && !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)) && !(netbsd || openbsd)

package sqldb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPostgresMigrateDriverReleasesConns asserts that the schema version
// methods release their connection.
func TestPostgresMigrateDriverReleasesConns(t *testing.T) {
	t.Parallel()

	store := NewTestDB(t, nil)

	serverConns := func() int {
		var n int
		err := store.DB.QueryRowContext(
			t.Context(), "SELECT count(*) FROM pg_stat_activity "+
				"WHERE datname = current_database()",
		).Scan(&n)
		require.NoError(t, err)

		return n
	}
	before := serverConns()

	version, dirty, err := store.GetSchemaVersion()
	require.NoError(t, err)
	require.NoError(t, store.SetSchemaVersion(version, dirty))

	require.Zero(t, store.DB.Stats().InUse)

	// The server ends a backend some time after its client closes it.
	require.Eventually(t, func() bool {
		return serverConns() <= before
	}, 10*time.Second, 100*time.Millisecond)
}
