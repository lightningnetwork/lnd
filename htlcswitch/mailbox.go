package htlcswitch

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrMailBoxShuttingDown is returned when the mailbox is interrupted by
	// a shutdown request.
	ErrMailBoxShuttingDown = errors.New("mailbox is shutting down")

	// ErrPacketAlreadyExists signals that an attempt to add a packet failed
	// because it already exists in the mailbox.
	ErrPacketAlreadyExists = errors.New("mailbox already has packet")
)

// MailBox is an interface which represents a concurrent-safe, in-order
// delivery queue for messages from the network and also from the main switch.
// This struct serves as a buffer between incoming messages, and messages to
// the handled by the link. Each of the mutating methods within this interface
// should be implemented in a non-blocking manner.
type MailBox interface {
	// AddMessage appends a new message to the end of the message queue.
	AddMessage(msg lnwire.Message) error

	// AddPacket appends a new message to the end of the packet queue.
	AddPacket(pkt *htlcPacket) error

	// HasPacket queries the packets for a circuit key, this is used to drop
	// packets bound for the switch that already have a queued response.
	HasPacket(CircuitKey) bool

	// AckPacket removes a packet from the mailboxes in-memory replay
	// buffer. This will prevent a packet from being delivered after a link
	// restarts if the switch has remained online. The returned boolean
	// indicates whether or not a packet with the passed incoming circuit
	// key was removed.
	AckPacket(CircuitKey) bool

	// FailAdd fails an UpdateAddHTLC that exists within the mailbox,
	// removing it from the in-memory replay buffer. This will prevent the
	// packet from being delivered after the link restarts if the switch has
	// remained online. The generated LinkError will show an
	// OutgoingFailureDownstreamHtlcAdd FailureDetail.
	FailAdd(pkt *htlcPacket)

	// MessageOutBox returns a channel that any new messages ready for
	// delivery will be sent on.
	MessageOutBox() chan lnwire.Message

	// PacketOutBox returns a channel that any new packets ready for
	// delivery will be sent on.
	PacketOutBox() chan *htlcPacket

	// Clears any pending wire messages from the inbox.
	ResetMessages() error

	// Reset the packet head to point at the first element in the list.
	ResetPackets() error

	// SetDustClosure takes in a closure that is used to evaluate whether
	// mailbox HTLC's are dust.
	SetDustClosure(isDust dustClosure)

	// SetFeeRate sets the feerate to be used when evaluating dust.
	SetFeeRate(feerate chainfee.SatPerKWeight)

	// DustPackets returns the dust sum for Adds in the mailbox for the
	// local and remote commitments.
	DustPackets() (lnwire.MilliSatoshi, lnwire.MilliSatoshi)

	// Start starts the mailbox and any goroutines it needs to operate
	// properly.
	Start()

	// Stop signals the mailbox and its goroutines for a graceful shutdown.
	Stop()
}

type mailBoxConfig struct {
	// shortChanID is the short channel id of the channel this mailbox
	// belongs to.
	shortChanID lnwire.ShortChannelID

	// forwardPackets send a varidic number of htlcPackets to the switch to
	// be routed. A quit channel should be provided so that the call can
	// properly exit during shutdown.
	forwardPackets func(<-chan struct{}, ...*htlcPacket) error

	// clock is a time source for the mailbox.
	clock clock.Clock

	// expiry is the interval after which Adds will be cancelled if they
	// have not been yet been delivered. The computed deadline will expiry
	// this long after the Adds are added via AddPacket.
	expiry time.Duration

	// failMailboxUpdate is used to fail an expired HTLC and use the
	// correct SCID if the underlying channel uses aliases.
	failMailboxUpdate func(outScid,
		mailboxScid lnwire.ShortChannelID) lnwire.FailureMessage
}

