package invoices

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/record"
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

const (
	// DefaultHtlcHoldDuration defines the default for how long mpp htlcs
	// are held while waiting for the other set members to arrive.
	DefaultHtlcHoldDuration = 120 * time.Second
)

// RegistryConfig contains the configuration parameters for invoice registry.
type RegistryConfig struct {
	// FinalCltvRejectDelta defines the number of blocks before the expiry
	// of the htlc where we no longer settle it as an exit hop and instead
	// cancel it back. Normally this value should be lower than the cltv
	// expiry of any invoice we create and the code effectuating this should
	// not be hit.
	FinalCltvRejectDelta int32

	// HtlcHoldDuration defines for how long mpp htlcs are held while
	// waiting for the other set members to arrive.
	HtlcHoldDuration time.Duration

	// Clock holds the clock implementation that is used to provide
	// Now() and TickAfter() and is useful to stub out the clock functions
	// during testing.
	Clock clock.Clock

	// AcceptKeySend indicates whether we want to accept spontaneous key
	// send payments.
	AcceptKeySend bool

	// AcceptAMP indicates whether we want to accept spontaneous AMP
	// payments.
	AcceptAMP bool

	// GcCanceledInvoicesOnStartup if set, we'll attempt to garbage collect
	// all canceled invoices upon start.
	GcCanceledInvoicesOnStartup bool

	// GcCanceledInvoicesOnTheFly if set, we'll garbage collect all newly
	// canceled invoices on the fly.
	GcCanceledInvoicesOnTheFly bool

	// KeysendHoldTime indicates for how long we want to accept and hold
	// spontaneous keysend payments.
	KeysendHoldTime time.Duration
}

// htlcReleaseEvent describes an htlc auto-release event. It is used to release
// mpp htlcs for which the complete set didn't arrive in time.
type htlcReleaseEvent struct {
	// invoiceRef identifiers the invoice this htlc belongs to.
	invoiceRef channeldb.InvoiceRef

	// key is the circuit key of the htlc to release.
	key channeldb.CircuitKey

	// releaseTime is the time at which to release the htlc.
	releaseTime time.Time
}

// Less is used to order PriorityQueueItem's by their release time such that
// items with the older release time are at the top of the queue.
//
// NOTE: Part of the queue.PriorityQueueItem interface.
func (r *htlcReleaseEvent) Less(other queue.PriorityQueueItem) bool {
	return r.releaseTime.Before(other.(*htlcReleaseEvent).releaseTime)
}

