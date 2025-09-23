package lnp2p

import (
	"context"
	"encoding/hex"
	"fmt"
	"iter"
	"net"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// BrontideConn is an interface that abstracts the brontide.Conn for testing.
// This allows us to mock the underlying encrypted connection without requiring
// the full brontide handshake.
type BrontideConn interface {
	// Embedded net.Conn for standard connection operations.
	net.Conn

	// RemotePub returns the remote peer's public key.
	RemotePub() *btcec.PublicKey

	// LocalPub returns the local peer's public key.
	LocalPub() *btcec.PublicKey

	// ReadNextMessage reads the next message from the connection.
	ReadNextMessage() ([]byte, error)

	// WriteMessage writes a message to the connection.
	WriteMessage([]byte) error

	// Flush flushes any buffered data to the connection.
	Flush() (int, error)
}

// ConnectionState represents the state of a P2P connection.
type ConnectionState int

const (
	// StateDisconnected indicates the connection is not established.
	StateDisconnected ConnectionState = iota

	// StateConnecting indicates the connection is being established.
	StateConnecting

	// StateHandshaking indicates the noise handshake is in progress.
	StateHandshaking

	// StateInitializing indicates the init message exchange is happening.
	StateInitializing

	// StateConnected indicates the connection is fully established.
	StateConnected

	// StateClosing indicates the connection is being closed.
	StateClosing
)

// String returns a human-readable representation of the connection state.
func (s ConnectionState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateHandshaking:
		return "handshaking"
	case StateInitializing:
		return "initializing"
	case StateConnected:
		return "connected"
	case StateClosing:
		return "closing"
	default:
		return "unknown"
	}
}

// ConnectionEvent represents a connection lifecycle event.
type ConnectionEvent struct {
	// State is the new connection state.
	State ConnectionState

	// Timestamp is when the event occurred.
	Timestamp time.Time

	// Error contains any error associated with the event.
	Error error

	// Details provides additional event-specific information.
	Details string
}

// P2PConnection provides a high-level interface for P2P connections.
type P2PConnection interface {
	// Connect establishes a connection and completes the handshake.
	Connect(ctx context.Context) error

	// SendMessage sends a message to the peer.
	SendMessage(msg lnwire.Message) error

	// ReceiveMessages returns an iterator for incoming messages.
	ReceiveMessages() iter.Seq[lnwire.Message]

	// ConnectionEvents returns an iterator for connection lifecycle events.
	ConnectionEvents() iter.Seq[ConnectionEvent]

	// Close terminates the connection.
	Close() error

	// LocalPubKey returns the local node's public key.
	LocalPubKey() *btcec.PublicKey

	// RemotePubKey returns the remote peer's public key.
	RemotePubKey() *btcec.PublicKey

	// IsConnected returns true if the connection is established.
	IsConnected() bool

	// RemoteAddr returns the remote address.
	RemoteAddr() net.Addr

	// LocalAddr returns the local address.
	LocalAddr() net.Addr
}

// NodeAddress encapsulates a Lightning node's identity and network address.
type NodeAddress struct {
	// PubKey is the node's public key.
	PubKey *btcec.PublicKey

	// Address is the network address.
	Address net.Addr
}

// String returns the canonical representation: pubkey@host:port.
func (n NodeAddress) String() string {
	pubKeyHex := n.PubKey.SerializeCompressed()
	return fmt.Sprintf("%x@%s", pubKeyHex, n.Address.String())
}

// ParseNodeAddress parses a string in the format pubkey@host:port.
func ParseNodeAddress(addr string) (*NodeAddress, error) {
	parts := strings.Split(addr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid node address format: %s", addr)
	}

	pubKeyBytes, err := hex.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid public key hex: %w", err)
	}

	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}

	if parts[1] == "" {
		return nil, fmt.Errorf("network address cannot be empty")
	}
	netAddr, err := net.ResolveTCPAddr("tcp", parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid network address: %w", err)
	}

	return &NodeAddress{
		PubKey:  pubKey,
		Address: netAddr,
	}, nil
}

