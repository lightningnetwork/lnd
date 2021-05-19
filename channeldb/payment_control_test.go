package channeldb

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	rhash := sha256.Sum256(preimage[:])
	attempt := NewHtlcAttemptInfo(
		0, priv, *testRoute.Copy(), time.Time{}, nil,
	)
	return &PaymentCreationInfo{
		PaymentIdentifier: rhash,
		Value:             testRoute.ReceiverAmt(),
		CreationTime:      time.Unix(time.Now().Unix(), 0),
		PaymentRequest:    []byte("hola"),
	}, attempt, preimage, nil
}

// TestPaymentControlSwitchFail checks that payment status returns to Failed
// status after failing, and that InitPayment allows another HTLC for the
// same payment hash.
func TestPaymentControlSwitchFail(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentIndex(t, pControl, info.PaymentIdentifier)
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	_, err = pControl.Fail(info.PaymentIdentifier, failReason)
	if err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusFailed)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, &failReason, nil,
	)

	// Lookup the payment so we can get its old sequence number before it is
	// overwritten.
	payment, err := pControl.FetchPayment(info.PaymentIdentifier)
	assert.NoError(t, err)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Check that our index has been updated, and the old index has been
	// removed.
	assertPaymentIndex(t, pControl, info.PaymentIdentifier)
	assertNoIndex(t, pControl, payment.SequenceNum)

	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, nil,
	)

	// Record a new attempt. In this test scenario, the attempt fails.
	// However, this is not communicated to control tower in the current
	// implementation. It only registers the initiation of the attempt.
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatalf("unable to register attempt: %v", err)
	}

	htlcReason := HTLCFailUnreadable
	_, err = pControl.FailAttempt(
		info.PaymentIdentifier, attempt.AttemptID,
		&HTLCFailInfo{
			Reason: htlcReason,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

	htlc := &htlcStatus{
		HTLCAttemptInfo: attempt,
		failure:         &htlcReason,
	}

	assertPaymentInfo(t, pControl, info.PaymentIdentifier, info, nil, htlc)

	// Record another attempt.
	attempt.AttemptID = 1
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

	htlc = &htlcStatus{
		HTLCAttemptInfo: attempt,
	}

	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, htlc,
	)

	// Settle the attempt and verify that status was changed to
	// StatusSucceeded.
	payment, err = pControl.SettleAttempt(
		info.PaymentIdentifier, attempt.AttemptID,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	if err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	if len(payment.HTLCs) != 2 {
		t.Fatalf("payment should have two htlcs, got: %d",
			len(payment.HTLCs))
	}

	err = assertRouteEqual(&payment.HTLCs[0].Route, &attempt.Route)
	if err != nil {
		t.Fatalf("unexpected route returned: %v vs %v: %v",
			spew.Sdump(attempt.Route),
			spew.Sdump(payment.HTLCs[0].Route), err)
	}

	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusSucceeded)

	htlc.settle = &preimg
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, htlc,
	)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func TestPaymentControlSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

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
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentIndex(t, pControl, info.PaymentIdentifier)
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, nil,
	)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// Record an attempt.
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

	htlc := &htlcStatus{
		HTLCAttemptInfo: attempt,
	}
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, htlc,
	)

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// After settling, the error should be ErrAlreadyPaid.
	_, err = pControl.SettleAttempt(
		info.PaymentIdentifier, attempt.AttemptID,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	if err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusSucceeded)

	htlc.settle = &preimg
	assertPaymentInfo(t, pControl, info.PaymentIdentifier, info, nil, htlc)

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestPaymentControlSuccessesWithoutInFlight checks that the payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Attempt to complete the payment should fail.
	_, err = pControl.SettleAttempt(
		info.PaymentIdentifier, 0,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusUnknown)
}

// TestPaymentControlFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, _, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Calling Fail should return an error.
	_, err = pControl.Fail(info.PaymentIdentifier, FailureReasonNoRoute)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusUnknown)
}

