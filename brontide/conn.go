package brontide

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// ErrConnClosed is returned when operations are attempted on a closed connection.
var ErrConnClosed = errors.New("brontide: connection closed")

// Conn is an implementation of net.Conn which enforces an authenticated key
// exchange and message encryption protocol dubbed "Brontide" after initial TCP
// connection establishment. In the case of a successful handshake, all
// messages sent via the .Write() method are encrypted with an AEAD cipher
// along with an encrypted length-prefix. See the Machine struct for
// additional details w.r.t to the handshake and encryption scheme.
type Conn struct {
	conn net.Conn

	// noise is stored as an atomic pointer to allow safe cleanup on Close()
	// while preventing nil pointer dereferences from concurrent operations.
	noise atomic.Pointer[Machine]

	readBuf bytes.Buffer

	// closed is an atomic flag that tracks whether Close() has been called.
	// This prevents nil pointer dereferences if methods are called after Close().
	closed atomic.Uint32
}

// A compile-time assertion to ensure that Conn meets the net.Conn interface.
var _ net.Conn = (*Conn)(nil)

// Dial attempts to establish an encrypted+authenticated connection with the
// remote peer located at address which has remotePub as its long-term static
// public key. In the case of a handshake failure, the connection is closed and
// a non-nil error is returned.
func Dial(local keychain.SingleKeyECDH, netAddr *lnwire.NetAddress,
	timeout time.Duration, dialer tor.DialFunc) (*Conn, error) {

	ipAddr := netAddr.Address.String()
	var conn net.Conn
	var err error
	conn, err = dialer("tcp", ipAddr, timeout)
	if err != nil {
		return nil, err
	}

	b := &Conn{
		conn: conn,
	}
	b.noise.Store(NewBrontideMachine(true, local, netAddr.IdentityKey))

	// Initiate the handshake by sending the first act to the receiver.
	noise := b.noise.Load()
	actOne, err := noise.GenActOne()
	if err != nil {
		b.conn.Close()
		return nil, err
	}
	if _, err := conn.Write(actOne[:]); err != nil {
		b.conn.Close()
		return nil, err
	}

	// We'll ensure that we get ActTwo from the remote peer in a timely
	// manner. If they don't respond within handshakeReadTimeout, then
	// we'll kill the connection.
	err = conn.SetReadDeadline(time.Now().Add(handshakeReadTimeout))
	if err != nil {
		b.conn.Close()
		return nil, err
	}

	// If the first act was successful (we know that address is actually
	// remotePub), then read the second act after which we'll be able to
	// send our static public key to the remote peer with strong forward
	// secrecy.
	var actTwo [ActTwoSize]byte
	if _, err := io.ReadFull(conn, actTwo[:]); err != nil {
		b.conn.Close()
		return nil, err
	}
	if err := noise.RecvActTwo(actTwo); err != nil {
		b.conn.Close()
		return nil, err
	}

	// Finally, complete the handshake by sending over our encrypted static
	// key and execute the final ECDH operation.
	actThree, err := noise.GenActThree()
	if err != nil {
		b.conn.Close()
		return nil, err
	}
	if _, err := conn.Write(actThree[:]); err != nil {
		b.conn.Close()
		return nil, err
	}

	// We'll reset the deadline as it's no longer critical beyond the
	// initial handshake.
	err = conn.SetReadDeadline(time.Time{})
	if err != nil {
		b.conn.Close()
		return nil, err
	}

	return b, nil
}

// ReadNextMessage uses the connection in a message-oriented manner, instructing
// it to read the next _full_ message with the brontide stream. This function
// will block until the read of the header and body succeeds.
//
// NOTE: This method SHOULD NOT be used in the case that the connection may be
// adversarial and induce long delays. If the caller needs to set read deadlines
// appropriately, it is preferred that they use the split ReadNextHeader and
// ReadNextBody methods so that the deadlines can be set appropriately on each.
func (c *Conn) ReadNextMessage() ([]byte, error) {
	if c.closed.Load() == 1 {
		return nil, ErrConnClosed
	}
	noise := c.noise.Load()
	if noise == nil {
		return nil, ErrConnClosed
	}
	return noise.ReadMessage(c.conn)
}

// ReadNextHeader uses the connection to read the next header from the brontide
// stream. This function will block until the read of the header succeeds and
// return the packet length (including MAC overhead) that is expected from the
// subsequent call to ReadNextBody.
func (c *Conn) ReadNextHeader() (uint32, error) {
	if c.closed.Load() == 1 {
		return 0, ErrConnClosed
	}
	noise := c.noise.Load()
	if noise == nil {
		return 0, ErrConnClosed
	}
	return noise.ReadHeader(c.conn)
}

// ReadNextBody uses the connection to read the next message body from the
// brontide stream. This function will block until the read of the body succeeds
// and return the decrypted payload. The provided buffer MUST be the packet
// length returned by the preceding call to ReadNextHeader.
func (c *Conn) ReadNextBody(buf []byte) ([]byte, error) {
	if c.closed.Load() == 1 {
		return nil, ErrConnClosed
	}
	noise := c.noise.Load()
	if noise == nil {
		return nil, ErrConnClosed
	}
	return noise.ReadBody(c.conn, buf)
}

