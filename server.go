package main

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/rt/graph"
)

// server is the main server of the Lightning Network Daemon. The server
// houses global state pertianing to the wallet, database, and the rpcserver.
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

	routingMgr *routing.RoutingManager

	utxoNursery *utxoNursery

	sphinx *sphinx.Router

	connMgr *connmgr.ConnManager

	pendingConnMtx      sync.RWMutex
	persistentConnReqs  map[string]*connmgr.ConnReq
	pendingConnRequests map[string]*connectPeerMsg

	newPeers  chan *peer
	donePeers chan *peer
	queries   chan interface{}

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
		utxoNursery: newUtxoNursery(notifier, wallet),

		identityPriv: privKey,

		// TODO(roasbeef): derive proper onion key based on rotation
		// schedule
		sphinx:      sphinx.NewRouter(privKey, activeNetParams.Params),
		lightningID: fastsha256.Sum256(serializedPubKey),

		pendingConnRequests: make(map[string]*connectPeerMsg),
		persistentConnReqs:  make(map[string]*connmgr.ConnReq),

		peersByID:  make(map[int32]*peer),
		peersByPub: make(map[string]*peer),

		newPeers:  make(chan *peer, 10),
		donePeers: make(chan *peer, 10),

		queries: make(chan interface{}),
		quit:    make(chan struct{}),
	}

	// If the debug HTLC flag is on, then we invoice a "master debug"
	// invoice which all outgoing payments will be sent and all incoming
	// HTLC's with the debug R-Hash immediately settled.
	if cfg.DebugHTLC {
		kiloCoin := btcutil.Amount(btcutil.SatoshiPerBitcoin * 1000)
		s.invoices.AddDebugInvoice(kiloCoin, *debugPre)
		srvrLog.Debugf("Debug HTLC invoice inserted, preimage=%x, hash=%x",
			debugPre[:], debugHash[:])
	}

	s.utxoNursery = newUtxoNursery(notifier, wallet)

	// Create a new routing manager with ourself as the sole node within
	// the graph.
	selfVertex := serializedPubKey
	routingMgrConfig := &routing.RoutingConfig{}
	routingMgrConfig.SendMessage = func(receiver [33]byte, msg lnwire.Message) error {
		receiverID := graph.NewVertex(receiver[:])
		if receiverID == graph.NilVertex {
			peerLog.Critical("receiverID == graph.NilVertex")
			return fmt.Errorf("receiverID == graph.NilVertex")
		}

		var targetPeer *peer
		for _, peer := range s.peersByID { // TODO: threadsafe API
			nodePub := peer.addr.IdentityKey.SerializeCompressed()
			nodeVertex := graph.NewVertex(nodePub[:])

			// We found the target
			if receiverID == nodeVertex {
				targetPeer = peer
				break
			}
		}

		if targetPeer != nil {
			targetPeer.queueMsg(msg, nil)
		} else {
			srvrLog.Errorf("Can't find peer to send message %v",
				receiverID)
		}
		return nil
	}
	s.routingMgr = routing.NewRoutingManager(graph.NewVertex(selfVertex), routingMgrConfig)

	s.htlcSwitch = newHtlcSwitch(serializedPubKey, s.routingMgr)

	s.rpcServer = newRpcServer(s)

	s.breachArbiter = newBreachArbiter(wallet, chanDB, notifier, s.htlcSwitch)

	s.fundingMgr = newFundingManager(wallet, s.breachArbiter)

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
	// connections to all our direct channel counter parties.
	linkNodes, err := s.chanDB.FetchAllLinkNodes()
	if err != nil && err != channeldb.ErrLinkNodesNotFound {
		return nil, err
	}
	for _, node := range linkNodes {
		// Create a wrapper address which couples the IP and the pubkey
		// so the brontide authenticated connection can be established.
		lnAddr := &lnwire.NetAddress{
			IdentityKey: node.IdentityPub,
			Address:     node.Addresses[0],
		}
		pubStr := string(node.IdentityPub.SerializeCompressed())
		srvrLog.Debugf("Attempting persistent connection to channel "+
			"peer %v", lnAddr)

		// Send the persistent connection request to the connection
		// manager, saving the request itself so we can
		// cancel/restart the process as needed.
		connReq := &connmgr.ConnReq{
			Addr:      lnAddr,
			Permanent: true,
		}
		s.persistentConnReqs[pubStr] = connReq
		go s.connMgr.Connect(connReq)

		s.pendingConnRequests[pubStr] = &connectPeerMsg{
			resp: make(chan int32, 1),
			err:  make(chan error, 1),
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
	// by the counter party.
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
	s.routingMgr.Start()

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
	s.routingMgr.Stop()
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
	peer, err := newPeer(conn, s, peerAddr, false)
	if err != nil {
		srvrLog.Errorf("unable to create peer %v", err)
		conn.Close()
		return
	}
	if connReq != nil {
		peer.connReq = connReq
	}

	// TODO(roasbeef): update IP address for link-node
	//  * also mark last-seen, do it one single transaction?

	peer.Start()
	s.newPeers <- peer

	// If this was an RPC initiated outbound connection that was
	// successfully established, then send a response back to the client so
	pubStr := string(peerAddr.IdentityKey.SerializeCompressed())
	s.pendingConnMtx.RLock()
	msg, ok := s.pendingConnRequests[pubStr]
	s.pendingConnMtx.RUnlock()
	if ok {
		msg.resp <- peer.id
		msg.err <- nil

		s.pendingConnMtx.Lock()
		delete(s.pendingConnRequests, pubStr)
		s.pendingConnMtx.Unlock()
	}
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
	if connReq, ok := s.persistentConnReqs[pubStr]; ok {
		fmt.Println("trying to cancel out attempt")
		s.connMgr.Remove(connReq.ID())
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
		return
	}

	s.peerConnected(conn, connReq, true)
}

// addPeer adds the passed peer to the server's global state of all active
// peers.
func (s *server) addPeer(p *peer) {
	s.peersMtx.Lock()
	defer s.peersMtx.Unlock()

	if p == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	s.peersByID[p.id] = p
	s.peersByPub[string(p.addr.IdentityKey.SerializeCompressed())] = p
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

	// Ignore deleting peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	delete(s.peersByID, p.id)
	delete(s.peersByPub, string(p.addr.IdentityKey.SerializeCompressed()))
}

// connectPeerMsg is a message requesting the server to open a connection to a
// particular peer. This message also houses an error channel which will be
// used to report success/failure.
type connectPeerMsg struct {
	addr *lnwire.NetAddress
	resp chan int32
	err  chan error
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
	peers := make([]*peer, 0, len(s.peersByID))
	for _, peer := range s.peersByID {
		peers = append(peers, peer)
	}

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
		msg.resp <- -1
		return
	}
	s.peersMtx.RUnlock()

	// If there's already a pending connection request for this pubkey,
	// then we ignore this request to ensure we don't create a redundant
	// connection.
	s.pendingConnMtx.RLock()
	if _, ok := s.pendingConnRequests[targetPub]; ok {
		s.pendingConnMtx.RUnlock()
		msg.err <- fmt.Errorf("connection attempt to %v is pending",
			addr)
		msg.resp <- -1
		return
	}
	if _, ok := s.persistentConnReqs[targetPub]; ok {
		s.pendingConnMtx.RUnlock()
		msg.err <- fmt.Errorf("connection attempt to %v is pending",
			addr)
		msg.resp <- -1
		return
	}
	s.pendingConnMtx.RUnlock()

	// If there's not already a pending or active connection to this node,
	// then instruct the connection manager to attempt to establish a
	// persistent connection to the peer.
	srvrLog.Debugf("connecting to %v", addr)
	go s.connMgr.Connect(&connmgr.ConnReq{
		Addr:      addr,
		Permanent: true,
	})

	// Finally, we store the original request keyed by the public key so we
	// can dispatch the response to the RPC client once a connection has
	// been initiated.
	s.pendingConnMtx.Lock()
	s.pendingConnRequests[targetPub] = msg
	s.pendingConnMtx.Unlock()
}

