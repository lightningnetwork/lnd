package main

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
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
	started int32
	stopped int32

	sync.RWMutex

	invoices *invoiceRegistry

	wCache *channeldb.WitnessCache

	clientCounter uint64
	subscribers   map[uint64]*preimageSubscriber

	notifier chainntnfs.ChainNotifier

	wg   sync.WaitGroup
	quit chan struct{}
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

// LookupPreImage attempts to lookup a preimage. True is returned for the
// second argument if the preimage is found. Returning the expiry height
// is unnecessary.
func (p *preimageBeacon) LookupPreimage(payHash []byte) ([]byte, bool) {
	p.RLock()
	defer p.RUnlock()

	// First, we'll check the invoice registry to see if we already know of
	// the preimage as it's on that we created ourselves.
	var invoiceKey chainhash.Hash
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
	if err != channeldb.ErrInvoiceNotFound {
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
// signals any subscribers of the newly discovered witness. After the preimage is
// first discovered, it will have an expiry height of Math.MaxUint32.  When the
// appropriate subsystems query for this preimage and construct an UpdateFulfillHTLC
// message to send to the downstream peer, the correct expiry height will be added
// to the global cache.
func (p *preimageBeacon) AddPreimage(pre []byte, expiryHeight uint32) error {
	p.Lock()
	defer p.Unlock()

	srvrLog.Infof("Adding preimage=%x to witness cache", pre[:])

	// First, we'll add the witness and height to the decaying witness cache.
	err := p.wCache.AddWitness(channeldb.Sha256HashWitness, pre, expiryHeight)
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

// Start starts the R-value preimage garbage collector.
func (p *preimageBeacon) Start() error {
	if !atomic.CompareAndSwapInt32(&p.started, 0, 1) {
		return nil
	}

	// Start garbage collector
	epochClient, err := p.notifier.RegisterBlockEpochNtfn()
	if err != nil {
		return fmt.Errorf("Unable to register for epoch "+
			"notifications: %v", err)
	}

	p.wg.Add(1)
	go p.garbageCollector(epochClient)

	return nil
}

// Stop stops the garbage collector.
func (p *preimageBeacon) Stop() error {
	if !atomic.CompareAndSwapInt32(&p.stopped, 0, 1) {
		return nil
	}

	// Stop garbage collector
	close(p.quit)

	p.wg.Wait()

	return nil
}

// garbageCollector calls gcExpiredPreimages, which actually does the garbage
// collecting, upon receiving new block notifications from the epochClient.
func (p *preimageBeacon) garbageCollector(epochClient *chainntnfs.BlockEpochEvent) {
	defer p.wg.Done()
	defer epochClient.Cancel()

	for {
		select {
		case epoch, ok := <-epochClient.Epochs:
			if !ok {
				// Block epoch was canceled, shutting down.
				srvrLog.Infof("Block epoch canceled, " +
					"garbage collector shutting down")
				return
			}

			// Using the current block height, scrub the cache
			// of expired preimages.
			height := uint32(epoch.Height)
			numStale, err := p.gcExpiredPreimages(height)
			if err != nil {
				srvrLog.Errorf("unable to expire preimages at "+
					"height=%d", height)
			}

			if numStale > 0 {
				srvrLog.Infof("Garbage collected %v preimages "+
					"at height=%d", numStale, height)
			}
		case <-p.quit:
			// Received shutdown request.
			srvrLog.Infof("Garbage collector received shutdown request")
			return
		}
	}
}

// gcExpiredPreimages is the function that actually removes the preimages.
// NOTE: We could remove the preimages when their associated HTLC's has been swept
// and confirmed or when we receive a revocation key from the remote party, but
// as the preimages already have a set termination date, this is unnecessary.
func (p *preimageBeacon) gcExpiredPreimages(height uint32) (uint32, error) {
	p.Lock()
	defer p.Unlock()
	var numExpiredPreimages uint32

	witnesses, expiryHeights, err := p.wCache.FetchAllWitnesses(channeldb.Sha256HashWitness)
	if err != nil {
		return 0, err
	}

	for i := 0; i < len(witnesses); i++ {
		if expiryHeights[i] < height {
			key := sha256.Sum256(witnesses[i])
			witnessKey := key[:]
			p.wCache.DeleteWitness(channeldb.Sha256HashWitness, witnessKey)
			numExpiredPreimages++
		}
	}

	return numExpiredPreimages, nil
}

var _ contractcourt.WitnessBeacon = (*preimageBeacon)(nil)
var _ lnwallet.PreimageCache = (*preimageBeacon)(nil)
