package htlcswitch

import (
	"crypto/sha256"
	"fmt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"sync"
	"time"
)

// InvoiceRegistry is an interface which represents the system which may
// search and settle invoices. Such interface abstraction helps as to
// create simple mock representation of this subsystem in unit tests.
// TODO(andrew.shvv) should be moved in other place.
type InvoiceRegistry interface {
	AddInvoice(*channeldb.Invoice) error
	LookupInvoice(chainhash.Hash) (*channeldb.Invoice, error)
	SettleInvoice(chainhash.Hash) error
}

// Peer is an interface which represents the remote lightning node inside our
// system. Such interface abstraction helps as to create simple
// mock representation of this subsystem in unit tests.
// TODO(andrew.shvv) should be moved in other place.
type Peer interface {
	// SendMessage sends message to current peer.
	SendMessage(lnwire.Message) error

	// WipeChannel removes the passed channel from all indexes associated
	// with the peer, and deletes the channel from the database.
	WipeChannel(*lnwallet.LightningChannel) error

	// ID is a lightning network peer id.
	ID() [sha256.Size]byte

	// HopID is id which represent node in terms of routing.
	HopID() *routing.HopID

	// Disconnect disconnects peer if we have error which we can;t
	// properly handle.
	Disconnect()
}

// HTLCManager is an interface which represents the subsystem for managing
// the incoming HTLC requests, applying the changes to the channel, and also
// propagating the HTLCs to HTLC switch if needed.
// NOTE: Such interface abstraction helps as to create simple mock
// representation of this  subsystem in unit tests.
type HTLCManager interface {
	// HandleRequest handles the switch requests which forwarded to us
	// from another peer.
	HandleRequest(*SwitchRequest) error

	// HandleMessage handles the htlc requests which sent to us from remote
	// peer.
	HandleMessage(lnwire.Message) error

	// Bandwidth return the amount of satohis which this HTLC manager can
	// work with at this time.
	Bandwidth() btcutil.Amount

	// SatSent returns the amount of satoshis which was successfully sent
	// and settled in channel by this manager.
	SatSent() btcutil.Amount

	// SatRecv returns the amount of satoshi which was successfully
	// received and settled in channel by this manager.
	SatRecv() btcutil.Amount

	// NumUpdates return the number of updates which was applied to
	// managed the channel.
	NumUpdates() uint64

	// ID return the id of the managed channel.
	ID() *wire.OutPoint

	// PeerID return the id of peer to which managed channel belongs.
	PeerID() [sha256.Size]byte

	// HopID return the id which is used within HTLC switch to route
	// the HTLC requests.
	HopID() *routing.HopID

	// Start is used to start the HTLC manager: receive incoming requests,
	// handle the channel notification, and also print some statistics.
	Start() error
	Stop()
}

// handleMessageCommand encapsulates peer HTLC message and adds error channel to
// receive response from message handler.
type handleMessageCommand struct {
	message lnwire.Message
	err     chan error
}

// handleRequestCommand encapsulates switch request and adds error channel to
// receive response from request handler.
type handleRequestCommand struct {
	request *SwitchRequest
	err     chan error
}

// HTLCManagerConfig defines the configuration for the htlcManager. ALL elements
// within the configuration MUST be non-nil for the htlcManager to carry out its
// duties.
type HTLCManagerConfig struct {
	// DecodeOnion function responsible for decoding HTLC onion blob, and
	// create hop iterator which gives us next route hop. This function
	// is included in config because of test purpose where instead of sphinx
	// encoded route we use simple array of hops.
	DecodeOnion func(data []byte, meta []byte) (routing.HopIterator, error)

	// Forward is a function which is used to forward the incoming HTLC
	// requests to other peer which should handle it.
	Forward func(*SwitchRequest) error

	// Peer is a lightning network node with which we have create managed
	// channel.
	Peer Peer

	// Registry is a sub-system which responsible for managing the
	// invoices set in thread-safe manner .
	Registry InvoiceRegistry

	// SettledContracts is used to notify the breachArbiter that a channel
	// has peacefully been closed. Once a channel has been closed the
	// arbiter no longer needs to watch for breach closes.
	SettledContracts chan *wire.OutPoint

	// DebugHTLC should be turned on if you want all HTLCs sent to a node
	// with the debug HTLC R-Hash are immediately settled in the next
	// available state transition.
	DebugHTLC bool
}

