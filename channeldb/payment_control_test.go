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

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	require.NoError(t, err, "unable to generate htlc message")

	// Sends base htlc message which initiate StatusInFlight.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	assertPaymentIndex(t, pControl, info.PaymentIdentifier)
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusInFlight)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, nil, nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	_, err = pControl.Fail(info.PaymentIdentifier, failReason)
	require.NoError(t, err, "unable to fail payment hash")

	// Verify the status is indeed Failed.
	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusFailed)
	assertPaymentInfo(
		t, pControl, info.PaymentIdentifier, info, &failReason, nil,
	)

	// Lookup the payment so we can get its old sequence number before it is
	// overwritten.
	payment, err := pControl.FetchPayment(info.PaymentIdentifier)
	require.NoError(t, err)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

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
	require.NoError(t, err, "unable to register attempt")

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
	require.NoError(t, err, "unable to send htlc message")
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
	require.NoError(t, err, "error shouldn't have been received, got")

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

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	info, attempt, preimg, err := genInfo()
	require.NoError(t, err, "unable to generate htlc message")

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

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
	require.NoError(t, err, "unable to send htlc message")
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
	require.NoError(t, err, "error shouldn't have been received, got")
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

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	info, _, preimg, err := genInfo()
	require.NoError(t, err, "unable to generate htlc message")

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

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	info, _, _, err := genInfo()
	require.NoError(t, err, "unable to generate htlc message")

	// Calling Fail should return an error.
	_, err = pControl.Fail(info.PaymentIdentifier, FailureReasonNoRoute)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, pControl, info.PaymentIdentifier, StatusUnknown)
}

