package htlcswitch

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"

	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/btcec"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// ErrChannelLinkNotFound is used when channel link hasn't been found.
	ErrChannelLinkNotFound = errors.New("channel link not found")

	// ErrDuplicateAdd signals that the ADD htlc was already forwarded
	// through the switch.
	ErrDuplicateAdd = errors.New("duplicate add HTLC detected")

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
	deobfuscator ErrorDecrypter
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

	// CloseBreach indicates that a channel breach has been detected, and
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

	// TargetFeePerKw is the ideal fee that was specified by the caller.
	// This value is only utilized if the closure type is CloseRegular.
	// This will be the starting offered fee when the fee negotiation
	// process for the cooperative closure transaction kicks off.
	TargetFeePerKw btcutil.Amount

	// Updates is used by request creator to receive the notifications about
	// execution of the close channel request.
	Updates chan *lnrpc.CloseStatusUpdate

	// Err is used by request creator to receive request execution error.
	Err chan error
}

// Config defines the configuration for the service. ALL elements within the
// configuration MUST be non-nil for the service to carry out its duties.
type Config struct {
	// SelfKey is the key of the backing Lightning node. This key is used
	// to properly craft failure messages, such that the Layer 3 router can
	// properly route around link./vertex failures.
	SelfKey *btcec.PublicKey

	// LocalChannelClose kicks-off the workflow to execute a cooperative or
	// forced unilateral closure of the channel initiated by a local
	// subsystem.
	LocalChannelClose func(pubKey []byte, request *ChanClose)

	// DB is the channeldb instance that will be used to back the switch's
	// persistent circuit map.
	DB *channeldb.DB
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

	// pendingPayments stores payments initiated by the user that are not yet
	// settled. The map is used to later look up the payments and notify the
	// user of the result when they are complete. Each payment is given a unique
	// integer ID when it is created.
	pendingPayments map[uint64]*pendingPayment
	pendingMutex    sync.RWMutex

	paymentSequencer Sequencer

	// circuits is storage for payment circuits which are used to
	// forward the settle/fail htlc updates back to the add htlc initiator.
	circuits CircuitMap

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

	// resolutionMsgs is the channel that all external contract resolution
	// messages will be sent over.
	resolutionMsgs chan *resolutionMsg

	// linkControl is a channel used to propagate add/remove/get htlc
	// switch handler commands.
	linkControl chan interface{}
}

// New creates the new instance of htlc switch.
func New(cfg Config) (*Switch, error) {
	circuitMap, err := NewCircuitMap(cfg.DB)
	if err != nil {
		return nil, err
	}

	sequencer, err := NewPersistentSequencer(cfg.DB)
	if err != nil {
		return nil, err
	}

	return &Switch{
		cfg:               &cfg,
		circuits:          circuitMap,
		paymentSequencer:  sequencer,
		linkIndex:         make(map[lnwire.ChannelID]ChannelLink),
		forwardingIndex:   make(map[lnwire.ShortChannelID]ChannelLink),
		interfaceIndex:    make(map[[33]byte]map[ChannelLink]struct{}),
		pendingPayments:   make(map[uint64]*pendingPayment),
		htlcPlex:          make(chan *plexPacket),
		chanCloseRequests: make(chan *ChanClose),
		resolutionMsgs:    make(chan *resolutionMsg),
		linkControl:       make(chan interface{}),
		quit:              make(chan struct{}),
	}, nil
}

// resolutionMsg is a struct that wraps an existing ResolutionMsg with a done
// channel. We'll use this channel to synchronize delivery of the message with
// the caller.
type resolutionMsg struct {
	contractcourt.ResolutionMsg

	doneChan chan struct{}
}

// ProcessContractResolution is called by active contract resolvers once a
// contract they are watching over has been fully resolved. The message carries
// an external signal that *would* have been sent if the outgoing channel
// didn't need to go to the chain in order to fulfill a contract. We'll process
// this message just as if it came from an active outgoing channel.
func (s *Switch) ProcessContractResolution(msg contractcourt.ResolutionMsg) error {

	done := make(chan struct{})

	select {
	case s.resolutionMsgs <- &resolutionMsg{
		ResolutionMsg: msg,
		doneChan:      done,
	}:
	case <-s.quit:
		return fmt.Errorf("switch shutting down")
	}

	select {
	case <-done:
	case <-s.quit:
		return fmt.Errorf("switch shutting down")
	}

	return nil
}

