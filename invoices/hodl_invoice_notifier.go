package invoices

import (
	"errors"
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrShuttingDownNotifier is returned when an operation failed because
	// the notifier is shutting down.
	ErrShuttingDownNotifier = errors.New("hodl HTLC notifier shutting down")
)

// HodlHTLCSub implements the fn.EventReceiver interface. The unique difference
// in this new EventReceiver is that it creates the unique ID based on the
// ShortChannelID data type. This
type HodlHTLCSub[T any] struct {
	// id is the internal process-unique ID of the subscription.
	id uint64

	// NewItemList is sent to when a new item was created successfully.
	NewItemList *fn.ConcurrentQueue[T]

	// ItemRemoved is sent to when an existing item was removed.
	ItemRemovedList *fn.ConcurrentQueue[T]
}

// ID returns the internal process-unique ID of the subscription.
func (e *HodlHTLCSub[T]) ID() uint64 {
	return e.id
}

// Stop stops the receiver from processing events.
func (e *HodlHTLCSub[T]) Stop() {
	e.NewItemList.Stop()
	e.ItemRemovedList.Stop()
}

// NewItemCreated returns the channel for new items created.
func (e *HodlHTLCSub[T]) NewItemCreated() *fn.ConcurrentQueue[T] {
	return e.NewItemList
}

// ItemRemoved returns the channel for items removed.
func (e *HodlHTLCSub[T]) ItemRemoved() *fn.ConcurrentQueue[T] {
	return e.ItemRemovedList
}

// A compile-time assertion to make sure EventReceiverImpl satisfies the
// EventReceiver interface.
var _ fn.EventReceiver[any] = (*HodlHTLCSub[any])(nil)

// NewHodlHTLCSub creates a new subscriber with an ID based on the short channel
// id. This makes it easy to identify the right
func NewHodlHTLCSub(id lnwire.ShortChannelID,
	queueSize int) *HodlHTLCSub[HtlcResolution] {

	created := fn.NewConcurrentQueue[HtlcResolution](queueSize)
	created.Start()
	removed := fn.NewConcurrentQueue[HtlcResolution](queueSize)
	removed.Start()

	return &HodlHTLCSub[HtlcResolution]{
		id:              id.ToUint64(),
		NewItemList:     created,
		ItemRemovedList: removed,
	}
}

type hodlHTLCNotifier interface {
	fn.EventPublisher[HtlcResolution, bool]

	publishSubscriberEvent(res HtlcResolution) error

	recordHTLC(htlc models.CircuitKey) error

	Start() error

	Stop() error
}

// hodlHTLCDisp is the event distributer which makes sure hodl htlcs are
// correctly signaled to the interested subscribers as soon as they are resolved
// e.g. failed or settled.
type hodlHTLCDisp struct {
	// subscribers is the map of all registered subscribers to the
	// hodl htlc notifier.
	subscribers map[uint64]fn.EventReceiver[HtlcResolution]

	// hodlMap keeps track of all current hodl htlcs which are still NOT
	// resolved and are acutal hodl htlcs. This makes sure we only signal
	// to the subscribers when hodl htlcs are resolved.
	hodlMap fn.Set[models.CircuitKey]

	// event is the channel which guides the event loop of the hodl HTLC
	// notifier. Every interaction with the the notifier should be done
	// through this channel.
	event chan eventMsg

	wg   sync.WaitGroup
	quit chan struct{}
}

