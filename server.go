package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/connmgr"
	"github.com/roasbeef/btcutil"
)

// server is the main server of the Lightning Network Daemon. The server houses
// global state pertaining to the wallet, database, and the rpcserver.
// Additionally, the server is also used as a central messaging bus to interact
// with any of its companion objects.
type server struct {
	started  int32 // atomic
	shutdown int32 // atomic

	// identityPriv is the private key used to authenticate any incoming
	// connections.
	identityPriv *btcec.PrivateKey

	// lightningID is the sha256 of the public key corresponding to our
	// long-term identity private key.
	lightningID [32]byte

	peersMtx   sync.RWMutex
	peersByID  map[int32]*peer
	peersByPub map[string]*peer

	rpcServer *rpcServer

	chainNotifier chainntnfs.ChainNotifier

	bio      lnwallet.BlockChainIO
	lnwallet *lnwallet.LightningWallet

	fundingMgr *fundingManager
	chanDB     *channeldb.DB

	htlcSwitch    *htlcSwitch
	invoices      *invoiceRegistry
	breachArbiter *breachArbiter

	chanRouter *routing.ChannelRouter

	utxoNursery *utxoNursery

	sphinx *sphinx.Router

	connMgr *connmgr.ConnManager

	pendingConnMtx     sync.RWMutex
	persistentConnReqs map[string][]*connmgr.ConnReq

	broadcastRequests chan *broadcastReq
	sendRequests      chan *sendReq

	newPeers  chan *peer
	donePeers chan *peer
	queries   chan interface{}

	// globalFeatures feature vector which affects HTLCs and thus are also
	// advertised to other nodes.
	globalFeatures *lnwire.FeatureVector

	// localFeatures is an feature vector which represent the features which
	// only affect the protocol between these two nodes.
	localFeatures *lnwire.FeatureVector

	wg   sync.WaitGroup
	quit chan struct{}
}

