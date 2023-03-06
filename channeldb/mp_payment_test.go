package channeldb

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
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
