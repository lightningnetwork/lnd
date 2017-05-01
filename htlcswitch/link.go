package htlcswitch

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"io"

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

	// DecodeOnion function responsible for decoding htlc Sphinx onion blob,
	// and creating hop iterator which will give us next destination of htlc.
	DecodeOnion func(r io.Reader, meta []byte) (HopIterator, error)

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
	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	// TODO(andrew.shvv) remove after payment descriptor start store
	// htlc cancel reasons.
	cancelReasons map[uint64]lnwire.OpaqueReason

	// blobs tracks the remote log index of the incoming htlc's,
	// mapped to the htlc onion blob which encapsulates the next hop.
	// TODO(andrew.shvv) remove after payment descriptor start store
	// htlc onion blobs.
	blobs map[uint64][lnwire.OnionPacketSize]byte

	// batchCounter is the number of updates which we received from
	// remote side, but not include in commitment transaciton yet.
	// TODO(andrew.shvv) remove after we add additional
	// BatchNumber() method in state machine.
	batchCounter uint64

	// channel is a lightning network channel to which we apply htlc
	// updates.
	channel *lnwallet.LightningChannel

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

// NewChannelLink create new instance of channel link.
func NewChannelLink(cfg *ChannelLinkConfig,
	channel *lnwallet.LightningChannel) ChannelLink {

	return &channelLink{
		cfg:           cfg,
		channel:       channel,
		blobs:         make(map[uint64][lnwire.OnionPacketSize]byte),
		upstream:      make(chan lnwire.Message),
		downstream:    make(chan *htlcPacket),
		control:       make(chan interface{}),
		cancelReasons: make(map[uint64]lnwire.OpaqueReason),
		quit:          make(chan struct{}),
	}
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
			if l.channel.FullySynced() {
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
			if l.batchCounter == 0 {
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
		l.batchCounter++

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
	if l.batchCounter >= 10 || isSettle {
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

		// Store the onion blob which encapsulate the htlc route and
		// use in on stage of htlc inclusion to retrieve the
		// next hope and propagate the htlc farther.
		l.blobs[index] = msg.OnionBlob

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

		l.cancelReasons[idx] = msg.Reason

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
		htlcs, err := l.channel.ReceiveRevocation(msg)
		if err != nil {
			log.Errorf("unable to accept revocation: %v", err)
			l.cfg.Peer.Disconnect()
			return
		}

		// After we treat HTLCs as included in both
		// remote/local commitment transactions they might be
		// safely propagated over htlc switch or settled if our node was
		// last node in htlc path.
		htlcsToForward := l.processLockedInHtlcs(htlcs)
		go func() {
			for _, packet := range htlcsToForward {
				if err := l.cfg.Switch.forward(packet); err != nil {
					log.Errorf("channel link(%v): "+
						"unhandled error while forwarding "+
						"htlc packet over htlc  "+
						"switch: %v", l, err)
				}
			}
		}()
	}
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point.
func (l *channelLink) updateCommitTx() error {
	sigTheirs, err := l.channel.SignNextCommitment()
	if err == lnwallet.ErrNoWindow {
		log.Tracef("revocation window exhausted, unable to send %v",
			l.batchCounter)
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

	l.batchCounter = 0
	return nil
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

// processLockedInHtlcs function is used to proceed the HTLCs which was
// designated as eligible for forwarding. But not all htlc will be
// forwarder, if htlc reached its final destination that we should settle it.
func (l *channelLink) processLockedInHtlcs(
	paymentDescriptors []*lnwallet.PaymentDescriptor) []*htlcPacket {

	var needUpdate bool

	var packetsToForward []*htlcPacket
	for _, pd := range paymentDescriptors {
		// TODO(roasbeef): rework log entries to a shared
		// interface.
		switch pd.EntryType {

		case lnwallet.Settle:
			// forward message to switch which will decide does
			// this peer is the final destination of htlc and we
			// should notify user about successful income or it
			// should be propagated back to the origin peer.
			packetsToForward = append(packetsToForward,
				newSettlePacket(l.ChanID(),
					&lnwire.UpdateFufillHTLC{
						PaymentPreimage: pd.RPreimage,
					}, pd.RHash, pd.Amount))

		case lnwallet.Fail:
			opaqueReason := l.cancelReasons[pd.ParentIndex]

			// forward message to switch which will decide does
			// this peer is the final destination of htlc and we
			// should notify user about fail income or it
			// should be propagated back to the origin peer.
			packetsToForward = append(packetsToForward,
				newFailPacket(l.ChanID(),
					&lnwire.UpdateFailHTLC{
						Reason: opaqueReason,
						ChanID: l.ChanID(),
					}, pd.RHash, pd.Amount))

		case lnwallet.Add:
			blob := l.blobs[pd.Index]
			buffer := bytes.NewBuffer(blob[:])
			delete(l.blobs, pd.Index)

			// Before adding the new htlc to the state machine,
			// parse the onion object in order to obtain the routing
			// information with DecodeOnion function which process
			// the Sphinx packet.
			// We include the payment hash of the htlc as it's
			// authenticated within the Sphinx packet itself as
			// associated data in order to thwart attempts a replay
			// attacks. In the case of a replay, an attacker is
			// *forced* to use the same payment hash twice, thereby
			// losing their money entirely.
			chanIterator, err := l.cfg.DecodeOnion(buffer, pd.RHash[:])
			if err != nil {
				// If we're unable to parse the Sphinx packet,
				// then we'll cancel the htlc.
				log.Errorf("unable to get the next hop: %v", err)
				reason := []byte{byte(lnwire.SphinxParseError)}
				l.sendHTLCError(pd.RHash, reason)
				needUpdate = true
				continue
			}

			if nextChan := chanIterator.Next(); nextChan != nil {
				// There are additional channels left within this
				// route.
				var b bytes.Buffer
				var blob [lnwire.OnionPacketSize]byte
				err := chanIterator.Encode(&b)
				if err != nil {
					log.Errorf("unable to encode the "+
						"remaining route %v", err)
					reason := []byte{byte(lnwire.UnknownError)}
					l.sendHTLCError(pd.RHash, reason)
					needUpdate = true
					continue
				}
				copy(blob[:], b.Bytes())

				packetsToForward = append(packetsToForward,
					newAddPacket(l.ChanID(), *nextChan,
						&lnwire.UpdateAddHTLC{
							Amount:      pd.Amount,
							PaymentHash: pd.RHash,
							OnionBlob:   blob,
						}))
			} else {
				// We're the designated payment destination.
				// Therefore we attempt to see if we have an
				// invoice locally which'll allow us to settle
				// this htlc.
				invoiceHash := chainhash.Hash(pd.RHash)
				invoice, err := l.cfg.Registry.LookupInvoice(invoiceHash)
				if err != nil {
					log.Errorf("unable to query to locate:"+
						" %v", err)
					reason := []byte{byte(lnwire.UnknownPaymentHash)}
					l.sendHTLCError(pd.RHash, reason)
					needUpdate = true
					continue
				}

				// If we're not currently in debug mode, and the
				// extended htlc doesn't meet the value requested,
				// then we'll fail the htlc. Otherwise, we settle
				// this htlc within our local state update log,
				// then send the update entry to the remote party.
				if !l.cfg.DebugHTLC && pd.Amount < invoice.Terms.Value {
					log.Errorf("rejecting htlc due to incorrect "+
						"amount: expected %v, received %v",
						invoice.Terms.Value, pd.Amount)
					reason := []byte{byte(lnwire.IncorrectValue)}
					l.sendHTLCError(pd.RHash, reason)
					needUpdate = true
					continue
				}

				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := l.channel.SettleHTLC(preimage)
				if err != nil {
					log.Errorf("unable to settle htlc: %v", err)
					l.cfg.Peer.Disconnect()
					return nil
				}

				// Notify the invoiceRegistry of the invoices we
				// just settled with this latest commitment
				// update.
				err = l.cfg.Registry.SettleInvoice(invoiceHash)
				if err != nil {
					log.Errorf("unable to settle invoice: %v", err)
					l.cfg.Peer.Disconnect()
					return nil
				}

				// htlc was successfully settled locally send
				// notification about it remote peer.
				l.cfg.Peer.SendMessage(&lnwire.UpdateFufillHTLC{
					ChanID:          l.ChanID(),
					ID:              logIndex,
					PaymentPreimage: preimage,
				})
				needUpdate = true
			}
		}
	}

	if needUpdate {
		// With all the settle/cancel updates added to the local and
		// remote htlc logs, initiate a state transition by updating the
		// remote commitment chain.
		if err := l.updateCommitTx(); err != nil {
			log.Errorf("unable to update commitment: %v", err)
			l.cfg.Peer.Disconnect()
			return nil
		}
	}

	return packetsToForward
}

// sendHTLCError functions cancels htlc and send cancel message back to the
// peer from which htlc was received.
func (l *channelLink) sendHTLCError(rHash [32]byte,
	reason lnwire.OpaqueReason) {

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