// newServer creates a new instance of the server which is to listen using the
// passed listener address.
func newServer(listenAddrs []string, notifier chainntnfs.ChainNotifier,
	bio lnwallet.BlockChainIO, wallet *lnwallet.LightningWallet,
	chanDB *channeldb.DB) (*server, error) {

	privKey, err := wallet.GetIdentitykey()
	if err != nil {
		return nil, err
	}
	privKey.Curve = btcec.S256()

	listeners := make([]net.Listener, len(listenAddrs))
	for i, addr := range listenAddrs {
		listeners[i], err = brontide.NewListener(privKey, addr)
		if err != nil {
			return nil, err
		}
	}

	serializedPubKey := privKey.PubKey().SerializeCompressed()
	s := &server{
		lnwallet:      wallet,
		bio:           bio,
		chainNotifier: notifier,
		chanDB:        chanDB,

		invoices:    newInvoiceRegistry(chanDB),
		utxoNursery: newUtxoNursery(chanDB, notifier, wallet),
		htlcSwitch:  newHtlcSwitch(),

		identityPriv: privKey,

		// TODO(roasbeef): derive proper onion key based on rotation
		// schedule
		sphinx:      sphinx.NewRouter(privKey, activeNetParams.Params),
		lightningID: sha256.Sum256(serializedPubKey),

		persistentConnReqs: make(map[string][]*connmgr.ConnReq),

		peersByID:  make(map[int32]*peer),
		peersByPub: make(map[string]*peer),

		newPeers:  make(chan *peer, 10),
		donePeers: make(chan *peer, 10),

		broadcastRequests: make(chan *broadcastReq),
		sendRequests:      make(chan *sendReq),

		globalFeatures: globalFeatures,
		localFeatures:  localFeatures,

		queries: make(chan interface{}),
		quit:    make(chan struct{}),
	}

	// If the debug HTLC flag is on, then we invoice a "master debug"
	// invoice which all outgoing payments will be sent and all incoming
	// HTLCs with the debug R-Hash immediately settled.
	if cfg.DebugHTLC {
		kiloCoin := btcutil.Amount(btcutil.SatoshiPerBitcoin * 1000)
		s.invoices.AddDebugInvoice(kiloCoin, *debugPre)
		srvrLog.Debugf("Debug HTLC invoice inserted, preimage=%x, hash=%x",
			debugPre[:], debugHash[:])
	}

	// If external IP addresses have been specified, add those to the list
	// of this server's addresses.
	selfAddrs := make([]net.Addr, 0)
	for _, ip := range cfg.ExternalIPs {
		addr, err := net.ResolveTCPAddr("tcp", ip)
		if err != nil {
			return nil, err
		}
		selfAddrs = append(selfAddrs, addr)
	}

	chanGraph := chanDB.ChannelGraph()
	self := &channeldb.LightningNode{
		LastUpdate: time.Now(),
		Addresses:  selfAddrs,
		PubKey:     privKey.PubKey(),
		// TODO(roasbeef): make alias configurable
		Alias: hex.EncodeToString(serializedPubKey[:10]),
	}
	if err := chanGraph.SetSourceNode(self); err != nil {
		return nil, err
	}

	s.chanRouter, err = routing.New(routing.Config{
		Graph:        chanGraph,
		Chain:        bio,
		Notifier:     notifier,
		Broadcast:    s.broadcastMessage,
		SendMessages: s.sendToPeer,
		SendToSwitch: func(firstHop *btcec.PublicKey,
			htlcAdd *lnwire.UpdateAddHTLC) ([32]byte, error) {

			firstHopPub := firstHop.SerializeCompressed()
			destInterface := chainhash.Hash(sha256.Sum256(firstHopPub))

			return s.htlcSwitch.SendHTLC(&htlcPacket{
				dest: destInterface,
				msg:  htlcAdd,
			})
		},
	})
	if err != nil {
		return nil, err
	}

	s.rpcServer = newRPCServer(s)
	s.breachArbiter = newBreachArbiter(wallet, chanDB, notifier, s.htlcSwitch)

	s.fundingMgr, err = newFundingManager(fundingConfig{
		IDKey:    s.identityPriv.PubKey(),
		Wallet:   wallet,
		Notifier: s.chainNotifier,
		SendToRouter: func(msg lnwire.Message) {
			s.chanRouter.ProcessRoutingMessage(msg,
				s.identityPriv.PubKey())
		},
		ArbiterChan: s.breachArbiter.newContracts,
		SendToPeer:  s.sendToPeer,
		FindPeer:    s.findPeer,
		FindChannel: s.rpcServer.fetchActiveChannel,
	})
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): introduce closure and config system to decouple the
	// initialization above ^

	// Create the connection manager which will be responsible for
	// maintaining persistent outbound connections and also accepting new
	// incoming connections
	cmgr, err := connmgr.New(&connmgr.Config{
		Listeners:      listeners,
		OnAccept:       s.inboundPeerConnected,
		RetryDuration:  time.Second * 5,
		TargetOutbound: 100,
		GetNewAddress:  nil,
		Dial:           noiseDial(s.identityPriv),
		OnConnection:   s.outboundPeerConnected,
	})
	if err != nil {
		return nil, err
	}
	s.connMgr = cmgr

	// In order to promote liveness of our active channels, instruct the
	// connection manager to attempt to establish and maintain persistent
	// connections to all our direct channel counterparties.
	linkNodes, err := s.chanDB.FetchAllLinkNodes()
	if err != nil && err != channeldb.ErrLinkNodesNotFound {
		return nil, err
	}
	for _, node := range linkNodes {
		pubStr := string(node.IdentityPub.SerializeCompressed())

		// In case a node has multiple addresses, attempt to connect to
		// each of them.
		for _, address := range node.Addresses {
			// Create a wrapper address which couples the IP and the pubkey
			// so the brontide authenticated connection can be established.
			lnAddr := &lnwire.NetAddress{
				IdentityKey: node.IdentityPub,
				Address:     address,
			}
			srvrLog.Debugf("Attempting persistent connection to channel "+
				"peer %v", lnAddr)

			// Send the persistent connection request to the connection
			// manager, saving the request itself so we can cancel/restart
			// the process as needed.
			// TODO(roasbeef): use default addr
			connReq := &connmgr.ConnReq{
				Addr:      lnAddr,
				Permanent: true,
			}
			s.persistentConnReqs[pubStr] =
				append(s.persistentConnReqs[pubStr], connReq)
			go s.connMgr.Connect(connReq)
		}
	}

	return s, nil
}

