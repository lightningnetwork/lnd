package htlcswitch

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"

	"github.com/davecgh/go-spew/spew"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// ErrChannelLinkNotFound is used when channel link hasn't been found.
	ErrChannelLinkNotFound = errors.New("channel link not found")

	// zeroPreimage is the empty preimage which is returned when we have
	// some errors.
	zeroPreimage [sha256.Size]byte
)

// pendingPayment represents the payment which made by user and waits for
// updates to be received whether the payment has been rejected or proceed
// successfully.
type pendingPayment struct {
	paymentHash lnwallet.PaymentHash
	amount      lnwire.MilliSatoshi

	preimage chan [sha256.Size]byte
	err      chan error

	// deobfuscator is an serializable entity which is used if we received
	// an error, it deobfuscates the onion failure blob, and extracts the
	// exact error from it.
	deobfuscator Deobfuscator
}

// plexPacket encapsulates switch packet and adds error channel to receive
// error from request handler.
type plexPacket struct {
	pkt *htlcPacket
	err chan error
}

// ChannelCloseType is a enum which signals the type of channel closure the
// peer should execute.
type ChannelCloseType uint8

const (
	// CloseRegular indicates a regular cooperative channel closure
	// should be attempted.
	CloseRegular ChannelCloseType = iota

	// CloseBreach indicates that a channel breach has been dtected, and
	// the link should immediately be marked as unavailable.
	CloseBreach
)

// ChanClose represents a request which close a particular channel specified by
// its id.
type ChanClose struct {
	// CloseType is a variable which signals the type of channel closure the
	// peer should execute.
	CloseType ChannelCloseType

	// ChanPoint represent the id of the channel which should be closed.
	ChanPoint *wire.OutPoint

	// Updates is used by request creator to receive the notifications about
	// execution of the close channel request.
	Updates chan *lnrpc.CloseStatusUpdate

	// Err is used by request creator to receive request execution error.
	Err chan error
}

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
	// LocalChannelClose kicks-off the workflow to execute a cooperative
	// or forced unilateral closure of the channel initiated by a local
	// subsystem.
	LocalChannelClose func(pubKey []byte, request *ChanClose)

	// UpdateTopology sends the onion error failure topology update to router
	// subsystem.
	//
	// TODO(roasbeef): remove
	UpdateTopology func(msg *lnwire.ChannelUpdate) error
}

// Switch is the central messaging bus for all incoming/outgoing HTLCs.
// Connected peers with active channels are treated as named interfaces which
// refer to active channels as links. A link is the switch's message
// communication point with the goroutine that manages an active channel. New
// links are registered each time a channel is created, and unregistered once
// the channel is closed. The switch manages the hand-off process for multi-hop
// HTLCs, forwarding HTLCs initiated from within the daemon, and finally
// notifies users local-systems concerning their outstanding payment requests.
type Switch struct {
	started  int32
	shutdown int32
	wg       sync.WaitGroup
	quit     chan struct{}

	// cfg is a copy of the configuration struct that the htlc switch
	// service was initialized with.
	cfg *Config

	// pendingPayments is correspondence of user payments and its hashes,
	// which is used to save the payments which made by user and notify
	// them about result later.
	pendingPayments map[lnwallet.PaymentHash][]*pendingPayment
	pendingMutex    sync.RWMutex

	// circuits is storage for payment circuits which are used to
	// forward the settle/fail htlc updates back to the add htlc initiator.
	circuits *circuitMap

	// links is a map of channel id and channel link which manages
	// this channel.
	linkIndex map[lnwire.ChannelID]ChannelLink

	// forwardingIndex is an index which is consulted by the switch when it
	// needs to locate the next hop to forward an incoming/outgoing HTLC
	// update to/from.
	//
	// TODO(roasbeef): eventually add a NetworkHop mapping before the
	// ChannelLink
	forwardingIndex map[lnwire.ShortChannelID]ChannelLink

	// interfaceIndex maps the compressed public key of a peer to all the
	// channels that the switch maintains iwht that peer.
	interfaceIndex map[[33]byte]map[ChannelLink]struct{}

	// htlcPlex is the channel which all connected links use to coordinate
	// the setup/teardown of Sphinx (onion routing) payment circuits.
	// Active links forward any add/settle messages over this channel each
	// state transition, sending new adds/settles which are fully locked
	// in.
	htlcPlex chan *plexPacket

	// chanCloseRequests is used to transfer the channel close request to
	// the channel close handler.
	chanCloseRequests chan *ChanClose

	// linkControl is a channel used to propagate add/remove/get htlc
	// switch handler commands.
	linkControl chan interface{}
}

