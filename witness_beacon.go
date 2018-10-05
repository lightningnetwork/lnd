package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
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

// LookupPreImage attempts to lookup a preimage given its hash and associated
// ShortChannelID. True is returned for the second argument if the preimage is found.
func (p *preimageBeacon) LookupPreimage(payHash []byte, chanID lnwire.ShortChannelID) ([]byte, bool) {
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
		channeldb.Sha256HashWitness, payHash, chanID,
	)
	if err != nil {
		ltndLog.Errorf("unable to lookup witness: %v", err)
		return nil, false
	}

	return preimage, true
}

// AddPreImage adds a newly discovered preimage to the global cache with its
// associated ShortChannelID, and also signals any subscribers of the newly
// discovered witness.
func (p *preimageBeacon) AddPreimage(pre []byte, chanID lnwire.ShortChannelID) error {
	p.Lock()
	defer p.Unlock()

	srvrLog.Infof("Adding preimage=%x to witness cache", pre[:])

	// First, we'll add the witness and height to the decaying witness cache.
	err := p.wCache.AddWitness(channeldb.Sha256HashWitness, pre, chanID)
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
	epochClient, err := p.notifier.RegisterBlockEpochNtfn(nil)
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
// TODO - Change the garbage collection heuristic? This totally doesn't handle
// block reorgs. Or is this more of a contractcourt problem?
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

			height := uint32(epoch.Height)

			// We received a new block, so we scrub the cache if we
			// can.
			numStale, err := p.gcExpiredPreimages()

			// We don't log ErrNoWitnesses in case the WitnessBucket
			// hasn't been created yet. Otherwise, this would cause
			// unnecessary spam.
			if err != nil && err != channeldb.ErrNoWitnesses {
				srvrLog.Errorf("unable to expire preimages at "+
					"height=%d, error: %v", height, err)
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
// Currently, preimages are deleted in bulk for a specific ShortChannelID when
// the associated channel is in the FINALIZED state and 6 blocks have passed.
func (p *preimageBeacon) gcExpiredPreimages() (uint32, error) {
	p.Lock()
	defer p.Unlock()
	var numExpiredPreimages uint32

	// Call FetchAllChannelStates so we can filter out non-FINALIZED channels.
	cids, states, counts, err := p.wCache.FetchAllChannelStates(channeldb.Sha256HashWitness)
	if err != nil {
		return 0, err
	}

	for i := 0; i < len(cids); i++ {
		if states[i] == channeldb.FINALIZED {
			// If the block counter is >= 6, we delete the channel's
			// preimages and the channel from the witness cache.
			// Otherwise, we just increment the block counter.
			if counts[i] >= 6 {
				p.wCache.DeleteChannel(channeldb.Sha256HashWitness, cids[i])
				numExpiredPreimages++
			} else {
				p.wCache.UpdateChannelCounter(
					channeldb.Sha256HashWitness, cids[i], counts[i],
				)
			}
		}
	}

	return numExpiredPreimages, nil
}

var _ contractcourt.WitnessBeacon = (*preimageBeacon)(nil)
var _ lnwallet.PreimageCache = (*preimageBeacon)(nil)