// SendHTLC is used by other subsystems which aren't belong to htlc switch
// package in order to send the htlc update.
func (s *Switch) SendHTLC(nextNode [33]byte, htlc *lnwire.UpdateAddHTLC,
	deobfuscator ErrorDecrypter) ([sha256.Size]byte, error) {

	// Create payment and add to the map of payment in order later to be
	// able to retrieve it and return response to the user.
	payment := &pendingPayment{
		err:          make(chan error, 1),
		preimage:     make(chan [sha256.Size]byte, 1),
		paymentHash:  htlc.PaymentHash,
		amount:       htlc.Amount,
		deobfuscator: deobfuscator,
	}

	paymentID, err := s.paymentSequencer.NextID()
	if err != nil {
		return zeroPreimage, err
	}

	s.pendingMutex.Lock()
	s.pendingPayments[paymentID] = payment
	s.pendingMutex.Unlock()

	// Generate and send new update packet, if error will be received on
	// this stage it means that packet haven't left boundaries of our
	// system and something wrong happened.
	packet := &htlcPacket{
		incomingChanID: sourceHop,
		incomingHTLCID: paymentID,
		destNode:       nextNode,
		htlc:           htlc,
	}

	if err := s.forward(packet); err != nil {
		s.removePendingPayment(paymentID)
		return zeroPreimage, err
	}

	// Returns channels so that other subsystem might wait/skip the
	// waiting of handling of payment.
	var preimage [sha256.Size]byte

	select {
	case e := <-payment.err:
		err = e
	case <-s.quit:
		return zeroPreimage, errors.New("htlc switch have been stopped " +
			"while waiting for payment result")
	}

	select {
	case p := <-payment.preimage:
		preimage = p
	case <-s.quit:
		return zeroPreimage, errors.New("htlc switch have been stopped " +
			"while waiting for payment result")
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
	switch htlc := packet.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		circuit := newPaymentCircuit(&htlc.PaymentHash, packet)
		circuitToAdd, err := s.circuits.CommitCircuits(circuit)
		if err != nil {
			log.Errorf("unable to commit circuit in switch: %v", err)
			return err
		}

		// Drop duplicate packet if it has already been seen.
		if len(circuitToAdd) == 0 {
			return ErrDuplicateAdd
		}
	}

	return s.route(packet)
}

// forwardBatch adds a list of packets to the switch for processing. Fails and
// settles are added on a first past, simultaneously constructing circuits for
// any adds. After persisting the circuits, another pass of the adds is given to
// forward them through the router.
// NOTE: This method guarantees that the returned err chan will eventually be
// closed. The receiver should read on the channel until receiving such a
// signal.
func (s *Switch) forwardBatch(packets ...*htlcPacket) chan error {
	var (
		// fwdChan is a buffered channel used to receive err msgs from
		// the htlcPlex when forwarding this batch.
		fwdChan = make(chan error, len(packets))

		// errChan is a buffered channel returned to the caller, that is
		// proxied by the fwdChan. This method guarantees that errChan
		// will be closed eventually to alert the receiver that it can
		// stop reading from the channel.
		errChan = make(chan error, len(packets))

		// numSent keeps a running count of how many packets are
		// forwarded to the switch, which determines how many responses
		// we will wait for on the fwdChan..
		numSent int
	)

	// No packets, nothing to do.
	if len(packets) == 0 {
		close(errChan)
		return errChan
	}

	// Setup a barrier to prevent the background tasks from processing
	// responses until this function returns to the user.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	// Spawn a goroutine the proxy the errs back to the returned err chan.
	// This is done to ensure the err chan returned to the caller closed
	// properly, alerting the receiver of completion or teardown.
	go func() {
		defer func() {
			close(errChan)
		}()

		// Wait here until the outer function has finished persisting
		// and routing the packets.
		wg.Wait()

		for i := 0; i < numSent; i++ {
			select {
			case err := <-fwdChan:
				errChan <- err
			case <-s.quit:
				log.Errorf("unable to forward htlc packet " +
					"htlc switch was stopped")
				return
			}
		}
	}()

	// Make a first pass over the packets, forwarding any settles or fails.
	// As adds are found, we create a circuit and append it to our set of
	// circuits to be written to disk.
	var circuits []*PaymentCircuit
	var addPackets []*htlcPacket
	for _, packet := range packets {
		switch htlc := packet.htlc.(type) {
		case *lnwire.UpdateAddHTLC:
			circuit := newPaymentCircuit(&htlc.PaymentHash, packet)
			circuits = append(circuits, circuit)
			addPackets = append(addPackets, packet)
		default:
			s.routeAsync(packet, fwdChan)
			numSent++
		}
	}

	// Write any circuits that we found to disk.
	circuitsToAdd, err := s.circuits.CommitCircuits(circuits...)
	if err != nil {
		log.Errorf("unable to commit circuits in switch: %v", err)
		return errChan
	}

	// If no new circuits were added as a result of CommitCircuits, exit
	// early since we don't need to perform the following seek.
	if len(circuitsToAdd) == 0 {
		return nil
	}

	// Using the list of circuits that were actually added, do an in-order
	// seek of the add packets provided, forwarding only those that match in
	// incoming chan and htlc ID.
	var cursor int
	for _, circuit := range circuitsToAdd {
	seek:
		for cursor < len(addPackets) {
			packet := addPackets[cursor]
			cursor++

			switch packet.htlc.(type) {
			case *lnwire.UpdateAddHTLC:
				// Skip any packets with incoming circuit keys
				// that do not match the next circuit's incoming
				// key.
				if packet.inKey() != circuit.InKey() {
					continue
				}

				s.routeAsync(packet, fwdChan)
				numSent++

				// This circuit's packet was found, move on to
				// the next circuit.
				break seek
			}
		}
	}

	return errChan
}

