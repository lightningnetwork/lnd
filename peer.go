package main

import (
	"net"
	"sync"
	"time"

	"li.lan/labs/plasma/lnwallet"
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
)

// peer...
// TODO(roasbeef): make this a package now??
// inspired by btcd/peer.go
//  * three goroutines
//  * inHandler
//  * ourHandler
//  * queueHandler (maybe?), we don't have any trickling issues so idk
type peer struct {
	started    int32
	connected  int32
	disconnect int32 // only to be used atomically
	// *ETcpConn or w/e it is in strux
	conn net.Conn

	// TODO(rosabeef): one for now, may need more granularity
	sync.RWMutex

	addr            string
	lnID            [32]byte // TODO(roasbeef): copy from strux
	inbound         bool
	protocolVersion uint32

	// For purposes of detecting retransmits, etc.
	// lastNMessages map[lnwire.Message]struct{}

	timeConnected    time.Time
	lastSend         time.Time
	lastRecv         time.Time
	bytesReceived    uint64
	bytesSent        uint64
	satoshisSent     uint64
	satoshisReceived uint64
	// TODO(roasbeef): pings??

	sendQueueDone chan struct{}
	// outgoingQueue chan lnwire.Message
	// sendQueue chan  lnwire.Message
	// TODO(roasbeef+j): something like?
	// type Message {
	//   Decode(uint32) error
	//   Encode(uint32) error
	//   Command() string
	//}

	// TODO(roasbeef): akward import, just rename to Wallet?
	wallet *lnwallet.LightningWallet

	// Only will be set if the channel is in the 'pending' state.
	reservation *lnwallet.ChannelReservation

	channel *lnwallet.LightningChannel // TODO(roasbeef): rename to PaymentChannel??

	queueQuit chan struct{}
	quit      chan struct{}
}
