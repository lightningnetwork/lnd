package htlcswitch

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/pkg/errors"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// ChannelLinkConfig defines the configuration for the channel link. ALL
// elements within the configuration MUST be non-nil for channel link to carry
// out its duties.
type ChannelLinkConfig struct {
	// Switch is a subsystem which is used to forward the incoming htlc
	// packets to other peer which should handle it.
	Switch *Switch

	// Peer is a lightning network node with which we have the channel
	// link opened.
	Peer Peer

	// Registry is a sub-system which responsible for managing the
	// invoices in thread-safe manner.
	Registry InvoiceDatabase

	// SettledContracts is used to notify that a channel has peacefully been
	// closed. Once a channel has been closed the other subsystem no longer
	// needs to watch for breach closes.
	SettledContracts chan *wire.OutPoint

	// DebugHTLC should be turned on if you want all HTLCs sent to a node
	// with the debug htlc R-Hash are immediately settled in the next
	// available state transition.
	DebugHTLC bool
}

// channelLink is the service which drives a channel's commitment update
// state-machine. In the event that an htlc needs to be propagated to another
// link, then forward handler from config is used which sends htlc to the
// switch. Additionally, the link encapsulate logic of commitment protocol
// message ordering and updates.
type channelLink struct {
	// htlcsToSettle is a list of preimages which allow us to settle one or
	// many of the pending HTLCs we've received from the upstream peer.
	htlcsToSettle map[uint64]*channeldb.Invoice

	// htlcsToCancel is a set of HTLCs identified by their log index which
	// are to be cancelled upon the next state transition.
	htlcsToCancel map[uint64]lnwire.FailCode

	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	cancelReasons map[uint64]lnwire.FailCode

	// pendingBatch is slice of payments which have been added to the
	// channel update log, but not yet committed to latest commitment.
	pendingBatch []*pendingPayment

	// clearedHTCLs is a map of outgoing HTLCs we've committed to in our
	// chain which have not yet been settled by the upstream peer.
	clearedHTCLs map[uint64]*pendingPayment

	// switchChan is a channel used to send packets to the htlc switch for
	// forwarding.
	switchChan chan<- *htlcPacket

	// sphinx is an instance of the Sphinx onion Router for this node. The
	// router will be used to process all incoming Sphinx packets embedded
	// within HTLC add messages.
	sphinx *sphinx.Router

	// pendingCircuits tracks the remote log index of the incoming HTLCs,
	// mapped to the processed Sphinx packet contained within the HTLC.
	// This map is used as a staging area between when an HTLC is added to
	// the log, and when it's locked into the commitment state of both
	// chains. Once locked in, the processed packet is sent to the switch
	// along with the HTLC to forward the packet to the next hop.
	pendingCircuits map[uint64]*sphinx.ProcessedPacket

	channel   *lnwallet.LightningChannel
	chanPoint *wire.OutPoint
	chanID    lnwire.ChannelID

	// cfg is a structure which carries all dependable fields/handlers
	// which may affect behaviour of the service.
	cfg *ChannelLinkConfig

	// upstream is a channel which responsible for propagating the
	// received from remote peer messages, with which we have an opened
	// channel, to handler function.
	upstream chan lnwire.Message

	// downstream is a channel which responsible for propagating
	// the received htlc switch packet which are forwarded from anther
	// channel to the handler function.
	downstream chan *htlcPacket

	// control is used to propagate the commands to its handlers. This
	// channel is needed in order to handle commands in sequence manner,
	// i.e in the main handler loop.
	control chan interface{}

	started  int32
	shutdown int32
	wg       sync.WaitGroup
	quit     chan struct{}
}

// A compile time check to ensure channelLink implements the ChannelLink
// interface.
var _ ChannelLink = (*channelLink)(nil)