// route sends a single htlcPacket through the switch and synchronously awaits a
// response.
func (s *Switch) route(packet *htlcPacket) error {
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

// routeAsync sends a packet through the htlc switch, using the provided err
// chan to propagate errors back to the caller. This method does not wait for
// a response before returning.
func (s *Switch) routeAsync(packet *htlcPacket, errChan chan error) error {
	command := &plexPacket{
		pkt: packet,
		err: errChan,
	}

	select {
	case s.htlcPlex <- command:
		return nil
	case <-s.quit:
		return errors.New("Htlc Switch was stopped")
	}
}

// handleLocalDispatch is used at the start/end of the htlc update life cycle.
// At the start (1) it is used to send the htlc to the channel link without
// creation of circuit. At the end (2) it is used to notify the user about the
// result of his payment is it was successful or not.
//
//   Alice         Bob          Carol
//     o --add----> o ---add----> o
//    (1)
//
//    (2)
//     o <-settle-- o <--settle-- o
//   Alice         Bob         Carol
//
func (s *Switch) handleLocalDispatch(pkt *htlcPacket) error {
	// Pending payments use a special interpretation of the incomingChanID and
	// incomingHTLCID fields on packet where the channel ID is blank and the
	// HTLC ID is the payment ID. The switch basically views the users of the
	// node as a special channel that also offers a sequence of HTLCs.
	payment, err := s.findPayment(pkt.incomingHTLCID)
	if err != nil {
		return err
	}

	switch htlc := pkt.htlc.(type) {

	// User have created the htlc update therefore we should find the
	// appropriate channel link and send the payment over this link.
	case *lnwire.UpdateAddHTLC:
		// Try to find links by node destination.
		links, err := s.getLinks(pkt.destNode)
		if err != nil {
			log.Errorf("unable to find links by destination %v", err)
			return &ForwardingError{
				ErrorSource:    s.cfg.SelfKey,
				FailureMessage: &lnwire.FailUnknownNextPeer{},
			}
		}

		// Try to find destination channel link with appropriate
		// bandwidth.
		var (
			destination      ChannelLink
			largestBandwidth lnwire.MilliSatoshi
		)
		for _, link := range links {
			// We'll skip any links that aren't yet eligible for
			// forwarding.
			if !link.EligibleToForward() {
				continue
			}

			bandwidth := link.Bandwidth()
			if bandwidth > largestBandwidth {

				largestBandwidth = bandwidth
			}

			if bandwidth >= htlc.Amount {
				destination = link
				break
			}
		}

		// If the channel link we're attempting to forward the update
		// over has insufficient capacity, then we'll cancel the HTLC
		// as the payment cannot succeed.
		if destination == nil {
			err := fmt.Errorf("insufficient capacity in available "+
				"outgoing links: need %v, max available is %v",
				htlc.Amount, largestBandwidth)
			log.Error(err)

			htlcErr := lnwire.NewTemporaryChannelFailure(nil)
			return &ForwardingError{
				ErrorSource:    s.cfg.SelfKey,
				ExtraMsg:       err.Error(),
				FailureMessage: htlcErr,
			}
		}

		// Send the packet to the destination channel link which
		// manages then channel.
		//
		// TODO(roasbeef): should return with an error
		pkt.outgoingChanID = destination.ShortChanID()
		return destination.HandleSwitchPacket(pkt)

	// We've just received a settle update which means we can finalize the
	// user payment and return successful response.
	case *lnwire.UpdateFulfillHTLC:
		circuit := s.lookupKeystone(pkt.outKey())
		if circuit == nil {
			// If no circuit is present, this is likely a duplicate
			// message.
			log.Errorf("unknown circuit for keystone=%v", pkt.outKey())
			return ErrUnknownCircuit
		}

		// Remove circuit since we are about to complete an
		// add/fail of this HTLC.
		// TODO(conner): batch teardown by only modifying memory here,
		// and batching at source link.
		if err := s.teardownCircuit(circuit, pkt); err != nil {
			return err
		}

		// Notify the user that his payment was successfully proceed.
		payment.err <- nil
		payment.preimage <- htlc.PaymentPreimage
		s.removePendingPayment(pkt.incomingHTLCID)

	// We've just received a fail update which means we can finalize the
	// user payment and return fail response.
	case *lnwire.UpdateFailHTLC:
		var (
			failure *ForwardingError
			circuit *PaymentCircuit
		)

		switch {

		// The payment never cleared the link, so we don't need to
		// decrypt the error, simply decode it them report back to the
		// user.
		case pkt.localFailure:
			var userErr string
			r := bytes.NewReader(htlc.Reason)
			failureMsg, err := lnwire.DecodeFailure(r, 0)
			if err != nil {
				userErr = fmt.Sprintf("unable to decode onion failure, "+
					"htlc with hash(%x): %v", payment.paymentHash[:], err)
				log.Error(userErr)
				failureMsg = lnwire.NewTemporaryChannelFailure(nil)
			}
			failure = &ForwardingError{
				ErrorSource:    s.cfg.SelfKey,
				ExtraMsg:       userErr,
				FailureMessage: failureMsg,
			}

			circuit = s.lookupCircuit(pkt.inKey())

		// A payment had to be timed out on chain before it got past
		// the first hop. In this case, we'll report a permanent
		// channel failure as this means us, or the remote party had to
		// go on chain.
		case pkt.isResolution && htlc.Reason == nil:
			userErr := fmt.Sprintf("payment was resolved " +
				"on-chain, then cancelled back")
			failure = &ForwardingError{
				ErrorSource:    s.cfg.SelfKey,
				ExtraMsg:       userErr,
				FailureMessage: lnwire.FailPermanentChannelFailure{},
			}

			circuit = s.lookupCircuit(pkt.inKey())

		// A regular multi-hop payment error that we'll need to
		// decrypt.
		default:
			// We'll attempt to fully decrypt the onion encrypted
			// error. If we're unable to then we'll bail early.
			failure, err = payment.deobfuscator.DecryptError(htlc.Reason)
			if err != nil {
				userErr := fmt.Sprintf("unable to de-obfuscate onion failure, "+
					"htlc with hash(%x): %v", payment.paymentHash[:], err)
				log.Error(userErr)
				failure = &ForwardingError{
					ErrorSource:    s.cfg.SelfKey,
					ExtraMsg:       userErr,
					FailureMessage: lnwire.NewTemporaryChannelFailure(nil),
				}
			}

			circuit = s.lookupKeystone(pkt.outKey())
		}

		// Circuit was not found, may have already been handled.
		if circuit == nil {
			return ErrUnknownCircuit
		}

		// TODO(conner): batch teardown by only modifying memory here,
		// and batching at source link.
		if err := s.teardownCircuit(circuit, pkt); err != nil {
			return err
		}

		payment.err <- failure
		payment.preimage <- zeroPreimage
		s.removePendingPayment(pkt.incomingHTLCID)

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
		if packet.incomingChanID == sourceHop {
			// A blank incomingChanID indicates that this is
			// a pending user-initiated payment.
			return s.handleLocalDispatch(packet)
		}

		source, err := s.getLinkByShortID(packet.incomingChanID)
		if err != nil {
			err := errors.Errorf("unable to find channel link "+
				"by channel point (%v): %v", packet.incomingChanID, err)
			log.Error(err)
			return err
		}

		targetLink, err := s.getLinkByShortID(packet.outgoingChanID)
		if err != nil {
			// If packet was forwarded from another channel link
			// than we should notify this link that some error
			// occurred.
			failure := &lnwire.FailUnknownNextPeer{}
			addErr := errors.Errorf("unable to find link with "+
				"destination %v", packet.outgoingChanID)

			return s.failAddPacket(source, packet, failure, addErr)
		}
		interfaceLinks, _ := s.getLinks(targetLink.Peer().PubKey())

		// Try to find destination channel link with appropriate
		// bandwidth.
		var destination ChannelLink
		for _, link := range interfaceLinks {
			// We'll skip any links that aren't yet eligible for
			// forwarding.
			if !link.EligibleToForward() {
				continue
			}

			if link.Bandwidth() >= htlc.Amount {
				destination = link
				break
			}
		}

		// If the channel link we're attempting to forward the update
		// over has insufficient capacity, then we'll cancel the htlc
		// as the payment cannot succeed.
		if destination == nil {
			// If packet was forwarded from another channel link
			// than we should notify this link that some error
			// occurred.
			failure := lnwire.NewTemporaryChannelFailure(nil)
			addErr := errors.Errorf("unable to find appropriate "+
				"channel link insufficient capacity, need "+
				"%v", htlc.Amount)

			return s.failAddPacket(source, packet, failure, addErr)
		}

		// Send the packet to the destination channel link which
		// manages the channel.
		packet.outgoingChanID = destination.ShortChanID()
		return destination.HandleSwitchPacket(packet)

	// We've just received a settle packet which means we can finalize the
	// payment circuit by forwarding the settle msg to the channel from
	// which htlc add packet was initially received.
	case *lnwire.UpdateFulfillHTLC:
		var (
			circuit *PaymentCircuit
			err     error
		)

		// If the source of this packet has not been set, use the
		// circuit map to lookup the origin.
		if !packet.hasSource {
			circuit, err = s.closeCircuit(htlc, packet)
			if err != nil {
				return err
			}
		}

		// A blank IncomingChanID in a circuit indicates that it is a pending
		// user-initiated payment.
		if packet.incomingChanID == sourceHop {
			return s.handleLocalDispatch(packet)
		}

		source, err := s.getLinkByShortID(packet.incomingChanID)
		if err != nil {
			err := errors.Errorf("Unable to get source channel "+
				"link to forward HTLC settle/fail: %v", err)
			log.Error(err)
			return err
		}

		// TODO(conner): batch teardown by only modifying memory here,
		// and batching at source link.
		if err := s.teardownCircuit(circuit, packet); err != nil {
			return err
		}

		return source.HandleSwitchPacket(packet)

	case *lnwire.UpdateFailHTLC:
		var (
			circuit *PaymentCircuit
			err     error
		)

		// If the source of this packet has not been set, use the
		// circuit map to lookup the origin.
		if !packet.hasSource {
			circuit, err = s.closeCircuit(htlc, packet)
			if err != nil {
				return err
			}
		}

		// A blank IncomingChanID in a circuit indicates that it is a pending
		// user-initiated payment.
		if packet.incomingChanID == sourceHop {
			return s.handleLocalDispatch(packet)
		}

		// Check to see that the source link is online before removing
		// the circuit.
		source, err := s.getLinkByShortID(packet.incomingChanID)
		if err != nil {
			err = errors.Errorf("Unable to get source channel "+
				"link to forward HTLC settle/fail: %v", err)
			log.Error(err)
			return err
		}

		// TODO(conner): batch teardown by only modifying memory here,
		// and batching at source link.
		if err := s.teardownCircuit(circuit, packet); err != nil {
			return err
		}

		return source.HandleSwitchPacket(packet)

	default:
		return errors.New("wrong update type")
	}
}

// failAddPacket encrypts a fail packet back to an add packet's source.
// The ciphertext will be derived from the failure message proivded by context.
// This method returns the failErr if all other steps complete successfully.
func (s *Switch) failAddPacket(source ChannelLink,
	packet *htlcPacket, failure lnwire.FailureMessage,
	failErr error) error {

	// Encrypt the failure so that the sender will be able to read the error
	// message. Since we failed this packet, we use EncryptFirstHop to
	// obfuscate the failure for their eyes only.
	reason, err := packet.obfuscator.EncryptFirstHop(failure)
	if err != nil {
		err := errors.Errorf("unable to obfuscate "+
			"error: %v", err)
		log.Error(err)
		return err
	}

	log.Error(failErr)

	// Route a fail packet back to the source link.
	if err = source.HandleSwitchPacket(&htlcPacket{
		incomingChanID: packet.incomingChanID,
		incomingHTLCID: packet.incomingHTLCID,
		hasSource:      true,
		localFailure:   true,
		htlc: &lnwire.UpdateFailHTLC{
			Reason: reason,
		},
	}); err != nil {
		err = errors.Errorf("source chanid=%v unable to "+
			"handle switch packet: %v",
			packet.incomingChanID, err)
		log.Error(err)
		return err
	}

	return failErr
}

// closeCircuit accepts a settle or fail htlc and the associated htlc packet and
// attempts to determine the source that forwarded this htlc. This method will
// set the incoming chan and htlc ID of the given packet if the source was
// found, and will properly [re]encrypt any failure messages.
func (s *Switch) closeCircuit(htlc lnwire.Message, pkt *htlcPacket) (*PaymentCircuit, error) {
	// Use circuit map to find the link to forward settle/fail to.
	circuit := s.lookupKeystone(pkt.outKey())
	if circuit == nil {
		err := errors.Errorf("Unable to find target channel for HTLC "+
			"settle/fail: channel ID = %s, HTLC ID = %d",
			pkt.outgoingChanID, pkt.outgoingHTLCID)
		log.Error(err)
		return nil, err
	}

	pkt.incomingChanID = circuit.Incoming.ChanID
	pkt.incomingHTLCID = circuit.Incoming.HtlcID

	// If the error encrypter is nil, this is a local payment so we don't
	// need to reencrypt the error message.
	if circuit.ErrorEncrypter == nil {
		return circuit, nil
	}

	// Otherwise, obfuscate the error message any fail updates before
	// sending back through the circuit.
	if fail, ok := htlc.(*lnwire.UpdateFailHTLC); ok {
		// If this is a resolution message, then we'll need to encrypt
		// it as it's actually internally sourced.
		if pkt.isResolution {
			var err error
			// TODO(roasbeef): don't need to pass actually?
			failure := &lnwire.FailPermanentChannelFailure{}
			fail.Reason, err = circuit.ErrorEncrypter.EncryptFirstHop(
				failure,
			)
			if err != nil {
				err = errors.Errorf("unable to obfuscate "+
					"error: %v", err)
				log.Error(err)
			}
		} else {
			// Otherwise, it's a forwarded error, so we'll perform a
			// wrapper encryption as normal.
			fail.Reason = circuit.ErrorEncrypter.IntermediateEncrypt(
				fail.Reason,
			)
		}
	}

	return circuit, nil
}

// teardownCircuit removes a pending or open circuit from the switch's circuit
// map and prints useful logging statements regarding the outcome.
func (s *Switch) teardownCircuit(circuit *PaymentCircuit, packet *htlcPacket) error {
	var pktType string
	switch htlc := packet.htlc.(type) {
	case *lnwire.UpdateFulfillHTLC:
		pktType = "SETTLE"
	case *lnwire.UpdateFailHTLC:
		pktType = "FAIL"
	default:
		err := fmt.Errorf("cannot tear down packet of type: %T", htlc)
		log.Errorf(err.Error())
		return err
	}

	switch {
	case circuit.HasKeystone():
		log.Debugf("Tearing down open circuit with %s pkt, removing circuit=%v "+
			"with keystone=%v", pktType, packet.inKey(), packet.outKey())

		err := s.deleteCircuit(packet.inKey())
		if err != nil {
			log.Warnf("Failed to tear down open circuit (%s, %d) <-> (%s, %d) "+
				"with payment_hash-%v using %s pkt",
				packet.incomingChanID, packet.incomingHTLCID,
				packet.outgoingChanID, packet.outgoingHTLCID,
				circuit.PaymentHash, pktType)
			return err
		}

		log.Debugf("Closed completed %s circuit for %x: "+
			"(%s, %d) <-> (%s, %d)", pktType, circuit.PaymentHash,
			packet.incomingChanID, packet.incomingHTLCID,
			packet.outgoingChanID, packet.outgoingHTLCID)

	default:
		log.Debugf("Tearing down pending circuit with %s for inkey=%v",
			pktType, packet.inKey())
		err := s.deleteCircuit(packet.inKey())
		if err != nil {
			log.Warnf("Failed to tear down pending %s circuit for %x: "+
				"(%s, %d)", pktType, circuit.PaymentHash,
				packet.incomingChanID, packet.incomingHTLCID)
			return err
		}

		log.Debugf("Removed pending onion circuit for %x: "+
			"(%s, %d)", circuit.PaymentHash,
			packet.incomingChanID, packet.incomingHTLCID)
	}

	return nil
}

// CloseLink creates and sends the close channel command to the target link
// directing the specified closure type. If the closure type if CloseRegular,
// then the last parameter should be the ideal fee-per-kw that will be used as
// a starting point for close negotiation.
func (s *Switch) CloseLink(chanPoint *wire.OutPoint,
	closeType ChannelCloseType,
	targetFeePerKw btcutil.Amount) (chan *lnrpc.CloseStatusUpdate, chan error) {

	// TODO(roasbeef) abstract out the close updates.
	updateChan := make(chan *lnrpc.CloseStatusUpdate, 2)
	errChan := make(chan error, 1)

	command := &ChanClose{
		CloseType:      closeType,
		ChanPoint:      chanPoint,
		Updates:        updateChan,
		TargetFeePerKw: targetFeePerKw,
		Err:            errChan,
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

		case resolutionMsg := <-s.resolutionMsgs:
			pkt := &htlcPacket{
				outgoingChanID: resolutionMsg.SourceChan,
				outgoingHTLCID: resolutionMsg.HtlcIndex,
				isResolution:   true,
			}

			// Resolution messages will either be cancelling
			// backwards an existing HTLC, or settling a previously
			// outgoing HTLC. Based on this, we'll map the message
			// to the proper htlcPacket.
			if resolutionMsg.Failure != nil {
				pkt.htlc = &lnwire.UpdateFailHTLC{}
			} else {
				pkt.htlc = &lnwire.UpdateFulfillHTLC{
					PaymentPreimage: *resolutionMsg.PreImage,
				}
			}

			log.Infof("Received outside contract resolution, "+
				"mapping to: %v", spew.Sdump(pkt))

			// We don't check the error, as the only failure we can
			// encounter is due to the circuit already being
			// closed. This is fine, as processing this message is
			// meant to be idempotent.
			err := s.handlePacketForward(pkt)
			if err != nil {
				log.Errorf("Unable to forward resolution msg: %v", err)
			}

			// With the message processed, we'll now close out
			close(resolutionMsg.doneChan)

		// A new packet has arrived for forwarding, we'll interpret the
		// packet concretely, then either forward it along, or
		// interpret a return packet to a locally initialized one.
		case cmd := <-s.htlcPlex:
			cmd.err <- s.handlePacketForward(cmd.pkt)

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
			case *updateForwardingIndexCmd:
				cmd.err <- s.updateShortChanID(
					cmd.chanID, cmd.shortChanID,
				)
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
		return errors.New("htlc switch already started")
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
		return errors.New("htlc switch already shutdown")
	}

	log.Infof("HTLC Switch shutting down")

	close(s.quit)
	s.wg.Wait()

	s.cfg.DB.Close()

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
		select {
		case err := <-command.err:
			return err
		case <-s.quit:
		}
	case <-s.quit:
	}

	return errors.New("unable to add link htlc switch was stopped")
}

