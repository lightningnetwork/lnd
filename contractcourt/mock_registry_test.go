package contractcourt

import (
	"context"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

type notifyExitHopData struct {
	payHash       lntypes.Hash
	paidAmount    lnwire.MilliSatoshi
	hodlChan      chan<- interface{}
	expiry        uint32
	currentHeight int32
}

type mockRegistry struct {
	notifyChan       chan notifyExitHopData
	notifyErr        error
	notifyResolution invoices.HtlcResolution
	notifyCalls      atomic.Int32

	// invoices holds the invoices returned by LookupInvoice. Hashes that
	// are not present report ErrInvoiceNotFound.
	invoices map[lntypes.Hash]invoices.Invoice

	// ampPreimages holds the settled AMP child preimages per set ID.
	ampPreimages map[[32]byte][]lntypes.Preimage
	ampLookupErr error
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash,
	paidAmount lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	wireCustomRecords lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	r.notifyCalls.Add(1)

	// Exit early if the notification channel is nil.
	if hodlChan == nil {
		return r.notifyResolution, r.notifyErr
	}

	r.notifyChan <- notifyExitHopData{
		hodlChan:      hodlChan,
		payHash:       payHash,
		paidAmount:    paidAmount,
		expiry:        expiry,
		currentHeight: currentHeight,
	}

	return r.notifyResolution, r.notifyErr
}

func (r *mockRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {}

func (r *mockRegistry) LookupInvoice(_ context.Context,
	payHash lntypes.Hash) (invoices.Invoice, error) {

	invoice, ok := r.invoices[payHash]
	if !ok {
		return invoices.Invoice{}, invoices.ErrInvoiceNotFound
	}

	return invoice, nil
}

func (r *mockRegistry) LookupAMPHtlcPreimage(_ context.Context,
	setID [32]byte, hash lntypes.Hash) (lntypes.Preimage, bool, error) {

	for _, preimage := range r.ampPreimages[setID] {
		if preimage.Matches(hash) {
			return preimage, true, nil
		}
	}

	return lntypes.Preimage{}, false, r.ampLookupErr
}
