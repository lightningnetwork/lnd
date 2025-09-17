package lnp2p

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/tor"
)

// SimplePeer provides a lightweight P2P connection implementation.
type SimplePeer struct {
	// cfg is the configuration for this peer.
	cfg SimplePeerConfig

	// conn is the underlying Brontide connection.
	conn BrontideConn

	// localKey is the ephemeral key used for the connection.
	localKey keychain.SingleKeyECDH

	// timeouts are the resolved timeout values for this peer.
	timeouts TimeoutConfig

	// peerLog is the logger for this peer with proper prefix.
	peerLog btclog.Logger

	// state tracks the connection state.
	state atomic.Int32

	// isConnected indicates if the connection is established.
	isConnected atomic.Bool

	// msgChan is the channel for incoming messages.
	msgChan chan lnwire.Message

	// eventChan is the channel for connection events.
	eventChan chan ConnectionEvent

	// sendQueue is the channel for outgoing messages.
	sendQueue chan lnwire.Message

	// msgRouter is an optional message router for msgmux integration.
	msgRouter fn.Option[msgmux.Router]

	// broadcastMode indicates if dual-mode message handling is enabled.
	broadcastMode atomic.Bool

	stopOnce sync.Once
	wg       sync.WaitGroup
	quit     chan struct{}
}

// SimplePeerConfig contains configuration for SimplePeer.
type SimplePeerConfig struct {
	// KeyGenerator provides ephemeral key generation.
	KeyGenerator KeyGenerator

	// Target is the remote node to connect to.
	Target NodeAddress

	// Features to advertise in the init message.
	Features *lnwire.FeatureVector

	// Timeouts for various operations.
	Timeouts fn.Option[TimeoutConfig]

	// MsgRouter is an optional message router for msgmux integration.
	MsgRouter fn.Option[msgmux.Router]

	// Dialer is an optional network dialer. If not provided, defaults to
	// net.Dial.
	Dialer fn.Option[tor.DialFunc]

	// BrontideDialer is an optional brontide dialer. If not provided,
	// defaults to DefaultBrontideDialer.
	BrontideDialer fn.Option[BrontideDialer]

	// InitHandler is an optional callback for custom init message handling.
	InitHandler fn.Option[func(*lnwire.Init) error]

	// ReadSemaphore is an optional channel that controls when the peer
	// can read from the connection. Before each read attempt, the reader
	// will wait for a signal on this channel. This allows external control
	// over message reading timing. If the channel is created but never
	// written to, reads will block forever.
	ReadSemaphore fn.Option[chan struct{}]
}

// NewSimplePeer creates a new SimplePeer instance.
func NewSimplePeer(cfg SimplePeerConfig) (*SimplePeer, error) {
	// We'll start with some basic upfont validation of the config.
	if cfg.KeyGenerator == nil {
		return nil, fmt.Errorf("key generator is required")
	}
	if cfg.Target.Address == nil {
		return nil, fmt.Errorf("target address is required")
	}
	if cfg.Target.PubKey == nil {
		return nil, fmt.Errorf("target public key is required")
	}

	// If no features were passed in, we'll create a blank feature vector
	// for use. This may end up causing the peer to not connect to use based
	// on required fields.
	if cfg.Features == nil {
		rawFeatures := lnwire.NewRawFeatureVector()
		cfg.Features = lnwire.NewFeatureVector(rawFeatures, nil)
	}

	timeouts := cfg.Timeouts.UnwrapOr(DefaultTimeouts())

	// Create the peer-specific logger with appropriate prefix.
	peerPrefix := fmt.Sprintf("Peer(%x@%s):",
		cfg.Target.PubKey.SerializeCompressed(),
		cfg.Target.Address)
	peerLog := log.WithPrefix(peerPrefix)

	return &SimplePeer{
		cfg:       cfg,
		timeouts:  timeouts,
		peerLog:   peerLog,
		msgChan:   make(chan lnwire.Message, 100),
		eventChan: make(chan ConnectionEvent, 10),
		sendQueue: make(chan lnwire.Message, 100),
		msgRouter: cfg.MsgRouter,
		quit:      make(chan struct{}),
	}, nil
}