// addLink is used to add the newly created channel link and start use it to
// handle the channel updates.
func (s *Switch) addLink(link ChannelLink) error {
	// TODO(roasbeef): reject if link already tehre?

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
		s.removeLink(link.ChanID())
		return err
	}

	log.Infof("Added channel link with chan_id=%v, short_chan_id=(%v)",
		link.ChanID(), spew.Sdump(link.ShortChanID()))

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

query:
	select {
	case s.linkControl <- command:

		var link ChannelLink
		select {
		case link = <-command.done:
		case <-s.quit:
			break query
		}

		select {
		case err := <-command.err:
			return link, err
		case <-s.quit:
		}
	case <-s.quit:
	}

	return nil, errors.New("unable to get link htlc switch was stopped")
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
		select {
		case err := <-command.err:
			return err
		case <-s.quit:
		}
	case <-s.quit:
	}

	return errors.New("unable to remove link htlc switch was stopped")
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

	link.Stop()

	return nil
}

// updateForwardingIndexCmd is a command sent by outside sub-systems to update
// the forwarding index of the switch in the event that the short channel ID of
// a particular link changes.
type updateForwardingIndexCmd struct {
	chanID      lnwire.ChannelID
	shortChanID lnwire.ShortChannelID

	err chan error
}

// UpdateShortChanID updates the short chan ID for an existing channel. This is
// required in the case of a re-org and re-confirmation or a channel, or in the
// case that a link was added to the switch before its short chan ID was known.
func (s *Switch) UpdateShortChanID(chanID lnwire.ChannelID,
	shortChanID lnwire.ShortChannelID) error {

	command := &updateForwardingIndexCmd{
		chanID:      chanID,
		shortChanID: shortChanID,
		err:         make(chan error, 1),
	}

	select {
	case s.linkControl <- command:
		select {
		case err := <-command.err:
			return err
		case <-s.quit:
		}
	case <-s.quit:
	}

	return errors.New("unable to update short chan id htlc switch was stopped")
}

