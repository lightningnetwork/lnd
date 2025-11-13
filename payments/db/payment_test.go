package paymentsdb

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	testHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	rev = [chainhash.HashSize]byte{
		0x51, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x48, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0x2d, 0xe7, 0x93, 0xe4,
	}
)

var (
	priv, _ = btcec.NewPrivateKey()
	pub     = priv.PubKey()
	vertex  = route.NewVertex(pub)

	testHop1 = &route.Hop{
		PubKeyBytes:      vertex,
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		CustomRecords: record.CustomSet{
			65536: []byte{},
			80001: []byte{},
		},
		MPP:      record.NewMPP(32, [32]byte{0x42}),
		Metadata: []byte{1, 2, 3},
	}

	testHop2 = &route.Hop{
		PubKeyBytes:      vertex,
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		LegacyPayload:    true,
	}

	testRoute = route.Route{
		TotalTimeLock: 123,
		TotalAmount:   1234567,
		SourcePubKey:  vertex,
		Hops: []*route.Hop{
			testHop2,
			testHop1,
		},
	}

	testBlindedRoute = route.Route{
		TotalTimeLock: 150,
		TotalAmount:   1000,
		SourcePubKey:  vertex,
		Hops: []*route.Hop{
			{
				PubKeyBytes:      vertex,
				ChannelID:        9876,
				OutgoingTimeLock: 120,
				AmtToForward:     900,
				EncryptedData:    []byte{1, 3, 3},
				BlindingPoint:    pub,
			},
			{
				PubKeyBytes:   vertex,
				EncryptedData: []byte{3, 2, 1},
			},
			{
				PubKeyBytes:      vertex,
				Metadata:         []byte{4, 5, 6},
				AmtToForward:     500,
				OutgoingTimeLock: 100,
				TotalAmtMsat:     500,
			},
		},
	}
)

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
func createTestPayments(t *testing.T, p DB, payments []*payment) {
	t.Helper()

	attemptID := uint64(0)

	for i := 0; i < len(payments); i++ {
		info, attempt, preimg, err := genInfo(t)
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

// assertRouteEquals compares to routes for equality and returns an error if
// they are not equal.
func assertRouteEqual(t *testing.T, a, b *route.Route) error {
	t.Helper()

	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("HTLCAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(b))
	}

	return nil
}

// assertPaymentInfo retrieves the payment referred to by hash and verifies the
// expected values.
func assertPaymentInfo(t *testing.T, p DB, hash lntypes.Hash,
	c *PaymentCreationInfo, f *FailureReason,
	a *htlcStatus) {

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
	if err := assertRouteEqual(t, &htlc.Route, &a.Route); err != nil {
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

// assertDBPaymentstatus retrieves the status of the payment referred to by hash
// and compares it with the expected state.
func assertDBPaymentstatus(t *testing.T, p DB, hash lntypes.Hash,
	expStatus PaymentStatus) {

	t.Helper()

	payment, err := p.FetchPayment(hash)
	if errors.Is(err, ErrPaymentNotInitiated) {
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

// assertDBPayments is a helper function that given a slice of payment and
// indices for the slice asserts that exactly the same payments in the
// slice for the provided indices exist when fetching payments from the
// database.
func assertDBPayments(t *testing.T, paymentDB DB, payments []*payment) {
	t.Helper()

	response, err := paymentDB.QueryPayments(
		t.Context(), Query{
			IndexOffset:       0,
			MaxPayments:       uint64(len(payments)),
			IncludeIncomplete: true,
		},
	)
	require.NoError(t, err, "could not fetch payments from db")

	dbPayments := response.Payments

	// Make sure that the number of fetched payments is the same
	// as expected.
	require.Len(
		t, dbPayments, len(payments), "unexpected number of payments",
	)

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

// genPreimage generates a random preimage.
func genPreimage(t *testing.T) ([32]byte, error) {
	t.Helper()

	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}

	return preimage, nil
}

// genInfo generates a payment creation info, an attempt info and a preimage.
func genInfo(t *testing.T) (*PaymentCreationInfo, *HTLCAttemptInfo,
	lntypes.Preimage, error) {

	preimage, err := genPreimage(t)
	if err != nil {
		return nil, nil, preimage, fmt.Errorf("unable to "+
			"generate preimage: %v", err)
	}

	rhash := sha256.Sum256(preimage[:])
	var hash lntypes.Hash
	copy(hash[:], rhash[:])

	attempt, err := NewHtlcAttempt(
		0, priv, *testRoute.Copy(), time.Time{}, &hash,
	)
	require.NoError(t, err)

	return &PaymentCreationInfo{
		PaymentIdentifier: rhash,
		Value:             testRoute.ReceiverAmt(),
		CreationTime:      time.Unix(time.Now().Unix(), 0),
		PaymentRequest:    []byte("hola"),
	}, &attempt.HTLCAttemptInfo, preimage, nil
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

// testDeleteFailedAttempts tests the DeleteFailedAttempts method with the
// given keepFailedPaymentAttempts flag as argument.
func testDeleteFailedAttempts(t *testing.T, keepFailedPaymentAttempts bool) {
	paymentDB := NewTestDB(
		t, WithKeepFailedPaymentAttempts(keepFailedPaymentAttempts),
	)

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
	createTestPayments(t, paymentDB, payments)

	// Check that all payments are there as we added them.
	assertDBPayments(t, paymentDB, payments)

	// Calling DeleteFailedAttempts on a failed payment should delete all
	// HTLCs.
	require.NoError(t, paymentDB.DeleteFailedAttempts(payments[0].id))

	// Expect all HTLCs to be deleted if the config is set to delete them.
	if !keepFailedPaymentAttempts {
		payments[0].htlcs = 0
	}
	assertDBPayments(t, paymentDB, payments)

	// Calling DeleteFailedAttempts on an in-flight payment should return
	// an error.
	//
	// NOTE: In case the option keepFailedPaymentAttempts is set no delete
	// operation are performed in general therefore we do NOT expect an
	// error in this case.
	if keepFailedPaymentAttempts {
		require.NoError(
			t, paymentDB.DeleteFailedAttempts(payments[1].id),
		)
	} else {
		require.Error(t, paymentDB.DeleteFailedAttempts(payments[1].id))
	}

	// Since DeleteFailedAttempts returned an error, we should expect the
	// payment to be unchanged.
	assertDBPayments(t, paymentDB, payments)

	// Cleaning up a successful payment should remove failed htlcs.
	require.NoError(t, paymentDB.DeleteFailedAttempts(payments[2].id))

	// Expect all HTLCs except for the settled one to be deleted if the
	// config is set to delete them.
	if !keepFailedPaymentAttempts {
		payments[2].htlcs = 1
	}
	assertDBPayments(t, paymentDB, payments)

	// NOTE: In case the option keepFailedPaymentAttempts is set no delete
	// operation are performed in general therefore we do NOT expect an
	// error in this case.
	if keepFailedPaymentAttempts {
		// DeleteFailedAttempts is ignored, even for non-existent
		// payments, if the control tower is configured to keep failed
		// HTLCs.
		require.NoError(
			t, paymentDB.DeleteFailedAttempts(lntypes.ZeroHash),
		)
	} else {
		// Attempting to cleanup a non-existent payment returns an
		// error.
		require.Error(
			t, paymentDB.DeleteFailedAttempts(lntypes.ZeroHash),
		)
	}
}

// TestMPPRecordValidation tests MPP record validation.
func TestMPPRecordValidation(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

	info, attempt, _, err := genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	// Init the payment.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
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

	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to send htlc message")

	// Now try to register a non-MPP attempt, which should fail.
	b := *attempt
	b.AttemptID = 1
	b.Route.FinalHop().MPP = nil
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
	require.ErrorIs(t, err, ErrMPPayment)

	// Try to register attempt one with a different payment address.
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value, [32]byte{2},
	)
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
	require.ErrorIs(t, err, ErrMPPPaymentAddrMismatch)

	// Try registering one with a different total amount.
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value/2, [32]byte{1},
	)
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
	require.ErrorIs(t, err, ErrMPPTotalAmountMismatch)

	// Create and init a new payment. This time we'll check that we cannot
	// register an MPP attempt if we already registered a non-MPP one.
	info, attempt, _, err = genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	attempt.Route.FinalHop().MPP = nil
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to send htlc message")

	// Attempt to register an MPP attempt, which should fail.
	b = *attempt
	b.AttemptID = 1
	b.Route.FinalHop().MPP = record.NewMPP(
		info.Value, [32]byte{1},
	)

	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
	require.ErrorIs(t, err, ErrNonMPPayment)
}

// TestDeleteSinglePayment tests that DeletePayment correctly
// deletes information about a completed payment from the database.
func TestDeleteSinglePayment(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

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
	createTestPayments(t, paymentDB, payments)

	// Check that all payments are there as we added them.
	assertDBPayments(t, paymentDB, payments)

	// Delete HTLC attempts for first payment only.
	require.NoError(t, paymentDB.DeletePayment(payments[0].id, true))

	// The first payment is the only altered one as its failed HTLC should
	// have been removed but is still present as payment.
	payments[0].htlcs = 0
	assertDBPayments(t, paymentDB, payments)

	// Delete the first payment completely.
	require.NoError(t, paymentDB.DeletePayment(payments[0].id, false))

	// The first payment should have been deleted.
	assertDBPayments(t, paymentDB, payments[1:])

	// Now delete the second payment completely.
	require.NoError(t, paymentDB.DeletePayment(payments[1].id, false))

	// The Second payment should have been deleted.
	assertDBPayments(t, paymentDB, payments[2:])

	// Delete failed HTLC attempts for the third payment.
	require.NoError(t, paymentDB.DeletePayment(payments[2].id, true))

	// Only the successful HTLC attempt should be left for the third
	// payment.
	payments[2].htlcs = 1
	assertDBPayments(t, paymentDB, payments[2:])

	// Now delete the third payment completely.
	require.NoError(t, paymentDB.DeletePayment(payments[2].id, false))

	// Only the last payment should be left.
	assertDBPayments(t, paymentDB, payments[3:])

	// Deleting HTLC attempts from InFlight payments should not work and an
	// error returned.
	require.Error(t, paymentDB.DeletePayment(payments[3].id, true))

	// The payment is InFlight and therefore should not have been altered.
	assertDBPayments(t, paymentDB, payments[3:])

	// Finally deleting the InFlight payment should also not work and an
	// error returned.
	require.Error(t, paymentDB.DeletePayment(payments[3].id, false))

	// The payment is InFlight and therefore should not have been altered.
	assertDBPayments(t, paymentDB, payments[3:])
}

// TestPaymentRegistrable checks the method `Registrable` behaves as expected
// for ALL possible payment statuses.
func TestPaymentRegistrable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		status         PaymentStatus
		registryErr    error
		hasSettledHTLC bool
		paymentFailed  bool
	}{
		{
			status:      StatusInitiated,
			registryErr: nil,
		},
		{
			// Test inflight status with no settled HTLC and no
			// failed payment.
			status:      StatusInFlight,
			registryErr: nil,
		},
		{
			// Test inflight status with settled HTLC but no failed
			// payment.
			status:         StatusInFlight,
			registryErr:    ErrPaymentPendingSettled,
			hasSettledHTLC: true,
		},
		{
			// Test inflight status with no settled HTLC but failed
			// payment.
			status:        StatusInFlight,
			registryErr:   ErrPaymentPendingFailed,
			paymentFailed: true,
		},
		{
			// Test error state with settled HTLC and failed
			// payment.
			status:         0,
			registryErr:    ErrUnknownPaymentStatus,
			hasSettledHTLC: true,
			paymentFailed:  true,
		},
		{
			status:      StatusSucceeded,
			registryErr: ErrPaymentAlreadySucceeded,
		},
		{
			status:      StatusFailed,
			registryErr: ErrPaymentAlreadyFailed,
		},
		{
			status:      0,
			registryErr: ErrUnknownPaymentStatus,
		},
	}

	for i, tc := range testCases {
		i, tc := i, tc

		p := &MPPayment{
			Status: tc.status,
			State: &MPPaymentState{
				HasSettledHTLC: tc.hasSettledHTLC,
				PaymentFailed:  tc.paymentFailed,
			},
		}

		name := fmt.Sprintf("test_%d_%s", i, p.Status.String())
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := p.Registrable()
			require.ErrorIs(t, err, tc.registryErr,
				"registrable under state %v", tc.status)
		})
	}
}

// TestPaymentSetState checks that the method setState creates the
// MPPaymentState as expected.
func TestPaymentSetState(t *testing.T) {
	t.Parallel()

	// Create a test preimage and failure reason.
	preimage := lntypes.Preimage{1}
	failureReasonError := FailureReasonError

	testCases := []struct {
		name     string
		payment  *MPPayment
		totalAmt int

		expectedState *MPPaymentState
		errExpected   error
	}{
		{
			// Test that when the sentAmt exceeds totalAmount, the
			// error is returned.
			name: "amount exceeded error",
			// SentAmt returns 90, 10
			// TerminalInfo returns non-nil, nil
			// InFlightHTLCs returns 0
			payment: &MPPayment{
				HTLCs: []HTLCAttempt{
					makeSettledAttempt(100, 10, preimage),
				},
			},
			totalAmt:    1,
			errExpected: ErrSentExceedsTotal,
		},
		{
			// Test that when the htlc is failed, the fee is not
			// used.
			name: "fee excluded for failed htlc",
			payment: &MPPayment{
				// SentAmt returns 90, 10
				// TerminalInfo returns nil, nil
				// InFlightHTLCs returns 1
				HTLCs: []HTLCAttempt{
					makeActiveAttempt(100, 10),
					makeFailedAttempt(100, 10),
				},
			},
			totalAmt: 1000,
			expectedState: &MPPaymentState{
				NumAttemptsInFlight: 1,
				RemainingAmt:        1000 - 90,
				FeesPaid:            10,
				HasSettledHTLC:      false,
				PaymentFailed:       false,
			},
		},
		{
			// Test when the payment is settled, the state should
			// be marked as terminated.
			name: "payment settled",
			// SentAmt returns 90, 10
			// TerminalInfo returns non-nil, nil
			// InFlightHTLCs returns 0
			payment: &MPPayment{
				HTLCs: []HTLCAttempt{
					makeSettledAttempt(100, 10, preimage),
				},
			},
			totalAmt: 1000,
			expectedState: &MPPaymentState{
				NumAttemptsInFlight: 0,
				RemainingAmt:        1000 - 90,
				FeesPaid:            10,
				HasSettledHTLC:      true,
				PaymentFailed:       false,
			},
		},
		{
			// Test when the payment is failed, the state should be
			// marked as terminated.
			name: "payment failed",
			// SentAmt returns 0, 0
			// TerminalInfo returns nil, non-nil
			// InFlightHTLCs returns 0
			payment: &MPPayment{
				FailureReason: &failureReasonError,
			},
			totalAmt: 1000,
			expectedState: &MPPaymentState{
				NumAttemptsInFlight: 0,
				RemainingAmt:        1000,
				FeesPaid:            0,
				HasSettledHTLC:      false,
				PaymentFailed:       true,
			},
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Attach the payment info.
			info := &PaymentCreationInfo{
				Value: lnwire.MilliSatoshi(tc.totalAmt),
			}
			tc.payment.Info = info

			// Call the method that updates the payment state.
			err := tc.payment.setState()
			require.ErrorIs(t, err, tc.errExpected)

			require.Equal(
				t, tc.expectedState, tc.payment.State,
				"state not updated as expected",
			)
		})
	}
}