// htlcManager is the service which drives a channel's commitment update
// state-machine in response to messages received from remote peer or
// forwarded to as from HTLC switch. In the event that an HTLC needs to be
// forwarded, then forward handler is used which sends HTLC to the switch for
// forwarding. Additionally, the htlcManager encapsulate logic of commitment
// protocol message ordering and updates.
type htlcManager struct {
	service *Service

	// cfg is a structure which carries all dependable fields/handler
	// which may affect behaviour of thi service.
	cfg *HTLCManagerConfig

	// notSettleHTLCs is a map of outgoing HTLC's we've committed to in
	// our chain which have not yet been settled by the peer.
	notSettleHTLCs map[uint32]*SwitchRequest

	// updateMutex is used to be sure in thread safeness of update function
	// behaviour.
	updateMutex sync.Mutex

	// updateTimer is used to postpone the execution of update commitment.
	updateTimer *time.Timer

	// cancelReasons stores the reason why a particular HTLC was cancelled.
	// The index of the HTLC within the log is mapped to the cancellation
	// reason. This value is used to thread the proper error through to the
	// htlcSwitch, or subsystem that initiated the HTLC.
	cancelReasons map[uint32]lnwire.CancelReason

	// blobs tracks the remote log index of the incoming HTLC's,
	// mapped to the htlc blob which encapsulate next hop.
	blobs map[uint32][]byte

	// channel is a lightning network channel to which we apply htlc requests.
	channel *lnwallet.LightningChannel

	// commands is a channel which used for handling the inner system
	// requests.
	commands chan interface{}
}

// A compile time check to ensure htlcManager implements the
// HTLCManager interface.
var _ HTLCManager = (*htlcManager)(nil)

// NewHTLCManager create new instance of htlc manager.
func NewHTLCManager(cfg *HTLCManagerConfig,
	channel *lnwallet.LightningChannel) HTLCManager {

	name := fmt.Sprintf("HTLC manager(%v)", channel.ChannelPoint())
	return &htlcManager{
		channel:        channel,
		cfg:            cfg,
		service:        NewService(name),
		notSettleHTLCs: make(map[uint32]*SwitchRequest),
		blobs:          make(map[uint32][]byte),
		commands:       make(chan interface{}),
		cancelReasons:  make(map[uint32]lnwire.CancelReason),
	}
}

// HandleMessage handles the htlc requests which sent to us form remote peer.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) HandleMessage(message lnwire.Message) error {
	command := &handleMessageCommand{
		message: message,
		err:     make(chan error),
	}

	select {
	case mgr.commands <- command:
		err := <-command.err
		if err != nil {
			log.Errorf("error while message handling in %v"+
				": %v", mgr.service.Name, err)
			mgr.cfg.Peer.Disconnect()
		}
		return err
	case <-mgr.service.Quit:
		return nil
	}
}