// TestPaymentControlDeleteNonInFlight checks that calling DeletePayments only
// deletes payments from the database that are not in-flight.
func TestPaymentControlDeleteNonInFligt(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	// Create a sequence number for duplicate payments that will not collide
	// with the sequence numbers for the payments we create. These values
	// start at 1, so 9999 is a safe bet for this test.
	var duplicateSeqNr = 9999

	pControl := NewPaymentControl(db)

	payments := []struct {
		failed       bool
		success      bool
		hasDuplicate bool
	}{
		{
			failed:       true,
			success:      false,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      true,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      false,
			hasDuplicate: false,
		},
		{
			failed:       false,
			success:      true,
			hasDuplicate: true,
		},
	}

	var numSuccess, numInflight int

	for _, p := range payments {
		info, attempt, preimg, err := genInfo()
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		// Sends base htlc message which initiate StatusInFlight.
		err = pControl.InitPayment(info.PaymentIdentifier, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}
		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		htlc := &htlcStatus{
			HTLCAttemptInfo: attempt,
		}

		if p.failed {
			// Fail the payment attempt.
			htlcFailure := HTLCFailUnreadable
			_, err := pControl.FailAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFailure,
				},
			)
			if err != nil {
				t.Fatalf("unable to fail htlc: %v", err)
			}

			// Fail the payment, which should moved it to Failed.
			failReason := FailureReasonNoRoute
			_, err = pControl.Fail(info.PaymentIdentifier, failReason)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

			// Verify the status is indeed Failed.
			assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusFailed)

			htlc.failure = &htlcFailure
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info,
				&failReason, htlc,
			)
		} else if p.success {
			// Verifies that status was changed to StatusSucceeded.
			_, err := pControl.SettleAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been received, got: %v", err)
			}

			assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusSucceeded)

			htlc.settle = &preimg
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info, nil, htlc,
			)

			numSuccess++
		} else {
			assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info, nil, htlc,
			)

			numInflight++
		}

		// If the payment is intended to have a duplicate payment, we
		// add one.
		if p.hasDuplicate {
			appendDuplicatePayment(
				t, pControl.db, info.PaymentIdentifier,
				uint64(duplicateSeqNr), preimg,
			)
			duplicateSeqNr++
			numSuccess++
		}
	}

	// Delete all failed payments.
	if err := db.DeletePayments(true, false); err != nil {
		t.Fatal(err)
	}

	// This should leave the succeeded and in-flight payments.
	dbPayments, err := db.FetchPayments()
	if err != nil {
		t.Fatal(err)
	}

	if len(dbPayments) != numSuccess+numInflight {
		t.Fatalf("expected %d payments, got %d",
			numSuccess+numInflight, len(dbPayments))
	}

	var s, i int
	for _, p := range dbPayments {
		fmt.Println("fetch payment has status", p.Status)
		switch p.Status {
		case StatusSucceeded:
			s++
		case StatusInFlight:
			i++
		}
	}

	if s != numSuccess {
		t.Fatalf("expected %d succeeded payments , got %d",
			numSuccess, s)
	}
	if i != numInflight {
		t.Fatalf("expected %d in-flight payments, got %d",
			numInflight, i)
	}

	// Now delete all payments except in-flight.
	if err := db.DeletePayments(false, false); err != nil {
		t.Fatal(err)
	}

	// This should leave the in-flight payment.
	dbPayments, err = db.FetchPayments()
	if err != nil {
		t.Fatal(err)
	}

	if len(dbPayments) != numInflight {
		t.Fatalf("expected %d payments, got %d", numInflight,
			len(dbPayments))
	}

	for _, p := range dbPayments {
		if p.Status != StatusInFlight {
			t.Fatalf("expected in-fligth status, got %v", p.Status)
		}
	}

	// Finally, check that we only have a single index left in the payment
	// index bucket.
	var indexCount int
	err = kvdb.View(db, func(tx walletdb.ReadTx) error {
		index := tx.ReadBucket(paymentsIndexBucket)

		return index.ForEach(func(k, v []byte) error {
			indexCount++
			return nil
		})
	}, func() { indexCount = 0 })
	require.NoError(t, err)

	require.Equal(t, 1, indexCount)
}

