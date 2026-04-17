package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewSqliteStoreRejectsInvalidQueryConfig(t *testing.T) {
	t.Parallel()

	_, err := NewSqliteStore(&SqliteConfig{
		QueryConfig: QueryConfig{
			MaxBatchSize: 0,
			MaxPageSize:  defaultSQLitePageSize,
		},
	}, t.TempDir()+"/invalid.db")
	require.ErrorContains(t, err, "invalid query config")
}

func TestNewPostgresStoreRejectsInvalidQueryConfig(t *testing.T) {
	t.Parallel()

	_, err := NewPostgresStore(&PostgresConfig{
		Dsn: "postgres://localhost/testdb",
		QueryConfig: QueryConfig{
			MaxBatchSize: 0,
			MaxPageSize:  defaultPostgresPageSize,
		},
	})
	require.ErrorContains(t, err, "invalid query config")
}