// handleMessage handles the remote peer messages.
func (mgr *htlcManager) handleMessage(message lnwire.Message) error {
	switch msg := message.(type) {

	case *lnwire.CancelHTLC:
		htlc := msg

		idx := uint32(htlc.HTLCKey)
		if err := mgr.channel.ReceiveCancelHTLC(idx); err != nil {
			return errors.Errorf("unable to recv HTLC cancel: %v", err)
		}

		// As far as we send cancellation message only after HTLC will
		// be included we should save the reason of HTLC cancellation
		// and then use it later to notify user or propagate cancel HTLC
		// message to another peer over htlc switch.
		mgr.cancelReasons[idx] = htlc.Reason
		mgr.updateCommitTx(false)

	case *lnwire.HTLCAddRequest:
		htlc := msg

		// We just received an add request from an remote peer, so we
		// add it to our state machine.
		index, err := mgr.channel.ReceiveHTLC(htlc)
		if err != nil {
			return errors.Errorf("receiving HTLC rejected: %v", err)
		}

		// TODO(roasbeef): perform sanity checks on per-hop payload
		//  * time-lock is sane, fee, chain, etc

		// Store the onion blob which encapsulate the HTLC route and
		// use in on stage of HTLC inclusion to propagate the HTLC
		// farther.
		mgr.blobs[index] = htlc.OnionBlob
		mgr.updateCommitTx(false)

	case *lnwire.HTLCSettleRequest:
		htlc := msg

		// TODO(roasbeef): this assumes no "multi-sig"
		pre := htlc.RedemptionProofs[0]
		idx := uint32(htlc.HTLCKey)
		if err := mgr.channel.ReceiveHTLCSettle(pre, idx); err != nil {
			// TODO(roasbeef): broadcast on-chain
			return errors.Errorf("settle for outgoing HTLC rejected: %v", err)
		}

		mgr.updateCommitTx(false)

	// TODO(roasbeef): add pre-image to DB in order to swipe
	// repeated r-values
	case *lnwire.CommitSignature:
		commit := msg

		// We just received a new update to our local commitment chain,
		// validate this new commitment, closing the link if invalid.
		// TODO(roasbeef): use uint64 for indexes?
		index := uint32(commit.LogIndex)
		sig := commit.CommitSig.Serialize()
		if err := mgr.channel.ReceiveNewCommitment(sig, index); err != nil {
			return errors.Errorf("unable to accept new commitment: %v", err)
		}

		// Update commitment transaction without delay.
		if err := mgr.updateCommitTx(true); err != nil {
			return err
		}

		// Finally, since we just accepted a new state, send the remote
		// peer a revocation for our prior cm.
		revocation, err := mgr.channel.RevokeCurrentCommitment()
		if err != nil {
			return errors.Errorf("unable to revoke current commitment: "+
				"%v", err)
		}
		mgr.cfg.Peer.SendMessage(revocation)

	case *lnwire.CommitRevocation:
		revocation := msg

		// We've received a revocation from the remote chain, if valid,
		// this moves the remote chain forward, and expands our
		// revocation window.
		htlcs, err := mgr.channel.ReceiveRevocation(revocation)
		if err != nil {
			return errors.Errorf("unable to accept revocation: %v", err)
		}

		// After we treat HTLCs as included in both
		// remote/local commitment transactions they might be
		// safely propagated over HTLC switch or settled if our node was
		// last node in HTLC path.
		requestsToForward, err := mgr.processIncludedHTLCs(htlcs)
		if err != nil {
			return errors.Errorf("unbale procees included htlcs: %v", err)
		}

		go func() {
			for _, request := range requestsToForward {
				err := mgr.cfg.Forward(request)
				if err != nil {
					log.Errorf("error while forward htlc "+
						"over htlc switch: %v", err)
				}
			}
		}()

	default:
		return errors.New("unknown message type")
	}

	return nil

}

// HandleMessage handles the HTLC requests which sent to us from remote peer.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) HandleRequest(request *SwitchRequest) error {
	command := &handleRequestCommand{
		request: request,
		err:     make(chan error),
	}

	select {
	case mgr.commands <- command:
		err := <-command.err
		if err != nil {
			log.Errorf("error while request handling in %v:"+
				" %v", mgr.service.Name, err)
		}
		return err
	case <-mgr.service.Quit:
		return nil
	}
}

// handleRequest handles HTLC switch requests which was forwarded to us from
// another channel, or sent to us from user who wants to send the payment.
func (mgr *htlcManager) handleRequest(request *SwitchRequest) error {
	switch htlc := request.Htlc.(type) {
	case *lnwire.HTLCAddRequest:
		// A new payment has been initiated, so we add the new HTLC
		// to our local log and the send it remote peer.
		htlc.ChannelPoint = mgr.ID()
		index, err := mgr.channel.AddHTLC(htlc)
		if err != nil {
			// TODO: possibly perform fallback/retry logic
			// depending on type of error
			return errors.Errorf("adding HTLC rejected: %v", err)
		}

		mgr.notSettleHTLCs[index] = request
		mgr.cfg.Peer.SendMessage(htlc)

	case *lnwire.HTLCSettleRequest:
		// HTLC switch notified us that HTLC which we forwarded was
		// settled, so we need to propagate this htlc to remote
		// peer.

		preimage := htlc.RedemptionProofs[0]
		index, err := mgr.channel.SettleHTLC(preimage)
		if err != nil {
			// TODO(roasbeef): broadcast on-chain
			mgr.cfg.Peer.Disconnect()
			return errors.Errorf("settle for incoming HTLC rejected: %v", err)
		}

		htlc.ChannelPoint = mgr.ID()
		htlc.HTLCKey = lnwire.HTLCKey(index)

		mgr.cfg.Peer.SendMessage(htlc)

	case *lnwire.CancelHTLC:
		// HTLC switch notified us that HTLC which we forwarded was
		// canceled, so we need to propagate this htlc to remote
		// peer.
		mgr.sendHTLCError(request.PayHash, htlc.Reason)
	}

	return nil
}

