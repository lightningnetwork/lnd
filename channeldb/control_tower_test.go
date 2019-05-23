package channeldb

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"testing"

	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/lnwire"
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

func genHtlc() (*lnwire.UpdateAddHTLC, error) {
	preimage, err := genPreimage()
	if err != nil {
		return nil, fmt.Errorf("unable to generate preimage: %v", err)
	}

	rhash := fastsha256.Sum256(preimage[:])
	htlc := &lnwire.UpdateAddHTLC{
		PaymentHash: rhash,
		Amount:      1,
	}

	return htlc, nil
}

type paymentControlTestCase func(*testing.T, bool)

var paymentControlTests = []struct {
	name     string
	strict   bool
	testcase paymentControlTestCase
}{
	{
		name:     "fail-strict",
		strict:   true,
		testcase: testPaymentControlSwitchFail,
	},
	{
		name:     "double-send-strict",
		strict:   true,
		testcase: testPaymentControlSwitchDoubleSend,
	},
	{
		name:     "double-pay-strict",
		strict:   true,
		testcase: testPaymentControlSwitchDoublePay,
	},
	{
		name:     "fail-not-strict",
		strict:   false,
		testcase: testPaymentControlSwitchFail,
	},
	{
		name:     "double-send-not-strict",
		strict:   false,
		testcase: testPaymentControlSwitchDoubleSend,
	},
	{
		name:     "double-pay-not-strict",
		strict:   false,
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
			test.testcase(t, test.strict)
		})
	}
}

// testPaymentControlSwitchFail checks that payment status returns to Grounded
// status after failing, and that ClearForTakeoff allows another HTLC for the
// same payment hash.
func testPaymentControlSwitchFail(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.ClearForTakeoff(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusInFlight)

	// Fail the payment, which should moved it to Grounded.
	if err := pControl.Fail(htlc.PaymentHash); err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Grounded.
	assertPaymentStatus(t, db, htlc.PaymentHash, StatusGrounded)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	if err := pControl.ClearForTakeoff(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusInFlight)

	// Verifies that status was changed to StatusCompleted.
	if err := pControl.Success(htlc.PaymentHash); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusCompleted)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	if err := pControl.ClearForTakeoff(htlc); err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// testPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func testPaymentControlSwitchDoubleSend(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	if err := pControl.ClearForTakeoff(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusInFlight)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	if err := pControl.ClearForTakeoff(htlc); err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}
}

// TestPaymentControlSwitchDoublePay checks the ability of payment control to
// prevent double payment.
func testPaymentControlSwitchDoublePay(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.ClearForTakeoff(htlc); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Verify that payment is InFlight.
	assertPaymentStatus(t, db, htlc.PaymentHash, StatusInFlight)

	// Move payment to completed status, second payment should return error.
	if err := pControl.Success(htlc.PaymentHash); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	// Verify that payment is Completed.
	assertPaymentStatus(t, db, htlc.PaymentHash, StatusCompleted)

	if err := pControl.ClearForTakeoff(htlc); err != ErrAlreadyPaid {
		t.Fatalf("payment control wrong behaviour:" +
			" double payment must trigger ErrAlreadyPaid")
	}
}

// TestPaymentControlNonStrictSuccessesWithoutInFlight checks that a non-strict
// payment control will allow calls to Success when no payment is in flight. This
// is necessary to gracefully handle the case in which the switch already sent
// out a payment for a particular payment hash in a prior db version that didn't
// have payment statuses.
func TestPaymentControlNonStrictSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(false, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	if err := pControl.Success(htlc.PaymentHash); err != nil {
		t.Fatalf("unable to mark payment hash success: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusCompleted)

	err = pControl.Success(htlc.PaymentHash)
	if err != ErrPaymentAlreadyCompleted {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}
}

// TestPaymentControlNonStrictFailsWithoutInFlight checks that a non-strict
// payment control will allow calls to Fail when no payment is in flight. This
// is necessary to gracefully handle the case in which the switch already sent
// out a payment for a particular payment hash in a prior db version that didn't
// have payment statuses.
func TestPaymentControlNonStrictFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(false, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	if err := pControl.Fail(htlc.PaymentHash); err != nil {
		t.Fatalf("unable to mark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusGrounded)

	err = pControl.Fail(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusGrounded)

	err = pControl.Success(htlc.PaymentHash)
	if err != nil {
		t.Fatalf("unable to remark payment hash success: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusCompleted)

	err = pControl.Fail(htlc.PaymentHash)
	if err != ErrPaymentAlreadyCompleted {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusCompleted)
}

// TestPaymentControlStrictSuccessesWithoutInFlight checks that a strict payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlStrictSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(true, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Success(htlc.PaymentHash)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusGrounded)
}

// TestPaymentControlStrictFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlStrictFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(true, db)

	htlc, err := genHtlc()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Fail(htlc.PaymentHash)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, htlc.PaymentHash, StatusGrounded)
}

func assertPaymentStatus(t *testing.T, db *DB,
	hash [32]byte, expStatus PaymentStatus) {

	t.Helper()

	pStatus, err := db.FetchPaymentStatus(hash)
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if pStatus != expStatus {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			expStatus, pStatus)
	}
}