// KeyGenerator provides an interface for generating ephemeral keys.
type KeyGenerator interface {
	// GenerateKey generates a new ephemeral key.
	GenerateKey() (keychain.SingleKeyECDH, error)
}

// EphemeralKeyGenerator generates random ephemeral keys.
type EphemeralKeyGenerator struct{}

// GenerateKey generates a new random ephemeral key.
func (e *EphemeralKeyGenerator) GenerateKey() (keychain.SingleKeyECDH, error) {
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	return &keychain.PrivKeyECDH{
		PrivKey: privKey,
	}, nil
}

// StaticKeyGenerator returns the same key every time (for testing).
type StaticKeyGenerator struct {
	key keychain.SingleKeyECDH
}

// NewStaticKeyGenerator creates a generator that returns the provided key.
func NewStaticKeyGenerator(key keychain.SingleKeyECDH) *StaticKeyGenerator {
	return &StaticKeyGenerator{key: key}
}

// GenerateKey returns the static key.
func (s *StaticKeyGenerator) GenerateKey() (keychain.SingleKeyECDH, error) {
	return s.key, nil
}

// TimeoutConfig contains timeout settings for various operations.
type TimeoutConfig struct {
	// DialTimeout is the timeout for establishing the TCP connection.
	DialTimeout time.Duration

	// HandshakeTimeout is the timeout for the noise handshake.
	HandshakeTimeout time.Duration

	// InitTimeout is the timeout for the init message exchange.
	InitTimeout time.Duration

	// ReadTimeout is the timeout for reading messages.
	ReadTimeout time.Duration

	// WriteTimeout is the timeout for writing messages.
	WriteTimeout time.Duration

	// PingInterval is the interval between ping messages.
	PingInterval time.Duration

	// PongTimeout is the timeout for receiving pong responses.
	PongTimeout time.Duration
}

// DefaultTimeouts returns default timeout values.
func DefaultTimeouts() TimeoutConfig {
	return TimeoutConfig{
		DialTimeout:      30 * time.Second,
		HandshakeTimeout: 15 * time.Second,
		InitTimeout:      15 * time.Second,
		ReadTimeout:      5 * time.Second,
		WriteTimeout:     5 * time.Second,
		PingInterval:     1 * time.Minute,
		PongTimeout:      30 * time.Second,
	}
}

// TorTimeouts returns timeout values adjusted for Tor connections.
func TorTimeouts() TimeoutConfig {
	base := DefaultTimeouts()
	multiplier := time.Duration(3)

	return TimeoutConfig{
		DialTimeout:      base.DialTimeout * multiplier,
		HandshakeTimeout: base.HandshakeTimeout * multiplier,
		InitTimeout:      base.InitTimeout * multiplier,
		ReadTimeout:      base.ReadTimeout * multiplier,
		WriteTimeout:     base.WriteTimeout * multiplier,
		PingInterval:     base.PingInterval,
		PongTimeout:      base.PongTimeout * multiplier,
	}
}

// BrontideDialer abstracts the brontide dial operation.
// This allows for dependency injection and easier testing.
type BrontideDialer interface {
	// Dial establishes an encrypted+authenticated connection with the
	// remote peer. It performs the noise handshake and returns a
	// BrontideConn on success.
	Dial(localKey keychain.SingleKeyECDH, netAddr *lnwire.NetAddress,
		timeout time.Duration, dialer tor.DialFunc) (BrontideConn, error)
}

// DefaultBrontideDialer is the production implementation that uses
// the real brontide.Dial to establish encrypted connections.
type DefaultBrontideDialer struct{}

// Dial establishes a real brontide connection with full handshake.
func (d *DefaultBrontideDialer) Dial(localKey keychain.SingleKeyECDH,
	netAddr *lnwire.NetAddress, timeout time.Duration,
	dialer tor.DialFunc) (BrontideConn, error) {

	// Use the real brontide.Dial which returns *brontide.Conn.
	// Since *brontide.Conn implements our BrontideConn interface,
	// we can return it directly.
	return brontide.Dial(localKey, netAddr, timeout, dialer)
}