// TestNeedWaitAttempts checks whether we need to wait for the results of the
// HTLC attempts against ALL possible payment statuses.
func TestNeedWaitAttempts(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		status           PaymentStatus
		remainingAmt     lnwire.MilliSatoshi
		hasSettledHTLC   bool
		hasFailureReason bool
		needWait         bool
		expectedErr      error
	}{
		{
			// For a newly created payment we don't need to wait
			// for results.
			status:       StatusInitiated,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  nil,
		},
		{
			// With HTLCs inflight we don't need to wait when the
			// remainingAmt is not zero and we have no settled
			// HTLCs.
			status:       StatusInFlight,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  nil,
		},
		{
			// With HTLCs inflight we need to wait when the
			// remainingAmt is not zero but we have settled HTLCs.
			status:         StatusInFlight,
			remainingAmt:   1000,
			hasSettledHTLC: true,
			needWait:       true,
			expectedErr:    nil,
		},
		{
			// With HTLCs inflight we need to wait when the
			// remainingAmt is not zero and the payment is failed.
			status:           StatusInFlight,
			remainingAmt:     1000,
			needWait:         true,
			hasFailureReason: true,
			expectedErr:      nil,
		},

		{
			// With the payment settled, but the remainingAmt is
			// not zero, we have an error state.
			status:       StatusSucceeded,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  ErrPaymentInternal,
		},
		{
			// Payment is in terminal state, no need to wait.
			status:       StatusFailed,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  nil,
		},
		{
			// A newly created payment with zero remainingAmt
			// indicates an error.
			status:       StatusInitiated,
			remainingAmt: 0,
			needWait:     false,
			expectedErr:  ErrPaymentInternal,
		},
		{
			// With zero remainingAmt we must wait for the results.
			status:       StatusInFlight,
			remainingAmt: 0,
			needWait:     true,
			expectedErr:  nil,
		},
		{
			// Payment is terminated, no need to wait for results.
			status:       StatusSucceeded,
			remainingAmt: 0,
			needWait:     false,
			expectedErr:  nil,
		},
		{
			// Payment is terminated, no need to wait for results.
			status:       StatusFailed,
			remainingAmt: 0,
			needWait:     false,
			expectedErr:  ErrPaymentInternal,
		},
		{
			// Payment is in an unknown status, return an error.
			status:       0,
			remainingAmt: 0,
			needWait:     false,
			expectedErr:  ErrUnknownPaymentStatus,
		},
		{
			// Payment is in an unknown status, return an error.
			status:       0,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  ErrUnknownPaymentStatus,
		},
	}

	for _, tc := range testCases {
		tc := tc

		p := &MPPayment{
			Info: &PaymentCreationInfo{
				PaymentIdentifier: [32]byte{1, 2, 3},
			},
			Status: tc.status,
			State: &MPPaymentState{
				RemainingAmt:   tc.remainingAmt,
				HasSettledHTLC: tc.hasSettledHTLC,
				PaymentFailed:  tc.hasFailureReason,
			},
		}

		name := fmt.Sprintf("status=%s|remainingAmt=%v|"+
			"settledHTLC=%v|failureReason=%v", tc.status,
			tc.remainingAmt, tc.hasSettledHTLC, tc.hasFailureReason)

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result, err := p.NeedWaitAttempts()
			require.ErrorIs(t, err, tc.expectedErr)
			require.Equalf(t, tc.needWait, result, "status=%v, "+
				"remainingAmt=%v", tc.status, tc.remainingAmt)
		})
	}
}