// Read reads data from the connection.  Read can be made to time out and
// return an Error with Timeout() == true after a fixed time limit; see
// SetDeadline and SetReadDeadline.
//
// Part of the net.Conn interface.
func (c *Conn) Read(b []byte) (n int, err error) {
	if c.closed.Load() == 1 {
		return 0, ErrConnClosed
	}

	// In order to reconcile the differences between the record abstraction
	// of our AEAD connection, and the stream abstraction of TCP, we
	// maintain an intermediate read buffer. If this buffer becomes
	// depleted, then we read the next record, and feed it into the
	// buffer. Otherwise, we read directly from the buffer.
	if c.readBuf.Len() == 0 {
		noise := c.noise.Load()
		if noise == nil {
			return 0, ErrConnClosed
		}
		plaintext, err := noise.ReadMessage(c.conn)
		if err != nil {
			return 0, err
		}

		if _, err := c.readBuf.Write(plaintext); err != nil {
			return 0, err
		}
	}

	return c.readBuf.Read(b)
}

// Write writes data to the connection.  Write can be made to time out and
// return an Error with Timeout() == true after a fixed time limit; see
// SetDeadline and SetWriteDeadline.
//
// Part of the net.Conn interface.
func (c *Conn) Write(b []byte) (n int, err error) {
	if c.closed.Load() == 1 {
		return 0, ErrConnClosed
	}

	// If the message doesn't require any chunking, then we can go ahead
	// with a single write.
	if len(b) <= math.MaxUint16 {
		noise := c.noise.Load()
		if noise == nil {
			return 0, ErrConnClosed
		}
		err = noise.WriteMessage(b)
		if err != nil {
			return 0, err
		}
		return noise.Flush(c.conn)
	}

	// If we need to split the message into fragments, then we'll write
	// chunks which maximize usage of the available payload.
	chunkSize := math.MaxUint16

	bytesToWrite := len(b)
	bytesWritten := 0
	for bytesWritten < bytesToWrite {
		// If we're on the last chunk, then truncate the chunk size as
		// necessary to avoid an out-of-bounds array memory access.
		if bytesWritten+chunkSize > len(b) {
			chunkSize = len(b) - bytesWritten
		}

		// Slice off the next chunk to be written based on our running
		// counter and next chunk size.
		chunk := b[bytesWritten : bytesWritten+chunkSize]
		noise := c.noise.Load()
		if noise == nil {
			return bytesWritten, ErrConnClosed
		}
		if err := noise.WriteMessage(chunk); err != nil {
			return bytesWritten, err
		}

		n, err := noise.Flush(c.conn)
		bytesWritten += n
		if err != nil {
			return bytesWritten, err
		}
	}

	return bytesWritten, nil
}

// WriteMessage encrypts and buffers the next message p for the connection. The
// ciphertext of the message is prepended with an encrypt+auth'd length which
// must be used as the AD to the AEAD construction when being decrypted by the
// other side.
//
// NOTE: This DOES NOT write the message to the wire, it should be followed by a
// call to Flush to ensure the message is written.
func (c *Conn) WriteMessage(b []byte) error {
	if c.closed.Load() == 1 {
		return ErrConnClosed
	}
	noise := c.noise.Load()
	if noise == nil {
		return ErrConnClosed
	}
	return noise.WriteMessage(b)
}

// Flush attempts to write a message buffered using WriteMessage to the
// underlying connection. If no buffered message exists, this will result in a
// NOP. Otherwise, it will continue to write the remaining bytes, picking up
// where the byte stream left off in the event of a partial write. The number of
// bytes returned reflects the number of plaintext bytes in the payload, and
// does not account for the overhead of the header or MACs.
//
// NOTE: It is safe to call this method again iff a timeout error is returned.
func (c *Conn) Flush() (int, error) {
	if c.closed.Load() == 1 {
		return 0, ErrConnClosed
	}
	noise := c.noise.Load()
	if noise == nil {
		return 0, ErrConnClosed
	}
	return noise.Flush(c.conn)
}

// Close closes the connection. Any blocked Read or Write operations will be
// unblocked and return errors.
//
// Part of the net.Conn interface.
func (c *Conn) Close() error {
	// Use compare-and-swap to ensure Close is only executed once.
	if !c.closed.CompareAndSwap(0, 1) {
		return ErrConnClosed
	}

	// Clear the state we created to be able to handle this connection.
	// We atomically swap the noise pointer to nil, which allows the ~64KB
	// buffers to be garbage collected immediately while preventing any
	// nil pointer dereferences from concurrent operations.
	c.noise.Store(nil)
	c.readBuf = bytes.Buffer{}

	return c.conn.Close()
}

// LocalAddr returns the local network address.
//
// Part of the net.Conn interface.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
//
// Part of the net.Conn interface.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated with the
// connection. It is equivalent to calling both SetReadDeadline and
// SetWriteDeadline.
//
// Part of the net.Conn interface.
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls. A zero value for t
// means Read will not time out.
//
// Part of the net.Conn interface.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls. Even if write
// times out, it may return n > 0, indicating that some of the data was
// successfully written. A zero value for t means Write will not time out.
//
// Part of the net.Conn interface.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// RemotePub returns the remote peer's static public key.
func (c *Conn) RemotePub() *btcec.PublicKey {
	if c.closed.Load() == 1 {
		return nil
	}
	noise := c.noise.Load()
	if noise == nil {
		return nil
	}
	return noise.remoteStatic
}

// LocalPub returns the local peer's static public key.
func (c *Conn) LocalPub() *btcec.PublicKey {
	if c.closed.Load() == 1 {
		return nil
	}
	noise := c.noise.Load()
	if noise == nil {
		return nil
	}
	return noise.localStatic.PubKey()
}
