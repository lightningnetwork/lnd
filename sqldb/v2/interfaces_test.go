package sqldb

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// testQuerier is a minimal query wrapper used to instantiate the generic
// transaction executor in tests.
type testQuerier struct {
}

// testBatchedQuerier is a minimal BatchedQuerier implementation used to verify
// that TransactionExecutor forwards backend identity.
type testBatchedQuerier struct {
	backend BackendType
}

// BeginTx is a stub implementation used to satisfy the BatchedQuerier
// interface in tests.
func (t testBatchedQuerier) BeginTx(context.Context,
	TxOptions) (*sql.Tx, error) {

	return nil, nil
}

// Backend returns the backend type used by the test batched querier.
func (t testBatchedQuerier) Backend() BackendType {
	return t.backend
}

// TestTransactionExecutorBackend verifies that the executor forwards the
// backend type from its batched querier.
func TestTransactionExecutorBackend(t *testing.T) {
	t.Parallel()

	executor := NewTransactionExecutor[testQuerier](
		testBatchedQuerier{backend: BackendTypePostgres},
		func(*sql.Tx) testQuerier {
			return testQuerier{}
		},
	)

	require.Equal(t, BackendTypePostgres, executor.Backend())
}

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