// TestAllowMoreAttempts checks whether more attempts can be created against
// ALL possible payment statuses.
func TestAllowMoreAttempts(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		status         PaymentStatus
		remainingAmt   lnwire.MilliSatoshi
		hasSettledHTLC bool
		paymentFailed  bool
		allowMore      bool
		expectedErr    error
	}{
		{
			// A newly created payment with zero remainingAmt
			// indicates an error.
			status:       StatusInitiated,
			remainingAmt: 0,
			allowMore:    false,
			expectedErr:  ErrPaymentInternal,
		},
		{
			// With zero remainingAmt we don't allow more HTLC
			// attempts.
			status:       StatusInFlight,
			remainingAmt: 0,
			allowMore:    false,
			expectedErr:  nil,
		},
		{
			// With zero remainingAmt we don't allow more HTLC
			// attempts.
			status:       StatusSucceeded,
			remainingAmt: 0,
			allowMore:    false,
			expectedErr:  nil,
		},
		{
			// With zero remainingAmt we don't allow more HTLC
			// attempts.
			status:       StatusFailed,
			remainingAmt: 0,
			allowMore:    false,
			expectedErr:  nil,
		},
		{
			// With zero remainingAmt and settled HTLCs we don't
			// allow more HTLC attempts.
			status:         StatusInFlight,
			remainingAmt:   0,
			hasSettledHTLC: true,
			allowMore:      false,
			expectedErr:    nil,
		},
		{
			// With zero remainingAmt and failed payment we don't
			// allow more HTLC attempts.
			status:        StatusInFlight,
			remainingAmt:  0,
			paymentFailed: true,
			allowMore:     false,
			expectedErr:   nil,
		},
		{
			// With zero remainingAmt and both settled HTLCs and
			// failed payment, we don't allow more HTLC attempts.
			status:         StatusInFlight,
			remainingAmt:   0,
			hasSettledHTLC: true,
			paymentFailed:  true,
			allowMore:      false,
			expectedErr:    nil,
		},
		{
			// A newly created payment can have more attempts.
			status:       StatusInitiated,
			remainingAmt: 1000,
			allowMore:    true,
			expectedErr:  nil,
		},
		{
			// With HTLCs inflight we can have more attempts when
			// the remainingAmt is not zero and we have neither
			// failed payment or settled HTLCs.
			status:       StatusInFlight,
			remainingAmt: 1000,
			allowMore:    true,
			expectedErr:  nil,
		},
		{
			// With HTLCs inflight we cannot have more attempts
			// though the remainingAmt is not zero but we have
			// settled HTLCs.
			status:         StatusInFlight,
			remainingAmt:   1000,
			hasSettledHTLC: true,
			allowMore:      false,
			expectedErr:    nil,
		},
		{
			// With HTLCs inflight we cannot have more attempts
			// though the remainingAmt is not zero but we have
			// failed payment.
			status:        StatusInFlight,
			remainingAmt:  1000,
			paymentFailed: true,
			allowMore:     false,
			expectedErr:   nil,
		},
		{
			// With HTLCs inflight we cannot have more attempts
			// though the remainingAmt is not zero but we have
			// settled HTLCs and failed payment.
			status:         StatusInFlight,
			remainingAmt:   1000,
			hasSettledHTLC: true,
			paymentFailed:  true,
			allowMore:      false,
			expectedErr:    nil,
		},
		{
			// With the payment settled, but the remainingAmt is
			// not zero, we have an error state.
			status:         StatusSucceeded,
			remainingAmt:   1000,
			hasSettledHTLC: true,
			allowMore:      false,
			expectedErr:    ErrPaymentInternal,
		},
		{
			// With the payment failed with no inflight HTLCs, we
			// don't allow more attempts to be made.
			status:        StatusFailed,
			remainingAmt:  1000,
			paymentFailed: true,
			allowMore:     false,
			expectedErr:   nil,
		},
		{
			// With the payment in an unknown state, we don't allow
			// more attempts to be made.
			status:       0,
			remainingAmt: 1000,
			allowMore:    false,
			expectedErr:  nil,
		},
	}

	for i, tc := range testCases {
		tc := tc

		p := &MPPayment{
			Info: &PaymentCreationInfo{
				PaymentIdentifier: [32]byte{1, 2, 3},
			},
			Status: tc.status,
			State: &MPPaymentState{
				RemainingAmt:   tc.remainingAmt,
				HasSettledHTLC: tc.hasSettledHTLC,
				PaymentFailed:  tc.paymentFailed,
			},
		}

		name := fmt.Sprintf("test_%d|status=%s|remainingAmt=%v", i,
			tc.status, tc.remainingAmt)

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result, err := p.AllowMoreAttempts()
			require.ErrorIs(t, err, tc.expectedErr)
			require.Equalf(t, tc.allowMore, result, "status=%v, "+
				"remainingAmt=%v", tc.status, tc.remainingAmt)
		})
	}
}