// Start starts all helper goroutines required for the operation of the
// channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Start() error {
	if !atomic.CompareAndSwapInt32(&l.started, 0, 1) {
		log.Warnf("channel link(%v): already started", l)
		return nil
	}

	log.Infof("channel link(%v): starting", l)

	l.wg.Add(1)
	go l.htlcHandler()

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stop() {
	if !atomic.CompareAndSwapInt32(&l.shutdown, 0, 1) {
		log.Warnf("channel link(%v): already stopped", l)
		return
	}

	log.Infof("channel link(%v): stopping", l)

	close(l.quit)
	l.wg.Wait()
}

// htlcHandler is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// This goroutine reads messages from the upstream (remote) peer, and also from
// downstream channel managed by the channel link. In the event that an htlc
// needs to be forwarded, then send-only forward handler is used which sends
// htlc packets to the switch. Additionally, the this goroutine handles acting
// upon all timeouts for any active HTLCs, manages the channel's revocation
// window, and also the htlc trickle queue+timer for this active channels.
// NOTE: Should be started as goroutine.
func (l *channelLink) htlcHandler() {
	defer l.wg.Done()

	log.Infof("HTLC manager for ChannelPoint(%v) started, "+
		"bandwidth=%v", l.channel.ChannelPoint(), l.getBandwidth())

	// A new session for this active channel has just started, therefore we
	// need to send our initial revocation window to the remote peer.
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		rev, err := l.channel.ExtendRevocationWindow()
		if err != nil {
			log.Errorf("unable to expand revocation window: %v", err)
			continue
		}
		l.cfg.Peer.SendMessage(rev)
	}

	// TODO(roasbeef): check to see if able to settle any currently pending
	// HTLCs
	//   * also need signals when new invoices are added by the
	//   invoiceRegistry

	batchTimer := time.NewTicker(50 * time.Millisecond)
	defer batchTimer.Stop()

	logCommitTimer := time.NewTicker(100 * time.Millisecond)
	defer logCommitTimer.Stop()
