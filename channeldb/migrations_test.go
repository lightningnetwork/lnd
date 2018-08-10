package channeldb

import (
	"crypto/sha256"
	"testing"
)

// TestPaymentStatusesMigration checks that already completed payments will have
// their payment statuses set to Completed after the migration.
func TestPaymentStatusesMigration(t *testing.T) {
	t.Parallel()

	fakePayment := makeFakePayment()
	paymentHash := sha256.Sum256(fakePayment.PaymentPreimage[:])

	// Add fake payment to test database, verifying that it was created,
	// that we have only one payment, and its status is not "Completed".
	beforeMigrationFunc := func(d *DB) {
		if err := d.AddPayment(fakePayment); err != nil {
			t.Fatalf("unable to add payment: %v", err)
		}

		payments, err := d.FetchAllPayments()
		if err != nil {
			t.Fatalf("unable to fetch payments: %v", err)
		}

		if len(payments) != 1 {
			t.Fatalf("wrong qty of paymets: expected 1, got %v",
				len(payments))
		}

		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		// We should receive default status if we have any in database.
		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}
	}

	// Verify that the created payment status is "Completed" for our one
	// fake payment.
	afterMigrationFunc := func(d *DB) {
		meta, err := d.FetchMeta(nil)
		if err != nil {
			t.Fatal(err)
		}

		if meta.DbVersionNumber != 1 {
			t.Fatal("migration 'paymentStatusesMigration' wasn't applied")
		}

		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusCompleted {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusCompleted.String(), paymentStatus.String())
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		paymentStatusesMigration,
		false)
}
