package peer

import (
	"testing"

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

func (m *mockEndpoint) CanHandle(msg lnwire.Message) bool {
	args := m.Called(msg)

	return args.Bool(0)
}

func (m *mockEndpoint) SendMessage(msg lnwire.Message) bool {
	args := m.Called(msg)

	return args.Bool(0)
}

// TestMessageRouterOperation tests the basic operation of the message router:
// add new endpoints, route to them, remove, them, etc.
func TestMessageRouterOperation(t *testing.T) {
	msgRouter := NewMultiMsgRouter()
	msgRouter.Start()
	defer msgRouter.Stop()

	openChanMsg := &lnwire.OpenChannel{}
	commitSigMsg := &lnwire.CommitSig{}

	errorMsg := &lnwire.Error{}

	// For this test, we'll have two endpoints, each with distinct names.
	// One endpoint will only handle OpenChannel, while the other will
	// handle the CommitSig message.
	fundingEndpoint := &mockEndpoint{}
	fundingEndpointName := "funding"
	fundingEndpoint.On("Name").Return(fundingEndpointName)
	fundingEndpoint.On("CanHandle", openChanMsg).Return(true)
	fundingEndpoint.On("CanHandle", errorMsg).Return(false)
	fundingEndpoint.On("CanHandle", commitSigMsg).Return(false)
	fundingEndpoint.On("SendMessage", openChanMsg).Return(true)

	commitEndpoint := &mockEndpoint{}
	commitEndpointName := "commit"
	commitEndpoint.On("Name").Return(commitEndpointName)
	commitEndpoint.On("CanHandle", commitSigMsg).Return(true)
	commitEndpoint.On("CanHandle", openChanMsg).Return(false)
	commitEndpoint.On("CanHandle", errorMsg).Return(false)
	commitEndpoint.On("SendMessage", commitSigMsg).Return(true)

	t.Run("add endpoints", func(t *testing.T) {
		// First, we'll add the funding endpoint to the router.
		require.NoError(t, msgRouter.RegisterEndpoint(fundingEndpoint))

		// There should be a single endpoint registered.
		require.Len(t, msgRouter.Endpoints(), 1)

		// The name of the registered endpoint should be "funding".
		require.Equal(
			t, "funding",
			msgRouter.Endpoints()[fundingEndpointName].Name(),
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

		fundingEndpoint.AssertCalled(t, "SendMessage", openChanMsg)
		commitEndpoint.AssertNotCalled(t, "SendMessage", openChanMsg)

		// We'll do the same for the commit sig message.
		require.NoError(t, msgRouter.RouteMsg(commitSigMsg))

		fundingEndpoint.AssertCalled(t, "CanHandle", commitSigMsg)
		commitEndpoint.AssertCalled(t, "CanHandle", commitSigMsg)

		commitEndpoint.AssertCalled(t, "SendMessage", commitSigMsg)
		fundingEndpoint.AssertNotCalled(t, "SendMessage", commitSigMsg)
	})

	t.Run("remove endpoints", func(t *testing.T) {
		// Finally, we'll remove both endpoints.
		require.NoError(
			t, msgRouter.UnregisterEndpoint(fundingEndpointName),
		)
		require.NoError(
			t, msgRouter.UnregisterEndpoint(commitEndpointName),
		)

		// There should be no endpoints registered.
		require.Len(t, msgRouter.Endpoints(), 0)

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
