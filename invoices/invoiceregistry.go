package invoices

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	// DebugPre is the default debug preimage which is inserted into the
	// invoice registry if the --debughtlc flag is activated on start up.
	// All nodes initialized with the flag active will immediately settle
	// any incoming HTLC whose rHash corresponds with the debug
	// preimage.
	DebugPre, _ = lntypes.NewPreimage(bytes.Repeat([]byte{1}, 32))

	// DebugHash is the hash of the default preimage.
	DebugHash = DebugPre.Hash()
)

// InvoiceRegistry is a central registry of all the outstanding invoices
// created by the daemon. The registry is a thin wrapper around a map in order
// to ensure that all updates/reads are thread safe.
type InvoiceRegistry struct {
	sync.RWMutex

	cdb *channeldb.DB

	clientMtx                 sync.Mutex
	nextClientID              uint32
	notificationClients       map[uint32]*InvoiceSubscription
	singleNotificationClients map[uint32]*SingleInvoiceSubscription

	newSubscriptions       chan *InvoiceSubscription
	newSingleSubscriptions chan *SingleInvoiceSubscription
	subscriptionCancels    chan uint32
	invoiceEvents          chan *invoiceEvent

	// debugInvoices is a map which stores special "debug" invoices which
	// should be only created/used when manual tests require an invoice
	// that *all* nodes are able to fully settle.
	debugInvoices map[lntypes.Hash]*channeldb.Invoice

	activeNetParams *chaincfg.Params

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewRegistry creates a new invoice registry. The invoice registry
// wraps the persistent on-disk invoice storage with an additional in-memory
// layer. The in-memory layer is in place such that debug invoices can be added
// which are volatile yet available system wide within the daemon.
func NewRegistry(cdb *channeldb.DB,
	activeNetParams *chaincfg.Params) *InvoiceRegistry {

	return &InvoiceRegistry{
		cdb:                       cdb,
		debugInvoices:             make(map[lntypes.Hash]*channeldb.Invoice),
		notificationClients:       make(map[uint32]*InvoiceSubscription),
		singleNotificationClients: make(map[uint32]*SingleInvoiceSubscription),
		newSubscriptions:          make(chan *InvoiceSubscription),
		newSingleSubscriptions:    make(chan *SingleInvoiceSubscription),
		subscriptionCancels:       make(chan uint32),
		invoiceEvents:             make(chan *invoiceEvent, 100),
		activeNetParams:           activeNetParams,
		quit:                      make(chan struct{}),
	}
}

// Start starts the registry and all goroutines it needs to carry out its task.
func (i *InvoiceRegistry) Start() error {
	i.wg.Add(1)

	go i.invoiceEventNotifier()

	return nil
}

// Stop signals the registry for a graceful shutdown.
func (i *InvoiceRegistry) Stop() {
	close(i.quit)

	i.wg.Wait()
}

// invoiceEvent represents a new event that has modified on invoice on disk.
// Only two event types are currently supported: newly created invoices, and
// instance where invoices are settled.
type invoiceEvent struct {
	state   channeldb.ContractState
	hash    lntypes.Hash
	invoice *channeldb.Invoice
}

// invoiceEventNotifier is the dedicated goroutine responsible for accepting
// new notification subscriptions, cancelling old subscriptions, and
// dispatching new invoice events.
func (i *InvoiceRegistry) invoiceEventNotifier() {
	defer i.wg.Done()

	for {
		select {
		// A new invoice subscription for all invoices has just arrived!
		// We'll query for any backlog notifications, then add it to the
		// set of clients.
		case newClient := <-i.newSubscriptions:
			// Before we add the client to our set of active
			// clients, we'll first attempt to deliver any backlog
			// invoice events.
			err := i.deliverBacklogEvents(newClient)
			if err != nil {
				log.Errorf("unable to deliver backlog invoice "+
					"notifications: %v", err)
			}

			log.Infof("New invoice subscription "+
				"client: id=%v", newClient.id)

			// With the backlog notifications delivered (if any),
			// we'll add this to our active subscriptions and
			// continue.
			i.notificationClients[newClient.id] = newClient

		// A new single invoice subscription has arrived. We'll query
		// for any backlog notifications, then add it to the set of
		// clients.
		case newClient := <-i.newSingleSubscriptions:
			err := i.deliverSingleBacklogEvents(newClient)
			if err != nil {
				log.Errorf("Unable to deliver backlog invoice "+
					"notifications: %v", err)
			}

			log.Infof("New single invoice subscription "+
				"client: id=%v, hash=%v",
				newClient.id, newClient.hash,
			)

			i.singleNotificationClients[newClient.id] = newClient

		// A client no longer wishes to receive invoice notifications.
		// So we'll remove them from the set of active clients.
		case clientID := <-i.subscriptionCancels:
			log.Infof("Cancelling invoice subscription for "+
				"client=%v", clientID)

			delete(i.notificationClients, clientID)
			delete(i.singleNotificationClients, clientID)

		// A sub-systems has just modified the invoice state, so we'll
		// dispatch notifications to all registered clients.
		case event := <-i.invoiceEvents:
			i.dispatchToClients(event)
			i.dispatchToSingleClients(event)

		case <-i.quit:
			return
		}
	}
}

// dispatchToSingleClients passes the supplied event to all notification clients
// that subscribed to all the invoice this event applies to.
func (i *InvoiceRegistry) dispatchToSingleClients(event *invoiceEvent) {
	// Dispatch to single invoice subscribers.
	for _, client := range i.singleNotificationClients {
		if client.hash != event.hash {
			continue
		}

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			state:   event.state,
			invoice: event.invoice,
		}:
		case <-i.quit:
			return
		}
	}
}