// New creates the new instance of htlc switch.
func New(cfg Config) *Switch {
	return &Switch{
		cfg:               &cfg,
		circuits:          newCircuitMap(),
		linkIndex:         make(map[lnwire.ChannelID]ChannelLink),
		forwardingIndex:   make(map[lnwire.ShortChannelID]ChannelLink),
		interfaceIndex:    make(map[[33]byte]map[ChannelLink]struct{}),
		pendingPayments:   make(map[lnwallet.PaymentHash][]*pendingPayment),
		htlcPlex:          make(chan *plexPacket),
		chanCloseRequests: make(chan *ChanClose),
		linkControl:       make(chan interface{}),
		quit:              make(chan struct{}),
	}
}

// SendHTLC is used by other subsystems which aren't belong to htlc switch
// package in order to send the htlc update.
func (s *Switch) SendHTLC(nextNode [33]byte, htlc *lnwire.UpdateAddHTLC,
	deobfuscator Deobfuscator) ([sha256.Size]byte, error) {

	// Create payment and add to the map of payment in order later to be
	// able to retrieve it and return response to the user.
	payment := &pendingPayment{
		err:          make(chan error, 1),
		preimage:     make(chan [sha256.Size]byte, 1),
		paymentHash:  htlc.PaymentHash,
		amount:       htlc.Amount,
		deobfuscator: deobfuscator,
	}

	s.pendingMutex.Lock()
	s.pendingPayments[htlc.PaymentHash] = append(
		s.pendingPayments[htlc.PaymentHash], payment)
	s.pendingMutex.Unlock()

	// Generate and send new update packet, if error will be received on
	// this stage it means that packet haven't left boundaries of our
	// system and something wrong happened.
	packet := newInitPacket(nextNode, htlc)
	if err := s.forward(packet); err != nil {
		s.removePendingPayment(payment.amount, payment.paymentHash)
		return zeroPreimage, err
	}

	// Returns channels so that other subsystem might wait/skip the
	// waiting of handling of payment.
	var preimage [sha256.Size]byte
	var err error

	select {
	case e := <-payment.err:
		err = e
	case <-s.quit:
		return zeroPreimage, errors.New("switch is shutting down")
	}

	select {
	case p := <-payment.preimage:
		preimage = p
	case <-s.quit:
		return zeroPreimage, errors.New("switch is shutting down")
	}

	return preimage, err
}

// UpdateForwardingPolicies sends a message to the switch to update the
// forwarding policies for the set of target channels. If the set of targeted
// channels is nil, then the forwarding policies for all active channels with
// be updated.
//
// NOTE: This function is synchronous and will block until either the
// forwarding policies for all links have been updated, or the switch shuts
// down.
func (s *Switch) UpdateForwardingPolicies(newPolicy ForwardingPolicy,
	targetChans ...wire.OutPoint) error {

	errChan := make(chan error, 1)
	select {
	case s.linkControl <- &updatePoliciesCmd{
		newPolicy:   newPolicy,
		targetChans: targetChans,
		err:         errChan,
	}:
	case <-s.quit:
		return fmt.Errorf("switch is shutting down")
	}

	select {
	case err := <-errChan:
		return err
	case <-s.quit:
		return fmt.Errorf("switch is shutting down")
	}
}

// updatePoliciesCmd is a message sent to the switch to update the forwarding
// policies of a set of target links.
type updatePoliciesCmd struct {
	newPolicy   ForwardingPolicy
	targetChans []wire.OutPoint

	err chan error
}

