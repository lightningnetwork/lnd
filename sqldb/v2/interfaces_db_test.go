//go:build !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)) && !(netbsd || openbsd)

package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBeginTxIsolationLevel asserts that the isolation level that the database
// itself reports for a transaction opened through BeginTx matches what we
// expect. On Postgres, read-only transactions run at repeatable read while
// read-write transactions remain serializable. We also assert the read-only
// flag that Postgres reports, so that dropping it from the tx options would be
// caught here as well.
func TestBeginTxIsolationLevel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db := NewTestDB(t, nil).GetBaseDB()

	tests := []struct {
		name         string
		opts         TxOptions
		expected     string
		expectedFlag string
	}{
		{
			name:         "read-only",
			opts:         ReadTxOpt(),
			expected:     "repeatable read",
			expectedFlag: "on",
		},
		{
			name:         "read-write",
			opts:         WriteTxOpt(),
			expected:     "serializable",
			expectedFlag: "off",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, test.opts)

			// SQLite has no notion of a transaction isolation
			// level and its driver ignores the one we request
			// outright. All we can assert there is that opening
			// the transaction still succeeds, which is what would
			// break if the driver ever started rejecting the
			// levels we ask for.
			require.NoError(t, err)
			defer func() {
				require.NoError(t, tx.Rollback())
			}()

			if db.Backend() != BackendTypePostgres {
				require.Equal(t, BackendTypeSqlite, db.Backend())
				return
			}

			var level string
			row := tx.QueryRowContext(
				ctx, "SHOW transaction_isolation",
			)
			require.NoError(t, row.Scan(&level))
			require.Equal(t, test.expected, level)

			var readOnly string
			row = tx.QueryRowContext(
				ctx, "SHOW transaction_read_only",
			)
			require.NoError(t, row.Scan(&readOnly))
			require.Equal(t, test.expectedFlag, readOnly)
		})
	}
}
