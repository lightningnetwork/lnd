package invoices

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
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
// priority queue will be the one that expires next.
func (e invoiceExpiryTs) Less(other queue.PriorityQueueItem) bool {
	return e.Expiry.Before(other.(*invoiceExpiryTs).Expiry)
}

// Compile time assertion that invoiceExpiryHeight implements invoiceExpiry.
var _ invoiceExpiry = (*invoiceExpiryHeight)(nil)

// invoiceExpiryHeight holds information about an invoice which can be used to
// cancel it based on its expiry height.
type invoiceExpiryHeight struct {
	paymentHash  lntypes.Hash
	expiryHeight uint32
}

// Less implements PriorityQueueItem.Less such that the top item in the
// priority queue is the lowest block height.
func (b invoiceExpiryHeight) Less(other queue.PriorityQueueItem) bool {
	return b.expiryHeight < other.(*invoiceExpiryHeight).expiryHeight
}

// expired returns a boolean that indicates whether this entry has expired,
// taking our expiry delta into account.
func (b invoiceExpiryHeight) expired(currentHeight, delta uint32) bool {
	return currentHeight+delta >= b.expiryHeight
}

// InvoiceExpiryWatcher handles automatic invoice cancellation of expired
// invoices. Upon start InvoiceExpiryWatcher will retrieve all pending (not yet
// settled or canceled) invoices invoices to its watching queue. When a new
// invoice is added to the InvoiceRegistry, it'll be forwarded to the
// InvoiceExpiryWatcher and will end up in the watching queue as well.
// If any of the watched invoices expire, they'll be removed from the watching
// queue and will be cancelled through InvoiceRegistry.CancelInvoice().
type InvoiceExpiryWatcher struct {
	sync.Mutex
	started bool

	// clock is the clock implementation that InvoiceExpiryWatcher uses.
	// It is useful for testing.
	clock clock.Clock

	// notifier provides us with block height updates.
	notifier chainntnfs.ChainNotifier

	// blockExpiryDelta is the number of blocks before a htlc's expiry that
	// we expire the invoice based on expiry height. We use a delta because
	// we will go to some delta before our expiry, so we want to cancel
	// before this to prevent force closes.
	blockExpiryDelta uint32

	// currentHeight is the current block height.
	currentHeight uint32

	// currentHash is the block hash for our current height.
	currentHash *chainhash.Hash

	// cancelInvoice is a template method that cancels an expired invoice.
	cancelInvoice func(lntypes.Hash, bool) error

	// timestampExpiryQueue holds invoiceExpiry items and is used to find
	// the next invoice to expire.
	timestampExpiryQueue queue.PriorityQueue

	// blockExpiryQueue holds blockExpiry items and is used to find the
	// next invoice to expire based on block height. Only hold invoices
	// with active htlcs are added to this queue, because they require
	// manual cancellation when the hltc is going to time out. Items in
	// this queue may already be in the timestampExpiryQueue, this is ok
	// because they will not be expired based on timestamp if they have
	// active htlcs.
	blockExpiryQueue queue.PriorityQueue

	// newInvoices channel is used to wake up the main loop when a new
	// invoices is added.
	newInvoices chan []invoiceExpiry

	wg sync.WaitGroup

	// quit signals InvoiceExpiryWatcher to stop.
	quit chan struct{}
}

// NewInvoiceExpiryWatcher creates a new InvoiceExpiryWatcher instance.
func NewInvoiceExpiryWatcher(clock clock.Clock,
	expiryDelta, startHeight uint32, startHash *chainhash.Hash,
	notifier chainntnfs.ChainNotifier) *InvoiceExpiryWatcher {

	return &InvoiceExpiryWatcher{
		clock:            clock,
		notifier:         notifier,
		blockExpiryDelta: expiryDelta,
		currentHeight:    startHeight,
		currentHash:      startHash,
		newInvoices:      make(chan []invoiceExpiry),
		quit:             make(chan struct{}),
	}
}

// Start starts the subscription handler and the main loop. Start() will
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

	ntfn, err := ew.notifier.RegisterBlockEpochNtfn(&chainntnfs.BlockEpoch{
		Height: int32(ew.currentHeight),
		Hash:   ew.currentHash,
	})
	if err != nil {
		return err
	}

	ew.wg.Add(1)
	go ew.mainLoop(ntfn)

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
	invoice *Invoice) invoiceExpiry {

	switch invoice.State {
	// If we have an open invoice with no htlcs, we want to expire the
	// invoice based on timestamp
	case ContractOpen:
		return makeTimestampExpiry(paymentHash, invoice)

	// If an invoice has active htlcs, we want to expire it based on block
	// height. We only do this for hodl invoices, since regular invoices
	// should resolve themselves automatically.
	case ContractAccepted:
		if !invoice.HodlInvoice {
			log.Debugf("Invoice in accepted state not added to "+
				"expiry watcher: %v", paymentHash)

			return nil
		}

		var minHeight uint32
		for _, htlc := range invoice.Htlcs {
			// We only care about accepted htlcs, since they will
			// trigger force-closes.
			if htlc.State != HtlcStateAccepted {
				continue
			}

			if minHeight == 0 || htlc.Expiry < minHeight {
				minHeight = htlc.Expiry
			}
		}

		return makeHeightExpiry(paymentHash, minHeight)

	default:
		log.Debugf("Invoice not added to expiry watcher: %v",
			paymentHash)

		return nil
	}
}

