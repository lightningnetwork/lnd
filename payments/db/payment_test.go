package paymentsdb

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
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

	testHop3 = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		CustomRecords: record.CustomSet{
			65536: []byte{},
			80001: []byte{},
		},
		AMP:      record.NewAMP([32]byte{0x69}, [32]byte{0x42}, 1),
		Metadata: []byte{1, 2, 3},
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

// assertRouteEquals compares to routes for equality and returns an error if
// they are not equal.
func assertRouteEqual(a, b *route.Route) error {
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("HTLCAttemptInfos don't match: %v vs %v",
			spew.Sdump(a), spew.Sdump(b))
	}

	return nil
}

// TestQueryPayments tests retrieval of payments with forwards and reversed
// queries.
func TestQueryPayments(t *testing.T) {
	// Define table driven test for QueryPayments.
	// Test payments have sequence indices [1, 3, 4, 5, 6, 7].
	// Note that the payment with index 7 has the same payment hash as 6,
	// and is stored in a nested bucket within payment 6 rather than being
	// its own entry in the payments bucket. We do this to test retrieval
	// of legacy payments.
	tests := []struct {
		name       string
		query      PaymentsQuery
		firstIndex uint64
		lastIndex  uint64

		// expectedSeqNrs contains the set of sequence numbers we expect
		// our query to return.
		expectedSeqNrs []uint64
	}{
		{
			name: "IndexOffset at the end of the payments range",
			query: PaymentsQuery{
				IndexOffset:       7,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     0,
			lastIndex:      0,
			expectedSeqNrs: nil,
		},
		{
			name: "query in forwards order, start at beginning",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "query in forwards order, start at end, overflow",
			query: PaymentsQuery{
				IndexOffset:       6,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "start at offset index outside of payments",
			query: PaymentsQuery{
				IndexOffset:       20,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     0,
			lastIndex:      0,
			expectedSeqNrs: nil,
		},
		{
			name: "overflow in forwards order",
			query: PaymentsQuery{
				IndexOffset:       4,
				MaxPayments:       math.MaxUint64,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     5,
			lastIndex:      7,
			expectedSeqNrs: []uint64{5, 6, 7},
		},
		{
			name: "start at offset index outside of payments, " +
				"reversed order",
			query: PaymentsQuery{
				IndexOffset:       9,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     6,
			lastIndex:      7,
			expectedSeqNrs: []uint64{6, 7},
		},
		{
			name: "query in reverse order, start at end",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     6,
			lastIndex:      7,
			expectedSeqNrs: []uint64{6, 7},
		},
		{
			name: "query in reverse order, starting in middle",
			query: PaymentsQuery{
				IndexOffset:       4,
				MaxPayments:       2,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "query in reverse order, starting in middle, " +
				"with underflow",
			query: PaymentsQuery{
				IndexOffset:       4,
				MaxPayments:       5,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      3,
			expectedSeqNrs: []uint64{1, 3},
		},
		{
			name: "all payments in reverse, order maintained",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      7,
			expectedSeqNrs: []uint64{1, 3, 4, 5, 6, 7},
		},
		{
			name: "exclude incomplete payments",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: false,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "query payments at index gap",
			query: PaymentsQuery{
				IndexOffset:       1,
				MaxPayments:       7,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     3,
			lastIndex:      7,
			expectedSeqNrs: []uint64{3, 4, 5, 6, 7},
		},
		{
			name: "query payments reverse before index gap",
			query: PaymentsQuery{
				IndexOffset:       3,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      1,
			expectedSeqNrs: []uint64{1},
		},
		{
			name: "query payments reverse on index gap",
			query: PaymentsQuery{
				IndexOffset:       2,
				MaxPayments:       7,
				Reversed:          true,
				IncludeIncomplete: true,
			},
			firstIndex:     1,
			lastIndex:      1,
			expectedSeqNrs: []uint64{1},
		},
		{
			name: "query payments forward on index gap",
			query: PaymentsQuery{
				IndexOffset:       2,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
			},
			firstIndex:     3,
			lastIndex:      4,
			expectedSeqNrs: []uint64{3, 4},
		},
		{
			name: "query in forwards order, with start creation " +
				"time",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
				CreationDateStart: 5,
			},
			firstIndex:     5,
			lastIndex:      6,
			expectedSeqNrs: []uint64{5, 6},
		},
		{
			name: "query in forwards order, with start creation " +
				"time at end, overflow",
			query: PaymentsQuery{
				IndexOffset:       0,
				MaxPayments:       2,
				Reversed:          false,
				IncludeIncomplete: true,
				CreationDateStart: 7,
			},
			firstIndex:     7,
			lastIndex:      7,
			expectedSeqNrs: []uint64{7},
		},
		{
			name: "query with start and end creation time",
			query: PaymentsQuery{
				IndexOffset:       9,
				MaxPayments:       math.MaxUint64,
				Reversed:          true,
				IncludeIncomplete: true,
				CreationDateStart: 3,
				CreationDateEnd:   5,
			},
			firstIndex:     3,
			lastIndex:      5,
			expectedSeqNrs: []uint64{3, 4, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()

			paymentDB := NewTestDB(t)

			// Initialize the payment database.
			paymentDB, err := NewKVPaymentsDB(paymentDB.db)
			require.NoError(t, err)

			// Make a preliminary query to make sure it's ok to
			// query when we have no payments.
			resp, err := paymentDB.QueryPayments(ctx, tt.query)
			require.NoError(t, err)
			require.Len(t, resp.Payments, 0)

			// Populate the database with a set of test payments.
			// We create 6 original payments, deleting the payment
			// at index 2 so that we cover the case where sequence
			// numbers are missing. We also add a duplicate payment
			// to the last payment added to test the legacy case
			// where we have duplicates in the nested duplicates
			// bucket.
			nonDuplicatePayments := 6

			for i := 0; i < nonDuplicatePayments; i++ {
				// Generate a test payment.
				info, _, preimg, err := genInfo(t)
				if err != nil {
					t.Fatalf("unable to create test "+
						"payment: %v", err)
				}
				// Override creation time to allow for testing
				// of CreationDateStart and CreationDateEnd.
				info.CreationTime = time.Unix(int64(i+1), 0)

				// Create a new payment entry in the database.
				err = paymentDB.InitPayment(
					info.PaymentIdentifier, info,
				)
				require.NoError(t, err)

				// Immediately delete the payment with index 2.
				if i == 1 {
					pmt, err := paymentDB.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					deletePayment(
						t, paymentDB.db,
						info.PaymentIdentifier,
						pmt.SequenceNum,
					)
				}

				// If we are on the last payment entry, add a
				// duplicate payment with sequence number equal
				// to the parent payment + 1. Note that
				// duplicate payments will always be succeeded.
				if i == (nonDuplicatePayments - 1) {
					pmt, err := paymentDB.FetchPayment(
						info.PaymentIdentifier,
					)
					require.NoError(t, err)

					appendDuplicatePayment(
						t, paymentDB.db,
						info.PaymentIdentifier,
						pmt.SequenceNum+1,
						preimg,
					)
				}
			}

			// Fetch all payments in the database.
			allPayments, err := paymentDB.FetchPayments()
			if err != nil {
				t.Fatalf("payments could not be fetched from "+
					"database: %v", err)
			}

			if len(allPayments) != 6 {
				t.Fatalf("Number of payments received does "+
					"not match expected one. Got %v, "+
					"want %v.", len(allPayments), 6)
			}

			querySlice, err := paymentDB.QueryPayments(
				ctx, tt.query,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.firstIndex != querySlice.FirstIndexOffset ||
				tt.lastIndex != querySlice.LastIndexOffset {

				t.Errorf("First or last index does not match "+
					"expected index. Want (%d, %d), "+
					"got (%d, %d).",
					tt.firstIndex, tt.lastIndex,
					querySlice.FirstIndexOffset,
					querySlice.LastIndexOffset)
			}

			if len(querySlice.Payments) != len(tt.expectedSeqNrs) {
				t.Errorf("expected: %v payments, got: %v",
					len(tt.expectedSeqNrs),
					len(querySlice.Payments))
			}

			for i, seqNr := range tt.expectedSeqNrs {
				q := querySlice.Payments[i]
				if seqNr != q.SequenceNum {
					t.Errorf("sequence numbers do not "+
						"match, got %v, want %v",
						q.SequenceNum, seqNr)
				}
			}
		})
	}
}

// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
func TestLazySessionKeyDeserialize(t *testing.T) {
	var b bytes.Buffer

	// Serialize as a private key.
	err := WriteElements(&b, priv)
	require.NoError(t, err)

	// Deserialize into [btcec.PrivKeyBytesLen]byte.
	attempt := HTLCAttemptInfo{}
	err = ReadElements(&b, &attempt.sessionKey)
	require.NoError(t, err)
	require.Zero(t, b.Len())

	sessionKey := attempt.SessionKey()
	require.Equal(t, priv, sessionKey)
}

// TestRegistrable checks the method `Registrable` behaves as expected for ALL
// possible payment statuses.
func TestRegistrable(t *testing.T) {
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
	failureReasonError := channeldb.FailureReasonError

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
			info := &channeldb.PaymentCreationInfo{
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
			Info: &channeldb.PaymentCreationInfo{
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
			Info: &channeldb.PaymentCreationInfo{
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
