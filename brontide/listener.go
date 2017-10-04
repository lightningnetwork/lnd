package brontide

import (
	"io"
	"net"
	"time"

	"github.com/roasbeef/btcd/btcec"
)

// Listener is an implementation of a net.Conn which executes an authenticated
// key exchange and message encryption protocol dubbed "Machine" after
// initial connection acceptance. See the Machine struct for additional
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
// key-exchange scheme. This function will fail with a non-nil error in the
// case that either the handshake breaks down, or the remote peer doesn't know
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

	// We'll ensure that we get ActOne from the remote peer in a timely
	// manner. If they don't respond within 15 seconds, then we'll kill the
	// connection.
	conn.SetReadDeadline(time.Now().Add(time.Second * 15))

	// Attempt to carry out the first act of the handshake protocol. If the
	// connecting node doesn't know our long-term static public key, then
	// this portion will fail with a non-nil error.
	var actOne [ActOneSize]byte
	if _, err := io.ReadFull(conn, actOne[:]); err != nil {
		brontideConn.conn.Close()
		return nil, err
	}
	if err := brontideConn.noise.RecvActOne(actOne); err != nil {
		brontideConn.conn.Close()
		return nil, err
	}

	// Next, progress the handshake processes by sending over our ephemeral
	// key for the session along with an authenticating tag.
	actTwo, err := brontideConn.noise.GenActTwo()
	if err != nil {
		brontideConn.conn.Close()
		return nil, err
	}
	if _, err := conn.Write(actTwo[:]); err != nil {
		brontideConn.conn.Close()
		return nil, err
	}

	// We'll ensure that we get ActTwo from the remote peer in a timely
	// manner. If they don't respond within 15 seconds, then we'll kill the
	// connection.
	conn.SetReadDeadline(time.Now().Add(time.Second * 15))

	// Finally, finish the handshake processes by reading and decrypting
	// the connection peer's static public key. If this succeeds then both
	// sides have mutually authenticated each other.
	var actThree [ActThreeSize]byte
	if _, err := io.ReadFull(conn, actThree[:]); err != nil {
		brontideConn.conn.Close()
		return nil, err
	}
	if err := brontideConn.noise.RecvActThree(actThree); err != nil {
		brontideConn.conn.Close()
		return nil, err
	}

	// We'll reset the deadline as it's no longer critical beyond the
	// initial handshake.
	conn.SetReadDeadline(time.Time{})

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
