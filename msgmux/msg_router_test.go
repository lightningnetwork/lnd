package msgmux

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type mockEndpoint struct {
	mock.Mock
}

func (m *mockEndpoint) Name() string {
	args := m.Called()

	return args.String(0)
}

func (m *mockEndpoint) CanHandle(msg PeerMsg) bool {
	args := m.Called(msg)

	return args.Bool(0)
}

func (m *mockEndpoint) SendMessage(ctx context.Context, msg PeerMsg) bool {
	args := m.Called(ctx, msg)

	return args.Bool(0)
}

// TestMessageRouterOperation tests the basic operation of the message router:
// add new endpoints, route to them, remove, them, etc.
func TestMessageRouterOperation(t *testing.T) {
	ctx := t.Context()
	msgRouter := NewMultiMsgRouter()
	msgRouter.Start(ctx)
	defer msgRouter.Stop()

	openChanMsg := PeerMsg{
		Message: &lnwire.OpenChannel{},
	}
	commitSigMsg := PeerMsg{
		Message: &lnwire.CommitSig{},
	}

	errorMsg := PeerMsg{
		Message: &lnwire.Error{},
	}

	// For this test, we'll have two endpoints, each with distinct names.
	// One endpoint will only handle OpenChannel, while the other will
	// handle the CommitSig message.
	fundingEndpoint := &mockEndpoint{}
	fundingEndpointName := "funding"
	fundingEndpoint.On("Name").Return(fundingEndpointName)
	fundingEndpoint.On("CanHandle", openChanMsg).Return(true)
	fundingEndpoint.On("CanHandle", errorMsg).Return(false)
	fundingEndpoint.On("CanHandle", commitSigMsg).Return(false)
	fundingEndpoint.On("SendMessage", ctx, openChanMsg).Return(true)

	commitEndpoint := &mockEndpoint{}
	commitEndpointName := "commit"
	commitEndpoint.On("Name").Return(commitEndpointName)
	commitEndpoint.On("CanHandle", commitSigMsg).Return(true)
	commitEndpoint.On("CanHandle", openChanMsg).Return(false)
	commitEndpoint.On("CanHandle", errorMsg).Return(false)
	commitEndpoint.On("SendMessage", ctx, commitSigMsg).Return(true)

	t.Run("add endpoints", func(t *testing.T) {
		// First, we'll add the funding endpoint to the router.
		require.NoError(t, msgRouter.RegisterEndpoint(fundingEndpoint))

		endpoints, err := msgRouter.endpoints().Unpack()
		require.NoError(t, err)

		// There should be a single endpoint registered.
		require.Len(t, endpoints, 1)

		// The name of the registered endpoint should be "funding".
		require.Equal(
			t, "funding", endpoints[fundingEndpointName].Name(),
		)
	})

	t.Run("duplicate endpoint reject", func(t *testing.T) {
		// Next, we'll attempt to add the funding endpoint again. This
		// should return an ErrDuplicateEndpoint error.
		require.ErrorIs(
			t, msgRouter.RegisterEndpoint(fundingEndpoint),
			ErrDuplicateEndpoint,
		)
	})

	t.Run("route to endpoint", func(t *testing.T) {
		// Next, we'll add our other endpoint, then attempt to route a
		// message.
		require.NoError(t, msgRouter.RegisterEndpoint(commitEndpoint))

		// If we try to route a message none of the endpoints know of,
		// we should get an error.
		require.ErrorIs(
			t, msgRouter.RouteMsg(errorMsg), ErrUnableToRouteMsg,
		)

		fundingEndpoint.AssertCalled(t, "CanHandle", errorMsg)
		commitEndpoint.AssertCalled(t, "CanHandle", errorMsg)

		// Next, we'll route the open channel message. Only the
		// fundingEndpoint should be used.
		require.NoError(t, msgRouter.RouteMsg(openChanMsg))

		fundingEndpoint.AssertCalled(t, "CanHandle", openChanMsg)
		commitEndpoint.AssertCalled(t, "CanHandle", openChanMsg)

		fundingEndpoint.AssertCalled(t, "SendMessage", ctx, openChanMsg)
		commitEndpoint.AssertNotCalled(
			t, "SendMessage", ctx, openChanMsg,
		)

		// We'll do the same for the commit sig message.
		require.NoError(t, msgRouter.RouteMsg(commitSigMsg))

		fundingEndpoint.AssertCalled(t, "CanHandle", commitSigMsg)
		commitEndpoint.AssertCalled(t, "CanHandle", commitSigMsg)

		commitEndpoint.AssertCalled(t, "SendMessage", ctx, commitSigMsg)
		fundingEndpoint.AssertNotCalled(
			t, "SendMessage", ctx, commitSigMsg,
		)
	})

	t.Run("remove endpoints", func(t *testing.T) {
		// Finally, we'll remove both endpoints.
		require.NoError(
			t, msgRouter.UnregisterEndpoint(fundingEndpointName),
		)
		require.NoError(
			t, msgRouter.UnregisterEndpoint(commitEndpointName),
		)

		endpoints, err := msgRouter.endpoints().Unpack()
		require.NoError(t, err)

		// There should be no endpoints registered.
		require.Len(t, endpoints, 0)

		// Trying to route a message should fail.
		require.ErrorIs(
			t, msgRouter.RouteMsg(openChanMsg),
			ErrUnableToRouteMsg,
		)
		require.ErrorIs(
			t, msgRouter.RouteMsg(commitSigMsg),
			ErrUnableToRouteMsg,
		)
	})

	commitEndpoint.AssertExpectations(t)
	fundingEndpoint.AssertExpectations(t)
}