// Start starts all helper goroutines required for the operation of the HTLC
// manager.
func (mgr *htlcManager) Start() error {
	if err := mgr.service.Start(); err != nil {
		log.Warn(err)
		return nil
	}

	mgr.service.Go(mgr.startHandleNotifications)
	mgr.service.Go(mgr.startHandleCommands)

	// A new session for this active channel has just started, therefore we
	// need to send our initial revocation window to the remote peer.
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		revocation, err := mgr.channel.ExtendRevocationWindow()
		if err != nil {
			log.Errorf("unable to expand revocation window: %v", err)
			continue
		}
		mgr.cfg.Peer.SendMessage(revocation)
	}

	return nil
}

// Stop gracefully stops all active helper goroutines, then waits until they've
// exited.
func (mgr *htlcManager) Stop() {
	if err := mgr.service.Stop(nil); err != nil {
		log.Warn(err)
	}
}

// Wait waits for service to stop.
// NOTE: This function is separated from Stop function because of deadlock
// possibility - when in handler itself we trigger Stop function of this
// service it leads to deadlock. (for example in WipeChannel function)
func (mgr *htlcManager) Wait() error {
	return mgr.service.Wait()
}

// handleCommands handles the inner service commands.
func (mgr *htlcManager) startHandleCommands() {
	defer mgr.service.Done()

	for {
		select {
		case command := <-mgr.commands:
			switch r := command.(type) {
			case *handleMessageCommand:
				r.err <- mgr.handleMessage(r.message)
			case *handleRequestCommand:
				r.err <- mgr.handleRequest(r.request)
			}
		case <-mgr.service.Quit:
			return
		}
	}
}

// handleNotifications handles the channel closing notifications.
func (mgr *htlcManager) startHandleNotifications() {
	defer mgr.service.Done()
	// TODO(roasbeef): check to see if able to settle any currently pending
	// HTLC's
	//   * also need signals when new invoices are added by the invoiceRegistry

	for {
		select {
		case <-mgr.channel.UnilateralCloseSignal:
			// TODO(roasbeef): need to send HTLC outputs to nursery
			log.Warnf("Remote peer has closed ChannelPoint(%v) "+
				"on-chain",
				mgr.ID())
			if err := mgr.cfg.Peer.WipeChannel(mgr.channel); err !=
				nil {
				log.Errorf("unable to wipe channel %v", err)
			}

			mgr.cfg.SettledContracts <- mgr.channel.ChannelPoint()
			return

		case <-mgr.channel.ForceCloseSignal:
			log.Warnf("ChannelPoint(%v) has been force "+
				"closed, disconnecting from peerID(%x)",
				mgr.ID(), mgr.PeerID())
			return

		case <-mgr.service.Quit:
			return
		}
	}
}

