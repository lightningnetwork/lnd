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
	immediateNotify  []notifyExitHopData
	notifyHook       func()
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash,
	paidAmount lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	wireCustomRecords lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	r.notifyCalls.Add(1)

	// Exit early if the notification channel is nil.
	if hodlChan == nil {
		r.immediateNotify = append(r.immediateNotify, notifyExitHopData{
			payHash:       payHash,
			paidAmount:    paidAmount,
			expiry:        expiry,
			currentHeight: currentHeight,
		})
		if r.notifyHook != nil {
			r.notifyHook()
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
	if r.notifyHook != nil {
		r.notifyHook()
	}

	return r.notifyResolution, r.notifyErr
}

func (r *mockRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {}

func (r *mockRegistry) LookupInvoice(context.Context, lntypes.Hash) (
	invoices.Invoice, error) {

	return invoices.Invoice{}, invoices.ErrInvoiceNotFound
}
