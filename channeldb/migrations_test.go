package channeldb

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/coreos/bbolt"
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

		// Lastly, we'll add a locally-sourced circuit and
		// non-locally-sourced circuit to the circuit map. The
		// locally-sourced payment should end up with an InFlight
		// status, while the other should remain unchanged, which
		// defaults to Grounded.
		err = d.Update(func(tx *bolt.Tx) error {
			circuits, err := tx.CreateBucketIfNotExists(
				[]byte("circuit-adds"),
			)
			if err != nil {
				return err
			}

			groundedKey := make([]byte, 16)
			binary.BigEndian.PutUint64(groundedKey[:8], 1)
			binary.BigEndian.PutUint64(groundedKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is the case for locally-sourced
			// payments. No payment status should end up being set
			// for this circuit, since the short channel id of the
			// key is non-zero (e.g., a forwarded circuit). This
			// will default it to Grounded.
			groundedCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			err = circuits.Put(groundedKey, groundedCircuit)
			if err != nil {
				return err
			}

			inFlightKey := make([]byte, 16)
			binary.BigEndian.PutUint64(inFlightKey[:8], 0)
			binary.BigEndian.PutUint64(inFlightKey[8:], 1)

			// Generated using TestHalfCircuitSerialization with nil
			// ErrorEncrypter, which is not the case for forwarded
			// payments, but should have no impact on the
			// correctness of the test. The payment status for this
			// circuit should be set to InFlight, since the short
			// channel id in the key is 0 (sourceHop).
			inFlightCircuit := []byte{
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x01,
				// start payment hash
				0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				// end payment hash
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x0f,
				0x42, 0x40, 0x00,
			}

			return circuits.Put(inFlightKey, inFlightCircuit)
		})
		if err != nil {
			t.Fatalf("unable to add circuit map entry: %v", err)
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

		// Check that our completed payments were migrated.
		paymentStatus, err := d.FetchPaymentStatus(paymentHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusCompleted {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusCompleted.String(), paymentStatus.String())
		}

		inFlightHash := [32]byte{
			0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that the locally sourced payment was transitioned to
		// InFlight.
		paymentStatus, err = d.FetchPaymentStatus(inFlightHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusInFlight {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusInFlight.String(), paymentStatus.String())
		}

		groundedHash := [32]byte{
			0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		}

		// Check that non-locally sourced payments remain in the default
		// Grounded state.
		paymentStatus, err = d.FetchPaymentStatus(groundedHash)
		if err != nil {
			t.Fatalf("unable to fetch payment status: %v", err)
		}

		if paymentStatus != StatusGrounded {
			t.Fatalf("wrong payment status: expected %v, got %v",
				StatusGrounded.String(), paymentStatus.String())
		}
	}

	applyMigration(t,
		beforeMigrationFunc,
		afterMigrationFunc,
		paymentStatusesMigration,
		false)
}