// InvoiceRegistry is a central registry of all the outstanding invoices
// created by the daemon. The registry is a thin wrapper around a map in order
// to ensure that all updates/reads are thread safe.
type InvoiceRegistry struct {
	sync.RWMutex

	nextClientID uint32 // must be used atomically

	cdb *channeldb.DB

	// cfg contains the registry's configuration parameters.
	cfg *RegistryConfig

	notificationClients map[uint32]*InvoiceSubscription

	// TODO(yy): use map[lntypes.Hash]*SingleInvoiceSubscription for better
	// performance.
	singleNotificationClients map[uint32]*SingleInvoiceSubscription

	// invoiceEvents is a single channel over which invoice updates are
	// carried.
	invoiceEvents chan *invoiceEvent

	// subscriptions is a map from a circuit key to a list of subscribers.
	// It is used for efficient notification of links.
	hodlSubscriptions map[channeldb.CircuitKey]map[chan<- interface{}]struct{}

	// reverseSubscriptions tracks circuit keys subscribed to per
	// subscriber. This is used to unsubscribe from all hashes efficiently.
	hodlReverseSubscriptions map[chan<- interface{}]map[channeldb.CircuitKey]struct{}

	// htlcAutoReleaseChan contains the new htlcs that need to be
	// auto-released.
	htlcAutoReleaseChan chan *htlcReleaseEvent

	expiryWatcher *InvoiceExpiryWatcher

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewRegistry creates a new invoice registry. The invoice registry
// wraps the persistent on-disk invoice storage with an additional in-memory
// layer. The in-memory layer is in place such that debug invoices can be added
// which are volatile yet available system wide within the daemon.
func NewRegistry(cdb *channeldb.DB, expiryWatcher *InvoiceExpiryWatcher,
	cfg *RegistryConfig) *InvoiceRegistry {

	return &InvoiceRegistry{
		cdb:                       cdb,
		notificationClients:       make(map[uint32]*InvoiceSubscription),
		singleNotificationClients: make(map[uint32]*SingleInvoiceSubscription),
		invoiceEvents:             make(chan *invoiceEvent, 100),
		hodlSubscriptions:         make(map[channeldb.CircuitKey]map[chan<- interface{}]struct{}),
		hodlReverseSubscriptions:  make(map[chan<- interface{}]map[channeldb.CircuitKey]struct{}),
		cfg:                       cfg,
		htlcAutoReleaseChan:       make(chan *htlcReleaseEvent),
		expiryWatcher:             expiryWatcher,
		quit:                      make(chan struct{}),
	}
}

// scanInvoicesOnStart will scan all invoices on start and add active invoices
// to the invoice expirt watcher while also attempting to delete all canceled
// invoices.
func (i *InvoiceRegistry) scanInvoicesOnStart() error {
	var (
		pending   []invoiceExpiry
		removable []channeldb.InvoiceDeleteRef
	)

	reset := func() {
		// Zero out our results on start and if the scan is ever run
		// more than once. This latter case can happen if the kvdb
		// layer needs to retry the View transaction underneath (eg.
		// using the etcd driver, where all transactions are allowed
		// to retry for serializability).
		pending = nil
		removable = make([]channeldb.InvoiceDeleteRef, 0)
	}

	scanFunc := func(paymentHash lntypes.Hash,
		invoice *channeldb.Invoice) error {

		if invoice.IsPending() {
			expiryRef := makeInvoiceExpiry(paymentHash, invoice)
			if expiryRef != nil {
				pending = append(pending, expiryRef)
			}
		} else if i.cfg.GcCanceledInvoicesOnStartup &&
			invoice.State == channeldb.ContractCanceled {

			// Consider invoice for removal if it is already
			// canceled. Invoices that are expired but not yet
			// canceled, will be queued up for cancellation after
			// startup and will be deleted afterwards.
			ref := channeldb.InvoiceDeleteRef{
				PayHash:     paymentHash,
				AddIndex:    invoice.AddIndex,
				SettleIndex: invoice.SettleIndex,
			}

			if invoice.Terms.PaymentAddr != channeldb.BlankPayAddr {
				ref.PayAddr = &invoice.Terms.PaymentAddr
			}

			removable = append(removable, ref)
		}
		return nil
	}

	err := i.cdb.ScanInvoices(scanFunc, reset)
	if err != nil {
		return err
	}

	log.Debugf("Adding %d pending invoices to the expiry watcher",
		len(pending))
	i.expiryWatcher.AddInvoices(pending...)

	if len(removable) > 0 {
		log.Infof("Attempting to delete %v canceled invoices",
			len(removable))
		if err := i.cdb.DeleteInvoice(removable); err != nil {
			log.Warnf("Deleting canceled invoices failed: %v", err)
		} else {
			log.Infof("Deleted %v canceled invoices",
				len(removable))
		}
	}

	return nil
}

// Start starts the registry and all goroutines it needs to carry out its task.
func (i *InvoiceRegistry) Start() error {
	// Start InvoiceExpiryWatcher and prepopulate it with existing active
	// invoices.
	err := i.expiryWatcher.Start(i.cancelInvoiceImpl)
	if err != nil {
		return err
	}

	log.Info("InvoiceRegistry starting")

	i.wg.Add(1)
	go i.invoiceEventLoop()

	// Now scan all pending and removable invoices to the expiry watcher or
	// delete them.
	err = i.scanInvoicesOnStart()
	if err != nil {
		_ = i.Stop()
		return err
	}

	return nil
}

// Stop signals the registry for a graceful shutdown.
func (i *InvoiceRegistry) Stop() error {
	log.Info("InvoiceRegistry shutting down")

	i.expiryWatcher.Stop()

	close(i.quit)

	i.wg.Wait()
	return nil
}

// invoiceEvent represents a new event that has modified on invoice on disk.
// Only two event types are currently supported: newly created invoices, and
// instance where invoices are settled.
type invoiceEvent struct {
	hash    lntypes.Hash
	invoice *channeldb.Invoice
	setID   *[32]byte
}

// tickAt returns a channel that ticks at the specified time. If the time has
// already passed, it will tick immediately.
func (i *InvoiceRegistry) tickAt(t time.Time) <-chan time.Time {
	now := i.cfg.Clock.Now()
	return i.cfg.Clock.TickAfter(t.Sub(now))
}

// invoiceEventLoop is the dedicated goroutine responsible for accepting
// new notification subscriptions, cancelling old subscriptions, and
// dispatching new invoice events.
func (i *InvoiceRegistry) invoiceEventLoop() {
	defer i.wg.Done()

	// Set up a heap for htlc auto-releases.
	autoReleaseHeap := &queue.PriorityQueue{}

	for {
		// If there is something to release, set up a release tick
		// channel.
		var nextReleaseTick <-chan time.Time
		if autoReleaseHeap.Len() > 0 {
			head := autoReleaseHeap.Top().(*htlcReleaseEvent)
			nextReleaseTick = i.tickAt(head.releaseTime)
		}

		select {
		// A sub-systems has just modified the invoice state, so we'll
		// dispatch notifications to all registered clients.
		case event := <-i.invoiceEvents:
			// For backwards compatibility, do not notify all
			// invoice subscribers of cancel and accept events.
			state := event.invoice.State
			if state != channeldb.ContractCanceled &&
				state != channeldb.ContractAccepted {

				i.dispatchToClients(event)
			}
			i.dispatchToSingleClients(event)

		// A new htlc came in for auto-release.
		case event := <-i.htlcAutoReleaseChan:
			log.Debugf("Scheduling auto-release for htlc: "+
				"ref=%v, key=%v at %v",
				event.invoiceRef, event.key, event.releaseTime)

			// We use an independent timer for every htlc rather
			// than a set timer that is reset with every htlc coming
			// in. Otherwise the sender could keep resetting the
			// timer until the broadcast window is entered and our
			// channel is force closed.
			autoReleaseHeap.Push(event)

		// The htlc at the top of the heap needs to be auto-released.
		case <-nextReleaseTick:
			event := autoReleaseHeap.Pop().(*htlcReleaseEvent)
			err := i.cancelSingleHtlc(
				event.invoiceRef, event.key, ResultMppTimeout,
			)
			if err != nil {
				log.Errorf("HTLC timer: %v", err)
			}

		case <-i.quit:
			return
		}
	}
}

// dispatchToSingleClients passes the supplied event to all notification
// clients that subscribed to all the invoice this event applies to.
func (i *InvoiceRegistry) dispatchToSingleClients(event *invoiceEvent) {
	// Dispatch to single invoice subscribers.
	clients := i.copySingleClients()
	for _, client := range clients {
		payHash := client.invoiceRef.PayHash()

		if payHash == nil || *payHash != event.hash {
			continue
		}

		if atomic.LoadUint32(&client.canceled) == 1 {
			log.Errorf("Client(id=%v) has stopped, skipped "+
				"notification for event(pay_hash=%v)",
				client.id, payHash)
			continue
		}
		client.notify(event)
	}
}

// dispatchToClients passes the supplied event to all notification clients that
// subscribed to all invoices. Add and settle indices are used to make sure
// that clients don't receive duplicate or unwanted events.
func (i *InvoiceRegistry) dispatchToClients(event *invoiceEvent) {
	invoice := event.invoice

	clients := i.copyClients()
	for clientID, client := range clients {
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
		// the client then we can skip this one, but only if this isn't
		// an AMP invoice. AMP invoices always remain in the settle
		// state as a base invoice.
		case event.setID == nil && state == channeldb.ContractOpen &&
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
			setID:   event.setID,
		}:
		case <-i.quit:
			return
		}

		// Each time we send a notification to a client, we'll record
		// the latest add/settle index it has. We'll use this to ensure
		// we don't send a notification twice, which can happen if a new
		// event is added while we're catching up a new client.
		invState := event.invoice.State
		switch {
		case invState == channeldb.ContractSettled:
			client.settleIndex = invoice.SettleIndex

		case invState == channeldb.ContractOpen && event.setID == nil:
			client.addIndex = invoice.AddIndex

		// If this is an AMP invoice, then we'll need to use the set ID
		// to keep track of the settle index of the client. AMP
		// invoices never go to the open state, but if a setID is
		// passed, then we know it was just settled and will track the
		// highest settle index so far.
		case invState == channeldb.ContractOpen && event.setID != nil:
			setID := *event.setID
			client.settleIndex = invoice.AMPState[setID].SettleIndex

		default:
			log.Errorf("unexpected invoice state: %v",
				event.invoice.State)
		}
	}
}

