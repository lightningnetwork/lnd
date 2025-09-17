package lnp2p

import (
	"context"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/stretchr/testify/mock"
)

// mockMsgRouter is a mock implementation of msgmux.Router for testing.
type mockMsgRouter struct {
	mock.Mock
	routed []lnwire.Message
}

// RegisterEndpoint registers a new endpoint with the router.
func (m *mockMsgRouter) RegisterEndpoint(endpoint msgmux.Endpoint) error {
	args := m.Called(endpoint)
	return args.Error(0)
}

// UnregisterEndpoint unregisters the target endpoint from the router.
func (m *mockMsgRouter) UnregisterEndpoint(name msgmux.EndpointName) error {
	args := m.Called(name)
	return args.Error(0)
}

// RouteMsg routes a message to registered endpoints.
func (m *mockMsgRouter) RouteMsg(msg msgmux.PeerMsg) error {
	m.routed = append(m.routed, msg.Message)
	args := m.Called(msg)
	return args.Error(0)
}

// Start starts the router.
func (m *mockMsgRouter) Start(ctx context.Context) {
	m.Called(ctx)
}

// Stop stops the router.
func (m *mockMsgRouter) Stop() {
	m.Called()
}