// memoryMailBox is an implementation of the MailBox struct backed by purely
// in-memory queues.
//
// TODO(morehouse): use typed lists instead of list.Lists to avoid type asserts.
type memoryMailBox struct {
	started sync.Once
	stopped sync.Once

	cfg *mailBoxConfig

	wireMessages *list.List
	wireMtx      sync.Mutex
	wireCond     *sync.Cond

	messageOutbox chan lnwire.Message
	msgReset      chan chan struct{}

	// repPkts is a queue for reply packets, e.g. Settles and Fails.
	repPkts  *list.List
	repIndex map[CircuitKey]*list.Element
	repHead  *list.Element

	// addPkts is a dedicated queue for Adds.
	addPkts  *list.List
	addIndex map[CircuitKey]*list.Element
	addHead  *list.Element

	pktMtx  sync.Mutex
	pktCond *sync.Cond

	pktOutbox chan *htlcPacket
	pktReset  chan chan struct{}

	wireShutdown chan struct{}
	pktShutdown  chan struct{}
	quit         chan struct{}

	// feeRate is set when the link receives or sends out fee updates. It
	// is refreshed when AttachMailBox is called in case a fee update did
	// not get committed. In some cases it may be out of sync with the
	// channel's feerate, but it should eventually get back in sync.
	feeRate chainfee.SatPerKWeight

	// isDust is set when AttachMailBox is called and serves to evaluate
	// the outstanding dust in the memoryMailBox given the current set
	// feeRate.
	isDust dustClosure
}

// newMemoryMailBox creates a new instance of the memoryMailBox.
func newMemoryMailBox(cfg *mailBoxConfig) *memoryMailBox {
	box := &memoryMailBox{
		cfg:           cfg,
		wireMessages:  list.New(),
		repPkts:       list.New(),
		addPkts:       list.New(),
		messageOutbox: make(chan lnwire.Message),
		pktOutbox:     make(chan *htlcPacket),
		msgReset:      make(chan chan struct{}, 1),
		pktReset:      make(chan chan struct{}, 1),
		repIndex:      make(map[CircuitKey]*list.Element),
		addIndex:      make(map[CircuitKey]*list.Element),
		wireShutdown:  make(chan struct{}),
		pktShutdown:   make(chan struct{}),
		quit:          make(chan struct{}),
	}
	box.wireCond = sync.NewCond(&box.wireMtx)
	box.pktCond = sync.NewCond(&box.pktMtx)

	return box
}

// A compile time assertion to ensure that memoryMailBox meets the MailBox
// interface.
var _ MailBox = (*memoryMailBox)(nil)

// courierType is an enum that reflects the distinct types of messages a
// MailBox can handle. Each type will be placed in an isolated mail box and
// will have a dedicated goroutine for delivering the messages.
type courierType uint8

const (
	// wireCourier is a type of courier that handles wire messages.
	wireCourier courierType = iota

	// pktCourier is a type of courier that handles htlc packets.
	pktCourier
)

// Start starts the mailbox and any goroutines it needs to operate properly.
//
// NOTE: This method is part of the MailBox interface.
func (m *memoryMailBox) Start() {
	m.started.Do(func() {
		go m.wireMailCourier()
		go m.pktMailCourier()
	})
}

// ResetMessages blocks until all buffered wire messages are cleared.
func (m *memoryMailBox) ResetMessages() error {
	msgDone := make(chan struct{})
	select {
	case m.msgReset <- msgDone:
		return m.signalUntilReset(wireCourier, msgDone)
	case <-m.quit:
		return ErrMailBoxShuttingDown
	}
}

// ResetPackets blocks until the head of packets buffer is reset, causing the
// packets to be redelivered in order.
func (m *memoryMailBox) ResetPackets() error {
	pktDone := make(chan struct{})
	select {
	case m.pktReset <- pktDone:
		return m.signalUntilReset(pktCourier, pktDone)
	case <-m.quit:
		return ErrMailBoxShuttingDown
	}
}

// signalUntilReset strobes the condition variable for the specified inbox type
// until receiving a response that the mailbox has processed a reset.
func (m *memoryMailBox) signalUntilReset(cType courierType,
	done chan struct{}) error {

	for {

		switch cType {
		case wireCourier:
			m.wireCond.Signal()
		case pktCourier:
			m.pktCond.Signal()
		}

		select {
		case <-time.After(time.Millisecond):
			continue
		case <-done:
			return nil
		case <-m.quit:
			return ErrMailBoxShuttingDown
		}
	}
}