// updateLinkPolicies attempts to update the forwarding policies for the set of
// passed links identified by their channel points. If a nil set of channel
// points is passed, then the forwarding policies for all active links will be
// updated.k
func (s *Switch) updateLinkPolicies(c *updatePoliciesCmd) error {
	log.Debugf("Updating link policies: %v", spew.Sdump(c))

	// If no channels have been targeted, then we'll update the link policies
	// for all active channels
	if len(c.targetChans) == 0 {
		for _, link := range s.linkIndex {
			link.UpdateForwardingPolicy(c.newPolicy)
		}
	}

	// Otherwise, we'll only attempt to update the forwarding policies for the
	// set of targeted links.
	for _, targetLink := range c.targetChans {
		cid := lnwire.NewChanIDFromOutPoint(&targetLink)

		// If we can't locate a link by its converted channel ID, then we'll
		// return an error back to the caller.
		link, ok := s.linkIndex[cid]
		if !ok {
			return fmt.Errorf("unable to find ChannelPoint(%v) to "+
				"update link policy", targetLink)
		}

		link.UpdateForwardingPolicy(c.newPolicy)
	}

	return nil
}

// forward is used in order to find next channel link and apply htlc
// update. Also this function is used by channel links itself in order to
// forward the update after it has been included in the channel.
func (s *Switch) forward(packet *htlcPacket) error {
	command := &plexPacket{
		pkt: packet,
		err: make(chan error, 1),
	}

	select {
	case s.htlcPlex <- command:
	case <-s.quit:
		return errors.New("Htlc Switch was stopped")
	}

	select {
	case err := <-command.err:
		return err
	case <-s.quit:
		return errors.New("Htlc Switch was stopped")
	}
}

// handleLocalDispatch is used at the start/end of the htlc update life
// cycle. At the start (1) it is used to send the htlc to the channel link
// without creation of circuit. At the end (2) it is used to notify the user
// about the result of his payment is it was successful or not.
//
//   Alice         Bob          Carol
//     o --add----> o ---add----> o
//    (1)
//
//    (2)
//     o <-settle-- o <--settle-- o
//   Alice         Bob         Carol
//
func (s *Switch) handleLocalDispatch(payment *pendingPayment, packet *htlcPacket) error {
	switch htlc := packet.htlc.(type) {

	// User have created the htlc update therefore we should find the
	// appropriate channel link and send the payment over this link.
	case *lnwire.UpdateAddHTLC:
		// Try to find links by node destination.
		links, err := s.getLinks(packet.destNode)
		if err != nil {
			log.Errorf("unable to find links by "+
				"destination %v", err)
			return errors.New(lnwire.CodeUnknownNextPeer)
		}

		// Try to find destination channel link with appropriate
		// bandwidth.
		var destination ChannelLink
		for _, link := range links {
			if link.Bandwidth() >= htlc.Amount {
				destination = link
				break
			}
		}

		// If the channel link we're attempting to forward the update
		// over has insufficient capacity, then we'll cancel the HTLC
		// as the payment cannot succeed.
		if destination == nil {
			log.Errorf("unable to find appropriate channel link "+
				"insufficient capacity, need %v", htlc.Amount)
			return errors.New(lnwire.CodeTemporaryChannelFailure)
		}

		// Send the packet to the destination channel link which
		// manages then channel.
		destination.HandleSwitchPacket(packet)
		return nil

	// We've just received a settle update which means we can finalize
	// the user payment and return successful response.
	case *lnwire.UpdateFufillHTLC:
		// Notify the user that his payment was
		// successfully proceed.
		payment.err <- nil
		payment.preimage <- htlc.PaymentPreimage
		s.removePendingPayment(payment.amount, payment.paymentHash)

	// We've just received a fail update which means we can finalize the
	// user payment and return fail response.
	case *lnwire.UpdateFailHTLC:
		var userErr error

		// We'll attempt to fully decrypt the onion encrypted error. If
		// we're unable to then we'll bail early.
		failure, err := payment.deobfuscator.Deobfuscate(htlc.Reason)
		if err != nil {
			userErr = errors.Errorf("unable to de-obfuscate "+
				"onion failure, htlc with hash(%x): %v",
				payment.paymentHash[:], err)
			log.Error(userErr)
		} else {
			// Process payment failure by updating the lightning
			// network topology by using router subsystem handler.
			var update *lnwire.ChannelUpdate

			// Only a few error message actually contain a channel
			// update message, so we'll filter out for those that
			// do.
			switch failure := failure.(type) {
			case *lnwire.FailTemporaryChannelFailure:
				update = failure.Update
			case *lnwire.FailAmountBelowMinimum:
				update = &failure.Update
			case *lnwire.FailFeeInsufficient:
				update = &failure.Update
			case *lnwire.FailIncorrectCltvExpiry:
				update = &failure.Update
			case *lnwire.FailExpiryTooSoon:
				update = &failure.Update
			case *lnwire.FailChannelDisabled:
				update = &failure.Update
			}

			// If we've been sent an error that includes an update,
			// then we'll apply it to the local graph.
			//
			// TODO(roasbeef): instead, make all onion errors the
			// error interface, and handle this within the router.
			// Will allow us more flexibility w.r.t how we handle
			// the error.
			if update != nil {
				log.Infof("Received payment failure(%v), "+
					"applying lightning network topology update",
					failure.Code())

				if err := s.cfg.UpdateTopology(update); err != nil {
					log.Errorf("unable to update topology: %v", err)
				}
			}

			userErr = errors.New(failure.Code())
		}

		// Notify user that his payment was discarded.
		payment.err <- userErr
		payment.preimage <- zeroPreimage
		s.removePendingPayment(payment.amount, payment.paymentHash)

	default:
		return errors.New("wrong update type")
	}

	return nil
}