// deliverBacklogEvents will attempts to query the invoice database for any
// notifications that the client has missed since it reconnected last.
func (i *InvoiceRegistry) deliverBacklogEvents(client *InvoiceSubscription) error {
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

	invoice, err := i.cdb.LookupInvoice(client.invoiceRef)

	// It is possible that the invoice does not exist yet, but the client is
	// already watching it in anticipation.
	if err == channeldb.ErrInvoiceNotFound ||
		err == channeldb.ErrNoInvoicesCreated {

		return nil
	}
	if err != nil {
		return err
	}

	payHash := client.invoiceRef.PayHash()
	if payHash == nil {
		return nil
	}

	err = client.notify(&invoiceEvent{
		hash:    *payHash,
		invoice: &invoice,
	})
	if err != nil {
		return err
	}

	log.Debugf("Client(id=%v) delivered single backlog event: payHash=%v",
		client.id, payHash)

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

	ref := channeldb.InvoiceRefByHash(paymentHash)
	log.Debugf("Invoice%v: added with terms %v", ref, invoice.Terms)

	addIndex, err := i.cdb.AddInvoice(invoice, paymentHash)
	if err != nil {
		i.Unlock()
		return 0, err
	}

	// Now that we've added the invoice, we'll send dispatch a message to
	// notify the clients of this new invoice.
	i.notifyClients(paymentHash, invoice, nil)
	i.Unlock()

	// InvoiceExpiryWatcher.AddInvoice must not be locked by InvoiceRegistry
	// to avoid deadlock when a new invoice is added while an other is being
	// canceled.
	invoiceExpiryRef := makeInvoiceExpiry(paymentHash, invoice)
	if invoiceExpiryRef != nil {
		i.expiryWatcher.AddInvoices(invoiceExpiryRef)
	}

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
	ref := channeldb.InvoiceRefByHash(rHash)
	return i.cdb.LookupInvoice(ref)
}

// LookupInvoiceByRef looks up an invoice by the given reference, if found
// then we're able to pull the funds pending within an HTLC.
func (i *InvoiceRegistry) LookupInvoiceByRef(
	ref channeldb.InvoiceRef) (channeldb.Invoice, error) {

	return i.cdb.LookupInvoice(ref)
}

// startHtlcTimer starts a new timer via the invoice registry main loop that
// cancels a single htlc on an invoice when the htlc hold duration has passed.
func (i *InvoiceRegistry) startHtlcTimer(invoiceRef channeldb.InvoiceRef,
	key channeldb.CircuitKey, acceptTime time.Time) error {

	releaseTime := acceptTime.Add(i.cfg.HtlcHoldDuration)
	event := &htlcReleaseEvent{
		invoiceRef:  invoiceRef,
		key:         key,
		releaseTime: releaseTime,
	}

	select {
	case i.htlcAutoReleaseChan <- event:
		return nil

	case <-i.quit:
		return ErrShuttingDown
	}
}

