package invoices

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
)

var (
	// ErrInvoiceExpiryTooSoon is returned when an invoice is attempted to be
	// accepted or settled with not enough blocks remaining.
	ErrInvoiceExpiryTooSoon = errors.New("invoice expiry too soon")

	// ErrInvoiceAmountTooLow is returned  when an invoice is attempted to be
	// accepted or settled with an amount that is too low.
	ErrInvoiceAmountTooLow = errors.New("paid amount less than invoice amount")

	// ErrShuttingDown is returned when an operation failed because the
	// invoice registry is shutting down.
	ErrShuttingDown = errors.New("invoice registry shutting down")
)

// HodlEvent describes how an htlc should be resolved. If HodlEvent.Preimage is
// set, the event indicates a settle event. If Preimage is nil, it is a cancel
// event.
type HodlEvent struct {
	// Preimage is the htlc preimage. Its value is nil in case of a cancel.
	Preimage *lntypes.Preimage

	// CircuitKey is the key of the htlc for which we have a resolution
	// decision.
	CircuitKey channeldb.CircuitKey

	// AcceptHeight is the original height at which the htlc was accepted.
	AcceptHeight int32
}

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

	newSubscriptions    chan *InvoiceSubscription
	subscriptionCancels chan uint32

	// invoiceEvents is a single channel over which both invoice updates and
	// new single invoice subscriptions are carried.
	invoiceEvents chan interface{}

	// subscriptions is a map from a circuit key to a list of subscribers.
	// It is used for efficient notification of links.
	hodlSubscriptions map[channeldb.CircuitKey]map[chan<- interface{}]struct{}

	// reverseSubscriptions tracks circuit keys subscribed to per
	// subscriber. This is used to unsubscribe from all hashes efficiently.
	hodlReverseSubscriptions map[chan<- interface{}]map[channeldb.CircuitKey]struct{}

	// finalCltvRejectDelta defines the number of blocks before the expiry
	// of the htlc where we no longer settle it as an exit hop and instead
	// cancel it back. Normally this value should be lower than the cltv
	// expiry of any invoice we create and the code effectuating this should
	// not be hit.
	finalCltvRejectDelta int32

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewRegistry creates a new invoice registry. The invoice registry
// wraps the persistent on-disk invoice storage with an additional in-memory
// layer. The in-memory layer is in place such that debug invoices can be added
// which are volatile yet available system wide within the daemon.
func NewRegistry(cdb *channeldb.DB, finalCltvRejectDelta int32) *InvoiceRegistry {

	return &InvoiceRegistry{
		cdb:                       cdb,
		notificationClients:       make(map[uint32]*InvoiceSubscription),
		singleNotificationClients: make(map[uint32]*SingleInvoiceSubscription),
		newSubscriptions:          make(chan *InvoiceSubscription),
		subscriptionCancels:       make(chan uint32),
		invoiceEvents:             make(chan interface{}, 100),
		hodlSubscriptions:         make(map[channeldb.CircuitKey]map[chan<- interface{}]struct{}),
		hodlReverseSubscriptions:  make(map[chan<- interface{}]map[channeldb.CircuitKey]struct{}),
		finalCltvRejectDelta:      finalCltvRejectDelta,
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

		// A client no longer wishes to receive invoice notifications.
		// So we'll remove them from the set of active clients.
		case clientID := <-i.subscriptionCancels:
			log.Infof("Cancelling invoice subscription for "+
				"client=%v", clientID)

			delete(i.notificationClients, clientID)
			delete(i.singleNotificationClients, clientID)

		// An invoice event has come in. This can either be an update to
		// an invoice or a new single invoice subscriber. Both type of
		// events are passed in via the same channel, to make sure that
		// subscribers get a consistent view of the event sequence.
		case event := <-i.invoiceEvents:
			switch e := event.(type) {

			// A sub-systems has just modified the invoice state, so
			// we'll dispatch notifications to all registered
			// clients.
			case *invoiceEvent:
				// For backwards compatibility, do not notify
				// all invoice subscribers of cancel and accept
				// events.
				state := e.invoice.State
				if state != channeldb.ContractCanceled &&
					state != channeldb.ContractAccepted {

					i.dispatchToClients(e)
				}
				i.dispatchToSingleClients(e)

			// A new single invoice subscription has arrived. Add it
			// to the set of clients. It is important to do this in
			// sequence with any other invoice events, because an
			// initial invoice update has already been sent out to
			// the subscriber.
			case *SingleInvoiceSubscription:
				log.Infof("New single invoice subscription "+
					"client: id=%v, hash=%v", e.id, e.hash)

				i.singleNotificationClients[e.id] = e
			}

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

		client.notify(event)
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
		state := event.invoice.State
		switch {
		// If we've already sent this settle event to
		// the client, then we can skip this.
		case state == channeldb.ContractSettled &&
			client.settleIndex >= invoice.SettleIndex:
			continue

		// Similarly, if we've already sent this add to
		// the client then we can skip this one.
		case state == channeldb.ContractOpen &&
			client.addIndex >= invoice.AddIndex:
			continue

		// These two states should never happen, but we
		// log them just in case so we can detect this
		// instance.
		case state == channeldb.ContractOpen &&
			client.addIndex+1 != invoice.AddIndex:
			log.Warnf("client=%v for invoice "+
				"notifications missed an update, "+
				"add_index=%v, new add event index=%v",
				clientID, client.addIndex,
				invoice.AddIndex)

		case state == channeldb.ContractSettled &&
			client.settleIndex+1 != invoice.SettleIndex:
			log.Warnf("client=%v for invoice "+
				"notifications missed an update, "+
				"settle_index=%v, new settle event index=%v",
				clientID, client.settleIndex,
				invoice.SettleIndex)
		}

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			invoice: invoice,
		}:
		case <-i.quit:
			return
		}

		// Each time we send a notification to a client, we'll record
		// the latest add/settle index it has. We'll use this to ensure
		// we don't send a notification twice, which can happen if a new
		// event is added while we're catching up a new client.
		switch event.invoice.State {
		case channeldb.ContractSettled:
			client.settleIndex = invoice.SettleIndex
		case channeldb.ContractOpen:
			client.addIndex = invoice.AddIndex
		default:
			log.Errorf("unexpected invoice state: %v",
				event.invoice.State)
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
			invoice: &addEvent,
		}:
		case <-i.quit:
			return ErrShuttingDown
		}
	}

	for _, settleEvent := range settleEvents {
		// We re-bind the loop variable to ensure we don't hold onto
		// the loop reference causing is to point to the same item.
		settleEvent := settleEvent

		select {
		case client.ntfnQueue.ChanIn() <- &invoiceEvent{
			invoice: &settleEvent,
		}:
		case <-i.quit:
			return ErrShuttingDown
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
	if err == channeldb.ErrInvoiceNotFound ||
		err == channeldb.ErrNoInvoicesCreated {

		return nil
	}
	if err != nil {
		return err
	}

	err = client.notify(&invoiceEvent{
		hash:    client.hash,
		invoice: &invoice,
	})
	if err != nil {
		return err
	}

	return nil
}

// AddInvoice adds a regular invoice for the specified amount, identified by
// the passed preimage. Additionally, any memo or receipt data provided will
// also be stored on-disk. Once this invoice is added, subsystems within the
// daemon add/forward HTLCs are able to obtain the proper preimage required for
// redemption in the case that we're the final destination. We also return the
// addIndex of the newly created invoice which monotonically increases for each
// new invoice added.  A side effect of this function is that it also sets
// AddIndex on the invoice argument.
func (i *InvoiceRegistry) AddInvoice(invoice *channeldb.Invoice,
	paymentHash lntypes.Hash) (uint64, error) {

	i.Lock()
	defer i.Unlock()

	log.Debugf("Invoice(%v): added %v", paymentHash,
		newLogClosure(func() string {
			return spew.Sdump(invoice)
		}),
	)

	addIndex, err := i.cdb.AddInvoice(invoice, paymentHash)
	if err != nil {
		return 0, err
	}

	// Now that we've added the invoice, we'll send dispatch a message to
	// notify the clients of this new invoice.
	i.notifyClients(paymentHash, invoice, channeldb.ContractOpen)

	return addIndex, nil
}

// LookupInvoice looks up an invoice by its payment hash (R-Hash), if found
// then we're able to pull the funds pending within an HTLC.
//
// TODO(roasbeef): ignore if settled?
func (i *InvoiceRegistry) LookupInvoice(rHash lntypes.Hash) (channeldb.Invoice,
	error) {

	// We'll check the database to see if there's an existing matching
	// invoice.
	return i.cdb.LookupInvoice(rHash)
}

// NotifyExitHopHtlc attempts to mark an invoice as settled. The return value
// describes how the htlc should be resolved.
//
// When the preimage of the invoice is not yet known (hodl invoice), this
// function moves the invoice to the accepted state. When SettleHoldInvoice is
// called later, a resolution message will be send back to the caller via the
// provided hodlChan. Invoice registry sends on this channel what action needs
// to be taken on the htlc (settle or cancel). The caller needs to ensure that
// the channel is either buffered or received on from another goroutine to
// prevent deadlock.
func (i *InvoiceRegistry) NotifyExitHopHtlc(rHash lntypes.Hash,
	amtPaid lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey channeldb.CircuitKey, hodlChan chan<- interface{},
	payload Payload) (*HodlEvent, error) {

	i.Lock()
	defer i.Unlock()

	debugLog := func(s string) {
		log.Debugf("Invoice(%x): %v, amt=%v, expiry=%v, circuit=%v",
			rHash[:], s, amtPaid, expiry, circuitKey)
	}

	// Create the update context containing the relevant details of the
	// incoming htlc.
	updateCtx := invoiceUpdateCtx{
		circuitKey:           circuitKey,
		amtPaid:              amtPaid,
		expiry:               expiry,
		currentHeight:        currentHeight,
		finalCltvRejectDelta: i.finalCltvRejectDelta,
		customRecords:        payload.CustomRecords(),
	}

	// We'll attempt to settle an invoice matching this rHash on disk (if
	// one exists). The callback will update the invoice state and/or htlcs.
	var (
		result            updateResult
		updateSubscribers bool
	)
	invoice, err := i.cdb.UpdateInvoice(
		rHash,
		func(inv *channeldb.Invoice) (
			*channeldb.InvoiceUpdateDesc, error) {

			updateDesc, res, err := updateInvoice(&updateCtx, inv)
			if err != nil {
				return nil, err
			}

			// Only send an update if the invoice state was changed.
			updateSubscribers = updateDesc != nil &&
				updateDesc.State != nil

			// Assign result to outer scope variable.
			result = res

			return updateDesc, nil
		},
	)
	if err != nil {
		debugLog(err.Error())

		return nil, err
	}
	debugLog(result.String())

	if updateSubscribers {
		i.notifyClients(rHash, invoice, invoice.State)
	}

	// Inspect latest htlc state on the invoice.
	invoiceHtlc, ok := invoice.Htlcs[circuitKey]

	// If it isn't recorded, cancel htlc.
	if !ok {
		return &HodlEvent{
			CircuitKey:   circuitKey,
			AcceptHeight: currentHeight,
		}, nil
	}

	// Determine accepted height of this htlc. If the htlc reached the
	// invoice database (possibly in a previous call to the invoice
	// registry), we'll take the original accepted height as it was recorded
	// in the database.
	acceptHeight := int32(invoiceHtlc.AcceptHeight)

	switch invoiceHtlc.State {
	case channeldb.HtlcStateCanceled:
		return &HodlEvent{
			CircuitKey:   circuitKey,
			AcceptHeight: acceptHeight,
		}, nil

	case channeldb.HtlcStateSettled:
		return &HodlEvent{
			CircuitKey:   circuitKey,
			Preimage:     &invoice.Terms.PaymentPreimage,
			AcceptHeight: acceptHeight,
		}, nil

	case channeldb.HtlcStateAccepted:
		i.hodlSubscribe(hodlChan, circuitKey)
		return nil, nil

	default:
		panic("unknown action")
	}
}

// SettleHodlInvoice sets the preimage of a hodl invoice.
func (i *InvoiceRegistry) SettleHodlInvoice(preimage lntypes.Preimage) error {
	i.Lock()
	defer i.Unlock()

	updateInvoice := func(invoice *channeldb.Invoice) (
		*channeldb.InvoiceUpdateDesc, error) {

		switch invoice.State {
		case channeldb.ContractOpen:
			return nil, channeldb.ErrInvoiceStillOpen
		case channeldb.ContractCanceled:
			return nil, channeldb.ErrInvoiceAlreadyCanceled
		case channeldb.ContractSettled:
			return nil, channeldb.ErrInvoiceAlreadySettled
		}

		return &channeldb.InvoiceUpdateDesc{
			State: &channeldb.InvoiceStateUpdateDesc{
				NewState: channeldb.ContractSettled,
				Preimage: preimage,
			},
		}, nil
	}

	hash := preimage.Hash()
	invoice, err := i.cdb.UpdateInvoice(hash, updateInvoice)
	if err != nil {
		log.Errorf("SettleHodlInvoice with preimage %v: %v", preimage, err)
		return err
	}

	log.Debugf("Invoice(%v): settled with preimage %v", hash,
		invoice.Terms.PaymentPreimage)

	// In the callback, we marked the invoice as settled. UpdateInvoice will
	// have seen this and should have moved all htlcs that were accepted to
	// the settled state. In the loop below, we go through all of these and
	// notify links and resolvers that are waiting for resolution. Any htlcs
	// that were already settled before, will be notified again. This isn't
	// necessary but doesn't hurt either.
	for key, htlc := range invoice.Htlcs {
		if htlc.State != channeldb.HtlcStateSettled {
			continue
		}

		i.notifyHodlSubscribers(HodlEvent{
			CircuitKey:   key,
			Preimage:     &preimage,
			AcceptHeight: int32(htlc.AcceptHeight),
		})
	}
	i.notifyClients(hash, invoice, invoice.State)

	return nil
}

// CancelInvoice attempts to cancel the invoice corresponding to the passed
// payment hash.
func (i *InvoiceRegistry) CancelInvoice(payHash lntypes.Hash) error {
	i.Lock()
	defer i.Unlock()

	log.Debugf("Invoice(%v): canceling invoice", payHash)

	updateInvoice := func(invoice *channeldb.Invoice) (
		*channeldb.InvoiceUpdateDesc, error) {

		// Move invoice to the canceled state. Rely on validation in
		// channeldb to return an error if the invoice is already
		// settled or canceled.
		return &channeldb.InvoiceUpdateDesc{
			State: &channeldb.InvoiceStateUpdateDesc{
				NewState: channeldb.ContractCanceled,
			},
		}, nil
	}

	invoice, err := i.cdb.UpdateInvoice(payHash, updateInvoice)

	// Implement idempotency by returning success if the invoice was already
	// canceled.
	if err == channeldb.ErrInvoiceAlreadyCanceled {
		log.Debugf("Invoice(%v): already canceled", payHash)
		return nil
	}
	if err != nil {
		return err
	}

	log.Debugf("Invoice(%v): canceled", payHash)

	// In the callback, some htlcs may have been moved to the canceled
	// state. We now go through all of these and notify links and resolvers
	// that are waiting for resolution. Any htlcs that were already canceled
	// before, will be notified again. This isn't necessary but doesn't hurt
	// either.
	for key, htlc := range invoice.Htlcs {
		if htlc.State != channeldb.HtlcStateCanceled {
			continue
		}

		i.notifyHodlSubscribers(HodlEvent{
			CircuitKey:   key,
			AcceptHeight: int32(htlc.AcceptHeight),
		})
	}
	i.notifyClients(payHash, invoice, channeldb.ContractCanceled)

	return nil
}

// notifyClients notifies all currently registered invoice notification clients
// of a newly added/settled invoice.
func (i *InvoiceRegistry) notifyClients(hash lntypes.Hash,
	invoice *channeldb.Invoice,
	state channeldb.ContractState) {

	event := &invoiceEvent{
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

	canceled   uint32 // To be used atomically.
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
	if !atomic.CompareAndSwapUint32(&i.canceled, 0, 1) {
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
		return ErrShuttingDown
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
				state := invoiceEvent.invoice.State
				switch state {
				case channeldb.ContractOpen:
					targetChan = client.NewInvoices
				case channeldb.ContractSettled:
					targetChan = client.SettledInvoices
				default:
					log.Errorf("unknown invoice "+
						"state: %v", state)

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
	hash lntypes.Hash) (*SingleInvoiceSubscription, error) {

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

	// Within the lock, we both query the invoice state and pass the client
	// subscription to the invoiceEvents channel. This is to make sure that
	// the client receives a consistent stream of events.
	i.Lock()
	defer i.Unlock()

	err := i.deliverSingleBacklogEvents(client)
	if err != nil {
		return nil, err
	}

	select {
	case i.invoiceEvents <- client:
	case <-i.quit:
		return nil, ErrShuttingDown
	}

	return client, nil
}

// notifyHodlSubscribers sends out the hodl event to all current subscribers.
func (i *InvoiceRegistry) notifyHodlSubscribers(hodlEvent HodlEvent) {
	subscribers, ok := i.hodlSubscriptions[hodlEvent.CircuitKey]
	if !ok {
		return
	}

	// Notify all interested subscribers and remove subscription from both
	// maps. The subscription can be removed as there only ever will be a
	// single resolution for each hash.
	for subscriber := range subscribers {
		select {
		case subscriber <- hodlEvent:
		case <-i.quit:
			return
		}

		delete(
			i.hodlReverseSubscriptions[subscriber],
			hodlEvent.CircuitKey,
		)
	}

	delete(i.hodlSubscriptions, hodlEvent.CircuitKey)
}

// hodlSubscribe adds a new invoice subscription.
func (i *InvoiceRegistry) hodlSubscribe(subscriber chan<- interface{},
	circuitKey channeldb.CircuitKey) {

	log.Debugf("Hodl subscribe for %v", circuitKey)

	subscriptions, ok := i.hodlSubscriptions[circuitKey]
	if !ok {
		subscriptions = make(map[chan<- interface{}]struct{})
		i.hodlSubscriptions[circuitKey] = subscriptions
	}
	subscriptions[subscriber] = struct{}{}

	reverseSubscriptions, ok := i.hodlReverseSubscriptions[subscriber]
	if !ok {
		reverseSubscriptions = make(map[channeldb.CircuitKey]struct{})
		i.hodlReverseSubscriptions[subscriber] = reverseSubscriptions
	}
	reverseSubscriptions[circuitKey] = struct{}{}
}

// HodlUnsubscribeAll cancels the subscription.
func (i *InvoiceRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {
	i.Lock()
	defer i.Unlock()

	hashes := i.hodlReverseSubscriptions[subscriber]
	for hash := range hashes {
		delete(i.hodlSubscriptions[hash], subscriber)
	}

	delete(i.hodlReverseSubscriptions, subscriber)
}
