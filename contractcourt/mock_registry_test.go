package contractcourt

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

type notifyExitHopData struct {
	payHash    lntypes.Hash
	paidAmount lnwire.MilliSatoshi
	hodlChan   chan<- interface{}
}

type mockRegistry struct {
	notifyChan  chan notifyExitHopData
	notifyErr   error
	notifyEvent *invoices.HodlEvent
}

func (r *mockRegistry) NotifyExitHopHtlc(payHash lntypes.Hash, paidAmount lnwire.MilliSatoshi,
	hodlChan chan<- interface{}) (*invoices.HodlEvent, error) {

	r.notifyChan <- notifyExitHopData{
		hodlChan:   hodlChan,
		payHash:    payHash,
		paidAmount: paidAmount,
	}

	return r.notifyEvent, r.notifyErr
}

func (r *mockRegistry) HodlUnsubscribeAll(subscriber chan<- interface{}) {}

func (r *mockRegistry) LookupInvoice(lntypes.Hash) (channeldb.Invoice, uint32,
	error) {

	return channeldb.Invoice{}, 0, channeldb.ErrInvoiceNotFound
}