// cancelSingleHtlc cancels a single accepted htlc on an invoice. It takes
// a resolution result which will be used to notify subscribed links and
// resolvers of the details of the htlc cancellation.
func (i *InvoiceRegistry) cancelSingleHtlc(invoiceRef channeldb.InvoiceRef,
	key channeldb.CircuitKey, result FailResolutionResult) error {

	i.Lock()
	defer i.Unlock()

	updateInvoice := func(invoice *channeldb.Invoice) (
		*channeldb.InvoiceUpdateDesc, error) {

		// Only allow individual htlc cancellation on open invoices.
		if invoice.State != channeldb.ContractOpen {
			log.Debugf("cancelSingleHtlc: invoice %v no longer "+
				"open", invoiceRef)

			return nil, nil
		}

		// Lookup the current status of the htlc in the database.
		var (
			htlcState channeldb.HtlcState
			setID     *channeldb.SetID
		)
		htlc, ok := invoice.Htlcs[key]
		if !ok {
			// If this is an AMP invoice, then all the HTLCs won't
			// be read out, so we'll consult the other mapping to
			// try to find the HTLC state in question here.
			var found bool
			for ampSetID, htlcSet := range invoice.AMPState {
				ampSetID := ampSetID
				for htlcKey := range htlcSet.InvoiceKeys {
					if htlcKey == key {
						htlcState = htlcSet.State
						setID = &ampSetID

						found = true
						break
					}
				}
			}

			if !found {
				return nil, fmt.Errorf("htlc %v not found", key)
			}
		} else {
			htlcState = htlc.State
		}

		// Cancellation is only possible if the htlc wasn't already
		// resolved.
		if htlcState != channeldb.HtlcStateAccepted {
			log.Debugf("cancelSingleHtlc: htlc %v on invoice %v "+
				"is already resolved", key, invoiceRef)

			return nil, nil
		}

		log.Debugf("cancelSingleHtlc: cancelling htlc %v on invoice %v",
			key, invoiceRef)

		// Return an update descriptor that cancels htlc and keeps
		// invoice open.
		canceledHtlcs := map[channeldb.CircuitKey]struct{}{
			key: {},
		}

		return &channeldb.InvoiceUpdateDesc{
			CancelHtlcs: canceledHtlcs,
			SetID:       setID,
		}, nil
	}

	// Try to mark the specified htlc as canceled in the invoice database.
	// Intercept the update descriptor to set the local updated variable. If
	// no invoice update is performed, we can return early.
	var updated bool
	invoice, err := i.cdb.UpdateInvoice(invoiceRef, nil,
		func(invoice *channeldb.Invoice) (
			*channeldb.InvoiceUpdateDesc, error) {

			updateDesc, err := updateInvoice(invoice)
			if err != nil {
				return nil, err
			}
			updated = updateDesc != nil

			return updateDesc, err
		},
	)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}

	// The invoice has been updated. Notify subscribers of the htlc
	// resolution.
	htlc, ok := invoice.Htlcs[key]
	if !ok {
		return fmt.Errorf("htlc %v not found", key)
	}
	if htlc.State == channeldb.HtlcStateCanceled {
		resolution := NewFailResolution(
			key, int32(htlc.AcceptHeight), result,
		)

		i.notifyHodlSubscribers(resolution)
	}
	return nil
}

// processKeySend just-in-time inserts an invoice if this htlc is a keysend
// htlc.
func (i *InvoiceRegistry) processKeySend(ctx invoiceUpdateCtx) error {
	// Retrieve keysend record if present.
	preimageSlice, ok := ctx.customRecords[record.KeySendType]
	if !ok {
		return nil
	}

	// Cancel htlc is preimage is invalid.
	preimage, err := lntypes.MakePreimage(preimageSlice)
	if err != nil {
		return err
	}
	if preimage.Hash() != ctx.hash {
		return fmt.Errorf("invalid keysend preimage %v for hash %v",
			preimage, ctx.hash)
	}

	// Only allow keysend for non-mpp payments.
	if ctx.mpp != nil {
		return errors.New("no mpp keysend supported")
	}

	// Create an invoice for the htlc amount.
	amt := ctx.amtPaid

	// Set tlv optional feature vector on the invoice. Otherwise we wouldn't
	// be able to pay to it with keysend.
	rawFeatures := lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional,
	)
	features := lnwire.NewFeatureVector(rawFeatures, lnwire.Features)

	// Use the minimum block delta that we require for settling htlcs.
	finalCltvDelta := i.cfg.FinalCltvRejectDelta

	// Pre-check expiry here to prevent inserting an invoice that will not
	// be settled.
	if ctx.expiry < uint32(ctx.currentHeight+finalCltvDelta) {
		return errors.New("final expiry too soon")
	}

	// The invoice database indexes all invoices by payment address, however
	// legacy keysend payment do not have one. In order to avoid a new
	// payment type on-disk wrt. to indexing, we'll continue to insert a
	// blank payment address which is special cased in the insertion logic
	// to not be indexed. In the future, once AMP is merged, this should be
	// replaced by generating a random payment address on the behalf of the
	// sender.
	payAddr := channeldb.BlankPayAddr

	// Create placeholder invoice.
	invoice := &channeldb.Invoice{
		CreationDate: i.cfg.Clock.Now(),
		Terms: channeldb.ContractTerm{
			FinalCltvDelta:  finalCltvDelta,
			Value:           amt,
			PaymentPreimage: &preimage,
			PaymentAddr:     payAddr,
			Features:        features,
		},
	}

	if i.cfg.KeysendHoldTime != 0 {
		invoice.HodlInvoice = true
		invoice.Terms.Expiry = i.cfg.KeysendHoldTime
	}

	// Insert invoice into database. Ignore duplicates, because this
	// may be a replay.
	_, err = i.AddInvoice(invoice, ctx.hash)
	if err != nil && err != channeldb.ErrDuplicateInvoice {
		return err
	}

	return nil
}

