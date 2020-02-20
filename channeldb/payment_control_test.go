package channeldb

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
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

func genInfo() (*PaymentCreationInfo, *HTLCAttemptInfo,
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
			CreationTime:   time.Unix(time.Now().Unix(), 0),
			PaymentRequest: []byte("hola"),
		},
		&HTLCAttemptInfo{
			AttemptID:  1,
			SessionKey: priv,
			Route:      testRoute,
		}, preimage, nil
}

// TestPaymentControlSwitchFail checks that payment status returns to Failed
// status after failing, and that InitPayment allows another HTLC for the
// same payment hash.
func TestPaymentControlSwitchFail(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	_, err = pControl.Fail(info.PaymentHash, failReason)
	if err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, pControl, info.PaymentHash, StatusFailed)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, nil, lntypes.Preimage{},
		&failReason,
	)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Record a new attempt.
	attempt.AttemptID = 2
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, attempt, lntypes.Preimage{},
		nil,
	)

	// Verifies that status was changed to StatusSucceeded.
	var payment *MPPayment
	payment, err = pControl.Success(info.PaymentHash, preimg)
	if err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	if len(payment.HTLCs) != 1 {
		t.Fatalf("payment should have one htlc, got: %d",
			len(payment.HTLCs))
	}

	err = assertRouteEqual(&payment.HTLCs[0].Route, &attempt.Route)
	if err != nil {
		t.Fatalf("unexpected route returned: %v vs %v: %v",
			spew.Sdump(attempt.Route),
			spew.Sdump(payment.HTLCs[0].Route), err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, pControl, info.PaymentHash, info, attempt, preimg, nil)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func TestPaymentControlSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// Record an attempt.
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentHash, info, attempt, lntypes.Preimage{},
		nil,
	)

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// After settling, the error should be ErrAlreadyPaid.
	if _, err := pControl.Success(info.PaymentHash, preimg); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, pControl, info.PaymentHash, info, attempt, preimg, nil)

	err = pControl.InitPayment(info.PaymentHash, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSuccessesWithoutInFlight checks that the payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlSuccessesWithoutInFlight(t *testing.T) {
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

	// Attempt to complete the payment should fail.
	_, err = pControl.Success(info.PaymentHash, preimg)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusUnknown)
}

// TestPaymentControlFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlFailsWithoutInFlight(t *testing.T) {
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

	// Calling Fail should return an error.
	_, err = pControl.Fail(info.PaymentHash, FailureReasonNoRoute)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentHash, StatusUnknown)
}

// TestPaymentControlDeleteNonInFlight checks that calling DeletaPayments only
// deletes payments from the database that are not in-flight.
func TestPaymentControlDeleteNonInFligt(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	payments := []struct {
		failed  bool
		success bool
	}{
		{
			failed:  true,
			success: false,
		},
		{
			failed:  false,
			success: true,
		},
		{
			failed:  false,
			success: false,
		},
	}

	for _, p := range payments {
		info, attempt, preimg, err := genInfo()
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		// Sends base htlc message which initiate StatusInFlight.
		err = pControl.InitPayment(info.PaymentHash, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}
		err = pControl.RegisterAttempt(info.PaymentHash, attempt)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		if p.failed {
			// Fail the payment, which should moved it to Failed.
			failReason := FailureReasonNoRoute
			_, err = pControl.Fail(info.PaymentHash, failReason)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

			// Verify the status is indeed Failed.
			assertPaymentStatus(t, pControl, info.PaymentHash, StatusFailed)
			assertPaymentInfo(
				t, pControl, info.PaymentHash, info, attempt,
				lntypes.Preimage{}, &failReason,
			)
		} else if p.success {
			// Verifies that status was changed to StatusSucceeded.
			_, err := pControl.Success(info.PaymentHash, preimg)
			if err != nil {
				t.Fatalf("error shouldn't have been received, got: %v", err)
			}

			assertPaymentStatus(t, pControl, info.PaymentHash, StatusSucceeded)
			assertPaymentInfo(
				t, pControl, info.PaymentHash, info, attempt, preimg, nil,
			)
		} else {
			assertPaymentStatus(t, pControl, info.PaymentHash, StatusInFlight)
			assertPaymentInfo(
				t, pControl, info.PaymentHash, info, attempt,
				lntypes.Preimage{}, nil,
			)
		}
	}

	// Delete payments.
	if err := db.DeletePayments(); err != nil {
		t.Fatal(err)
	}

	// This should leave the in-flight payment.
	dbPayments, err := db.FetchPayments()
	if err != nil {
		t.Fatal(err)
	}

	if len(dbPayments) != 1 {
		t.Fatalf("expected one payment, got %d", len(dbPayments))
	}

	status := dbPayments[0].Status
	if status != StatusInFlight {
		t.Fatalf("expected in-fligth status, got %v", status)
	}
}

// assertPaymentStatus retrieves the status of the payment referred to by hash
// and compares it with the expected state.
func assertPaymentStatus(t *testing.T, p *PaymentControl,
	hash lntypes.Hash, expStatus PaymentStatus) {

	t.Helper()

	payment, err := p.FetchPayment(hash)
	if expStatus == StatusUnknown && err == ErrPaymentNotInitiated {
		return
	}
	if err != nil {
		t.Fatal(err)
	}

	if payment.Status != expStatus {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			expStatus, payment.Status)
	}
}

// assertPaymentInfo retrieves the payment referred to by hash and verifies the
// expected values.
func assertPaymentInfo(t *testing.T, p *PaymentControl, hash lntypes.Hash,
	c *PaymentCreationInfo, a *HTLCAttemptInfo, s lntypes.Preimage,
	f *FailureReason) {

	t.Helper()

	payment, err := p.FetchPayment(hash)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(payment.Info, c) {
		t.Fatalf("PaymentCreationInfos don't match: %v vs %v",
			spew.Sdump(payment.Info), spew.Sdump(c))
	}

	if f != nil {
		if *payment.FailureReason != *f {
			t.Fatal("unexpected failure reason")
		}
	} else {
		if payment.FailureReason != nil {
			t.Fatal("unexpected failure reason")
		}
	}

	if a == nil {
		if len(payment.HTLCs) > 0 {
			t.Fatal("expected no htlcs")
		}
		return
	}

	htlc := payment.HTLCs[0]
	if err := assertRouteEqual(&htlc.Route, &a.Route); err != nil {
		t.Fatal("routes do not match")
	}

	var zeroPreimage = lntypes.Preimage{}
	if s != zeroPreimage {
		if htlc.Settle.Preimage != s {
			t.Fatalf("Preimages don't match: %x vs %x",
				htlc.Settle.Preimage, s)
		}
	} else {
		if htlc.Settle != nil {
			t.Fatal("expected no settle info")
		}
	}
}
