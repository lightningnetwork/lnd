package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"golang.org/x/crypto/ripemd160"
)

const (
	// htlcQueueSize...
	// buffer bloat ;)
	htlcQueueSize = 50
)

var (
	zeroBytes [32]byte
)

// boundedLinkChan is a simple wrapper around a link's communication channel
// that bounds the total flow into and through the channel. Channels attached
// the link have a value which defines the max number of pending HTLC's present
// within the commitment transaction. Using this struct we establish a
// synchronization primitive that ensure we don't send additional htlcPackets
// to a link if the max limit has een reached. Once HTLC's are cleared from the
// commitment transaction, slots are freed up and more can proceed.
type boundedLinkChan struct {
	// slots is a buffered channel whose buffer is the total number of
	// outstanding HTLC's we can add to a link's commitment transaction.
	// This channel is essentially used as a semaphore.
	slots chan struct{}

	// linkChan is a channel that is connected to the channel state machine
	// for a link. The switch will send adds, settles, and cancels over
	// this channel.
	linkChan chan *htlcPacket
}

// newBoundedChan makes a new boundedLinkChan that has numSlots free slots that
// are depleted on each send until a slot is re-stored. linkChan is the
// underlying channel that will be sent upon.
func newBoundedLinkChan(numSlots uint32,
	linkChan chan *htlcPacket) *boundedLinkChan {

	b := &boundedLinkChan{
		slots:    make(chan struct{}, numSlots),
		linkChan: linkChan,
	}

	b.restoreSlots(numSlots)
	return b
}

// sendAndConsume sends a packet to the linkChan and consumes a single token in
// the process.
//
// TODO(roasbeef): add error fall through case?
func (b *boundedLinkChan) sendAndConsume(pkt *htlcPacket) {
	<-b.slots
	b.linkChan <- pkt
}

// sendAndRestore sends a packet to the linkChan and consumes a single token in
// the process. This method is called when the switch sends either a cancel or
// settle HTLC message to the link.
func (b *boundedLinkChan) sendAndRestore(pkt *htlcPacket) {
	b.linkChan <- pkt
	b.slots <- struct{}{}
}

// consumeSlot consumes a single slot from the bounded channel. This method is
// called once the switch receives a new htlc add message from a link right
// before forwarding it to the next hop.
func (b *boundedLinkChan) consumeSlot() {
	<-b.slots
}

// restoreSlot restores a single slots to the bounded channel. This method is
// called once the switch receives an HTLC cancel or settle from a link.
func (b *boundedLinkChan) restoreSlot() {
	b.slots <- struct{}{}
}

// restoreSlots adds numSlots additional slots to the bounded channel.
func (b *boundedLinkChan) restoreSlots(numSlots uint32) {
	for i := uint32(0); i < numSlots; i++ {
		b.slots <- struct{}{}
	}
}

// link represents an active channel capable of forwarding HTLCs. Each
// active channel registered with the htlc switch creates a new link which will
// be used for forwarding outgoing HTLCs. The link also has additional
// metadata such as the current available bandwidth of the link (in satoshis)
// which aid the switch in optimally forwarding HTLCs.
type link struct {
	chanID lnwire.ChannelID

	capacity btcutil.Amount

	availableBandwidth int64 // atomic

	peer *peer

	*boundedLinkChan
}

// htlcPacket is a wrapper around an lnwire message which adds, times out, or
// settles an active HTLC. The dest field denotes the name of the interface to
// forward this htlcPacket on.
type htlcPacket struct {
	sync.RWMutex

	dest chainhash.Hash

	srcLink lnwire.ChannelID
	onion   *sphinx.ProcessedPacket

	msg lnwire.Message

	// TODO(roasbeef): refactor and add type to pkt message
	payHash [32]byte
	amt     btcutil.Amount

	preImage chan [32]byte

	err  chan error
	done chan struct{}
}