// AckPacket removes the packet identified by it's incoming circuit key from the
// queue of packets to be delivered. The returned boolean indicates whether or
// not a packet with the passed incoming circuit key was removed.
//
// NOTE: It is safe to call this method multiple times for the same circuit key.
func (m *memoryMailBox) AckPacket(inKey CircuitKey) bool {
	m.pktCond.L.Lock()
	defer m.pktCond.L.Unlock()

	if entry, ok := m.repIndex[inKey]; ok {
		// Check whether we are removing the head of the queue. If so,
		// we must advance the head to the next packet before removing.
		// It's possible that the courier has already advanced the
		// repHead, so this check prevents the repHead from getting
		// desynchronized.
		if entry == m.repHead {
			m.repHead = entry.Next()
		}
		m.repPkts.Remove(entry)
		delete(m.repIndex, inKey)

		return true
	}

	if entry, ok := m.addIndex[inKey]; ok {
		// Check whether we are removing the head of the queue. If so,
		// we must advance the head to the next add before removing.
		// It's possible that the courier has already advanced the
		// addHead, so this check prevents the addHead from getting
		// desynchronized.
		//
		// NOTE: While this event is rare for Settles or Fails, it could
		// be very common for Adds since the mailbox has the ability to
		// cancel Adds before they are delivered. When that occurs, the
		// head of addPkts has only been peeked and we expect to be
		// removing the head of the queue.
		if entry == m.addHead {
			m.addHead = entry.Next()
		}

		m.addPkts.Remove(entry)
		delete(m.addIndex, inKey)

		return true
	}

	return false
}

// HasPacket queries the packets for a circuit key, this is used to drop packets
// bound for the switch that already have a queued response.
func (m *memoryMailBox) HasPacket(inKey CircuitKey) bool {
	m.pktCond.L.Lock()
	_, ok := m.repIndex[inKey]
	m.pktCond.L.Unlock()

	return ok
}

// Stop signals the mailbox and its goroutines for a graceful shutdown.
//
// NOTE: This method is part of the MailBox interface.
func (m *memoryMailBox) Stop() {
	m.stopped.Do(func() {
		close(m.quit)

		m.signalUntilShutdown(wireCourier)
		m.signalUntilShutdown(pktCourier)
	})
}

// signalUntilShutdown strobes the condition variable of the passed courier
// type, blocking until the worker has exited.
func (m *memoryMailBox) signalUntilShutdown(cType courierType) {
	var (
		cond     *sync.Cond
		shutdown chan struct{}
	)

	switch cType {
	case wireCourier:
		cond = m.wireCond
		shutdown = m.wireShutdown
	case pktCourier:
		cond = m.pktCond
		shutdown = m.pktShutdown
	}

	for {
		select {
		case <-time.After(time.Millisecond):
			cond.Signal()
		case <-shutdown:
			return
		}
	}
}

// pktWithExpiry wraps an incoming packet and records the time at which it it
// should be canceled from the mailbox. This will be used to detect if it gets
// stuck in the mailbox and inform when to cancel back.
type pktWithExpiry struct {
	pkt    *htlcPacket
	expiry time.Time
}

func (p *pktWithExpiry) deadline(clock clock.Clock) <-chan time.Time {
	return clock.TickAfter(p.expiry.Sub(clock.Now()))
}