// dispatchToClients passes the supplied event to all notification clients that
// subscribed to all invoices. Add and settle indices are used to make sure that
// clients don't receive duplicate or unwanted events.
func (i *InvoiceRegistry) dispatchToClients(event *invoiceEvent) {
	invoice := event.invoice

	for clientID, client := range i.notificationClients {
		// Before we dispatch this event, we'll check
		// to ensure that this client hasn't already
		// received this notification in order to
		// ensure we don't duplicate any events.

		// TODO(joostjager): Refactor switches.
		switch {
		// If we've already sent this settle event to
		// the client, then we can skip this.
		case event.state == channeldb.ContractSettled &&
			client.settleIndex >= invoice.SettleIndex:
			continue

		// Similarly, if we've already sent this add to
		// the client then we can skip this one.
		case event.state == channeldb.ContractOpen &&
			client.addIndex >= invoice.AddIndex:
			continue

		// These two states should never happen, but we
		// log them just in case so we can detect this
		// instance.
		case event.state == channeldb.ContractOpen &&
			client.addIndex+1 != invoice.AddIndex:
			log.Warnf("client=%v for invoice "+
				"notifications missed an update, "+
				"add_index=%v, new add event index=%v",
				clientID, client.addIndex,
				invoice.AddIndex)

		case event.state == channeldb.ContractSettled &&
			client.settleIndex+1 != invoice.SettleIndex:
			log.Warnf("client=%v for invoice "+
				"notifications missed an update, "+
				"settle_index=%v, new settle event index=%v",
				clientID, client.settleIndex,
				invoice.SettleIndex)
		}

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			state:   event.state,
			invoice: invoice,
		}:
		case <-i.quit:
			return
		}

		// Each time we send a notification to a client, we'll record
		// the latest add/settle index it has. We'll use this to ensure
		// we don't send a notification twice, which can happen if a new
		// event is added while we're catching up a new client.
		switch event.state {
		case channeldb.ContractSettled:
			client.settleIndex = invoice.SettleIndex
		case channeldb.ContractOpen:
			client.addIndex = invoice.AddIndex
		default:
			log.Errorf("unknown invoice state: %v", event.state)
		}
	}
}

// deliverBacklogEvents will attempts to query the invoice database for any
// notifications that the client has missed since it reconnected last.
func (i *InvoiceRegistry) deliverBacklogEvents(client *InvoiceSubscription) error {
	// First, we'll query the database to see if based on the provided
	// addIndex and settledIndex we need to deliver any backlog
	// notifications.
	addEvents, err := i.cdb.InvoicesAddedSince(client.addIndex)
	if err != nil {
		return err
	}

	settleEvents, err := i.cdb.InvoicesSettledSince(client.settleIndex)
	if err != nil {
		return err
	}

	// If we have any to deliver, then we'll append them to the end of the
	// notification queue in order to catch up the client before delivering
	// any new notifications.
	for _, addEvent := range addEvents {
		// We re-bind the loop variable to ensure we don't hold onto
		// the loop reference causing is to point to the same item.
		addEvent := addEvent

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			state:   channeldb.ContractOpen,
			invoice: &addEvent,
		}:
		case <-i.quit:
			return fmt.Errorf("registry shutting down")
		}
	}

	for _, settleEvent := range settleEvents {
		// We re-bind the loop variable to ensure we don't hold onto
		// the loop reference causing is to point to the same item.
		settleEvent := settleEvent

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			state:   channeldb.ContractSettled,
			invoice: &settleEvent,
		}:
		case <-i.quit:
			return fmt.Errorf("registry shutting down")
		}
	}

	return nil
}