// processIncludedHTLCs this function is used to proceed the HTLCs which was
// designated as eligible for forwarding. But not all HTLC will be forwarder,
// if HTLC reached its final destination that we should settle it.
func (mgr *htlcManager) processIncludedHTLCs(
	paymentDescriptors []*lnwallet.PaymentDescriptor) ([]*SwitchRequest,
	error) {

	var requestsToForward []*SwitchRequest
	for _, pd := range paymentDescriptors {
		// TODO(roasbeef): rework log entries to a shared
		// interface.
		switch pd.EntryType {

		case lnwallet.Settle:
			// Trying to find the pending HTLC to which this
			// settle HTLC belongs.
			request, ok := mgr.notSettleHTLCs[pd.ParentIndex]
			if !ok {
				continue
			}
			delete(mgr.notSettleHTLCs, pd.ParentIndex)

			switch request.Type {
			case UserAddRequest:
				// Notify user that his payment was
				// successfully proceed.
				request.Error() <- nil

			case ForwardAddRequest:
				// If this request came from switch that we
				// should forward settle message back to peer.
				requestsToForward = append(requestsToForward,
					NewForwardSettleRequest(
						mgr.ID(),
						&lnwire.HTLCSettleRequest{
							RedemptionProofs: [][32]byte{
								pd.RPreimage,
							},
						}))
			}

		case lnwallet.Cancel:
			request, ok := mgr.notSettleHTLCs[pd.ParentIndex]
			if !ok {
				continue
			}
			delete(mgr.notSettleHTLCs, pd.ParentIndex)
			reason := mgr.cancelReasons[pd.ParentIndex]

			switch request.Type {
			case UserAddRequest:
				// Notify user that his payment was canceled.
				request.Error() <- errors.New(reason.String())

			case ForwardAddRequest:
				// If this request came from switch that we
				// should forward cancel message back to peer.
				requestsToForward = append(requestsToForward,
					NewCancelRequest(
						mgr.ID(),
						&lnwire.CancelHTLC{
							Reason:       reason,
							ChannelPoint: mgr.ID(),
							HTLCKey:      lnwire.HTLCKey(pd.Index),
						},
						pd.RHash))
			}

		case lnwallet.Add:
			blob := mgr.blobs[pd.Index]
			delete(mgr.blobs, pd.Index)

			// Before adding the new HTLC to the state machine,
			// parse the onion object in order to obtain the routing
			// information with DecodeOnion function which process
			// the Sphinx packet.
			// We include the payment hash of the HTLC as it's
			// authenticated within the Sphinx packet itself as
			// associated data in order to thwart attempts a replay
			// attacks. In the case of a replay, an attacker is
			// *forced* to use the same payment hash twice, thereby
			// losing their money entirely.
			hopIterator, err := mgr.cfg.DecodeOnion(blob, pd.RHash[:])
			if err != nil {
				// If we're unable to parse the Sphinx packet,
				// then we'll cancel the HTLC.
				log.Errorf("unable to get the next hop: %v", err)
				mgr.sendHTLCError(pd.RHash, lnwire.SphinxParseError)
				continue
			}

			if dest := hopIterator.Next(); dest != nil {
				// There are additional hops left within this
				// route.
				nextBlob, err := hopIterator.ToBytes()
				if err != nil {
					log.Errorf("unable to encode the hop "+
						"iterator: %v", err)
					continue
				}

				requestsToForward = append(requestsToForward,
					NewForwardAddRequest(
						dest, mgr.ID(),
						&lnwire.HTLCAddRequest{
							Amount:           pd.Amount,
							RedemptionHashes: [][32]byte{pd.RHash},
							OnionBlob:        nextBlob,
						}))
			} else {
				// We're the designated payment destination.
				// Therefore we attempt to see if we have an
				// invoice locally which'll allow us to settle
				// this HTLC.
				invoiceHash := chainhash.Hash(pd.RHash)
				invoice, err := mgr.cfg.Registry.LookupInvoice(invoiceHash)
				if err != nil {
					log.Errorf("unable to query to locate:"+
						" %v", err)
					mgr.sendHTLCError(pd.RHash, lnwire.UnknownPaymentHash)
					continue
				}

				// If we're not currently in debug mode, and the
				// extended HTLC doesn't meet the value requested,
				// then we'll fail the HTLC. Otherwise, we settle
				// this HTLC within our local state update log,
				// then send the update entry to the remote party.
				if !mgr.cfg.DebugHTLC && pd.Amount < invoice.Terms.Value {
					log.Errorf("rejecting HTLC due to incorrect "+
						"amount: expected %v, received %v",
						invoice.Terms.Value, pd.Amount)
					mgr.sendHTLCError(pd.RHash, lnwire.IncorrectValue)
					continue
				}

				preimage := invoice.Terms.PaymentPreimage
				logIndex, err := mgr.channel.SettleHTLC(preimage)
				if err != nil {
					return nil, errors.Errorf("unable to "+
						"settle htlc: %v", err)
				}

				// Notify the invoiceRegistry of the invoices we
				// just settled with this latest commitment
				// update.
				err = mgr.cfg.Registry.SettleInvoice(invoiceHash)
				if err != nil {
					return nil, errors.Errorf("unable to "+
						"settle invoice: %v", err)
				}

				// HTLC was successfully settled locally send
				// notification about it remote peer.
				mgr.cfg.Peer.SendMessage(&lnwire.HTLCSettleRequest{
					ChannelPoint:     mgr.ID(),
					HTLCKey:          lnwire.HTLCKey(logIndex),
					RedemptionProofs: [][32]byte{preimage},
				})
			}
		}
	}

	return requestsToForward, nil
}

// sendHTLCError functions cancels HTLC and send cancel message back to the
// peer from which HTLC was received.
func (mgr *htlcManager) sendHTLCError(rHash [32]byte,
	reason lnwire.CancelReason) {

	index, err := mgr.channel.CancelHTLC(rHash)
	if err != nil {
		log.Errorf("can't cancel htlc: %v", err)
		return
	}

	mgr.cfg.Peer.SendMessage(&lnwire.CancelHTLC{
		ChannelPoint: mgr.ID(),
		HTLCKey:      lnwire.HTLCKey(index),
		Reason:       reason,
	})
}

