package main

import (
	"bytes"
	"sync"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// debugPre is the default debug preimage which is inserted into the
	// invoice registry if the --debughtlc flag is activated on start up.
	// All nodes initialize with the flag active will immediately settle
	// any incoming HTLC whose rHash is corresponds with the debug
	// preimage.
	debugPre, _ = wire.NewShaHash(bytes.Repeat([]byte{1}, 32))

	debugHash = wire.ShaHash(fastsha256.Sum256(debugPre[:]))
)

// invoiceRegistry is a central registry of all the outstanding invoices
// created by the daemon. The registry is a thin wrapper around a map in order
// to ensure that all updates/reads are thread safe.
type invoiceRegistry struct {
	sync.RWMutex

	cdb *channeldb.DB

	// debugInvoices is a mp which stores special "debug" invoices which
	// should be only created/used when manual tests require an invoice
	// that *all* nodes are able to fully settle.
	debugInvoices map[wire.ShaHash]*channeldb.Invoice
}

// newInvoiceRegistry creates a new invoice registry. The invoice registry
// wraps the persistent on-disk invoice storage with an additional in-memory
// layer. The in-memory layer is in pace such that debug invoices can be added
// which are volatile yet available system wide within the daemon.
func newInvoiceRegistry(cdb *channeldb.DB) *invoiceRegistry {
	return &invoiceRegistry{
		cdb:           cdb,
		debugInvoices: make(map[wire.ShaHash]*channeldb.Invoice),
	}
}

// addDebugInvoice adds a debug invoice for the specified amount, identified
// by the passed preimage. Once this invoice is added, sub-systems within the
// daemon add/forward HTLC's are able to obtain the proper preimage required
// for redemption in the case that we're the final destination.
func (i *invoiceRegistry) AddDebugInvoice(amt btcutil.Amount, preimage wire.ShaHash) {
	paymentHash := wire.ShaHash(fastsha256.Sum256(preimage[:]))

	i.Lock()
	i.debugInvoices[paymentHash] = &channeldb.Invoice{
		CreationDate: time.Now(),
		Terms: channeldb.ContractTerm{
			Value:           amt,
			PaymentPreimage: preimage,
		},
	}
	i.Unlock()
}

// AddInvoice adds a regular invoice for the specified amount, identified by
// the passed preimage. Additionally, any memo or recipt data provided will
// also be stored on-disk. Once this invoice is added, sub-systems within the
// daemon add/forward HTLC's are able to obtain the proper preimage required
// for redemption in the case that we're the final destination.
func (i *invoiceRegistry) AddInvoice(invoice *channeldb.Invoice) error {
	// TODO(roasbeef): also check in memory for quick lookups/settles?
	return i.cdb.AddInvoice(invoice)
}

// lookupInvoice looks up an invoice by it's payment hash (R-Hash), if found
// then we're able to pull the funds pending within an HTLC.
// TODO(roasbeef): ignore if settled?
func (i *invoiceRegistry) LookupInvoice(rHash wire.ShaHash) (*channeldb.Invoice, error) {
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
// dbueg invoice, then this method is a nooop as debug invoices are never fully
// settled.
func (i *invoiceRegistry) SettleInvoice(rHash wire.ShaHash) error {
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
	return i.cdb.SettleInvoice(rHash)
}
