package wtmock

import (
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
)

// MockPeer emulates a single endpoint of brontide transport.
type MockPeer struct {
	remotePub  *btcec.PublicKey
	remoteAddr net.Addr

	IncomingMsgs chan []byte
	OutgoingMsgs chan []byte

	writeDeadline <-chan time.Time
	readDeadline  <-chan time.Time

	Quit chan struct{}
}

// NewMockPeer returns a fresh MockPeer.
func NewMockPeer(pk *btcec.PublicKey, addr net.Addr, bufferSize int) *MockPeer {
	return &MockPeer{
		remotePub:    pk,
		remoteAddr:   addr,
		IncomingMsgs: make(chan []byte, bufferSize),
		OutgoingMsgs: make(chan []byte, bufferSize),
		Quit:         make(chan struct{}),
	}
}

// Write sends the raw bytes as the next full message read to the remote peer.
// The write will fail if either party closes the connection or the write
// deadline expires. The passed bytes slice is copied before sending, thus the
// bytes may be reused once the method returns.
func (p *MockPeer) Write(b []byte) (n int, err error) {
	select {
	case p.OutgoingMsgs <- b:
		return len(b), nil
	case <-p.writeDeadline:
		return 0, fmt.Errorf("write timeout expired")
	case <-p.Quit:
		return 0, fmt.Errorf("connection closed")
	}
}

// Close tearsdown the connection, and fails any pending reads or writes.
func (p *MockPeer) Close() error {
	select {
	case <-p.Quit:
		return fmt.Errorf("connection already closed")
	default:
		close(p.Quit)
		return nil
	}
}

// ReadNextMessage returns the raw bytes of the next full message read from the
// remote peer. The read will fail if either party closes the connection or the
// read deadline expires.
func (p *MockPeer) ReadNextMessage() ([]byte, error) {
	select {
	case b := <-p.IncomingMsgs:
		return b, nil
	case <-p.readDeadline:
		return nil, fmt.Errorf("read timeout expired")
	case <-p.Quit:
		return nil, fmt.Errorf("connection closed")
	}
}

// SetWriteDeadline initializes a timer that will cause any pending writes to
// fail at time t. If t is zero, the deadline is infinite.
func (p *MockPeer) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		p.writeDeadline = nil
		return nil
	}

	duration := time.Until(t)
	p.writeDeadline = time.After(duration)

	return nil
}

// SetReadDeadline initializes a timer that will cause any pending reads to fail
// at time t. If t is zero, the deadline is infinite.
func (p *MockPeer) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		p.readDeadline = nil
		return nil
	}

	duration := time.Until(t)
	p.readDeadline = time.After(duration)

	return nil
}

// RemotePub returns the public key of the remote peer.
func (p *MockPeer) RemotePub() *btcec.PublicKey {
	return p.remotePub
}

// RemoteAddr returns the net address of the remote peer.
func (p *MockPeer) RemoteAddr() net.Addr {
	return p.remoteAddr
}

// Compile-time constraint ensuring the MockPeer implements the wserver.Peer
// interface.
var _ wtserver.Peer = (*MockPeer)(nil)
