package main

import (
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
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

// link represents an active channel capable of forwarding HTLCs. Each
// active channel registered with the htlc switch creates a new link which will
// be used for forwarding outgoing HTLCs. The link also has additional
// metadata such as the current available bandwidth of the link (in satoshis)
// which aid the switch in optimally forwarding HTLCs.
type link struct {
	capacity btcutil.Amount

	availableBandwidth int64 // atomic

	linkChan chan *htlcPacket

	peer *peer

	chanPoint *wire.OutPoint
}

// htlcPacket is a wrapper around an lnwire message which adds, times out, or
// settles an active HTLC. The dest field denotes the name of the interface to
// forward this htlcPacket on.
type htlcPacket struct {
	sync.RWMutex

	dest chainhash.Hash

	srcLink wire.OutPoint
	onion   *sphinx.ProcessedPacket

	msg lnwire.Message

	// TODO(roasbeef): refactor and add type to pkt message
	payHash [32]byte
	amt     btcutil.Amount

	err chan error
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
	// TODO(roasbeef): add reference count so know when to delete?
	//  * atomic int re
	//  * due to same r-value being re-used?

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

	// chanIndex maps a channel's outpoint to a link which contains
	// additional information about the channel, and additionally houses a
	// pointer to the peer managing the channel.
	chanIndexMtx sync.RWMutex
	chanIndex    map[wire.OutPoint]*link

	// interfaces maps a node's ID to the set of links (active channels) we
	// currently have open with that peer.
	// TODO(roasbeef): combine w/ onionIndex?
	interfaceMtx sync.RWMutex
	interfaces   map[chainhash.Hash][]*link

	// onionIndex is an index used to properly forward a message
	// to the next hop within a Sphinx circuit. Within the sphinx packets,
	// the "next-hop" destination is encoded as the hash160 of the node's
	// public key serialized in compressed format.
	onionMtx   sync.RWMutex
	onionIndex map[[ripemd160.Size]byte][]*link

	// paymentCircuits maps a circuit key to an active payment circuit
	// amongst two oepn channels. This map is used to properly clear/settle
	// onion routed payments within the network.
	paymentCircuits map[circuitKey]*paymentCircuit

	// linkControl is a channel used by connected links to notify the
	// switch of a non-multi-hop triggered link state update.
	linkControl chan interface{}

	// outgoingPayments is a channel that outgoing payments initiated by
	// the RPC system.
	outgoingPayments chan *htlcPacket

	// htlcPlex is the channel which all connected links use to
	// coordinate the setup/teardown of Sphinx (onion routing) payment
	// circuits. Active links forward any add/settle messages over this
	// channel each state transition, sending new adds/settles which are
	// fully locked in.
	htlcPlex chan *htlcPacket

	// TODO(roasbeef): sampler to log sat/sec and tx/sec

	wg   sync.WaitGroup
	quit chan struct{}
}

// newHtlcSwitch creates a new htlcSwitch.
func newHtlcSwitch() *htlcSwitch {
	return &htlcSwitch{
		chanIndex:        make(map[wire.OutPoint]*link),
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
func (h *htlcSwitch) SendHTLC(htlcPkt *htlcPacket) error {
	htlcPkt.err = make(chan error, 1)

	h.outgoingPayments <- htlcPkt

	return <-htlcPkt.err
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
	var numUpdates uint64
	var satSent, satRecv btcutil.Amount
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
				htlcPkt.err <- err
				continue
			}

			wireMsg := htlcPkt.msg.(*lnwire.HTLCAddRequest)
			amt := btcutil.Amount(wireMsg.Amount)

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
					link.linkChan <- htlcPkt
				}()

				n := atomic.AddInt64(&link.availableBandwidth,
					-int64(amt))
				hswcLog.Tracef("Decrementing link %v bandwidth to %v",
					link.chanPoint, n)

				continue out
			}

			hswcLog.Errorf("Unable to send payment, insufficient capacity")
			htlcPkt.err <- fmt.Errorf("Insufficient capacity")
		case pkt := <-h.htlcPlex:
			// TODO(roasbeef): properly account with cleared vs settled
			numUpdates += 1

			hswcLog.Tracef("plex packet: %v", newLogClosure(func() string {
				return spew.Sdump(pkt)
			}))

			switch wireMsg := pkt.msg.(type) {
			// A link has just forwarded us a new HTLC, therefore
			// we initiate the payment circuit within our internal
			// state so we can properly forward the ultimate
			// settle message.
			case *lnwire.HTLCAddRequest:
				payHash := wireMsg.RedemptionHashes[0]

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
						msg: &lnwire.CancelHTLC{
							Reason: lnwire.UnknownDestination,
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

				// If the link we're attempting to forward the
				// HTLC over has insufficient capacity, then
				// we'll cancel the HTLC as the payment cannot
				// succeed.
				linkBandwidth := atomic.LoadInt64(&clearLink[0].availableBandwidth)
				if linkBandwidth < int64(wireMsg.Amount) {
					hswcLog.Errorf("unable to forward HTLC "+
						"link %v has insufficient "+
						"capacity, have %v need %v",
						clearLink[0].chanPoint, linkBandwidth,
						int64(wireMsg.Amount))

					pkt := &htlcPacket{
						payHash: payHash,
						msg: &lnwire.CancelHTLC{
							Reason: lnwire.InsufficientCapacity,
						},
						err: make(chan error, 1),
					}

					settleLink.linkChan <- pkt
					continue
				}

				circuit := &paymentCircuit{
					clear:  clearLink[0],
					settle: settleLink,
				}

				cKey := circuitKey(wireMsg.RedemptionHashes[0])
				h.paymentCircuits[cKey] = circuit

				hswcLog.Debugf("Creating onion circuit for %x: %v<->%v",
					cKey[:], clearLink[0].chanPoint,
					settleLink.chanPoint)

				// With the circuit initiated, send the htlcPkt
				// to the clearing link within the circuit to
				// continue propagating the HTLC across the
				// network.
				circuit.clear.linkChan <- &htlcPacket{
					msg: wireMsg,
					err: make(chan error, 1),
				}

				// Reduce the available bandwidth for the link
				// as it will clear the above HTLC, increasing
				// the limbo balance within the channel.
				n := atomic.AddInt64(&circuit.clear.availableBandwidth,
					-int64(pkt.amt))
				hswcLog.Tracef("Decrementing link %v bandwidth to %v",
					circuit.clear.chanPoint, n)

				satRecv += pkt.amt

			// We've just received a settle message which means we
			// can finalize the payment circuit by forwarding the
			// settle msg to the link which initially created the
			// circuit.
			case *lnwire.HTLCSettleRequest:
				rHash := fastsha256.Sum256(wireMsg.RedemptionProofs[0][:])
				var cKey circuitKey
				copy(cKey[:], rHash[:])

				// If we initiated the payment then there won't
				// be an active circuit to continue propagating
				// the settle over. Therefore, we exit early.
				circuit, ok := h.paymentCircuits[cKey]
				if !ok {
					hswcLog.Debugf("No existing circuit "+
						"for %x to settle", rHash[:])
					satSent += pkt.amt
					continue
				}

				hswcLog.Debugf("Closing completed onion "+
					"circuit for %x: %v<->%v", rHash[:],
					circuit.clear.chanPoint,
					circuit.settle.chanPoint)

				circuit.settle.linkChan <- &htlcPacket{
					msg: wireMsg,
					err: make(chan error, 1),
				}

				// Increase the available bandwidth for the
				// link as it will settle the above HTLC,
				// subtracting from the limbo balance and
				// incrementing its local balance.
				n := atomic.AddInt64(&circuit.settle.availableBandwidth,
					int64(pkt.amt))
				hswcLog.Tracef("Incrementing link %v bandwidth to %v",
					circuit.settle.chanPoint, n)

				satSent += pkt.amt

				delete(h.paymentCircuits, cKey)

			// We've just received an HTLC cancellation triggered
			// by an upstream peer somewhere within the ultimate
			// route. In response, we'll terminate the payment
			// circuit and propagate the error backwards.
			case *lnwire.CancelHTLC:
				// In order to properly handle the error, we'll
				// need to look up the original circuit that
				// the incoming HTLC created.
				circuit, ok := h.paymentCircuits[pkt.payHash]
				if !ok {
					hswcLog.Debugf("No existing circuit "+
						"for %x to cancel", pkt.payHash)
					continue
				}

				// Since an outgoing HTLC we sent on the clear
				// link has been cancelled, we update the
				// bandwidth of the clear link, restoring the
				// value of the HTLC worth.
				n := atomic.AddInt64(&circuit.clear.availableBandwidth,
					int64(pkt.amt))
				hswcLog.Debugf("HTLC %x has been cancelled, "+
					"incrementing link %v bandwidth to %v", pkt.payHash,
					circuit.clear.chanPoint, n)

				// With our link info updated, we now continue
				// the error propagation by sending the
				// cancellation message over the link that sent
				// us the incoming HTLC.
				circuit.settle.linkChan <- &htlcPacket{
					msg:     wireMsg,
					payHash: pkt.payHash,
					err:     make(chan error, 1),
				}

				delete(h.paymentCircuits, pkt.payHash)
			}
		case <-logTicker.C:
			if numUpdates == 0 {
				continue
			}

			hswcLog.Infof("Sent %v satoshis, received %v satoshis in "+
				"the last 10 seconds (%v tx/sec)",
				satSent.ToUnit(btcutil.AmountSatoshi),
				satRecv.ToUnit(btcutil.AmountSatoshi),
				float64(numUpdates)/10)
			satSent = 0
			satRecv = 0
			numUpdates = 0
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
	newLink := &link{
		capacity:           req.linkInfo.Capacity,
		availableBandwidth: int64(req.linkInfo.LocalBalance),
		linkChan:           req.linkChan,
		peer:               req.peer,
		chanPoint:          chanPoint,
	}

	// First update the channel index with this new channel point. The
	// channel index will be used to quickly lookup channels in order to:
	// close them, update their link capacity, or possibly during multi-hop
	// HTLC forwarding.
	h.chanIndexMtx.Lock()
	h.chanIndex[*chanPoint] = newLink
	h.chanIndexMtx.Unlock()

	interfaceID := req.peer.lightningID

	h.interfaceMtx.Lock()
	h.interfaces[interfaceID] = append(h.interfaces[interfaceID], newLink)
	h.interfaceMtx.Unlock()

	// Next, update the onion index which is used to look up the
	// settle/clear links during multi-hop payments and to dispatch
	// outgoing payments initiated by a local subsystem.
	var onionId [ripemd160.Size]byte
	copy(onionId[:], btcutil.Hash160(req.peer.addr.IdentityKey.SerializeCompressed()))

	h.onionMtx.Lock()
	h.onionIndex[onionId] = h.interfaces[interfaceID]
	h.onionMtx.Unlock()

	hswcLog.Infof("registering new link, interface=%x, onion_link=%x, "+
		"chan_point=%v, capacity=%v", interfaceID[:], onionId,
		chanPoint, newLink.capacity)

	if req.done != nil {
		req.done <- struct{}{}
	}
}

// handleUnregisterLink unregisters a currently active link. If the deletion of
// this link leaves the interface empty, then the interface entry itself is
// also deleted.
func (h *htlcSwitch) handleUnregisterLink(req *unregisterLinkMsg) {
	hswcLog.Debugf("unregistering active link, interface=%v, chan_point=%v",
		hex.EncodeToString(req.chanInterface[:]), req.chanPoint)

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
	if req.chanPoint == nil {
		hswcLog.Debugf("purging all active links for interface %v",
			hex.EncodeToString(chanInterface[:]))

		for _, link := range links {
			delete(h.chanIndex, *link.chanPoint)
		}

		links = nil
	} else {
		delete(h.chanIndex, *req.chanPoint)

		for i := 0; i < len(links); i++ {
			chanLink := links[i]
			if chanLink.chanPoint == req.chanPoint {
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
		var onionId [ripemd160.Size]byte
		copy(onionId[:], btcutil.Hash160(req.remoteID))
		delete(h.onionIndex, onionId)

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
	h.chanIndexMtx.RLock()
	targetLink, ok := h.chanIndex[*req.chanPoint]
	h.chanIndexMtx.RUnlock()

	if !ok {
		req.err <- fmt.Errorf("channel point %v not found, or peer "+
			"offline", req.chanPoint)
		return
	}

	hswcLog.Debugf("requesting interface %v to close link %v",
		hex.EncodeToString(targetLink.peer.lightningID[:]), req.chanPoint)
	targetLink.peer.localCloseChanReqs <- req

	// TODO(roasbeef): if type was CloseBreach initiate force closure with
	// all other channels (if any) we have with the remote peer.
}

// handleLinkUpdate processes the link info update message by adjusting the
// channel's available bandwidth by the delta specified within the message.
func (h *htlcSwitch) handleLinkUpdate(req *linkInfoUpdateMsg) {
	h.chanIndexMtx.RLock()
	link := h.chanIndex[*req.targetLink]
	h.chanIndexMtx.RUnlock()

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
	chanPoint     *wire.OutPoint

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
func (h *htlcSwitch) UnregisterLink(remotePub *btcec.PublicKey, chanPoint *wire.OutPoint) {
	done := make(chan struct{}, 1)
	rawPub := remotePub.SerializeCompressed()

	h.linkControl <- &unregisterLinkMsg{
		chanInterface: fastsha256.Sum256(rawPub),
		chanPoint:     chanPoint,
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
	targetLink *wire.OutPoint

	bandwidthDelta btcutil.Amount
}

// UpdateLink sends a message to the switch to update the available bandwidth
// within the link by the passed satoshi delta. This function may be used when
// re-anchoring to boost the capacity of a channel, or once a peer settles an
// HTLC invoice.
func (h *htlcSwitch) UpdateLink(chanPoint *wire.OutPoint, bandwidthDelta btcutil.Amount) {
	h.linkControl <- &linkInfoUpdateMsg{chanPoint, bandwidthDelta}
}
