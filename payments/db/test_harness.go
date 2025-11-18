package paymentsdb

import (
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
)

// TestHarness provides implementation-specific test utilities for the payments
// database. Different database backends (KV, SQL) have different internal
// structures and indexing mechanisms, so this interface allows tests to verify
// implementation-specific behavior without coupling the test logic to a
// particular backend.
type TestHarness interface {
	// AssertPaymentIndex checks that a payment is correctly indexed.
	// For KV: verifies the payment index bucket entry exists and points
	// to the correct payment hash.
	// For SQL: no-op (SQL doesn't use a separate index bucket).
	AssertPaymentIndex(t *testing.T, expectedHash lntypes.Hash)

	// AssertNoIndex checks that an index for a sequence number doesn't
	// exist.
	// For KV: verifies the index bucket entry is deleted.
	// For SQL: no-op.
	AssertNoIndex(t *testing.T, seqNr uint64)
}