// handlePacketForward is used in cases when we need forward the htlc update
// from one channel link to another and be able to propagate the settle/fail
// updates back. This behaviour is achieved by creation of payment circuits.
func (s *Switch) handlePacketForward(packet *htlcPacket) error {
	switch htlc := packet.htlc.(type) {

	// Channel link forwarded us a new htlc, therefore we initiate the
	// payment circuit within our internal state so we can properly forward
	// the ultimate settle message back latter.
	case *lnwire.UpdateAddHTLC:
		source, err := s.getLinkByShortID(packet.src)
		if err != nil {
			err := errors.Errorf("unable to find channel link "+
				"by channel point (%v): %v", packet.src, err)
			log.Error(err)
			return err
		}

		targetLink, err := s.getLinkByShortID(packet.dest)
		if err != nil {
			// If packet was forwarded from another channel link
			// than we should notify this link that some error
			// occurred.
			failure := lnwire.FailUnknownNextPeer{}
			reason, err := packet.obfuscator.InitialObfuscate(failure)
			if err != nil {
				err := errors.Errorf("unable to obfuscate "+
					"error: %v", err)
				log.Error(err)
				return err
			}

			go source.HandleSwitchPacket(newFailPacket(
				packet.src,
				&lnwire.UpdateFailHTLC{
					Reason: reason,
				},
				htlc.PaymentHash, 0, true,
			))
			err = errors.Errorf("unable to find link with "+
				"destination %v", packet.dest)
			log.Error(err)
			return err
		}
		interfaceLinks, _ := s.getLinks(targetLink.Peer().PubKey())

		// Try to find destination channel link with appropriate
		// bandwidth.
		var destination ChannelLink
		for _, link := range interfaceLinks {
			if link.Bandwidth() >= htlc.Amount {
				destination = link
				break
			}
		}

		// If the channel link we're attempting to forward the update
		// over has insufficient capacity, then we'll cancel the htlc
		// as the payment cannot succeed.
		if destination == nil {
			// If packet was forwarded from another
			// channel link than we should notify this
			// link that some error occurred.
			failure := lnwire.NewTemporaryChannelFailure(nil)
			reason, err := packet.obfuscator.InitialObfuscate(failure)
			if err != nil {
				err := errors.Errorf("unable to obfuscate "+
					"error: %v", err)
				log.Error(err)
				return err
			}

			go source.HandleSwitchPacket(newFailPacket(
				packet.src,
				&lnwire.UpdateFailHTLC{
					Reason: reason,
				},
				htlc.PaymentHash,
				0, true,
			))

			err = errors.Errorf("unable to find appropriate "+
				"channel link insufficient capacity, need "+
				"%v", htlc.Amount)
			log.Error(err)
			return err
		}

		// If packet was forwarded from another channel link than we
		// should create circuit (remember the path) in order to
		// forward settle/fail packet back.
		if err := s.circuits.add(newPaymentCircuit(
			source.ShortChanID(),
			destination.ShortChanID(),
			htlc.PaymentHash,
			packet.obfuscator,
		)); err != nil {
			failure := lnwire.NewTemporaryChannelFailure(nil)
			reason, err := packet.obfuscator.InitialObfuscate(failure)
			if err != nil {
				err := errors.Errorf("unable to obfuscate "+
					"error: %v", err)
				log.Error(err)
				return err
			}

			go source.HandleSwitchPacket(newFailPacket(
				packet.src,
				&lnwire.UpdateFailHTLC{
					Reason: reason,
				},
				htlc.PaymentHash, 0, true,
			))
			err = errors.Errorf("unable to add circuit: "+
				"%v", err)
			log.Error(err)
			return err
		}

		// Send the packet to the destination channel link which
		// manages the channel.
		destination.HandleSwitchPacket(packet)
		return nil

	// We've just received a settle packet which means we can finalize the
	// payment circuit by forwarding the settle msg to the channel from
	// which htlc add packet was initially received.
	case *lnwire.UpdateFufillHTLC, *lnwire.UpdateFailHTLC:
		// Exit if we can't find and remove the active circuit to
		// continue propagating the fail over.
		circuit, err := s.circuits.remove(packet.payHash)
		if err != nil {
			err := errors.Errorf("unable to remove "+
				"circuit for payment hash: %v", packet.payHash)
			log.Error(err)
			return err
		}

		// If this is failure than we need to obfuscate the error.
		if htlc, ok := htlc.(*lnwire.UpdateFailHTLC); ok && !packet.isObfuscated {
			htlc.Reason = circuit.Obfuscator.BackwardObfuscate(
				htlc.Reason,
			)
		}

		// Propagating settle/fail htlc back to src of add htlc packet.
		source, err := s.getLinkByShortID(circuit.Src)
		if err != nil {
			err := errors.Errorf("unable to get source "+
				"channel link to forward settle/fail htlc: %v",
				err)
			log.Error(err)
			return err
		}

		log.Debugf("Closing completed onion "+
			"circuit for %x: %v<->%v", packet.payHash[:],
			circuit.Src, circuit.Dest)

		source.HandleSwitchPacket(packet)
		return nil

	default:
		return errors.New("wrong update type")
	}
}

