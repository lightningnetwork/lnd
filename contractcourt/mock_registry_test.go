package contractcourt

import (
	"context"

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
	immediateNotify  []notifyExitHopData
	lookupInvoice    invoices.Invoice
	lookupInvoiceSet bool
	lookupErr        error
	lookupCount      int
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash,
	paidAmount lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	wireCustomRecords lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	// Exit early if the notification channel is nil.
	if hodlChan == nil {
		r.immediateNotify = append(r.immediateNotify, notifyExitHopData{
			payHash:       payHash,
			paidAmount:    paidAmount,
			expiry:        expiry,
			currentHeight: currentHeight,
		})

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

func (r *mockRegistry) LookupInvoice(context.Context, lntypes.Hash) (
	invoices.Invoice, error) {

	return r.lookupInvoiceResult()
}

func (r *mockRegistry) LookupInvoiceByRef(_ context.Context,
	_ invoices.InvoiceRef) (invoices.Invoice, error) {

	return r.lookupInvoiceResult()
}

func (r *mockRegistry) lookupInvoiceResult() (invoices.Invoice, error) {
	r.lookupCount++
	if r.lookupErr != nil {
		return invoices.Invoice{}, r.lookupErr
	}

	if !r.lookupInvoiceSet {
		return invoices.Invoice{}, invoices.ErrInvoiceNotFound
	}

	return r.lookupInvoice, nil
}