// updateShortChanID updates the short chan ID of an existing link.
func (s *Switch) updateShortChanID(chanID lnwire.ChannelID,
	shortChanID lnwire.ShortChannelID) error {

	// First, we'll extract the current link as is from the link link
	// index. If the link isn't even in the index, then we'll return an
	// error.
	link, ok := s.linkIndex[chanID]
	if !ok {
		return fmt.Errorf("link %v not found", chanID)
	}

	log.Infof("Updating short_chan_id for ChannelLink(%v): old=%v, new=%v",
		chanID, link.ShortChanID(), shortChanID)

	// At this point the link is actually active, so we'll update the
	// forwarding index with the next short channel ID.
	s.forwardingIndex[shortChanID] = link

	// Finally, we'll notify the link of its new short channel ID.
	link.UpdateShortChanID(shortChanID)

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

query:
	select {
	case s.linkControl <- command:

		var links []ChannelLink
		select {
		case links = <-command.done:
		case <-s.quit:
			break query
		}

		select {
		case err := <-command.err:
			return links, err
		case <-s.quit:
		}
	case <-s.quit:
	}

	return nil, errors.New("unable to get links htlc switch was stopped")
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
func (s *Switch) removePendingPayment(paymentID uint64) error {
	s.pendingMutex.Lock()
	defer s.pendingMutex.Unlock()

	if _, ok := s.pendingPayments[paymentID]; !ok {
		return errors.Errorf("Cannot find pending payment with ID %d",
			paymentID)
	}

	delete(s.pendingPayments, paymentID)
	return nil
}

// findPayment is the helper function which find the payment.
func (s *Switch) findPayment(paymentID uint64) (*pendingPayment, error) {
	s.pendingMutex.RLock()
	defer s.pendingMutex.RUnlock()

	payment, ok := s.pendingPayments[paymentID]
	if !ok {
		return nil, errors.Errorf("Cannot find pending payment with ID %d",
			paymentID)
	}
	return payment, nil
}

// numPendingPayments is helper function which returns the overall number of
// pending user payments.
func (s *Switch) numPendingPayments() int {
	return len(s.pendingPayments)
}

// addCircuit persistently adds a circuit to the switch's circuit map.
func (s *Switch) commitCircuits(circuit ...*PaymentCircuit) ([]*PaymentCircuit, error) {
	return s.circuits.CommitCircuits(circuit...)
}

// addHalfCircuit persistently adds a half-circuit to the switch's circuit map.
func (s *Switch) setKeystone(inKey, outKey CircuitKey) error {
	return s.circuits.SetKeystone(inKey, outKey)
}

// deleteCircuit persistently removes the circuit, and keystone if present,
// from the circuit map.
func (s *Switch) deleteCircuit(inKey CircuitKey) error {
	return s.circuits.Delete(inKey)
}

// lookupCircuit queries the in memory representation of the circuit map to
// retrieve a particular circuit.
func (s *Switch) lookupCircuit(inKey CircuitKey) *PaymentCircuit {
	return s.circuits.LookupCircuit(inKey)
}

// lookupKeystone queries the in-memory representation of the circuit map for a
// circuit whose outgoing circuit key matches outKey.
func (s *Switch) lookupKeystone(outKey CircuitKey) *PaymentCircuit {
	return s.circuits.LookupByKeystone(outKey)
}
