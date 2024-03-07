package wtserver

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

var (
	// ErrPeerAlreadyConnected signals that a peer with the same session id
	// is already active within the server.
	ErrPeerAlreadyConnected = errors.New("peer already connected")

	// ErrServerExiting signals that a request could not be processed
	// because the server has been requested to shut down.
	ErrServerExiting = errors.New("server shutting down")
)

// Config abstracts the primary components and dependencies of the server.
type Config struct {
	// DB provides persistent access to the server's sessions and for
	// storing state updates.
	DB DB

	// NodeKeyECDH is the ECDH capable wrapper of the key to be used in
	// accepting new brontide connections.
	NodeKeyECDH keychain.SingleKeyECDH

	// Listeners specifies which address to which clients may connect.
	Listeners []net.Listener

	// ReadTimeout specifies how long a client may go without sending a
	// message.
	ReadTimeout time.Duration

	// WriteTimeout specifies how long a client may go without reading a
	// message from the other end, if the connection has stopped buffering
	// the server's replies.
	WriteTimeout time.Duration

	// NewAddress is used to generate reward addresses, where a cut of
	// successfully sent funds can be received.
	NewAddress func() (btcutil.Address, error)

	// ChainHash identifies the network that the server is watching.
	ChainHash chainhash.Hash

	// NoAckCreateSession causes the server to not reply to create session
	// requests, this should only be used for testing.
	NoAckCreateSession bool

	// NoAckUpdates causes the server to not acknowledge state updates, this
	// should only be used for testing.
	NoAckUpdates bool

	// DisableReward causes the server to reject any session creation
	// attempts that request rewards.
	DisableReward bool
}

// Server houses the state required to handle watchtower peers. It's primary job
// is to accept incoming connections, and dispatch processing of the client
// message streams.
type Server struct {
	started sync.Once
	stopped sync.Once

	cfg *Config

	connMgr *connmgr.ConnManager

	clientMtx sync.RWMutex
	clients   map[wtdb.SessionID]Peer

	newPeers chan Peer

	localInit *wtwire.Init

	wg   sync.WaitGroup
	quit chan struct{}
}

// New creates a new server to handle watchtower clients. The server will accept
// clients connecting to the listener addresses, and allows them to open
// sessions and send state updates.
func New(cfg *Config) (*Server, error) {
	localInit := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(
			wtwire.AltruistSessionsOptional,
			wtwire.AnchorCommitOptional,
		),
		cfg.ChainHash,
	)

	s := &Server{
		cfg:       cfg,
		clients:   make(map[wtdb.SessionID]Peer),
		newPeers:  make(chan Peer),
		localInit: localInit,
		quit:      make(chan struct{}),
	}

	connMgr, err := connmgr.New(&connmgr.Config{
		Listeners: cfg.Listeners,
		OnAccept:  s.inboundPeerConnected,
		Dial:      noDial,
	})
	if err != nil {
		return nil, err
	}

	s.connMgr = connMgr

	return s, nil
}

// Start begins listening on the server's listeners.
func (s *Server) Start() error {
	s.started.Do(func() {
		log.Infof("Starting watchtower server")

		s.wg.Add(1)
		go s.peerHandler()

		s.connMgr.Start()

		log.Infof("Watchtower server started successfully")
	})
	return nil
}

// Stop shutdowns down the server's listeners and any active requests.
func (s *Server) Stop() error {
	s.stopped.Do(func() {
		log.Infof("Stopping watchtower server")

		s.connMgr.Stop()

		close(s.quit)
		s.wg.Wait()

		log.Infof("Watchtower server stopped successfully")
	})
	return nil
}

