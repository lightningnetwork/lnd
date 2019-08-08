package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Registry is an interface which represents the invoice registry.
type Registry interface {
	// LookupInvoice attempts to look up an invoice according to its 32
	// byte payment hash. This method should also reutrn the min final CLTV
	// delta for this invoice. We'll use this to ensure that the HTLC
	// extended to us gives us enough time to settle as we prescribe.
	LookupInvoice(lntypes.Hash) (channeldb.Invoice, uint32, error)

	// NotifyExitHopHtlc attempts to mark an invoice as settled. If the
	// invoice is a debug invoice, then this method is a noop as debug
	// invoices are never fully settled. The return value describes how the
	// htlc should be resolved. If the htlc cannot be resolved immediately,
	// the resolution is sent on the passed in hodlChan later.
	NotifyExitHopHtlc(payHash lntypes.Hash, paidAmount lnwire.MilliSatoshi,
		expiry uint32, currentHeight int32,
		circuitKey channeldb.CircuitKey, hodlChan chan<- interface{},
		eob []byte) (*invoices.HodlEvent, error)

	// HodlUnsubscribeAll unsubscribes from all hodl events.
	HodlUnsubscribeAll(subscriber chan<- interface{})
}
