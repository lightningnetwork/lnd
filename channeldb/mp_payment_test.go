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

// TestRegistrable checks whether a list of actions can be applied
// against ALL possible payment statuses. Unlike normal unit tests where we
// check against a single function, all the actions including `removable`,
// `initable`, `registrable` and `updatable` are tested together so this test
// can be used as a reference of state transition.
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

	// Create test objects.
	reason := FailureReasonError
	htlcSettled := HTLCAttempt{
		Settle: &HTLCSettleInfo{},
	}

	for i, tc := range testCases {
		i, tc := i, tc

		p := &MPPayment{
			Status: tc.status,
		}

		// Add the settled htlc to the payment if needed.
		htlcs := make([]HTLCAttempt, 0)
		if tc.hasSettledHTLC {
			htlcs = append(htlcs, htlcSettled)
		}
		p.HTLCs = htlcs

		// Add the failure reason if needed.
		if tc.paymentFailed {
			p.FailureReason = &reason
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

// TestUpdatePaymentState checks that the method updateState updates the
// MPPaymentState as expected.
func TestUpdatePaymentState(t *testing.T) {
	t.Parallel()

	// paymentHash is the identifier on paymentLifecycle.
	preimage := lntypes.Preimage{}
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
			// Test that when the htlc is failed, the fee is used.
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
				Terminate:           false,
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
				Terminate:           true,
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
				Terminate:           true,
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
			err := tc.payment.updateState()
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
