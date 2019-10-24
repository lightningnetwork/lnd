package migration_01_to_11

import (
	"github.com/coreos/bbolt"
)

// fetchPaymentStatus fetches the payment status of the payment. If the payment
// isn't found, it will default to "StatusUnknown".
func fetchPaymentStatus(bucket *bbolt.Bucket) PaymentStatus {
	if bucket.Get(paymentSettleInfoKey) != nil {
		return StatusSucceeded
	}

	if bucket.Get(paymentFailInfoKey) != nil {
		return StatusFailed
	}

	if bucket.Get(paymentCreationInfoKey) != nil {
		return StatusInFlight
	}

	return StatusUnknown
}
