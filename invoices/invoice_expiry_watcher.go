package invoices

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/zpay32"
)

// invoiceExpiry holds and invoice's payment hash and its expiry. This
// is used to order invoices by their expiry for cancellation.
type invoiceExpiry struct {
	PaymentHash lntypes.Hash
	Expiry      time.Time
	Keysend     bool
}

// Less implements PriorityQueueItem.Less such that the top item in the
// priorty queue will be the one that expires next.
func (e invoiceExpiry) Less(other queue.PriorityQueueItem) bool {
	return e.Expiry.Before(other.(*invoiceExpiry).Expiry)
}

// InvoiceExpiryWatcher handles automatic invoice cancellation of expried
// invoices. Upon start InvoiceExpiryWatcher will retrieve all pending (not yet
// settled or canceled) invoices invoices to its watcing queue. When a new
// invoice is added to the InvoiceRegistry, it'll be forarded to the
// InvoiceExpiryWatcher and will end up in the watching queue as well.
// If any of the watched invoices expire, they'll be removed from the watching
// queue and will be cancelled through InvoiceRegistry.CancelInvoice().
type InvoiceExpiryWatcher struct {
	sync.Mutex
	started bool

	// clock is the clock implementation that InvoiceExpiryWatcher uses.
	// It is useful for testing.
	clock clock.Clock

	// cancelInvoice is a template method that cancels an expired invoice.
	cancelInvoice func(lntypes.Hash, bool) error

	// expiryQueue holds invoiceExpiry items and is used to find the next
	// invoice to expire.
	expiryQueue queue.PriorityQueue

	// newInvoices channel is used to wake up the main loop when a new invoices
	// is added.
	newInvoices chan []*invoiceExpiry

	wg sync.WaitGroup

	// quit signals InvoiceExpiryWatcher to stop.
	quit chan struct{}
}

// NewInvoiceExpiryWatcher creates a new InvoiceExpiryWatcher instance.
func NewInvoiceExpiryWatcher(clock clock.Clock) *InvoiceExpiryWatcher {
	return &InvoiceExpiryWatcher{
		clock:       clock,
		newInvoices: make(chan []*invoiceExpiry),
		quit:        make(chan struct{}),
	}
}

// Start starts the the subscription handler and the main loop. Start() will
// return with error if InvoiceExpiryWatcher is already started. Start()
// expects a cancellation function passed that will be use to cancel expired
// invoices by their payment hash.
func (ew *InvoiceExpiryWatcher) Start(
	cancelInvoice func(lntypes.Hash, bool) error) error {

	ew.Lock()
	defer ew.Unlock()

	if ew.started {
		return fmt.Errorf("InvoiceExpiryWatcher already started")
	}

	ew.started = true
	ew.cancelInvoice = cancelInvoice
	ew.wg.Add(1)
	go ew.mainLoop()

	return nil
}

// Stop quits the expiry handler loop and waits for InvoiceExpiryWatcher to
// fully stop.
func (ew *InvoiceExpiryWatcher) Stop() {
	ew.Lock()
	defer ew.Unlock()

	if ew.started {
		// Signal subscriptionHandler to quit and wait for it to return.
		close(ew.quit)
		ew.wg.Wait()
		ew.started = false
	}
}

// prepareInvoice checks if the passed invoice may be canceled and calculates
// the expiry time.
func (ew *InvoiceExpiryWatcher) prepareInvoice(
	paymentHash lntypes.Hash, invoice *channeldb.Invoice) *invoiceExpiry {

	if invoice.State != channeldb.ContractOpen {
		log.Debugf("Invoice not added to expiry watcher: %v", paymentHash)
		return nil
	}

	realExpiry := invoice.Terms.Expiry
	if realExpiry == 0 {
		realExpiry = zpay32.DefaultInvoiceExpiry
	}

	expiry := invoice.CreationDate.Add(realExpiry)
	return &invoiceExpiry{
		PaymentHash: paymentHash,
		Expiry:      expiry,
		Keysend:     len(invoice.PaymentRequest) == 0,
	}
}