// deliverSingleBacklogEvents will attempt to query the invoice database to
// retrieve the current invoice state and deliver this to the subscriber. Single
// invoice subscribers will always receive the current state right after
// subscribing. Only in case the invoice does not yet exist, nothing is sent
// yet.
func (i *InvoiceRegistry) deliverSingleBacklogEvents(
	client *SingleInvoiceSubscription) error {

	invoice, err := i.cdb.LookupInvoice(client.hash)

	// It is possible that the invoice does not exist yet, but the client is
	// already watching it in anticipation.
	if err == channeldb.ErrInvoiceNotFound {
		return nil
	}
	if err != nil {
		return err
	}

	err = client.notify(&invoiceEvent{
		hash:    client.hash,
		invoice: &invoice,
		state:   invoice.Terms.State,
	})
	if err != nil {
		return err
	}

	return nil
}

// AddDebugInvoice adds a debug invoice for the specified amount, identified
// by the passed preimage. Once this invoice is added, subsystems within the
// daemon add/forward HTLCs that are able to obtain the proper preimage
// required for redemption in the case that we're the final destination.
func (i *InvoiceRegistry) AddDebugInvoice(amt btcutil.Amount,
	preimage lntypes.Preimage) {

	paymentHash := preimage.Hash()

	invoice := &channeldb.Invoice{
		CreationDate: time.Now(),
		Terms: channeldb.ContractTerm{
			Value:           lnwire.NewMSatFromSatoshis(amt),
			PaymentPreimage: preimage,
		},
	}

	i.Lock()
	i.debugInvoices[paymentHash] = invoice
	i.Unlock()

	log.Debugf("Adding debug invoice %v", newLogClosure(func() string {
		return spew.Sdump(invoice)
	}))
}

// AddInvoice adds a regular invoice for the specified amount, identified by
// the passed preimage. Additionally, any memo or receipt data provided will
// also be stored on-disk. Once this invoice is added, subsystems within the
// daemon add/forward HTLCs are able to obtain the proper preimage required for
// redemption in the case that we're the final destination. We also return the
// addIndex of the newly created invoice which monotonically increases for each
// new invoice added.
func (i *InvoiceRegistry) AddInvoice(invoice *channeldb.Invoice,
	paymentHash lntypes.Hash) (uint64, error) {

	i.Lock()
	defer i.Unlock()

	log.Debugf("Adding invoice %v", newLogClosure(func() string {
		return spew.Sdump(invoice)
	}))

	addIndex, err := i.cdb.AddInvoice(invoice)
	if err != nil {
		return 0, err
	}

	// Now that we've added the invoice, we'll send dispatch a message to
	// notify the clients of this new invoice.
	i.notifyClients(paymentHash, invoice, channeldb.ContractOpen)

	return addIndex, nil
}

// LookupInvoice looks up an invoice by its payment hash (R-Hash), if found
// then we're able to pull the funds pending within an HTLC. We'll also return
// what the expected min final CLTV delta is, pre-parsed from the payment
// request. This may be used by callers to determine if an HTLC is well formed
// according to the cltv delta.
//
// TODO(roasbeef): ignore if settled?
func (i *InvoiceRegistry) LookupInvoice(rHash lntypes.Hash) (channeldb.Invoice, uint32, error) {
	// First check the in-memory debug invoice index to see if this is an
	// existing invoice added for debugging.
	i.RLock()
	debugInv, ok := i.debugInvoices[rHash]
	i.RUnlock()

	// If found, then simply return the invoice directly.
	if ok {
		return *debugInv, 0, nil
	}

	// Otherwise, we'll check the database to see if there's an existing
	// matching invoice.
	invoice, err := i.cdb.LookupInvoice(rHash)
	if err != nil {
		return channeldb.Invoice{}, 0, err
	}

	payReq, err := zpay32.Decode(
		string(invoice.PaymentRequest), i.activeNetParams,
	)
	if err != nil {
		return channeldb.Invoice{}, 0, err
	}

	return invoice, uint32(payReq.MinFinalCLTVExpiry()), nil
}