// Start starts the main daemon server, all requested listeners, and any helper
// goroutines.
func (s *server) Start() error {
	// Already running?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	// Start the notification server. This is used so channel management
	// goroutines can be notified when a funding transaction reaches a
	// sufficient number of confirmations, or when the input for the
	// funding transaction is spent in an attempt at an uncooperative close
	// by the counterparty.
	if err := s.chainNotifier.Start(); err != nil {
		return err
	}

	if err := s.rpcServer.Start(); err != nil {
		return err
	}
	if err := s.fundingMgr.Start(); err != nil {
		return err
	}
	if err := s.htlcSwitch.Start(); err != nil {
		return err
	}
	if err := s.utxoNursery.Start(); err != nil {
		return err
	}
	if err := s.breachArbiter.Start(); err != nil {
		return err
	}
	if err := s.chanRouter.Start(); err != nil {
		return err
	}

	s.wg.Add(1)
	go s.queryHandler()

	return nil
}

// Stop gracefully shutsdown the main daemon server. This function will signal
// any active goroutines, or helper objects to exit, then blocks until they've
// all successfully exited. Additionally, any/all listeners are closed.
func (s *server) Stop() error {
	// Bail if we're already shutting down.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// Shutdown the wallet, funding manager, and the rpc server.
	s.chainNotifier.Stop()
	s.rpcServer.Stop()
	s.fundingMgr.Stop()
	s.chanRouter.Stop()
	s.htlcSwitch.Stop()
	s.utxoNursery.Stop()
	s.breachArbiter.Stop()

	s.lnwallet.Shutdown()

	// Signal all the lingering goroutines to quit.
	close(s.quit)
	s.wg.Wait()

	return nil
}

// WaitForShutdown blocks all goroutines have been stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}

// broadcastReq is a message sent to the server by a related subsystem when it
// wishes to broadcast one or more messages to all connected peers. Thi
type broadcastReq struct {
	ignore *btcec.PublicKey
	msgs   []lnwire.Message

	errChan chan error // MUST be buffered.
}

// broadcastMessage sends a request to the server to broadcast a set of
// messages to all peers other than the one specified by the `skip` parameter.
func (s *server) broadcastMessage(skip *btcec.PublicKey, msgs ...lnwire.Message) error {
	errChan := make(chan error, 1)

	msgsToSend := make([]lnwire.Message, 0, len(msgs))
	msgsToSend = append(msgsToSend, msgs...)
	broadcastReq := &broadcastReq{
		ignore:  skip,
		msgs:    msgsToSend,
		errChan: errChan,
	}

	select {
	case s.broadcastRequests <- broadcastReq:
	case <-s.quit:
		return errors.New("server shutting down")
	}

	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return errors.New("server shutting down")
	}
}

// sendReq is  message sent to the server by a related subsystem which it
// wishes to send a set of messages to a specified peer.
type sendReq struct {
	target *btcec.PublicKey
	msgs   []lnwire.Message

	errChan chan error
}

// sendToPeer send a message to the server telling it to send the specific set
// of message to a particular peer. If the peer connect be found, then this
// method will return a non-nil error.
func (s *server) sendToPeer(target *btcec.PublicKey, msgs ...lnwire.Message) error {
	errChan := make(chan error, 1)

	msgsToSend := make([]lnwire.Message, 0, len(msgs))
	msgsToSend = append(msgsToSend, msgs...)
	sMsg := &sendReq{
		target:  target,
		msgs:    msgsToSend,
		errChan: errChan,
	}

	select {
	case s.sendRequests <- sMsg:
	case <-s.quit:
		return errors.New("server shutting down")
	}

	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return errors.New("server shutting down")
	}
}

// findPeer will return the peer that corresponds to the passed in public key.
// This function is used by the funding manager, allowing it to update the
// daemon's local representation of the remote peer.
func (s *server) findPeer(peerKey *btcec.PublicKey) (*peer, error) {
	serializedIDKey := string(peerKey.SerializeCompressed())

	s.peersMtx.RLock()
	peer := s.peersByPub[serializedIDKey]
	s.peersMtx.RUnlock()

	if peer == nil {
		return nil, errors.New("Peer not found. Pubkey: " +
			string(peerKey.SerializeCompressed()))
	}

	return peer, nil
}