// Connect establishes a connection and completes the handshake.
func (p *SimplePeer) Connect(ctx context.Context) error {
	// Update the state, then send out an update that we're about to try to
	// connect to the remote peer.
	p.setState(StateConnecting)
	p.sendEvent(ConnectionEvent{
		State:     StateConnecting,
		Timestamp: time.Now(),
		Details:   fmt.Sprintf("Connecting to %s", p.cfg.Target),
	})

	p.peerLog.Infof("Connecting to remote peer")

	// For our brontide handshake, we'll need an ephemeral key, so we'll
	// generate that now.
	localKey, err := p.cfg.KeyGenerator.GenerateKey()
	if err != nil {
		p.setState(StateDisconnected)

		p.peerLog.Errorf("Failed to generate ephemeral key: %v", err)

		return fmt.Errorf("failed to generate ephemeral key: %w", err)
	}
	p.localKey = localKey

	// Next, we'll wrap the target address and pubkey into a net address
	// that can be used with the brontide API.
	netAddr := &lnwire.NetAddress{
		IdentityKey: p.cfg.Target.PubKey,
		Address:     p.cfg.Target.Address,
	}

	// At this point, we'll shift into the connection estasblishment, as
	// we're now about to dial the remote peer and perform the noise
	// handshake.
	p.setState(StateHandshaking)
	p.sendEvent(ConnectionEvent{
		State:     StateHandshaking,
		Timestamp: time.Now(),
		Details:   "Performing noise handshake",
	})

	p.peerLog.Debugf("Initiating noise handshake")

	// Use the provided network dialer or default to net.DialTimeout. This
	// enables callers to use tor or other custom dialers.
	defaultNetDialer := func(network, address string,
		timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout(network, address, timeout)
	}
	netDialer := p.cfg.Dialer.UnwrapOr(defaultNetDialer)

	defaultBrontideDialer := &DefaultBrontideDialer{}
	brontideDialer := p.cfg.BrontideDialer.UnwrapOr(defaultBrontideDialer)

	conn, err := brontideDialer.Dial(
		localKey, netAddr, p.timeouts.DialTimeout, netDialer,
	)
	if err != nil {
		p.setState(StateDisconnected)
		p.sendEvent(ConnectionEvent{
			State:     StateDisconnected,
			Timestamp: time.Now(),
			Error:     err,
			Details:   "Handshake failed",
		})

		p.peerLog.Errorf("Failed to establish connection: %v", err)

		return fmt.Errorf("failed to establish connection: %w", err)
	}
	p.conn = conn

	p.peerLog.Debugf("Noise handshake completed successfully")

	// Now that we're connected, we'll proceed with the init message
	// exchange.
	p.setState(StateInitializing)
	p.sendEvent(ConnectionEvent{
		State:     StateInitializing,
		Timestamp: time.Now(),
		Details:   "Exchanging init messages",
	})

	if err := p.exchangeInit(ctx); err != nil {
		p.conn.Close()

		p.setState(StateDisconnected)

		p.sendEvent(ConnectionEvent{
			State:     StateDisconnected,
			Timestamp: time.Now(),
			Error:     err,
			Details:   "Init exchange failed",
		})

		p.peerLog.Errorf("Failed to exchange init messages: %v", err)

		return fmt.Errorf("failed to exchange init: %w", err)
	}

	// Start optional msgmux router BEFORE message handlers to avoid missing
	// events.
	p.msgRouter.WhenSome(func(router msgmux.Router) {
		router.Start(ctx)
	})

	// At this point, we're fully connected, so we'll update our state and
	// launch the read and write handlers.
	p.setState(StateConnected)

	p.isConnected.Store(true)

	p.sendEvent(ConnectionEvent{
		State:     StateConnected,
		Timestamp: time.Now(),
		Details:   "Connection established",
	})

	p.peerLog.Infof("Connection established successfully")

	// Always start both read and write handlers.
	// The read handler will respect ReadSemaphore if set.
	p.wg.Add(2)
	go p.readHandler()
	go p.writeHandler()

	return nil
}