func makeActiveAttempt(total, fee int) HTLCAttempt {
	return HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(total, total-fee),
	}
}

func makeSettledAttempt(total, fee int,
	preimage lntypes.Preimage) HTLCAttempt {

	return HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(total, total-fee),
		Settle:          &HTLCSettleInfo{Preimage: preimage},
	}
}

func makeFailedAttempt(total, fee int) HTLCAttempt {
	return HTLCAttempt{
		HTLCAttemptInfo: makeAttemptInfo(total, total-fee),
		Failure: &HTLCFailInfo{
			Reason: HTLCFailInternal,
		},
	}
}

func makeAttemptInfo(total, amtForwarded int) HTLCAttemptInfo {
	hop := &route.Hop{AmtToForward: lnwire.MilliSatoshi(amtForwarded)}
	return HTLCAttemptInfo{
		Route: route.Route{
			TotalAmount: lnwire.MilliSatoshi(total),
			Hops:        []*route.Hop{hop},
		},
	}
}

// lastHopArgs is a helper struct that holds the arguments for the last hop
// when creating an attempt with a route with a single hop (last hop).
type lastHopArgs struct {
	amt       lnwire.MilliSatoshi
	total     lnwire.MilliSatoshi
	mpp       *record.MPP
	encrypted []byte
}