// peerConnected is a function that handles initialization a newly connected
// peer by adding it to the server's global list of all active peers, and
// starting all the goroutines the peer needs to function properly.
func (s *server) peerConnected(conn net.Conn, connReq *connmgr.ConnReq, inbound bool) {
	brontideConn := conn.(*brontide.Conn)
	peerAddr := &lnwire.NetAddress{
		IdentityKey: brontideConn.RemotePub(),
		Address:     conn.RemoteAddr().(*net.TCPAddr),
		ChainNet:    activeNetParams.Net,
	}

	// Now that we've established a connection, create a peer, and
	// it to the set of currently active peers.
	p, err := newPeer(conn, connReq, s, peerAddr, inbound)
	if err != nil {
		srvrLog.Errorf("unable to create peer %v", err)
		if connReq != nil {
			s.connMgr.Remove(connReq.ID())
		}
		return
	}

	// TODO(roasbeef): update IP address for link-node
	//  * also mark last-seen, do it one single transaction?

	if err := p.Start(); err != nil {
		srvrLog.Errorf("unable to start peer: %v", err)
		if p.connReq != nil {
			s.connMgr.Remove(p.connReq.ID())
		}
		p.Disconnect()
		return
	}

	s.newPeers <- p
}

// inboundPeerConnected initializes a new peer in response to a new inbound
// connection.
func (s *server) inboundPeerConnected(conn net.Conn) {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	srvrLog.Tracef("New inbound connection from %v", conn.RemoteAddr())

	nodePub := conn.(*brontide.Conn).RemotePub()

	// If we already have an outbound connection to this peer, simply drop
	// the connection.
	pubStr := string(nodePub.SerializeCompressed())
	if _, ok := s.peersByPub[pubStr]; ok {
		srvrLog.Errorf("Received inbound connection from peer %x, but "+
			"already connected, dropping conn",
			nodePub.SerializeCompressed())
		conn.Close()
		return
	}

	// However, if we receive an incoming connection from a peer we're
	// attempting to maintain a persistent connection with then we need to
	// cancel the ongoing connection attempts to ensure that we don't end
	// up with a duplicate connecting to the same peer.
	s.pendingConnMtx.RLock()
	if connReqs, ok := s.persistentConnReqs[pubStr]; ok {
		for _, connReq := range connReqs {
			s.connMgr.Remove(connReq.ID())
		}
		delete(s.persistentConnReqs, pubStr)
	}
	s.pendingConnMtx.RUnlock()

	s.peerConnected(conn, nil, false)
}

// outboundPeerConnected initializes a new peer in response to a new outbound
// connection.
func (s *server) outboundPeerConnected(connReq *connmgr.ConnReq, conn net.Conn) {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	srvrLog.Tracef("Established connection to: %v", conn.RemoteAddr())

	nodePub := conn.(*brontide.Conn).RemotePub()

	// If we already have an inbound connection from this peer, simply drop
	// the connection.
	pubStr := string(nodePub.SerializeCompressed())
	if _, ok := s.peersByPub[pubStr]; ok {
		srvrLog.Errorf("Established outbound connection to peer %x, but "+
			"already connected, dropping conn",
			nodePub.SerializeCompressed())
		s.connMgr.Remove(connReq.ID())
		conn.Close()
		return
	}

	s.peerConnected(conn, connReq, true)
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (s *server) addPeer(p *peer) {
	if p == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	// Track the new peer in our indexes so we can quickly look it up either
	// according to its public key, or it's peer ID.
	// TODO(roasbeef): pipe all requests through to the
	// queryHandler/peerManager
	s.peersMtx.Lock()
	s.peersByID[p.id] = p
	s.peersByPub[string(p.addr.IdentityKey.SerializeCompressed())] = p
	s.peersMtx.Unlock()

	// Once the peer has been added to our indexes, send a message to the
	// channel router so we can synchronize our view of the channel graph
	// with this new peer.
	go s.chanRouter.SynchronizeNode(p.addr.IdentityKey)
}

// removePeer removes the passed peer from the server's state of all active
// peers.
func (s *server) removePeer(p *peer) {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	srvrLog.Debugf("removing peer %v", p)

	if p == nil {
		return
	}

	// As the peer is now finished, ensure that the TCP connection is
	// closed and all of its related goroutines have exited.
	if err := p.Stop(); err != nil {
		peerLog.Errorf("unable to stop peer: %v", err)
	}

	// Ignore deleting peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		return
	}

	delete(s.peersByID, p.id)
	delete(s.peersByPub, string(p.addr.IdentityKey.SerializeCompressed()))
}

// connectPeerMsg is a message requesting the server to open a connection to a
// particular peer. This message also houses an error channel which will be
// used to report success/failure.
type connectPeerMsg struct {
	addr       *lnwire.NetAddress
	persistent bool

	err chan error
}