// circuitKey uniquely identifies an active Sphinx (onion routing) circuit
// between two open channels. Currently, the rHash of the HTLC which created
// the circuit is used to uniquely identify each circuit.
// TODO(roasbeef): need to also add in the settle/clear channel points in order
// to support fragmenting payments on the link layer: 1 to N, N to N, etc.
type circuitKey [32]byte

// paymentCircuit represents an active Sphinx (onion routing) circuit between
// two active links within the htlcSwitch. A payment circuit is created once a
// link forwards an HTLC add request which initiates the creation of the
// circuit.  The onion routing information contained within this message is
// used to identify the settle/clear ends of the circuit. A circuit may be
// re-used (not torndown) in the case that multiple HTLCs with the send RHash
// are sent.
type paymentCircuit struct {
	refCount uint32

	// clear is the link the htlcSwitch will forward the HTLC add message
	// that initiated the circuit to. Once the message is forwarded, the
	// payment circuit is considered "active" from the POV of the switch as
	// both the incoming/outgoing channels have the cleared HTLC within
	// their latest state.
	clear *link

	// settle is the link the htlcSwitch will forward the HTLC settle it
	// receives from the outgoing peer to. Once the switch forwards the
	// settle message to this link, the payment circuit is considered
	// complete unless the reference count on the circuit is greater than
	// 1.
	settle *link
}

// htlcSwitch is a central messaging bus for all incoming/outgoing HTLCs.
// Connected peers with active channels are treated as named interfaces which
// refer to active channels as links. A link is the switch's message
// communication point with the goroutine that manages an active channel. New
// links are registered each time a channel is created, and unregistered once
// the channel is closed. The switch manages the hand-off process for multi-hop
// HTLCs, forwarding HTLCs initiated from within the daemon, and additionally
// splitting up incoming/outgoing HTLCs to a particular interface amongst many
// links (payment fragmentation).
// TODO(roasbeef): active sphinx circuits need to be synced to disk
type htlcSwitch struct {
	started  int32 // atomic
	shutdown int32 // atomic

	// chanIndex maps a channel's ID to a link which contains additional
	// information about the channel, and additionally houses a pointer to
	// the peer managing the channel.
	chanIndexMtx sync.RWMutex
	chanIndex    map[lnwire.ChannelID]*link

	// interfaces maps a node's ID to the set of links (active channels) we
	// currently have open with that peer.
	// TODO(roasbeef): combine w/ onionIndex?
	interfaceMtx sync.RWMutex
	interfaces   map[chainhash.Hash][]*link

	// onionIndex is an index used to properly forward a message to the
	// next hop within a Sphinx circuit. Within the sphinx packets, the
	// "next-hop" destination is encoded as the hash160 of the node's
	// public key serialized in compressed format.
	onionMtx   sync.RWMutex
	onionIndex map[[ripemd160.Size]byte][]*link

	// paymentCircuits maps a circuit key to an active payment circuit
	// amongst two open channels. This map is used to properly clear/settle
	// onion routed payments within the network.
	paymentCircuits map[circuitKey]*paymentCircuit

	// linkControl is a channel used by connected links to notify the
	// switch of a non-multi-hop triggered link state update.
	linkControl chan interface{}

	// outgoingPayments is a channel that outgoing payments initiated by
	// the RPC system.
	outgoingPayments chan *htlcPacket

	// htlcPlex is the channel which all connected links use to coordinate
	// the setup/teardown of Sphinx (onion routing) payment circuits.
	// Active links forward any add/settle messages over this channel each
	// state transition, sending new adds/settles which are fully locked
	// in.
	htlcPlex chan *htlcPacket

	// TODO(roasbeef): sampler to log sat/sec and tx/sec

	wg   sync.WaitGroup
	quit chan struct{}
}

// newHtlcSwitch creates a new htlcSwitch.
func newHtlcSwitch() *htlcSwitch {
	return &htlcSwitch{
		chanIndex:        make(map[lnwire.ChannelID]*link),
		interfaces:       make(map[chainhash.Hash][]*link),
		onionIndex:       make(map[[ripemd160.Size]byte][]*link),
		paymentCircuits:  make(map[circuitKey]*paymentCircuit),
		linkControl:      make(chan interface{}),
		htlcPlex:         make(chan *htlcPacket, htlcQueueSize),
		outgoingPayments: make(chan *htlcPacket, htlcQueueSize),
		quit:             make(chan struct{}),
	}
}