// SettleInvoice attempts to mark an invoice as settled. If the invoice is a
// debug invoice, then this method is a noop as debug invoices are never fully
// settled.
func (i *InvoiceRegistry) SettleInvoice(rHash lntypes.Hash,
	amtPaid lnwire.MilliSatoshi) error {

	i.Lock()
	defer i.Unlock()

	log.Debugf("Settling invoice %x", rHash[:])

	// First check the in-memory debug invoice index to see if this is an
	// existing invoice added for debugging.
	if _, ok := i.debugInvoices[rHash]; ok {
		// Debug invoices are never fully settled, so we simply return
		// immediately in this case.
		return nil
	}

	// If this isn't a debug invoice, then we'll attempt to settle an
	// invoice matching this rHash on disk (if one exists).
	invoice, err := i.cdb.SettleInvoice(rHash, amtPaid)

	// Implement idempotency by returning success if the invoice was already
	// settled.
	if err == channeldb.ErrInvoiceAlreadySettled {
		log.Debugf("Invoice %v already settled", rHash)
		return nil
	}

	if err != nil {
		return err
	}

	log.Infof("Payment received: %v", spew.Sdump(invoice))

	i.notifyClients(rHash, invoice, channeldb.ContractSettled)

	return nil
}

// notifyClients notifies all currently registered invoice notification clients
// of a newly added/settled invoice.
func (i *InvoiceRegistry) notifyClients(hash lntypes.Hash,
	invoice *channeldb.Invoice,
	state channeldb.ContractState) {

	event := &invoiceEvent{
		state:   state,
		invoice: invoice,
		hash:    hash,
	}

	select {
	case i.invoiceEvents <- event:
	case <-i.quit:
	}
}

// invoiceSubscriptionKit defines that are common to both all invoice
// subscribers and single invoice subscribers.
type invoiceSubscriptionKit struct {
	id        uint32
	inv       *InvoiceRegistry
	ntfnQueue *queue.ConcurrentQueue

	cancelled  uint32 // To be used atomically.
	cancelChan chan struct{}
	wg         sync.WaitGroup
}

// InvoiceSubscription represents an intent to receive updates for newly added
// or settled invoices. For each newly added invoice, a copy of the invoice
// will be sent over the NewInvoices channel. Similarly, for each newly settled
// invoice, a copy of the invoice will be sent over the SettledInvoices
// channel.
type InvoiceSubscription struct {
	invoiceSubscriptionKit

	// NewInvoices is a channel that we'll use to send all newly created
	// invoices with an invoice index greater than the specified
	// StartingInvoiceIndex field.
	NewInvoices chan *channeldb.Invoice

	// SettledInvoices is a channel that we'll use to send all setted
	// invoices with an invoices index greater than the specified
	// StartingInvoiceIndex field.
	SettledInvoices chan *channeldb.Invoice

	// addIndex is the highest add index the caller knows of. We'll use
	// this information to send out an event backlog to the notifications
	// subscriber. Any new add events with an index greater than this will
	// be dispatched before any new notifications are sent out.
	addIndex uint64

	// settleIndex is the highest settle index the caller knows of. We'll
	// use this information to send out an event backlog to the
	// notifications subscriber. Any new settle events with an index
	// greater than this will be dispatched before any new notifications
	// are sent out.
	settleIndex uint64
}

// SingleInvoiceSubscription represents an intent to receive updates for a
// specific invoice.
type SingleInvoiceSubscription struct {
	invoiceSubscriptionKit

	hash lntypes.Hash

	// Updates is a channel that we'll use to send all invoice events for
	// the invoice that is subscribed to.
	Updates chan *channeldb.Invoice
}