// listPeersMsg is a message sent to the server in order to obtain a listing
// of all currently active channels.
type listPeersMsg struct {
	resp chan []*peer
}

// openChanReq is a message sent to the server in order to request the
// initiation of a channel funding workflow to the peer with either the specified
// relative peer ID, or a global lightning  ID.
type openChanReq struct {
	targetPeerID int32
	targetPubkey *btcec.PublicKey

	// TODO(roasbeef): make enums in lnwire
	channelType uint8
	coinType    uint64

	localFundingAmt  btcutil.Amount
	remoteFundingAmt btcutil.Amount

	pushAmt btcutil.Amount

	numConfs uint32

	updates chan *lnrpc.OpenStatusUpdate
	err     chan error
}

// queryHandler handles any requests to modify the server's internal state of
// all active peers, or query/mutate the server's global state. Additionally,
// any queries directed at peers will be handled by this goroutine.
//
// NOTE: This MUST be run as a goroutine.
func (s *server) queryHandler() {
	go s.connMgr.Start()

out:
	for {
		select {
		// New peers.
		case p := <-s.newPeers:
			s.addPeer(p)

		// Finished peers.
		case p := <-s.donePeers:
			s.removePeer(p)

		case bMsg := <-s.broadcastRequests:
			ignore := bMsg.ignore

			srvrLog.Debugf("Broadcasting %v messages", len(bMsg.msgs))

			// Launch a new goroutine to handle the broadcast
			// request, this allows us process this request
			// asynchronously without blocking subsequent broadcast
			// requests.
			go func() {
				s.peersMtx.RLock()
				for _, sPeer := range s.peersByPub {
					if ignore != nil &&
						sPeer.addr.IdentityKey.IsEqual(ignore) {

						srvrLog.Debugf("Skipping %v in broadcast",
							ignore.SerializeCompressed())

						continue
					}

					go func(p *peer) {
						for _, msg := range bMsg.msgs {
							p.queueMsg(msg, nil)
						}
					}(sPeer)
				}
				s.peersMtx.RUnlock()

				bMsg.errChan <- nil
			}()
		case sMsg := <-s.sendRequests:
			// TODO(roasbeef): use [33]byte everywhere instead
			//  * eliminate usage of mutexes, funnel all peer
			//    mutation to this goroutine
			target := sMsg.target.SerializeCompressed()

			srvrLog.Debugf("Attempting to send msgs %v to: %x",
				len(sMsg.msgs), target)

			// Launch a new goroutine to handle this send request,
			// this allows us process this request asynchronously
			// without blocking future send requests.
			go func() {
				s.peersMtx.RLock()
				targetPeer, ok := s.peersByPub[string(target)]
				if !ok {
					s.peersMtx.RUnlock()
					srvrLog.Errorf("unable to send message to %x, "+
						"peer not found", target)
					sMsg.errChan <- errors.New("peer not found")
					return
				}
				s.peersMtx.RUnlock()

				sMsg.errChan <- nil

				for _, msg := range sMsg.msgs {
					targetPeer.queueMsg(msg, nil)
				}
			}()
		case query := <-s.queries:
			switch msg := query.(type) {
			case *connectPeerMsg:
				s.handleConnectPeer(msg)
			case *listPeersMsg:
				s.handleListPeers(msg)
			case *openChanReq:
				s.handleOpenChanReq(msg)
			}
		case <-s.quit:
			break out
		}
	}

	s.connMgr.Stop()

	s.wg.Done()
}

// handleListPeers sends a lice of all currently active peers to the original
// caller.
func (s *server) handleListPeers(msg *listPeersMsg) {
	s.peersMtx.RLock()

	peers := make([]*peer, 0, len(s.peersByID))
	for _, peer := range s.peersByID {
		peers = append(peers, peer)
	}

	s.peersMtx.RUnlock()

	msg.resp <- peers
}

