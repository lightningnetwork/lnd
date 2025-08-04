package channeldb

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
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
)

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
			registryErr:    paymentsdb.ErrPaymentPendingSettled,
			hasSettledHTLC: true,
		},
		{
			// Test inflight status with no settled HTLC but failed
			// payment.
			status:        StatusInFlight,
			registryErr:   paymentsdb.ErrPaymentPendingFailed,
			paymentFailed: true,
		},
		{
			// Test error state with settled HTLC and failed
			// payment.
			status:         0,
			registryErr:    paymentsdb.ErrUnknownPaymentStatus,
			hasSettledHTLC: true,
			paymentFailed:  true,
		},
		{
			status:      StatusSucceeded,
			registryErr: paymentsdb.ErrPaymentAlreadySucceeded,
		},
		{
			status:      StatusFailed,
			registryErr: paymentsdb.ErrPaymentAlreadyFailed,
		},
		{
			status:      0,
			registryErr: paymentsdb.ErrUnknownPaymentStatus,
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
			errExpected: paymentsdb.ErrSentExceedsTotal,
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
			expectedErr:  paymentsdb.ErrPaymentInternal,
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
			expectedErr:  paymentsdb.ErrPaymentInternal,
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
			expectedErr:  paymentsdb.ErrPaymentInternal,
		},
		{
			// Payment is in an unknown status, return an error.
			status:       0,
			remainingAmt: 0,
			needWait:     false,
			expectedErr:  paymentsdb.ErrUnknownPaymentStatus,
		},
		{
			// Payment is in an unknown status, return an error.
			status:       0,
			remainingAmt: 1000,
			needWait:     false,
			expectedErr:  paymentsdb.ErrUnknownPaymentStatus,
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
			expectedErr:  paymentsdb.ErrPaymentInternal,
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
			expectedErr:    paymentsdb.ErrPaymentInternal,
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
