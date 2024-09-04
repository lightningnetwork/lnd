package proxy

import (
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/buffer"
	"github.com/lightningnetwork/lnd/pool"
)

// Conn is a mux-proxied connection that contains a remote pubkey
// and local pubkey, just like a Conn.
type Conn struct {
	// We extend a connection. The two kinds we use are either a
	// *Conn from this package, or a net.Conn returned by net.Pipe().
	net.Conn

	// remotePub and localPub support keeping track of peer keys for
	// net.Conn objects returned by net.Pipe().
	remotePub *btcec.PublicKey
	localPub  *btcec.PublicKey

	// readPool is a pointer to the backing mux's readPool, which is
	// shared among all of the connections backed by the mux.
	readPool *pool.Read

	// mtxMsg is a mutex to control access to the current message so that
	// it can only be read/reset at the same time.
	mtxMsg sync.Mutex

	// curMsg is nil if there's no message that's been read, or a pointer
	// to the byte slice representing the current message.
	curMsg *[]byte
}

// ReadNextHeader reads the next header. It passes implementation details
// to a brontide.Conn if possible, otherwise it assumes that this is a pipe
// connection and it can read an entire message in a single read.
//
// TODO(aakselrod): handle large messages/fragmentation/partial reads/timeouts.
func (c *Conn) ReadNextHeader() (uint32, error) {
	// If the underlying connection is able to read messages, do that.
	if conn, ok := c.Conn.(MessageConnWithPubkey); ok {
		return conn.ReadNextHeader()
	}

	c.mtxMsg.Lock()
	defer c.mtxMsg.Unlock()

	// If we've already read a message from a piped connection, return
	// its length.
	if c.curMsg != nil {
		return uint32(len(*c.curMsg)), nil
	}

	// Use the regular API for net.Conn to read from the connection, save
	// the data for returning as a body later, and return its length as
	// expected for now.
	var retBuf []byte

	// Use a buffer from the read pool to read from the underlying conn.
	err := c.readPool.Submit(func(buf *buffer.Read) error {
		n, err := c.Read(buf[:])
		if err != nil {
			return err
		}

		// Make a copy of the data before we give the read buffer back
		// to the pool.
		retBuf = make([]byte, n)

		copy(retBuf, buf[:n])

		return nil
	})
	if err != nil {
		return 0, err
	}

	c.curMsg = &retBuf

	return uint32(len(retBuf)), nil
}

// ReadNextBody reads the next body.
//
// TODO(aakselrod): handle large messages/fragmentation/partial reads/timeouts.
func (c *Conn) ReadNextBody(buf []byte) ([]byte, error) {
	// If the underlying connection is able to read messages, do that.
	if conn, ok := c.Conn.(MessageConnWithPubkey); ok {
		return conn.ReadNextBody(buf)
	}

	c.mtxMsg.Lock()
	defer c.mtxMsg.Unlock()

	// If we haven't read a message using ReadNextHeader, we shouldn't be
	// reading a body.
	if c.curMsg == nil {
		return nil, fmt.Errorf("need to read header before body")
	}

	// If buffer and data lengths mismatch, return an error
	if len(buf) != len(*c.curMsg) {
		return nil, fmt.Errorf("wrong size buffer for message")
	}

	// Copy the message data into the buffer we've been passed
	copy(buf, *c.curMsg)

	// Reset current message for next read
	c.curMsg = nil

	return buf, nil
}

// ReadNextMessage reads the next message.
//
// TODO(aakselrod): handle large messages/fragmentation/partial reads/timeouts.
func (c *Conn) ReadNextMessage() ([]byte, error) {
	// First we read the length of the message.
	n, err := c.ReadNextHeader()
	if err != nil {
		return nil, err
	}

	// Create a new buffer and return the read.
	buf := make([]byte, n)
	return c.ReadNextBody(buf)
}

// WriteMessage implements the method defined in MessageConnWithPubkey.
// If the underlying connection is capable, the message API is used. Otherwise,
// it's converted into a regular Write.
//
// TODO(aakselrod): handle large messages/fragmentation/partial reads/timeouts.
func (c *Conn) WriteMessage(buf []byte) error {
	// If the underlying connection is able to write messages, do that.
	if conn, ok := c.Conn.(MessageConnWithPubkey); ok {
		return conn.WriteMessage(buf)
	}

	// Use the regular API for net.Conn to write to the connection and
	// return the result as expected.
	n, err := c.Write(buf)
	if err != nil {
		return err
	}

	// Partial write.
	if n < len(buf) {
		return fmt.Errorf("only wrote %d of %d byte message", n,
			len(buf))
	}

	return nil
}

// Flush implements the method defined in MessageConnWithPubkey. If the
// underlying connection is capable, its Flush method is called. Otherwise,
// this results in a NOOP.
func (c *Conn) Flush() (int, error) {
	// If the underlying connection can be flushed, do that.
	if conn, ok := c.Conn.(MessageConnWithPubkey); ok {
		return conn.Flush()
	}

	// Do nothing.
	return 0, nil
}

// LocalPub implements the method defined in MessageConnWithPubkey.
func (c *Conn) LocalPub() *btcec.PublicKey {
	return c.localPub
}

// RemotePub implements the method defined in MessageConnWithPubkey.
func (c *Conn) RemotePub() *btcec.PublicKey {
	return c.remotePub
}