// processAMP just-in-time inserts an invoice if this htlc is a keysend
// htlc.
func (i *InvoiceRegistry) processAMP(ctx invoiceUpdateCtx) error {
	// AMP payments MUST also include an MPP record.
	if ctx.mpp == nil {
		return errors.New("no MPP record for AMP")
	}

	// Create an invoice for the total amount expected, provided in the MPP
	// record.
	amt := ctx.mpp.TotalMsat()

	// Set the TLV and MPP optional features on the invoice. We'll also make
	// the AMP features required so that it can't be paid by legacy or MPP
	// htlcs.
	rawFeatures := lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional,
		lnwire.PaymentAddrOptional,
		lnwire.AMPRequired,
	)
	features := lnwire.NewFeatureVector(rawFeatures, lnwire.Features)

	// Use the minimum block delta that we require for settling htlcs.
	finalCltvDelta := i.cfg.FinalCltvRejectDelta

	// Pre-check expiry here to prevent inserting an invoice that will not
	// be settled.
	if ctx.expiry < uint32(ctx.currentHeight+finalCltvDelta) {
		return errors.New("final expiry too soon")
	}

	// We'll use the sender-generated payment address provided in the HTLC
	// to create our AMP invoice.
	payAddr := ctx.mpp.PaymentAddr()

	// Create placeholder invoice.
	invoice := &channeldb.Invoice{
		CreationDate: i.cfg.Clock.Now(),
		Terms: channeldb.ContractTerm{
			FinalCltvDelta:  finalCltvDelta,
			Value:           amt,
			PaymentPreimage: nil,
			PaymentAddr:     payAddr,
			Features:        features,
		},
	}

	// Insert invoice into database. Ignore duplicates payment hashes and
	// payment addrs, this may be a replay or a different HTLC for the AMP
	// invoice.
	_, err := i.AddInvoice(invoice, ctx.hash)
	switch {
	case err == channeldb.ErrDuplicateInvoice:
		return nil
	case err == channeldb.ErrDuplicatePayAddr:
		return nil
	default:
		return err
	}
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
//
// In the case that the htlc is part of a larger set of htlcs that pay to the
// same invoice (multi-path payment), the htlc is held until the set is
// complete. If the set doesn't fully arrive in time, a timer will cancel the
// held htlc.
func (i *InvoiceRegistry) NotifyExitHopHtlc(rHash lntypes.Hash,
	amtPaid lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey channeldb.CircuitKey, hodlChan chan<- interface{},
	payload Payload) (HtlcResolution, error) {

	// Create the update context containing the relevant details of the
	// incoming htlc.
	ctx := invoiceUpdateCtx{
		hash:                 rHash,
		circuitKey:           circuitKey,
		amtPaid:              amtPaid,
		expiry:               expiry,
		currentHeight:        currentHeight,
		finalCltvRejectDelta: i.cfg.FinalCltvRejectDelta,
		customRecords:        payload.CustomRecords(),
		mpp:                  payload.MultiPath(),
		amp:                  payload.AMPRecord(),
	}

	switch {
	// If we are accepting spontaneous AMP payments and this payload
	// contains an AMP record, create an AMP invoice that will be settled
	// below.
	case i.cfg.AcceptAMP && ctx.amp != nil:
		err := i.processAMP(ctx)
		if err != nil {
			ctx.log(fmt.Sprintf("amp error: %v", err))

			return NewFailResolution(
				circuitKey, currentHeight, ResultAmpError,
			), nil
		}

	// If we are accepting spontaneous keysend payments, create a regular
	// invoice that will be settled below. We also enforce that this is only
	// done when no AMP payload is present since it will only be settle-able
	// by regular HTLCs.
	case i.cfg.AcceptKeySend && ctx.amp == nil:
		err := i.processKeySend(ctx)
		if err != nil {
			ctx.log(fmt.Sprintf("keysend error: %v", err))

			return NewFailResolution(
				circuitKey, currentHeight, ResultKeySendError,
			), nil
		}
	}

	// Execute locked notify exit hop logic.
	i.Lock()
	resolution, err := i.notifyExitHopHtlcLocked(&ctx, hodlChan)
	i.Unlock()
	if err != nil {
		return nil, err
	}

	switch r := resolution.(type) {
	// The htlc is held. Start a timer outside the lock if the htlc should
	// be auto-released, because otherwise a deadlock may happen with the
	// main event loop.
	case *htlcAcceptResolution:
		if r.autoRelease {
			err := i.startHtlcTimer(
				ctx.invoiceRef(), circuitKey, r.acceptTime,
			)
			if err != nil {
				return nil, err
			}
		}

		// We return a nil resolution because htlc acceptances are
		// represented as nil resolutions externally.
		// TODO(carla) update calling code to handle accept resolutions.
		return nil, nil

	// A direct resolution was received for this htlc.
	case HtlcResolution:
		return r, nil

	// Fail if an unknown resolution type was received.
	default:
		return nil, errors.New("invalid resolution type")
	}
}

