package htlcswitch

import (
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
)

// mailBox is an interface which represents a concurrent-safe, in-order
// delivery queue for messages from the network and also from the main switch.
// This struct servers as a buffer between incoming messages, and messages to
// the handled by the link. Each of the mutating methods within this interface
// should be implemented in a non-blocking manner.
type mailBox interface {
	// AddMessage appends a new message to the end of the message queue.
	AddMessage(msg lnwire.Message) error

	// AddPacket appends a new message to the end of the packet queue.
	AddPacket(pkt *htlcPacket) error

	// MessageOutBox returns a channel that any new messages ready for
	// delivery will be sent on.
	MessageOutBox() chan lnwire.Message

	// PacketOutBox returns a channel that any new packets ready for
	// delivery will be sent on.
	PacketOutBox() chan *htlcPacket

	// Start starts the mailbox and any goroutines it needs to operate
	// properly.
	Start() error

	// Stop signals the mailbox and its goroutines for a graceful shutdown.
	Stop() error
}

// memoryMailBox is an implementation of the mailBox struct backed by purely
// in-memory queues.
type memoryMailBox struct {
	wireMessages []lnwire.Message
	wireMtx      sync.Mutex
	wireCond     *sync.Cond

	messageOutbox chan lnwire.Message

	htlcPkts []*htlcPacket
	pktMtx   sync.Mutex
	pktCond  *sync.Cond

	pktOutbox chan *htlcPacket

	wg   sync.WaitGroup
	quit chan struct{}
}

// newMemoryMailBox creates a new instance of the memoryMailBox.
func newMemoryMailBox() *memoryMailBox {
	box := &memoryMailBox{
		quit:          make(chan struct{}),
		messageOutbox: make(chan lnwire.Message),
		pktOutbox:     make(chan *htlcPacket),
	}
	box.wireCond = sync.NewCond(&box.wireMtx)
	box.pktCond = sync.NewCond(&box.pktMtx)

	return box
}

// A compile time assertion to ensure that memoryMailBox meets the mailBox
// interface.
var _ mailBox = (*memoryMailBox)(nil)

// courierType is an enum that reflects the distinct types of messages a
// mailBox can handle. Each type will be placed in an isolated mail box and
// will have a dedicated goroutine for delivering the messages.
type courierType uint8

const (
	// wireCourier is a type of courier that handles wire messages.
	wireCourier courierType = iota

	// pktCourier is a type of courier that handles hltc packets.
	pktCourier
)

// Start starts the mailbox and any goroutines it needs to operate properly.
//
// NOTE: This method is part of the mailBox interface.
func (m *memoryMailBox) Start() error {
	m.wg.Add(2)
	go m.mailCourier(wireCourier)
	go m.mailCourier(pktCourier)

	return nil
}

// Stop signals the mailbox and its goroutines for a graceful shutdown.
//
// NOTE: This method is part of the mailBox interface.
func (m *memoryMailBox) Stop() error {
	close(m.quit)

	m.wireCond.Signal()
	m.pktCond.Signal()

	return nil
}

// mailCourier is a dedicated goroutine whose job is to reliably deliver
// messages of a particular type. There are two types of couriers: wire
// couriers, and mail couriers. Depending on the passed courierType, this
// goroutine will assume one of two roles.
func (m *memoryMailBox) mailCourier(cType courierType) {
	defer m.wg.Done()

	// TODO(roasbeef): refactor...

	for {
		// First, we'll check our condition. If our target mailbox is
		// empty, then we'll wait until a new item is added.
		switch cType {
		case wireCourier:
			m.wireCond.L.Lock()
			for len(m.wireMessages) == 0 {
				m.wireCond.Wait()

				select {
				case <-m.quit:
					m.wireCond.L.Unlock()
					return
				default:
				}
			}

		case pktCourier:
			m.pktCond.L.Lock()
			for len(m.htlcPkts) == 0 {
				m.pktCond.Wait()

				select {
				case <-m.quit:
					m.pktCond.L.Unlock()
					return
				default:
				}
			}
		}

		// Grab the datum off the front of the queue, shifting the
		// slice's reference down one in order to remove the datum from
		// the queue.
		var (
			nextPkt *htlcPacket
			nextMsg lnwire.Message
		)
		switch cType {
		case wireCourier:
			nextMsg = m.wireMessages[0]
			m.wireMessages[0] = nil // Set to nil to prevent GC leak.
			m.wireMessages = m.wireMessages[1:]
		case pktCourier:
			nextPkt = m.htlcPkts[0]
			m.htlcPkts[0] = nil // Set to nil to prevent GC leak.
			m.htlcPkts = m.htlcPkts[1:]
		}

		// Now that we're done with the condition, we can unlock it to
		// allow any callers to append to the end of our target queue.
		switch cType {
		case wireCourier:
			m.wireCond.L.Unlock()
		case pktCourier:
			m.pktCond.L.Unlock()
		}

		// With the next message obtained, we'll now select to attempt
		// to deliver the message. If we receive a kill signal, then
		// we'll bail out.
		switch cType {
		case wireCourier:
			select {
			case m.messageOutbox <- nextMsg:
			case <-m.quit:
				return
			}

		case pktCourier:
			select {
			case m.pktOutbox <- nextPkt:
			case <-m.quit:
				return
			}
		}

	}
}

// AddMessage appends a new message to the end of the message queue.
//
// NOTE: This method is safe for concrete use and part of the mailBox
// interface.
func (m *memoryMailBox) AddMessage(msg lnwire.Message) error {
	// First, we'll lock the condition, and add the message to the end of
	// the wire message inbox.
	m.wireCond.L.Lock()
	m.wireMessages = append(m.wireMessages, msg)
	m.wireCond.L.Unlock()

	// With the message added, we signal to the mailCourier that there are
	// additional messages to deliver.
	m.wireCond.Signal()

	return nil
}

// AddPacket appends a new message to the end of the packet queue.
//
// NOTE: This method is safe for concrete use and part of the mailBox
// interface.
func (m *memoryMailBox) AddPacket(pkt *htlcPacket) error {
	// First, we'll lock the condition, and add the packet to the end of
	// the htlc packet inbox.
	m.pktCond.L.Lock()
	m.htlcPkts = append(m.htlcPkts, pkt)
	m.pktCond.L.Unlock()

	// With the packet added, we signal to the mailCourier that there are
	// additional packets to consume.
	m.pktCond.Signal()

	return nil
}

// MessageOutBox returns a channel that any new messages ready for delivery
// will be sent on.
//
// NOTE: This method is part of the mailBox interface.
func (m *memoryMailBox) MessageOutBox() chan lnwire.Message {
	return m.messageOutbox
}

// PacketOutBox returns a channel that any new packets ready for delivery will
// be sent on.
//
// NOTE: This method is part of the mailBox interface.
func (m *memoryMailBox) PacketOutBox() chan *htlcPacket {
	return m.pktOutbox
}
