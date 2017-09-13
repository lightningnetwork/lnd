package htlcswitch

import (
	"bytes"
	"sync"
	"sync/atomic"
	"time"

	"io"

	"crypto/sha256"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (
	// expiryGraceDelta is a grace period that the timeout of incoming
	// HTLC's that pay directly to us (i.e we're the "exit node") must up
	// hold. We'll reject any HTLC's who's timeout minus this value is less
	// that or equal to the current block height. We require this in order
	// to ensure that if the extending party goes to the chain, then we'll
	// be able to claim the HTLC still.
	//
	// TODO(roasbeef): must be < default delta
	expiryGraceDelta = 2
)

// ForwardingPolicy describes the set of constraints that a given ChannelLink
// is to adhere to when forwarding HTLC's. For each incoming HTLC, this set of
// constraints will be consulted in order to ensure that adequate fees are
// paid, and our time-lock parameters are respected. In the event that an
// incoming HTLC violates any of these constraints, it is to be _rejected_ with
// the error possibly carrying along a ChannelUpdate message that includes the
// latest policy.
type ForwardingPolicy struct {
	// MinHTLC is the smallest HTLC that is to be forwarded.
	MinHTLC lnwire.MilliSatoshi

	// BaseFee is the base fee, expressed in milli-satoshi that must be
	// paid for each incoming HTLC. This field, combined with FeeRate is
	// used to compute the required fee for a given HTLC.
	BaseFee lnwire.MilliSatoshi

	// FeeRate is the fee rate, expressed in milli-satoshi that must be
	// paid for each incoming HTLC. This field combined with BaseFee is
	// used to compute the required fee for a given HTLC.
	FeeRate lnwire.MilliSatoshi

	// TimeLockDelta is the absolute time-lock value, expressed in blocks,
	// that will be subtracted from an incoming HTLC's timelock value to
	// create the time-lock value for the forwarded outgoing HTLC. The
	// following constraint MUST hold for an HTLC to be forwarded:
	//
	//  * incomingHtlc.timeLock - timeLockDelta = fwdInfo.OutgoingCTLV
	//
	//    where fwdInfo is the forwarding information extracted from the
	//    per-hop payload of the incoming HTLC's onion packet.
	TimeLockDelta uint32

	// TODO(roasbeef): add fee module inside of switch
}

// ExpectedFee computes the expected fee for a given htlc amount. The value
// returned from this function is to be used as a sanity check when forwarding
// HTLC's to ensure that an incoming HTLC properly adheres to our propagated
// forwarding policy.
//
// TODO(roasbeef): also add in current available channel bandwidth, inverse
// func
func ExpectedFee(f ForwardingPolicy, htlcAmt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	// TODO(roasbeef): write some basic table driven tests
	return f.BaseFee + (htlcAmt*f.FeeRate)/1000000
}

// ChannelLinkConfig defines the configuration for the channel link. ALL
// elements within the configuration MUST be non-nil for channel link to carry
// out its duties.
type ChannelLinkConfig struct {
	// FwrdingPolicy is the initial forwarding policy to be used when
	// deciding whether to forwarding incoming HTLC's or not. This value
	// can be updated with subsequent calls to UpdateForwardingPolicy
	// targeted at a given ChannelLink concrete interface implementation.
	FwrdingPolicy ForwardingPolicy

	// Switch is a subsystem which is used to forward the incoming HTLC
	// packets according to the encoded hop forwarding information
	// contained in the forwarding blob within each HTLC.
	Switch *Switch

	// DecodeHopIterator function is responsible for decoding HTLC Sphinx
	// onion blob, and creating hop iterator which will give us next
	// destination of HTLC.
	DecodeHopIterator func(r io.Reader, rHash []byte) (HopIterator, lnwire.FailCode)

	// DecodeOnionObfuscator function is responsible for decoding HTLC
	// Sphinx onion blob, and creating onion failure obfuscator.
	DecodeOnionObfuscator func(r io.Reader) (Obfuscator, lnwire.FailCode)

	// GetLastChannelUpdate retrieves the latest routing policy for this
	// particular channel. This will be used to provide payment senders our
	// latest policy when sending encrypted error messages.
	GetLastChannelUpdate func() (*lnwire.ChannelUpdate, error)

	// Peer is a lightning network node with which we have the channel link
	// opened.
	Peer Peer

	// Registry is a sub-system which responsible for managing the invoices
	// in thread-safe manner.
	Registry InvoiceDatabase

	// BlockEpochs is an active block epoch event stream backed by an
	// active ChainNotifier instance. The ChannelLink will use new block
	// notifications sent over this channel to decide when a _new_ HTLC is
	// too close to expiry, and also when any active HTLC's have expired
	// (or are close to expiry).
	BlockEpochs *chainntnfs.BlockEpochEvent

	// SettledContracts is used to notify that a channel has peacefully
	// been closed. Once a channel has been closed the other subsystem no
	// longer needs to watch for breach closes.
	SettledContracts chan *wire.OutPoint

	// DebugHTLC should be turned on if you want all HTLCs sent to a node
	// with the debug htlc R-Hash are immediately settled in the next
	// available state transition.
	DebugHTLC bool
}

// channelLink is the service which drives a channel's commitment update
// state-machine. In the event that an htlc needs to be propagated to another
// link, the forward handler from config is used which sends htlc to the
// switch. Additionally, the link encapsulate logic of commitment protocol
// message ordering and updates.
type channelLink struct {
	// The following fields are only meant to be used *atomically*
	started  int32
	shutdown int32

	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	//
	// TODO(andrew.shvv) remove after payment descriptor start store
	// htlc cancel reasons.
	cancelReasons map[uint64]lnwire.OpaqueReason

	// clearedOnionBlobs tracks the remote log index of the incoming
	// htlc's, mapped to the htlc onion blob which encapsulates the next
	// hop. HTLC's are added to this map once the HTLC has been cleared,
	// meaning the commitment state reflects the update encoded within this
	// HTLC.
	//
	// TODO(andrew.shvv) remove after payment descriptor start store
	// htlc onion blobs.
	clearedOnionBlobs map[uint64][lnwire.OnionPacketSize]byte

	// batchCounter is the number of updates which we received from remote
	// side, but not include in commitment transaction yet and plus the
	// current number of settles that have been sent, but not yet committed
	// to the commitment.
	//
	// TODO(andrew.shvv) remove after we add additional
	// BatchNumber() method in state machine.
	batchCounter uint32

	// bestHeight is the best known height of the main chain. The link will
	// use this information to govern decisions based on HTLC timeouts.
	bestHeight uint32

	// channel is a lightning network channel to which we apply htlc
	// updates.
	channel *lnwallet.LightningChannel

	// cfg is a structure which carries all dependable fields/handlers
	// which may affect behaviour of the service.
	cfg ChannelLinkConfig

	// overflowQueue is used to store the htlc add updates which haven't
	// been processed because of the commitment transaction overflow.
	overflowQueue *packetQueue

	// upstream is a channel that new messages sent from the remote peer to
	// the local peer will be sent across.
	upstream chan lnwire.Message

	// downstream is a channel in which new multi-hop HTLC's to be
	// forwarded will be sent across. Messages from this channel are sent
	// by the HTLC switch.
	downstream chan *htlcPacket

	// linkControl is a channel which is used to query the state of the
	// link, or update various policies used which govern if an HTLC is to
	// be forwarded and/or accepted.
	linkControl chan interface{}

	// logCommitTimer is a timer which is sent upon if we go an interval
	// without receiving/sending a commitment update. It's role is to
	// ensure both chains converge to identical state in a timely manner.
	//
	// TODO(roasbeef): timer should be >> then RTT
	logCommitTimer *time.Timer
	logCommitTick  <-chan time.Time

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewChannelLink creates a new instance of a ChannelLink given a configuration
// and active channel that will be used to verify/apply updates to.
func NewChannelLink(cfg ChannelLinkConfig, channel *lnwallet.LightningChannel,
	currentHeight uint32) ChannelLink {

	return &channelLink{
		cfg:               cfg,
		channel:           channel,
		clearedOnionBlobs: make(map[uint64][lnwire.OnionPacketSize]byte),
		upstream:          make(chan lnwire.Message),
		downstream:        make(chan *htlcPacket),
		linkControl:       make(chan interface{}),
		cancelReasons:     make(map[uint64]lnwire.OpaqueReason),
		logCommitTimer:    time.NewTimer(300 * time.Millisecond),
		overflowQueue:     newWaitingQueue(),
		bestHeight:        currentHeight,
		quit:              make(chan struct{}),
	}
}

// A compile time check to ensure channelLink implements the ChannelLink
// interface.
var _ ChannelLink = (*channelLink)(nil)

// Start starts all helper goroutines required for the operation of the channel
// link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Start() error {
	if !atomic.CompareAndSwapInt32(&l.started, 0, 1) {
		log.Warnf("channel link(%v): already started", l)
		return nil
	}

	log.Infof("ChannelLink(%v) is starting", l)

	l.wg.Add(1)
	go l.htlcManager()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stop() {
	if !atomic.CompareAndSwapInt32(&l.shutdown, 0, 1) {
		log.Warnf("channel link(%v): already stopped", l)
		return
	}

	log.Infof("ChannelLink(%v) is stopping", l)

	close(l.quit)
	l.wg.Wait()

	l.cfg.BlockEpochs.Cancel()
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// This goroutine reads messages from the upstream (remote) peer, and also from
// downstream channel managed by the channel link. In the event that an htlc
// needs to be forwarded, then send-only forward handler is used which sends
// htlc packets to the switch. Additionally, the this goroutine handles acting
// upon all timeouts for any active HTLCs, manages the channel's revocation
// window, and also the htlc trickle queue+timer for this active channels.
//
// NOTE: This MUST be run as a goroutine.
func (l *channelLink) htlcManager() {
	defer l.wg.Done()

	log.Infof("HTLC manager for ChannelPoint(%v) started, "+
		"bandwidth=%v", l.channel.ChannelPoint(), l.getBandwidth())

	// TODO(roasbeef): check to see if able to settle any currently pending
	// HTLCs
	//   * also need signals when new invoices are added by the
	//   invoiceRegistry

	batchTimer := time.NewTicker(50 * time.Millisecond)
	defer batchTimer.Stop()

	// TODO(roasbeef): fail chan in case of protocol violation

	// TODO(roasbeef): resend funding locked if state zero

out:
	for {
		select {
		// A new block has arrived, we'll examine all the active HTLC's
		// to see if any of them have expired, and also update our
		// track of the best current height.
		case blockEpoch, ok := <-l.cfg.BlockEpochs.Epochs:
			if !ok {
				break out
			}

			log.Debugf("ChannelPoint(%v): new block(height=%v, "+
				"hash=%v) examining active HTLC's",
				l.channel.ChannelPoint(), blockEpoch.Height,
				blockEpoch.Hash)

			// TODO(roasbeef): check HTLC's for expiry
			l.bestHeight = uint32(blockEpoch.Height)

		// The underlying channel has notified us of a unilateral close
		// carried out by the remote peer. In the case of such an
		// event, we'll wipe the channel state from the peer, and mark
		// the contract as fully settled. Afterwards we can exit.
		case <-l.channel.UnilateralCloseSignal:
			log.Warnf("Remote peer has closed ChannelPoint(%v) on-chain",
				l.channel.ChannelPoint())
			if err := l.cfg.Peer.WipeChannel(l.channel); err != nil {
				log.Errorf("unable to wipe channel %v", err)
			}

			// TODO(roasbeef): need to send HTLC outputs to nursery

			// TODO(roasbeef): or let the arb sweep?
			l.cfg.SettledContracts <- l.channel.ChannelPoint()
			break out

		// A local sub-system has initiated a force close of the active
		// channel. In this case we can exit immediately as no further
		// updates should be processed for the channel.
		case <-l.channel.ForceCloseSignal:
			// TODO(roasbeef): path never taken now that server
			// force closes's directly?
			log.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peer(%x)",
				l.channel.ChannelPoint(), l.cfg.Peer.PubKey())
			break out

		case <-l.logCommitTick:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit due to our
			// commitment chains being desynchronized.
			if l.channel.FullySynced() {
				continue
			}

			if err := l.updateCommitTx(); err != nil {
				l.fail("unable to update commitment: %v", err)
				break out
			}

		case <-batchTimer.C:
			// If the current batch is empty, then we have no work
			// here.
			if l.batchCounter == 0 {
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if err := l.updateCommitTx(); err != nil {
				l.fail("unable to update commitment: %v", err)
				break out
			}

		// A packet that previously overflowed the commitment
		// transaction is now eligible for processing once again. So
		// we'll attempt to re-process the packet in order to allow it
		// to continue propagating within the network.
		case packet := <-l.overflowQueue.pending:
			msg := packet.htlc.(*lnwire.UpdateAddHTLC)
			log.Tracef("Reprocessing downstream add update "+
				"with payment hash(%x)", msg.PaymentHash[:])

			l.handleDownStreamPkt(packet)

		// A message from the switch was just received. This indicates
		// that the link is an intermediate hop in a multi-hop HTLC
		// circuit.
		case pkt := <-l.downstream:
			// If we have non empty processing queue then we'll add
			// this to the overflow rather than processing it
			// directly. Once an active HTLC is either settled or
			// failed, then we'll free up a new slot.
			htlc, ok := pkt.htlc.(*lnwire.UpdateAddHTLC)
			if ok && l.overflowQueue.length() != 0 {
				log.Infof("Downstream htlc add update with "+
					"payment hash(%x) have been added to "+
					"reprocessing queue, batch: %v",
					htlc.PaymentHash[:],
					l.batchCounter)

				l.overflowQueue.consume(pkt)
				continue
			}
			l.handleDownStreamPkt(pkt)

		// A message from the connected peer was just received. This
		// indicates that we have a new incoming HTLC, either directly
		// for us, or part of a multi-hop HTLC circuit.
		case msg := <-l.upstream:
			l.handleUpstreamMsg(msg)

		case cmd := <-l.linkControl:
			switch req := cmd.(type) {
			case *getBandwidthCmd:
				req.resp <- l.getBandwidth()
			case *policyUpdate:
				// In order to avoid overriding a valid policy
				// with a "null" field in the new policy, we'll
				// only update to the set sub policy if the new
				// value isn't uninitialized.
				if req.policy.MinHTLC != 0 {
					l.cfg.FwrdingPolicy.MinHTLC = req.policy.MinHTLC
				}
				if req.policy.BaseFee != 0 {
					l.cfg.FwrdingPolicy.BaseFee = req.policy.BaseFee
				}
				if req.policy.FeeRate != 0 {
					l.cfg.FwrdingPolicy.FeeRate = req.policy.FeeRate
				}
				if req.policy.TimeLockDelta != 0 {
					l.cfg.FwrdingPolicy.TimeLockDelta = req.policy.TimeLockDelta
				}

				if req.done != nil {
					close(req.done)
				}
			}

		case <-l.quit:
			break out
		}
	}

	log.Infof("ChannelLink(%v) has exited", l)
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
func (l *channelLink) handleDownStreamPkt(pkt *htlcPacket) {
	var isSettle bool
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		// A new payment has been initiated via the downstream channel,
		// so we add the new HTLC to our local log, then update the
		// commitment chains.
		htlc.ChanID = l.ChanID()
		index, err := l.channel.AddHTLC(htlc)
		if err != nil {
			switch err {

			// The channels spare bandwidth is fully allocated, so
			// we'll put this HTLC into the overflow queue.
			case lnwallet.ErrMaxHTLCNumber:
				log.Infof("Downstream htlc add update with "+
					"payment hash(%x) have been added to "+
					"reprocessing queue, batch: %v",
					htlc.PaymentHash[:],
					l.batchCounter)
				l.overflowQueue.consume(pkt)
				return

			// The HTLC was unable to be added to the state
			// machine, as a result, we'll signal the switch to
			// cancel the pending payment.
			default:
				var (
					isObfuscated bool
					reason       lnwire.OpaqueReason
				)

				// We'll parse the sphinx packet enclosed so we
				// can obtain the shared secret required to
				// encrypt the error back to the source.
				failure := lnwire.NewTemporaryChannelFailure(nil)
				onionReader := bytes.NewReader(htlc.OnionBlob[:])
				obfuscator, failCode := l.cfg.DecodeOnionObfuscator(onionReader)

				switch {
				// If we were unable to parse the onion blob,
				// then we'll send an error back to the source.
				case failCode != lnwire.CodeNone:
					var b bytes.Buffer
					err := lnwire.EncodeFailure(&b, failure, 0)
					if err != nil {
						log.Errorf("unable to encode failure: %v", err)
						return
					}
					reason = lnwire.OpaqueReason(b.Bytes())
					isObfuscated = false

				// Otherwise, we'll send back a proper failure
				// message.
				default:
					reason, err = obfuscator.InitialObfuscate(failure)
					if err != nil {
						log.Errorf("unable to obfuscate error: %v", err)
						return
					}
					isObfuscated = true
				}

				upddateFail := &lnwire.UpdateFailHTLC{
					Reason: reason,
				}
				failPkt := newFailPacket(
					l.ShortChanID(), upddateFail,
					htlc.PaymentHash, htlc.Amount,
					isObfuscated,
				)

				go l.cfg.Switch.forward(failPkt)
				log.Infof("Unable to handle downstream add HTLC: %v", err)
				return
			}
		}

		log.Tracef("Received downstream htlc: payment_hash=%x, "+
			"local_log_index=%v, batch_size=%v",
			htlc.PaymentHash[:], index, l.batchCounter+1)

		htlc.ID = index
		l.cfg.Peer.SendMessage(htlc)

	case *lnwire.UpdateFufillHTLC:
		// An HTLC we forward to the switch has just settled somewhere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		pre := htlc.PaymentPreimage
		logIndex, err := l.channel.SettleHTLC(pre)
		if err != nil {
			// TODO(roasbeef): broadcast on-chain
			l.fail("unable to settle incoming HTLC: %v", err)
			return
		}

		// With the HTLC settled, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled.
		htlc.ChanID = l.ChanID()
		htlc.ID = logIndex

		// Then we send the HTLC settle message to the connected peer
		// so we can continue the propagation of the settle message.
		l.cfg.Peer.SendMessage(htlc)
		isSettle = true

	case *lnwire.UpdateFailHTLC:
		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		logIndex, err := l.channel.FailHTLC(pkt.payHash)
		if err != nil {
			log.Errorf("unable to cancel HTLC: %v", err)
			return
		}

		// With the HTLC removed, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled. The "Reason" field will have already been set
		// within the switch.
		htlc.ChanID = l.ChanID()
		htlc.ID = logIndex

		// Finally, we send the HTLC message to the peer which
		// initially created the HTLC.
		l.cfg.Peer.SendMessage(htlc)
		isSettle = true
	}

	l.batchCounter++

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	if l.batchCounter >= 10 || isSettle {
		if err := l.updateCommitTx(); err != nil {
			l.fail("unable to update commitment: %v", err)
			return
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (l *channelLink) handleUpstreamMsg(msg lnwire.Message) {
	switch msg := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the preimage.
		index, err := l.channel.ReceiveHTLC(msg)
		if err != nil {
			l.fail("unable to handle upstream add HTLC: %v", err)
			return
		}
		log.Tracef("Receive upstream htlc with payment hash(%x), "+
			"assigning index: %v", msg.PaymentHash[:], index)

		// Store the onion blob which encapsulate the htlc route and
		// use in on stage of HTLC inclusion to retrieve the next hop
		// and propagate the HTLC along the remaining route.
		l.clearedOnionBlobs[index] = msg.OnionBlob

	case *lnwire.UpdateFufillHTLC:
		pre := msg.PaymentPreimage
		idx := msg.ID
		if err := l.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			l.fail("unable to handle upstream settle HTLC: %v", err)
			return
		}

		// TODO(roasbeef): add preimage to DB in order to swipe
		// repeated r-values

	case *lnwire.UpdateFailMalformedHTLC:
		// If remote side have been unable to parse the onion blob we
		// have sent to it, than we should transform the malformed HTLC
		// message to the usual HTLC fail message.
		idx := msg.ID
		if err := l.channel.ReceiveFailHTLC(idx); err != nil {
			l.fail("unable to handle upstream fail HTLC: %v", err)
			return
		}

		// Convert the failure type encoded within the HTLC fail
		// message to the proper generic lnwire error code.
		var failure lnwire.FailureMessage
		switch msg.FailureCode {
		case lnwire.CodeInvalidOnionVersion:
			failure = &lnwire.FailInvalidOnionVersion{
				OnionSHA256: msg.ShaOnionBlob,
			}
		case lnwire.CodeInvalidOnionHmac:
			failure = &lnwire.FailInvalidOnionHmac{
				OnionSHA256: msg.ShaOnionBlob,
			}

		case lnwire.CodeInvalidOnionKey:
			failure = &lnwire.FailInvalidOnionKey{
				OnionSHA256: msg.ShaOnionBlob,
			}
		default:
			// TODO(roasbeef): fail channel here?
			log.Errorf("unable to understand code of received " +
				"malformed error")
			return
		}

		// With the error parsed, we'll convert the into it's opaque
		// form.
		var b bytes.Buffer
		if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
			log.Errorf("unable to encode malformed error: %v", err)
			return
		}

		l.cancelReasons[idx] = lnwire.OpaqueReason(b.Bytes())

	case *lnwire.UpdateFailHTLC:
		idx := msg.ID
		if err := l.channel.ReceiveFailHTLC(idx); err != nil {
			l.fail("unable to handle upstream fail HTLC: %v", err)
			return
		}

		l.cancelReasons[idx] = msg.Reason

	case *lnwire.CommitSig:
		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		err := l.channel.ReceiveNewCommitment(msg.CommitSig, msg.HtlcSigs)
		if err != nil {
			l.fail("unable to accept new commitment: %v", err)
			return
		}

		// As we've just just accepted a new state, we'll now
		// immediately send the remote peer a revocation for our prior
		// state.
		nextRevocation, err := l.channel.RevokeCurrentCommitment()
		if err != nil {
			log.Errorf("unable to revoke commitment: %v", err)
			return
		}
		l.cfg.Peer.SendMessage(nextRevocation)

		// As we've just received a commitment signature, we'll
		// re-start the log commit timer to wake up the main processing
		// loop to check if we need to send a commitment signature as
		// we owe one.
		//
		// TODO(roasbeef): instead after revocation?
		if !l.logCommitTimer.Stop() {
			select {
			case <-l.logCommitTimer.C:
			default:
			}
		}
		l.logCommitTimer.Reset(300 * time.Millisecond)
		l.logCommitTick = l.logCommitTimer.C

		// If both commitment chains are fully synced from our PoV,
		// then we don't need to reply with a signature as both sides
		// already have a commitment with the latest accepted l.
		if l.channel.FullySynced() {
			return
		}

		// Otherwise, the remote party initiated the state transition,
		// so we'll reply with a signature to provide them with their
		// version of the latest commitment l.
		if err := l.updateCommitTx(); err != nil {
			l.fail("unable to update commitment: %v", err)
			return
		}

	case *lnwire.RevokeAndAck:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		htlcs, err := l.channel.ReceiveRevocation(msg)
		if err != nil {
			l.fail("unable to accept revocation: %v", err)
			return
		}

		// After we treat HTLCs as included in both remote/local
		// commitment transactions they might be safely propagated over
		// htlc switch or settled if our node was last node in htlc
		// path.
		htlcsToForward := l.processLockedInHtlcs(htlcs)
		go func() {
			log.Debugf("ChannelPoint(%v) forwarding %v HTLC's",
				l.channel.ChannelPoint(), len(htlcsToForward))
			for _, packet := range htlcsToForward {
				if err := l.cfg.Switch.forward(packet); err != nil {
					log.Errorf("channel link(%v): "+
						"unhandled error while forwarding "+
						"htlc packet over htlc  "+
						"switch: %v", l, err)
				}
			}
		}()
	case *lnwire.UpdateFee:
		// We received fee update from peer. If we are the initator we
		// will fail the channel, if not we will apply the update.
		fee := msg.FeePerKw
		if err := l.channel.ReceiveUpdateFee(fee); err != nil {
			l.fail("error receiving fee update: %v", err)
			return
		}
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (l *channelLink) updateCommitTx() error {
	theirCommitSig, htlcSigs, err := l.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		log.Tracef("revocation window exhausted, unable to send %v",
			l.batchCounter)
		return nil
	} else if err != nil {
		return err
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    l.ChanID(),
		CommitSig: theirCommitSig,
		HtlcSigs:  htlcSigs,
	}
	l.cfg.Peer.SendMessage(commitSig)

	// We've just initiated a state transition, attempt to stop the
	// logCommitTimer. If the timer already ticked, then we'll consume the
	// value, dropping
	if l.logCommitTimer != nil && !l.logCommitTimer.Stop() {
		select {
		case <-l.logCommitTimer.C:
		default:
		}
	}
	l.logCommitTick = nil

	// Finally, clear our the current batch, so we can accurately make
	// further batch flushing decisions.
	l.batchCounter = 0

	return nil
}

// Peer returns the representation of remote peer with which we have the
// channel link opened.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Peer() Peer {
	return l.cfg.Peer
}

// ShortChanID returns the short channel ID for the channel link. The short
// channel ID encodes the exact location in the main chain that the original
// funding output can be found.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ShortChanID() lnwire.ShortChannelID {
	return l.channel.ShortChanID()
}

// ChanID returns the channel ID for the channel link. The channel ID is a more
// compact representation of a channel's full outpoint.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ChanID() lnwire.ChannelID {
	return lnwire.NewChanIDFromOutPoint(l.channel.ChannelPoint())
}

// getBandwidthCmd is a wrapper for get bandwidth handler.
type getBandwidthCmd struct {
	resp chan lnwire.MilliSatoshi
}

// Bandwidth returns the amount which current link might pass through channel
// link. Execution through control channel gives as confidence that bandwidth
// will not be changed during function execution.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Bandwidth() lnwire.MilliSatoshi {
	command := &getBandwidthCmd{
		resp: make(chan lnwire.MilliSatoshi, 1),
	}

	select {
	case l.linkControl <- command:
		return <-command.resp
	case <-l.quit:
		return 0
	}
}

