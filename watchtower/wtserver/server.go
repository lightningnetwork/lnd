package wtserver

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

var (
	// ErrPeerAlreadyConnected signals that a peer with the same session id
	// is already active within the server.
	ErrPeerAlreadyConnected = errors.New("peer already connected")
)

// Config abstracts the primary components and dependencies of the server.
type Config struct {
	// DB provides persistent access to the server's sessions and for
	// storing state updates.
	DB DB

	// NodePrivKey is private key to be used in accepting new brontide
	// connections.
	NodePrivKey *btcec.PrivateKey

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
}

// Server houses the state required to handle watchtower peers. It's primary job
// is to accept incoming connections, and dispatch processing of the client
// message streams.
type Server struct {
	started  int32 // atomic
	shutdown int32 // atomic

	cfg *Config

	connMgr *connmgr.ConnManager

	clientMtx sync.RWMutex
	clients   map[wtdb.SessionID]Peer

	globalFeatures *lnwire.RawFeatureVector
	localFeatures  *lnwire.RawFeatureVector

	wg   sync.WaitGroup
	quit chan struct{}
}

// New creates a new server to handle watchtower clients. The server will accept
// clients connecting to the listener addresses, and allows them to open
// sessions and send state updates.
func New(cfg *Config) (*Server, error) {
	localFeatures := lnwire.NewRawFeatureVector(
		wtwire.WtSessionsOptional,
	)

	s := &Server{
		cfg:            cfg,
		clients:        make(map[wtdb.SessionID]Peer),
		globalFeatures: lnwire.NewRawFeatureVector(),
		localFeatures:  localFeatures,
		quit:           make(chan struct{}),
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
	// Already running?
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		return nil
	}

	log.Infof("Starting watchtower server")

	s.connMgr.Start()

	log.Infof("Watchtower server started successfully")

	return nil
}

// Stop shutdowns down the server's listeners and any active requests.
func (s *Server) Stop() error {
	// Bail if we're already shutting down.
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		return nil
	}

	log.Infof("Stopping watchtower server")

	s.connMgr.Stop()

	close(s.quit)
	s.wg.Wait()

	log.Infof("Watchtower server stopped successfully")

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
	s.wg.Add(1)
	go s.handleClient(peer)
}

// handleClient processes a series watchtower messages sent by a client. The
// client may either send:
//  * a single CreateSession message.
//  * a series of StateUpdate messages.
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
	defer s.removePeer(&id)

	msg, err := s.readMessage(peer)
	remoteInit, ok := msg.(*wtwire.Init)
	if !ok {
		log.Errorf("Client %s@%s did not send Init msg as first "+
			"message", id, peer.RemoteAddr())
		return
	}

	localInit := wtwire.NewInitMessage(
		s.localFeatures, s.globalFeatures,
	)

	err = s.sendMessage(peer, localInit)
	if err != nil {
		log.Errorf("Unable to send Init msg to %s: %v", id, err)
		return
	}

	if err = s.handleInit(localInit, remoteInit); err != nil {
		log.Errorf("Cannot support client %s: %v", id, err)
		return
	}

	// stateUpdateOnlyMode will become true if the client's first message is
	// a StateUpdate. If instead, it is a CreateSession, this method will exit
	// immediately after replying. We track this to ensure that the client
	// can't send a CreateSession after having already sent a StateUpdate.
	var stateUpdateOnlyMode bool
	for {
		select {
		case <-s.quit:
			return
		default:
		}

		nextMsg, err := s.readMessage(peer)
		if err != nil {
			log.Errorf("Unable to read watchtower msg from %x: %v",
				id[:], err)
			return
		}

		// Process the request according to the message's type.
		switch msg := nextMsg.(type) {

		// A CreateSession indicates a request to establish a new session
		// with our watchtower.
		case *wtwire.CreateSession:
			// Ensure CreateSession can only be sent as the first
			// message.
			if stateUpdateOnlyMode {
				log.Errorf("client %x sent CreateSession after "+
					"StateUpdate", id)
				return
			}

			log.Infof("Received CreateSession from %s, "+
				"version=%d nupdates=%d rewardrate=%d "+
				"sweepfeerate=%d", id, msg.BlobVersion,
				msg.MaxUpdates, msg.RewardRate,
				msg.SweepFeeRate)

			// Attempt to open a new session for this client.
			err := s.handleCreateSession(peer, &id, msg)
			if err != nil {
				log.Errorf("unable to handle CreateSession "+
					"from %s: %v", id, err)
			}

			// Exit after replying to CreateSession.
			return

		// A StateUpdate indicates an existing client attempting to
		// back-up a revoked commitment state.
		case *wtwire.StateUpdate:
			log.Infof("Received SessionUpdate from %s, seqnum=%d "+
				"lastapplied=%d complete=%v hint=%x", id,
				msg.SeqNum, msg.LastApplied, msg.IsComplete,
				msg.Hint[:])

			// Try to accept the state update from the client.
			err := s.handleStateUpdate(peer, &id, msg)
			if err != nil {
				log.Errorf("unable to handle StateUpdate "+
					"from %s: %v", id, err)
				return
			}

			// If the client signals that this is last StateUpdate
			// message, we can disconnect the client.
			if msg.IsComplete == 1 {
				return
			}

			// The client has signaled that more StateUpdates are
			// yet to come. Enter state-update-only mode to disallow
			// future sends of CreateSession messages.
			stateUpdateOnlyMode = true

		default:
			log.Errorf("received unsupported message type: %T "+
				"from %s", nextMsg, id)
			return
		}
	}
}