// TestPaymentControlDeletePayments tests that DeletePayments correcly deletes
// information about completed payments from the database.
func TestPaymentControlDeletePayments(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	// Register three payments:
	// 1. A payment with two failed attempts.
	// 2. A Payment with one failed and one settled attempt.
	// 3. A payment with one failed and one in-flight attempt.
	attemptID := uint64(0)
	for i := 0; i < 3; i++ {
		info, attempt, preimg, err := genInfo()
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		attempt.AttemptID = attemptID
		attemptID++

		// Init the payment.
		err = pControl.InitPayment(info.PaymentIdentifier, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		// Register and fail the first attempt for all three payments.
		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		htlcFailure := HTLCFailUnreadable
		_, err = pControl.FailAttempt(
			info.PaymentIdentifier, attempt.AttemptID,
			&HTLCFailInfo{
				Reason: htlcFailure,
			},
		)
		if err != nil {
			t.Fatalf("unable to fail htlc: %v", err)
		}

		// Depending on the test case, fail or succeed the next
		// attempt.
		attempt.AttemptID = attemptID
		attemptID++

		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		switch i {

		// Fail the attempt and the payment overall.
		case 0:
			htlcFailure := HTLCFailUnreadable
			_, err = pControl.FailAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFailure,
				},
			)
			if err != nil {
				t.Fatalf("unable to fail htlc: %v", err)
			}

			failReason := FailureReasonNoRoute
			_, err = pControl.Fail(info.PaymentIdentifier, failReason)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

		// Settle the attempt
		case 1:
			_, err := pControl.SettleAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been received, got: %v", err)
			}

		// We leave the attmpet in-flight by doing nothing.
		case 2:
		}
	}

	type fetchedPayment struct {
		status PaymentStatus
		htlcs  int
	}

	assertPayments := func(expPayments []fetchedPayment) {
		t.Helper()

		dbPayments, err := db.FetchPayments()
		if err != nil {
			t.Fatal(err)
		}

		if len(dbPayments) != len(expPayments) {
			t.Fatalf("expected %d payments, got %d",
				len(expPayments), len(dbPayments))
		}

		for i := range dbPayments {
			if dbPayments[i].Status != expPayments[i].status {
				t.Fatalf("unexpected payment status")
			}

			if len(dbPayments[i].HTLCs) != expPayments[i].htlcs {
				t.Fatalf("unexpected number of htlcs")
			}

		}
	}

	// Check that all payments are there as we added them.
	assertPayments([]fetchedPayment{
		{
			status: StatusFailed,
			htlcs:  2,
		},
		{
			status: StatusSucceeded,
			htlcs:  2,
		},
		{
			status: StatusInFlight,
			htlcs:  2,
		},
	})

	// Delete HTLC attempts for failed payments only.
	if err := db.DeletePayments(true, true); err != nil {
		t.Fatal(err)
	}

	// The failed payment is the only altered one.
	assertPayments([]fetchedPayment{
		{
			status: StatusFailed,
			htlcs:  0,
		},
		{
			status: StatusSucceeded,
			htlcs:  2,
		},
		{
			status: StatusInFlight,
			htlcs:  2,
		},
	})

	// Delete failed attempts for all payments.
	if err := db.DeletePayments(false, true); err != nil {
		t.Fatal(err)
	}

	// The failed attempts should be deleted, except for the in-flight
	// payment, that shouldn't be altered until it has completed.
	assertPayments([]fetchedPayment{
		{
			status: StatusFailed,
			htlcs:  0,
		},
		{
			status: StatusSucceeded,
			htlcs:  1,
		},
		{
			status: StatusInFlight,
			htlcs:  2,
		},
	})

	// Now delete all failed payments.
	if err := db.DeletePayments(true, false); err != nil {
		t.Fatal(err)
	}

	assertPayments([]fetchedPayment{
		{
			status: StatusSucceeded,
			htlcs:  1,
		},
		{
			status: StatusInFlight,
			htlcs:  2,
		},
	})

	// Finally delete all completed payments.
	if err := db.DeletePayments(false, false); err != nil {
		t.Fatal(err)
	}

	assertPayments([]fetchedPayment{
		{
			status: StatusInFlight,
			htlcs:  2,
		},
	})
}