// CloseLink creates and sends the close channel command.
func (s *Switch) CloseLink(chanPoint *wire.OutPoint,
	closeType ChannelCloseType) (chan *lnrpc.CloseStatusUpdate, chan error) {

	// TODO(roasbeef) abstract out the close updates.
	updateChan := make(chan *lnrpc.CloseStatusUpdate, 2)
	errChan := make(chan error, 1)

	command := &ChanClose{
		CloseType: closeType,
		ChanPoint: chanPoint,
		Updates:   updateChan,
		Err:       errChan,
	}

	select {
	case s.chanCloseRequests <- command:
		return updateChan, errChan

	case <-s.quit:
		errChan <- errors.New("unable close channel link, htlc " +
			"switch already stopped")
		close(updateChan)
		return updateChan, errChan
	}
}

// htlcForwarder is responsible for optimally forwarding (and possibly
// fragmenting) incoming/outgoing HTLCs amongst all active interfaces and their
// links. The duties of the forwarder are similar to that of a network switch,
// in that it facilitates multi-hop payments by acting as a central messaging
// bus. The switch communicates will active links to create, manage, and tear
// down active onion routed payments. Each active channel is modeled as
// networked device with metadata such as the available payment bandwidth, and
// total link capacity.
//
// NOTE: This MUST be run as a goroutine.
func (s *Switch) htlcForwarder() {
	defer s.wg.Done()

	// Remove all links once we've been signalled for shutdown.
	defer func() {
		for _, link := range s.linkIndex {
			if err := s.removeLink(link.ChanID()); err != nil {
				log.Errorf("unable to remove "+
					"channel link on stop: %v", err)
			}
		}
	}()

	// TODO(roasbeef): cleared vs settled distinction
	var (
		totalNumUpdates uint64
		totalSatSent    btcutil.Amount
		totalSatRecv    btcutil.Amount
	)
	logTicker := time.NewTicker(10 * time.Second)
	defer logTicker.Stop()

	for {
		select {
		// A local close request has arrived, we'll forward this to the
		// relevant link (if it exists) so the channel can be
		// cooperatively closed (if possible).
		case req := <-s.chanCloseRequests:
			chanID := lnwire.NewChanIDFromOutPoint(req.ChanPoint)
			link, ok := s.linkIndex[chanID]
			if !ok {
				req.Err <- errors.Errorf("channel with "+
					"chan_id=%x not found", chanID[:])
				continue
			}

			peerPub := link.Peer().PubKey()
			log.Debugf("Requesting local channel close: peer=%v, "+
				"chan_id=%x", link.Peer(), chanID[:])

			go s.cfg.LocalChannelClose(peerPub[:], req)

		// A new packet has arrived for forwarding, we'll interpret the
		// packet concretely, then either forward it along, or
		// interpret a return packet to a locally initialized one.
		case cmd := <-s.htlcPlex:
			var (
				paymentHash lnwallet.PaymentHash
				amount      lnwire.MilliSatoshi
			)

			// Only three types of message should be forwarded:
			// add, fails, and settles. Anything else is an error.
			switch m := cmd.pkt.htlc.(type) {
			case *lnwire.UpdateAddHTLC:
				paymentHash = m.PaymentHash
				amount = m.Amount
			case *lnwire.UpdateFufillHTLC, *lnwire.UpdateFailHTLC:
				paymentHash = cmd.pkt.payHash
				amount = cmd.pkt.amount
			default:
				cmd.err <- errors.New("wrong type of update")
				return
			}

			// If we can locate this packet in our local records,
			// then this means a local sub-system initiated it.
			// Otherwise, this is just a packet to be forwarded, so
			// we'll treat it as so.
			//
			// TODO(roasbeef): can fast path this
			payment, err := s.findPayment(amount, paymentHash)
			if err != nil {
				cmd.err <- s.handlePacketForward(cmd.pkt)
			} else {
				cmd.err <- s.handleLocalDispatch(payment, cmd.pkt)
			}

		// The log ticker has fired, so we'll calculate some forwarding
		// stats for the last 10 seconds to display within the logs to
		// users.
		case <-logTicker.C:
			// First, we'll collate the current running tally of
			// our forwarding stats.
			prevSatSent := totalSatSent
			prevSatRecv := totalSatRecv
			prevNumUpdates := totalNumUpdates

			var (
				newNumUpdates uint64
				newSatSent    btcutil.Amount
				newSatRecv    btcutil.Amount
			)

			// Next, we'll run through all the registered links and
			// compute their up-to-date forwarding stats.
			for _, link := range s.linkIndex {
				// TODO(roasbeef): when links first registered
				// stats printed.
				updates, sent, recv := link.Stats()
				newNumUpdates += updates
				newSatSent += sent.ToSatoshis()
				newSatRecv += recv.ToSatoshis()
			}

			var (
				diffNumUpdates uint64
				diffSatSent    btcutil.Amount
				diffSatRecv    btcutil.Amount
			)

			// If this is the first time we're computing these
			// stats, then the diff is just the new value. We do
			// this in order to avoid integer underflow issues.
			if prevNumUpdates == 0 {
				diffNumUpdates = newNumUpdates
				diffSatSent = newSatSent
				diffSatRecv = newSatRecv
			} else {
				diffNumUpdates = newNumUpdates - prevNumUpdates
				diffSatSent = newSatSent - prevSatSent
				diffSatRecv = newSatRecv - prevSatRecv
			}

			// If the diff of num updates is zero, then we haven't
			// forwarded anything in the last 10 seconds, so we can
			// skip this update.
			if diffNumUpdates == 0 {
				continue
			}

			// Otherwise, we'll log this diff, then accumulate the
			// new stats into the running total.
			log.Infof("Sent %v satoshis received %v satoshis "+
				"in the last 10 seconds (%v tx/sec)",
				int64(diffSatSent), int64(diffSatRecv),
				float64(diffNumUpdates)/10)

			totalNumUpdates += diffNumUpdates
			totalSatSent += diffSatSent
			totalSatRecv += diffSatRecv

		case req := <-s.linkControl:
			switch cmd := req.(type) {
			case *updatePoliciesCmd:
				cmd.err <- s.updateLinkPolicies(cmd)
			case *addLinkCmd:
				cmd.err <- s.addLink(cmd.link)
			case *removeLinkCmd:
				cmd.err <- s.removeLink(cmd.chanID)
			case *getLinkCmd:
				link, err := s.getLink(cmd.chanID)
				cmd.done <- link
				cmd.err <- err
			case *getLinksCmd:
				links, err := s.getLinks(cmd.peer)
				cmd.done <- links
				cmd.err <- err
			}

		case <-s.quit:
			return
		}
	}
}