// wireMailCourier is a dedicated goroutine whose job is to reliably deliver
// wire messages.
func (m *memoryMailBox) wireMailCourier() {
	defer close(m.wireShutdown)

	for {
		// First, we'll check our condition. If our mailbox is empty,
		// then we'll wait until a new item is added.
		m.wireCond.L.Lock()
		for m.wireMessages.Front() == nil {
			m.wireCond.Wait()

			select {
			case msgDone := <-m.msgReset:
				m.wireMessages.Init()
				close(msgDone)
			case <-m.quit:
				m.wireCond.L.Unlock()
				return
			default:
			}
		}

		// Grab the datum off the front of the queue, shifting the
		// slice's reference down one in order to remove the datum from
		// the queue.
		entry := m.wireMessages.Front()

		//nolint:forcetypeassert
		nextMsg := m.wireMessages.Remove(entry).(lnwire.Message)

		// Now that we're done with the condition, we can unlock it to
		// allow any callers to append to the end of our target queue.
		m.wireCond.L.Unlock()

		// With the next message obtained, we'll now select to attempt
		// to deliver the message. If we receive a kill signal, then
		// we'll bail out.
		select {
		case m.messageOutbox <- nextMsg:
		case msgDone := <-m.msgReset:
			m.wireCond.L.Lock()
			m.wireMessages.Init()
			m.wireCond.L.Unlock()

			close(msgDone)
		case <-m.quit:
			return
		}
	}
}

// pktMailCourier is a dedicated goroutine whose job is to reliably deliver
// packet messages.
func (m *memoryMailBox) pktMailCourier() {
	defer close(m.pktShutdown)

	for {
		// First, we'll check our condition. If our mailbox is empty,
		// then we'll wait until a new item is added.
		m.pktCond.L.Lock()
		for m.repHead == nil && m.addHead == nil {
			m.pktCond.Wait()

			select {
			// Resetting the packet queue means just moving our
			// pointer to the front. This ensures that any un-ACK'd
			// messages are re-delivered upon reconnect.
			case pktDone := <-m.pktReset:
				m.repHead = m.repPkts.Front()
				m.addHead = m.addPkts.Front()

				close(pktDone)

			case <-m.quit:
				m.pktCond.L.Unlock()
				return
			default:
			}
		}

		var (
			nextRep   *htlcPacket
			nextRepEl *list.Element
			nextAdd   *pktWithExpiry
			nextAddEl *list.Element
		)
		// For packets, we actually never remove an item until it has
		// been ACK'd by the link. This ensures that if a read packet
		// doesn't make it into a commitment, then it'll be
		// re-delivered once the link comes back online.

		// Peek at the head of the Settle/Fails and Add queues. We peak
		// both even if there is a Settle/Fail present because we need
		// to set a deadline for the next pending Add if it's present.
		// Due to clock monotonicity, we know that the head of the Adds
		// is the next to expire.
		if m.repHead != nil {
			//nolint:forcetypeassert
			nextRep = m.repHead.Value.(*htlcPacket)
			nextRepEl = m.repHead
		}
		if m.addHead != nil {
			//nolint:forcetypeassert
			nextAdd = m.addHead.Value.(*pktWithExpiry)
			nextAddEl = m.addHead
		}

		// Now that we're done with the condition, we can unlock it to
		// allow any callers to append to the end of our target queue.
		m.pktCond.L.Unlock()

		var (
			pktOutbox chan *htlcPacket
			addOutbox chan *htlcPacket
			add       *htlcPacket
			deadline  <-chan time.Time
		)

		// Prioritize delivery of Settle/Fail packets over Adds. This
		// ensures that we actively clear the commitment of existing
		// HTLCs before trying to add new ones. This can help to improve
		// forwarding performance since the time to sign a commitment is
		// linear in the number of HTLCs manifested on the commitments.
		//
		// NOTE: Both types are eventually delivered over the same
		// channel, but we can control which is delivered by exclusively
		// making one nil and the other non-nil. We know from our loop
		// condition that at least one nextRep and nextAdd are non-nil.
		if nextRep != nil {
			pktOutbox = m.pktOutbox
		} else {
			addOutbox = m.pktOutbox
		}

		// If we have a pending Add, we'll also construct the deadline
		// so we can fail it back if we are unable to deliver any
		// message in time. We also dereference the nextAdd's packet,
		// since we will need access to it in the case we are delivering
		// it and/or if the deadline expires.
		//
		// NOTE: It's possible after this point for add to be nil, but
		// this can only occur when addOutbox is also nil, hence we
		// won't accidentally deliver a nil packet.
		if nextAdd != nil {
			add = nextAdd.pkt
			deadline = nextAdd.deadline(m.cfg.clock)
		}

		select {
		case pktOutbox <- nextRep:
			m.pktCond.L.Lock()
			// Only advance the repHead if this Settle or Fail is
			// still at the head of the queue.
			if m.repHead != nil && m.repHead == nextRepEl {
				m.repHead = m.repHead.Next()
			}
			m.pktCond.L.Unlock()

		case addOutbox <- add:
			m.pktCond.L.Lock()
			// Only advance the addHead if this Add is still at the
			// head of the queue.
			if m.addHead != nil && m.addHead == nextAddEl {
				m.addHead = m.addHead.Next()
			}
			m.pktCond.L.Unlock()

		case <-deadline:
			log.Debugf("Expiring add htlc with "+
				"keystone=%v", add.keystone())
			m.FailAdd(add)

		case pktDone := <-m.pktReset:
			m.pktCond.L.Lock()
			m.repHead = m.repPkts.Front()
			m.addHead = m.addPkts.Front()
			m.pktCond.L.Unlock()

			close(pktDone)

		case <-m.quit:
			return
		}
	}
}