// handleConnectPeer attempts to establish a connection to the address enclosed
// within the passed connectPeerMsg. This function is *async*, a goroutine will
// be spawned in order to finish the request, and respond to the caller.
func (s *server) handleConnectPeer(msg *connectPeerMsg) {
	addr := msg.addr

	targetPub := string(msg.addr.IdentityKey.SerializeCompressed())

	// Ensure we're not already connected to this
	// peer.
	s.peersMtx.RLock()
	peer, ok := s.peersByPub[targetPub]
	if ok {
		s.peersMtx.RUnlock()
		msg.err <- fmt.Errorf("already connected to peer: %v", peer)
		return
	}
	s.peersMtx.RUnlock()

	// If there's already a pending connection request for this pubkey,
	// then we ignore this request to ensure we don't create a redundant
	// connection.
	s.pendingConnMtx.RLock()
	if _, ok := s.persistentConnReqs[targetPub]; ok {
		s.pendingConnMtx.RUnlock()
		msg.err <- fmt.Errorf("connection attempt to %v is pending",
			addr)
		return
	}
	s.pendingConnMtx.RUnlock()

	// If there's not already a pending or active connection to this node,
	// then instruct the connection manager to attempt to establish a
	// persistent connection to the peer.
	srvrLog.Debugf("Connecting to %v", addr)
	if msg.persistent {
		go s.connMgr.Connect(&connmgr.ConnReq{
			Addr:      addr,
			Permanent: true,
		})
		msg.err <- nil
	} else {
		// If we're not making a persistent connection, then we'll
		// attempt to connect o the target peer, returning an error
		// which indicates success of failure.
		go func() {
			// Attempt to connect to the remote node. If the we
			// can't make the connection, or the crypto negotiation
			// breaks down, then return an error to the caller.
			conn, err := brontide.Dial(s.identityPriv, addr)
			if err != nil {
				msg.err <- err
				return
			}

			s.outboundPeerConnected(nil, conn)
			msg.err <- nil
		}()
	}
}

// handleOpenChanReq first locates the target peer, and if found hands off the
// request to the funding manager allowing it to initiate the channel funding
// workflow.
func (s *server) handleOpenChanReq(req *openChanReq) {
	var (
		targetPeer  *peer
		pubKeyBytes []byte
	)

	// If the user is targeting the peer by public key, then we'll need to
	// convert that into a string for our map. Otherwise, we expect them to
	// target by peer ID instead.
	if req.targetPubkey != nil {
		pubKeyBytes = req.targetPubkey.SerializeCompressed()
	}

	// First attempt to locate the target peer to open a channel with, if
	// we're unable to locate the peer then this request will fail.
	s.peersMtx.RLock()
	if peer, ok := s.peersByID[req.targetPeerID]; ok {
		targetPeer = peer
	} else if peer, ok := s.peersByPub[string(pubKeyBytes)]; ok {
		targetPeer = peer
	}
	s.peersMtx.RUnlock()

	if targetPeer == nil {
		req.err <- fmt.Errorf("unable to find peer nodeID(%x), "+
			"peerID(%v)", pubKeyBytes, req.targetPeerID)
		return
	}

	// Spawn a goroutine to send the funding workflow request to the funding
	// manager. This allows the server to continue handling queries instead
	// of blocking on this request which is exported as a synchronous
	// request to the outside world.
	// TODO(roasbeef): server semaphore to restrict num goroutines
	go s.fundingMgr.initFundingWorkflow(targetPeer.addr, req)
}

// ConnectToPeer requests that the server connect to a Lightning Network peer
// at the specified address. This function will *block* until either a
// connection is established, or the initial handshake process fails.
func (s *server) ConnectToPeer(addr *lnwire.NetAddress,
	perm bool) error {

	errChan := make(chan error, 1)

	s.queries <- &connectPeerMsg{
		addr:       addr,
		persistent: perm,
		err:        errChan,
	}

	return <-errChan
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by ID with the passed channel funding paramters.
func (s *server) OpenChannel(peerID int32, nodeKey *btcec.PublicKey,
	localAmt, pushAmt btcutil.Amount,
	numConfs uint32) (chan *lnrpc.OpenStatusUpdate, chan error) {

	errChan := make(chan error, 1)
	updateChan := make(chan *lnrpc.OpenStatusUpdate, 1)

	req := &openChanReq{
		targetPeerID:    peerID,
		targetPubkey:    nodeKey,
		localFundingAmt: localAmt,
		pushAmt:         pushAmt,
		numConfs:        numConfs,
		updates:         updateChan,
		err:             errChan,
	}

	s.queries <- req

	return updateChan, errChan
}

// Peers returns a slice of all active peers.
func (s *server) Peers() []*peer {
	resp := make(chan []*peer, 1)

	s.queries <- &listPeersMsg{resp}

	return <-resp
}