// notifyExitHopHtlcLocked is the internal implementation of NotifyExitHopHtlc
// that should be executed inside the registry lock.
func (i *InvoiceRegistry) notifyExitHopHtlcLocked(
	ctx *invoiceUpdateCtx, hodlChan chan<- interface{}) (
	HtlcResolution, error) {

	// We'll attempt to settle an invoice matching this rHash on disk (if
	// one exists). The callback will update the invoice state and/or htlcs.
	var (
		resolution        HtlcResolution
		updateSubscribers bool
	)
	invoice, err := i.cdb.UpdateInvoice(
		ctx.invoiceRef(),
		(*channeldb.SetID)(ctx.setID()),
		func(inv *channeldb.Invoice) (
			*channeldb.InvoiceUpdateDesc, error) {

			updateDesc, res, err := updateInvoice(ctx, inv)
			if err != nil {
				return nil, err
			}

			// Only send an update if the invoice state was changed.
			updateSubscribers = updateDesc != nil &&
				updateDesc.State != nil

			// Assign resolution to outer scope variable.
			resolution = res

			return updateDesc, nil
		},
	)
	switch err {
	case channeldb.ErrInvoiceNotFound:
		// If the invoice was not found, return a failure resolution
		// with an invoice not found result.
		return NewFailResolution(
			ctx.circuitKey, ctx.currentHeight,
			ResultInvoiceNotFound,
		), nil

	case nil:

	default:
		ctx.log(err.Error())
		return nil, err
	}

	switch res := resolution.(type) {
	case *HtlcFailResolution:
		// Inspect latest htlc state on the invoice. If it is found,
		// we will update the accept height as it was recorded in the
		// invoice database (which occurs in the case where the htlc
		// reached the database in a previous call). If the htlc was
		// not found on the invoice, it was immediately failed so we
		// send the failure resolution as is, which has the current
		// height set as the accept height.
		invoiceHtlc, ok := invoice.Htlcs[ctx.circuitKey]
		if ok {
			res.AcceptHeight = int32(invoiceHtlc.AcceptHeight)
		}

		ctx.log(fmt.Sprintf("failure resolution result "+
			"outcome: %v, at accept height: %v",
			res.Outcome, res.AcceptHeight))

		// Some failures apply to the entire HTLC set. Break here if
		// this isn't one of them.
		if !res.Outcome.IsSetFailure() {
			break
		}

		// Also cancel any HTLCs in the HTLC set that are also in the
		// canceled state with the same failure result.
		setID := ctx.setID()
		canceledHtlcSet := invoice.HTLCSet(setID, channeldb.HtlcStateCanceled)
		for key, htlc := range canceledHtlcSet {
			htlcFailResolution := NewFailResolution(
				key, int32(htlc.AcceptHeight), res.Outcome,
			)

			i.notifyHodlSubscribers(htlcFailResolution)
		}

	// If the htlc was settled, we will settle any previously accepted
	// htlcs and notify our peer to settle them.
	case *HtlcSettleResolution:
		ctx.log(fmt.Sprintf("settle resolution result "+
			"outcome: %v, at accept height: %v",
			res.Outcome, res.AcceptHeight))

		// Also settle any previously accepted htlcs. If a htlc is
		// marked as settled, we should follow now and settle the htlc
		// with our peer.
		setID := ctx.setID()
		settledHtlcSet := invoice.HTLCSet(setID, channeldb.HtlcStateSettled)
		for key, htlc := range settledHtlcSet {
			preimage := res.Preimage
			if htlc.AMP != nil && htlc.AMP.Preimage != nil {
				preimage = *htlc.AMP.Preimage
			}

			// Notify subscribers that the htlcs should be settled
			// with our peer. Note that the outcome of the
			// resolution is set based on the outcome of the single
			// htlc that we just settled, so may not be accurate
			// for all htlcs.
			htlcSettleResolution := NewSettleResolution(
				preimage, key,
				int32(htlc.AcceptHeight), res.Outcome,
			)

			// Notify subscribers that the htlc should be settled
			// with our peer.
			i.notifyHodlSubscribers(htlcSettleResolution)
		}

		// If concurrent payments were attempted to this invoice before
		// the current one was ultimately settled, cancel back any of
		// the HTLCs immediately. As a result of the settle, the HTLCs
		// in other HTLC sets are automatically converted to a canceled
		// state when updating the invoice.
		//
		// TODO(roasbeef): can remove now??
		canceledHtlcSet := invoice.HTLCSetCompliment(
			setID, channeldb.HtlcStateCanceled,
		)
		for key, htlc := range canceledHtlcSet {
			htlcFailResolution := NewFailResolution(
				key, int32(htlc.AcceptHeight),
				ResultInvoiceAlreadySettled,
			)

			i.notifyHodlSubscribers(htlcFailResolution)
		}

	// If we accepted the htlc, subscribe to the hodl invoice and return
	// an accept resolution with the htlc's accept time on it.
	case *htlcAcceptResolution:
		invoiceHtlc, ok := invoice.Htlcs[ctx.circuitKey]
		if !ok {
			return nil, fmt.Errorf("accepted htlc: %v not"+
				" present on invoice: %x", ctx.circuitKey,
				ctx.hash[:])
		}

		// Determine accepted height of this htlc. If the htlc reached
		// the invoice database (possibly in a previous call to the
		// invoice registry), we'll take the original accepted height
		// as it was recorded in the database.
		acceptHeight := int32(invoiceHtlc.AcceptHeight)

		ctx.log(fmt.Sprintf("accept resolution result "+
			"outcome: %v, at accept height: %v",
			res.outcome, acceptHeight))

		// Auto-release the htlc if the invoice is still open. It can
		// only happen for mpp payments that there are htlcs in state
		// Accepted while the invoice is Open.
		if invoice.State == channeldb.ContractOpen {
			res.acceptTime = invoiceHtlc.AcceptTime
			res.autoRelease = true
		}

		// If we have fully accepted the set of htlcs for this invoice,
		// we can now add it to our invoice expiry watcher. We do not
		// add invoices before they are fully accepted, because it is
		// possible that we MppTimeout the htlcs, and then our relevant
		// expiry height could change.
		if res.outcome == resultAccepted {
			expiry := makeInvoiceExpiry(ctx.hash, invoice)
			i.expiryWatcher.AddInvoices(expiry)
		}

		i.hodlSubscribe(hodlChan, ctx.circuitKey)

	default:
		panic("unknown action")
	}

	// Now that the links have been notified of any state changes to their
	// HTLCs, we'll go ahead and notify any clients wiaiting on the invoice
	// state changes.
	if updateSubscribers {
		// We'll add a setID onto the notification, but only if this is
		// an AMP invoice being settled.
		var setID *[32]byte
		if _, ok := resolution.(*HtlcSettleResolution); ok {
			setID = ctx.setID()
		}

		i.notifyClients(ctx.hash, invoice, setID)
	}

	return resolution, nil
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
				Preimage: &preimage,
			},
		}, nil
	}

	hash := preimage.Hash()
	invoiceRef := channeldb.InvoiceRefByHash(hash)
	invoice, err := i.cdb.UpdateInvoice(invoiceRef, nil, updateInvoice)
	if err != nil {
		log.Errorf("SettleHodlInvoice with preimage %v: %v",
			preimage, err)

		return err
	}

	log.Debugf("Invoice%v: settled with preimage %v", invoiceRef,
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

		resolution := NewSettleResolution(
			preimage, key, int32(htlc.AcceptHeight), ResultSettled,
		)

		i.notifyHodlSubscribers(resolution)
	}
	i.notifyClients(hash, invoice, nil)

	return nil
}

