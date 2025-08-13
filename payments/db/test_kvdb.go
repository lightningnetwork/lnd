package paymentsdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// NewTestDB is a helper function that creates an BBolt database for testing.
func NewTestDB(t *testing.T, opts ...OptionModifier) *KVPaymentsDB {
	backend, backendCleanup, err := kvdb.GetTestBackend(
		t.TempDir(), "kvPaymentDB",
	)
	require.NoError(t, err)

	t.Cleanup(backendCleanup)

	paymentDB, err := NewKVPaymentsDB(backend, opts...)
	require.NoError(t, err)

	return paymentDB
}