// exchangeInit handles the init message exchange between us and the remote
// peer.
func (p *SimplePeer) exchangeInit(ctx context.Context) error {
	// Copy feature bits from FeatureVector to RawFeatureVector.
	rawFeatures := lnwire.NewRawFeatureVector()
	if p.cfg.Features != nil {
		for feature := range p.cfg.Features.Features() {
			rawFeatures.Set(feature)
		}
	}

	ourInit := &lnwire.Init{
		GlobalFeatures: lnwire.NewRawFeatureVector(),
		Features:       rawFeatures,
	}

	// With the init constructed, we'll now encode then send it off to the
	// remote party.
	var buf bytes.Buffer
	if _, err := lnwire.WriteMessage(&buf, ourInit, 0); err != nil {
		return fmt.Errorf("failed to encode init: %w", err)
	}
	// Set write deadline before sending init.
	writeDeadline := time.Now().Add(p.timeouts.WriteTimeout)
	if err := p.conn.SetWriteDeadline(writeDeadline); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}
	if err := p.conn.WriteMessage(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to send init: %w", err)
	}
	if _, err := p.conn.Flush(); err != nil {
		return fmt.Errorf("failed to flush init: %w", err)
	}

	// Make sure to set a read deadline to avoid hanging forever if they
	// never respond.
	deadline := time.Now().Add(p.timeouts.InitTimeout)
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("failed to set read deadline: %w", err)
	}

	// Read their next message, this should be an init, otherwise it's a
	// protocol violation.
	remoteMsg, err := p.conn.ReadNextMessage()
	if err != nil {
		return fmt.Errorf("failed to read remote init: %w", err)
	}
	msg, err := lnwire.ReadMessage(bytes.NewReader(remoteMsg), 0)
	if err != nil {
		return fmt.Errorf("failed to parse remote init: %w", err)
	}
	remoteInit, ok := msg.(*lnwire.Init)
	if !ok {
		return fmt.Errorf("expected init message, got %T", msg)
	}

	// Clear the read deadline as we just relived the respond.
	if err := p.conn.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("failed to clear read deadline: %w", err)
	}

	// If a custom init handler was specified, we'll invoke it now.
	var initErr error
	p.cfg.InitHandler.WhenSome(func(handler func(*lnwire.Init) error) {
		initErr = handler(remoteInit)
	})

	return initErr
}

// waitForReadPermission waits for permission to read if a semaphore is configured.
// Returns true if we should continue reading, false if we should exit.
func (p *SimplePeer) waitForReadPermission() bool {
	// Check if we're shutting down.
	select {
	case <-p.quit:
		return false
	default:
	}

	// If no semaphore is configured, we can always read.
	if p.cfg.ReadSemaphore.IsNone() {
		return true
	}

	readSem := p.cfg.ReadSemaphore.UnsafeFromSome()

	// Wait for permission to read or quit signal.
	// Each read consumes one token from the channel.
	select {
	case <-readSem:
		// Got permission to read this message.
		return true
	case <-p.quit:
		return false
	}
}

// readHandler continuously reads messages from the connection.
func (p *SimplePeer) readHandler() {
	defer p.wg.Done()

	p.peerLog.Debugf("Read handler started")

	for {
		// Wait for permission to read (or check if we should exit).
		if !p.waitForReadPermission() {
			return
		}

		// Read next message.
		msgBytes, err := p.conn.ReadNextMessage()
		if err != nil {
			select {
			case <-p.quit:
				return
			default:
				p.peerLog.Errorf("Read error: %v", err)

				p.sendEvent(ConnectionEvent{
					State:     StateDisconnected,
					Timestamp: time.Now(),
					Error:     err,
					Details:   "Read error",
				})
				p.Close()
				return
			}
		}

		// Parse the message.
		msg, err := lnwire.ReadMessage(bytes.NewReader(msgBytes), 0)
		if err != nil {
			p.peerLog.Warnf("Failed to parse message: %v", err)

			// Continue reading despite parse error.
			continue
		}

		// Handle ping messages internally.
		if ping, ok := msg.(*lnwire.Ping); ok {
			p.peerLog.Tracef("Received ping, sending pong")

			p.handlePing(ping)
			continue
		}

		// Send to message channel for iterator.
		//
		// TODO(roasbeef): remove buffer?
		select {
		case p.msgChan <- msg:
		case <-p.quit:
			return
		}

		// If broadcast mode is enabled, also route to msgmux.
		if p.broadcastMode.Load() {
			p.msgRouter.WhenSome(func(router msgmux.Router) {
				peerMsg := msgmux.PeerMsg{
					Message: msg,
					PeerPub: *p.RemotePubKey(),
				}
				router.RouteMsg(peerMsg)
			})
		}
	}
}

// writeHandler processes the outgoing message queue.
func (p *SimplePeer) writeHandler() {
	defer p.wg.Done()

	p.peerLog.Debugf("Write handler started")

	for {
		select {
		case msg := <-p.sendQueue:
			var buf bytes.Buffer
			_, err := lnwire.WriteMessage(&buf, msg, 0)
			if err != nil {
				p.peerLog.Errorf("Failed to encode message %T: %v",
					msg, err)

				continue
			}

			writeDeadline := time.Now().Add(p.timeouts.WriteTimeout)
			err = p.conn.SetWriteDeadline(writeDeadline)
			if err != nil {
				p.peerLog.Errorf("Failed to set write deadline: %v",
					err)

				p.sendEvent(ConnectionEvent{
					State:     StateDisconnected,
					Timestamp: time.Now(),
					Error:     err,
					Details: "Failed to set write " +
						"deadline",
				})
				p.Close()
				return
			}

			if err := p.conn.WriteMessage(buf.Bytes()); err != nil {
				p.peerLog.Errorf("Write error: %v", err)

				p.sendEvent(ConnectionEvent{
					State:     StateDisconnected,
					Timestamp: time.Now(),
					Error:     err,
					Details:   "Write error",
				})

				p.Close()

				return
			}

			p.conn.Flush()

		case <-p.quit:
			return
		}
	}
}

