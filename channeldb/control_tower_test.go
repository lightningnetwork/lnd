package channeldb

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
)

func initDB() (*DB, error) {
	tempPath, err := ioutil.TempDir("", "switchdb")
	if err != nil {
		return nil, err
	}

	db, err := Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, err
}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}

func genInfo() (*PaymentCreationInfo, *PaymentAttemptInfo,
	lntypes.Preimage, error) {

	preimage, err := genPreimage()
	if err != nil {
		return nil, nil, preimage, fmt.Errorf("unable to "+
			"generate preimage: %v", err)
	}

	rhash := fastsha256.Sum256(preimage[:])
	return &PaymentCreationInfo{
			PaymentHash:    rhash,
			Value:          1,
			CreationDate:   time.Unix(time.Now().Unix(), 0),
			PaymentRequest: []byte("hola"),
		},
		&PaymentAttemptInfo{
			PaymentID:  1,
			SessionKey: priv,
			Route:      testRoute,
		}, preimage, nil
}

type paymentControlTestCase func(*testing.T)

var paymentControlTests = []struct {
	name     string
	testcase paymentControlTestCase
}{
	{
		name:     "fail",
		testcase: testPaymentControlSwitchFail,
	},
	{
		name:     "double-send",
		testcase: testPaymentControlSwitchDoubleSend,
	},
	{
		name:     "double-pay",
		testcase: testPaymentControlSwitchDoublePay,
	},
}

// TestPaymentControls runs a set of common tests against both the strict and
// non-strict payment control instances. This ensures that the two both behave
// identically when making the expected state-transitions of the stricter
// implementation. Behavioral differences in the strict and non-strict
// implementations are tested separately.
func TestPaymentControls(t *testing.T) {
	for _, test := range paymentControlTests {
		t.Run(test.name, func(t *testing.T) {
			test.testcase(t)
		})
	}
}

// testPaymentControlSwitchFail checks that payment status returns to Grounded
// status after failing, and that ClearForTakeoff allows another HTLC for the
// same payment hash.
func testPaymentControlSwitchFail(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Fail the payment, which should moved it to Grounded.
	if err := pControl.Fail(info.PaymentHash); err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, db, info.PaymentHash, StatusFailed)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Verifies that status was changed to StatusCompleted.
	if err := pControl.Success(info.PaymentHash, preimg); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// testPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func testPaymentControlSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}
}

// TestPaymentControlSwitchDoublePay checks the ability of payment control to
// prevent double payment.
func testPaymentControlSwitchDoublePay(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Verify that payment is InFlight.
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Move payment to completed status, second payment should return error.
	err = pControl.Success(info.PaymentHash, preimg)
	if err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	// Verify that payment is Completed.
	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("payment control wrong behaviour:" +
			" double payment must trigger ErrAlreadyPaid")
	}
}

// TestPaymentControlStrictSuccessesWithoutInFlight checks that a strict payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlStrictSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Success(info.PaymentHash, preimg)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)
}

// TestPaymentControlStrictFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlStrictFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Fail(info.PaymentHash)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)
}

func assertPaymentStatus(t *testing.T, db *DB,
	hash [32]byte, expStatus PaymentStatus) {

	t.Helper()

	var paymentStatus = StatusGrounded
	err := db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentsRootBucket)
		if payments == nil {
			return nil
		}

		bucket := payments.Bucket(hash[:])
		if bucket == nil {
			return nil
		}

		// Get the existing status of this payment, if any.
		paymentStatus = fetchPaymentStatus(bucket)
		return nil
	})
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if paymentStatus != expStatus {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			expStatus, paymentStatus)
	}
}
