package sqldb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockTx is a minimal Tx implementation used to drive the transaction retry
// loop without an actual database behind it.
type mockTx struct{}

// Commit always succeeds.
func (m *mockTx) Commit() error {
	return nil
}

// Rollback always succeeds.
func (m *mockTx) Rollback() error {
	return nil
}

// retryHarness bundles the callbacks the retry loop needs, along with a counter
// for the number of attempts that were made.
type retryHarness struct {
	attempts int

	// bodyErr is returned by the transaction body on every attempt. If it
	// is nil, the transaction is considered successful.
	bodyErr error
}

// run executes the retry loop with the harness' callbacks.
func (h *retryHarness) run(ctx context.Context,
	retryCfg RetryConfig) error {

	makeTx := func() (Tx, error) {
		return &mockTx{}, nil
	}

	txBody := func(Tx) error {
		h.attempts++

		return h.bodyErr
	}

	rollbackTx := func(Tx) error {
		return nil
	}

	onBackoff := func(int, time.Duration) {}

	return ExecuteSQLTransactionWithRetryConfig(
		ctx, makeTx, rollbackTx, txBody, onBackoff, retryCfg,
	)
}

// TestRetryConfigExhausted tests the two independent budgets that bound the
// transaction retry loop.
func TestRetryConfigExhausted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      RetryConfig
		attempts int
		elapsed  time.Duration
		exp      bool
	}{
		{
			name:     "zero value falls back to default count",
			cfg:      RetryConfig{},
			attempts: DefaultNumTxRetries - 1,
			exp:      false,
		},
		{
			name:     "zero value is bounded by default count",
			cfg:      RetryConfig{},
			attempts: DefaultNumTxRetries,
			exp:      true,
		},
		{
			name:     "count budget not yet used up",
			cfg:      RetryConfig{MaxRetries: 3},
			attempts: 2,
			exp:      false,
		},
		{
			name:     "count budget used up",
			cfg:      RetryConfig{MaxRetries: 3},
			attempts: 3,
			exp:      true,
		},
		{
			name:     "time budget ignores the attempt count",
			cfg:      RetryConfig{MaxElapsed: time.Minute},
			attempts: 10_000,
			elapsed:  time.Second,
			exp:      false,
		},
		{
			name:     "time budget used up",
			cfg:      RetryConfig{MaxElapsed: time.Minute},
			attempts: 1,
			elapsed:  time.Minute,
			exp:      true,
		},
		{
			name: "either budget stops the loop",
			cfg: RetryConfig{
				MaxRetries: 3,
				MaxElapsed: time.Minute,
			},
			attempts: 3,
			exp:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.exp, test.cfg.exhausted(
				test.attempts, test.elapsed,
			))
		})
	}
}

// TestExecuteSQLTransactionWithRetrySuccess tests that a transaction that
// commits on the first attempt isn't retried.
func TestExecuteSQLTransactionWithRetrySuccess(t *testing.T) {
	t.Parallel()

	h := &retryHarness{}
	err := h.run(t.Context(), NumRetriesConfig(5))
	require.NoError(t, err)
	require.Equal(t, 1, h.attempts)
}

// TestExecuteSQLTransactionWithRetryNonRetryable tests that an error that isn't
// a serialization error is returned as-is, without any retries.
func TestExecuteSQLTransactionWithRetryNonRetryable(t *testing.T) {
	t.Parallel()

	bodyErr := errors.New("not a serialization error")
	h := &retryHarness{bodyErr: bodyErr}

	err := h.run(t.Context(), NumRetriesConfig(5))
	require.ErrorIs(t, err, bodyErr)
	require.Equal(t, 1, h.attempts)
	require.False(t, IsInternalDBError(err))
}

