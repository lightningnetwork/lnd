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
