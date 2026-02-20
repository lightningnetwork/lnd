package routing

import (
	"testing"

	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestResumePaymentsRemoteModeNoInflight tests that resumePayments returns nil
// and skips all resume logic when ExternalPaymentLifecycle is true and there
// are no in-flight payments.
func TestResumePaymentsRemoteModeNoInflight(t *testing.T) {
	t.Parallel()

	controlTower := &mockControlTower{}
	controlTower.On(
		"FetchInFlightPayments",
	).Return([]*paymentsdb.MPPayment{}, nil)

	router := &ChannelRouter{cfg: &Config{
		Control:                  controlTower,
		ExternalPaymentLifecycle: true,
	}}

	err := router.resumePayments()
	require.NoError(t, err)

	// Verify FetchInFlightPayments was called exactly once.
	controlTower.AssertExpectations(t)
}

// TestResumePaymentsRemoteModeWithInflight tests that resumePayments returns
// an error when ExternalPaymentLifecycle is true and there are in-flight
// payments from a previous local-router session.
func TestResumePaymentsRemoteModeWithInflight(t *testing.T) {
	t.Parallel()

	controlTower := &mockControlTower{}
	controlTower.On(
		"FetchInFlightPayments",
	).Return([]*paymentsdb.MPPayment{
		{},
		{},
	}, nil)

	router := &ChannelRouter{cfg: &Config{
		Control:                  controlTower,
		ExternalPaymentLifecycle: true,
	}}

	err := router.resumePayments()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot start with switchrpc")
	require.Contains(t, err.Error(), "2 in-flight payment(s)")

	controlTower.AssertExpectations(t)
}

// TestResumePaymentsLocalMode tests that resumePayments calls CleanStore and
// proceeds normally when ExternalPaymentLifecycle is false.
func TestResumePaymentsLocalMode(t *testing.T) {
	t.Parallel()

	controlTower := &mockControlTower{}
	payer := &mockPaymentAttemptDispatcher{}

	controlTower.On(
		"FetchInFlightPayments",
	).Return([]*paymentsdb.MPPayment{}, nil)

	payer.On(
		"CleanStore", mock.Anything,
	).Return(nil)

	router := &ChannelRouter{cfg: &Config{
		Control:                  controlTower,
		Payer:                    payer,
		ExternalPaymentLifecycle: false,
	}}

	err := router.resumePayments()
	require.NoError(t, err)

	// Verify CleanStore was called (only happens in local mode).
	controlTower.AssertExpectations(t)
	payer.AssertExpectations(t)
}