// makeLastHopAttemptInfo creates an HTLCAttemptInfo with a route with a single
// hop (last hop).
func makeLastHopAttemptInfo(id uint64, args lastHopArgs) HTLCAttemptInfo {
	lastHop := &route.Hop{
		PubKeyBytes:   vertex,
		ChannelID:     1,
		AmtToForward:  args.amt,
		MPP:           args.mpp,
		EncryptedData: args.encrypted,
		TotalAmtMsat:  args.total,
	}

	return HTLCAttemptInfo{
		AttemptID: id,
		Route: route.Route{
			SourcePubKey: vertex,
			TotalAmount:  args.amt,
			Hops:         []*route.Hop{lastHop},
		},
	}
}

// makePayment creates an MPPayment with set of attempts.
func makePayment(total lnwire.MilliSatoshi,
	attempts ...HTLCAttempt) *MPPayment {

	return &MPPayment{
		Info: &PaymentCreationInfo{
			Value: total,
		},
		HTLCs: attempts,
	}
}

// TestVerifyAttemptNonMPPAmountMismatch tests that we return an error if the
// attempted amount doesn't match the payment amount.
func TestVerifyAttemptNonMPPAmountMismatch(t *testing.T) {
	t.Parallel()

	payment := makePayment(1000)
	attempt := makeLastHopAttemptInfo(1, lastHopArgs{amt: 900})

	require.ErrorIs(t, verifyAttempt(payment, &attempt), ErrValueMismatch)
}

// TestVerifyAttemptNonMPPSuccess tests that we don't return an error if the
// attempted amount matches the payment amount.
func TestVerifyAttemptNonMPPSuccess(t *testing.T) {
	t.Parallel()

	payment := makePayment(1200)
	attempt := makeLastHopAttemptInfo(1, lastHopArgs{amt: 1200})

	require.NoError(t, verifyAttempt(payment, &attempt))
}

// TestVerifyAttemptMPPTransitionErrors tests cases where we cannot transition
// from a non-MPP payment to an MPP payment or vice versa.
func TestVerifyAttemptMPPTransitionErrors(t *testing.T) {
	t.Parallel()

	total := lnwire.MilliSatoshi(2000)
	mpp := record.NewMPP(total, testHash)

	paymentWithMPP := makePayment(
		total,
		HTLCAttempt{
			HTLCAttemptInfo: makeLastHopAttemptInfo(
				1,
				lastHopArgs{amt: 1000, mpp: mpp},
			),
		},
	)
	nonMPP := makeLastHopAttemptInfo(2, lastHopArgs{amt: 1000})
	require.ErrorIs(t, verifyAttempt(paymentWithMPP, &nonMPP), ErrMPPayment)

	paymentWithNonMPP := makePayment(
		total,
		HTLCAttempt{
			HTLCAttemptInfo: makeLastHopAttemptInfo(
				1,
				lastHopArgs{amt: total},
			),
		},
	)
	mppAttempt := makeLastHopAttemptInfo(
		2, lastHopArgs{amt: 1000, mpp: mpp},
	)
	require.ErrorIs(
		t,
		verifyAttempt(paymentWithNonMPP, &mppAttempt),
		ErrNonMPPayment,
	)
}

