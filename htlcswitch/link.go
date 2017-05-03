package htlcswitch

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/Masterminds/glide/cfg"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
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

// commitmentState is the volatile+persistent state of an active channel's
// commitment update state-machine. This struct is used by htlcManager's to
// save meta-state required for proper functioning.
type commitmentState struct {
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
}

// htlcManager is the primary goroutine which drives a channel's commitment
// update state-machine in response to messages received via several channels.
// The htlcManager reads messages from the upstream (remote) peer, and also
// from several possible downstream channels managed by the htlcSwitch. In the
// event that an htlc needs to be forwarded, then send-only htlcPlex chan is
// used which sends htlc packets to the switch for forwarding. Additionally,
// the htlcManager handles acting upon all timeouts for any active HTLCs,
// manages the channel's revocation window, and also the htlc trickle
// queue+timer for this active channels.
func (p *peer) htlcManager(channel *lnwallet.LightningChannel,
	htlcPlex chan<- *htlcPacket, downstreamLink <-chan *htlcPacket,
	upstreamLink <-chan lnwire.Message) {

	chanStats := channel.StateSnapshot()
	peerLog.Infof("HTLC manager for ChannelPoint(%v) started, "+
		"our_balance=%v, their_balance=%v, chain_height=%v",
		channel.ChannelPoint(), chanStats.LocalBalance,
		chanStats.RemoteBalance, chanStats.NumUpdates)

	// A new session for this active channel has just started, therefore we
	// need to send our initial revocation window to the remote peer.
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		rev, err := channel.ExtendRevocationWindow()
		if err != nil {
			peerLog.Errorf("unable to expand revocation window: %v", err)
			continue
		}
		p.queueMsg(rev, nil)
	}

	chanPoint := channel.ChannelPoint()
	state := &commitmentState{
		channel:         channel,
		chanPoint:       chanPoint,
		chanID:          lnwire.NewChanIDFromOutPoint(chanPoint),
		clearedHTCLs:    make(map[uint64]*pendingPayment),
		htlcsToSettle:   make(map[uint64]*channeldb.Invoice),
		htlcsToCancel:   make(map[uint64]lnwire.FailCode),
		cancelReasons:   make(map[uint64]lnwire.FailCode),
		pendingCircuits: make(map[uint64]*sphinx.ProcessedPacket),
		sphinx:          p.server.sphinx,
		switchChan:      htlcPlex,
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
		case <-channel.UnilateralCloseSignal:
			// TODO(roasbeef): need to send HTLC outputs to nursery
			peerLog.Warnf("Remote peer has closed ChannelPoint(%v) on-chain",
				state.chanPoint)
			if err := wipeChannel(p, channel); err != nil {
				peerLog.Errorf("unable to wipe channel %v", err)
			}

			p.server.breachArbiter.settledContracts <- state.chanPoint

			break out

		case <-channel.ForceCloseSignal:
			// TODO(roasbeef): path never taken now that server
			// force closes's directly?
			peerLog.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peerID(%x)",
				state.chanPoint, p.id)
			break out

		case <-logCommitTimer.C:
			// If we haven't sent or received a new commitment
			// update in some time, check to see if we have any
			// pending updates we need to commit due to our
			// commitment chains being desynchronized.
			if state.channel.FullySynced() &&
				len(state.htlcsToSettle) == 0 {
				continue
			}

			if err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update commitment: %v",
					err)
				p.Disconnect()
				break out
			}

		case <-batchTimer.C:
			// If the current batch is empty, then we have no work
			// here.
			if len(state.pendingBatch) == 0 {
				continue
			}

			// Otherwise, attempt to extend the remote commitment
			// chain including all the currently pending entries.
			// If the send was unsuccessful, then abandon the
			// update, waiting for the revocation window to open
			// up.
			if err := p.updateCommitTx(state); err != nil {
				peerLog.Errorf("unable to update "+
					"commitment: %v", err)
				p.Disconnect()
				break out
			}

		case pkt := <-downstreamLink:
			p.handleDownStreamPkt(state, pkt)

		case msg, ok := <-upstreamLink:
			// If the upstream message link is closed, this signals
			// that the channel itself is being closed, therefore
			// we exit.
			if !ok {
				break out
			}

			p.handleUpstreamMsg(state, msg)
		case <-p.quit:
			break out
		}
	}

	p.wg.Done()
	peerLog.Tracef("htlcManager for peer %v done", p)
}

