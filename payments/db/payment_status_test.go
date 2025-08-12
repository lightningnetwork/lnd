package paymentsdb

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestDecidePaymentStatus checks that given a set of HTLC and a failure
// reason, the payment's current status is returned as expected.
func TestDecidePaymentStatus(t *testing.T) {
	t.Parallel()

	// Create two attempts used for testing.
	inflight := HTLCAttempt{}
	settled := HTLCAttempt{
		Settle: &HTLCSettleInfo{Preimage: lntypes.Preimage{}},
	}
	failed := HTLCAttempt{
		Failure: &HTLCFailInfo{FailureSourceIndex: 1},
	}

	// Create a test failure reason and get the pointer.
	reason := FailureReasonNoRoute
	failure := &reason

	testCases := []struct {
		name           string
		htlcs          []HTLCAttempt
		reason         *FailureReason
		expectedStatus PaymentStatus
		expectedErr    error
	}{
		{
			// Test when inflight=true, settled=true, failed=true,
			// reason=yes.
			name: "state 1111",
			htlcs: []HTLCAttempt{
				inflight, settled, failed,
			},
			reason:         failure,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=true, failed=true,
			// reason=no.
			name: "state 1110",
			htlcs: []HTLCAttempt{
				inflight, settled, failed,
			},
			reason:         nil,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=true, failed=false,
			// reason=yes.
			name:           "state 1101",
			htlcs:          []HTLCAttempt{inflight, settled},
			reason:         failure,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=true, failed=false,
			// reason=no.
			name:           "state 1100",
			htlcs:          []HTLCAttempt{inflight, settled},
			reason:         nil,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=false, failed=true,
			// reason=yes.
			name:           "state 1011",
			htlcs:          []HTLCAttempt{inflight, failed},
			reason:         failure,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=false, failed=true,
			// reason=no.
			name:           "state 1010",
			htlcs:          []HTLCAttempt{inflight, failed},
			reason:         nil,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=false, failed=false,
			// reason=yes.
			name:           "state 1001",
			htlcs:          []HTLCAttempt{inflight},
			reason:         failure,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=true, settled=false, failed=false,
			// reason=no.
			name:           "state 1000",
			htlcs:          []HTLCAttempt{inflight},
			reason:         nil,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=false, settled=true, failed=true,
			// reason=yes.
			name:           "state 0111",
			htlcs:          []HTLCAttempt{settled, failed},
			reason:         failure,
			expectedStatus: StatusSucceeded,
		},
		{
			// Test when inflight=false, settled=true, failed=true,
			// reason=no.
			name:           "state 0110",
			htlcs:          []HTLCAttempt{settled, failed},
			reason:         nil,
			expectedStatus: StatusSucceeded,
		},
		{
			// Test when inflight=false, settled=true,
			// failed=false, reason=yes.
			name:           "state 0101",
			htlcs:          []HTLCAttempt{settled},
			reason:         failure,
			expectedStatus: StatusSucceeded,
		},
		{
			// Test when inflight=false, settled=true,
			// failed=false, reason=no.
			name:           "state 0100",
			htlcs:          []HTLCAttempt{settled},
			reason:         nil,
			expectedStatus: StatusSucceeded,
		},
		{
			// Test when inflight=false, settled=false,
			// failed=true, reason=yes.
			name:           "state 0011",
			htlcs:          []HTLCAttempt{failed},
			reason:         failure,
			expectedStatus: StatusFailed,
		},
		{
			// Test when inflight=false, settled=false,
			// failed=true, reason=no.
			name:           "state 0010",
			htlcs:          []HTLCAttempt{failed},
			reason:         nil,
			expectedStatus: StatusInFlight,
		},
		{
			// Test when inflight=false, settled=false,
			// failed=false, reason=yes.
			name:           "state 0001",
			htlcs:          []HTLCAttempt{},
			reason:         failure,
			expectedStatus: StatusFailed,
		},
		{
			// Test when inflight=false, settled=false,
			// failed=false, reason=no.
			name:           "state 0000",
			htlcs:          []HTLCAttempt{},
			reason:         nil,
			expectedStatus: StatusInitiated,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, err := decidePaymentStatus(tc.htlcs, tc.reason)
			require.Equalf(t, tc.expectedStatus, status,
				"got %s, want %s", status, tc.expectedStatus)
			require.ErrorIs(t, err, tc.expectedErr)
		})
	}
}

// TestPaymentStatusActions checks whether a list of actions can be applied
// against ALL possible payment statuses. Unlike normal unit tests where we
// check against a single function, all the actions including `removable`,
// `initable`, and `updatable` are tested together so this test can be used as
// a reference of state transition.
func TestPaymentStatusActions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		status    PaymentStatus
		initErr   error
		updateErr error
		removeErr error
	}{
		{
			status:    StatusInitiated,
			initErr:   ErrPaymentExists,
			updateErr: nil,
			removeErr: nil,
		},
		{
			status:    StatusInFlight,
			initErr:   ErrPaymentInFlight,
			updateErr: nil,
			removeErr: ErrPaymentInFlight,
		},
		{
			status:    StatusSucceeded,
			initErr:   ErrAlreadyPaid,
			updateErr: ErrPaymentAlreadySucceeded,
			removeErr: nil,
		},
		{
			status:    StatusFailed,
			initErr:   nil,
			updateErr: ErrPaymentAlreadyFailed,
			removeErr: nil,
		},
		{
			status:    0,
			initErr:   ErrUnknownPaymentStatus,
			updateErr: ErrUnknownPaymentStatus,
			removeErr: ErrUnknownPaymentStatus,
		},
	}

	for i, tc := range testCases {
		i, tc := i, tc

		ps := tc.status
		name := fmt.Sprintf("test_%d_%s", i, ps.String())
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, ps.initializable(), tc.initErr,
				"initable under state %v", tc.status)

			require.ErrorIs(t, ps.updatable(), tc.updateErr,
				"updatable under state %v", tc.status)

			require.ErrorIs(t, ps.removable(), tc.removeErr,
				"removable under state %v", tc.status)
		})
	}
}