// TestVerifyAttemptMPPOptionMismatch tests that we return an error if the
// MPP options don't match the payment options.
func TestVerifyAttemptMPPOptionMismatch(t *testing.T) {
	t.Parallel()

	total := lnwire.MilliSatoshi(3000)
	goodMPP := record.NewMPP(total, testHash)
	payment := makePayment(
		total,
		HTLCAttempt{
			HTLCAttemptInfo: makeLastHopAttemptInfo(
				1,
				lastHopArgs{amt: 1500, mpp: goodMPP},
			),
		},
	)

	badAddr := record.NewMPP(total, rev)
	attemptBadAddr := makeLastHopAttemptInfo(
		2,
		lastHopArgs{amt: 1500, mpp: badAddr},
	)
	require.ErrorIs(
		t,
		verifyAttempt(payment, &attemptBadAddr),
		ErrMPPPaymentAddrMismatch,
	)

	badTotal := record.NewMPP(total-1, testHash)
	attemptBadTotal := makeLastHopAttemptInfo(
		3,
		lastHopArgs{amt: 1500, mpp: badTotal},
	)
	require.ErrorIs(
		t,
		verifyAttempt(payment, &attemptBadTotal),
		ErrMPPTotalAmountMismatch,
	)

	matching := makeLastHopAttemptInfo(
		4,
		lastHopArgs{amt: 1500, mpp: record.NewMPP(total, testHash)},
	)
	require.NoError(t, verifyAttempt(payment, &matching))
}

// TestVerifyAttemptBlindedValidation tests that we return an error if we try
// to register an MPP attempt for a blinded payment.
func TestVerifyAttemptBlindedValidation(t *testing.T) {
	t.Parallel()

	total := lnwire.MilliSatoshi(5000)

	// Payment with a blinded attempt.
	existing := makeLastHopAttemptInfo(
		1,
		lastHopArgs{amt: 2500, total: total, encrypted: []byte{1}},
	)
	payment := makePayment(
		total,
		HTLCAttempt{HTLCAttemptInfo: existing},
	)

	// Attempt with a normal MPP record should fail because a payment
	// cannot have a mix of blinded and non-blinded attempts.
	goodMPP := makeLastHopAttemptInfo(
		2,
		lastHopArgs{amt: 2500, mpp: record.NewMPP(total, testHash)},
	)
	require.ErrorIs(
		t, verifyAttempt(payment, &goodMPP),
		ErrMixedBlindedAndNonBlindedPayments,
	)

	blindedMPP := makeLastHopAttemptInfo(
		2,
		lastHopArgs{
			amt:       2500,
			total:     total,
			mpp:       record.NewMPP(total, testHash),
			encrypted: []byte{2},
		},
	)
	require.ErrorIs(
		t,
		verifyAttempt(payment, &blindedMPP),
		ErrMPPRecordInBlindedPayment,
	)

	mismatchedTotal := makeLastHopAttemptInfo(
		3,
		lastHopArgs{amt: 2500, total: total + 1, encrypted: []byte{3}},
	)
	require.ErrorIs(
		t,
		verifyAttempt(payment, &mismatchedTotal),
		ErrBlindedPaymentTotalAmountMismatch,
	)

	matching := makeLastHopAttemptInfo(
		4,
		lastHopArgs{amt: 2500, total: total, encrypted: []byte{4}},
	)
	require.NoError(t, verifyAttempt(payment, &matching))
}

// TestVerifyAttemptBlindedMixedWithNonBlinded tests that we return an error if
// we try to register a non-MPP attempt for a blinded payment.
func TestVerifyAttemptBlindedMixedWithNonBlinded(t *testing.T) {
	t.Parallel()

	total := lnwire.MilliSatoshi(4000)

	// Payment with a blinded attempt.
	existing := makeLastHopAttemptInfo(
		1,
		lastHopArgs{amt: 2000, total: total, encrypted: []byte{1}},
	)
	payment := makePayment(
		total,
		HTLCAttempt{HTLCAttemptInfo: existing},
	)

	partial := makeLastHopAttemptInfo(2, lastHopArgs{amt: 2000})
	require.ErrorIs(
		t,
		verifyAttempt(payment, &partial),
		ErrMixedBlindedAndNonBlindedPayments,
	)

	full := makeLastHopAttemptInfo(3, lastHopArgs{amt: total})
	require.ErrorIs(
		t,
		verifyAttempt(payment, &full),
		ErrMixedBlindedAndNonBlindedPayments,
	)
}

// TestVerifyAttemptAmountExceedsTotal tests that we return an error if the
// attempted amount exceeds the payment amount.
func TestVerifyAttemptAmountExceedsTotal(t *testing.T) {
	t.Parallel()

	total := lnwire.MilliSatoshi(1000)
	mpp := record.NewMPP(total, testHash)
	existing := makeLastHopAttemptInfo(1, lastHopArgs{amt: 800, mpp: mpp})
	payment := makePayment(
		total,
		HTLCAttempt{HTLCAttemptInfo: existing},
	)

	attempt := makeLastHopAttemptInfo(2, lastHopArgs{amt: 300, mpp: mpp})
	require.ErrorIs(t, verifyAttempt(payment, &attempt), ErrValueExceedsAmt)
}

// TestEmptyRoutesGenerateSphinxPacket tests that the generateSphinxPacket
// function is able to gracefully handle being passed a nil set of hops for the
// route by the caller.
func TestEmptyRoutesGenerateSphinxPacket(t *testing.T) {
	t.Parallel()

	sessionKey, _ := btcec.NewPrivateKey()
	emptyRoute := &route.Route{}
	_, _, err := generateSphinxPacket(emptyRoute, testHash[:], sessionKey)
	require.ErrorIs(t, err, route.ErrNoRouteHopsProvided)
}

// TestSuccessesWithoutInFlight tests that the payment control will disallow
// calls to Success when no payment is in flight.
func TestSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

	info, _, preimg, err := genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	// Attempt to complete the payment should fail.
	_, err = paymentDB.SettleAttempt(
		info.PaymentIdentifier, 0,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	require.ErrorIs(t, err, ErrPaymentNotInitiated)
}

// TestFailsWithoutInFlight checks that a strict payment control will disallow
// calls to Fail when no payment is in flight.
func TestFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

	info, _, _, err := genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	// Calling Fail should return an error.
	_, err = paymentDB.Fail(
		info.PaymentIdentifier, FailureReasonNoRoute,
	)
	require.ErrorIs(t, err, ErrPaymentNotInitiated)
}

