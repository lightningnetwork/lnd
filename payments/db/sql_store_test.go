//go:build test_db_sqlite || test_db_postgres

package paymentsdb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestComputePaymentStatus tests the SQL to domain type conversion logic in
// computePaymentStatusFromResolutions. This is a pure unit test with no
// database interaction. However the function is only used in the SQL store and
// used sql data types so we test it in a sql specific file.
func TestComputePaymentStatus(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		resolutionTypes []sql.NullInt32
		failReason      sql.NullInt32
		expectedStatus  PaymentStatus
		expectError     bool
	}{
		{
			name: "all NULL resolutions means in-flight",
			resolutionTypes: []sql.NullInt32{
				{Valid: false}, // NULL = in-flight
				{Valid: false},
			},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusInFlight,
		},
		{
			name: "settled resolution without fail reason",
			resolutionTypes: []sql.NullInt32{{
				Int32: int32(HTLCAttemptResolutionSettled),
				Valid: true,
			}},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusSucceeded,
		},
		{
			name: "failed resolution without fail reason",
			resolutionTypes: []sql.NullInt32{{
				Int32: int32(HTLCAttemptResolutionFailed),
				Valid: true,
			}},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusInFlight,
		},
		{
			name: "failed resolution with fail reason",
			resolutionTypes: []sql.NullInt32{{
				Int32: int32(HTLCAttemptResolutionFailed),
				Valid: true,
			}},
			failReason: sql.NullInt32{
				Int32: int32(FailureReasonNoRoute),
				Valid: true,
			},
			expectedStatus: StatusFailed,
		},
		{
			name: "mixed: in-flight and settled",
			resolutionTypes: []sql.NullInt32{
				{Valid: false}, // in-flight
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
			},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusInFlight,
		},
		{
			name: "mixed: in-flight and failed",
			resolutionTypes: []sql.NullInt32{
				{Valid: false}, // in-flight
				{
					Int32: int32(
						HTLCAttemptResolutionFailed,
					),
					Valid: true,
				},
			},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusInFlight,
		},
		{
			name: "mixed: settled and failed",
			resolutionTypes: []sql.NullInt32{
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
				{
					Int32: int32(
						HTLCAttemptResolutionFailed,
					),
					Valid: true,
				},
			},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusSucceeded,
		},
		{
			name: "no resolutions, no fail reason, " +
				"means initiated",
			resolutionTypes: []sql.NullInt32{},
			failReason:      sql.NullInt32{Valid: false},
			expectedStatus:  StatusInitiated,
		},
		{
			name: "no resolutions with fail reason, " +
				"means failed",
			resolutionTypes: []sql.NullInt32{},
			failReason: sql.NullInt32{
				Int32: int32(FailureReasonNoRoute),
				Valid: true,
			},
			expectedStatus: StatusFailed,
		},
		{
			name: "unknown resolution type returns error",
			resolutionTypes: []sql.NullInt32{
				{Int32: 999, Valid: true}, // invalid type
			},
			failReason:  sql.NullInt32{Valid: false},
			expectError: true,
		},
		{
			name: "all three states: in-flight, settled, failed",
			resolutionTypes: []sql.NullInt32{
				{
					Valid: false, // in-flight
				},
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
				{
					Int32: int32(
						HTLCAttemptResolutionFailed,
					),
					Valid: true,
				},
			},
			failReason: sql.NullInt32{
				Int32: int32(FailureReasonTimeout),
				Valid: true,
			},
			expectedStatus: StatusInFlight,
		},
		{
			name: "multiple settled HTLCs",
			resolutionTypes: []sql.NullInt32{
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
				{
					Int32: int32(
						HTLCAttemptResolutionSettled,
					),
					Valid: true,
				},
			},
			failReason:     sql.NullInt32{Valid: false},
			expectedStatus: StatusSucceeded,
		},
		{
			name: "multiple failed HTLCs with fail reason",
			resolutionTypes: []sql.NullInt32{
				{
					Int32: int32(
						HTLCAttemptResolutionFailed,
					),
					Valid: true,
				},
				{
					Int32: int32(
						HTLCAttemptResolutionFailed,
					),
					Valid: true,
				},
			},
			failReason: sql.NullInt32{
				Int32: int32(FailureReasonNoRoute),
				Valid: true,
			},
			expectedStatus: StatusFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status, err := computePaymentStatusFromResolutions(
				tc.resolutionTypes, tc.failReason,
			)

			if tc.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedStatus, status,
				"got %s, want %s", status, tc.expectedStatus)
		})
	}
}
