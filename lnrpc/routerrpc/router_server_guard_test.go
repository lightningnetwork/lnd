package routerrpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCheckLocalSendAllowed verifies the external payment lifecycle guard: local
// payment sending is refused with codes.FailedPrecondition when an external router owns
// the payment lifecycle, and permitted otherwise.
func TestCheckLocalSendAllowed(t *testing.T) {
	t.Parallel()

	// Guard inactive: local sending is allowed.
	allowed := &Server{cfg: &Config{EnableLocalPaymentDispatch: true}}
	require.NoError(t, allowed.checkLocalSendAllowed())

	// Guard active: local sending is refused with codes.FailedPrecondition so an
	// operator reading the error learns the reason without reading source.
	refused := &Server{cfg: &Config{EnableLocalPaymentDispatch: false}}
	err := refused.checkLocalSendAllowed()
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

// TestExternalLifecycleGuardsHandlers verifies the guard is wired into both
// payment-sending handlers, so neither can dispatch a local payment while
// external lifecycle mode is active. Both return before touching the router, so
// a minimal Server with no backend suffices.
func TestExternalLifecycleGuardsHandlers(t *testing.T) {
	t.Parallel()

	srv := &Server{cfg: &Config{EnableLocalPaymentDispatch: false}}

	// SendPaymentV2 is server-streaming.
	streamErr := srv.SendPaymentV2(
		&SendPaymentRequest{}, makeStreamMock(context.Background()),
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(streamErr))

	// SendToRouteV2 is unary. The guard runs before the nil-route check,
	// so an empty request still returns the guard error.
	_, unaryErr := srv.SendToRouteV2(
		context.Background(), &SendToRouteRequest{},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(unaryErr))
}
