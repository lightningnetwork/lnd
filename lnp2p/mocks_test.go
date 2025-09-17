package lnp2p

import (
	"context"
	"iter"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
)

// mockP2PConnection is a mock implementation of P2PConnection using testify/mock.
type mockP2PConnection struct {
	mock.Mock
}

// Connect mocks the Connect method.
func (m *mockP2PConnection) Connect(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

// SendMessage mocks the SendMessage method.
func (m *mockP2PConnection) SendMessage(msg lnwire.Message) error {
	args := m.Called(msg)
	return args.Error(0)
}

// ReceiveMessages mocks the ReceiveMessages method.
func (m *mockP2PConnection) ReceiveMessages() iter.Seq[lnwire.Message] {
	args := m.Called()
	if fn := args.Get(0); fn != nil {
		// Check if it's already an iter.Seq[lnwire.Message]
		if seq, ok := fn.(iter.Seq[lnwire.Message]); ok {
			return seq
		}
		// Check if it's a func(yield func(lnwire.Message) bool)
		if f, ok := fn.(func(yield func(lnwire.Message) bool)); ok {
			return iter.Seq[lnwire.Message](f)
		}
	}
	// Return empty iterator if not configured.
	return func(yield func(lnwire.Message) bool) {}
}

// ConnectionEvents mocks the ConnectionEvents method.
func (m *mockP2PConnection) ConnectionEvents() iter.Seq[ConnectionEvent] {
	args := m.Called()
	if fn := args.Get(0); fn != nil {
		// Check if it's already an iter.Seq[ConnectionEvent]
		if seq, ok := fn.(iter.Seq[ConnectionEvent]); ok {
			return seq
		}
		// Check if it's a func(yield func(ConnectionEvent) bool)
		if f, ok := fn.(func(yield func(ConnectionEvent) bool)); ok {
			return iter.Seq[ConnectionEvent](f)
		}
	}
	// Return empty iterator if not configured.
	return func(yield func(ConnectionEvent) bool) {}
}

// Close mocks the Close method.
func (m *mockP2PConnection) Close() error {
	args := m.Called()
	return args.Error(0)
}

// LocalPubKey mocks the LocalPubKey method.
func (m *mockP2PConnection) LocalPubKey() *btcec.PublicKey {
	args := m.Called()
	if key := args.Get(0); key != nil {
		return key.(*btcec.PublicKey)
	}
	return nil
}

// RemotePubKey mocks the RemotePubKey method.
func (m *mockP2PConnection) RemotePubKey() *btcec.PublicKey {
	args := m.Called()
	if key := args.Get(0); key != nil {
		return key.(*btcec.PublicKey)
	}
	return nil
}

// IsConnected mocks the IsConnected method.
func (m *mockP2PConnection) IsConnected() bool {
	args := m.Called()
	return args.Bool(0)
}

// RemoteAddr mocks the RemoteAddr method.
func (m *mockP2PConnection) RemoteAddr() net.Addr {
	args := m.Called()
	if addr := args.Get(0); addr != nil {
		return addr.(net.Addr)
	}
	return nil
}

// LocalAddr mocks the LocalAddr method.
func (m *mockP2PConnection) LocalAddr() net.Addr {
	args := m.Called()
	if addr := args.Get(0); addr != nil {
		return addr.(net.Addr)
	}
	return nil
}

// Ensure mockP2PConnection implements P2PConnection.
var _ P2PConnection = (*mockP2PConnection)(nil)

// mockUnknownMessage is a message type not handled by the actor.
type mockUnknownMessage struct {
	basePeerMessage
}

// MessageType returns the type of this message.
func (m *mockUnknownMessage) MessageType() string {
	return "UnknownMessage"
}