// AddInvoices adds multiple invoices to the InvoiceExpiryWatcher.
func (ew *InvoiceExpiryWatcher) AddInvoices(
	invoices []channeldb.InvoiceWithPaymentHash) {

	invoicesWithExpiry := make([]*invoiceExpiry, 0, len(invoices))
	for _, invoiceWithPaymentHash := range invoices {
		newInvoiceExpiry := ew.prepareInvoice(
			invoiceWithPaymentHash.PaymentHash, &invoiceWithPaymentHash.Invoice,
		)
		if newInvoiceExpiry != nil {
			invoicesWithExpiry = append(invoicesWithExpiry, newInvoiceExpiry)
		}
	}

	if len(invoicesWithExpiry) > 0 {
		log.Debugf("Added %d invoices to the expiry watcher",
			len(invoicesWithExpiry))
		select {
		case ew.newInvoices <- invoicesWithExpiry:
		// Select on quit too so that callers won't get blocked in case
		// of concurrent shutdown.
		case <-ew.quit:
		}
	}
}

// AddInvoice adds a new invoice to the InvoiceExpiryWatcher. This won't check
// if the invoice is already added and will only add invoices with ContractOpen
// state.
func (ew *InvoiceExpiryWatcher) AddInvoice(
	paymentHash lntypes.Hash, invoice *channeldb.Invoice) {

	newInvoiceExpiry := ew.prepareInvoice(paymentHash, invoice)
	if newInvoiceExpiry != nil {
		log.Debugf("Adding invoice '%v' to expiry watcher, expiration: %v",
			paymentHash, newInvoiceExpiry.Expiry)

		select {
		case ew.newInvoices <- []*invoiceExpiry{newInvoiceExpiry}:
		// Select on quit too so that callers won't get blocked in case
		// of concurrent shutdown.
		case <-ew.quit:
		}
	}
}

// nextExpiry returns a Time chan to wait on until the next invoice expires.
// If there are no active invoices, then it'll simply wait indefinitely.
func (ew *InvoiceExpiryWatcher) nextExpiry() <-chan time.Time {
	if !ew.expiryQueue.Empty() {
		top := ew.expiryQueue.Top().(*invoiceExpiry)
		return ew.clock.TickAfter(top.Expiry.Sub(ew.clock.Now()))
	}

	return nil
}

// cancelNextExpiredInvoice will cancel the next expired invoice and removes
// it from the expiry queue.
func (ew *InvoiceExpiryWatcher) cancelNextExpiredInvoice() {
	if !ew.expiryQueue.Empty() {
		top := ew.expiryQueue.Top().(*invoiceExpiry)
		if !top.Expiry.Before(ew.clock.Now()) {
			return
		}

		// Don't force-cancel already accepted invoices. An exception to
		// this are auto-generated keysend invoices. Because those move
		// to the Accepted state directly after being opened, the expiry
		// field would never be used. Enabling cancellation for accepted
		// keysend invoices creates a safety mechanism that can prevents
		// channel force-closes.
		err := ew.cancelInvoice(top.PaymentHash, top.Keysend)
		if err != nil && err != channeldb.ErrInvoiceAlreadySettled &&
			err != channeldb.ErrInvoiceAlreadyCanceled {

			log.Errorf("Unable to cancel invoice: %v", top.PaymentHash)
		}

		ew.expiryQueue.Pop()
	}
}

// mainLoop is a goroutine that receives new invoices and handles cancellation
// of expired invoices.
func (ew *InvoiceExpiryWatcher) mainLoop() {
	defer ew.wg.Done()

	for {
		// Cancel any invoices that may have expired.
		ew.cancelNextExpiredInvoice()

		select {

		case invoicesWithExpiry := <-ew.newInvoices:
			// Take newly forwarded invoices with higher priority
			// in order to not block the newInvoices channel.
			for _, invoiceWithExpiry := range invoicesWithExpiry {
				ew.expiryQueue.Push(invoiceWithExpiry)
			}
			continue

		default:
			select {

			case <-ew.nextExpiry():
				// Wait until the next invoice expires.
				continue

			case invoicesWithExpiry := <-ew.newInvoices:
				for _, invoiceWithExpiry := range invoicesWithExpiry {
					ew.expiryQueue.Push(invoiceWithExpiry)
				}

			case <-ew.quit:
				return
			}
		}
	}
}
