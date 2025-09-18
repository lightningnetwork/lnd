//go:build test_db_postgres || test_db_sqlite

package paymentsdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
)

// assertNoIndex is a no-op for the SQL implementation.
func assertNoIndex(t *testing.T, p DB, seqNr uint64) {
	return
}

// assertPaymentIndex is a no-op for the SQL implementation.
func assertPaymentIndex(t *testing.T, p DB, expectedHash lntypes.Hash) {
	return
}