// Start starts all helper goroutines required for the operation of the switch.
func (s *Switch) Start() error {
	if !atomic.CompareAndSwapInt32(&s.started, 0, 1) {
		log.Warn("Htlc Switch already started")
		return nil
	}

	log.Infof("Starting HTLC Switch")

	s.wg.Add(1)
	go s.htlcForwarder()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (s *Switch) Stop() error {
	if !atomic.CompareAndSwapInt32(&s.shutdown, 0, 1) {
		log.Warn("Htlc Switch already stopped")
		return nil
	}

	log.Infof("HLTC Switch shutting down")

	close(s.quit)
	s.wg.Wait()

	return nil
}

// addLinkCmd is a add link command wrapper, it is used to propagate handler
// parameters and return handler error.
type addLinkCmd struct {
	link ChannelLink
	err  chan error
}

// AddLink is used to initiate the handling of the add link command. The
// request will be propagated and handled in the main goroutine.
func (s *Switch) AddLink(link ChannelLink) error {
	command := &addLinkCmd{
		link: link,
		err:  make(chan error, 1),
	}

	select {
	case s.linkControl <- command:
		return <-command.err
	case <-s.quit:
		return errors.New("Htlc Switch was stopped")
	}
}

// addLink is used to add the newly created channel link and start use it to
// handle the channel updates.
func (s *Switch) addLink(link ChannelLink) error {
	// First we'll add the link to the linkIndex which lets us quickly look
	// up a channel when we need to close or register it, and the
	// forwarding index which'll be used when forwarding HTLC's in the
	// multi-hop setting.
	s.linkIndex[link.ChanID()] = link
	s.forwardingIndex[link.ShortChanID()] = link

	// Next we'll add the link to the interface index so we can quickly
	// look up all the channels for a particular node.
	peerPub := link.Peer().PubKey()
	if _, ok := s.interfaceIndex[peerPub]; !ok {
		s.interfaceIndex[peerPub] = make(map[ChannelLink]struct{})
	}
	s.interfaceIndex[peerPub][link] = struct{}{}

	if err := link.Start(); err != nil {
		return err
	}

	log.Infof("Added channel link with chan_id=%v, short_chan_id=(%v), "+
		"bandwidth=%v", link.ChanID(), spew.Sdump(link.ShortChanID()),
		link.Bandwidth())

	return nil
}

// getLinkCmd is a get link command wrapper, it is used to propagate handler
// parameters and return handler error.
type getLinkCmd struct {
	chanID lnwire.ChannelID
	err    chan error
	done   chan ChannelLink
}

// GetLink is used to initiate the handling of the get link command. The
// request will be propagated/handled to/in the main goroutine.
func (s *Switch) GetLink(chanID lnwire.ChannelID) (ChannelLink, error) {
	command := &getLinkCmd{
		chanID: chanID,
		err:    make(chan error, 1),
		done:   make(chan ChannelLink, 1),
	}

	select {
	case s.linkControl <- command:
		return <-command.done, <-command.err
	case <-s.quit:
		return nil, errors.New("Htlc Switch was stopped")
	}
}

// getLink attempts to return the link that has the specified channel ID.
func (s *Switch) getLink(chanID lnwire.ChannelID) (ChannelLink, error) {
	link, ok := s.linkIndex[chanID]
	if !ok {
		return nil, ErrChannelLinkNotFound
	}

	return link, nil
}

// getLinkByShortID attempts to return the link which possesses the target
// short channel ID.
func (s *Switch) getLinkByShortID(chanID lnwire.ShortChannelID) (ChannelLink, error) {
	link, ok := s.forwardingIndex[chanID]
	if !ok {
		return nil, ErrChannelLinkNotFound
	}

	return link, nil
}

// removeLinkCmd is a get link command wrapper, it is used to propagate handler
// parameters and return handler error.
type removeLinkCmd struct {
	chanID lnwire.ChannelID
	err    chan error
}

// RemoveLink is used to initiate the handling of the remove link command. The
// request will be propagated/handled to/in the main goroutine.
func (s *Switch) RemoveLink(chanID lnwire.ChannelID) error {
	command := &removeLinkCmd{
		chanID: chanID,
		err:    make(chan error, 1),
	}

	select {
	case s.linkControl <- command:
		return <-command.err
	case <-s.quit:
		return errors.New("Htlc Switch was stopped")
	}
}

// removeLink is used to remove and stop the channel link.
func (s *Switch) removeLink(chanID lnwire.ChannelID) error {
	log.Infof("Removing channel link with ChannelID(%v)", chanID)

	link, ok := s.linkIndex[chanID]
	if !ok {
		return ErrChannelLinkNotFound
	}

	// Remove the channel from channel map.
	delete(s.linkIndex, chanID)
	delete(s.forwardingIndex, link.ShortChanID())

	// Remove the channel from channel index.
	peerPub := link.Peer().PubKey()
	delete(s.interfaceIndex, peerPub)

	go link.Stop()

	return nil
}

// getLinksCmd is a get links command wrapper, it is used to propagate handler
// parameters and return handler error.
type getLinksCmd struct {
	peer [33]byte
	err  chan error
	done chan []ChannelLink
}

// GetLinksByInterface fetches all the links connected to a particular node
// identified by the serialized compressed form of its public key.
func (s *Switch) GetLinksByInterface(hop [33]byte) ([]ChannelLink, error) {
	command := &getLinksCmd{
		peer: hop,
		err:  make(chan error, 1),
		done: make(chan []ChannelLink, 1),
	}

	select {
	case s.linkControl <- command:
		return <-command.done, <-command.err
	case <-s.quit:
		return nil, errors.New("Htlc Switch was stopped")
	}
}

// getLinks is function which returns the channel links of the peer by hop
// destination id.
func (s *Switch) getLinks(destination [33]byte) ([]ChannelLink, error) {
	links, ok := s.interfaceIndex[destination]
	if !ok {
		return nil, errors.Errorf("unable to locate channel link by"+
			"destination hop id %x", destination)
	}

	channelLinks := make([]ChannelLink, 0, len(links))
	for link := range links {
		channelLinks = append(channelLinks, link)
	}

	return channelLinks, nil
}

// removePendingPayment is the helper function which removes the pending user
// payment.
func (s *Switch) removePendingPayment(amount lnwire.MilliSatoshi,
	hash lnwallet.PaymentHash) error {

	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	payments, ok := s.pendingPayments[hash]
	if ok {
		for i, payment := range payments {
			if payment.amount == amount {
				// Delete without preserving order
				// Google: Golang slice tricks
				payments[i] = payments[len(payments)-1]
				payments[len(payments)-1] = nil
				s.pendingPayments[hash] = payments[:len(payments)-1]

				if len(s.pendingPayments[hash]) == 0 {
					delete(s.pendingPayments, hash)
				}

				return nil
			}
		}
	}

	return errors.Errorf("unable to remove pending payment with "+
		"hash(%v) and amount(%v)", hash, amount)
}

// findPayment is the helper function which find the payment.
func (s *Switch) findPayment(amount lnwire.MilliSatoshi,
	hash lnwallet.PaymentHash) (*pendingPayment, error) {

	s.pendingMutex.RLock()
	defer s.pendingMutex.RUnlock()

	payments, ok := s.pendingPayments[hash]
	if ok {
		for _, payment := range payments {
			if payment.amount == amount {
				return payment, nil
			}
		}
	}

	return nil, errors.Errorf("unable to remove pending payment with "+
		"hash(%v) and amount(%v)", hash, amount)
}

// numPendingPayments is helper function which returns the overall number of
// pending user payments.
func (s *Switch) numPendingPayments() int {
	var l int
	for _, payments := range s.pendingPayments {
		l += len(payments)
	}

	return l
}