// TestPaymentControlDeleteNonInFlight checks that calling DeletePayments only
// deletes payments from the database that are not in-flight.
func TestPaymentControlDeleteNonInFlight(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

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
		t.Log("fetch payment has status", p.Status)
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

// TestPaymentControlDeletePayments tests that DeletePayments correctly deletes
// information about completed payments from the database.
func TestPaymentControlDeletePayments(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	// Register three payments:
	// 1. A payment with two failed attempts.
	// 2. A payment with one failed and one settled attempt.
	// 3. A payment with one failed and one in-flight attempt.
	payments := []*payment{
		{status: StatusFailed},
		{status: StatusSucceeded},
		{status: StatusInFlight},
	}

	// Use helper function to register the test payments in the data and
	// populate the data to the payments slice.
	createTestPayments(t, pControl, payments)

	// Check that all payments are there as we added them.
	assertPayments(t, db, payments)

	// Delete HTLC attempts for failed payments only.
	require.NoError(t, db.DeletePayments(true, true))

	// The failed payment is the only altered one.
	payments[0].htlcs = 0
	assertPayments(t, db, payments)

	// Delete failed attempts for all payments.
	require.NoError(t, db.DeletePayments(false, true))

	// The failed attempts should be deleted, except for the in-flight
	// payment, that shouldn't be altered until it has completed.
	payments[1].htlcs = 1
	assertPayments(t, db, payments)

	// Now delete all failed payments.
	require.NoError(t, db.DeletePayments(true, false))

	assertPayments(t, db, payments[1:])

	// Finally delete all completed payments.
	require.NoError(t, db.DeletePayments(false, false))

	assertPayments(t, db, payments[2:])
}

// TestPaymentControlDeleteSinglePayment tests that DeletePayment correctly
// deletes information about a completed payment from the database.
func TestPaymentControlDeleteSinglePayment(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	// Register four payments:
	// All payments will have one failed HTLC attempt and one HTLC attempt
	// according to its final status.
	// 1. A payment with two failed attempts.
	// 2. Another payment with two failed attempts.
	// 3. A payment with one failed and one settled attempt.
	// 4. A payment with one failed and one in-flight attempt.

	// Initiate payments, which is a slice of payment that is used as
	// template to create the corresponding test payments in the database.
	//
	// Note: The payment id and number of htlc attempts of each payment will
	// be added to this slice when creating the payments below.
	// This allows the slice to be used directly for testing purposes.
	payments := []*payment{
		{status: StatusFailed},
		{status: StatusFailed},
		{status: StatusSucceeded},
		{status: StatusInFlight},
	}

	// Use helper function to register the test payments in the data and
	// populate the data to the payments slice.
	createTestPayments(t, pControl, payments)

	// Check that all payments are there as we added them.
	assertPayments(t, db, payments)

	// Delete HTLC attempts for first payment only.
	require.NoError(t, db.DeletePayment(payments[0].id, true))

	// The first payment is the only altered one as its failed HTLC should
	// have been removed but is still present as payment.
	payments[0].htlcs = 0
	assertPayments(t, db, payments)

	// Delete the first payment completely.
	require.NoError(t, db.DeletePayment(payments[0].id, false))

	// The first payment should have been deleted.
	assertPayments(t, db, payments[1:])

	// Now delete the second payment completely.
	require.NoError(t, db.DeletePayment(payments[1].id, false))

	// The Second payment should have been deleted.
	assertPayments(t, db, payments[2:])

	// Delete failed HTLC attempts for the third payment.
	require.NoError(t, db.DeletePayment(payments[2].id, true))

	// Only the successful HTLC attempt should be left for the third payment.
	payments[2].htlcs = 1
	assertPayments(t, db, payments[2:])

	// Now delete the third payment completely.
	require.NoError(t, db.DeletePayment(payments[2].id, false))

	// Only the last payment should be left.
	assertPayments(t, db, payments[3:])

	// Deleting HTLC attempts from InFlight payments should not work and an
	// error returned.
	require.Error(t, db.DeletePayment(payments[3].id, true))

	// The payment is InFlight and therefore should not have been altered.
	assertPayments(t, db, payments[3:])

	// Finally deleting the InFlight payment should also not work and an
	// error returned.
	require.Error(t, db.DeletePayment(payments[3].id, false))

	// The payment is InFlight and therefore should not have been altered.
	assertPayments(t, db, payments[3:])
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
		db, err := MakeTestDB(t)
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

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to init db")

	pControl := NewPaymentControl(db)

	info, attempt, _, err := genInfo()
	require.NoError(t, err, "unable to generate htlc message")

	// Init the payment.
	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

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
	require.NoError(t, err, "unable to send htlc message")

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
	require.NoError(t, err, "unable to generate htlc message")

	err = pControl.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	attempt.Route.FinalHop().MPP = nil
	_, err = pControl.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to send htlc message")

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

// TestDeleteFailedAttempts checks that DeleteFailedAttempts properly removes
// failed HTLCs from finished payments.
func TestDeleteFailedAttempts(t *testing.T) {
	t.Parallel()

	t.Run("keep failed payment attempts", func(t *testing.T) {
		testDeleteFailedAttempts(t, true)
	})
	t.Run("remove failed payment attempts", func(t *testing.T) {
		testDeleteFailedAttempts(t, false)
	})
}

func testDeleteFailedAttempts(t *testing.T, keepFailedPaymentAttempts bool) {
	db, err := MakeTestDB(t)

	require.NoError(t, err, "unable to init db")
	db.keepFailedPaymentAttempts = keepFailedPaymentAttempts

	pControl := NewPaymentControl(db)

	// Register three payments:
	// All payments will have one failed HTLC attempt and one HTLC attempt
	// according to its final status.
	// 1. A payment with two failed attempts.
	// 2. A payment with one failed and one in-flight attempt.
	// 3. A payment with one failed and one settled attempt.

	// Initiate payments, which is a slice of payment that is used as
	// template to create the corresponding test payments in the database.
	//
	// Note: The payment id and number of htlc attempts of each payment will
	// be added to this slice when creating the payments below.
	// This allows the slice to be used directly for testing purposes.
	payments := []*payment{
		{status: StatusFailed},
		{status: StatusInFlight},
		{status: StatusSucceeded},
	}

	// Use helper function to register the test payments in the data and
	// populate the data to the payments slice.
	createTestPayments(t, pControl, payments)

	// Check that all payments are there as we added them.
	assertPayments(t, db, payments)

	// Calling DeleteFailedAttempts on a failed payment should delete all
	// HTLCs.
	require.NoError(t, pControl.DeleteFailedAttempts(payments[0].id))

	// Expect all HTLCs to be deleted if the config is set to delete them.
	if !keepFailedPaymentAttempts {
		payments[0].htlcs = 0
	}
	assertPayments(t, db, payments)

	// Calling DeleteFailedAttempts on an in-flight payment should return
	// an error.
	if keepFailedPaymentAttempts {
		require.NoError(t, pControl.DeleteFailedAttempts(payments[1].id))
	} else {
		require.Error(t, pControl.DeleteFailedAttempts(payments[1].id))
	}

	// Since DeleteFailedAttempts returned an error, we should expect the
	// payment to be unchanged.
	assertPayments(t, db, payments)

	// Cleaning up a successful payment should remove failed htlcs.
	require.NoError(t, pControl.DeleteFailedAttempts(payments[2].id))
	// Expect all HTLCs except for the settled one to be deleted if the
	// config is set to delete them.
	if !keepFailedPaymentAttempts {
		payments[2].htlcs = 1
	}
	assertPayments(t, db, payments)

	if keepFailedPaymentAttempts {
		// DeleteFailedAttempts is ignored, even for non-existent
		// payments, if the control tower is configured to keep failed
		// HTLCs.
		require.NoError(t, pControl.DeleteFailedAttempts(lntypes.ZeroHash))
	} else {
		// Attempting to cleanup a non-existent payment returns an error.
		require.Error(t, pControl.DeleteFailedAttempts(lntypes.ZeroHash))
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

// payment is a helper structure that holds basic information on a test payment,
// such as the payment id, the status and the total number of HTLCs attempted.
type payment struct {
	id     lntypes.Hash
	status PaymentStatus
	htlcs  int
}

// createTestPayments registers payments depending on the provided statuses in
// the payments slice. Each payment will receive one failed HTLC and another
// HTLC depending on the final status of the payment provided.
func createTestPayments(t *testing.T, p *PaymentControl, payments []*payment) {
	attemptID := uint64(0)

	for i := 0; i < len(payments); i++ {
		info, attempt, preimg, err := genInfo()
		require.NoError(t, err, "unable to generate htlc message")

		// Set the payment id accordingly in the payments slice.
		payments[i].id = info.PaymentIdentifier

		attempt.AttemptID = attemptID
		attemptID++

		// Init the payment.
		err = p.InitPayment(info.PaymentIdentifier, info)
		require.NoError(t, err, "unable to send htlc message")

		// Register and fail the first attempt for all payments.
		_, err = p.RegisterAttempt(info.PaymentIdentifier, attempt)
		require.NoError(t, err, "unable to send htlc message")

		htlcFailure := HTLCFailUnreadable
		_, err = p.FailAttempt(
			info.PaymentIdentifier, attempt.AttemptID,
			&HTLCFailInfo{
				Reason: htlcFailure,
			},
		)
		require.NoError(t, err, "unable to fail htlc")

		// Increase the HTLC counter in the payments slice for the
		// failed attempt.
		payments[i].htlcs++

		// Depending on the test case, fail or succeed the next
		// attempt.
		attempt.AttemptID = attemptID
		attemptID++

		_, err = p.RegisterAttempt(info.PaymentIdentifier, attempt)
		require.NoError(t, err, "unable to send htlc message")

		switch payments[i].status {
		// Fail the attempt and the payment overall.
		case StatusFailed:
			htlcFailure := HTLCFailUnreadable
			_, err = p.FailAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCFailInfo{
					Reason: htlcFailure,
				},
			)
			require.NoError(t, err, "unable to fail htlc")

			failReason := FailureReasonNoRoute
			_, err = p.Fail(info.PaymentIdentifier,
				failReason)
			require.NoError(t, err, "unable to fail payment hash")

		// Settle the attempt
		case StatusSucceeded:
			_, err := p.SettleAttempt(
				info.PaymentIdentifier, attempt.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			require.NoError(t, err, "no error should have been "+
				"received from settling a htlc attempt")

		// We leave the attempt in-flight by doing nothing.
		case StatusInFlight:
		}

		// Increase the HTLC counter in the payments slice for any
		// attempt above.
		payments[i].htlcs++
	}
}

// assertPayments is a helper function that given a slice of payment and
// indices for the slice asserts that exactly the same payments in the
// slice for the provided indices exist when fetching payments from the
// database.
func assertPayments(t *testing.T, db *DB, payments []*payment) {
	t.Helper()

	dbPayments, err := db.FetchPayments()
	require.NoError(t, err, "could not fetch payments from db")

	// Make sure that the number of fetched payments is the same
	// as expected.
	require.Len(t, dbPayments, len(payments), "unexpected number of payments")

	// Convert fetched payments of type MPPayment to our helper structure.
	p := make([]*payment, len(dbPayments))
	for i, dbPayment := range dbPayments {
		p[i] = &payment{
			id:     dbPayment.Info.PaymentIdentifier,
			status: dbPayment.Status,
			htlcs:  len(dbPayment.HTLCs),
		}
	}

	// Check that each payment we want to assert exists in the database.
	require.Equal(t, payments, p)
}
