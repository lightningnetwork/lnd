package sqldb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWaitForPostgresReadySkipsWhenDisabled verifies that WaitForPostgresReady
// returns immediately when StartupMaxRetries is set to zero.
func TestWaitForPostgresReadySkipsWhenDisabled(t *testing.T) {
	t.Parallel()

	cfg := &PostgresConfig{
		Dsn:               "postgres://localhost:1/testdb",
		StartupMaxRetries: 0,
		StartupRetryDelay: 1 * time.Second,
	}

	err := WaitForPostgresReady(context.Background(), cfg)
	require.NoError(t, err)
}

// TestWaitForPostgresReadyExhaustsRetries verifies that WaitForPostgresReady
// returns an error after exhausting all retry attempts against an unreachable
// endpoint.
func TestWaitForPostgresReadyExhaustsRetries(t *testing.T) {
	t.Parallel()

	cfg := &PostgresConfig{
		Dsn:               "postgres://localhost:1/testdb",
		StartupMaxRetries: 3,
		StartupRetryDelay: 10 * time.Millisecond,
	}

	err := WaitForPostgresReady(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to connect to postgres")
	require.Contains(t, err.Error(), "3 attempts")
}

// TestWaitForPostgresReadyContextCancel verifies that WaitForPostgresReady
// respects context cancellation and stops retrying early.
func TestWaitForPostgresReadyContextCancel(t *testing.T) {
	t.Parallel()

	cfg := &PostgresConfig{
		Dsn:               "postgres://localhost:1/testdb",
		StartupMaxRetries: 100,
		StartupRetryDelay: 1 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context after a short delay.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := WaitForPostgresReady(ctx, cfg)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "context canceled")

	// Ensure we didn't wait for all 100 retries.
	require.Less(t, elapsed, 10*time.Second)
}