// Cancel unregisters the InvoiceSubscription, freeing any previously allocated
// resources.
func (i *invoiceSubscriptionKit) Cancel() {
	if !atomic.CompareAndSwapUint32(&i.cancelled, 0, 1) {
		return
	}

	select {
	case i.inv.subscriptionCancels <- i.id:
	case <-i.inv.quit:
	}

	i.ntfnQueue.Stop()
	close(i.cancelChan)

	i.wg.Wait()
}

func (i *invoiceSubscriptionKit) notify(event *invoiceEvent) error {
	select {
	case i.ntfnQueue.ChanIn() <- event:
	case <-i.inv.quit:
		return fmt.Errorf("registry shutting down")
	}

	return nil
}

// SubscribeNotifications returns an InvoiceSubscription which allows the
// caller to receive async notifications when any invoices are settled or
// added. The invoiceIndex parameter is a streaming "checkpoint". We'll start
// by first sending out all new events with an invoice index _greater_ than
// this value. Afterwards, we'll send out real-time notifications.
func (i *InvoiceRegistry) SubscribeNotifications(addIndex, settleIndex uint64) *InvoiceSubscription {
	client := &InvoiceSubscription{
		NewInvoices:     make(chan *channeldb.Invoice),
		SettledInvoices: make(chan *channeldb.Invoice),
		addIndex:        addIndex,
		settleIndex:     settleIndex,
		invoiceSubscriptionKit: invoiceSubscriptionKit{
			inv:        i,
			ntfnQueue:  queue.NewConcurrentQueue(20),
			cancelChan: make(chan struct{}),
		},
	}
	client.ntfnQueue.Start()

	i.clientMtx.Lock()
	client.id = i.nextClientID
	i.nextClientID++
	i.clientMtx.Unlock()

	// Before we register this new invoice subscription, we'll launch a new
	// goroutine that will proxy all notifications appended to the end of
	// the concurrent queue to the two client-side channels the caller will
	// feed off of.
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()

		for {
			select {
			// A new invoice event has been sent by the
			// invoiceRegistry! We'll figure out if this is an add
			// event or a settle event, then dispatch the event to
			// the client.
			case ntfn := <-client.ntfnQueue.ChanOut():
				invoiceEvent := ntfn.(*invoiceEvent)

				var targetChan chan *channeldb.Invoice
				switch invoiceEvent.state {
				case channeldb.ContractOpen:
					targetChan = client.NewInvoices
				case channeldb.ContractSettled:
					targetChan = client.SettledInvoices
				default:
					log.Errorf("unknown invoice "+
						"state: %v", invoiceEvent.state)

					continue
				}

				select {
				case targetChan <- invoiceEvent.invoice:

				case <-client.cancelChan:
					return

				case <-i.quit:
					return
				}

			case <-client.cancelChan:
				return

			case <-i.quit:
				return
			}
		}
	}()

	select {
	case i.newSubscriptions <- client:
	case <-i.quit:
	}

	return client
}

// SubscribeSingleInvoice returns an SingleInvoiceSubscription which allows the
// caller to receive async notifications for a specific invoice.
func (i *InvoiceRegistry) SubscribeSingleInvoice(
	hash lntypes.Hash) *SingleInvoiceSubscription {

	client := &SingleInvoiceSubscription{
		Updates: make(chan *channeldb.Invoice),
		invoiceSubscriptionKit: invoiceSubscriptionKit{
			inv:        i,
			ntfnQueue:  queue.NewConcurrentQueue(20),
			cancelChan: make(chan struct{}),
		},
		hash: hash,
	}
	client.ntfnQueue.Start()

	i.clientMtx.Lock()
	client.id = i.nextClientID
	i.nextClientID++
	i.clientMtx.Unlock()

	// Before we register this new invoice subscription, we'll launch a new
	// goroutine that will proxy all notifications appended to the end of
	// the concurrent queue to the two client-side channels the caller will
	// feed off of.
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()

		for {
			select {
			// A new invoice event has been sent by the
			// invoiceRegistry. We will dispatch the event to the
			// client.
			case ntfn := <-client.ntfnQueue.ChanOut():
				invoiceEvent := ntfn.(*invoiceEvent)

				select {
				case client.Updates <- invoiceEvent.invoice:

				case <-client.cancelChan:
					return

				case <-i.quit:
					return
				}

			case <-client.cancelChan:
				return

			case <-i.quit:
				return
			}
		}
	}()

	select {
	case i.newSingleSubscriptions <- client:
	case <-i.quit:
	}

	return client
}
