package paymentsdb

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// paymentsBucketTombstone is the key used to mark the payments bucket
	// as permanently closed after a successful migration.
	paymentsBucketTombstone = []byte("payments-tombstone")
)

// SetPaymentsBucketTombstone sets the tombstone key in the payments bucket to
// mark the bucket as permanently closed. This prevents it from being reopened
// in the future.
func SetPaymentsBucketTombstone(db kvdb.Backend) error {
	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		// Access the top-level payments bucket.
		payments := tx.ReadWriteBucket(paymentsRootBucket)
		if payments == nil {
			var err error
			payments, err = tx.CreateTopLevelBucket(
				paymentsRootBucket,
			)
			if err != nil {
				return fmt.Errorf("payments bucket does not "+
					"exist: %w", err)
			}
		}

		// Add the tombstone key to the payments bucket.
		err := payments.Put(paymentsBucketTombstone, []byte("1"))
		if err != nil {
			return fmt.Errorf("failed to set tombstone: %w", err)
		}

		return nil
	}, func() {})
}

// GetPaymentsBucketTombstone checks if the tombstone key exists in the payments
// bucket. It returns true if the tombstone is present and false otherwise.
func GetPaymentsBucketTombstone(db kvdb.Backend) (bool, error) {
	var tombstoneExists bool

	err := kvdb.View(db, func(tx kvdb.RTx) error {
		// Access the top-level payments bucket.
		payments := tx.ReadBucket(paymentsRootBucket)
		if payments == nil {
			tombstoneExists = false
			return nil
		}

		// Check if the tombstone key exists.
		tombstone := payments.Get(paymentsBucketTombstone)
		tombstoneExists = tombstone != nil

		return nil
	}, func() {})
	if err != nil {
		return false, err
	}

	return tombstoneExists, nil
}