// AddMessage appends a new message to the end of the message queue.
//
// NOTE: This method is safe for concrete use and part of the MailBox
// interface.
func (m *memoryMailBox) AddMessage(msg lnwire.Message) error {
	// First, we'll lock the condition, and add the message to the end of
	// the wire message inbox.
	m.wireCond.L.Lock()
	m.wireMessages.PushBack(msg)
	m.wireCond.L.Unlock()

	// With the message added, we signal to the mailCourier that there are
	// additional messages to deliver.
	m.wireCond.Signal()

	return nil
}

// AddPacket appends a new message to the end of the packet queue.
//
// NOTE: This method is safe for concrete use and part of the MailBox
// interface.
func (m *memoryMailBox) AddPacket(pkt *htlcPacket) error {
	m.pktCond.L.Lock()
	switch htlc := pkt.htlc.(type) {
	// Split off Settle/Fail packets into the repPkts queue.
	case *lnwire.UpdateFulfillHTLC, *lnwire.UpdateFailHTLC:
		if _, ok := m.repIndex[pkt.inKey()]; ok {
			m.pktCond.L.Unlock()
			return ErrPacketAlreadyExists
		}

		entry := m.repPkts.PushBack(pkt)
		m.repIndex[pkt.inKey()] = entry
		if m.repHead == nil {
			m.repHead = entry
		}

	// Split off Add packets into the addPkts queue.
	case *lnwire.UpdateAddHTLC:
		if _, ok := m.addIndex[pkt.inKey()]; ok {
			m.pktCond.L.Unlock()
			return ErrPacketAlreadyExists
		}

		entry := m.addPkts.PushBack(&pktWithExpiry{
			pkt:    pkt,
			expiry: m.cfg.clock.Now().Add(m.cfg.expiry),
		})
		m.addIndex[pkt.inKey()] = entry
		if m.addHead == nil {
			m.addHead = entry
		}

	default:
		m.pktCond.L.Unlock()
		return fmt.Errorf("unknown htlc type: %T", htlc)
	}
	m.pktCond.L.Unlock()

	// With the packet added, we signal to the mailCourier that there are
	// additional packets to consume.
	m.pktCond.Signal()

	return nil
}

// SetFeeRate sets the memoryMailBox's feerate for use in DustPackets.
func (m *memoryMailBox) SetFeeRate(feeRate chainfee.SatPerKWeight) {
	m.pktCond.L.Lock()
	defer m.pktCond.L.Unlock()

	m.feeRate = feeRate
}

// SetDustClosure sets the memoryMailBox's dustClosure for use in DustPackets.
func (m *memoryMailBox) SetDustClosure(isDust dustClosure) {
	m.pktCond.L.Lock()
	defer m.pktCond.L.Unlock()

	m.isDust = isDust
}