// handleInit accepts the local and remote Init messages, and verifies that the
// client is not requesting any required features that are unknown to the tower.
func (s *Server) handleInit(localInit, remoteInit *wtwire.Init) error {
	remoteLocalFeatures := lnwire.NewFeatureVector(
		remoteInit.LocalFeatures, wtwire.LocalFeatures,
	)
	remoteGlobalFeatures := lnwire.NewFeatureVector(
		remoteInit.GlobalFeatures, wtwire.GlobalFeatures,
	)

	unknownLocalFeatures := remoteLocalFeatures.UnknownRequiredFeatures()
	if len(unknownLocalFeatures) > 0 {
		err := fmt.Errorf("Peer set unknown local feature bits: %v",
			unknownLocalFeatures)
		return err
	}

	unknownGlobalFeatures := remoteGlobalFeatures.UnknownRequiredFeatures()
	if len(unknownGlobalFeatures) > 0 {
		err := fmt.Errorf("Peer set unknown global feature bits: %v",
			unknownGlobalFeatures)
		return err
	}

	return nil
}

// handleCreateSession processes a CreateSession message from the peer, and returns
// a CreateSessionReply in response. This method will only succeed if no existing
// session info is known about the session id. If an existing session is found,
// the reward address is returned in case the client lost our reply.
func (s *Server) handleCreateSession(peer Peer, id *wtdb.SessionID,
	init *wtwire.CreateSession) error {

	// TODO(conner): validate accept against policy

	// Query the db for session info belonging to the client's session id.
	existingInfo, err := s.cfg.DB.GetSessionInfo(id)
	switch {

	// We already have a session corresponding to this session id, return an
	// error signaling that it already exists in our database. We return the
	// reward address to the client in case they were not able to process
	// our reply earlier.
	case err == nil:
		log.Debugf("Already have session for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CreateSessionCodeAlreadyExists,
			[]byte(existingInfo.RewardAddress),
		)

	// Some other database error occurred, return a temporary failure.
	case err != wtdb.ErrSessionNotFound:
		log.Errorf("unable to load session info for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CodeTemporaryFailure, nil,
		)
	}

	// Now that we've established that this session does not exist in the
	// database, retrieve the sweep address that will be given to the
	// client. This address is to be included by the client when signing
	// sweep transactions destined for this tower, if its negotiated output
	// is not dust.
	rewardAddress, err := s.cfg.NewAddress()
	if err != nil {
		log.Errorf("unable to generate reward addr for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CodeTemporaryFailure, nil,
		)
	}

	rewardAddrBytes := rewardAddress.ScriptAddress()

	// TODO(conner): create invoice for upfront payment

	// Assemble the session info using the agreed upon parameters, reward
	// address, and session id.
	info := wtdb.SessionInfo{
		ID:            *id,
		Version:       init.BlobVersion,
		MaxUpdates:    init.MaxUpdates,
		RewardRate:    init.RewardRate,
		SweepFeeRate:  init.SweepFeeRate,
		RewardAddress: rewardAddrBytes,
	}

	// Insert the session info into the watchtower's database. If
	// successful, the session will now be ready for use.
	err = s.cfg.DB.InsertSessionInfo(&info)
	if err != nil {
		log.Errorf("unable to create session for %s", id)
		return s.replyCreateSession(
			peer, id, wtwire.CodeTemporaryFailure, nil,
		)
	}

	log.Infof("Accepted session for %s", id)

	return s.replyCreateSession(
		peer, id, wtwire.CodeOK, rewardAddrBytes,
	)
}