// TestDeletePayments tests that DeletePayments correctly deletes information
// about completed payments from the database.
func TestDeletePayments(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

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
	createTestPayments(t, paymentDB, payments)

	// Check that all payments are there as we added them.
	assertDBPayments(t, paymentDB, payments)

	// Delete HTLC attempts for failed payments only.
	numPayments, err := paymentDB.DeletePayments(true, true)
	require.NoError(t, err)
	require.EqualValues(t, 0, numPayments)

	// The failed payment is the only altered one.
	payments[0].htlcs = 0
	assertDBPayments(t, paymentDB, payments)

	// Delete failed attempts for all payments.
	numPayments, err = paymentDB.DeletePayments(false, true)
	require.NoError(t, err)
	require.EqualValues(t, 0, numPayments)

	// The failed attempts should be deleted, except for the in-flight
	// payment, that shouldn't be altered until it has completed.
	payments[1].htlcs = 1
	assertDBPayments(t, paymentDB, payments)

	// Now delete all failed payments.
	numPayments, err = paymentDB.DeletePayments(true, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, numPayments)

	assertDBPayments(t, paymentDB, payments[1:])

	// Finally delete all completed payments.
	numPayments, err = paymentDB.DeletePayments(false, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, numPayments)

	assertDBPayments(t, paymentDB, payments[2:])
}

// TestSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func TestSwitchDoubleSend(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

	info, attempt, preimg, err := genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	assertPaymentIndex(t, paymentDB, info.PaymentIdentifier)
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInitiated,
	)
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, nil,
	)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	require.ErrorIs(t, err, ErrPaymentExists)

	// Record an attempt.
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to send htlc message")
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInFlight,
	)

	htlc := &htlcStatus{
		HTLCAttemptInfo: attempt,
	}
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, htlc,
	)

	// Sends base htlc message which initiate StatusInFlight.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	if !errors.Is(err, ErrPaymentInFlight) {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}

	// After settling, the error should be ErrAlreadyPaid.
	_, err = paymentDB.SettleAttempt(
		info.PaymentIdentifier, attempt.AttemptID,
		&HTLCSettleInfo{
			Preimage: preimg,
		},
	)
	require.NoError(t, err, "error shouldn't have been received, got")
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusSucceeded,
	)

	htlc.settle = &preimg
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, htlc,
	)

	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	if !errors.Is(err, ErrAlreadyPaid) {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestSwitchFail checks that payment status returns to Failed status after
// failing, and that InitPayment allows another HTLC for the same payment hash.
func TestSwitchFail(t *testing.T) {
	t.Parallel()

	paymentDB := NewTestDB(t)

	info, attempt, preimg, err := genInfo(t)
	require.NoError(t, err, "unable to generate htlc message")

	// Sends base htlc message which initiate StatusInFlight.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	assertPaymentIndex(t, paymentDB, info.PaymentIdentifier)
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInitiated,
	)
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, nil,
	)

	// Fail the payment, which should moved it to Failed.
	failReason := FailureReasonNoRoute
	_, err = paymentDB.Fail(info.PaymentIdentifier, failReason)
	require.NoError(t, err, "unable to fail payment hash")

	// Verify the status is indeed Failed.
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusFailed,
	)

	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, &failReason, nil,
	)

	// Lookup the payment so we can get its old sequence number before it is
	// overwritten.
	payment, err := paymentDB.FetchPayment(info.PaymentIdentifier)
	require.NoError(t, err)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	require.NoError(t, err, "unable to send htlc message")

	// Check that our index has been updated, and the old index has been
	// removed.
	assertPaymentIndex(t, paymentDB, info.PaymentIdentifier)
	assertNoIndex(t, paymentDB, payment.SequenceNum)

	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInitiated,
	)
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, nil,
	)

	// Record a new attempt. In this test scenario, the attempt fails.
	// However, this is not communicated to control tower in the current
	// implementation. It only registers the initiation of the attempt.
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to register attempt")

	htlcReason := HTLCFailUnreadable
	_, err = paymentDB.FailAttempt(
		info.PaymentIdentifier, attempt.AttemptID,
		&HTLCFailInfo{
			Reason: htlcReason,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInFlight,
	)

	htlc := &htlcStatus{
		HTLCAttemptInfo: attempt,
		failure:         &htlcReason,
	}

	assertPaymentInfo(t, paymentDB, info.PaymentIdentifier, info, nil, htlc)

	// Record another attempt.
	attempt.AttemptID = 1
	_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, attempt)
	require.NoError(t, err, "unable to send htlc message")
	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusInFlight,
	)

	htlc = &htlcStatus{
		HTLCAttemptInfo: attempt,
	}

	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, htlc,
	)

	// Settle the attempt and verify that status was changed to
	// StatusSucceeded.
	payment, err = paymentDB.SettleAttempt(
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

	err = assertRouteEqual(t, &payment.HTLCs[0].Route, &attempt.Route)
	if err != nil {
		t.Fatalf("unexpected route returned: %v vs %v: %v",
			spew.Sdump(attempt.Route),
			spew.Sdump(payment.HTLCs[0].Route), err)
	}

	assertDBPaymentstatus(
		t, paymentDB, info.PaymentIdentifier, StatusSucceeded,
	)

	htlc.settle = &preimg
	assertPaymentInfo(
		t, paymentDB, info.PaymentIdentifier, info, nil, htlc,
	)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	err = paymentDB.InitPayment(info.PaymentIdentifier, info)
	if !errors.Is(err, ErrAlreadyPaid) {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// TestMultiShard checks the ability of payment control to have multiple in-
// flight HTLCs for a single payment.
func TestMultiShard(t *testing.T) {
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
		paymentDB := NewTestDB(t)

		info, attempt, preimg, err := genInfo(t)
		if err != nil {
			t.Fatalf("unable to generate htlc message: %v", err)
		}

		// Init the payment, moving it to the StatusInFlight state.
		err = paymentDB.InitPayment(info.PaymentIdentifier, info)
		if err != nil {
			t.Fatalf("unable to send htlc message: %v", err)
		}

		assertPaymentIndex(t, paymentDB, info.PaymentIdentifier)
		assertDBPaymentstatus(
			t, paymentDB, info.PaymentIdentifier, StatusInitiated,
		)
		assertPaymentInfo(
			t, paymentDB, info.PaymentIdentifier, info, nil, nil,
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

			_, err = paymentDB.RegisterAttempt(
				info.PaymentIdentifier, &a,
			)
			if err != nil {
				t.Fatalf("unable to send htlc message: %v", err)
			}
			assertDBPaymentstatus(
				t, paymentDB, info.PaymentIdentifier,
				StatusInFlight,
			)

			htlc := &htlcStatus{
				HTLCAttemptInfo: &a,
			}
			assertPaymentInfo(
				t, paymentDB, info.PaymentIdentifier, info, nil,
				htlc,
			)
		}

		// For a fourth attempt, check that attempting to
		// register it will fail since the total sent amount
		// will be too large.
		b := *attempt
		b.AttemptID = 3
		_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
		require.ErrorIs(t, err, ErrValueExceedsAmt)

		// Fail the second attempt.
		a := attempts[1]
		htlcFail := HTLCFailUnreadable
		_, err = paymentDB.FailAttempt(
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
			t, paymentDB, info.PaymentIdentifier, info, nil, htlc,
		)

		// Payment should still be in-flight.
		assertDBPaymentstatus(
			t, paymentDB, info.PaymentIdentifier, StatusInFlight,
		)

		// Depending on the test case, settle or fail the first attempt.
		a = attempts[0]
		htlc = &htlcStatus{
			HTLCAttemptInfo: a,
		}

		var firstFailReason *FailureReason
		if test.settleFirst {
			_, err := paymentDB.SettleAttempt(
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
				t, paymentDB, info.PaymentIdentifier, info, nil,
				htlc,
			)
		} else {
			_, err := paymentDB.FailAttempt(
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
				t, paymentDB, info.PaymentIdentifier, info, nil,
				htlc,
			)

			// We also record a payment level fail, to move it into
			// a terminal state.
			failReason := FailureReasonNoRoute
			_, err = paymentDB.Fail(
				info.PaymentIdentifier, failReason,
			)
			if err != nil {
				t.Fatalf("unable to fail payment hash: %v", err)
			}

			// Record the reason we failed the payment, such that
			// we can assert this later in the test.
			firstFailReason = &failReason

			// The payment is now considered pending fail, since
			// there is still an active HTLC.
			assertDBPaymentstatus(
				t, paymentDB, info.PaymentIdentifier,
				StatusInFlight,
			)
		}

		// Try to register yet another attempt. This should fail now
		// that the payment has reached a terminal condition.
		b = *attempt
		b.AttemptID = 3
		_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
		if test.settleFirst {
			require.ErrorIs(
				t, err, ErrPaymentPendingSettled,
			)
		} else {
			require.ErrorIs(
				t, err, ErrPaymentPendingFailed,
			)
		}

		assertDBPaymentstatus(
			t, paymentDB, info.PaymentIdentifier, StatusInFlight,
		)

		// Settle or fail the remaining attempt based on the testcase.
		a = attempts[2]
		htlc = &htlcStatus{
			HTLCAttemptInfo: a,
		}
		if test.settleLast {
			// Settle the last outstanding attempt.
			_, err = paymentDB.SettleAttempt(
				info.PaymentIdentifier, a.AttemptID,
				&HTLCSettleInfo{
					Preimage: preimg,
				},
			)
			require.NoError(t, err, "unable to settle")

			htlc.settle = &preimg
			assertPaymentInfo(
				t, paymentDB, info.PaymentIdentifier,
				info, firstFailReason, htlc,
			)
		} else {
			// Fail the attempt.
			_, err := paymentDB.FailAttempt(
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
				t, paymentDB, info.PaymentIdentifier, info,
				firstFailReason, htlc,
			)

			// Check that we can override any perevious terminal
			// failure. This is to allow multiple concurrent shard
			// write a terminal failure to the database without
			// syncing.
			failReason := FailureReasonPaymentDetails
			_, err = paymentDB.Fail(
				info.PaymentIdentifier, failReason,
			)
			require.NoError(t, err, "unable to fail")
		}

		var (
			finalStatus PaymentStatus
			registerErr error
		)

		switch {
		// If one of the attempts settled but the other failed with
		// terminal error, we would still consider the payment is
		// settled.
		case test.settleFirst && !test.settleLast:
			finalStatus = StatusSucceeded
			registerErr = ErrPaymentAlreadySucceeded

		case !test.settleFirst && test.settleLast:
			finalStatus = StatusSucceeded
			registerErr = ErrPaymentAlreadySucceeded

		// If both failed, we end up in a failed status.
		case !test.settleFirst && !test.settleLast:
			finalStatus = StatusFailed
			registerErr = ErrPaymentAlreadyFailed

		// Otherwise, the payment has a succeed status.
		case test.settleFirst && test.settleLast:
			finalStatus = StatusSucceeded
			registerErr = ErrPaymentAlreadySucceeded
		}

		assertDBPaymentstatus(
			t, paymentDB, info.PaymentIdentifier, finalStatus,
		)

		// Finally assert we cannot register more attempts.
		_, err = paymentDB.RegisterAttempt(info.PaymentIdentifier, &b)
		require.Equal(t, registerErr, err)
	}

	for _, test := range tests {
		subTest := fmt.Sprintf("first=%v, second=%v",
			test.settleFirst, test.settleLast)

		t.Run(subTest, func(t *testing.T) {
			runSubTest(t, test)
		})
	}
}
