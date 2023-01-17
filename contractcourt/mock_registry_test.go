package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb/models"
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
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash,
	paidAmount lnwire.MilliSatoshi, expiry uint32, currentHeight int32,
	circuitKey models.CircuitKey, hodlChan chan<- interface{},
	payload invoices.Payload) (invoices.HtlcResolution, error) {

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

func (r *mockRegistry) LookupInvoice(lntypes.Hash) (invoices.Invoice,
	error) {

	return invoices.Invoice{}, invoices.ErrInvoiceNotFound
}