// Bandwidth returns the amount of satohis which this HTLC manager can
// handle at this time.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) Bandwidth() btcutil.Amount {
	snapshot := mgr.channel.StateSnapshot()
	return snapshot.LocalBalance
}

// SatSent returns the amount of satoshis which was successfully sent
// and settled.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) SatSent() btcutil.Amount {
	snapshot := mgr.channel.StateSnapshot()
	return btcutil.Amount(snapshot.TotalSatoshisSent)
}

// SatRecv returns the amount of satoshis which was successfully
// received and settled.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) SatRecv() btcutil.Amount {
	snapshot := mgr.channel.StateSnapshot()
	return btcutil.Amount(snapshot.TotalSatoshisReceived)
}

// NumUpdates returns the number of updates which was applied to channel
// which corresponds to this HTLC manager.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) NumUpdates() uint64 {
	snapshot := mgr.channel.StateSnapshot()
	return snapshot.NumUpdates
}

// ID returns the id of the htlc manager which is equivalent to channel
// point the managed channel.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) ID() *wire.OutPoint {
	return mgr.channel.ChannelPoint()
}

// PeerID is an id of the peer to which lightning channel is belongs.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) PeerID() [sha256.Size]byte {
	return mgr.cfg.Peer.ID()
}

// HopID is an id of the peer which is used by htlc switch in order to properly
// propagate the htlc requests.
// NOTE: Part of the HTLCManager interface.
func (mgr *htlcManager) HopID() *routing.HopID {
	return mgr.cfg.Peer.HopID()
}

// updateCommitTx signs, then sends an update to the remote peer adding a new
// commitment to their commitment chain which includes all the latest updates
// we've received+processed up to this point. This function have two mode:
// * Delayed mode - used to delay commitment update process when we receive HTLC
// 	update, it reduces the number of commitment update message between nodes.
//	If number of HTLCs was received update period than only one update
// 	will be send.
// * Immediate mode - used when we need send commit sig in sync manner.
//
//
// htlc arrived
// and delayed (T)
// channel state
// update was			 update channel state
// initialised 				   +
//    +					   |
//    |					   |
//  --x----x-------------------------------x----------> t
//    |	   |				   |
//    |    +				   |
//    | another htlc arrived		   |
//    | but update already 		   |
//    | initiated			   |
//    |		   		   	   |
//    | <--------------------------------> |
//			T
//
func (mgr *htlcManager) updateCommitTx(immediately bool) error {
	mgr.updateMutex.Lock()
	defer mgr.updateMutex.Unlock()

	update := func() error {
		if !mgr.channel.NeedUpdate() {
			return nil
		}

		sigTheirs, logIndexTheirs, err := mgr.channel.SignNextCommitment()
		if err == lnwallet.ErrNoWindow {
			log.Trace(err)
		} else if err != nil {
			return errors.Errorf("unable to update commitment: %v", err)
		}

		parsedSig, err := btcec.ParseSignature(sigTheirs, btcec.S256())
		if err != nil {
			return errors.Errorf("unable to update commitment: %v", err)
		}

		commitSig := &lnwire.CommitSignature{
			ChannelPoint: mgr.ID(),
			CommitSig:    parsedSig,
			LogIndex:     uint64(logIndexTheirs),
		}

		if err := mgr.cfg.Peer.SendMessage(commitSig); err != nil {
			return errors.Errorf("unable to update commitment: %v", err)
		}

		return nil
	}

	if immediately {
		// We should stop the update commit tx timer if it was
		// initialized before.
		if mgr.updateTimer != nil {
			if mgr.updateTimer.Stop() {
				mgr.updateTimer = nil
			} else {
				return errors.New("can't stop update commit " +
					"tx timer")
			}
		}

		return update()

	} else {
		// If timer wasn't set before than initialize new delayed
		// commit tx update, otherwise wait for previously initialized
		// update to be triggered.
		if mgr.updateTimer == nil {
			mgr.updateTimer = time.NewTimer(10 * time.Millisecond)
			go func() {
				<-mgr.updateTimer.C

				mgr.updateMutex.Lock()
				if err := update(); err != nil {
					log.Error(err)
					mgr.cfg.Peer.Disconnect()
				}
				mgr.updateTimer = nil
				mgr.updateMutex.Unlock()
			}()
		}
	}

	return nil
}