// handlePing responds to ping messages with pongs.
func (p *SimplePeer) handlePing(ping *lnwire.Ping) {
	// Create pong data of requested size.
	pongData := make([]byte, ping.NumPongBytes)
	pong := lnwire.NewPong(pongData)

	// Send the pong.
	p.SendMessage(pong)
}

// SendMessage sends a message to the peer.
func (p *SimplePeer) SendMessage(msg lnwire.Message) error {
	if !p.isConnected.Load() {
		return fmt.Errorf("not connected")
	}

	select {
	case p.sendQueue <- msg:
		return nil
	case <-p.quit:
		return fmt.Errorf("peer shutting down")
	case <-time.After(p.timeouts.WriteTimeout):
		return fmt.Errorf("send queue full")
	}
}

// ReceiveMessages returns an iterator for incoming messages.
func (p *SimplePeer) ReceiveMessages() iter.Seq[lnwire.Message] {
	return func(yield func(lnwire.Message) bool) {
		for {
			select {
			case msg := <-p.msgChan:
				if !yield(msg) {
					return
				}
			case <-p.quit:
				return
			}
		}
	}
}

// ConnectionEvents returns an iterator for connection lifecycle events.
func (p *SimplePeer) ConnectionEvents() iter.Seq[ConnectionEvent] {
	return func(yield func(ConnectionEvent) bool) {
		for {
			select {
			case event := <-p.eventChan:
				if !yield(event) {
					return
				}
			case <-p.quit:
				return
			}
		}
	}
}

// EnableBroadcastMode enables dual-mode message handling (iterator + msgmux).
func (p *SimplePeer) EnableBroadcastMode(router msgmux.Router) {
	p.msgRouter = fn.Some(router)
	p.broadcastMode.Store(true)
}

// Close terminates the connection.
func (p *SimplePeer) Close() error {
	var closeErr error
	p.stopOnce.Do(func() {
		p.peerLog.Infof("Closing connection")

		p.setState(StateClosing)
		p.isConnected.Store(false)

		close(p.quit)

		p.msgRouter.WhenSome(func(router msgmux.Router) {
			router.Stop()
		})

		if p.conn != nil {
			closeErr = p.conn.Close()
		}

		p.wg.Wait()

		p.sendEvent(ConnectionEvent{
			State:     StateDisconnected,
			Timestamp: time.Now(),
			Details:   "Connection closed",
		})

		p.setState(StateDisconnected)
	})

	return closeErr
}

// LocalPubKey returns the local node's public key.
func (p *SimplePeer) LocalPubKey() *btcec.PublicKey {
	if p.localKey == nil {
		return nil
	}
	return p.localKey.PubKey()
}

// RemotePubKey returns the remote peer's public key.
func (p *SimplePeer) RemotePubKey() *btcec.PublicKey {
	if p.conn == nil {
		return nil
	}
	return p.conn.RemotePub()
}

// IsConnected returns true if the connection is established.
func (p *SimplePeer) IsConnected() bool {
	return p.isConnected.Load()
}

// RemoteAddr returns the remote address.
func (p *SimplePeer) RemoteAddr() net.Addr {
	if p.conn == nil {
		return nil
	}
	return p.conn.RemoteAddr()
}

// LocalAddr returns the local address.
func (p *SimplePeer) LocalAddr() net.Addr {
	if p.conn == nil {
		return nil
	}
	return p.conn.LocalAddr()
}

// setState updates the connection state.
func (p *SimplePeer) setState(state ConnectionState) {
	p.state.Store(int32(state))
}

// getState returns the current connection state.
func (p *SimplePeer) getState() ConnectionState {
	return ConnectionState(p.state.Load())
}

// sendEvent sends a connection event to the event channel.
func (p *SimplePeer) sendEvent(event ConnectionEvent) {
	select {
	case p.eventChan <- event:

	default:
	}
}

// Ensure SimplePeer implements P2PConnection.
var _ P2PConnection = (*SimplePeer)(nil)