// handleDownStreamPkt processes an HTLC packet sent from the downstream HTLC
// Switch. Possible messages sent by the switch include requests to forward new
// HTLCs, timeout previously cleared HTLCs, and finally to settle currently
// cleared HTLCs with the upstream peer.
func (p *peer) handleDownStreamPkt(state *commitmentState, pkt *htlcPacket) {
	var isSettle bool
	switch htlc := pkt.msg.(type) {
	case *lnwire.UpdateAddHTLC:
		// A new payment has been initiated via the
		// downstream channel, so we add the new HTLC
		// to our local log, then update the commitment
		// chains.
		htlc.ChanID = state.chanID
		index, err := state.channel.AddHTLC(htlc)
		if err != nil {
			// TODO: possibly perform fallback/retry logic
			// depending on type of error
			peerLog.Errorf("Adding HTLC rejected: %v", err)
			pkt.err <- err
			close(pkt.done)

			// The HTLC was unable to be added to the state
			// machine, as a result, we'll signal the switch to
			// cancel the pending payment.
			// TODO(roasbeef): need to update link as well if local
			// HTLC?
			state.switchChan <- &htlcPacket{
				amt: htlc.Amount,
				msg: &lnwire.UpdateFailHTLC{
					Reason: []byte{byte(0)},
				},
				srcLink: state.chanID,
			}
			return
		}

		p.queueMsg(htlc, nil)

		state.pendingBatch = append(state.pendingBatch, &pendingPayment{
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
		logIndex, err := state.channel.SettleHTLC(pre)
		if err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for incoming HTLC rejected: %v", err)
			p.Disconnect()
			return
		}

		// With the HTLC settled, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled.
		htlc.ChanID = state.chanID
		htlc.ID = logIndex

		// Then we send the HTLC settle message to the connected peer
		// so we can continue the propagation of the settle message.
		p.queueMsg(htlc, nil)
		isSettle = true

	case *lnwire.UpdateFailHTLC:
		// An HTLC cancellation has been triggered somewhere upstream,
		// we'll remove then HTLC from our local state machine.
		logIndex, err := state.channel.FailHTLC(pkt.payHash)
		if err != nil {
			peerLog.Errorf("unable to cancel HTLC: %v", err)
			return
		}

		// With the HTLC removed, we'll need to populate the wire
		// message to target the specific channel and HTLC to be
		// cancelled. The "Reason" field will have already been set
		// within the switch.
		htlc.ChanID = state.chanID
		htlc.ID = logIndex

		// Finally, we send the HTLC message to the peer which
		// initially created the HTLC.
		p.queueMsg(htlc, nil)
		isSettle = true
	}

	// If this newly added update exceeds the min batch size for adds, or
	// this is a settle request, then initiate an update.
	// TODO(roasbeef): enforce max HTLCs in flight limit
	if len(state.pendingBatch) >= 10 || isSettle {
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update "+
				"commitment: %v", err)
			p.Disconnect()
			return
		}
	}
}

