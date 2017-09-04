package invoiceregistry

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// InvoiceRegistry is a central registry of all the outstanding invoices
// created by the daemon. The registry is a thin wrapper around a map in order
// to ensure that all updates/reads are thread safe.
type InvoiceRegistry struct {
	sync.RWMutex

	cdb *channeldb.DB

	clientMtx           sync.Mutex
	nextClientID        uint32
	notificationClients map[uint32]*invoiceSubscription

	// debugInvoices is a map which stores special "debug" invoices which
	// should be only created/used when manual tests require an invoice
	// that *all* nodes are able to fully settle.
	debugInvoices map[chainhash.Hash]*channeldb.Invoice
}

// newInvoiceRegistry creates a new invoice registry. The invoice registry
// wraps the persistent on-disk invoice storage with an additional in-memory
// layer. The in-memory layer is in place such that debug invoices can be added
// which are volatile yet available system wide within the daemon.
func NewInvoiceRegistry(cdb *channeldb.DB) *InvoiceRegistry {
	return &InvoiceRegistry{
		cdb:                 cdb,
		debugInvoices:       make(map[chainhash.Hash]*channeldb.Invoice),
		notificationClients: make(map[uint32]*invoiceSubscription),
	}
}

// addDebugInvoice adds a debug invoice for the specified amount, identified
// by the passed preimage. Once this invoice is added, subsystems within the
// daemon add/forward HTLCs are able to obtain the proper preimage required
// for redemption in the case that we're the final destination.
func (i *InvoiceRegistry) AddDebugInvoice(amt btcutil.Amount, preimage chainhash.Hash) {
	paymentHash := chainhash.Hash(sha256.Sum256(preimage[:]))

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
// daemon add/forward HTLCs are able to obtain the proper preimage required
// for redemption in the case that we're the final destination.
func (i *InvoiceRegistry) AddInvoice(invoice *channeldb.Invoice) error {
	log.Debugf("Adding invoice %v", newLogClosure(func() string {
		return spew.Sdump(invoice)
	}))

	// TODO(roasbeef): also check in memory for quick lookups/settles?
	return i.cdb.AddInvoice(invoice)

	// TODO(roasbeef): re-enable?
	//go i.notifyClients(invoice, false)
}

// lookupInvoice looks up an invoice by its payment hash (R-Hash), if found
// then we're able to pull the funds pending within an HTLC.
// TODO(roasbeef): ignore if settled?
func (i *InvoiceRegistry) LookupInvoice(rHash chainhash.Hash) (*channeldb.Invoice, error) {
	// First check the in-memory debug invoice index to see if this is an
	// existing invoice added for debugging.
	i.RLock()
	invoice, ok := i.debugInvoices[rHash]
	i.RUnlock()

	// If found, then simply return the invoice directly.
	if ok {
		return invoice, nil
	}

	// Otherwise, we'll check the database to see if there's an existing
	// matching invoice.
	return i.cdb.LookupInvoice(rHash)
}

// SettleInvoice attempts to mark an invoice as settled. If the invoice is a
// debug invoice, then this method is a noop as debug invoices are never fully
// settled.
func (i *InvoiceRegistry) SettleInvoice(rHash chainhash.Hash) error {
	log.Debugf("Settling invoice %x", rHash[:])

	// First check the in-memory debug invoice index to see if this is an
	// existing invoice added for debugging.
	i.RLock()
	if _, ok := i.debugInvoices[rHash]; ok {
		// Debug invoices are never fully settled, so we simply return
		// immediately in this case.
		i.RUnlock()

		return nil
	}
	i.RUnlock()

	// If this isn't a debug invoice, then we'll attempt to settle an
	// invoice matching this rHash on disk (if one exists).
	if err := i.cdb.SettleInvoice(rHash); err != nil {
		return err
	}

	// Launch a new goroutine to notify any/all registered invoice
	// notification clients.
	go func() {
		invoice, err := i.cdb.LookupInvoice(rHash)
		if err != nil {
			log.Errorf("unable to find invoice: %v", err)
			return
		}

		log.Infof("Payment received: %v", spew.Sdump(invoice))

		i.notifyClients(invoice, true)
	}()

	return nil
}

// notifyClients notifies all currently registered invoice notification clients
// of a newly added/settled invoice.
func (i *InvoiceRegistry) notifyClients(invoice *channeldb.Invoice, settle bool) {
	i.clientMtx.Lock()
	defer i.clientMtx.Unlock()

	for _, client := range i.notificationClients {
		var eventChan chan *channeldb.Invoice
		if settle {
			eventChan = client.SettledInvoices
		} else {
			eventChan = client.NewInvoices
		}

		go func() {
			eventChan <- invoice
		}()
	}
}

// invoiceSubscription represents an intent to receive updates for newly added
// or settled invoices. For each newly added invoice, a copy of the invoice
// will be sent over the NewInvoices channel. Similarly, for each newly settled
// invoice, a copy of the invoice will be sent over the SettledInvoices
// channel.
type invoiceSubscription struct {
	NewInvoices     chan *channeldb.Invoice
	SettledInvoices chan *channeldb.Invoice

	inv *InvoiceRegistry
	id  uint32
}

// Cancel unregisters the invoiceSubscription, freeing any previously allocated
// resources.
func (i *invoiceSubscription) Cancel() {
	i.inv.clientMtx.Lock()
	delete(i.inv.notificationClients, i.id)
	i.inv.clientMtx.Unlock()
}

// SubscribeNotifications returns an invoiceSubscription which allows the
// caller to receive async notifications when any invoices are settled or
// added.
func (i *InvoiceRegistry) SubscribeNotifications() *invoiceSubscription {
	client := &invoiceSubscription{
		NewInvoices:     make(chan *channeldb.Invoice),
		SettledInvoices: make(chan *channeldb.Invoice),
		inv:             i,
	}

	i.clientMtx.Lock()
	i.notificationClients[i.nextClientID] = client
	client.id = i.nextClientID
	i.nextClientID++
	i.clientMtx.Unlock()

	return client
}