// handleOpenChanReq first locates the target peer, and if found hands off the
// request to the funding manager allowing it to initiate the channel funding
// workflow.
func (s *server) handleOpenChanReq(req *openChanReq) {
	var targetPeer *peer

	pubStr := string(req.targetPubkey.SerializeCompressed())

	// First attempt to locate the target peer to open a channel with, if
	// we're unable to locate the peer then this request will fail.
	s.peersMtx.RLock()
	if peer, ok := s.peersByID[req.targetPeerID]; ok {
		targetPeer = peer
	} else if peer, ok := s.peersByPub[pubStr]; ok {
		targetPeer = peer
	}
	s.peersMtx.RUnlock()

	if targetPeer == nil {
		req.err <- fmt.Errorf("unable to find peer nodeID(%x), "+
			"peerID(%v)", req.targetPubkey.SerializeCompressed(),
			req.targetPeerID)
		return
	}

	// Spawn a goroutine to send the funding workflow request to the funding
	// manager. This allows the server to continue handling queries instead
	// of blocking on this request which is exported as a synchronous
	// request to the outside world.
	// TODO(roasbeef): server semaphore to restrict num goroutines
	go s.fundingMgr.initFundingWorkflow(targetPeer, req)
}

// ConnectToPeer requests that the server connect to a Lightning Network peer
// at the specified address. This function will *block* until either a
// connection is established, or the initial handshake process fails.
func (s *server) ConnectToPeer(addr *lnwire.NetAddress) (int32, error) {
	reply := make(chan int32, 1)
	errChan := make(chan error, 1)

	s.queries <- &connectPeerMsg{addr, reply, errChan}

	return <-reply, <-errChan
}

// OpenChannel sends a request to the server to open a channel to the specified
// peer identified by ID with the passed channel funding paramters.
func (s *server) OpenChannel(peerID int32, nodeKey *btcec.PublicKey,
	localAmt, remoteAmt btcutil.Amount,
	numConfs uint32) (chan *lnrpc.OpenStatusUpdate, chan error) {

	errChan := make(chan error, 1)
	updateChan := make(chan *lnrpc.OpenStatusUpdate, 1)

	req := &openChanReq{
		targetPeerID:     peerID,
		targetPubkey:     nodeKey,
		localFundingAmt:  localAmt,
		remoteFundingAmt: remoteAmt,
		numConfs:         numConfs,
		updates:          updateChan,
		err:              errChan,
	}

	s.queries <- req

	return updateChan, errChan
}

// Peers returns a slice of all active peers.
func (s *server) Peers() []*peer {
	resp := make(chan []*peer)

	s.queries <- &listPeersMsg{resp}

	return <-resp
}
