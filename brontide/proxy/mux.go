package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/pool"
	"golang.org/x/sync/errgroup"
)

// connKey is an 8-byte key to identify connections multiplexed over a single
// connection between two muxes. In a special case, an all-zeroes connKey is
// a signal (right now, only supported to announce a new connection).
type connKey [8]byte

// pubKeyToConnKey takes the last 8 bytes of a public key and copies then to
// a connKey object.
//
// TODO(aakselrod): use a better derivation of connection key from pubkey like
// SipHash64 or just use the whole pubkey?
func pubKeyToConnKey(key *btcec.PublicKey) connKey {
	var ck connKey

	keyBytes := key.SerializeCompressed()

	copy(ck[:], keyBytes[len(keyBytes)-8:])

	return ck
}

// Mux multiplexes a set of connections over a single proxy connection. It sends
// an entire packet.
type Mux struct {

	// mtx is a mutex for async access.
	mtx sync.RWMutex

	// eg is an rror group to collect errors from goroutines if wanted.
	eg *errgroup.Group

	// ctx is the context.
	ctx context.Context

	// stop is the cancel function for clean shutdown.
	stop context.CancelFunc

	// started tracks whether we've started the handler.
	started bool

	// conn is a brontide connection to another mux for multiplexing the
	// conns below.
	conn *brontide.Conn

	// conns is a map of connections by substring of pubkey. The interface
	// can be either a *brontide.Conn or a *Conn.
	conns map[connKey]MessageConnWithPubkey

	// readPool is a pool of read buffers shared among all of the mux's
	// connections.
	readPool *pool.Read

	// accept is a channel for accepting new connections from the connected
	// mux. The public key is that of a newly connected peer.
	accept chan *btcec.PublicKey
}

// NewMux creates a new Mux and populates it with initial values.
func NewMux(ctx context.Context, readPool *pool.Read,
	conn *brontide.Conn) *Mux {

	m := &Mux{}

	// Assign the mux connection and read buffer pool.
	m.conn = conn
	m.readPool = readPool

	// Derive a context with our own cancel signal.
	m.ctx, m.stop = context.WithCancel(ctx)

	// Derive a new error group from the context above.
	m.eg, m.ctx = errgroup.WithContext(m.ctx)

	// Initialize the conns map and accept channel.
	m.conns = make(map[connKey]MessageConnWithPubkey)
	m.accept = make(chan *btcec.PublicKey)

	return m
}

// Start launches the mux's event loop goroutine.
func (m *Mux) Start() error {
	// Ensure we lock the mutex to modify the state.
	m.mtx.Lock()
	defer m.mtx.Unlock()

	if m.started {
		return fmt.Errorf("mux already running")
	}

	// Set to started.
	m.started = true

	m.eg.Go(m.eventLoop)

	return nil
}

// Accept is used by the node mux to listen for p2p connections through the
// proxy mux to which it's connected. It's part of the net.Listener interface.
func (m *Mux) Accept() (net.Conn, error) {
	for {
		select {
		// Wait for a message on the accept channel.
		case pubKey, ok := <-m.accept:

			// Channel closed, return error
			if !ok {
				return nil, net.ErrClosed
			}

			// Got a new pubkey, create the Conn objects we'll
			// need. First, we need virtual piped connections.
			ourConn, theirConn := net.Pipe()

			// Create a connection for the mux to handle.
			conn := &Conn{
				Conn:      ourConn,
				localPub:  m.conn.LocalPub(),
				remotePub: pubKey,
				readPool:  m.readPool,
			}

			// Register the connection with the mux.
			m.mtx.Lock()
			err := m.add(conn)
			m.mtx.Unlock()
			if err != nil {
				return nil, err
			}

			// Return a connection for the caller to use.
			return &Conn{
				Conn:      theirConn,
				localPub:  conn.localPub,
				remotePub: pubKey,
				readPool:  m.readPool,
			}, nil

		// Our context is cancelled, time to exit.
		case <-m.ctx.Done():
			return nil, net.ErrClosed
		}
	}
}

// Close closes the mux, stops the handler goroutine, closes any connections
// used by the mux, and stops their goroutines as well. It is part of the
// net.Listener interface.
func (m *Mux) Close() error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	// We're not started.
	if !m.started {
		return fmt.Errorf("mux isn't running")
	}

	// Stop all of the connections' goroutines and delete them from the map.
	for k := range m.conns {
		m.stopConn(k)
	}

	// Close the mux connection and zero its pointer.
	m.conn.Close()
	m.conn = nil

	// Let future callers know the mux isn't running.
	m.started = false

	return nil
}

// Addr returns the address of the mux's local connection. It is part of the
// net.Listener interface.
//
// TODO(aakselrod): should we specify this on init to return the proxy's
// listen address instead?
func (m *Mux) Addr() net.Addr {
	return m.conn.LocalAddr()
}

