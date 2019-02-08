package htlcswitch

import (
	"sync"
	"sync/atomic"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lntypes"
)

// HodlManager manages hold invoices.
type HodlManager struct {
	// The following fields are only meant to be used *atomically*
	started  int32
	shutdown int32

	// Registry is a sub-system which responsible for managing the invoices
	// in thread-safe manner.
	registry InvoiceDatabase

	// PreimageCache is a global witness beacon that houses any new
	// preimages discovered by other links. We'll use this to add new
	// witnesses that we discover which will notify any sub-systems
	// subscribed to new events.
	preimageCache contractcourt.WitnessBeacon

	preimageSubscription *contractcourt.WitnessSubscription

	wg   sync.WaitGroup
	quit chan struct{}

	// subscriptions is a map from a payment hash to a list of subscribers.
	// It is used for efficient notification of links.
	subscriptions map[lntypes.Hash]map[uint64]*hodlEventSubscription

	// reverseSubscriptions tracks hashes subscribed to per subscriber. This
	// is used to unsubscribe from all hashes efficiently.
	reverseSubscriptions map[uint64]map[lntypes.Hash]struct{}

	lock             sync.Mutex
	lastSubscriberID uint64
}

type hodlEventSubscription struct {
	manager    *HodlManager
	id         uint64
	settleChan chan lntypes.Preimage

	// TODO: add cancelChan
}

func (s *hodlEventSubscription) subscribe(hash lntypes.Hash) {
	s.manager.subscribeHash(s, hash)
}

func (s *hodlEventSubscription) cancel() {
	s.manager.unsubscribe(s.id)
}

// NewHodlManager returns a new instance.
func NewHodlManager(registry InvoiceDatabase,
	preimageCache contractcourt.WitnessBeacon) *HodlManager {

	return &HodlManager{
		registry:             registry,
		preimageCache:        preimageCache,
		subscriptions:        make(map[lntypes.Hash]map[uint64]*hodlEventSubscription),
		reverseSubscriptions: make(map[uint64]map[lntypes.Hash]struct{}),
	}
}

// Start starts hodl manager.
func (h *HodlManager) Start() error {
	if !atomic.CompareAndSwapInt32(&h.started, 0, 1) {
		err := errors.New("hodl manager already started")
		log.Warn(err)
		return err
	}

	log.Infof("HodlManager is starting")

	h.preimageSubscription = h.preimageCache.SubscribeUpdates()

	h.wg.Add(1)
	go h.eventLoop()

	return nil
}

func (h *HodlManager) eventLoop() {
	defer h.wg.Done()
	for {
		select {

		case pre := <-h.preimageSubscription.WitnessUpdates:
			preimage, _ := lntypes.NewPreimage(pre)
			hash := preimage.Hash()

			log.Debugf("HodlManager received preimage %v", preimage)

			// TODO: Improve concurrency model / locking by handling
			// (un)subscribe in this event loop.

			h.lock.Lock()
			subscribers, ok := h.subscriptions[hash]
			if !ok {
				h.lock.Unlock()
				continue
			}

			// Notify all interested subscribers and remove
			// subscription from both maps.
			for _, subscriber := range subscribers {
				log.Debugf("HodlManager notify subscriber %v",
					subscriber.id)

				subscriber.settleChan <- preimage

				delete(h.reverseSubscriptions[subscriber.id], hash)
			}

			delete(h.subscriptions, hash)

			h.lock.Unlock()

		// TODO: Subscribe to invoiceregistry cancel notifications

		case <-h.quit:
			return
		}
	}

}

func (h *HodlManager) subscribeHash(s *hodlEventSubscription, hash lntypes.Hash) {
	h.lock.Lock()
	defer h.lock.Unlock()

	subscriptions, ok := h.subscriptions[hash]
	if !ok {
		subscriptions = make(map[uint64]*hodlEventSubscription)
		h.subscriptions[hash] = subscriptions
	}
	subscriptions[s.id] = s

	reverseSubscriptions, ok := h.reverseSubscriptions[s.id]
	if !ok {
		reverseSubscriptions = make(map[lntypes.Hash]struct{})
		h.reverseSubscriptions[s.id] = reverseSubscriptions
	}
	reverseSubscriptions[hash] = struct{}{}
}

func (h *HodlManager) unsubscribe(id uint64) {
	h.lock.Lock()
	defer h.lock.Unlock()

	hashes := h.reverseSubscriptions[id]
	for hash := range hashes {
		delete(h.subscriptions[hash], id)
	}

	delete(h.reverseSubscriptions, id)
}

func (h *HodlManager) subscribe() *hodlEventSubscription {
	h.lock.Lock()
	defer h.lock.Unlock()

	id := h.lastSubscriberID
	h.lastSubscriberID++

	subscription := &hodlEventSubscription{
		id:         id,
		manager:    h,
		settleChan: make(chan lntypes.Preimage),
	}

	return subscription
}

// Stop stops hodl manager.
func (h *HodlManager) Stop() {
	if !atomic.CompareAndSwapInt32(&h.shutdown, 0, 1) {
		log.Warnf("hodl manager already stopped")
		return
	}

	close(h.quit)
	h.wg.Wait()

	h.preimageSubscription.CancelSubscription()
}