// TestPaymentControlMultiShard checks the ability of payment control to
// have multiple in-flight HTLCs for a single payment.
func TestPaymentControlMultiShard(t *testing.T) {
	t.Parallel()

	// We will register three HTLC attempts, and always fail the second
	// one. We'll generate all combinations of settling/failing the first
	// and third HTLC, and assert that the payment status end up as we
	// expect.
	type testCase struct {
		settleFirst bool
		settleLast  bool
	}

	var tests []testCase
	for _, f := range []bool{true, false} {
		for _, l := range []bool{true, false} {
			tests = append(tests, testCase{f, l})
		}
	}

	runSubTest := func(t *testing.T, test testCase) {
		db, cleanup, err := MakeTestDB()
		defer cleanup()

		if err != nil {
			t.Fatalf("unable to init db: %v", err)
		}

		pControl := NewPaymentControl(db)

		info, attempt, preimg, err := genInfo()
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		// Init the payment, moving it to the StatusInFlight state.
		err = pControl.InitPayment(info.PaymentIdentifier, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		assertPaymentIndex(t, pControl, info.PaymentIdentifier)
		assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
		assertPaymentInfo(
			t, pControl, info.PaymentIdentifier, info, nil, nil,
		)

		// Create three unique attempts we'll use for the test, and
		// register them with the payment control. We set each
		// attempts's value to one third of the payment amount, and
		// populate the MPP options.
		shardAmt := info.Value / 3
		attempt.Route.FinalHop().AmtToForward = shardAmt
		attempt.Route.FinalHop().MPP = record.NewMPP(
			info.Value, [32]byte{1},
		)

		var attempts []*HTLCAttemptInfo
		for i := uint64(0); i < 3; i++ {
			a := *attempt
			a.AttemptID = i
			attempts = append(attempts, &a)

			_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &a)
			if err != nil {
				t.Fatalf("unable to send htlc message: %v", err)
			}
			assertPaymentStatus(
				t, pControl, info.PaymentIdentifier, StatusInFlight,
			)

			htlc := &htlcStatus{
				HTLCAttemptInfo: &a,
			}
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info, nil, htlc,
			)
		}

		// For a fourth attempt, check that attempting to
		// register it will fail since the total sent amount
		// will be too large.
		b := *attempt
		b.AttemptID = 3
		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
		if err != ErrValueExceedsAmt {
			t.Fatalf("expected ErrValueExceedsAmt, got: %v",
				err)
		}

		// Fail the second attempt.
		a := attempts[1]
		htlcFail := HTLCFailUnreadable
		_, err = pControl.FailAttempt(
			info.PaymentIdentifier, a.AttemptID,
			&HTLCFailInfo{
				Reason: htlcFail,
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		htlc := &htlcStatus{
			HTLCAttemptInfo: a,
			failure:         &htlcFail,
		}
		assertPaymentInfo(
			t, pControl, info.PaymentIdentifier, info, nil, htlc,
		)

		// Payment should still be in-flight.
		assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

		// Depending on the test case, settle or fail the first attempt.
		a = attempts[0]
		htlc = &htlcStatus{
			HTLCAttemptInfo: a,
		}

		var firstFailReason *FailureReason
		if test.settleFirst {
			_, err := pControl.SettleAttempt(
				info.PaymentIdentifier, a.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been "+
					"received, got: %v", err)
			}

			// Assert that the HTLC has had the preimage recorded.
			htlc.settle = &preimg
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info, nil, htlc,
			)
		} else {
			_, err := pControl.FailAttempt(
				info.PaymentIdentifier, a.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFail,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been "+
					"received, got: %v", err)
			}

			// Assert the failure was recorded.
			htlc.failure = &htlcFail
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info, nil, htlc,
			)

			// We also record a payment level fail, to move it into
			// a terminal state.
			failReason := FailureReasonNoRoute
			_, err = pControl.Fail(info.PaymentIdentifier, failReason)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

			// Record the reason we failed the payment, such that
			// we can assert this later in the test.
			firstFailReason = &failReason
		}

		// The payment should still be considered in-flight, since there
		// is still an active HTLC.
		assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

		// Try to register yet another attempt. This should fail now
		// that the payment has reached a terminal condition.
		b = *attempt
		b.AttemptID = 3
		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
		if err != ErrPaymentTerminal {
			t.Fatalf("expected ErrPaymentTerminal, got: %v", err)
		}

		assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)

		// Settle or fail the remaining attempt based on the testcase.
		a = attempts[2]
		htlc = &htlcStatus{
			HTLCAttemptInfo: a,
		}
		if test.settleLast {
			// Settle the last outstanding attempt.
			_, err = pControl.SettleAttempt(
				info.PaymentIdentifier, a.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been "+
					"received, got: %v", err)
			}

			htlc.settle = &preimg
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info,
				firstFailReason, htlc,
			)
		} else {
			// Fail the attempt.
			_, err := pControl.FailAttempt(
				info.PaymentIdentifier, a.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFail,
				},
			)
			if err != nil {
				t.Fatalf("error shouldn't have been "+
					"received, got: %v", err)
			}

			// Assert the failure was recorded.
			htlc.failure = &htlcFail
			assertPaymentInfo(
				t, pControl, info.PaymentIdentifier, info,
				firstFailReason, htlc,
			)

			// Check that we can override any perevious terminal
			// failure. This is to allow multiple concurrent shard
			// write a terminal failure to the database without
			// syncing.
			failReason := FailureReasonPaymentDetails
			_, err = pControl.Fail(info.PaymentIdentifier, failReason)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}
		}

		// If any of the two attempts settled, the payment should end
		// up in the Succeeded state. If both failed the payment should
		// also be Failed at this poinnt.
		finalStatus := StatusFailed
		if test.settleFirst || test.settleLast {
			finalStatus = StatusSucceeded
		}

		assertPaymentStatus(t, pControl, info.PaymentIdentifier, finalStatus)

		// Finally assert we cannot register more attempts.
		_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
		require.Equal(t, ErrPaymentTerminal, err)
	}

	for _, test := range tests {
		test := test
		subTest := fmt.Sprintf("first=%v, second=%v",
			test.settleFirst, test.settleLast)

		t.Run(subTest, func(t *testing.T) {
			runSubTest(t, test)
		})
	}
}

