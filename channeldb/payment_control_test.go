package channeldb

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
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

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	_, err = pControl.Fail(info.PaymentHash, failReason)
	if err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, db, info.PaymentHash, StatusFailed)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		&failReason,
	)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentHash, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
		nil,
	)

	// Record a new attempt.
	attempt.PaymentID = 2
	err = pControl.RegisterAttempt(info.PaymentHash, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, attempt, lntypes.Preimage{},
		nil,
	)

	// Verifies that status was changed to StatusSucceeded.
	var route *route.Route
	route, err = pControl.Success(info.PaymentHash, preimg)
	if err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}
	if !reflect.DeepEqual(*route, attempt.Route) {
		t.Fatalf("unexpected route returned")
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, db, info.PaymentHash, info, attempt, preimg, nil)

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

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, nil, lntypes.Preimage{},
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
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
	assertPaymentInfo(
		t, db, info.PaymentHash, info, attempt, lntypes.Preimage{},
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
	assertPaymentStatus(t, db, info.PaymentHash, StatusSucceeded)
	assertPaymentInfo(t, db, info.PaymentHash, info, attempt, preimg, nil)

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

	assertPaymentStatus(t, db, info.PaymentHash, StatusUnknown)
	assertPaymentInfo(
		t, db, info.PaymentHash, nil, nil, lntypes.Preimage{},
		nil,
	)
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

	assertPaymentStatus(t, db, info.PaymentHash, StatusUnknown)
	assertPaymentInfo(
		t, db, info.PaymentHash, nil, nil, lntypes.Preimage{}, nil,
	)
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
			assertPaymentStatus(t, db, info.PaymentHash, StatusFailed)
			assertPaymentInfo(
				t, db, info.PaymentHash, info, attempt,
				lntypes.Preimage{}, &failReason,
			)
		} else if p.success {
			// Verifies that status was changed to StatusSucceeded.
			_, err := pControl.Success(info.PaymentHash, preimg)
			if err != nil {
				t.Fatalf("error shouldn't have been received, got: %v", err)
			}

			assertPaymentStatus(t, db, info.PaymentHash, StatusSucceeded)
			assertPaymentInfo(
				t, db, info.PaymentHash, info, attempt, preimg, nil,
			)
		} else {
			assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)
			assertPaymentInfo(
				t, db, info.PaymentHash, info, attempt,
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

func assertPaymentStatus(t *testing.T, db *DB,
	hash [32]byte, expStatus PaymentStatus) {

	t.Helper()

	var paymentStatus = StatusUnknown
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

func checkPaymentCreationInfo(bucket *bbolt.Bucket, c *PaymentCreationInfo) error {
	b := bucket.Get(paymentCreationInfoKey)
	switch {
	case b == nil && c == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected creation info not found")
	case c == nil:
		return fmt.Errorf("unexpected creation info found")
	}

	r := bytes.NewReader(b)
	c2, err := deserializePaymentCreationInfo(r)
	if err != nil {
		fmt.Println("creation info err: ", err)
		return err
	}
	if !reflect.DeepEqual(c, c2) {
		return fmt.Errorf("PaymentCreationInfos don't match: %v vs %v",
			spew.Sdump(c), spew.Sdump(c2))
	}

	return nil
}

func checkPaymentAttemptInfo(bucket *bbolt.Bucket, a *PaymentAttemptInfo) error {
	b := bucket.Get(paymentAttemptInfoKey)
	switch {
	case b == nil && a == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected attempt info not found")
	case a == nil:
		return fmt.Errorf("unexpected attempt info found")
	}

	r := bytes.NewReader(b)
	a2, err := deserializePaymentAttemptInfo(r)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, a2) {
		return fmt.Errorf("PaymentAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(a2))
	}

	return nil
}

func checkSettleInfo(bucket *bbolt.Bucket, preimg lntypes.Preimage) error {
	zero := lntypes.Preimage{}
	b := bucket.Get(paymentSettleInfoKey)
	switch {
	case b == nil && preimg == zero:
		return nil
	case b == nil:
		return fmt.Errorf("expected preimage not found")
	case preimg == zero:
		return fmt.Errorf("unexpected preimage found")
	}

	var pre2 lntypes.Preimage
	copy(pre2[:], b[:])
	if preimg != pre2 {
		return fmt.Errorf("Preimages don't match: %x vs %x",
			preimg, pre2)
	}

	return nil
}

func checkFailInfo(bucket *bbolt.Bucket, failReason *FailureReason) error {
	b := bucket.Get(paymentFailInfoKey)
	switch {
	case b == nil && failReason == nil:
		return nil
	case b == nil:
		return fmt.Errorf("expected fail info not found")
	case failReason == nil:
		return fmt.Errorf("unexpected fail info found")
	}

	failReason2 := FailureReason(b[0])
	if *failReason != failReason2 {
		return fmt.Errorf("Failure infos don't match: %v vs %v",
			*failReason, failReason2)
	}

	return nil
}

func assertPaymentInfo(t *testing.T, db *DB, hash lntypes.Hash,
	c *PaymentCreationInfo, a *PaymentAttemptInfo, s lntypes.Preimage,
	f *FailureReason) {

	t.Helper()

	err := db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(paymentsRootBucket)
		if payments == nil && c == nil {
			return nil
		}
		if payments == nil {
			return fmt.Errorf("sent payments not found")
		}

		bucket := payments.Bucket(hash[:])
		if bucket == nil && c == nil {
			return nil
		}

		if bucket == nil {
			return fmt.Errorf("payment not found")
		}

		if err := checkPaymentCreationInfo(bucket, c); err != nil {
			return err
		}

		if err := checkPaymentAttemptInfo(bucket, a); err != nil {
			return err
		}

		if err := checkSettleInfo(bucket, s); err != nil {
			return err
		}

		if err := checkFailInfo(bucket, f); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assert payment info failed: %v", err)
	}

}