// RegisterSubscriber adds a new subscriber to the set of subscribers that will
// be notified of any new events targeted for this receiver.
func (h *hodlHTLCDisp) RegisterSubscriber(
	receiver fn.EventReceiver[HtlcResolution], _, _ bool) error {

	eventMsg := eventMsg{
		action:     register,
		receiver:   receiver,
		responseCh: make(chan error),
	}

	select {
	case h.event <- eventMsg:

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	select {
	case err := <-eventMsg.responseCh:
		if err != nil {
			return err
		}

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	log.Debugf("Registered hodl subscriber for channelid: %v",
		receiver.ID())

	return nil
}

// RemoveSubscriber removes a subscriber from the set of subscribers that will
// be notified of any new events targeted for this subscriber.
func (h *hodlHTLCDisp) RemoveSubscriber(
	subscriber fn.EventReceiver[HtlcResolution]) error {

	eventMsg := eventMsg{
		action:     remove,
		receiver:   subscriber,
		responseCh: make(chan error),
	}

	select {
	case h.event <- eventMsg:

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	select {
	case err := <-eventMsg.responseCh:
		if err != nil {
			return err
		}

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	log.Debugf("Removed hodl subscriber for channelid: %v",
		subscriber.ID())

	return nil
}

// A compile-time assertion to make sure hodlHTLCDisp satisfies the
// fn.EventPublisher interface.
var _ fn.EventPublisher[HtlcResolution, bool] = (*hodlHTLCDisp)(nil)

// publishSubscriberEvent publishes an HTLC resolution to the intended
// subsriber.
func (h *hodlHTLCDisp) publishSubscriberEvent(res HtlcResolution) error {

	eventMsg := eventMsg{
		action:     notify,
		resolution: res,
		responseCh: make(chan error),
	}

	select {
	case h.event <- eventMsg:

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	select {
	case err := <-eventMsg.responseCh:
		if err != nil {
			return err
		}

	case <-h.quit:
		return ErrShuttingDownNotifier
	}

	log.Debugf("Published hodl resolution(%v) successfully to subscriber",
		res)

	return nil
}

// recordHTLC records an incoming hodl htlc with the notifier.
func (h *hodlHTLCDisp) recordHTLC(htlc models.CircuitKey) error {
	eventMsg := eventMsg{
		action:     recordHTLC,
		htlc:       htlc,
		responseCh: make(chan error),
	}

	select {
	case h.event <- eventMsg:

	case <-h.quit:
		return fmt.Errorf("shutting down")
	}

	select {
	case err := <-eventMsg.responseCh:
		if err != nil {
			return err
		}

	case <-h.quit:
		return fmt.Errorf("shutting down")
	}

	log.Debugf("Recorded new hodl htlc with circuitKey: %v", htlc)

	return nil
}

// newEventDistributor creates a new event distributor of the declared type.
func newHodlHTLCResDistributor() *hodlHTLCDisp {
	return &hodlHTLCDisp{
		subscribers: make(map[uint64]fn.EventReceiver[HtlcResolution]),
		hodlMap:     fn.NewSet[models.CircuitKey](),
		event:       make(chan eventMsg),
		quit:        make(chan struct{}),
	}
}

// eventType defines which event is registered with the event loop so that a
// particular action can be executed.
type eventType int

const (
	// unknown is the default when not initialized.
	unknown eventType = iota

	// register registers a new subscriber.
	register

	// remove removes a given subscriber from the internal mapping.
	remove

	// notify notifies the relevant subscribers for a given hodl HTLC.
	notify

	// recordHTLC records a new hodl HLTC with the notifier.
	recordHTLC
)

// eventMsg is the msg which is used to communicate with the central event loop
// of the htlc notifier.
type eventMsg struct {
	action     eventType
	receiver   fn.EventReceiver[HtlcResolution]
	htlc       models.CircuitKey
	resolution HtlcResolution
	responseCh chan error
}

// Start is part of the  hodlHTLCNotifier interface and starts the htlc
// notifier.
func (h *hodlHTLCDisp) Start() error {
	h.wg.Add(1)
	go h.start()

	return nil
}

// start starts the internal event loop of the hodl HTLC notifier.
func (h *hodlHTLCDisp) start() {
	defer h.wg.Done()

	for {
		select {
		case msg := <-h.event:
			var err error
			switch msg.action {
			case register:
				receiver := msg.receiver

				_, ok := h.subscribers[receiver.ID()]
				if ok {
					err = fmt.Errorf("subscriber with ID "+
						"%d already registered",
						receiver.ID())

					break
				}
				h.subscribers[receiver.ID()] = receiver

			case remove:
				receiver := msg.receiver
				_, ok := h.subscribers[receiver.ID()]
				if !ok {
					err = fmt.Errorf("subscriber with ID"+
						"%d not found", receiver.ID())

					break
				}

				receiver.Stop()
				delete(h.subscribers, receiver.ID())

				// delete all the htlc in the hold map related
				// to this subscriber
				htlcs := h.hodlMap.ToSlice()
				removeHTLCs := fn.Filter(
					func(htlc models.CircuitKey) bool {
						id := htlc.ChanID.ToUint64()
						return id == receiver.ID()
					}, htlcs)

				for _, htlc := range removeHTLCs {
					h.hodlMap.Remove(htlc)
				}

			case notify:
				circuitKey := msg.resolution.CircuitKey()
				subID := circuitKey.ChanID.ToUint64()
				subscriber, ok := h.subscribers[subID]
				if !ok {
					err = fmt.Errorf("subscriber with ID "+
						"%d not found", subID)

					break
				}

				// Check if htlc is part of the hodlmap
				if !h.hodlMap.Contains(circuitKey) {
					log.Trace("htlc not in hodlmap")

					break
				}

				// Only notify the relevant subscriber.
				res := msg.resolution
				subscriber.NewItemCreated().ChanIn() <- res

				// delete the corresponding key from the map
				// as soon as the subscriber was notified.
				h.hodlMap.Remove(circuitKey)

			case recordHTLC:
				circuitKey := msg.htlc
				subID := circuitKey.ChanID.ToUint64()

				// Make sure we do have a subscriber listening
				// for this channel.
				if _, ok := h.subscribers[subID]; !ok {
					log.Debugf("no subscriber with "+
						"ChanID(%v) listening", subID)

					break
				}

				if h.hodlMap.Contains(circuitKey) {
					err = fmt.Errorf("htlc with "+
						"circuitkey(%v) already in map",
						circuitKey)

					break
				}

				h.hodlMap.Add(circuitKey)

			case unknown:
				err = fmt.Errorf("unkown action type")
			}

			msg.responseCh <- err

		case <-h.quit:
			return
		}
	}
}

// Stop stops the hodl HTLC notifier.
func (h *hodlHTLCDisp) Stop() error {
	log.Info("hodlHTLCDisp shutting down...")

	close(h.quit)
	h.wg.Wait()

	log.Debug("hodlHTLCDisp shutdown complete")

	return nil
}
