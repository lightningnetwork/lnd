package brontide

import (
	"io"
	"net"

	"github.com/roasbeef/btcd/btcec"
)

// Listener is an implementation of a net.Conn which executes an authenticated
// key exchange and message encryption protocol dubeed "BrontideMachine" after
// initial connection acceptance. See the BrontideMachine struct for additional
// details w.r.t the handshake and encryption scheme used within the
// connection.
type Listener struct {
	localStatic *btcec.PrivateKey

	tcp *net.TCPListener
}

// A compile-time assertion to ensure that Conn meets the net.Listener interface.
var _ net.Listener = (*Listener)(nil)

// NewListener returns a new net.Listener which enforces the Brontide scheme
// during both initial connection establishment and data transfer.
func NewListener(localStatic *btcec.PrivateKey, listenAddr string) (*Listener,
	error) {
	addr, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &Listener{
		localStatic: localStatic,
		tcp:         l,
	}, nil
}

// Accept waits for and returns the next connection to the listener. All
// incoming connections are authenticated via the three act Brontide
// key-exchange scheme. This funciton will fail with a non-nil error in the
// case that either the handhska breaks down, or the remote peer doesn't know
// our static public key.
//
// Part of the net.Listener interface.
func (l *Listener) Accept() (net.Conn, error) {
	conn, err := l.tcp.Accept()
	if err != nil {
		return nil, err
	}

	brontideConn := &Conn{
		conn:  conn,
		noise: NewBrontideMachine(false, l.localStatic, nil),
	}

	// Attempt to carry out the first act of the handshake protocol. If the
	// connecting node doesn't know our long-term static public key, then
	// this portion will fail with a non-nil error.
	var actOne [ActOneSize]byte
	if _, err := io.ReadFull(conn, actOne[:]); err != nil {
		return nil, err
	}
	if err := brontideConn.noise.RecvActOne(actOne); err != nil {
		return nil, err
	}

	// Next, progress the handshake processes by sending over our ephemeral
	// key for the session along with an authenticating tag.
	actTwo, err := brontideConn.noise.GenActTwo()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(actTwo[:]); err != nil {
		return nil, err
	}

	// Finally, finish the handhskae processes by reading and decrypting
	// the conneciton peer's static public key. If this succeeeds then both
	// sides have mutually authenticated each other.
	var actThree [ActThreeSize]byte
	if _, err := io.ReadFull(conn, actThree[:]); err != nil {
		return nil, err
	}
	if err := brontideConn.noise.RecvActThree(actThree); err != nil {
		return nil, err
	}

	return brontideConn, nil
}

// Close closes the listener.  Any blocked Accept operations will be unblocked
// and return errors.
//
// Part of the net.Listener interface.
func (l *Listener) Close() error {
	return l.tcp.Close()
}

// Addr returns the listener's network address.
//
// Part of the net.Listener interface.
func (l *Listener) Addr() net.Addr {
	return l.tcp.Addr()
}