// DustPackets returns the dust sum for add packets in the mailbox. The first
// return value is the local dust sum and the second is the remote dust sum.
// This will keep track of a given dust HTLC from the time it is added via
// AddPacket until it is removed via AckPacket.
func (m *memoryMailBox) DustPackets() (lnwire.MilliSatoshi,
	lnwire.MilliSatoshi) {

	m.pktCond.L.Lock()
	defer m.pktCond.L.Unlock()

	var (
		localDustSum  lnwire.MilliSatoshi
		remoteDustSum lnwire.MilliSatoshi
	)

	// Run through the map of HTLC's and determine the dust sum with calls
	// to the memoryMailBox's isDust closure. Note that all mailbox packets
	// are outgoing so the second argument to isDust will be false.
	for _, e := range m.addIndex {
		addPkt := e.Value.(*pktWithExpiry).pkt

		// Evaluate whether this HTLC is dust on the local commitment.
		if m.isDust(
			m.feeRate, false, lntypes.Local,
			addPkt.amount.ToSatoshis(),
		) {

			localDustSum += addPkt.amount
		}

		// Evaluate whether this HTLC is dust on the remote commitment.
		if m.isDust(
			m.feeRate, false, lntypes.Remote,
			addPkt.amount.ToSatoshis(),
		) {

			remoteDustSum += addPkt.amount
		}
	}

	return localDustSum, remoteDustSum
}

// FailAdd fails an UpdateAddHTLC that exists within the mailbox, removing it
// from the in-memory replay buffer. This will prevent the packet from being
// delivered after the link restarts if the switch has remained online. The
// generated LinkError will show an OutgoingFailureDownstreamHtlcAdd
// FailureDetail.
func (m *memoryMailBox) FailAdd(pkt *htlcPacket) {
	// First, remove the packet from mailbox. If we didn't find the packet
	// because it has already been acked, we'll exit early to avoid sending
	// a duplicate fail message through the switch.
	if !m.AckPacket(pkt.inKey()) {
		return
	}

	var (
		localFailure = false
		reason       lnwire.OpaqueReason
	)

	// Create a temporary channel failure which we will send back to our
	// peer if this is a forward, or report to the user if the failed
	// payment was locally initiated.
	failure := m.cfg.failMailboxUpdate(
		pkt.originalOutgoingChanID, m.cfg.shortChanID,
	)

	// If the payment was locally initiated (which is indicated by a nil
	// obfuscator), we do not need to encrypt it back to the sender.
	if pkt.obfuscator == nil {
		var b bytes.Buffer
		err := lnwire.EncodeFailure(&b, failure, 0)
		if err != nil {
			log.Errorf("Unable to encode failure: %v", err)
			return
		}
		reason = lnwire.OpaqueReason(b.Bytes())
		localFailure = true
	} else {
		// If the packet is part of a forward, (identified by a non-nil
		// obfuscator) we need to encrypt the error back to the source.
		var err error
		reason, err = pkt.obfuscator.EncryptFirstHop(failure)
		if err != nil {
			log.Errorf("Unable to obfuscate error: %v", err)
			return
		}
	}

	// Create a link error containing the temporary channel failure and a
	// detail which indicates the we failed to add the htlc.
	linkError := NewDetailedLinkError(
		failure, OutgoingFailureDownstreamHtlcAdd,
	)

	failPkt := &htlcPacket{
		incomingChanID: pkt.incomingChanID,
		incomingHTLCID: pkt.incomingHTLCID,
		circuit:        pkt.circuit,
		sourceRef:      pkt.sourceRef,
		hasSource:      true,
		localFailure:   localFailure,
		obfuscator:     pkt.obfuscator,
		linkFailure:    linkError,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}

	if err := m.cfg.forwardPackets(m.quit, failPkt); err != nil {
		log.Errorf("Unhandled error while reforwarding packets "+
			"settle/fail over htlcswitch: %v", err)
	}
}

// MessageOutBox returns a channel that any new messages ready for delivery
// will be sent on.
//
// NOTE: This method is part of the MailBox interface.
func (m *memoryMailBox) MessageOutBox() chan lnwire.Message {
	return m.messageOutbox
}