// eventLoop handles messages coming in over the mux connection and
// de-multiplexes them to other connections. It also handles signals for the
// node mux that the proxy mux has added an incoming peer connection by
// signaling over the accept channel.
func (m *Mux) eventLoop() error {
	// On event loop exit, stop all of the connections
	defer func() {
		m.Close()
	}()

	m.mtx.RLock()
	muxConn := m.conn
	m.mtx.RUnlock()

	if muxConn == nil {
		return net.ErrClosed
	}

	// Start the event loop.
	for {
		// TODO(aakselrod): handle oversized messages, fragmentation,
		// timeouts, partial reads, etc.
		l, err := muxConn.ReadNextHeader()
		if err != nil {
			return err
		}

		pkt := make([]byte, l)
		pkt, err = muxConn.ReadNextBody(pkt)
		if err != nil {
			return err
		}

		// TODO(aakselrod): more robust signaling..
		if len(pkt) < 9 {
			// We expect 8 bytes for the connection key and at
			// least 1 byte of data here.
			return fmt.Errorf("mux requires at least 9 bytes, "+
				"got %d", len(pkt))
		}

		// Get the connection key for this message from the first 8
		// bytes.
		var key connKey
		copy(key[:], pkt[:8])

		// If key is all zeroes, we're opening a connection to a new
		// peer, so next 33 bytes are the peer pubkey.
		//
		// TODO(aakselrod): more robust signaling.
		if (key == connKey{}) {
			if len(pkt) != btcec.PubKeyBytesLenCompressed+8 {
				// We expect exactly 33+8=41 bytes, otherwise
				// ignore the message
				continue
			}

			// The first 8 bytes are zeroes as a signal that the
			// message is from the peer mux, not a multiplexed
			// connection. The rest of the message is a pubkey for
			// adding a new connection.
			//
			// TODO(aakselrod): handle more types of messages,
			// perhaps dial-out messages or other useful things
			// that proxies do.
			pubKey, err := btcec.ParsePubKey(pkt[8:])
			if err != nil {
				return fmt.Errorf("error parsing pubkey: %v",
					err)
			}

			// Derive the connKey.
			key = pubKeyToConnKey(pubKey)

			// Check if a connection already exists under this
			// pubkey. If so, it could be a duplicate or a
			// reconnection attempt.
			m.mtx.RLock()
			_, connected := m.conns[key]
			m.mtx.RUnlock()

			if connected {
				// We already have a proxied connection for
				// this key, so we wait for the next one.
				//
				// TODO(aakselrod): handle reconnects. Some
				// handling already exists in the Add function.
				continue
			}

			select {
			// Send the public key to an Accept() running somewhere.
			//
			// TODO(aakselrod): use better notifications or at
			// least handle the case where there isn't an Accept()
			// running.
			case m.accept <- pubKey:
				continue

			// Time to quit, our context is cancelled.
			case <-m.ctx.Done():
				return nil
			}
		}

		// Got the connection key in the message, so look up the
		// matching connection by key.
		m.mtx.RLock()
		conn, ok := m.conns[key]
		m.mtx.RUnlock()

		if !ok {
			// Drop packet, it doesn't match a connection that we
			// have..
			//
			// TODO(aakselrod): smarter handling. Perhaps we signal
			// a new connection just by sending a message relating
			// to its public key instead of an all-zeroes connKey?
			continue
		}

		n, err := conn.Write(pkt[8:])
		if err != nil || n < len(pkt)-8 {
			m.mtx.Lock()
			m.stopConn(key)
			m.mtx.Unlock()
		}
	}
}

// stopConn stops a connection and must be run with the read-write lock
// acquired.
func (m *Mux) stopConn(key connKey) {
	conn, ok := m.conns[key]
	delete(m.conns, key)

	if ok && conn != nil {
		conn.Close()
	}
}

// add is used to add and start handling a new connection and must be run with
// the read-write lock acquired.
func (m *Mux) add(conn MessageConnWithPubkey) error {
	// Check if mux is started.
	if !m.started {
		return fmt.Errorf("can't add connection to mux that's " +
			"not running")
	}

	// Derive connection key from public key.
	key := pubKeyToConnKey(conn.RemotePub())

	// In case we're replacing a stale connection.
	//
	// TODO(aakselrod): robust handling of reconnects.
	m.stopConn(key)

	// Add connection to map by connKey.
	m.conns[key] = conn

	// Start a goroutine for the mux to handle the newly added connection.
	m.eg.Go(func() error { return m.handleConn(key, conn) })

	return nil
}

// Add adds a new connection to be multiplexed through the multiplexer. It can
// be either a *Conn or a *brontide.Conn.
func (m *Mux) Add(conn MessageConnWithPubkey) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	err := m.add(conn)
	if err != nil {
		return err
	}

	// Signal the peer mux that we've got a new connection. First, get the
	// key bytes.
	//
	// TODO(aakselrod): use a separate mutex for writing to the mux
	// connection.
	err = m.conn.WriteMessage(append(make([]byte, 8),
		conn.RemotePub().SerializeCompressed()...))
	if err != nil {
		return err
	}

	_, err = m.conn.Flush()
	if err != nil {
		return err
	}

	return nil
}

// handleConn runs in a goroutine to multiplex a connection through the
// mux.
func (m *Mux) handleConn(key connKey, conn MessageConnWithPubkey) error {
	// Stop connection on exit
	defer func() {
		m.mtx.Lock()
		m.stopConn(key)
		m.mtx.Unlock()
	}()

	for {
		// Read a message from the connection.
		//
		// TODO(aakselrod): handle oversized messages, fragmentation,
		// timeouts, partial reads, etc.
		n, err := conn.ReadNextHeader()
		if err != nil {
			return err
		}

		buf := make([]byte, n)
		buf, err = conn.ReadNextBody(buf)
		if err != nil {
			return err
		}

		// Multiplex the message through the mux connection.
		//
		// TODO(aakselrod): use a separate mutex for writing to the
		// mux connection.
		m.mtx.Lock()

		err = m.conn.WriteMessage(append(key[:], buf...))
		if err != nil {
			m.mtx.Unlock()
			return err
		}

		_, err = m.conn.Flush()
		if err != nil {
			m.mtx.Unlock()
			return err
		}

		m.mtx.Unlock()
	}
}