func TestPaymentControlMPPRecordValidation(t *testing.T) {
	t.Parallel()

	db, cleanup, err := MakeTestDB()
	defer cleanup()

	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(db)

	info, attempt, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Init the payment.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Create three unique attempts we'll use for the test, and
	// register them with the payment control. We set each
	// attempts's value to one third of the payment amount, and
	// populate the MPP options.
	shardAmt := info.Value / 3
	attempt.Route.FinalHop().AmtToForward = shardAmt
	attempt.Route.FinalHop().MPP = record.NewMPP(
		info.Value, [32]byte{1},
	)

	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Now try to register a non-MPP attempt, which should fail.
	b := *attempt
	b.AttemptID = 1
	b.Route.FinalHop().MPP = nil
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
	if err != ErrMPPayment {
		t.Fatalf("expected ErrMPPayment, got: %v", err)
	}

	// Try to register attempt one with a different payment address.
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value, [32]byte{2},
	)
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
	if err != ErrMPPPaymentAddrMismatch {
		t.Fatalf("expected ErrMPPPaymentAddrMismatch, got: %v", err)
	}

	// Try registering one with a different total amount.
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value/2, [32]byte{1},
	)
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
	if err != ErrMPPTotalAmountMismatch {
		t.Fatalf("expected ErrMPPTotalAmountMismatch, got: %v", err)
	}

	// Create and init a new payment. This time we'll check that we cannot
	// register an MPP attempt if we already registered a non-MPP one.
	info, attempt, _, err = genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	attempt.Route.FinalHop().MPP = nil
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	if err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Attempt to register an MPP attempt, which should fail.
	b = *attempt
	b.AttemptID = 1
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value, [32]byte{1},
	)

	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, &b)
	if err != ErrNonMPPayment {
		t.Fatalf("expected ErrNonMPPayment, got: %v", err)
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

type htlcStatus struct {
	*HTLCAttemptInfo
	settle  *lntypes.Preimage
	failure *HTLCFailReason
}

// assertPaymentInfo retrieves the payment referred to by hash and verifies the
// expected values.
func assertPaymentInfo(t *testing.T, p *PaymentControl, hash lntypes.Hash,
	c *PaymentCreationInfo, f *FailureReason, a *htlcStatus) {

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

	htlc := payment.HTLCs[a.AttemptID]
	if err := assertRouteEqual(&htlc.Route, &a.Route); err != nil {
		t.Fatal("routes do not match")
	}

	if htlc.AttemptID != a.AttemptID {
		t.Fatalf("unnexpected attempt ID %v, expected %v",
			htlc.AttemptID, a.AttemptID)
	}

	if a.failure != nil {
		if htlc.Failure == nil {
			t.Fatalf("expected HTLC to be failed")
		}

		if htlc.Failure.Reason != *a.failure {
			t.Fatalf("expected HTLC failure %v, had %v",
				*a.failure, htlc.Failure.Reason)
		}
	} else if htlc.Failure != nil {
		t.Fatalf("expected no HTLC failure")
	}

	if a.settle != nil {
		if htlc.Settle.Preimage != *a.settle {
			t.Fatalf("Preimages don't match: %x vs %x",
				htlc.Settle.Preimage, a.settle)
		}
	} else if htlc.Settle != nil {
		t.Fatal("expected no settle info")
	}
}

// fetchPaymentIndexEntry gets the payment hash for the sequence number provided
// from our payment indexes bucket.
func fetchPaymentIndexEntry(_ *testing.T, p *PaymentControl,
	sequenceNumber uint64) (*lntypes.Hash, error) {

	var hash lntypes.Hash

	if err := kvdb.View(p.db, func(tx walletdb.ReadTx) error {
		indexBucket := tx.ReadBucket(paymentsIndexBucket)
		key := make([]byte, 8)
		byteOrder.PutUint64(key, sequenceNumber)

		indexValue := indexBucket.Get(key)
		if indexValue == nil {
			return errNoSequenceNrIndex
		}

		r := bytes.NewReader(indexValue)

		var err error
		hash, err = deserializePaymentIndex(r)
		return err
	}, func() {
		hash = lntypes.Hash{}
	}); err != nil {
		return nil, err
	}

	return &hash, nil
}

// assertPaymentIndex looks up the index for a payment in the db and checks
// that its payment hash matches the expected hash passed in.
func assertPaymentIndex(t *testing.T, p *PaymentControl,
	expectedHash lntypes.Hash) {

	// Lookup the payment so that we have its sequence number and check
	// that is has correctly been indexed in the payment indexes bucket.
	pmt, err := p.FetchPayment(expectedHash)
	require.NoError(t, err)

	hash, err := fetchPaymentIndexEntry(t, p, pmt.SequenceNum)
	require.NoError(t, err)
	assert.Equal(t, expectedHash, *hash)
}

// assertNoIndex checks that an index for the sequence number provided does not
// exist.
func assertNoIndex(t *testing.T, p *PaymentControl, seqNr uint64) {
	_, err := fetchPaymentIndexEntry(t, p, seqNr)
	require.Equal(t, errNoSequenceNrIndex, err)
}