// PacketOutBox returns a channel that any new packets ready for delivery will
// be sent on.
//
// NOTE: This method is part of the MailBox interface.
func (m *memoryMailBox) PacketOutBox() chan *htlcPacket {
	return m.pktOutbox
}

// mailOrchestrator is responsible for coordinating the creation and lifecycle
// of mailboxes used within the switch. It supports the ability to create
// mailboxes, reassign their short channel id's, deliver htlc packets, and
// queue packets for mailboxes that have not been created due to a link's late
// registration.
type mailOrchestrator struct {
	mu sync.RWMutex

	cfg *mailOrchConfig

	// mailboxes caches exactly one mailbox for all known channels.
	mailboxes map[lnwire.ChannelID]MailBox

	// liveIndex maps a live short chan id to the primary mailbox key.
	// An index in liveIndex map is only entered under two conditions:
	//   1. A link has a non-zero short channel id at time of AddLink.
	//   2. A link receives a non-zero short channel via UpdateShortChanID.
	liveIndex map[lnwire.ShortChannelID]lnwire.ChannelID

	// TODO(conner): add another pair of indexes:
	//   chan_id -> short_chan_id
	//   short_chan_id -> mailbox
	// so that Deliver can lookup mailbox directly once live,
	// but still queryable by channel_id.

	// unclaimedPackets maps a live short chan id to queue of packets if no
	// mailbox has been created.
	unclaimedPackets map[lnwire.ShortChannelID][]*htlcPacket
}

type mailOrchConfig struct {
	// forwardPackets send a varidic number of htlcPackets to the switch to
	// be routed. A quit channel should be provided so that the call can
	// properly exit during shutdown.
	forwardPackets func(<-chan struct{}, ...*htlcPacket) error

	// clock is a time source for the generated mailboxes.
	clock clock.Clock

	// expiry is the interval after which Adds will be cancelled if they
	// have not been yet been delivered. The computed deadline will expiry
	// this long after the Adds are added to a mailbox via AddPacket.
	expiry time.Duration

	// failMailboxUpdate is used to fail an expired HTLC and use the
	// correct SCID if the underlying channel uses aliases.
	failMailboxUpdate func(outScid,
		mailboxScid lnwire.ShortChannelID) lnwire.FailureMessage
}

// newMailOrchestrator initializes a fresh mailOrchestrator.
func newMailOrchestrator(cfg *mailOrchConfig) *mailOrchestrator {
	return &mailOrchestrator{
		cfg:              cfg,
		mailboxes:        make(map[lnwire.ChannelID]MailBox),
		liveIndex:        make(map[lnwire.ShortChannelID]lnwire.ChannelID),
		unclaimedPackets: make(map[lnwire.ShortChannelID][]*htlcPacket),
	}
}

// Stop instructs the orchestrator to stop all active mailboxes.
func (mo *mailOrchestrator) Stop() {
	for _, mailbox := range mo.mailboxes {
		mailbox.Stop()
	}
}

// GetOrCreateMailBox returns an existing mailbox belonging to `chanID`, or
// creates and returns a new mailbox if none is found.
func (mo *mailOrchestrator) GetOrCreateMailBox(chanID lnwire.ChannelID,
	shortChanID lnwire.ShortChannelID) MailBox {

	// First, try lookup the mailbox directly using only the shared mutex.
	mo.mu.RLock()
	mailbox, ok := mo.mailboxes[chanID]
	if ok {
		mo.mu.RUnlock()
		return mailbox
	}
	mo.mu.RUnlock()

	// Otherwise, we will try again with exclusive lock, creating a mailbox
	// if one still has not been created.
	mo.mu.Lock()
	mailbox = mo.exclusiveGetOrCreateMailBox(chanID, shortChanID)
	mo.mu.Unlock()

	return mailbox
}

