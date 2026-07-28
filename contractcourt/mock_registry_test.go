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
	immediateResult  invoices.HtlcResolution
	notifyCalls      atomic.Int32
	immediateNotify  []notifyExitHopData
	notifyHook       func()
	lookupErr        error
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash,
	paidAmount lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	wireCustomRecords lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	r.notifyCalls.Add(1)
	notifyHook := r.notifyHook

	// Exit early if the notification channel is nil.
	if hodlChan == nil {
		r.immediateNotify = append(r.immediateNotify, notifyExitHopData{
			payHash:       payHash,
			paidAmount:    paidAmount,
			expiry:        expiry,
			currentHeight: currentHeight,
		})
		if notifyHook != nil {
			notifyHook()
		}
		if r.immediateResult != nil {
			return r.immediateResult, r.notifyErr
		}

		return r.notifyResolution, r.notifyErr
	}

	r.notifyChan <- notifyExitHopData{
		hodlChan:      hodlChan,
		payHash:       payHash,
		paidAmount:    paidAmount,
		expiry:        expiry,
		currentHeight: currentHeight,
	}
	if notifyHook != nil {
		notifyHook()
	}

	return r.notifyResolution, r.notifyErr
}

func (r *mockRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {}

func (r *mockRegistry) LookupInvoice(context.Context, lntypes.Hash) (
	invoices.Invoice, error) {

	if r.lookupErr != nil {
		return invoices.Invoice{}, r.lookupErr
	}

	return invoices.Invoice{}, invoices.ErrInvoiceNotFound
}