// handleStateUpdate processes a StateUpdate message request from a client. An
// attempt will be made to insert the update into the db, where it is validated
// against the client's session. The possible errors are then mapped back to
// StateUpdateCodes specified by the watchtower wire protocol, and sent back
// using a StateUpdateReply message.
func (s *Server) handleStateUpdate(peer Peer, id *wtdb.SessionID,
	update *wtwire.StateUpdate) error {

	var (
		lastApplied uint16
		failCode    wtwire.ErrorCode
		err         error
	)

	sessionUpdate := wtdb.SessionStateUpdate{
		ID:            *id,
		Hint:          update.Hint,
		SeqNum:        update.SeqNum,
		LastApplied:   update.LastApplied,
		EncryptedBlob: update.EncryptedBlob,
	}

	lastApplied, err = s.cfg.DB.InsertStateUpdate(&sessionUpdate)
	switch {
	case err == nil:
		log.Infof("State update %d accepted for %s",
			update.SeqNum, id)

		failCode = wtwire.CodeOK

	// Return a permanent failure if a client tries to send an update for
	// which we have no session.
	case err == wtdb.ErrSessionNotFound:
		failCode = wtwire.CodePermanentFailure

	case err == wtdb.ErrSeqNumAlreadyApplied:
		failCode = wtwire.CodePermanentFailure

		// TODO(conner): remove session state for protocol
		// violation. Could also double as clean up method for
		// session-related state.

	case err == wtdb.ErrLastAppliedReversion:
		failCode = wtwire.StateUpdateCodeClientBehind

	case err == wtdb.ErrSessionConsumed:
		failCode = wtwire.StateUpdateCodeMaxUpdatesExceeded

	case err == wtdb.ErrUpdateOutOfOrder:
		failCode = wtwire.StateUpdateCodeSeqNumOutOfOrder

	default:
		failCode = wtwire.CodeTemporaryFailure
	}

	return s.replyStateUpdate(
		peer, id, failCode, lastApplied,
	)
}

// connFailure is a default error used when a request failed with a non-zero
// error code.
type connFailure struct {
	ID   wtdb.SessionID
	Code uint16
}

// Error displays the SessionID and Code that caused the connection failure.
func (f *connFailure) Error() string {
	return fmt.Sprintf("connection with %s failed with code=%v",
		f.ID, f.Code,
	)
}

// replyCreateSession sends a response to a CreateSession from a client. If the
// status code in the reply is OK, the error from the write will be bubbled up.
// Otherwise, this method returns a connection error to ensure we don't continue
// communication with the client.
func (s *Server) replyCreateSession(peer Peer, id *wtdb.SessionID,
	code wtwire.ErrorCode, data []byte) error {

	msg := &wtwire.CreateSessionReply{
		Code: code,
		Data: data,
	}

	err := s.sendMessage(peer, msg)
	if err != nil {
		log.Errorf("unable to send CreateSessionReply to %s", id)
	}

	// Return the write error if the request succeeded.
	if code == wtwire.CodeOK {
		return err
	}

	// Otherwise the request failed, return a connection failure to
	// disconnect the client.
	return &connFailure{
		ID:   *id,
		Code: uint16(code),
	}
}

// replyStateUpdate sends a response to a StateUpdate from a client. If the
// status code in the reply is OK, the error from the write will be bubbled up.
// Otherwise, this method returns a connection error to ensure we don't continue
// communication with the client.
func (s *Server) replyStateUpdate(peer Peer, id *wtdb.SessionID,
	code wtwire.StateUpdateCode, lastApplied uint16) error {

	msg := &wtwire.StateUpdateReply{
		Code:        code,
		LastApplied: lastApplied,
	}

	err := s.sendMessage(peer, msg)
	if err != nil {
		log.Errorf("unable to send StateUpdateReply to %s", id)
	}

	// Return the write error if the request succeeded.
	if code == wtwire.CodeOK {
		return err
	}

	// Otherwise the request failed, return a connection failure to
	// disconnect the client.
	return &connFailure{
		ID:   *id,
		Code: uint16(code),
	}
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
		err = fmt.Errorf("unable to set read deadline: %v", err)
		return nil, err
	}

	// Pull the next message off the wire, and parse it according to the
	// watchtower wire specification.
	rawMsg, err := peer.ReadNextMessage()
	if err != nil {
		err = fmt.Errorf("unable to read message: %v", err)
		return nil, err
	}

	msgReader := bytes.NewReader(rawMsg)
	nextMsg, err := wtwire.ReadMessage(msgReader, 0)
	if err != nil {
		err = fmt.Errorf("unable to parse message: %v", err)
		return nil, err
	}

	return nextMsg, nil
}

// sendMessage sends a watchtower wire message to the target peer.
func (s *Server) sendMessage(peer Peer, msg wtwire.Message) error {
	// TODO(conner): use buffer pool?

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		err = fmt.Errorf("unable to encode msg: %v", err)
		return err
	}

	err = peer.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	if err != nil {
		err = fmt.Errorf("unable to set write deadline: %v", err)
		return err
	}

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
func (s *Server) removePeer(id *wtdb.SessionID) {
	log.Infof("Releasing incoming peer %s", id)

	s.clientMtx.Lock()
	peer, ok := s.clients[*id]
	delete(s.clients, *id)
	s.clientMtx.Unlock()

	if ok {
		peer.Close()
	}
}

// noDial is a dummy dial method passed to the server's connmgr.
func noDial(_ net.Addr) (net.Conn, error) {
	return nil, fmt.Errorf("watchtower cannot make outgoing conns")
}