// handleUpstreamMsg processes wire messages related to commitment state
// updates from the upstream peer. The upstream peer is the peer whom we have a
// direct channel with, updating our respective commitment chains.
func (p *peer) handleUpstreamMsg(state *commitmentState, msg lnwire.Message) {
	switch htlcPkt := msg.(type) {
	// TODO(roasbeef): timeouts
	//  * fail if can't parse sphinx mix-header
	case *lnwire.UpdateAddHTLC:
		// Before adding the new HTLC to the state machine, parse the
		// onion object in order to obtain the routing information.
		blobReader := bytes.NewReader(htlcPkt.OnionBlob[:])
		onionPkt := &sphinx.OnionPacket{}
		if err := onionPkt.Decode(blobReader); err != nil {
			peerLog.Errorf("unable to decode onion pkt: %v", err)
			p.Disconnect()
			return
		}

		// We just received an add request from an upstream peer, so we
		// add it to our state machine, then add the HTLC to our
		// "settle" list in the event that we know the preimage
		index, err := state.channel.ReceiveHTLC(htlcPkt)
		if err != nil {
			peerLog.Errorf("Receiving HTLC rejected: %v", err)
			p.Disconnect()
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
		rHash := htlcPkt.PaymentHash[:]
		sphinxPacket, err := state.sphinx.ProcessOnionPacket(onionPkt, rHash)
		if err != nil {
			// If we're unable to parse the Sphinx packet, then
			// we'll cancel the HTLC after the current commitment
			// transition.
			peerLog.Errorf("unable to process onion pkt: %v", err)
			state.htlcsToCancel[index] = lnwire.SphinxParseError
			return
		}

		switch sphinxPacket.Action {
		// We're the designated payment destination. Therefore we
		// attempt to see if we have an invoice locally which'll allow
		// us to settle this HTLC.
		case sphinx.ExitNode:
			rHash := htlcPkt.PaymentHash
			invoice, err := p.server.invoices.LookupInvoice(rHash)
			if err != nil {
				// If we're the exit node, but don't recognize
				// the payment hash, then we'll fail the HTLC
				// on the next state transition.
				peerLog.Errorf("unable to settle HTLC, "+
					"payment hash (%x) unrecognized", rHash[:])
				state.htlcsToCancel[index] = lnwire.UnknownPaymentHash
				return
			}

			// If we're not currently in debug mode, and the
			// extended HTLC doesn't meet the value requested, then
			// we'll fail the HTLC.
			if !cfg.DebugHTLC && htlcPkt.Amount < invoice.Terms.Value {
				peerLog.Errorf("rejecting HTLC due to incorrect "+
					"amount: expected %v, received %v",
					invoice.Terms.Value, htlcPkt.Amount)
				state.htlcsToCancel[index] = lnwire.IncorrectValue
			} else {
				// Otherwise, everything is in order and we'll
				// settle the HTLC after the current state
				// transition.
				state.htlcsToSettle[index] = invoice
			}

		// There are additional hops left within this route, so we
		// track the next hop according to the index of this HTLC
		// within their log. When forwarding locked-in HLTC's to the
		// switch, we'll attach the routing information so the switch
		// can finalize the circuit.
		case sphinx.MoreHops:
			state.pendingCircuits[index] = sphinxPacket
		default:
			peerLog.Errorf("mal formed onion packet")
			state.htlcsToCancel[index] = lnwire.SphinxParseError
		}

	case *lnwire.UpdateFufillHTLC:
		pre := htlcPkt.PaymentPreimage
		idx := htlcPkt.ID
		if err := state.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			peerLog.Errorf("settle for outgoing HTLC rejected: %v", err)
			p.Disconnect()
			return
		}

		// TODO(roasbeef): add preimage to DB in order to swipe
		// repeated r-values
	case *lnwire.UpdateFailHTLC:
		idx := htlcPkt.ID
		if err := state.channel.ReceiveFailHTLC(idx); err != nil {
			peerLog.Errorf("unable to recv HTLC cancel: %v", err)
			p.Disconnect()
			return
		}

		state.cancelReasons[idx] = lnwire.FailCode(htlcPkt.Reason[0])

	case *lnwire.CommitSig:
		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		// TODO(roasbeef): redundant re-serialization
		sig := htlcPkt.CommitSig.Serialize()
		if err := state.channel.ReceiveNewCommitment(sig); err != nil {
			peerLog.Errorf("unable to accept new commitment: %v", err)
			p.Disconnect()
			return
		}

		// As we've just just accepted a new state, we'll now
		// immediately send the remote peer a revocation for our prior
		// state.
		nextRevocation, err := state.channel.RevokeCurrentCommitment()
		if err != nil {
			peerLog.Errorf("unable to revoke commitment: %v", err)
			return
		}
		p.queueMsg(nextRevocation, nil)

		// If both commitment chains are fully synced from our PoV,
		// then we don't need to reply with a signature as both sides
		// already have a commitment with the latest accepted state.
		if state.channel.FullySynced() {
			return
		}

		// Otherwise, the remote party initiated the state transition,
		// so we'll reply with a signature to provide them with their
		// version of the latest commitment state.
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update commitment: %v", err)
			p.Disconnect()
			return
		}

	case *lnwire.RevokeAndAck:
		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		htlcsToForward, err := state.channel.ReceiveRevocation(htlcPkt)
		if err != nil {
			peerLog.Errorf("unable to accept revocation: %v", err)
			p.Disconnect()
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
			if p, ok := state.clearedHTCLs[parentIndex]; ok {
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
					errMsg := state.cancelReasons[parentIndex]
					p.preImage <- [32]byte{}
					p.err <- errors.New(errMsg.String())
				}

				close(p.done)

				delete(state.clearedHTCLs, htlc.ParentIndex)
			}

			// TODO(roasbeef): rework log entries to a shared
			// interface.
			if htlc.EntryType != lnwallet.Add {
				continue
			}

			// If we can settle this HTLC within our local state
			// update log, then send the update entry to the remote
			// party.
			invoice, ok := state.htlcsToSettle[htlc.Index]
			if ok {
				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := state.channel.SettleHTLC(preimage)
				if err != nil {
					peerLog.Errorf("unable to settle htlc: %v", err)
					p.Disconnect()
					continue
				}

				settleMsg := &lnwire.UpdateFufillHTLC{
					ChanID:          state.chanID,
					ID:              logIndex,
					PaymentPreimage: preimage,
				}
				p.queueMsg(settleMsg, nil)

				delete(state.htlcsToSettle, htlc.Index)
				settledPayments[htlc.RHash] = struct{}{}

				bandwidthUpdate += htlc.Amount
				continue
			}

			// Alternatively, if we marked this HTLC for
			// cancellation, then immediately cancel the HTLC as
			// it's now locked in within both commitment
			// transactions.
			reason, ok := state.htlcsToCancel[htlc.Index]
			if !ok {
				continue
			}

			logIndex, err := state.channel.FailHTLC(htlc.RHash)
			if err != nil {
				peerLog.Errorf("unable to cancel htlc: %v", err)
				p.Disconnect()
				continue
			}

			cancelMsg := &lnwire.UpdateFailHTLC{
				ChanID: state.chanID,
				ID:     logIndex,
				Reason: []byte{byte(reason)},
			}
			p.queueMsg(cancelMsg, nil)
			delete(state.htlcsToCancel, htlc.Index)

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

				onionPkt := state.pendingCircuits[htlc.Index]
				delete(state.pendingCircuits, htlc.Index)

				reason := state.cancelReasons[htlc.ParentIndex]
				delete(state.cancelReasons, htlc.ParentIndex)

				// Send this fully activated HTLC to the htlc
				// switch to continue the chained clear/settle.
				pkt, err := logEntryToHtlcPkt(state.chanID,
					htlc, onionPkt, reason)
				if err != nil {
					peerLog.Errorf("unable to make htlc pkt: %v",
						err)
					continue
				}

				state.switchChan <- pkt
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
		if err := p.updateCommitTx(state); err != nil {
			peerLog.Errorf("unable to update commitment: %v", err)
			p.Disconnect()
			return
		}

		// Notify the invoiceRegistry of the invoices we just settled
		// with this latest commitment update.
		// TODO(roasbeef): wait until next transition?
		for invoice := range settledPayments {
			err := p.server.invoices.SettleInvoice(chainhash.Hash(invoice))
			if err != nil {
				peerLog.Errorf("unable to settle invoice: %v", err)
			}
		}
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (p *peer) updateCommitTx(state *commitmentState) error {
	sigTheirs, err := state.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		peerLog.Tracef("revocation window exhausted, unable to send %v",
			len(state.pendingBatch))
		return nil
	} else if err != nil {
		return err
	}

	parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
	if err != nil {
		return fmt.Errorf("unable to parse sig: %v", err)
	}

	commitSig := &lnwire.CommitSig{
		ChanID:    state.chanID,
		CommitSig: parsedSig,
	}
	p.queueMsg(commitSig, nil)

	// As we've just cleared out a batch, move all pending updates to the
	// map of cleared HTLCs, clearing out the set of pending updates.
	for _, update := range state.pendingBatch {
		state.clearedHTCLs[update.index] = update
	}

	// Finally, clear our the current batch, and flip the pendingUpdate
	// bool to indicate were waiting for a commitment signature.
	// TODO(roasbeef): re-slice instead to avoid GC?
	state.pendingBatch = nil

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

	pkt.amt = pd.Amount
	pkt.msg = msg

	pkt.srcLink = chanID
	pkt.onion = onionPkt

	return pkt, nil
}