// CancelInvoice attempts to cancel the invoice corresponding to the passed
// payment hash.
func (i *InvoiceRegistry) CancelInvoice(payHash lntypes.Hash) error {
	return i.cancelInvoiceImpl(payHash, true)
}

// shouldCancel examines the state of an invoice and whether we want to
// cancel already accepted invoices, taking our force cancel boolean into
// account. This is pulled out into its own function so that tests that mock
// cancelInvoiceImpl can reuse this logic.
func shouldCancel(state channeldb.ContractState, cancelAccepted bool) bool {
	if state != channeldb.ContractAccepted {
		return true
	}

	// If the invoice is accepted, we should only cancel if we want to
	// force cancellation of accepted invoices.
	return cancelAccepted
}

// cancelInvoice attempts to cancel the invoice corresponding to the passed
// payment hash. Accepted invoices will only be canceled if explicitly
// requested to do so. It notifies subscribing links and resolvers that
// the associated htlcs were canceled if they change state.
func (i *InvoiceRegistry) cancelInvoiceImpl(payHash lntypes.Hash,
	cancelAccepted bool) error {

	i.Lock()
	defer i.Unlock()

	ref := channeldb.InvoiceRefByHash(payHash)
	log.Debugf("Invoice%v: canceling invoice", ref)

	updateInvoice := func(invoice *channeldb.Invoice) (
		*channeldb.InvoiceUpdateDesc, error) {

		if !shouldCancel(invoice.State, cancelAccepted) {
			return nil, nil
		}

		// Move invoice to the canceled state. Rely on validation in
		// channeldb to return an error if the invoice is already
		// settled or canceled.
		return &channeldb.InvoiceUpdateDesc{
			State: &channeldb.InvoiceStateUpdateDesc{
				NewState: channeldb.ContractCanceled,
			},
		}, nil
	}

	invoiceRef := channeldb.InvoiceRefByHash(payHash)
	invoice, err := i.cdb.UpdateInvoice(invoiceRef, nil, updateInvoice)

	// Implement idempotency by returning success if the invoice was already
	// canceled.
	if err == channeldb.ErrInvoiceAlreadyCanceled {
		log.Debugf("Invoice%v: already canceled", ref)
		return nil
	}
	if err != nil {
		return err
	}

	// Return without cancellation if the invoice state is ContractAccepted.
	if invoice.State == channeldb.ContractAccepted {
		log.Debugf("Invoice%v: remains accepted as cancel wasn't"+
			"explicitly requested.", ref)
		return nil
	}

	log.Debugf("Invoice%v: canceled", ref)

	// In the callback, some htlcs may have been moved to the canceled
	// state. We now go through all of these and notify links and resolvers
	// that are waiting for resolution. Any htlcs that were already canceled
	// before, will be notified again. This isn't necessary but doesn't hurt
	// either.
	for key, htlc := range invoice.Htlcs {
		if htlc.State != channeldb.HtlcStateCanceled {
			continue
		}

		i.notifyHodlSubscribers(
			NewFailResolution(
				key, int32(htlc.AcceptHeight), ResultCanceled,
			),
		)
	}
	i.notifyClients(payHash, invoice, nil)

	// Attempt to also delete the invoice if requested through the registry
	// config.
	if i.cfg.GcCanceledInvoicesOnTheFly {
		// Assemble the delete reference and attempt to delete through
		// the invocice from the DB.
		deleteRef := channeldb.InvoiceDeleteRef{
			PayHash:     payHash,
			AddIndex:    invoice.AddIndex,
			SettleIndex: invoice.SettleIndex,
		}
		if invoice.Terms.PaymentAddr != channeldb.BlankPayAddr {
			deleteRef.PayAddr = &invoice.Terms.PaymentAddr
		}

		err = i.cdb.DeleteInvoice(
			[]channeldb.InvoiceDeleteRef{deleteRef},
		)
		// If by any chance deletion failed, then log it instead of
		// returning the error, as the invoice itsels has already been
		// canceled.
		if err != nil {
			log.Warnf("Invoice%v could not be deleted: %v",
				ref, err)
		}
	}

	return nil
}

// notifyClients notifies all currently registered invoice notification clients
// of a newly added/settled invoice.
func (i *InvoiceRegistry) notifyClients(hash lntypes.Hash,
	invoice *channeldb.Invoice, setID *[32]byte) {

	event := &invoiceEvent{
		invoice: invoice,
		hash:    hash,
		setID:   setID,
	}

	select {
	case i.invoiceEvents <- event:
	case <-i.quit:
	}
}

// invoiceSubscriptionKit defines that are common to both all invoice
// subscribers and single invoice subscribers.
type invoiceSubscriptionKit struct {
	id uint32 // nolint:structcheck

	// quit is a chan mouted to InvoiceRegistry that signals a shutdown.
	quit chan struct{}

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

	// SettledInvoices is a channel that we'll use to send all settled
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

	invoiceRef channeldb.InvoiceRef

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

	i.ntfnQueue.Stop()
	close(i.cancelChan)

	i.wg.Wait()
}

func (i *invoiceSubscriptionKit) notify(event *invoiceEvent) error {
	select {
	case i.ntfnQueue.ChanIn() <- event:
	case <-i.quit:
		return ErrShuttingDown
	}

	return nil
}