// makeTimestampExpiry creates a timestamp-based expiry entry.
func makeTimestampExpiry(paymentHash lntypes.Hash,
	invoice *Invoice) *invoiceExpiryTs {

	if invoice.State != ContractOpen {
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

// makeHeightExpiry creates height-based expiry for an invoice based on its
// lowest htlc expiry height.
func makeHeightExpiry(paymentHash lntypes.Hash,
	minHeight uint32) *invoiceExpiryHeight {

	if minHeight == 0 {
		log.Warnf("make height expiry called with 0 height")
		return nil
	}

	return &invoiceExpiryHeight{
		paymentHash:  paymentHash,
		expiryHeight: minHeight,
	}
}

// AddInvoices adds invoices to the InvoiceExpiryWatcher.
func (ew *InvoiceExpiryWatcher) AddInvoices(invoices ...invoiceExpiry) {
	if len(invoices) == 0 {
		return
	}

	select {
	case ew.newInvoices <- invoices:
		log.Debugf("Added %d invoices to the expiry watcher",
			len(invoices))

	// Select on quit too so that callers won't get blocked in case
	// of concurrent shutdown.
	case <-ew.quit:
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

// nextHeightExpiry returns a channel that will immediately be read from if
// the top item on our queue has expired.
func (ew *InvoiceExpiryWatcher) nextHeightExpiry() <-chan uint32 {
	if ew.blockExpiryQueue.Empty() {
		return nil
	}

	top := ew.blockExpiryQueue.Top().(*invoiceExpiryHeight)
	if !top.expired(ew.currentHeight, ew.blockExpiryDelta) {
		return nil
	}

	blockChan := make(chan uint32, 1)
	blockChan <- top.expiryHeight
	return blockChan
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

// cancelNextHeightExpiredInvoice looks at our height based queue and expires
// the next invoice if we have reached its expiry block.
func (ew *InvoiceExpiryWatcher) cancelNextHeightExpiredInvoice() {
	if ew.blockExpiryQueue.Empty() {
		return
	}

	top := ew.blockExpiryQueue.Top().(*invoiceExpiryHeight)
	if !top.expired(ew.currentHeight, ew.blockExpiryDelta) {
		return
	}

	// We always force-cancel block-based expiry so that we can
	// cancel invoices that have been accepted but not yet resolved.
	// This helps us avoid force closes.
	ew.expireInvoice(top.paymentHash, true)
	ew.blockExpiryQueue.Pop()
}

// expireInvoice attempts to expire an invoice and logs an error if we get an
// unexpected error.
func (ew *InvoiceExpiryWatcher) expireInvoice(hash lntypes.Hash, force bool) {
	err := ew.cancelInvoice(hash, force)
	switch {
	case err == nil:

	case errors.Is(err, ErrInvoiceAlreadyCanceled):

	case errors.Is(err, ErrInvoiceAlreadySettled):

	case errors.Is(err, ErrInvoiceNotFound):
		// It's possible that the user has manually canceled the invoice
		// which will then be deleted by the garbage collector resulting
		// in an ErrInvoiceNotFound error.

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

		case *invoiceExpiryHeight:
			if expiry != nil {
				ew.blockExpiryQueue.Push(expiry)
			}

		default:
			log.Errorf("unexpected queue item: %T", inv)
		}
	}
}

// mainLoop is a goroutine that receives new invoices and handles cancellation
// of expired invoices.
func (ew *InvoiceExpiryWatcher) mainLoop(blockNtfns *chainntnfs.BlockEpochEvent) {
	defer func() {
		blockNtfns.Cancel()
		ew.wg.Done()
	}()

	// We have two different queues, so we use a different cancel method
	// depending on which expiry condition we have hit. Starting with time
	// based expiry is an arbitrary choice to start off.
	cancelNext := ew.cancelNextExpiredInvoice

	for {
		// Cancel any invoices that may have expired.
		cancelNext()

		select {
		case newInvoices := <-ew.newInvoices:
			// Take newly forwarded invoices with higher priority
			// in order to not block the newInvoices channel.
			ew.pushInvoices(newInvoices)
			continue

		default:
			select {
			// Wait until the next invoice expires.
			case <-ew.nextTimestampExpiry():
				cancelNext = ew.cancelNextExpiredInvoice
				continue

			case <-ew.nextHeightExpiry():
				cancelNext = ew.cancelNextHeightExpiredInvoice
				continue

			case newInvoices := <-ew.newInvoices:
				ew.pushInvoices(newInvoices)

			// Consume new blocks.
			case block, ok := <-blockNtfns.Epochs:
				if !ok {
					log.Debugf("block notifications " +
						"canceled")
					return
				}

				ew.currentHeight = uint32(block.Height)
				ew.currentHash = block.Hash

			case <-ew.quit:
				return
			}
		}
	}
}
