package lnp2p

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// mockBrontideConn implements BrontideConn for testing.
type mockBrontideConn struct {
	net.Conn
	localKey  *btcec.PrivateKey
	remoteKey *btcec.PublicKey
	
	// Message queues for testing.
	incomingMessages chan lnwire.Message
	outgoingMessages chan lnwire.Message
	
	// State tracking.
	closed    atomic.Bool
	flushed   atomic.Bool
	mu        sync.Mutex
	
	// Error injection for testing.
	readErr  error
	writeErr error
	flushErr error
}

// newMockBrontideConn creates a new mock brontide connection.
func newMockBrontideConn(conn net.Conn, localKey *btcec.PrivateKey, remoteKey *btcec.PublicKey) *mockBrontideConn {
	return &mockBrontideConn{
		Conn:             conn,
		localKey:         localKey,
		remoteKey:        remoteKey,
		incomingMessages: make(chan lnwire.Message, 100),
		outgoingMessages: make(chan lnwire.Message, 100),
	}
}

// RemotePub returns the remote peer's public key.
func (c *mockBrontideConn) RemotePub() *btcec.PublicKey {
	return c.remoteKey
}

// LocalPub returns the local peer's public key.
func (c *mockBrontideConn) LocalPub() *btcec.PublicKey {
	return c.localKey.PubKey()
}

// ReadNextMessage reads the next message from the connection.
func (c *mockBrontideConn) ReadNextMessage() ([]byte, error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	
	if c.closed.Load() {
		return nil, io.EOF
	}
	
	select {
	case msg, ok := <-c.incomingMessages:
		if !ok {
			return nil, io.EOF
		}
		// Encode the message to bytes.
		var buf bytes.Buffer
		if _, err := lnwire.WriteMessage(&buf, msg, 0); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		// Simulate blocking read.
		msg, ok := <-c.incomingMessages
		if !ok {
			return nil, io.EOF
		}
		// Encode the message to bytes.
		var buf bytes.Buffer
		if _, err := lnwire.WriteMessage(&buf, msg, 0); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// WriteMessage writes a message to the connection.
func (c *mockBrontideConn) WriteMessage(msgBytes []byte) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	
	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	
	// Parse the message bytes.
	msg, err := lnwire.ReadMessage(bytes.NewReader(msgBytes), 0)
	if err != nil {
		return err
	}
	
	select {
	case c.outgoingMessages <- msg:
		return nil
	default:
		return fmt.Errorf("outgoing message buffer full")
	}
}

// Flush flushes any buffered data to the connection.
func (c *mockBrontideConn) Flush() (int, error) {
	if c.flushErr != nil {
		return 0, c.flushErr
	}
	
	c.flushed.Store(true)
	return 0, nil
}

// Close closes the connection.
func (c *mockBrontideConn) Close() error {
	if c.closed.Swap(true) {
		return nil // Already closed.
	}
	
	close(c.incomingMessages)
	close(c.outgoingMessages)
	
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}

// QueueIncomingMessage queues a message to be read.
func (c *mockBrontideConn) QueueIncomingMessage(msg lnwire.Message) {
	if !c.closed.Load() {
		c.incomingMessages <- msg
	}
}

// GetOutgoingMessage gets the next outgoing message.
func (c *mockBrontideConn) GetOutgoingMessage() (lnwire.Message, bool) {
	select {
	case msg := <-c.outgoingMessages:
		return msg, true
	default:
		return nil, false
	}
}

// SetReadError sets an error to be returned on read.
func (c *mockBrontideConn) SetReadError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readErr = err
}

// SetWriteError sets an error to be returned on write.
func (c *mockBrontideConn) SetWriteError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeErr = err
}

// SetFlushError sets an error to be returned on flush.
func (c *mockBrontideConn) SetFlushError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushErr = err
}

// IsClosed returns whether the connection is closed.
func (c *mockBrontideConn) IsClosed() bool {
	return c.closed.Load()
}