// inboundPeerConnected is the callback given to the connection manager, and is
// called each time a new connection is made to the watchtower. This method
// proxies the new peers by filtering out those that do not satisfy the
// server.Peer interface, and closes their connection. Successful connections
// will be passed on to the public InboundPeerConnected method.
func (s *Server) inboundPeerConnected(c net.Conn) {
	peer, ok := c.(Peer)
	if !ok {
		log.Warnf("incoming connection %T does not satisfy "+
			"server.Peer interface", c)
		c.Close()
		return
	}

	s.InboundPeerConnected(peer)
}

// InboundPeerConnected accepts a server.Peer, and handles the request submitted
// by the client. This method serves also as a public endpoint for locally
// registering new clients with the server.
func (s *Server) InboundPeerConnected(peer Peer) {
	select {
	case s.newPeers <- peer:
	case <-s.quit:
	}
}

// peerHandler processes newly accepted peers and spawns a client handler for
// each. The peerHandler is used to ensure that waitgrouped client handlers are
// spawned from a waitgrouped goroutine.
func (s *Server) peerHandler() {
	defer s.wg.Done()
	defer s.removeAllPeers()

	for {
		select {
		case peer := <-s.newPeers:
			s.wg.Add(1)
			go s.handleClient(peer)

		case <-s.quit:
			return
		}
	}
}

// handleClient processes a series watchtower messages sent by a client. The
// client may either send:
//   - a single CreateSession message.
//   - a series of StateUpdate messages.
//
// This method uses the server's peer map to ensure at most one peer using the
// same session id can enter the main event loop. The connection will be
// dropped by the watchtower if no messages are sent or received by the
// configured Read/WriteTimeouts.
//
// NOTE: This method MUST be run as a goroutine.
func (s *Server) handleClient(peer Peer) {
	defer s.wg.Done()

	// Use the connection's remote pubkey as the client's session id.
	id := wtdb.NewSessionIDFromPubKey(peer.RemotePub())

	// Register this peer in the server's client map, and defer the
	// connection's cleanup. If the peer already exists, we will close the
	// connection and exit immediately.
	err := s.addPeer(&id, peer)
	if err != nil {
		peer.Close()
		return
	}
	defer s.removePeer(&id, peer.RemoteAddr())

	msg, err := s.readMessage(peer)
	if err != nil {
		log.Errorf("Unable to read message from client %s@%s: %v",
			id, peer.RemoteAddr(), err)
		return
	}

	remoteInit, ok := msg.(*wtwire.Init)
	if !ok {
		log.Errorf("Client %s@%s did not send Init msg as first "+
			"message", id, peer.RemoteAddr())
		return
	}

	err = s.sendMessage(peer, s.localInit)
	if err != nil {
		log.Errorf("Unable to send Init msg to %s: %v", id, err)
		return
	}

	err = s.localInit.CheckRemoteInit(remoteInit, wtwire.FeatureNames)
	if err != nil {
		log.Errorf("Cannot support client %s: %v", id, err)
		return
	}

	nextMsg, err := s.readMessage(peer)
	if err != nil {
		log.Errorf("Unable to read watchtower msg from %s: %v",
			id, err)
		return
	}

	switch msg := nextMsg.(type) {
	case *wtwire.CreateSession:
		// Attempt to open a new session for this client.
		err = s.handleCreateSession(peer, &id, msg)
		if err != nil {
			log.Errorf("Unable to handle CreateSession "+
				"from %s: %v", id, err)
		}

	case *wtwire.DeleteSession:
		err = s.handleDeleteSession(peer, &id)
		if err != nil {
			log.Errorf("Unable to handle DeleteSession "+
				"from %s: %v", id, err)
		}

	case *wtwire.StateUpdate:
		err = s.handleStateUpdates(peer, &id, msg)
		if err != nil {
			log.Errorf("Unable to handle StateUpdate "+
				"from %s: %v", id, err)
		}

	default:
		log.Errorf("Received unsupported message type: %T "+
			"from %s", nextMsg, id)
	}
}

// connFailure is a default error used when a request failed with a non-zero
// error code.
type connFailure struct {
	ID   wtdb.SessionID
	Code wtwire.ErrorCode
}

