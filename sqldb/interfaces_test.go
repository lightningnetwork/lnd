package sqldb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTxIsolationLevel tests that the isolation level of a transaction is only
// relaxed for read-only transactions on Postgres, and for read-write
// transactions on Postgres once the opt-in knob is set. Every other combination
// must remain fully serializable.
func TestTxIsolationLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		backend  BackendType
		readOnly bool
		rrWrites bool
		expected sql.IsolationLevel
	}{
		{
			name:     "postgres read-only",
			backend:  BackendTypePostgres,
			readOnly: true,
			expected: sql.LevelRepeatableRead,
		},
		{
			name:     "postgres read-write",
			backend:  BackendTypePostgres,
			readOnly: false,
			expected: sql.LevelSerializable,
		},
		{
			name:     "postgres read-only, rr writes",
			backend:  BackendTypePostgres,
			readOnly: true,
			rrWrites: true,
			expected: sql.LevelRepeatableRead,
		},
		{
			name:     "postgres read-write, rr writes",
			backend:  BackendTypePostgres,
			readOnly: false,
			rrWrites: true,
			expected: sql.LevelRepeatableRead,
		},
		{
			name:     "sqlite read-only",
			backend:  BackendTypeSqlite,
			readOnly: true,
			expected: sql.LevelSerializable,
		},
		{
			name:     "sqlite read-write",
			backend:  BackendTypeSqlite,
			readOnly: false,
			expected: sql.LevelSerializable,
		},
		{
			name:     "sqlite read-write, rr writes",
			backend:  BackendTypeSqlite,
			readOnly: false,
			rrWrites: true,
			expected: sql.LevelSerializable,
		},
		{
			name:     "unknown read-only",
			backend:  BackendTypeUnknown,
			readOnly: true,
			expected: sql.LevelSerializable,
		},
		{
			name:     "unknown read-write, rr writes",
			backend:  BackendTypeUnknown,
			readOnly: false,
			rrWrites: true,
			expected: sql.LevelSerializable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.expected, txIsolationLevel(
					test.backend, test.readOnly,
					test.rrWrites,
				),
			)
		})
	}
}

// TestBeginTxIsolationLevel asserts that the isolation level that the database
// itself reports for a transaction opened through BeginTx matches what we
// expect. On Postgres, read-only transactions run at repeatable read while
// read-write transactions remain serializable unless the opt-in knob moves
// them to repeatable read as well. We also assert the read-only flag that
// Postgres reports, so that dropping it from the tx options would be caught
// here as well.
func TestBeginTxIsolationLevel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	db := NewTestDB(t).GetBaseDB()

	// The knob defaults to off, which is what the first two cases below
	// assert.
	require.False(t, db.WriteTxRepeatableRead)

	tests := []struct {
		name         string
		opts         TxOptions
		rrWrites     bool
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
		{
			name:         "read-only, rr writes",
			opts:         ReadTxOpt(),
			rrWrites:     true,
			expected:     "repeatable read",
			expectedFlag: "on",
		},
		{
			name:         "read-write, rr writes",
			opts:         WriteTxOpt(),
			rrWrites:     true,
			expected:     "repeatable read",
			expectedFlag: "off",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Nothing else reads this field while a transaction is
			// being opened, and these sub tests are not run in
			// parallel, so it's safe to flip the knob in place
			// rather than to bring up a second database.
			db.WriteTxRepeatableRead = test.rrWrites

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

			if isSQLite {
				require.Equal(t, BackendTypeSqlite, db.Backend())
				return
			}

			require.Equal(t, BackendTypePostgres, db.Backend())

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
