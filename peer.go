package main

import (
	"container/list"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"li.lan/labs/plasma/lnwallet"
	"li.lan/labs/plasma/lnwire"
)

var (
	numNodes int32
)

// channelState...
type channelState uint8

const (
	// TODO(roasbeef): others??
	channelPending channelState = iota
	channelOpen
	channelClosed
	channelDispute
	channelPendingPayment
)

const (
	numAllowedRetransmits = 5
	pingInterval          = 1 * time.Minute

	outgoingQueueLen = 50
)

// lnAddr...
type lnAddr struct {
	lnId   [16]byte // redundant because adr contains it
	pubKey *btcec.PublicKey

	bitcoinAddr btcutil.Address
	netAddr     *net.TCPAddr

	name        string
	endorsement []byte
}

// String...
func (l *lnAddr) String() string {
	var encodedId []byte
	if l.pubKey == nil {
		encodedId = l.bitcoinAddr.ScriptAddress()
	} else {
		encodedId = l.pubKey.SerializeCompressed()
	}

	return fmt.Sprintf("%v@%v", encodedId, l.netAddr)
}

// newLnAddr...
func newLnAddr(encodedAddr string) (*lnAddr, error) {
	// The format of an lnaddr is "<pubkey or pkh>@host"
	idHost := strings.Split(encodedAddr, "@")
	if len(idHost) != 2 {
		return nil, fmt.Errorf("invalid format for lnaddr string: %v", encodedAddr)
	}

	// Attempt to resolve the IP address, this handles parsing IPv6 zones,
	// and such.
	fmt.Println("host: ", idHost[1])
	ipAddr, err := net.ResolveTCPAddr("tcp", idHost[1])
	if err != nil {
		return nil, err
	}

	addr := &lnAddr{netAddr: ipAddr}

	idLen := len(idHost[0])
	switch {
	// Is the ID a hex-encoded compressed public key?
	case idLen > 65 && idLen < 69:
		pubkeyBytes, err := hex.DecodeString(idHost[0])
		if err != nil {
			return nil, err
		}

		addr.pubKey, err = btcec.ParsePubKey(pubkeyBytes, btcec.S256())
		if err != nil {
			return nil, err
		}

		// got pubey, populate address from pubkey
		pkh := btcutil.Hash160(addr.pubKey.SerializeCompressed())
		addr.bitcoinAddr, err = btcutil.NewAddressPubKeyHash(pkh,
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	// Is the ID a string encoded bitcoin address?
	case idLen > 33 && idLen < 37:
		addr.bitcoinAddr, err = btcutil.DecodeAddress(idHost[0],
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid address %s", idHost[0])
	}

	// Finally, populate the lnid from the address.
	copy(addr.lnId[:], addr.bitcoinAddr.ScriptAddress())

	return addr, nil
}

// outgoinMsg...
type outgoinMsg struct {
	msg      lnwire.Message
	sentChan chan struct{}
}

// peer...
// inspired by btcd/peer.go
type peer struct {
	// only to be used atomically
	started    int32
	connected  int32
	disconnect int32

	conn net.Conn

	lightningAddr   lnAddr
	inbound         bool
	protocolVersion uint32
	peerId          int32

	// For purposes of detecting retransmits, etc.
	lastNMessages map[lnwire.Message]struct{}

	sync.RWMutex
	timeConnected    time.Time
	lastSend         time.Time
	lastRecv         time.Time
	bytesReceived    uint64
	bytesSent        uint64
	satoshisSent     uint64
	satoshisReceived uint64
	// TODO(roasbeef): pings??

	sendQueueSync chan struct{}
	outgoingQueue chan outgoinMsg
	sendQueue     chan outgoinMsg

	// Only will be set if the channel is in the 'pending' state.
	reservation *lnwallet.ChannelReservation

	lnChannel *lnwallet.LightningChannel

	queueQuit chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
}

// newPeer...
func newPeer(conn net.Conn, server *server) *peer {
	return &peer{
		conn:   conn,
		peerId: atomic.AddInt32(&numNodes, 1),

		lastNMessages: make(map[lnwire.Message]struct{}),

		sendQueueSync: make(chan struct{}, 1),
		sendQueue:     make(chan outgoinMsg, 1),
		outgoingQueue: make(chan outgoinMsg, outgoingQueueLen),

		queueQuit: make(chan struct{}),
		quit:      make(chan struct{}),
	}
}

func (p *peer) Start() error {
	if atomic.AddInt32(&p.started, 1) != 1 {
		return nil
	}

	// TODO(roasbeef): version handshake

	p.wg.Add(3)
	go p.inHandler()
	go p.queueHandler()
	go p.outHandler()

	return nil
}

func (p *peer) Stop() error {
	// If we're already disconnecting, just exit.
	if atomic.AddInt32(&p.disconnect, 1) != 1 {
		return nil
	}

	// Otherwise, close the connection if we're currently connected.
	if atomic.LoadInt32(&p.connected) != 0 {
		p.conn.Close()
	}

	// Signal all worker goroutines to gracefully exit.
	close(p.quit)

	return nil
}

// readNextMessage...
func (p *peer) readNextMessage() (lnwire.Message, []byte, error) {
	// TODO(roasbeef): use our own net magic?
	_, nextMsg, rawPayload, err := lnwire.ReadMessage(p.conn, 0, wire.TestNet)
	if err != nil {
		return nil, nil, err
	}

	return nextMsg, rawPayload, nil
}

// inHandler..
func (p *peer) inHandler() {
	// TODO(roasbeef): set timeout for initial channel request or version
	// exchange.

out:
	for atomic.LoadInt32(&p.disconnect) == 0 {
		nextMsg, _, err := p.readNextMessage()
		if err != nil {
			// TODO(roasbeef): log error
			break out
		}

		// TODO(roasbeef): state-machine to track version exchange
		switch msg := nextMsg.(type) {
		// TODO(roasbeef): cases
		}
	}

	p.wg.Done()
}

// writeMessage...
func (p *peer) writeMessage(msg lnwire.Message) error {
	// Simply exit if we're shutting down.
	if atomic.LoadInt32(&p.disconnect) != 0 {
		return nil
	}

	_, err := lnwire.WriteMessage(p.conn, msg, 0,
		wire.TestNet)

	return err
}

// outHandler..
func (p *peer) outHandler() {
	// pingTicker is used to periodically send pings to the remote peer.
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

out:
	for {
		select {
		case outMsg := <-p.sendQueue:
			switch m := outMsg.msg.(type) {
			// TODO(roasbeef): handle special write cases
			}

			if err := p.writeMessage(outMsg.msg); err != nil {
				// TODO(roasbeef): disconnect
			}

			// Synchronize with the outHandler.
			p.sendQueueSync <- struct{}{}
		case <-pingTicker.C:
			// TODO(roasbeef): ping em
		case <-p.quit:
			break out

		}
	}

	// Wait for the queueHandler to finish so we can empty out all pending
	// messages avoiding a possible deadlock somewhere.
	<-p.queueQuit

	// Drain any lingering messages that we're meant to be sent. But since
	// we're shutting down, just ignore them.
fin:
	for {
		select {
		case msg := <-p.sendQueue:
			if msg.sentChan != nil {
				msg.sentChan <- struct{}{}
			}
		default:
			break fin
		}
	}
	p.wg.Done()
}

// queueHandler..
func (p *peer) queueHandler() {
	waitOnSync := false
	pendingMsgs := list.New()
out:
	for {
		select {
		case msg := <-p.outgoingQueue:
			if !waitOnSync {
				p.sendQueue <- msg
			} else {
				pendingMsgs.PushBack(msg)
			}
			waitOnSync = true
		case <-p.sendQueueSync:
			// If there aren't any more remaining messages in the
			// queue, then we're no longer waiting to synchronize
			// with the outHandler.
			next := pendingMsgs.Front()
			if next == nil {
				waitOnSync = false
				continue
			}

			// Notify the outHandler about the next item to
			// asynchronously send.
			val := pendingMsgs.Remove(next)
			p.sendQueue <- val.(outgoinMsg)
			// TODO(roasbeef): other sync stuffs
		case <-p.quit:
			break out
		}
	}

	close(p.queueQuit)
	p.wg.Done()
}