// getBandwidth returns the amount which current link might pass through
// channel link.
//
// NOTE: Should be used inside main goroutine only, otherwise the result might
// not be accurate.
func (l *channelLink) getBandwidth() lnwire.MilliSatoshi {
	// TODO(roasbeef): factor in reserve, just grab mutex
	return l.channel.LocalAvailableBalance() - l.overflowQueue.pendingAmount()
}

// policyUpdate is a message sent to a channel link when an outside sub-system
// wishes to update the current forwarding policy.
type policyUpdate struct {
	policy ForwardingPolicy

	done chan struct{}
}

// UpdateForwardingPolicy updates the forwarding policy for the target
// ChannelLink. Once updated, the link will use the new forwarding policy to
// govern if it an incoming HTLC should be forwarded or not. Note that this
// processing of the new policy will ensure that uninitialized fields in the
// passed policy won't override already initialized fields in the current
// policy.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) UpdateForwardingPolicy(newPolicy ForwardingPolicy) {
	cmd := &policyUpdate{
		policy: newPolicy,
		done:   make(chan struct{}),
	}

	select {
	case l.linkControl <- cmd:
	case <-l.quit:
	}

	select {
	case <-cmd.done:
	case <-l.quit:
	}
}

// Stats returns the statistics of channel link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stats() (uint64, lnwire.MilliSatoshi, lnwire.MilliSatoshi) {
	snapshot := l.channel.StateSnapshot()

	return snapshot.NumUpdates,
		snapshot.TotalMilliSatoshisSent,
		snapshot.TotalMilliSatoshisReceived
}

