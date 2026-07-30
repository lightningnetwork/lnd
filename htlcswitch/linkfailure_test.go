package htlcswitch

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestLinkFailureForDBErr tests that errors originating from our own database
// are mapped onto a link failure that is neither reported to our peer nor
// causes a force close, while all other errors keep the failure the caller
// asked for.
func TestLinkFailureForDBErr(t *testing.T) {
	t.Parallel()

	// serializationErr mimics the error postgres hands us when a
	// transaction couldn't be serialized against the other concurrent
	// transactions.
	serializationErr := sqldb.MapSQLError(errors.New("ERROR: could not " +
		"serialize access due to read/write dependencies among " +
		"transactions (SQLSTATE 40001)"))

	// The default failure is the one the revocation path used to always
	// use. It both reports an error to the peer and asks for a disconnect.
	defaultFailure := LinkFailureError{
		code:          ErrInvalidRevocation,
		FailureAction: LinkFailureDisconnect,
	}

	tests := []struct {
		name string
		err  error

		// expCode is the error code we expect the resulting failure to
		// carry.
		expCode errorCode

		// expSendToPeer is whether we expect the resulting failure to
		// be reported to our peer on the wire.
		expSendToPeer bool
	}{
		{
			name:          "peer error is left alone",
			err:           errors.New("revocation key mismatch"),
			expCode:       ErrInvalidRevocation,
			expSendToPeer: true,
		},
		{
			name:          "serialization error",
			err:           serializationErr,
			expCode:       ErrInternalDBError,
			expSendToPeer: false,
		},
		{
			name: "wrapped serialization error",
			err: fmt.Errorf("unable to restore remote unsigned "+
				"local updates: %w", serializationErr),
			expCode:       ErrInternalDBError,
			expSendToPeer: false,
		},
		{
			name:          "retries exceeded",
			err:           sqldb.ErrRetriesExceeded,
			expCode:       ErrInternalDBError,
			expSendToPeer: false,
		},
		{
			name: "wrapped retries exceeded",
			err: fmt.Errorf("unable to accept revocation: %w",
				fmt.Errorf("%w: %w", sqldb.ErrRetriesExceeded,
					serializationErr)),
			expCode:       ErrInternalDBError,
			expSendToPeer: false,
		},
		{
			name: "canceled retry",
			err: fmt.Errorf("%w: %w", sqldb.ErrRetryCanceled,
				serializationErr),
			expCode:       ErrInternalDBError,
			expSendToPeer: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			failure := linkFailureForDBErr(test.err, defaultFailure)

			require.Equal(t, test.expCode, failure.code)
			require.Equal(
				t, test.expSendToPeer,
				failure.ShouldSendToPeer(),
			)

			// No matter the error, we never want to force close
			// here, and we always want the connection recycled.
			require.NotEqual(
				t, LinkFailureForceClose,
				failure.FailureAction,
			)
			require.Equal(
				t, LinkFailureDisconnect, failure.FailureAction,
			)
			require.False(t, failure.PermanentFailure)
		})
	}
}

// TestDBErrLinkFailureIsSilent asserts that the failure we use for database
// errors is never reported to our peer and never force closes the channel.
func TestDBErrLinkFailureIsSilent(t *testing.T) {
	t.Parallel()

	require.False(t, dbErrLinkFailure.ShouldSendToPeer())
	require.False(t, dbErrLinkFailure.PermanentFailure)
	require.Equal(
		t, LinkFailureDisconnect, dbErrLinkFailure.FailureAction,
	)
	require.Equal(t, "internal database error", dbErrLinkFailure.Error())
}

// TestDBFailureTracker tests that we only escalate once several database caused
// link failures happen close enough together, and that a quiet spell resets the
// count.
func TestDBFailureTracker(t *testing.T) {
	t.Parallel()

	var tracker dbFailureTracker
	now := time.Now()

	// A burst of failures within the window accumulates.
	require.Equal(t, 1, tracker.record(now))
	require.Equal(t, 2, tracker.record(now.Add(time.Second)))
	require.Equal(t, 3, tracker.record(now.Add(2*time.Second)))

	// A failure that arrives after the window has passed is unrelated, so
	// we start counting from scratch. Note that this must land below the
	// escalation threshold, otherwise a healthy node that hits one such
	// error a month would eventually escalate.
	late := now.Add(2*time.Second + dbFailureWindow + time.Second)
	require.Equal(t, 1, tracker.record(late))
	require.Less(t, 1, dbFailureEscalation)

	// Failures within the window of that one accumulate again.
	require.Equal(t, 2, tracker.record(late.Add(time.Second)))
}