// Error displays the SessionID and Code that caused the connection failure.
func (f *connFailure) Error() string {
	return fmt.Sprintf("connection with %s failed with code=%s",
		f.ID, f.Code,
	)
}

// readMessage receives and parses the next message from the given Peer. An
// error is returned if a message is not received before the server's read
// timeout, the read off the wire failed, or the message could not be
// deserialized.
func (s *Server) readMessage(peer Peer) (wtwire.Message, error) {
	// Set a read timeout to ensure we drop the client if not sent in a
	// timely manner.
	err := peer.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set read deadline: %w", err)
		return nil, err
	}

	// Pull the next message off the wire, and parse it according to the
	// watchtower wire specification.
	rawMsg, err := peer.ReadNextMessage()
	if err != nil {
		err = fmt.Errorf("unable to read message: %w", err)
		return nil, err
	}

	msgReader := bytes.NewReader(rawMsg)
	msg, err := wtwire.ReadMessage(msgReader, 0)
	if err != nil {
		err = fmt.Errorf("unable to parse message: %w", err)
		return nil, err
	}

	logMessage(peer, msg, true)

	return msg, nil
}

// sendMessage sends a watchtower wire message to the target peer.
func (s *Server) sendMessage(peer Peer, msg wtwire.Message) error {
	// TODO(conner): use buffer pool?

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		err = fmt.Errorf("unable to encode msg: %w", err)
		return err
	}

	err = peer.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set write deadline: %w", err)
		return err
	}

	logMessage(peer, msg, false)

	_, err = peer.Write(b.Bytes())
	return err
}

// addPeer stores a client in the server's client map. An error is returned if a
// client with the same session id already exists.
func (s *Server) addPeer(id *wtdb.SessionID, peer Peer) error {
	s.clientMtx.Lock()
	defer s.clientMtx.Unlock()

	if existingPeer, ok := s.clients[*id]; ok {
		log.Infof("Already connected to peer %s@%s, disconnecting %s",
			id, existingPeer.RemoteAddr(), peer.RemoteAddr())
		return ErrPeerAlreadyConnected
	}
	s.clients[*id] = peer

	log.Infof("Accepted incoming peer %s@%s",
		id, peer.RemoteAddr())

	return nil
}

// removePeer deletes a client from the server's client map. If a peer is found,
// this method will close the peer's connection.
func (s *Server) removePeer(id *wtdb.SessionID, addr net.Addr) {
	log.Infof("Releasing incoming peer %s@%s", id, addr)

	s.clientMtx.Lock()
	peer, ok := s.clients[*id]
	delete(s.clients, *id)
	s.clientMtx.Unlock()

	if ok {
		peer.Close()
	}
}

// removeAllPeers iterates through the server's current set of peers and closes
// all open connections.
func (s *Server) removeAllPeers() {
	s.clientMtx.Lock()
	defer s.clientMtx.Unlock()

	for id, peer := range s.clients {
		log.Infof("Releasing incoming peer %s@%s", id,
			peer.RemoteAddr())

		delete(s.clients, id)
		peer.Close()
	}
}

// logMessage writes information about a message exchanged with a remote peer,
// using directional prepositions to signal whether the message was sent or
// received.
func logMessage(peer Peer, msg wtwire.Message, read bool) {
	var action = "Received"
	var preposition = "from"
	if !read {
		action = "Sending"
		preposition = "to"
	}

	summary := wtwire.MessageSummary(msg)
	if len(summary) > 0 {
		summary = "(" + summary + ")"
	}

	log.Debugf("%s %s%v %s %x@%s", action, msg.MsgType(), summary,
		preposition, peer.RemotePub().SerializeCompressed(),
		peer.RemoteAddr())
}

// noDial is a dummy dial method passed to the server's connmgr.
func noDial(_ net.Addr) (net.Conn, error) {
	return nil, fmt.Errorf("watchtower cannot make outgoing conns")
}