// exclusiveGetOrCreateMailBox checks for the existence of a mailbox for the
// given channel id. If none is found, a new one is creates, started, and
// recorded.
//
// NOTE: This method MUST be invoked with the mailOrchestrator's exclusive lock.
func (mo *mailOrchestrator) exclusiveGetOrCreateMailBox(
	chanID lnwire.ChannelID, shortChanID lnwire.ShortChannelID) MailBox {

	mailbox, ok := mo.mailboxes[chanID]
	if !ok {
		mailbox = newMemoryMailBox(&mailBoxConfig{
			shortChanID:       shortChanID,
			forwardPackets:    mo.cfg.forwardPackets,
			clock:             mo.cfg.clock,
			expiry:            mo.cfg.expiry,
			failMailboxUpdate: mo.cfg.failMailboxUpdate,
		})
		mailbox.Start()
		mo.mailboxes[chanID] = mailbox
	}

	return mailbox
}

// BindLiveShortChanID registers that messages bound for a particular short
// channel id should be forwarded to the mailbox corresponding to the given
// channel id. This method also checks to see if there are any unclaimed
// packets for this short_chan_id. If any are found, they are delivered to the
// mailbox and removed (marked as claimed).
func (mo *mailOrchestrator) BindLiveShortChanID(mailbox MailBox,
	cid lnwire.ChannelID, sid lnwire.ShortChannelID) {

	mo.mu.Lock()
	// Update the mapping from short channel id to mailbox's channel id.
	mo.liveIndex[sid] = cid

	// Retrieve any unclaimed packets destined for this mailbox.
	pkts := mo.unclaimedPackets[sid]
	delete(mo.unclaimedPackets, sid)
	mo.mu.Unlock()

	// Deliver the unclaimed packets.
	for _, pkt := range pkts {
		mailbox.AddPacket(pkt)
	}
}

// Deliver lookups the target mailbox using the live index from short_chan_id
// to channel_id. If the mailbox is found, the message is delivered directly.
// Otherwise the packet is recorded as unclaimed, and will be delivered to the
// mailbox upon the subsequent call to BindLiveShortChanID.
func (mo *mailOrchestrator) Deliver(
	sid lnwire.ShortChannelID, pkt *htlcPacket) error {

	var (
		mailbox MailBox
		found   bool
	)

	// First, try to find the channel id for the target short_chan_id. If
	// the link is live, we will also look up the created mailbox.
	mo.mu.RLock()
	chanID, isLive := mo.liveIndex[sid]
	if isLive {
		mailbox, found = mo.mailboxes[chanID]
	}
	mo.mu.RUnlock()

	// The link is live and target mailbox was found, deliver immediately.
	if isLive && found {
		return mailbox.AddPacket(pkt)
	}

	// If we detected that the link has not been made live, we will acquire
	// the exclusive lock preemptively in order to queue this packet in the
	// list of unclaimed packets.
	mo.mu.Lock()

	// Double check to see if the mailbox has been not made live since the
	// release of the shared lock.
	//
	// NOTE: Checking again with the exclusive lock held prevents a race
	// condition where BindLiveShortChanID is interleaved between the
	// release of the shared lock, and acquiring the exclusive lock. The
	// result would be stuck packets, as they wouldn't be redelivered until
	// the next call to BindLiveShortChanID, which is expected to occur
	// infrequently.
	chanID, isLive = mo.liveIndex[sid]
	if isLive {
		// Reaching this point indicates the mailbox is actually live.
		// We'll try to load the mailbox using the fresh channel id.
		//
		// NOTE: This should never create a new mailbox, as the live
		// index should only be set if the mailbox had been initialized
		// beforehand.  However, this does ensure that this case is
		// handled properly in the event that it could happen.
		mailbox = mo.exclusiveGetOrCreateMailBox(chanID, sid)
		mo.mu.Unlock()

		// Deliver the packet to the mailbox if it was found or created.
		return mailbox.AddPacket(pkt)
	}

	// Finally, if the channel id is still not found in the live index,
	// we'll add this to the list of unclaimed packets. These will be
	// delivered upon the next call to BindLiveShortChanID.
	mo.unclaimedPackets[sid] = append(mo.unclaimedPackets[sid], pkt)
	mo.mu.Unlock()

	return nil
}
