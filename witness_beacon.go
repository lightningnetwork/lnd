package main

import (
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// preimageSubscriber reprints an active subscription to be notified once the
// daemon discovers new preimages, either on chain or off-chain.
type preimageSubscriber struct {
	updateChan chan []byte

	quit chan struct{}
}

// preimageBeacon is an implementation of the contractcourt.WitnessBeacon
// interface, and the lnwallet.PreimageCache interface. This implementation is
// concerned with a single witness type: sha256 hahsh preimages.
type preimageBeacon struct {
	sync.RWMutex

	invoices *invoices.InvoiceRegistry

	wCache *channeldb.WitnessCache

	clientCounter uint64
	subscribers   map[uint64]*preimageSubscriber
}

// SubscribeUpdates returns a channel that will be sent upon *each* time a new
// preimage is discovered.
func (p *preimageBeacon) SubscribeUpdates() *contractcourt.WitnessSubscription {
	p.Lock()
	defer p.Unlock()

	clientID := p.clientCounter
	client := &preimageSubscriber{
		updateChan: make(chan []byte, 10),
		quit:       make(chan struct{}),
	}

	p.subscribers[p.clientCounter] = client

	p.clientCounter++

	srvrLog.Debugf("Creating new witness beacon subscriber, id=%v",
		p.clientCounter)

	return &contractcourt.WitnessSubscription{
		WitnessUpdates: client.updateChan,
		CancelSubscription: func() {
			p.Lock()
			defer p.Unlock()

			delete(p.subscribers, clientID)

			close(client.quit)
		},
	}
}

// LookupPreImage attempts to lookup a preimage in the global cache.  True is
// returned for the second argument if the preimage is found.
func (p *preimageBeacon) LookupPreimage(payHash []byte) ([]byte, bool) {
	p.RLock()
	defer p.RUnlock()

	// First, we'll check the invoice registry to see if we already know of
	// the preimage as it's on that we created ourselves.
	var invoiceKey lntypes.Hash
	copy(invoiceKey[:], payHash)
	invoice, _, err := p.invoices.LookupInvoice(invoiceKey)
	switch {
	case err == channeldb.ErrInvoiceNotFound:
		// If we get this error, then it simply means that this invoice
		// wasn't found, so we don't treat it as a critical error.
	case err != nil:
		return nil, false
	}

	// If we've found the invoice, then we can return the preimage
	// directly.
	if err != channeldb.ErrInvoiceNotFound &&
		invoice.Terms.PaymentPreimage != channeldb.UnknownPreimage {

		return invoice.Terms.PaymentPreimage[:], true
	}

	// Otherwise, we'll perform a final check using the witness cache.
	preimage, err := p.wCache.LookupWitness(
		channeldb.Sha256HashWitness, payHash,
	)
	if err != nil {
		ltndLog.Errorf("unable to lookup witness: %v", err)
		return nil, false
	}

	return preimage, true
}

// AddPreImage adds a newly discovered preimage to the global cache, and also
// signals any subscribers of the newly discovered witness.
func (p *preimageBeacon) AddPreimage(pre []byte) error {
	p.Lock()
	defer p.Unlock()

	srvrLog.Infof("Adding preimage=%x to witness cache", pre[:])

	// First, we'll add the witness to the decaying witness cache.
	err := p.wCache.AddWitness(channeldb.Sha256HashWitness, pre)
	if err != nil {
		return err
	}

	// With the preimage added to our state, we'll now send a new
	// notification to all subscribers.
	for _, client := range p.subscribers {
		go func(c *preimageSubscriber) {
			select {
			case c.updateChan <- pre:
			case <-c.quit:
				return
			}
		}(client)
	}

	return nil
}

var _ contractcourt.WitnessBeacon = (*preimageBeacon)(nil)
var _ lnwallet.PreimageCache = (*preimageBeacon)(nil)
