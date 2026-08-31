package lnwallet

import (
	"testing"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestHandleFundingCounterPartySigsMissingReservation tests the missing
// reservation response.
func TestHandleFundingCounterPartySigsMissingReservation(t *testing.T) {
	t.Parallel()

	wallet := &LightningWallet{
		fundingLimbo: make(map[uint64]*ChannelReservation),
	}
	promise := actor.NewPromise[fn.Result[*chanstate.OpenChannel]]()

	wallet.handleFundingCounterPartySigs(&addCounterPartySigsMsg{
		openChanRequest:  openChanRequest{resp: promise},
		pendingFundingID: 1,
	})

	channel, err := awaitWalletResult(promise.Future()).Unpack()
	require.Nil(t, channel)
	require.ErrorContains(t, err, "non-existent funding state")
}

// TestReservationErrorTypePreserved asserts that the promises the wallet
// answers its requests with hand concrete error types back to callers
// untouched. funding.Manager.failFundingFlow type switches on
// lnwallet.ReservationError to decide whether an error may be forwarded to the
// remote peer, so any wrapping along the way would silently change that
// decision.
func TestReservationErrorTypePreserved(t *testing.T) {
	t.Parallel()

	// forwarded runs the same type switch failFundingFlow does, returning
	// the error text the remote peer would be told about.
	forwarded := func(err error) string {
		// NOTE: failFundingFlow uses a plain type switch rather than
		// errors.As, so we deliberately mirror that here. Switching
		// this to errors.As would let a wrapped error slip past the
		// test while still breaking the real decision.
		//
		//nolint:errorlint
		switch e := err.(type) {
		case ReservationError:
			return e.Error()
		default:
			t.Fatalf("error is %T, not a ReservationError", err)
			return ""
		}
	}

	// The reservation initiation path answers with a result carrying the
	// new reservation.
	wallet := &LightningWallet{}
	initPromise := actor.NewPromise[fn.Result[*ChannelReservation]]()
	wallet.handleFundingReserveRequest(&InitFundingReserveMsg{
		resp: initPromise,
	})

	reservation, initErr := awaitWalletResult(
		initPromise.Future(),
	).Unpack()
	require.Nil(t, reservation)
	require.Equal(t, ErrZeroCapacity().Error(), forwarded(initErr))

	// The single contribution path, which is where the initial balance
	// check lives, answers with a bare error instead.
	balanceErr := ErrBalancesBelowReserve(0, 1, 0, 1)
	req := &addSingleContributionMsg{
		errRequest: errRequest{resp: actor.NewPromise[error]()},
	}
	req.complete(balanceErr)

	require.Equal(
		t, balanceErr.Error(),
		forwarded(awaitWalletResult(req.resp.Future())),
	)
}

// TestRegisterFundingIntent checks RegisterFundingIntent behaves as expected.
func TestRegisterFundingIntent(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a testing wallet.
	lw, err := NewLightningWallet(Config{})
	require.NoError(err)

	// Init an empty testing channel ID.
	var testID [32]byte

	// Call the method with empty ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrEmptyPendingChanID)

	// Modify the ID and call the method again should result in no error.
	testID[0] = 1
	err = lw.RegisterFundingIntent(testID, nil)
	require.NoError(err)

	// Call the method using the same ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrDuplicatePendingChanID)
}
