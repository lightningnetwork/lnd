//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

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
		name       string
		driverName string
		readOnly   bool
		rrWrites   bool
		expected   sql.IsolationLevel
	}{
		{
			name:       "postgres read-only",
			driverName: "pgx",
			readOnly:   true,
			expected:   sql.LevelRepeatableRead,
		},
		{
			name:       "postgres read-write",
			driverName: "pgx",
			readOnly:   false,
			expected:   sql.LevelSerializable,
		},
		{
			name:       "postgres read-only, rr writes",
			driverName: "pgx",
			readOnly:   true,
			rrWrites:   true,
			expected:   sql.LevelRepeatableRead,
		},
		{
			name:       "postgres read-write, rr writes",
			driverName: "pgx",
			readOnly:   false,
			rrWrites:   true,
			expected:   sql.LevelRepeatableRead,
		},
		{
			name:       "sqlite read-only",
			driverName: "sqlite",
			readOnly:   true,
			expected:   sql.LevelSerializable,
		},
		{
			name:       "sqlite read-write",
			driverName: "sqlite",
			readOnly:   false,
			expected:   sql.LevelSerializable,
		},
		{
			name:       "sqlite read-write, rr writes",
			driverName: "sqlite",
			readOnly:   false,
			rrWrites:   true,
			expected:   sql.LevelSerializable,
		},

		// Anything we don't positively recognize as Postgres must fall
		// back to the strictest level. In particular "postgres" is not
		// the driver name we register, so it must not opt in here,
		// with or without the write isolation knob.
		{
			name:       "unset driver read-only",
			driverName: "",
			readOnly:   true,
			expected:   sql.LevelSerializable,
		},
		{
			name:       "unknown driver read-only",
			driverName: "postgres",
			readOnly:   true,
			expected:   sql.LevelSerializable,
		},
		{
			name:       "unknown driver read-write, rr writes",
			driverName: "postgres",
			readOnly:   false,
			rrWrites:   true,
			expected:   sql.LevelSerializable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			db := &db{
				cfg: &Config{
					DriverName:            test.driverName,
					WriteTxRepeatableRead: test.rrWrites,
				},
			}

			require.Equal(
				t, test.expected,
				txIsolationLevel(db, test.readOnly),
			)
		})
	}
}
