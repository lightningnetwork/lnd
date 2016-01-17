package main

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/lnwallet"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcwallet/walletdb"
)

// server...
type server struct {
	listeners []net.Listener
	peers     map[int32]*peer

	started  int32 // atomic
	shutdown int32 // atomic

	bitcoinNet *chaincfg.Params

	rpcServer *rpcServer
	lnwallet  *lnwallet.LightningWallet

	db walletdb.DB

	newPeers  chan *peer
	donePeers chan *peer

	wg   sync.WaitGroup
	quit chan struct{}
}

// addPeer...
func (s *server) addPeer(p *peer) {
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

func (s *server) queryHandler() {
out:
	for {
		select {
		// TODO(roasbeef): meta-rpc-stuff
		case <-s.quit:
			break out
		}
	}

	s.wg.Done()
}

// AddPeer...
func (s *server) AddPeer(p *peer) {
	s.newPeers <- p
}

// listener...
func (s *server) listener(l net.Listener) {
	for atomic.LoadInt32(&s.shutdown) == 0 {
		conn, err := l.Accept()
		if err != nil {
			// TODO(roasbeef): log
			continue
		}
		// TODO(roasbeef): create new peer, start it's goroutines
		fmt.Println(conn)
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

	s.wg.Add(1)
	go s.peerManager()

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
	s.lnwallet.Stop()

	// Signal all the lingering goroutines to quit.
	close(s.quit)
	return nil
}

// getIdentityPrivKey gets the identity private key out of the wallet DB.
func getIdentityPrivKey(l *lnwallet.LightningWallet) (*btcec.PrivateKey, error) {
	adr, err := l.ChannelDB.GetIdAdr()
	if err != nil {
		return nil, err
	}
	fmt.Printf("got ID address: %s\n", adr.String())
	adr2, err := l.Manager.Address(adr)
	if err != nil {
		return nil, err
	}
	fmt.Println("pubkey: %v", hex.EncodeToString(adr2.(waddrmgr.ManagedPubKeyAddress).PubKey().SerializeCompressed()))
	priv, err := adr2.(waddrmgr.ManagedPubKeyAddress).PrivKey()
	if err != nil {
		return nil, err
	}

	return priv, nil
}
