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

// invoiceExpiry is a vanity interface for different invoice expiry types
// which implement the priority queue item interface, used to improve code
// readability.
type invoiceExpiry queue.PriorityQueueItem

// Compile time assertion that invoiceExpiryTs implements invoiceExpiry.
var _ invoiceExpiry = (*invoiceExpiryTs)(nil)

// invoiceExpiryTs holds and invoice's payment hash and its expiry. This
// is used to order invoices by their expiry time for cancellation.
type invoiceExpiryTs struct {
	PaymentHash lntypes.Hash
	Expiry      time.Time
	Keysend     bool
}

// Less implements PriorityQueueItem.Less such that the top item in the
// priorty queue will be the one that expires next.
func (e invoiceExpiryTs) Less(other queue.PriorityQueueItem) bool {
	return e.Expiry.Before(other.(*invoiceExpiryTs).Expiry)
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

	// timestampExpiryQueue holds invoiceExpiry items and is used to find
	// the next invoice to expire.
	timestampExpiryQueue queue.PriorityQueue

	// newInvoices channel is used to wake up the main loop when a new
	// invoices is added.
	newInvoices chan []invoiceExpiry

	wg sync.WaitGroup

	// quit signals InvoiceExpiryWatcher to stop.
	quit chan struct{}
}

// NewInvoiceExpiryWatcher creates a new InvoiceExpiryWatcher instance.
func NewInvoiceExpiryWatcher(clock clock.Clock) *InvoiceExpiryWatcher {
	return &InvoiceExpiryWatcher{
		clock:       clock,
		newInvoices: make(chan []invoiceExpiry),
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

// makeInvoiceExpiry checks if the passed invoice may be canceled and calculates
// the expiry time and creates a slimmer invoiceExpiry implementation.
func makeInvoiceExpiry(paymentHash lntypes.Hash,
	invoice *channeldb.Invoice) invoiceExpiry {

	switch invoice.State {
	// If we have an open invoice with no htlcs, we want to expire the
	// invoice based on timestamp
	case channeldb.ContractOpen:
		return makeTimestampExpiry(paymentHash, invoice)

	default:
		log.Debugf("Invoice not added to expiry watcher: %v",
			paymentHash)

		return nil
	}
}

// makeTimestampExpiry creates a timestamp-based expiry entry.
func makeTimestampExpiry(paymentHash lntypes.Hash,
	invoice *channeldb.Invoice) *invoiceExpiryTs {

	if invoice.State != channeldb.ContractOpen {
		return nil
	}

	realExpiry := invoice.Terms.Expiry
	if realExpiry == 0 {
		realExpiry = zpay32.DefaultInvoiceExpiry
	}

	expiry := invoice.CreationDate.Add(realExpiry)
	return &invoiceExpiryTs{
		PaymentHash: paymentHash,
		Expiry:      expiry,
		Keysend:     len(invoice.PaymentRequest) == 0,
	}
}

// AddInvoices adds invoices to the InvoiceExpiryWatcher.
func (ew *InvoiceExpiryWatcher) AddInvoices(invoices ...invoiceExpiry) {
	if len(invoices) > 0 {
		select {
		case ew.newInvoices <- invoices:
			log.Debugf("Added %d invoices to the expiry watcher",
				len(invoices))

		// Select on quit too so that callers won't get blocked in case
		// of concurrent shutdown.
		case <-ew.quit:
		}
	}
}

// nextTimestampExpiry returns a Time chan to wait on until the next invoice
// expires. If there are no active invoices, then it'll simply wait
// indefinitely.
func (ew *InvoiceExpiryWatcher) nextTimestampExpiry() <-chan time.Time {
	if !ew.timestampExpiryQueue.Empty() {
		top := ew.timestampExpiryQueue.Top().(*invoiceExpiryTs)
		return ew.clock.TickAfter(top.Expiry.Sub(ew.clock.Now()))
	}

	return nil
}

// cancelNextExpiredInvoice will cancel the next expired invoice and removes
// it from the expiry queue.
func (ew *InvoiceExpiryWatcher) cancelNextExpiredInvoice() {
	if !ew.timestampExpiryQueue.Empty() {
		top := ew.timestampExpiryQueue.Top().(*invoiceExpiryTs)
		if !top.Expiry.Before(ew.clock.Now()) {
			return
		}

		// Don't force-cancel already accepted invoices. An exception to
		// this are auto-generated keysend invoices. Because those move
		// to the Accepted state directly after being opened, the expiry
		// field would never be used. Enabling cancellation for accepted
		// keysend invoices creates a safety mechanism that can prevents
		// channel force-closes.
		ew.expireInvoice(top.PaymentHash, top.Keysend)
		ew.timestampExpiryQueue.Pop()
	}
}

// expireInvoice attempts to expire an invoice and logs an error if we get an
// unexpected error.
func (ew *InvoiceExpiryWatcher) expireInvoice(hash lntypes.Hash, force bool) {
	err := ew.cancelInvoice(hash, force)
	switch err {
	case nil:

	case channeldb.ErrInvoiceAlreadyCanceled:

	case channeldb.ErrInvoiceAlreadySettled:

	default:
		log.Errorf("Unable to cancel invoice: %v: %v", hash, err)
	}
}

// pushInvoices adds invoices to be expired to their relevant queue.
func (ew *InvoiceExpiryWatcher) pushInvoices(invoices []invoiceExpiry) {
	for _, inv := range invoices {
		// Switch on the type of entry we have. We need to check nil
		// on the implementation of the interface because the interface
		// itself is non-nil.
		switch expiry := inv.(type) {
		case *invoiceExpiryTs:
			if expiry != nil {
				ew.timestampExpiryQueue.Push(expiry)
			}

		default:
			log.Errorf("unexpected queue item: %T", inv)
		}
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

		case newInvoices := <-ew.newInvoices:
			// Take newly forwarded invoices with higher priority
			// in order to not block the newInvoices channel.
			ew.pushInvoices(newInvoices)
			continue

		default:
			select {

			case <-ew.nextTimestampExpiry():
				// Wait until the next invoice expires.
				continue

			case newInvoices := <-ew.newInvoices:
				ew.pushInvoices(newInvoices)

			case <-ew.quit:
				return
			}
		}
	}
}