// TestExecuteSQLTransactionWithRetryExceeded tests that once the attempt budget
// is used up, we return a labeled error that still carries the underlying
// database error along for diagnostics.
func TestExecuteSQLTransactionWithRetryExceeded(t *testing.T) {
	t.Parallel()

	h := &retryHarness{bodyErr: serializationErr()}

	err := h.run(t.Context(), NumRetriesConfig(3))
	require.Equal(t, 3, h.attempts)

	// The error must be recognizable both as an exhausted retry loop and as
	// the serialization error that caused it.
	require.ErrorIs(t, err, ErrRetriesExceeded)
	require.True(t, IsSerializationError(err))
	require.True(t, IsInternalDBError(err))

	// The raw postgres text alone is not a useful error message, so we make
	// sure our own label is part of it as well.
	require.Contains(t, err.Error(), ErrRetriesExceeded.Error())
	require.Contains(t, err.Error(), "SQLSTATE 40001")
}

// TestExecuteSQLTransactionWithRetryCanceled tests that a retry loop that is
// interrupted by a canceled context returns a labeled error instead of the bare
// database error.
func TestExecuteSQLTransactionWithRetryCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	h := &retryHarness{bodyErr: serializationErr()}

	// The attempt budget is generous here, we expect the canceled context
	// to stop the loop after the very first backoff.
	err := h.run(ctx, NumRetriesConfig(1000))
	require.Equal(t, 1, h.attempts)

	require.ErrorIs(t, err, ErrRetryCanceled)
	require.True(t, IsSerializationError(err))
	require.True(t, IsInternalDBError(err))

	// The bare serialization error used to leak out of here unlabeled,
	// which made it look like a protocol failure to the callers.
	require.Contains(t, err.Error(), ErrRetryCanceled.Error())
	require.Contains(t, err.Error(), "SQLSTATE 40001")
}

// TestExecuteSQLTransactionWithRetryBudget tests that a purely time based retry
// budget keeps retrying past any fixed attempt count, and still terminates.
func TestExecuteSQLTransactionWithRetryBudget(t *testing.T) {
	t.Parallel()

	const budget = 500 * time.Millisecond

	h := &retryHarness{bodyErr: serializationErr()}

	start := time.Now()
	err := h.run(t.Context(), RetryConfig{MaxElapsed: budget})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrRetriesExceeded)
	require.True(t, IsInternalDBError(err))

	// We should have spent at least the budget retrying, and we should not
	// have overshot it by more than a single capped backoff.
	require.GreaterOrEqual(t, elapsed, budget)
	require.Less(t, elapsed, budget+2*DefaultMaxRetryDelay)

	// More than one attempt must have been made, otherwise the budget
	// wasn't actually driving the loop.
	require.Greater(t, h.attempts, 1)
}

// TestExecuteSQLTransactionWithRetryQuit tests that closing the quit channel
// aborts the retry loop immediately, even though the retry budget is nowhere
// near used up. This is what keeps a generous time budget from holding up
// shutdown.
func TestExecuteSQLTransactionWithRetryQuit(t *testing.T) {
	t.Parallel()

	quit := make(chan struct{})
	close(quit)

	h := &retryHarness{bodyErr: serializationErr()}

	start := time.Now()
	err := h.run(t.Context(), RetryConfig{
		MaxElapsed: time.Hour,
		Quit:       quit,
	})
	elapsed := time.Since(start)

	// Exactly one attempt is made, and we don't wait out a single backoff.
	require.Equal(t, 1, h.attempts)
	require.Less(t, elapsed, time.Second)

	require.ErrorIs(t, err, ErrRetryCanceled)
	require.True(t, IsInternalDBError(err))
}

// TestExecuteSQLTransactionWithRetryCompat tests that the backwards compatible
// entry point still bounds the loop by the passed attempt count.
func TestExecuteSQLTransactionWithRetryCompat(t *testing.T) {
	t.Parallel()

	h := &retryHarness{bodyErr: serializationErr()}

	makeTx := func() (Tx, error) {
		return &mockTx{}, nil
	}
	txBody := func(Tx) error {
		h.attempts++

		return h.bodyErr
	}
	rollbackTx := func(Tx) error {
		return nil
	}

	err := ExecuteSQLTransactionWithRetry(
		t.Context(), makeTx, rollbackTx, txBody,
		func(int, time.Duration) {}, 2,
	)
	require.Equal(t, 2, h.attempts)
	require.ErrorIs(t, err, ErrRetriesExceeded)
}
