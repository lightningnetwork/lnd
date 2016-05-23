package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"

	"github.com/roasbeef/btcwallet/waddrmgr"
)

// server...
type server struct {
	started  int32 // atomic
	shutdown int32 // atomic

	longTermPriv *btcec.PrivateKey

	listeners []net.Listener
	peers     map[int32]*peer

	rpcServer *rpcServer
	lnwallet  *lnwallet.LightningWallet
	chanDB    *channeldb.DB

	newPeers  chan *peer
	donePeers chan *peer
	queries   chan interface{}

	wg   sync.WaitGroup
	quit chan struct{}
}

// newServer...
func newServer(listenAddrs []string, wallet *lnwallet.LightningWallet,
	chanDB *channeldb.DB) (*server, error) {

	privKey, err := getIdentityPrivKey(chanDB, wallet)
	if err != nil {
		return nil, err
	}

	listeners := make([]net.Listener, len(listenAddrs))
	for i, addr := range listenAddrs {
		listeners[i], err = lndc.NewListener(privKey, addr)
		if err != nil {
			return nil, err
		}
	}

	s := &server{
		chanDB:       chanDB,
		longTermPriv: privKey,
		listeners:    listeners,
		peers:        make(map[int32]*peer),
		newPeers:     make(chan *peer, 100),
		donePeers:    make(chan *peer, 100),
		lnwallet:     wallet,
		queries:      make(chan interface{}),
		quit:         make(chan struct{}),
	}

	s.rpcServer = newRpcServer(s)

	return s, nil
}

// addPeer...
func (s *server) addPeer(p *peer) {
	if p == nil {
		return
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		p.Stop()
		return
	}

	s.peers[p.peerId] = p
}

// removePeer...
func (s *server) removePeer(p *peer) {
}

// peerManager...
func (s *server) peerManager() {
out:
	for {
		select {
		// New peers.
		case p := <-s.newPeers:
			s.addPeer(p)
		// Finished peers.
		case p := <-s.donePeers:
			s.removePeer(p)
		case <-s.quit:
			break out
		}
	}
	s.wg.Done()
}

// connectPeerMsg...
type connectPeerMsg struct {
	addr  *lndc.LNAdr
	reply chan error
}

// queryHandler...
func (s *server) queryHandler() {
out:
	for {
		select {
		case query := <-s.queries:
			switch msg := query.(type) {
			case *connectPeerMsg:
				addr := msg.addr

				// Ensure we're not already connected to this
				// peer.
				for _, peer := range s.peers {
					if peer.lightningAddr.String() ==
						addr.String() {
						msg.reply <- fmt.Errorf(
							"already connected to peer: %v",
							peer.lightningAddr,
						)
					}
				}

				// Launch a goroutine to connect to the requested
				// peer so we can continue to handle queries.
				go func() {
					// For the lndc crypto handshake, we
					// either need a compressed pubkey, or a
					// 20-byte pkh.
					var remoteId []byte
					if addr.PubKey == nil {
						remoteId = addr.Base58Adr.ScriptAddress()
					} else {
						remoteId = addr.PubKey.SerializeCompressed()
					}

					// Attempt to connect to the remote
					// node. If the we can't make the
					// connection, or the crypto negotation
					// breaks down, then return an error to the
					// caller.
					ipAddr := addr.NetAddr.String()
					conn := lndc.NewConn(nil)
					if err := conn.Dial(
						s.longTermPriv, ipAddr, remoteId); err != nil {
						msg.reply <- err
					}

					// Now that we've established a connection,
					// create a peer, and it to the set of
					// currently active peers.
					peer := newPeer(conn, s)
					s.newPeers <- peer

					msg.reply <- nil
				}()
			}
		case <-s.quit:
			break out
		}
	}

	s.wg.Done()
}

// ConnectToPeer...
func (s *server) ConnectToPeer(addr *lndc.LNAdr) error {
	reply := make(chan error, 1)

	s.queries <- &connectPeerMsg{addr, reply}

	return <-reply
}

// AddPeer...
func (s *server) AddPeer(p *peer) {
	s.newPeers <- p
}

// listener...
func (s *server) listener(l net.Listener) {
	srvrLog.Infof("Server listening on %s", l.Addr())
	for atomic.LoadInt32(&s.shutdown) == 0 {
		conn, err := l.Accept()
		if err != nil {
			// Only log the error message if we aren't currently
			// shutting down.
			if atomic.LoadInt32(&s.shutdown) == 0 {
				srvrLog.Errorf("Can't accept connection: %v", err)
			}
			continue
		}

		srvrLog.Tracef("New inbound connection from %v", conn.RemoteAddr())
		peer := newPeer(conn, s)
		peer.Start()

		s.newPeers <- peer
	}

	s.wg.Done()
}

// Start...
func (s *server) Start() {
	// Already running?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}

	// Start all the listeners.
	for _, l := range s.listeners {
		s.wg.Add(1)
		go s.listener(l)
	}

	s.wg.Add(2)
	go s.peerManager()
	go s.queryHandler()
}

// Stop...
func (s *server) Stop() error {
	// Bail if we're already shutting down.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	// Stop all the listeners.
	for _, listener := range s.listeners {
		if err := listener.Close(); err != nil {
			return err
		}
	}

	s.rpcServer.Stop()
	s.lnwallet.Shutdown()

	// Signal all the lingering goroutines to quit.
	close(s.quit)
	return nil
}

// getIdentityPrivKey gets the identity private key out of the wallet DB.
func getIdentityPrivKey(c *channeldb.DB, w *lnwallet.LightningWallet) (*btcec.PrivateKey, error) {
	adr, err := c.GetIdAdr()
	if err != nil {
		return nil, err
	}
	ltndLog.Infof("got ID address: %s", adr.String())
	adr2, err := w.Manager.Address(adr)
	if err != nil {
		return nil, err
	}
	ltndLog.Infof("pubkey: %v", hex.EncodeToString(adr2.(waddrmgr.ManagedPubKeyAddress).PubKey().SerializeCompressed()))
	priv, err := adr2.(waddrmgr.ManagedPubKeyAddress).PrivKey()
	if err != nil {
		return nil, err
	}

	return priv, nil
}

// WaitForShutdown blocks all goroutines have been stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}