// Start starts all helper goroutines required for the operation of the switch.
func (h *htlcSwitch) Start() error {
	if !atomic.CompareAndSwapInt32(&h.started, 0, 1) {
		return nil
	}

	hswcLog.Tracef("Starting HTLC switch")

	h.wg.Add(2)
	go h.networkAdmin()
	go h.htlcForwarder()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (h *htlcSwitch) Stop() error {
	if !atomic.CompareAndSwapInt32(&h.shutdown, 0, 1) {
		return nil
	}

	hswcLog.Infof("HLTC switch shutting down")

	close(h.quit)
	h.wg.Wait()

	return nil
}

// SendHTLC queues a HTLC packet for forwarding over the designated interface.
// In the event that the interface has insufficient capacity for the payment,
// an error is returned. Additionally, if the interface cannot be found, an
// alternative error is returned.
func (h *htlcSwitch) SendHTLC(htlcPkt *htlcPacket) ([32]byte, error) {
	htlcPkt.err = make(chan error, 1)
	htlcPkt.done = make(chan struct{})
	htlcPkt.preImage = make(chan [32]byte, 1)

	h.outgoingPayments <- htlcPkt

	return <-htlcPkt.preImage, <-htlcPkt.err
}

// htlcForwarder is responsible for optimally forwarding (and possibly
// fragmenting) incoming/outgoing HTLCs amongst all active interfaces and
// their links. The duties of the forwarder are similar to that of a network
// switch, in that it facilitates multi-hop payments by acting as a central
// messaging bus. The switch communicates will active links to create, manage,
// and tear down active onion routed payments. Each active channel is modeled
// as networked device with metadata such as the available payment bandwidth,
// and total link capacity.
func (h *htlcSwitch) htlcForwarder() {
	// TODO(roasbeef): track pending payments here instead of within each peer?
	// Examine settles/timeouts from htlcPlex. Add src to htlcPacket, key by
	// (src, htlcKey).

	// TODO(roasbeef): cleared vs settled distinction
	var (
		deltaNumUpdates, totalNumUpdates uint64

		deltaSatSent, deltaSatRecv btcutil.Amount
		totalSatSent, totalSatRecv btcutil.Amount
	)
	logTicker := time.NewTicker(10 * time.Second)
out:
	for {
		select {
		case htlcPkt := <-h.outgoingPayments:
			dest := htlcPkt.dest
			h.interfaceMtx.RLock()
			chanInterface, ok := h.interfaces[dest]
			h.interfaceMtx.RUnlock()
			if !ok {
				err := fmt.Errorf("Unable to locate link %x",
					dest[:])
				hswcLog.Errorf(err.Error())
				htlcPkt.preImage <- zeroBytes
				htlcPkt.err <- err
				continue
			}

			wireMsg := htlcPkt.msg.(*lnwire.UpdateAddHTLC)
			amt := wireMsg.Amount

			// Handle this send request in a distinct goroutine in
			// order to avoid a possible deadlock between the htlc
			// switch and channel's htlc manager.
			for _, link := range chanInterface {
				// TODO(roasbeef): implement HTLC fragmentation
				//  * avoid full channel depletion at higher
				//    level (here) instead of within state
				//    machine?
				if atomic.LoadInt64(&link.availableBandwidth) < int64(amt) {
					continue
				}

				hswcLog.Tracef("Sending %v to %x", amt, dest[:])

				go func() {
					link.sendAndConsume(htlcPkt)
					<-htlcPkt.done
					link.restoreSlot()
				}()

				n := atomic.AddInt64(&link.availableBandwidth,
					-int64(amt))
				hswcLog.Tracef("Decrementing link %v bandwidth to %v",
					link.chanID, n)

				continue out
			}

			hswcLog.Errorf("Unable to send payment, insufficient capacity")
			htlcPkt.preImage <- zeroBytes
			htlcPkt.err <- fmt.Errorf("Insufficient capacity")
		case pkt := <-h.htlcPlex:
			// TODO(roasbeef): properly account with cleared vs settled
			deltaNumUpdates++

			hswcLog.Tracef("plex packet: %v", newLogClosure(func() string {
				if pkt.onion != nil {
					pkt.onion.Packet.Header.EphemeralKey.Curve = nil
				}
				return spew.Sdump(pkt)
			}))

			switch wireMsg := pkt.msg.(type) {
			// A link has just forwarded us a new HTLC, therefore
			// we initiate the payment circuit within our internal
			// state so we can properly forward the ultimate settle
			// message.
			case *lnwire.UpdateAddHTLC:
				payHash := wireMsg.PaymentHash

				// Create the two ends of the payment circuit
				// required to ensure completion of this new
				// payment.
				nextHop := pkt.onion.NextHop
				h.onionMtx.RLock()
				clearLink, ok := h.onionIndex[nextHop]
				h.onionMtx.RUnlock()
				if !ok {
					hswcLog.Errorf("unable to find dest end of "+
						"circuit: %x", nextHop)

					// We were unable to locate the
					// next-hop as encoded within the
					// Sphinx packet. Therefore, we send a
					// cancellation message back to the
					// source of the packet so they can
					// propagate the message back to the
					// origin.
					cancelPkt := &htlcPacket{
						payHash: payHash,
						msg: &lnwire.UpdateFailHTLC{
							Reason: []byte{uint8(lnwire.UnknownDestination)},
						},
						err: make(chan error, 1),
					}

					h.chanIndexMtx.RLock()
					cancelLink := h.chanIndex[pkt.srcLink]
					h.chanIndexMtx.RUnlock()

					cancelLink.linkChan <- cancelPkt
					continue
				}

				h.chanIndexMtx.RLock()
				settleLink := h.chanIndex[pkt.srcLink]
				h.chanIndexMtx.RUnlock()

				// As the link now has a new HTLC that's been
				// propagated to us, we'll consume a slot from
				// it's bounded channel.
				settleLink.consumeSlot()

				// If the link we're attempting to forward the
				// HTLC over has insufficient capacity, then
				// we'll cancel the HTLC as the payment cannot
				// succeed.
				linkBandwidth := atomic.LoadInt64(&clearLink[0].availableBandwidth)
				if linkBandwidth < int64(wireMsg.Amount) {
					hswcLog.Errorf("unable to forward HTLC "+
						"link %v has insufficient "+
						"capacity, have %v need %v",
						clearLink[0].chanID, linkBandwidth,
						int64(wireMsg.Amount))

					pkt := &htlcPacket{
						payHash: payHash,
						msg: &lnwire.UpdateFailHTLC{
							Reason: []byte{uint8(lnwire.InsufficientCapacity)},
						},
						err: make(chan error, 1),
					}

					// Send the cancel message along the
					// link, restoring a slot in the
					// bounded channel in the process.
					settleLink.sendAndRestore(pkt)
					continue
				}

				// Examine the circuit map to see if this
				// circuit is already in use or not. If so,
				// then we'll simply increment the reference
				// count. Otherwise, we'll create a new circuit
				// from scratch.
				//
				// TODO(roasbeef): include dest+src+amt in key
				cKey := circuitKey(wireMsg.PaymentHash)
				circuit, ok := h.paymentCircuits[cKey]
				if ok {
					hswcLog.Debugf("Increasing ref_count "+
						"of circuit: %x, from %v to %v",
						wireMsg.PaymentHash,
						circuit.refCount,
						circuit.refCount+1)

					circuit.refCount += 1
				} else {
					hswcLog.Debugf("Creating onion "+
						"circuit for %x: %v<->%v",
						cKey[:], clearLink[0].chanID,
						settleLink.chanID)

					circuit = &paymentCircuit{
						clear:    clearLink[0],
						settle:   settleLink,
						refCount: 1,
					}

					h.paymentCircuits[cKey] = circuit
				}

				// With the circuit initiated, send the htlcPkt
				// to the clearing link within the circuit to
				// continue propagating the HTLC across the
				// network.
				circuit.clear.sendAndConsume(&htlcPacket{
					msg:      wireMsg,
					preImage: make(chan [32]byte, 1),
					err:      make(chan error, 1),
					done:     make(chan struct{}),
				})

				// Reduce the available bandwidth for the link
				// as it will clear the above HTLC, increasing
				// the limbo balance within the channel.
				n := atomic.AddInt64(&circuit.clear.availableBandwidth,
					-int64(pkt.amt))
				hswcLog.Tracef("Decrementing link %v bandwidth to %v",
					circuit.clear.chanID, n)

				deltaSatRecv += pkt.amt

			// We've just received a settle message which means we
			// can finalize the payment circuit by forwarding the
			// settle msg to the link which initially created the
			// circuit.
			case *lnwire.UpdateFufillHTLC:
				rHash := sha256.Sum256(wireMsg.PaymentPreimage[:])
				var cKey circuitKey
				copy(cKey[:], rHash[:])

				// If we initiated the payment then there won't
				// be an active circuit to continue propagating
				// the settle over. Therefore, we exit early.
				circuit, ok := h.paymentCircuits[cKey]
				if !ok {
					hswcLog.Debugf("No existing circuit "+
						"for %x to settle", rHash[:])
					deltaSatSent += pkt.amt
					continue
				}

				circuit.clear.restoreSlot()

				circuit.settle.sendAndRestore(&htlcPacket{
					msg: wireMsg,
					err: make(chan error, 1),
				})

				// Increase the available bandwidth for the
				// link as it will settle the above HTLC,
				// subtracting from the limbo balance and
				// incrementing its local balance.
				n := atomic.AddInt64(&circuit.settle.availableBandwidth,
					int64(pkt.amt))
				hswcLog.Tracef("Incrementing link %v bandwidth to %v",
					circuit.settle.chanID, n)

				deltaSatSent += pkt.amt

				if circuit.refCount--; circuit.refCount == 0 {
					hswcLog.Debugf("Closing completed onion "+
						"circuit for %x: %v<->%v", rHash[:],
						circuit.clear.chanID,
						circuit.settle.chanID)
					delete(h.paymentCircuits, cKey)
				}

			// We've just received an HTLC cancellation triggered
			// by an upstream peer somewhere within the ultimate
			// route. In response, we'll terminate the payment
			// circuit and propagate the error backwards.
			case *lnwire.UpdateFailHTLC:
				// In order to properly handle the error, we'll
				// need to look up the original circuit that
				// the incoming HTLC created.
				circuit, ok := h.paymentCircuits[pkt.payHash]
				if !ok {
					hswcLog.Debugf("No existing circuit "+
						"for %x to cancel", pkt.payHash)
					continue
				}

				circuit.clear.restoreSlot()

				// Since an outgoing HTLC we sent on the clear
				// link has been cancelled, we update the
				// bandwidth of the clear link, restoring the
				// value of the HTLC worth.
				n := atomic.AddInt64(&circuit.clear.availableBandwidth,
					int64(pkt.amt))
				hswcLog.Debugf("HTLC %x has been cancelled, "+
					"incrementing link %v bandwidth to %v", pkt.payHash,
					circuit.clear.chanID, n)

				// With our link info updated, we now continue
				// the error propagation by sending the
				// cancellation message over the link that sent
				// us the incoming HTLC.
				circuit.settle.sendAndRestore(&htlcPacket{
					msg:     wireMsg,
					payHash: pkt.payHash,
					err:     make(chan error, 1),
				})

				if circuit.refCount--; circuit.refCount == 0 {
					hswcLog.Debugf("Closing cancelled onion "+
						"circuit for %x: %v<->%v", pkt.payHash,
						circuit.clear.chanID,
						circuit.settle.chanID)
					delete(h.paymentCircuits, pkt.payHash)
				}
			}
		case <-logTicker.C:
			if deltaNumUpdates == 0 {
				continue
			}

			oldSatSent := totalSatRecv
			oldSatRecv := totalSatRecv
			oldNumUpdates := totalNumUpdates

			newSatSent := oldSatRecv + deltaSatSent
			newSatRecv := totalSatRecv + deltaSatRecv
			newNumUpdates := totalNumUpdates + deltaNumUpdates

			satSent := newSatSent - oldSatSent
			satRecv := newSatRecv - oldSatRecv
			numUpdates := newNumUpdates - oldNumUpdates
			hswcLog.Infof("Sent %v satoshis, received %v satoshis in "+
				"the last 10 seconds (%v tx/sec)",
				satSent.ToUnit(btcutil.AmountSatoshi),
				satRecv.ToUnit(btcutil.AmountSatoshi),
				numUpdates)

			totalSatSent += deltaSatSent
			deltaSatSent = 0

			totalSatRecv += deltaSatRecv
			deltaSatRecv = 0

			totalNumUpdates += deltaNumUpdates
			deltaNumUpdates = 0
		case <-h.quit:
			break out
		}
	}
	h.wg.Done()
}

// networkAdmin is responsible for handling requests to register, unregister,
// and close any link. In the event that an unregister request leaves an
// interface with no active links, that interface is garbage collected.
func (h *htlcSwitch) networkAdmin() {
out:
	for {
		select {
		case msg := <-h.linkControl:
			switch req := msg.(type) {
			case *closeLinkReq:
				h.handleCloseLink(req)
			case *registerLinkMsg:
				h.handleRegisterLink(req)
			case *unregisterLinkMsg:
				h.handleUnregisterLink(req)
			case *linkInfoUpdateMsg:
				h.handleLinkUpdate(req)
			}
		case <-h.quit:
			break out
		}
	}
	h.wg.Done()
}

// handleRegisterLink registers a new link within the channel index, and also
// adds the link to the existing set of links for the target interface.
func (h *htlcSwitch) handleRegisterLink(req *registerLinkMsg) {
	chanPoint := req.linkInfo.ChannelPoint
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	newLink := &link{
		capacity:           req.linkInfo.Capacity,
		availableBandwidth: int64(req.linkInfo.LocalBalance),
		peer:               req.peer,
		chanID:             chanID,
	}

	// To ensure we never accidentally cause an HTLC overflow, we'll limit,
	// we'll use this buffered channel as as semaphore in order to limit
	// the number of outstanding HTLC's we extend to the target link.
	//const numSlots = (lnwallet.MaxHTLCNumber / 2) - 1
	const numSlots = lnwallet.MaxHTLCNumber - 5
	newLink.boundedLinkChan = newBoundedLinkChan(numSlots, req.linkChan)

	// First update the channel index with this new channel point. The
	// channel index will be used to quickly lookup channels in order to:
	// close them, update their link capacity, or possibly during multi-hop
	// HTLC forwarding.
	h.chanIndexMtx.Lock()
	h.chanIndex[chanID] = newLink
	h.chanIndexMtx.Unlock()

	interfaceID := req.peer.lightningID

	h.interfaceMtx.Lock()
	h.interfaces[interfaceID] = append(h.interfaces[interfaceID], newLink)
	h.interfaceMtx.Unlock()

	// Next, update the onion index which is used to look up the
	// settle/clear links during multi-hop payments and to dispatch
	// outgoing payments initiated by a local subsystem.
	var onionID [ripemd160.Size]byte
	copy(onionID[:], btcutil.Hash160(req.peer.addr.IdentityKey.SerializeCompressed()))

	h.onionMtx.Lock()
	h.onionIndex[onionID] = h.interfaces[interfaceID]
	h.onionMtx.Unlock()

	hswcLog.Infof("registering new link, interface=%x, onion_link=%x, "+
		"chan_point=%v, capacity=%v", interfaceID[:], onionID,
		chanPoint, newLink.capacity)

	if req.done != nil {
		req.done <- struct{}{}
	}
}

// handleUnregisterLink unregisters a currently active link. If the deletion of
// this link leaves the interface empty, then the interface entry itself is
// also deleted.
func (h *htlcSwitch) handleUnregisterLink(req *unregisterLinkMsg) {
	hswcLog.Debugf("unregistering active link, interface=%v, chan_id=%v",
		hex.EncodeToString(req.chanInterface[:]), req.chanID)

	chanInterface := req.chanInterface

	h.interfaceMtx.RLock()
	links := h.interfaces[chanInterface]
	h.interfaceMtx.RUnlock()

	h.chanIndexMtx.Lock()
	defer h.chanIndexMtx.Unlock()

	h.onionMtx.Lock()
	defer h.onionMtx.Unlock()

	// A request with a nil channel point indicates that all the current
	// links for this channel should be cleared.
	if req.chanID == nil {
		hswcLog.Debugf("purging all active links for interface %v",
			hex.EncodeToString(chanInterface[:]))

		for _, link := range links {
			delete(h.chanIndex, link.chanID)
		}

		links = nil
	} else {
		delete(h.chanIndex, *req.chanID)

		for i := 0; i < len(links); i++ {
			chanLink := links[i]
			if chanLink.chanID == *req.chanID {
				// We perform an in-place delete by sliding
				// every element down one, then slicing off the
				// last element. Additionally, we update the
				// slice reference within the source map to
				// ensure full deletion.
				copy(links[i:], links[i+1:])
				links[len(links)-1] = nil
				h.interfaceMtx.Lock()
				h.interfaces[chanInterface] = links[:len(links)-1]
				h.interfaceMtx.Unlock()

				break
			}
		}
	}

	if len(links) == 0 {
		hswcLog.Debugf("interface %v has no active links, destroying",
			hex.EncodeToString(chanInterface[:]))

		// Delete the peer from the onion index so that the
		// htlcForwarder knows not to attempt to forward any further
		// HTLCs in this direction.
		var onionID [ripemd160.Size]byte
		copy(onionID[:], btcutil.Hash160(req.remoteID))
		delete(h.onionIndex, onionID)

		// Finally, delete the interface itself so that outgoing
		// payments don't select this path.
		h.interfaceMtx.Lock()
		delete(h.interfaces, chanInterface)
		h.interfaceMtx.Unlock()

	}

	if req.done != nil {
		req.done <- struct{}{}
	}
}

// handleCloseLink sends a message to the peer responsible for the target
// channel point, instructing it to initiate a cooperative channel closure.
func (h *htlcSwitch) handleCloseLink(req *closeLinkReq) {
	chanID := lnwire.NewChanIDFromOutPoint(req.chanPoint)

	h.chanIndexMtx.RLock()
	targetLink, ok := h.chanIndex[chanID]
	h.chanIndexMtx.RUnlock()

	if !ok {
		req.err <- fmt.Errorf("channel %v not found, or peer "+
			"offline", req.chanPoint)
		return
	}

	hswcLog.Debugf("requesting interface %v to close link %v",
		hex.EncodeToString(targetLink.peer.lightningID[:]), chanID)
	targetLink.peer.localCloseChanReqs <- req

	// TODO(roasbeef): if type was CloseBreach initiate force closure with
	// all other channels (if any) we have with the remote peer.
}

// handleLinkUpdate processes the link info update message by adjusting the
// channel's available bandwidth by the delta specified within the message.
func (h *htlcSwitch) handleLinkUpdate(req *linkInfoUpdateMsg) {
	h.chanIndexMtx.RLock()
	link, ok := h.chanIndex[req.targetLink]
	h.chanIndexMtx.RUnlock()
	if !ok {
		hswcLog.Errorf("received link update for non-existent link: %v",
			req.targetLink)
		return
	}

	atomic.AddInt64(&link.availableBandwidth, int64(req.bandwidthDelta))

	hswcLog.Tracef("adjusting bandwidth of link %v by %v", req.targetLink,
		req.bandwidthDelta)
}

// registerLinkMsg is message which requests a new link to be registered.
type registerLinkMsg struct {
	peer     *peer
	linkInfo *channeldb.ChannelSnapshot

	linkChan chan *htlcPacket

	done chan struct{}
}

// RegisterLink requests the htlcSwitch to register a new active link. The new
// link encapsulates an active channel. The htlc plex channel is returned. The
// plex channel allows the switch to properly de-multiplex incoming/outgoing
// HTLC messages forwarding them to their proper destination in the multi-hop
// settings.
func (h *htlcSwitch) RegisterLink(p *peer, linkInfo *channeldb.ChannelSnapshot,
	linkChan chan *htlcPacket) chan *htlcPacket {

	done := make(chan struct{}, 1)
	req := &registerLinkMsg{p, linkInfo, linkChan, done}
	h.linkControl <- req

	<-done

	return h.htlcPlex
}

// unregisterLinkMsg is a message which requests the active link be unregistered.
type unregisterLinkMsg struct {
	chanInterface [32]byte
	chanID        *lnwire.ChannelID

	// remoteID is the identity public key of the node we're removing the
	// link between. The public key is expected to be serialized in
	// compressed form.
	// TODO(roasbeef): redo interface map
	remoteID []byte

	done chan struct{}
}

// UnregisterLink requests the htlcSwitch to register the new active link. An
// unregistered link will no longer be considered a candidate to forward
// HTLCs.
func (h *htlcSwitch) UnregisterLink(remotePub *btcec.PublicKey,
	chanID *lnwire.ChannelID) {

	done := make(chan struct{}, 1)
	rawPub := remotePub.SerializeCompressed()

	h.linkControl <- &unregisterLinkMsg{
		chanInterface: sha256.Sum256(rawPub),
		chanID:        chanID,
		remoteID:      rawPub,
		done:          done,
	}

	<-done
}

// LinkCloseType is an enum which signals the type of channel closure the switch
// should execute.
type LinkCloseType uint8

const (
	// CloseRegular indicates a regular cooperative channel closure should
	// be attempted.
	CloseRegular LinkCloseType = iota

	// CloseBreach indicates that a channel breach has been detected, and
	// the link should immediately be marked as unavailable.
	CloseBreach
)

// closeChanReq represents a request to close a particular channel specified by
// its outpoint.
type closeLinkReq struct {
	CloseType LinkCloseType

	chanPoint *wire.OutPoint

	updates chan *lnrpc.CloseStatusUpdate
	err     chan error
}

// CloseLink closes an active link targetted by its channel point. Closing the
// link initiates a cooperative channel closure iff forceClose is false. If
// forceClose is true, then a unilateral channel closure is executed.
// TODO(roasbeef): consolidate with UnregisterLink?
func (h *htlcSwitch) CloseLink(chanPoint *wire.OutPoint,
	closeType LinkCloseType) (chan *lnrpc.CloseStatusUpdate, chan error) {

	updateChan := make(chan *lnrpc.CloseStatusUpdate, 1)
	errChan := make(chan error, 1)

	h.linkControl <- &closeLinkReq{
		CloseType: closeType,
		chanPoint: chanPoint,
		updates:   updateChan,
		err:       errChan,
	}

	return updateChan, errChan
}

// linkInfoUpdateMsg encapsulates a request for the htlc switch to update the
// metadata related to the target link.
type linkInfoUpdateMsg struct {
	targetLink lnwire.ChannelID

	bandwidthDelta btcutil.Amount
}

// UpdateLink sends a message to the switch to update the available bandwidth
// within the link by the passed satoshi delta. This function may be used when
// re-anchoring to boost the capacity of a channel, or once a peer settles an
// HTLC invoice.
func (h *htlcSwitch) UpdateLink(chanID lnwire.ChannelID, delta btcutil.Amount) {
	h.linkControl <- &linkInfoUpdateMsg{
		targetLink:     chanID,
		bandwidthDelta: delta,
	}
}
