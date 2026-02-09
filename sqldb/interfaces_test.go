package sqldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errBodyFailed is a sentinel error used in tests to simulate a transaction
// body failure.
var errBodyFailed = errors.New("body failed: SQLITE_IOERR")

// errRollbackFailed is a sentinel error used in tests to simulate a rollback
// failure.
var errRollbackFailed = errors.New("rollback failed: no transaction is active")

// errCommitFailed is a sentinel error used in tests to simulate a commit
// failure.
var errCommitFailed = errors.New("commit failed: database is locked")

// mockTx implements the Tx interface for testing.
type mockTx struct {
	commitErr   error
	rollbackErr error
}

func (m *mockTx) Commit() error {
	return m.commitErr
}

func (m *mockTx) Rollback() error {
	return m.rollbackErr
}

// TestExecuteSQLTransactionPreservesBodyError verifies that when a transaction
// body fails and the subsequent rollback also fails, the original body error is
// returned (not the rollback error).
func TestExecuteSQLTransactionPreservesBodyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bodyErr     error
		rollbackErr error
		wantErr     error
	}{
		{
			name:        "body error preserved when rollback fails",
			bodyErr:     errBodyFailed,
			rollbackErr: errRollbackFailed,
			wantErr:     errBodyFailed,
		},
		{
			name:        "body error returned when rollback succeeds",
			bodyErr:     errBodyFailed,
			rollbackErr: nil,
			wantErr:     errBodyFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := &mockTx{
				rollbackErr: tc.rollbackErr,
			}

			makeTx := func() (Tx, error) {
				return tx, nil
			}

			rollbackTx := func(tx Tx) error {
				return tx.Rollback()
			}

			txBody := func(tx Tx) error {
				return tc.bodyErr
			}

			onBackoff := func(retry int, delay time.Duration) {
			}

			err := ExecuteSQLTransactionWithRetry(
				context.Background(), makeTx,
				rollbackTx, txBody, onBackoff, 1,
			)

			require.Error(t, err)

			// The original body error should be present, not
			// the rollback error.
			require.ErrorContains(t, err, tc.bodyErr.Error())
		})
	}
}

// TestExecuteSQLTransactionPreservesCommitError verifies that when a commit
// fails and the subsequent rollback also fails, the original commit error is
// returned (not the rollback error).
func TestExecuteSQLTransactionPreservesCommitError(t *testing.T) {
	t.Parallel()

	commitCallCount := 0
	tx := &mockTx{
		commitErr:   errCommitFailed,
		rollbackErr: errRollbackFailed,
	}

	makeTx := func() (Tx, error) {
		return tx, nil
	}

	rollbackTx := func(tx Tx) error {
		return tx.Rollback()
	}

	txBody := func(tx Tx) error {
		commitCallCount++
		return nil
	}

	onBackoff := func(retry int, delay time.Duration) {}

	err := ExecuteSQLTransactionWithRetry(
		context.Background(), makeTx, rollbackTx, txBody,
		onBackoff, 1,
	)

	require.Error(t, err)

	// The original commit error should be present, not the rollback
	// error.
	require.ErrorContains(t, err, errCommitFailed.Error())
}