// SubscribeNotifications returns an InvoiceSubscription which allows the
// caller to receive async notifications when any invoices are settled or
// added. The invoiceIndex parameter is a streaming "checkpoint". We'll start
// by first sending out all new events with an invoice index _greater_ than
// this value. Afterwards, we'll send out real-time notifications.
func (i *InvoiceRegistry) SubscribeNotifications(
	addIndex, settleIndex uint64) (*InvoiceSubscription, error) {

	client := &InvoiceSubscription{
		NewInvoices:     make(chan *channeldb.Invoice),
		SettledInvoices: make(chan *channeldb.Invoice),
		addIndex:        addIndex,
		settleIndex:     settleIndex,
		invoiceSubscriptionKit: invoiceSubscriptionKit{
			quit:       i.quit,
			ntfnQueue:  queue.NewConcurrentQueue(20),
			cancelChan: make(chan struct{}),
		},
	}
	client.ntfnQueue.Start()

	// Always increment by 1 first, and our client ID will start with 1,
	// not 0.
	client.id = atomic.AddUint32(&i.nextClientID, 1)

	// Before we register this new invoice subscription, we'll launch a new
	// goroutine that will proxy all notifications appended to the end of
	// the concurrent queue to the two client-side channels the caller will
	// feed off of.
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		defer i.deleteClient(client.id)

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
				switch {
				// AMP invoices never move to settled, but will
				// be sent with a set ID if an HTLC set is
				// being settled.
				case state == channeldb.ContractOpen &&
					invoiceEvent.setID != nil:
					fallthrough
				case state == channeldb.ContractSettled:
					targetChan = client.SettledInvoices

				case state == channeldb.ContractOpen:
					targetChan = client.NewInvoices

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

	// Query the database to see if based on the provided addIndex and
	// settledIndex we need to deliver any backlog notifications.
	err := i.deliverBacklogEvents(client)
	if err != nil {
		return nil, err
	}

	log.Infof("New invoice subscription client: id=%v", client.id)

	i.Lock()
	// With the backlog notifications delivered (if any), we'll add this to
	// our active subscriptions.
	i.notificationClients[client.id] = client
	i.Unlock()

	return client, nil
}

// SubscribeSingleInvoice returns an SingleInvoiceSubscription which allows the
// caller to receive async notifications for a specific invoice.
func (i *InvoiceRegistry) SubscribeSingleInvoice(
	hash lntypes.Hash) (*SingleInvoiceSubscription, error) {

	client := &SingleInvoiceSubscription{
		Updates: make(chan *channeldb.Invoice),
		invoiceSubscriptionKit: invoiceSubscriptionKit{
			quit:       i.quit,
			ntfnQueue:  queue.NewConcurrentQueue(20),
			cancelChan: make(chan struct{}),
		},
		invoiceRef: channeldb.InvoiceRefByHash(hash),
	}
	client.ntfnQueue.Start()

	// Always increment by 1 first, and our client ID will start with 1,
	// not 0.
	client.id = atomic.AddUint32(&i.nextClientID, 1)

	// Before we register this new invoice subscription, we'll launch a new
	// goroutine that will proxy all notifications appended to the end of
	// the concurrent queue to the two client-side channels the caller will
	// feed off of.
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		defer i.deleteClient(client.id)

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

	err := i.deliverSingleBacklogEvents(client)
	if err != nil {
		return nil, err
	}

	log.Infof("New single invoice subscription client: id=%v, ref=%v",
		client.id, client.invoiceRef)

	i.Lock()
	i.singleNotificationClients[client.id] = client
	i.Unlock()

	return client, nil
}

// notifyHodlSubscribers sends out the htlc resolution to all current
// subscribers.
func (i *InvoiceRegistry) notifyHodlSubscribers(htlcResolution HtlcResolution) {
	subscribers, ok := i.hodlSubscriptions[htlcResolution.CircuitKey()]
	if !ok {
		return
	}

	// Notify all interested subscribers and remove subscription from both
	// maps. The subscription can be removed as there only ever will be a
	// single resolution for each hash.
	for subscriber := range subscribers {
		select {
		case subscriber <- htlcResolution:
		case <-i.quit:
			return
		}

		delete(
			i.hodlReverseSubscriptions[subscriber],
			htlcResolution.CircuitKey(),
		)
	}

	delete(i.hodlSubscriptions, htlcResolution.CircuitKey())
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

// copySingleClients copies i.SingleInvoiceSubscription inside a lock. This is
// useful when we need to iterate the map to send notifications.
func (i *InvoiceRegistry) copySingleClients() map[uint32]*SingleInvoiceSubscription {
	i.RLock()
	defer i.RUnlock()

	clients := make(map[uint32]*SingleInvoiceSubscription)
	for k, v := range i.singleNotificationClients {
		clients[k] = v
	}
	return clients
}

// copyClients copies i.notificationClients inside a lock. This is useful when
// we need to iterate the map to send notifications.
func (i *InvoiceRegistry) copyClients() map[uint32]*InvoiceSubscription {
	i.RLock()
	defer i.RUnlock()

	clients := make(map[uint32]*InvoiceSubscription)
	for k, v := range i.notificationClients {
		clients[k] = v
	}
	return clients
}

// deleteClient removes a client by its ID inside a lock. Noop if the client is
// not found.
func (i *InvoiceRegistry) deleteClient(clientID uint32) {
	i.Lock()
	defer i.Unlock()

	log.Infof("Cancelling invoice subscription for client=%v", clientID)
	delete(i.notificationClients, clientID)
	delete(i.singleNotificationClients, clientID)
}
