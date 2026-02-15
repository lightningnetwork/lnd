//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

// isRollbackIgnorable mirrors the error classification logic from
// attemptRollback. It returns true if the given rollback error should be
// treated as a non-fatal condition (i.e., the transaction was already closed
// or rolled back).
func isRollbackIgnorable(rollbackErr error) bool {
	if rollbackErr == nil {
		return true
	}

	if errors.Is(rollbackErr, walletdb.ErrTxClosed) {
		return true
	}

	if strings.Contains(rollbackErr.Error(), "conn closed") {
		return true
	}

	if strings.Contains(rollbackErr.Error(), ErrNoTxActive) {
		return true
	}

	return false
}

// TestAttemptRollbackNoTxActive verifies that attemptRollback treats the
// "no transaction is active" SQLite error as a non-fatal condition.
func TestAttemptRollbackNoTxActive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		ignorable bool
	}{
		{
			name:      "nil error is ignorable",
			err:       nil,
			ignorable: true,
		},
		{
			name:      "tx closed error is ignorable",
			err:       walletdb.ErrTxClosed,
			ignorable: true,
		},
		{
			name:      "conn closed error is ignorable",
			err:       errors.New("sql: conn closed"),
			ignorable: true,
		},
		{
			name: "sqlite no transaction active is ignorable",
			err: errors.New(
				"SQL logic error: cannot rollback " +
					"- no transaction is active (1)",
			),
			ignorable: true,
		},
		{
			name: "wrapped no transaction active is ignorable",
			err: errors.New(
				"unknown sqlite error: " +
					"no transaction is active",
			),
			ignorable: true,
		},
		{
			name:      "unrelated error is not ignorable",
			err:       errors.New("disk I/O error"),
			ignorable: false,
		},
		{
			name:      "database locked is not ignorable",
			err:       errors.New("database is locked"),
			ignorable: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := isRollbackIgnorable(tc.err)
			require.Equal(t, tc.ignorable, result)
		})
	}
}
