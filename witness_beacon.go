package lnd

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// preimageSubscriber reprints an active subscription to be notified once the
// daemon discovers new preimages, either on chain or off-chain.
type preimageSubscriber struct {
	updateChan chan lntypes.Preimage

	quit chan struct{}
}

type witnessCache interface {
	// LookupSha256Witness attempts to lookup the preimage for a sha256
	// hash. If the witness isn't found, ErrNoWitnesses will be returned.
	LookupSha256Witness(hash lntypes.Hash) (lntypes.Preimage, error)

	// AddSha256Witnesses adds a batch of new sha256 preimages into the
	// witness cache. This is an alias for AddWitnesses that uses
	// Sha256HashWitness as the preimages' witness type.
	AddSha256Witnesses(preimages ...lntypes.Preimage) error
}

// preimageBeacon is an implementation of the contractcourt.WitnessBeacon
// interface, and the lnwallet.PreimageCache interface. This implementation is
// concerned with a single witness type: sha256 hahsh preimages.
type preimageBeacon struct {
	sync.RWMutex

	wCache witnessCache

	clientCounter uint64
	subscribers   map[uint64]*preimageSubscriber

	interceptor func(htlcswitch.InterceptedForward) error
}

func newPreimageBeacon(wCache witnessCache,
	interceptor func(htlcswitch.InterceptedForward) error) *preimageBeacon {

	return &preimageBeacon{
		wCache:      wCache,
		interceptor: interceptor,
		subscribers: make(map[uint64]*preimageSubscriber),
	}
}

// SubscribeUpdates returns a channel that will be sent upon *each* time a new
// preimage is discovered.
func (p *preimageBeacon) SubscribeUpdates(
	chanID lnwire.ShortChannelID, htlc *channeldb.HTLC,
	payload *hop.Payload,
	nextHopOnionBlob []byte) (*contractcourt.WitnessSubscription, error) {

	p.Lock()
	defer p.Unlock()

	clientID := p.clientCounter
	client := &preimageSubscriber{
		updateChan: make(chan lntypes.Preimage, 10),
		quit:       make(chan struct{}),
	}

	p.subscribers[p.clientCounter] = client

	p.clientCounter++

	srvrLog.Debugf("Creating new witness beacon subscriber, id=%v",
		p.clientCounter)

	sub := &contractcourt.WitnessSubscription{
		WitnessUpdates: client.updateChan,
		CancelSubscription: func() {
			p.Lock()
			defer p.Unlock()

			delete(p.subscribers, clientID)

			close(client.quit)
		},
	}

	// Notify the htlc interceptor. There may be a client connected
	// and willing to supply a preimage.
	packet := &htlcswitch.InterceptedPacket{
		Hash:           htlc.RHash,
		IncomingExpiry: htlc.RefundTimeout,
		IncomingAmount: htlc.Amt,
		IncomingCircuit: models.CircuitKey{
			ChanID: chanID,
			HtlcID: htlc.HtlcIndex,
		},
		OutgoingChanID:       payload.FwdInfo.NextHop,
		OutgoingExpiry:       payload.FwdInfo.OutgoingCTLV,
		OutgoingAmount:       payload.FwdInfo.AmountToForward,
		InOnionCustomRecords: payload.CustomRecords(),
		InWireCustomRecords:  htlc.CustomRecords,
	}
	copy(packet.OnionBlob[:], nextHopOnionBlob)

	fwd := newInterceptedForward(packet, p)

	err := p.interceptor(fwd)
	if err != nil {
		return nil, err
	}

	return sub, nil
}

// LookupPreimage attempts to lookup a preimage in the global cache.  True is
// returned for the second argument if the preimage is found.
func (p *preimageBeacon) LookupPreimage(
	payHash lntypes.Hash) (lntypes.Preimage, bool) {

	p.RLock()
	defer p.RUnlock()

	// Otherwise, we'll perform a final check using the witness cache.
	preimage, err := p.wCache.LookupSha256Witness(payHash)
	if errors.Is(err, channeldb.ErrNoWitnesses) {
		ltndLog.Debugf("No witness for payment %v", payHash)
		return lntypes.Preimage{}, false
	}

	if err != nil {
		ltndLog.Errorf("Unable to lookup witness: %v", err)
		return lntypes.Preimage{}, false
	}

	return preimage, true
}

// AddPreimages adds a batch of newly discovered preimages to the global cache,
// and also signals any subscribers of the newly discovered witness.
func (p *preimageBeacon) AddPreimages(preimages ...lntypes.Preimage) error {
	// Exit early if no preimages are presented.
	if len(preimages) == 0 {
		return nil
	}

	// Copy the preimages to ensure the backing area can't be modified by
	// the caller when delivering notifications.
	preimageCopies := make([]lntypes.Preimage, 0, len(preimages))
	for _, preimage := range preimages {
		srvrLog.Infof("Adding preimage=%v to witness cache for %v",
			preimage, preimage.Hash())

		preimageCopies = append(preimageCopies, preimage)
	}

	// First, we'll add the witness to the decaying witness cache.
	err := p.wCache.AddSha256Witnesses(preimages...)
	if err != nil {
		return err
	}

	p.Lock()
	defer p.Unlock()

	// With the preimage added to our state, we'll now send a new
	// notification to all subscribers.
	for _, client := range p.subscribers {
		go func(c *preimageSubscriber) {
			for _, preimage := range preimageCopies {
				select {
				case c.updateChan <- preimage:
				case <-c.quit:
					return
				}
			}
		}(client)
	}

	srvrLog.Debugf("Added %d preimage(s) to witness cache",
		len(preimageCopies))

	return nil
}

var _ contractcourt.WitnessBeacon = (*preimageBeacon)(nil)