out:
	for {
		select {
		case <-l.channel.UnilateralCloseSignal:
			// TODO(roasbeef): need to send HTLC outputs to nursery
			log.Warnf("Remote peer has closed ChannelPoint(%v) on-chain",
				l.channel.ChannelPoint())
			if err := l.cfg.Peer.WipeChannel(l.channel); err != nil {
				log.Errorf("unable to wipe channel %v", err)
			}

			l.cfg.SettledContracts <- l.channel.ChannelPoint()
			break out

		case <-l.channel.ForceCloseSignal:
			// TODO(roasbeef): path never taken now that server
			// force closes's directly?
			log.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peerID(%x)",
				l.channel.ChannelPoint(), l.cfg.Peer.ID())
			break out

		case <-logCommitTimer.C:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit due to our
			// commitment chains being desynchronized.
			if l.channel.FullySynced() &&
				len(l.htlcsToSettle) == 0 {
				continue
			}

			if err := l.updateCommitTx(); err != nil {
				log.Errorf("unable to update commitment: %v",
					err)
				l.cfg.Peer.Disconnect()
				break out
			}

		case <-batchTimer.C:
			// If the current batch is empty, then we have no work
			// here.
			if len(l.pendingBatch) == 0 {
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if err := l.updateCommitTx(); err != nil {
				log.Errorf("unable to update "+
					"commitment: %v", err)
				l.cfg.Peer.Disconnect()
				break out
			}

		case pkt := <-l.downstream:
			l.handleDownStreamPkt(pkt)

		case msg := <-l.upstream:
			l.handleUpstreamMsg(msg)

		case cmd := <-l.control:
			switch cmd := cmd.(type) {
			case *getBandwidthCmd:
				cmd.done <- l.getBandwidth()
			}

		case <-l.quit:
			break out
		}
	}

	log.Infof("channel link(%v): htlc handler closed", l)
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
func (l *channelLink) handleDownStreamPkt(pkt *htlcPacket) {
	var isSettle bool
	switch htlc := pkt.htlc.(type) {
	case *lnwire.UpdateAddHTLC:
		// A new payment has been initiated via the
		// downstream channel, so we add the new HTLC
		// to our local log, then update the commitment
		// chains.
		htlc.ChanID = l.ChanID()
		index, err := l.channel.AddHTLC(htlc)
		if err != nil {
			// TODO: possibly perform fallback/retry logic
			// depending on type of error

			// The HTLC was unable to be added to the state
			// machine, as a result, we'll signal the switch to
			// cancel the pending payment.
			go l.cfg.Switch.forward(newFailPacket(l.ChanID(),
				&lnwire.UpdateFailHTLC{
					Reason: []byte{byte(0)},
				}, htlc.PaymentHash, htlc.Amount))

			log.Errorf("unable to handle downstream add HTLC: %v",
				err)
			return
		}
		htlc.ID = index

		l.cfg.Peer.SendMessage(htlc)

		l.pendingBatch = append(l.pendingBatch, &pendingPayment{
			htlc:     htlc,
			index:    index,
			preImage: pkt.preImage,
			err:      pkt.err,
			done:     pkt.done,
		})

	case *lnwire.UpdateFufillHTLC:
		// An HTLC we forward to the switch has just settled somewhere
		// upstream. Therefore we settle the HTLC within the our local
		// state machine.
		pre := htlc.PaymentPreimage
		logIndex, err := l.channel.SettleHTLC(pre)
		if err != nil {
			// TODO(roasbeef): broadcast on-chain
			log.Errorf("settle for incoming HTLC "+
				"rejected: %v", err)
			l.cfg.Peer.Disconnect()
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

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	// TODO(roasbeef): enforce max HTLCs in flight limit
	if len(l.pendingBatch) >= 10 || isSettle {
		if err := l.updateCommitTx(); err != nil {
			log.Errorf("unable to update "+
				"commitment: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (l *channelLink) handleUpstreamMsg(msg lnwire.Message) {
	switch msg := msg.(type) {
	// TODO(roasbeef): timeouts
	//  * fail if can't parse sphinx mix-header
	case *lnwire.UpdateAddHTLC:
		// Before adding the new HTLC to the state machine, parse the
		// onion object in order to obtain the routing information.
		blobReader := bytes.NewReader(msg.OnionBlob[:])
		onionPkt := &sphinx.OnionPacket{}
		if err := onionPkt.Decode(blobReader); err != nil {
			log.Errorf("unable to decode onion pkt: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the preimage
		index, err := l.channel.ReceiveHTLC(msg)
		if err != nil {
			log.Errorf("unable to handle upstream add HTLC: %v",
				err)
			l.cfg.Peer.Disconnect()
			return
		}

		// TODO(roasbeef): perform sanity checks on per-hop payload
		//  * time-lock is sane, fee, chain, etc

		// Attempt to process the Sphinx packet. We include the payment
		// hash of the HTLC as it's authenticated within the Sphinx
		// packet itself as associated data in order to thwart attempts
		// a replay attacks. In the case of a replay, an attacker is
		// *forced* to use the same payment hash twice, thereby losing
		// their money entirely.
		rHash := msg.PaymentHash[:]
		sphinxPacket, err := l.sphinx.ProcessOnionPacket(onionPkt, rHash)
		if err != nil {
			// If we're unable to parse the Sphinx packet, then
			// we'll cancel the HTLC after the current commitment
			// transition.
			log.Errorf("unable to process onion pkt: %v",
				err)
			l.htlcsToCancel[index] = lnwire.SphinxParseError
			return
		}

		switch sphinxPacket.Action {
		// We're the designated payment destination. Therefore we
		// attempt to see if we have an invoice locally which'll allow
		// us to settle this HTLC.
		case sphinx.ExitNode:
			rHash := msg.PaymentHash
			invoice, err := l.cfg.Registry.LookupInvoice(rHash)
			if err != nil {
				// If we're the exit node, but don't recognize
				// the payment hash, then we'll fail the HTLC
				// on the next state transition.
				log.Errorf("unable to settle HTLC, "+
					"payment hash (%x) unrecognized", rHash[:])
				l.htlcsToCancel[index] = lnwire.UnknownPaymentHash
				return
			}

			// If we're not currently in debug mode, and the
			// extended HTLC doesn't meet the value requested, then
			// we'll fail the HTLC.
			if !l.cfg.DebugHTLC && msg.Amount < invoice.Terms.Value {
				log.Errorf("rejecting HTLC due to incorrect "+
					"amount: expected %v, received %v",
					invoice.Terms.Value, msg.Amount)
				l.htlcsToCancel[index] = lnwire.IncorrectValue
			} else {
				// Otherwise, everything is in order and we'll
				// settle the HTLC after the current state
				// transition.
				l.htlcsToSettle[index] = invoice
			}

		// There are additional hops left within this route, so we
		// track the next hop according to the index of this HTLC
		// within their log. When forwarding locked-in HLTC's to the
		// switch, we'll attach the routing information so the switch
		// can finalize the circuit.
		case sphinx.MoreHops:
			l.pendingCircuits[index] = sphinxPacket
		default:
			log.Errorf("malformed onion packet")
			l.htlcsToCancel[index] = lnwire.SphinxParseError
		}

	case *lnwire.UpdateFufillHTLC:
		pre := msg.PaymentPreimage
		idx := msg.ID
		if err := l.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			log.Errorf("unable to handle upstream settle "+
				"HTLC: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		// TODO(roasbeef): add preimage to DB in order to swipe
		// repeated r-values
	case *lnwire.UpdateFailHTLC:
		idx := msg.ID
		if err := l.channel.ReceiveFailHTLC(idx); err != nil {
			log.Errorf("unable to handle upstream fail HTLC: "+
				"%v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		l.cancelReasons[idx] = lnwire.FailCode(msg.Reason[0])

	case *lnwire.CommitSig:
		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		// TODO(roasbeef): redundant re-serialization
		sig := msg.CommitSig.Serialize()
		if err := l.channel.ReceiveNewCommitment(sig); err != nil {
			log.Errorf("unable to accept new commitment: %v", err)
			l.cfg.Peer.Disconnect()
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
			log.Errorf("unable to update commitment: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

	case *lnwire.RevokeAndAck:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		htlcsToForward, err := l.channel.ReceiveRevocation(msg)
		if err != nil {
			log.Errorf("unable to accept revocation: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		// If any of the HTLCs eligible for forwarding are pending
		// settling or timing out previous outgoing payments, then we
		// can them from the pending set, and signal the requester (if
		// existing) that the payment has been fully fulfilled.
		var bandwidthUpdate btcutil.Amount
		settledPayments := make(map[lnwallet.PaymentHash]struct{})
		cancelledHtlcs := make(map[uint64]struct{})
		for _, htlc := range htlcsToForward {
			parentIndex := htlc.ParentIndex
			if p, ok := l.clearedHTCLs[parentIndex]; ok {
				switch htlc.EntryType {
				// If the HTLC was settled successfully, then
				// we return a nil error as well as the payment
				// preimage back to the possible caller.
				case lnwallet.Settle:
					p.preImage <- htlc.RPreimage
					p.err <- nil

					// Otherwise, the HTLC failed, so we propagate
					// the error back to the potential caller.
				case lnwallet.Fail:
					errMsg := l.cancelReasons[parentIndex]
					p.preImage <- [32]byte{}
					p.err <- errors.New(errMsg.String())
				}

				close(p.done)

				delete(l.clearedHTCLs, htlc.ParentIndex)
			}

			// TODO(roasbeef): rework log entries to a shared
			// interface.
			if htlc.EntryType != lnwallet.Add {
				continue
			}

			// If we can settle this HTLC within our local state
			// update log, then send the update entry to the remote
			// party.
			invoice, ok := l.htlcsToSettle[htlc.Index]
			if ok {
				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := l.channel.SettleHTLC(preimage)
				if err != nil {
					log.Errorf("unable to settle htlc: %v", err)
					l.cfg.Peer.Disconnect()
					continue
				}

				settleMsg := &lnwire.UpdateFufillHTLC{
					ChanID:          l.chanID,
					ID:              logIndex,
					PaymentPreimage: preimage,
				}
				l.cfg.Peer.SendMessage(settleMsg)

				delete(l.htlcsToSettle, htlc.Index)
				settledPayments[htlc.RHash] = struct{}{}

				bandwidthUpdate += htlc.Amount
				continue
			}

			// Alternatively, if we marked this HTLC for
			// cancellation, then immediately cancel the HTLC as
			// it's now locked in within both commitment
			// transactions.
			reason, ok := l.htlcsToCancel[htlc.Index]
			if !ok {
				continue
			}

			logIndex, err := l.channel.FailHTLC(htlc.RHash)
			if err != nil {
				log.Errorf("unable to cancel htlc: %v", err)
				l.cfg.Peer.Disconnect()
				continue
			}

			cancelMsg := &lnwire.UpdateFailHTLC{
				ChanID: l.chanID,
				ID:     logIndex,
				Reason: []byte{byte(reason)},
			}
			l.cfg.Peer.SendMessage(cancelMsg)
			delete(l.htlcsToCancel, htlc.Index)

			cancelledHtlcs[htlc.Index] = struct{}{}
		}

		go func() {
			for _, htlc := range htlcsToForward {
				// We don't need to forward any HTLCs that we
				// just settled or cancelled above.
				// TODO(roasbeef): key by index instead?
				if _, ok := settledPayments[htlc.RHash]; ok {
					continue
				}
				if _, ok := cancelledHtlcs[htlc.Index]; ok {
					continue
				}

				onionPkt := l.pendingCircuits[htlc.Index]
				delete(l.pendingCircuits, htlc.Index)

				reason := l.cancelReasons[htlc.ParentIndex]
				delete(l.cancelReasons, htlc.ParentIndex)

				// Send this fully activated HTLC to the htlc
				// switch to continue the chained clear/settle.
				pkt, err := logEntryToHtlcPkt(l.chanID,
					htlc, onionPkt, reason)
				if err != nil {
					log.Errorf("unable to make htlc pkt: %v",
						err)
					continue
				}

				l.switchChan <- pkt
			}

		}()

		if len(settledPayments) == 0 && len(cancelledHtlcs) == 0 {
			return
		}

		// Send an update to the htlc switch of our newly available
		// payment bandwidth.
		// TODO(roasbeef): ideally should wait for next state update.
		if bandwidthUpdate != 0 {
			p.server.htlcSwitch.UpdateLink(state.chanID,
				bandwidthUpdate)
		}

		// With all the settle updates added to the local and remote
		// HTLC logs, initiate a state transition by updating the
		// remote commitment chain.
		if err := l.updateCommitTx(); err != nil {
			log.Errorf("unable to update commitment: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		// Notify the invoiceRegistry of the invoices we just settled
		// with this latest commitment update.
		// TODO(roasbeef): wait until next transition?
		for invoice := range settledPayments {
			err := l.cfg.Registry.SettleInvoice(chainhash.Hash(invoice))
			if err != nil {
				log.Errorf("unable to settle invoice: %v", err)
			}
		}
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (l *channelLink) updateCommitTx() error {
	sigTheirs, err := l.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		log.Tracef("revocation window exhausted, unable to send %v",
			len(l.pendingBatch))
		return nil
	} else if err != nil {
		return err
	}

	parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
	if err != nil {
		return fmt.Errorf("unable to parse sig: %v", err)
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    l.ChanID(),
		CommitSig: parsedSig,
	}
	l.cfg.Peer.SendMessage(commitSig)

	// As we've just cleared out a batch, move all pending updates to the
	// map of cleared HTLCs, clearing out the set of pending updates.
	for _, update := range l.pendingBatch {
		l.clearedHTCLs[update.index] = update
	}

	// Finally, clear our the current batch, and flip the pendingUpdate
	// bool to indicate were waiting for a commitment signature.
	// TODO(roasbeef): re-slice instead to avoid GC?
	l.pendingBatch = nil

	return nil
}

// logEntryToHtlcPkt converts a particular Lightning Commitment Protocol (LCP)
// log entry the corresponding htlcPacket with src/dest set along with the
// proper wire message. This helper method is provided in order to aid an
// htlcManager in forwarding packets to the htlcSwitch.
func logEntryToHtlcPkt(chanID lnwire.ChannelID, pd *lnwallet.PaymentDescriptor,
	onionPkt *sphinx.ProcessedPacket,
	reason lnwire.FailCode) (*htlcPacket, error) {

	pkt := &htlcPacket{}

	// TODO(roasbeef): alter after switch to log entry interface
	var msg lnwire.Message
	switch pd.EntryType {

	case lnwallet.Add:
		// TODO(roasbeef): timeout, onion blob, etc
		var b bytes.Buffer
		if err := onionPkt.Packet.Encode(&b); err != nil {
			return nil, err
		}

		htlc := &lnwire.UpdateAddHTLC{
			Amount:      pd.Amount,
			PaymentHash: pd.RHash,
		}
		copy(htlc.OnionBlob[:], b.Bytes())
		msg = htlc

	case lnwallet.Settle:
		msg = &lnwire.UpdateFufillHTLC{
			PaymentPreimage: pd.RPreimage,
		}

	case lnwallet.Fail:
		// For cancellation messages, we'll also need to set the rHash
		// within the htlcPacket so the switch knows on which outbound
		// link to forward the cancellation message
		msg = &lnwire.UpdateFailHTLC{
			Reason: []byte{byte(reason)},
		}
		pkt.payHash = pd.RHash
	}

	pkt.htlc = msg
	pkt.src = chanID

	return pkt, nil
}

// Peer returns the representation of remote peer with which we
// have the channel link opened.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Peer() Peer {
	return l.cfg.Peer
}

// ChannelPoint returns the unique identificator of the channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) ChanID() lnwire.ChannelID {
	return lnwire.NewChanIDFromOutPoint(l.channel.ChannelPoint())
}

// getBandwidthCmd is a wrapper for get bandwidth handler.
type getBandwidthCmd struct {
	done chan btcutil.Amount
}

// Bandwidth returns the amount which current link might pass
// through channel link. Execution through control channel gives as
// confidence that bandwidth will not be changed during function execution.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Bandwidth() btcutil.Amount {
	command := &getBandwidthCmd{
		done: make(chan btcutil.Amount, 1),
	}

	select {
	case l.control <- command:
		return <-command.done
	case <-l.quit:
		return 0
	}
}

// getBandwidth returns the amount which current link might pass
// through channel link.
// NOTE: Should be use inside main goroutine only, otherwise the result might
// be accurate.
func (l *channelLink) getBandwidth() btcutil.Amount {
	return l.channel.LocalAvailableBalance() - l.queue.pendingAmount()
}

// Stats return the statistics of channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) Stats() (uint64, btcutil.Amount, btcutil.Amount) {
	snapshot := l.channel.StateSnapshot()
	return snapshot.NumUpdates,
		btcutil.Amount(snapshot.TotalSatoshisSent),
		btcutil.Amount(snapshot.TotalSatoshisReceived)
}

// String returns the string representation of channel link.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) String() string {
	return l.channel.ChannelPoint().String()
}

// HandleSwitchPacket handles the switch packets. This packets which might
// be forwarded to us from another channel link in case the htlc update came
// from another peer or if the update was created by user
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleSwitchPacket(packet *htlcPacket) {
	select {
	case l.downstream <- packet:
	case <-l.quit:
	}
}

// HandleChannelUpdate handles the htlc requests as settle/add/fail which
// sent to us from remote peer we have a channel with.
// NOTE: Part of the ChannelLink interface.
func (l *channelLink) HandleChannelUpdate(message lnwire.Message) {
	select {
	case l.upstream <- message:
	case <-l.quit:
	}
}