// String returns the string representation of channel link.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) String() string {
	return l.channel.ChannelPoint().String()
}

// HandleSwitchPacket handles the switch packets. This packets which might be
// forwarded to us from another channel link in case the htlc update came from
// another peer or if the update was created by user
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleSwitchPacket(packet *htlcPacket) {
	select {
	case l.downstream <- packet:
	case <-l.quit:
	}
}

// HandleChannelUpdate handles the htlc requests as settle/add/fail which sent
// to us from remote peer we have a channel with.
//
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleChannelUpdate(message lnwire.Message) {
	select {
	case l.upstream <- message:
	case <-l.quit:
	}
}

// updateChannelFee updates the commitment fee-per-kw on this channel by
// committing to an update_fee message.
func (l *channelLink) updateChannelFee(feePerKw btcutil.Amount) error {
	// Update local fee.
	if err := l.channel.UpdateFee(feePerKw); err != nil {
		return err
	}

	// Send fee update to remote.
	msg := lnwire.NewUpdateFee(l.ChanID(), feePerKw)
	return l.cfg.Peer.SendMessage(msg)
}

// processLockedInHtlcs serially processes each of the log updates which have
// been "locked-in". An HTLC is considered locked-in once it has been fully
// committed to in both the remote and local commitment state. Once a channel
// updates is locked-in, then it can be acted upon, meaning: settling htlc's,
// cancelling them, or forwarding new HTLC's to the next hop.
func (l *channelLink) processLockedInHtlcs(
	paymentDescriptors []*lnwallet.PaymentDescriptor) []*htlcPacket {

	var (
		needUpdate       bool
		packetsToForward []*htlcPacket
	)

	for _, pd := range paymentDescriptors {
		// TODO(roasbeef): rework log entries to a shared
		// interface.
		switch pd.EntryType {

		// A settle for an HTLC we previously forwarded HTLC has been
		// received. So we'll forward the HTLC to the switch which
		// will handle propagating the settle to the prior hop.
		case lnwallet.Settle:
			settleUpdate := &lnwire.UpdateFufillHTLC{
				PaymentPreimage: pd.RPreimage,
			}
			settlePacket := newSettlePacket(l.ShortChanID(),
				settleUpdate, pd.RHash, pd.Amount)

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			packetsToForward = append(packetsToForward, settlePacket)
			l.overflowQueue.release()

		// A failureCode message for a previously forwarded HTLC has been
		// received. As a result a new slot will be freed up in our
		// commitment state, so we'll forward this to the switch so the
		// backwards undo can continue.
		case lnwallet.Fail:
			// Fetch the reason the HTLC was cancelled so we can
			// continue to propagate it.
			opaqueReason := l.cancelReasons[pd.ParentIndex]

			failUpdate := &lnwire.UpdateFailHTLC{
				Reason: opaqueReason,
				ChanID: l.ChanID(),
			}
			failPacket := newFailPacket(l.ShortChanID(), failUpdate,
				pd.RHash, pd.Amount, false)

			// Add the packet to the batch to be forwarded, and
			// notify the overflow queue that a spare spot has been
			// freed up within the commitment state.
			packetsToForward = append(packetsToForward, failPacket)
			l.overflowQueue.release()

		// An incoming HTLC add has been full-locked in. As a result we
		// can no examine the forwarding details of the HTLC, and the
		// HTLC itself to decide if: we should forward it, cancel it,
		// or are able to settle it (and it adheres to our fee related
		// constraints).
		case lnwallet.Add:
			// Fetch the onion blob that was included within this
			// processed payment descriptor.
			onionBlob := l.clearedOnionBlobs[pd.Index]
			delete(l.clearedOnionBlobs, pd.Index)

			// Retrieve onion obfuscator from onion blob in order
			// to produce initial obfuscation of the onion
			// failureCode.
			onionReader := bytes.NewReader(onionBlob[:])
			obfuscator, failureCode := l.cfg.DecodeOnionObfuscator(
				onionReader,
			)
			if failureCode != lnwire.CodeNone {
				// If we're unable to process the onion blob
				// than we should send the malformed htlc error
				// to payment sender.
				l.sendMalformedHTLCError(pd.RHash, failureCode,
					onionBlob[:])
				needUpdate = true

				log.Errorf("unable to decode onion "+
					"obfuscator: %v", failureCode)
				continue
			}

			// Before adding the new htlc to the state machine,
			// parse the onion object in order to obtain the
			// routing information with DecodeHopIterator function
			// which process the Sphinx packet.
			//
			// We include the payment hash of the htlc as it's
			// authenticated within the Sphinx packet itself as
			// associated data in order to thwart attempts a replay
			// attacks. In the case of a replay, an attacker is
			// *forced* to use the same payment hash twice, thereby
			// losing their money entirely.
			onionReader = bytes.NewReader(onionBlob[:])
			chanIterator, failureCode := l.cfg.DecodeHopIterator(
				onionReader, pd.RHash[:],
			)
			if failureCode != lnwire.CodeNone {
				// If we're unable to process the onion blob
				// than we should send the malformed htlc error
				// to payment sender.
				l.sendMalformedHTLCError(pd.RHash, failureCode,
					onionBlob[:])
				needUpdate = true

				log.Errorf("unable to decode onion hop "+
					"iterator: %v", failureCode)
				continue
			}

			heightNow := l.bestHeight

			fwdInfo := chanIterator.ForwardingInstructions()
			switch fwdInfo.NextHop {
			case exitHop:
				// First, we'll check the expiry of the HTLC
				// itself against, the current block height. If
				// the timeout is too soon, then we'll reject
				// the HTLC.
				if pd.Timeout-expiryGraceDelta <= heightNow {
					log.Errorf("htlc(%x) has an expiry "+
						"that's too soon: expiry=%v, "+
						"best_height=%v", pd.RHash[:],
						pd.Timeout, heightNow)

					failure := lnwire.FailFinalIncorrectCltvExpiry{}
					l.sendHTLCError(pd.RHash, &failure, obfuscator)
					needUpdate = true
					continue
				}

				// We're the designated payment destination.
				// Therefore we attempt to see if we have an
				// invoice locally which'll allow us to settle
				// this htlc.
				invoiceHash := chainhash.Hash(pd.RHash)
				invoice, err := l.cfg.Registry.LookupInvoice(invoiceHash)
				if err != nil {
					log.Errorf("unable to query invoice registry: "+
						" %v", err)
					failure := lnwire.FailUnknownPaymentHash{}
					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// As we're the exit hop, we'll double check
				// the hop-payload included in the HTLC to
				// ensure that it was crafted correctly by the
				// sender and matches the HTLC we were
				// extended.
				if !l.cfg.DebugHTLC &&
					fwdInfo.AmountToForward != invoice.Terms.Value {

					log.Errorf("Onion payload of incoming "+
						"htlc(%x) has incorrect value: "+
						"expected %v, got %v", pd.RHash,
						invoice.Terms.Value,
						fwdInfo.AmountToForward)

					failure := lnwire.FailIncorrectPaymentAmount{}
					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// We'll also ensure that our time-lock value
				// has been computed correctly.
				expectedHeight := heightNow + l.cfg.FwrdingPolicy.TimeLockDelta
				if !l.cfg.DebugHTLC {
					switch {
					case fwdInfo.OutgoingCTLV < expectedHeight:
						log.Errorf("Onion payload of incoming "+
							"htlc(%x) has incorrect time-lock: "+
							"expected %v, got %v",
							pd.RHash[:], expectedHeight,
							fwdInfo.OutgoingCTLV)

						failure := lnwire.NewFinalIncorrectCltvExpiry(
							fwdInfo.OutgoingCTLV,
						)
						l.sendHTLCError(pd.RHash, failure, obfuscator)
						needUpdate = true
						continue
					case pd.Timeout != fwdInfo.OutgoingCTLV:
						log.Errorf("HTLC(%x) has incorrect "+
							"time-lock: expected %v, got %v",
							pd.RHash[:], pd.Timeout,
							fwdInfo.OutgoingCTLV)

						failure := lnwire.NewFinalIncorrectCltvExpiry(
							fwdInfo.OutgoingCTLV,
						)
						l.sendHTLCError(pd.RHash, failure, obfuscator)
						needUpdate = true
						continue
					}
				}

				// If we're not currently in debug mode, and
				// the extended htlc doesn't meet the value
				// requested, then we'll fail the htlc.
				// Otherwise, we settle this htlc within our
				// local state update log, then send the update
				// entry to the remote party.
				if !l.cfg.DebugHTLC && pd.Amount < invoice.Terms.Value {
					log.Errorf("rejecting htlc due to incorrect "+
						"amount: expected %v, received %v",
						invoice.Terms.Value, pd.Amount)
					failure := lnwire.FailIncorrectPaymentAmount{}
					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := l.channel.SettleHTLC(preimage)
				if err != nil {
					l.fail("unable to settle htlc: %v", err)
					return nil
				}

				// Notify the invoiceRegistry of the invoices
				// we just settled with this latest commitment
				// update.
				err = l.cfg.Registry.SettleInvoice(invoiceHash)
				if err != nil {
					l.fail("unable to settle invoice: %v", err)
					return nil
				}

				// HTLC was successfully settled locally send
				// notification about it remote peer.
				l.cfg.Peer.SendMessage(&lnwire.UpdateFufillHTLC{
					ChanID:          l.ChanID(),
					ID:              logIndex,
					PaymentPreimage: preimage,
				})
				needUpdate = true

			// There are additional channels left within this
			// route. So we'll verify that our forwarding
			// constraints have been properly met by by this
			// incoming HTLC.
			default:
				// We want to avoid forwarding an HTLC which
				// will expire in the near future, so we'll
				// reject an HTLC if its expiration time is too
				// close to the current height.
				timeDelta := l.cfg.FwrdingPolicy.TimeLockDelta
				if pd.Timeout-timeDelta <= heightNow {
					log.Errorf("htlc(%x) has an expiry "+
						"that's too soon: expiry=%v, "+
						"best_height=%v", pd.RHash[:],
						pd.Timeout, heightNow)

					var failure lnwire.FailureMessage
					update, err := l.cfg.GetLastChannelUpdate()
					if err != nil {
						failure = lnwire.NewTemporaryChannelFailure(nil)
					} else {
						failure = lnwire.NewExpiryTooSoon(*update)
					}

					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// As our second sanity check, we'll ensure that
				// the passed HTLC isn't too small. If so, then
				// we'll cancel the HTLC directly.
				if pd.Amount < l.cfg.FwrdingPolicy.MinHTLC {
					log.Errorf("Incoming htlc(%x) is too "+
						"small: min_htlc=%v, hltc_value=%v",
						pd.RHash[:], l.cfg.FwrdingPolicy.MinHTLC,
						pd.Amount)

					// As part of the returned error, we'll
					// send our latest routing policy so
					// the sending node obtains the most up
					// to date data.
					var failure lnwire.FailureMessage
					update, err := l.cfg.GetLastChannelUpdate()
					if err != nil {
						failure = lnwire.NewTemporaryChannelFailure(nil)
					} else {
						failure = lnwire.NewAmountBelowMinimum(
							pd.Amount, *update)
					}

					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// Next, using the amount of the incoming HTLC,
				// we'll calculate the expected fee this
				// incoming HTLC must carry in order to be
				// accepted.
				expectedFee := ExpectedFee(
					l.cfg.FwrdingPolicy,
					fwdInfo.AmountToForward,
				)

				// If the amount of the incoming HTLC, minus
				// our expected fee isn't equal to the
				// forwarding instructions, then either the
				// values have been tampered with, or the send
				// used incorrect/dated information to
				// construct the forwarding information for
				// this hop. In any case, we'll cancel this
				// HTLC.
				if pd.Amount-expectedFee < fwdInfo.AmountToForward {
					log.Errorf("Incoming htlc(%x) has "+
						"insufficient fee: expected "+
						"%v, got %v", pd.RHash[:],
						int64(expectedFee),
						int64(pd.Amount-fwdInfo.AmountToForward))

					// As part of the returned error, we'll
					// send our latest routing policy so
					// the sending node obtains the most up
					// to date data.
					var failure lnwire.FailureMessage
					update, err := l.cfg.GetLastChannelUpdate()
					if err != nil {
						failure = lnwire.NewTemporaryChannelFailure(nil)
					} else {
						failure = lnwire.NewFeeInsufficient(pd.Amount,
							*update)
					}

					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// Finally, we'll ensure that the time-lock on
				// the outgoing HTLC meets the following
				// constraint: the incoming time-lock minus our
				// time-lock delta should equal the outgoing
				// time lock. Otherwise, whether the sender
				// messed up, or an intermediate node tampered
				// with the HTLC.
				if pd.Timeout-timeDelta != fwdInfo.OutgoingCTLV {
					log.Errorf("Incoming htlc(%x) has "+
						"incorrect time-lock value: expected "+
						"%v blocks, got %v blocks",
						pd.RHash[:], pd.Timeout-timeDelta,
						fwdInfo.OutgoingCTLV)

					// Grab the latest routing policy so
					// the sending node is up to date with
					// our current policy.
					update, err := l.cfg.GetLastChannelUpdate()
					if err != nil {
						l.fail("unable to create channel update "+
							"while handling the error: %v", err)
						return nil
					}

					failure := lnwire.NewIncorrectCltvExpiry(
						pd.Timeout, *update)
					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				// TODO(roasbeef): also add max timeout value

				// With all our forwarding constraints met,
				// we'll create the outgoing HTLC using the
				// parameters as specified in the forwarding
				// info.
				addMsg := &lnwire.UpdateAddHTLC{
					Expiry:      fwdInfo.OutgoingCTLV,
					Amount:      fwdInfo.AmountToForward,
					PaymentHash: pd.RHash,
				}

				// Finally, we'll encode the onion packet for
				// the _next_ hop using the hop iterator
				// decoded for the current hop.
				buf := bytes.NewBuffer(addMsg.OnionBlob[0:0])
				err := chanIterator.EncodeNextHop(buf)
				if err != nil {
					log.Errorf("unable to encode the "+
						"remaining route %v", err)

					failure := lnwire.NewTemporaryChannelFailure(nil)
					l.sendHTLCError(pd.RHash, failure, obfuscator)
					needUpdate = true
					continue
				}

				updatePacket := newAddPacket(l.ShortChanID(),
					fwdInfo.NextHop, addMsg, obfuscator)
				packetsToForward = append(packetsToForward, updatePacket)
			}
		}
	}

	if needUpdate {
		// With all the settle/cancel updates added to the local and
		// remote HTLC logs, initiate a state transition by updating
		// the remote commitment chain.
		if err := l.updateCommitTx(); err != nil {
			l.fail("unable to update commitment: %v", err)
			return nil
		}
	}

	return packetsToForward
}

// sendHTLCError functions cancels HTLC and send cancel message back to the
// peer from which HTLC was received.
func (l *channelLink) sendHTLCError(rHash [32]byte, failure lnwire.FailureMessage,
	obfuscator Obfuscator) {
	reason, err := obfuscator.InitialObfuscate(failure)
	if err != nil {
		log.Errorf("unable to obfuscate error: %v", err)
		return
	}

	index, err := l.channel.FailHTLC(rHash)
	if err != nil {
		log.Errorf("unable cancel htlc: %v", err)
		return
	}

	l.cfg.Peer.SendMessage(&lnwire.UpdateFailHTLC{
		ChanID: l.ChanID(),
		ID:     index,
		Reason: reason,
	})
}

// sendMalformedHTLCError helper function which sends the malformed HTLC update
// to the payment sender.
func (l *channelLink) sendMalformedHTLCError(rHash [32]byte, code lnwire.FailCode,
	onionBlob []byte) {
	index, err := l.channel.FailHTLC(rHash)
	if err != nil {
		log.Errorf("unable cancel htlc: %v", err)
		return
	}

	l.cfg.Peer.SendMessage(&lnwire.UpdateFailMalformedHTLC{
		ChanID:       l.ChanID(),
		ID:           index,
		ShaOnionBlob: sha256.Sum256(onionBlob),
		FailureCode:  code,
	})
}

// fail helper function which is used to encapsulate the action necessary for
// proper disconnect.
func (l *channelLink) fail(format string, a ...interface{}) {
	reason := errors.Errorf(format, a...)
	log.Error(reason)
	l.cfg.Peer.Disconnect(reason)
}