// panicEndpoint is an endpoint that panics from whichever of the two
// message-facing methods it was told to.
type panicEndpoint struct {
	name          string
	panicOnHandle bool
	panicOnSend   bool
}

func (p *panicEndpoint) Name() EndpointName {
	return p.name
}

func (p *panicEndpoint) CanHandle(msg PeerMsg) bool {
	if p.panicOnHandle {
		panic("panic from CanHandle")
	}

	return true
}

func (p *panicEndpoint) SendMessage(ctx context.Context, msg PeerMsg) bool {
	if p.panicOnSend {
		panic("panic from SendMessage")
	}

	return true
}

// partialDeliveryPanicEndpoint records the message as delivered before it
// panics. It models an endpoint that applies a side effect and then fails
// before it can report successful delivery to the router.
type partialDeliveryPanicEndpoint struct {
	delivered chan PeerMsg
}

func (p *partialDeliveryPanicEndpoint) Name() EndpointName {
	return "partial-delivery"
}

func (p *partialDeliveryPanicEndpoint) CanHandle(PeerMsg) bool {
	return true
}

func (p *partialDeliveryPanicEndpoint) SendMessage(_ context.Context,
	msg PeerMsg) bool {

	p.delivered <- msg
	panic("panic after delivery")
}

// TestMessageRouterEndpointPanic verifies that an endpoint panic returns an
// error and that later routing requests are processed.
func TestMessageRouterEndpointPanic(t *testing.T) {
	t.Parallel()

	// Run RouteMsg with a timeout so a missing response fails the test.
	routeWithTimeout := func(t *testing.T, r *MultiMsgRouter,
		msg PeerMsg) error {

		t.Helper()

		errChan := make(chan error, 1)
		go func() {
			errChan <- r.RouteMsg(msg)
		}()

		select {
		case err := <-errChan:
			return err

		case <-time.After(10 * time.Second):
			t.Fatal("RouteMsg never returned: the router failed " +
				"to resolve the caller's request")

			return nil
		}
	}

	tests := []struct {
		name     string
		endpoint *panicEndpoint
	}{
		{
			name: "panic in CanHandle",
			endpoint: &panicEndpoint{
				name:          "panic-handle",
				panicOnHandle: true,
			},
		},
		{
			name: "panic in SendMessage",
			endpoint: &panicEndpoint{
				name:        "panic-send",
				panicOnSend: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			msgRouter := NewMultiMsgRouter()
			msgRouter.Start(ctx)
			defer msgRouter.Stop()

			require.NoError(
				t, msgRouter.RegisterEndpoint(test.endpoint),
			)

			// Routing to the panicking endpoint should return a
			// routing error.
			err := routeWithTimeout(t, msgRouter, PeerMsg{
				Message: &lnwire.OpenChannel{},
			})
			require.ErrorIs(t, err, ErrRoutePanic)

			// A second route verifies that the router remains
			// available.
			require.NoError(t, msgRouter.UnregisterEndpoint(
				test.endpoint.Name(),
			))
			require.NoError(t, msgRouter.RegisterEndpoint(
				&panicEndpoint{name: "healthy"},
			))

			err = routeWithTimeout(t, msgRouter, PeerMsg{
				Message: &lnwire.CommitSig{},
			})
			require.NoError(t, err)
		})
	}
}

// TestMessageRouterPartialDeliveryPanic asserts that ErrRoutePanic does not
// claim that delivery failed. An endpoint may apply the message before it
// panics, so callers must treat delivery as unknown and must not replay it.
func TestMessageRouterPartialDeliveryPanic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	msgRouter := NewMultiMsgRouter()
	msgRouter.Start(ctx)
	defer msgRouter.Stop()

	endpoint := &partialDeliveryPanicEndpoint{
		delivered: make(chan PeerMsg, 1),
	}
	require.NoError(t, msgRouter.RegisterEndpoint(endpoint))

	msg := PeerMsg{Message: &lnwire.Ping{}}
	err := msgRouter.RouteMsg(msg)
	require.ErrorIs(t, err, ErrRoutePanic)

	select {
	case delivered := <-endpoint.delivered:
		require.Same(t, msg.Message, delivered.Message)

	case <-time.After(10 * time.Second):
		t.Fatal("endpoint did not record the partial delivery")
	}
}
