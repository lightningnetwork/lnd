//go:build !test_db_sqlite && !test_db_postgres

package paymentsdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates an BBolt database for testing.
func NewTestDB(t *testing.T, opts ...OptionModifier) PaymentDB {
	backend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "kvPaymentDB",
	)
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	paymentDB, err := NewKVStore(backend, opts...)
	require.NoError(t, err)

	return paymentDB
}
