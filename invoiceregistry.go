package main

import (
	"bytes"
	"sync"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// invoice represents a payment invoice which will be dispatched via the
// Lightning Network.
type invoice struct {
	value btcutil.Amount

	paymentHash     wire.ShaHash
	paymentPreimage wire.ShaHash

	// TODO(roasbeef): other contract stuff
}

// invoiceRegistry is a central registry of all the outstanding invoices
// created by the daemon. The registry is a thin wrapper around a map in order
// to ensure that all updates/reads are thread safe.
type invoiceRegistry struct {
	sync.RWMutex
	invoiceIndex map[wire.ShaHash]*invoice
}

// newInvoiceRegistry creates a new invoice registry.
func newInvoiceRegistry() *invoiceRegistry {
	return &invoiceRegistry{
		invoiceIndex: make(map[wire.ShaHash]*invoice),
	}
}

// addInvoice adds an invoice for the specified amount, identified by the
// passed preimage. Once this invoice is added, sub-systems within the daemon
// add/forward HTLC's are able to obtain the proper preimage required for
// redemption in the case that we're the final destination.
func (i *invoiceRegistry) addInvoice(amt btcutil.Amount, preimage wire.ShaHash) {
	paymentHash := wire.ShaHash(fastsha256.Sum256(preimage[:]))

	i.Lock()
	i.invoiceIndex[paymentHash] = &invoice{
		value:           amt,
		paymentHash:     paymentHash,
		paymentPreimage: preimage,
	}
	i.Unlock()
}

// lookupInvoice looks up an invoice by it's payment hash (R-Hash), if found
// then we're able to pull the funds pending within an HTLC.
func (i *invoiceRegistry) lookupInvoice(hash wire.ShaHash) (*invoice, bool) {
	i.RLock()
	inv, ok := i.invoiceIndex[hash]
	i.RUnlock()

	return inv, ok
}

var (
	debugPre, _ = wire.NewShaHash(bytes.Repeat([]byte{1}, 32))
	debugHash   = wire.ShaHash(fastsha256.Sum256(debugPre[:]))
)

// debugInvoice is a fake invoice created for debugging purposes within the
// daemon.
func (i *invoiceRegistry) debugInvoice() *invoice {
	return &invoice{
		value:           btcutil.Amount(100000 * 1e8),
		paymentPreimage: *debugPre,
		paymentHash:     debugHash,
	}
}