// WasFlushed returns whether Flush was called.
func (c *mockBrontideConn) WasFlushed() bool {
	return c.flushed.Load()
}

// SetReadDeadline sets the read deadline for the connection.
func (c *mockBrontideConn) SetReadDeadline(t time.Time) error {
	if c.Conn != nil {
		return c.Conn.SetReadDeadline(t)
	}
	// Mock implementation: just return nil for now.
	return nil
}

// SetWriteDeadline sets the write deadline for the connection.
func (c *mockBrontideConn) SetWriteDeadline(t time.Time) error {
	if c.Conn != nil {
		return c.Conn.SetWriteDeadline(t)
	}
	// Mock implementation: just return nil for now.
	return nil
}

// createMockBrontidePair creates a pair of connected mock brontide connections.
func createMockBrontidePair() (*mockBrontideConn, *mockBrontideConn, error) {
	// Create pipes for bidirectional communication.
	client, server := net.Pipe()

	// Generate keys.
	clientKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, err
	}

	serverKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, err
	}

	clientConn := newMockBrontideConn(client, clientKey, serverKey.PubKey())
	serverConn := newMockBrontideConn(server, serverKey, clientKey.PubKey())

	return clientConn, serverConn, nil
}

// createMockBrontidePairWithKeys creates a pair of connected mock brontide connections with specified keys.
func createMockBrontidePairWithKeys(clientKey, serverKey *btcec.PrivateKey) (*mockBrontideConn, *mockBrontideConn, error) {
	// Create pipes for bidirectional communication.
	client, server := net.Pipe()

	clientConn := newMockBrontideConn(client, clientKey, serverKey.PubKey())
	serverConn := newMockBrontideConn(server, serverKey, clientKey.PubKey())

	return clientConn, serverConn, nil
}

// MockBrontideDialer is a mock implementation of BrontideDialer for testing.
// It allows tests to return a pre-configured mock connection without
// performing the actual noise handshake.
type MockBrontideDialer struct {
	// dialFunc is the custom dial function to use.
	// If nil, returns an error.
	dialFunc func(localKey keychain.SingleKeyECDH, netAddr *lnwire.NetAddress,
		timeout time.Duration, dialer tor.DialFunc) (BrontideConn, error)
}

// NewMockBrontideDialer creates a new mock dialer with a custom dial function.
func NewMockBrontideDialer(dialFunc func(keychain.SingleKeyECDH, *lnwire.NetAddress,
	time.Duration, tor.DialFunc) (BrontideConn, error)) *MockBrontideDialer {
	return &MockBrontideDialer{dialFunc: dialFunc}
}

// Dial implements the BrontideDialer interface.
func (m *MockBrontideDialer) Dial(localKey keychain.SingleKeyECDH, 
	netAddr *lnwire.NetAddress, timeout time.Duration, 
	dialer tor.DialFunc) (BrontideConn, error) {
	
	if m.dialFunc == nil {
		return nil, fmt.Errorf("mock dial function not configured")
	}
	
	return m.dialFunc(localKey, netAddr, timeout, dialer)
}

// mockNetConn is a simple in-memory net.Conn implementation.
type mockNetConn struct {
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
	closed   atomic.Bool
	mu       sync.Mutex
	
	localAddr  net.Addr
	remoteAddr net.Addr
}

// newMockNetConn creates a new mock network connection.
func newMockNetConn() *mockNetConn {
	return &mockNetConn{
		readBuf:    &bytes.Buffer{},
		writeBuf:   &bytes.Buffer{},
		localAddr:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735},
		remoteAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9736},
	}
}

func (c *mockNetConn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if c.closed.Load() {
		return 0, io.EOF
	}
	
	return c.readBuf.Read(b)
}

func (c *mockNetConn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if c.closed.Load() {
		return 0, fmt.Errorf("connection closed")
	}
	
	return c.writeBuf.Write(b)
}

func (c *mockNetConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *mockNetConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *mockNetConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *mockNetConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *mockNetConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *mockNetConn) SetWriteDeadline(t time.Time) error {
	return nil
}