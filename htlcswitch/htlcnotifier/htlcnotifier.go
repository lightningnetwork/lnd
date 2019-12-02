// Package htlcnotifier notifies clients of HTLC forwards, failures and settles
// for HTLCs that our node handles. HTLCs which originated at our node can be
// identified by a zero channel ID, on the incoming channel for sends and on the
// outgoing channel for receives.
//
// It takes subscriptions for its events and notifies them when HTLC events
// occur. These are served on a best-effort basis; events are not persisted,
// delivery is not guaranteed (in the event of a crash in the switch, forward
// events may be lost) and events will be replayed upon restart. Events consumed
// from this package should be de-duplicated, and not relied upon for critical
// operations.
package htlcnotifier

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
)

// HTLCNotifier is a subsystem which all HTLC forward, failure and settle
// events pipe through. It takes subscriptions for its events, and whenever
// it receives a new event it notifies its subscribers over the proper channel.
type HTLCNotifier struct {
	started sync.Once
	stopped sync.Once

	ntfnServer *subscribe.Server
}

// New creates a new HTLCNotifier which gets HTLC forwarded, failed and settled
// events from links our node has established with peers and sends notifications
// to subscribing clients.
func New() *HTLCNotifier {
	return &HTLCNotifier{
		ntfnServer: subscribe.NewServer(),
	}
}

// Start starts the HTLCNotifier and all goroutines it needs to consume events
// and provide subscriptions to clients.
func (h *HTLCNotifier) Start() error {
	var err error
	h.started.Do(func() {
		log.Trace("HTLCNotifier starting")
		err = h.ntfnServer.Start()
	})
	return err
}

// Stop signals the notifier for a graceful shutdown.
func (h *HTLCNotifier) Stop() {
	h.stopped.Do(func() {
		if err := h.ntfnServer.Stop(); err != nil {
			log.Warnf("error stopping htlc notifier: %v", err)
		}
	})
}

// SubscribeHTLCEvents returns a subscribe.Client that will receive updates
// any time the Server is made aware of a new event.
func (h *HTLCNotifier) SubscribeHTLCEvents() (*subscribe.Client, error) {
	return h.ntfnServer.Subscribe()
}

// HTLCKey uniquely identifies a htlc event. It can be used to associate forward
// events with their subsequent settle/fail event. Note that when matching a
// forward event with its subsequent settle/fail, the key will be reversed,
// because HTLCs are forwarded in one direction, then settled or failed back in
// the reverse direction.
type HTLCKey struct {
	// IncomingChannel is the channel that the incoming HTLC arrived at our node
	// on.
	IncomingChannel lnwire.ShortChannelID

	// OutgoingChannel is the channel that the outgoing HTLC left our node on.
	OutgoingChannel lnwire.ShortChannelID

	// HTLCInIndex is the index of the incoming HTLC in the incoming channel.
	HTLCInIndex uint64

	// HTLCOutIndex is the index of the outgoing HTLC in the outgoing channel.
	HTLCOutIndex uint64
}

type HTLCInfo struct {
	// IncomingTimelock is the time lock of the HTLC on our incoming channel.
	IncomingTimeLock uint32

	// OutgoingTimelock is the time lock the HTLC on our outgoing channel.
	OutgoingTimeLock uint32

	// IncomingAmt is the amount of the HTLC on our incoming channel.
	IncomingAmt lnwire.MilliSatoshi

	// OutgoingAmt is the amount of the HTLC on our outgoing channel.
	OutgoingAmt lnwire.MilliSatoshi

	// PayHash is the payment hash of the HTLC. Note that this value is not
	// guaranteed to be unique.
	PayHash [32]byte
}

// ForwardingEvent represents a HTLC that was forwarded through our node. Sends
// which originate from our node will report forward events with zero incoming
// channel IDs in their HTLCKey.
type ForwardingEvent struct {
	// HTLCKey contains the information which uniquely identifies this forward
	// event.
	HTLCKey

	// HTLCInfo contains further information about the HTLC.
	HTLCInfo

	// Timestamp is the time when this HTLC was forwarded.
	Timestamp time.Time
}

// FailureDetail is an enum which provides further granularity for HTLC failure
// reasons beyond those provided by the failure reasons exposed to the network
// in lnwire FailureMessages. When paired with a lnwire FailureMessage, they
// provide detailed information about failures.
type FailureDetail int

const (
	// FailureDetailNone is used when there are no further failure details
	// beyond lnwire's failure message.
	FailureDetailNone FailureDetail = iota

	// FailureDetailInsufficientBalance accompanies TemporaryChannelFailure to
	// indicate that we did not have sufficient balance in our channel.
	FailureDetailInsufficientBalance

	// FailureDetailHTLCTooBig accompanies TemporaryChannelFailure to indicate
	// that the HTLC violated our maximum HTLC size.
	FailureDetailHTLCTooBig

	// FailureDetailHODLCancel is returned when we cancel a HODL invoice.
	FailureDetailHODLCancel

	// FailureDetailCannotEncodeRoute is returned when we cannot encode the onion
	// packet for the next hop on the route.
	FailureDetailCannotEncodeRoute

	// FailureDetailLinkNotEligible is returned when a link is not eligible for
	// forwarding.
	FailureDetailLinkNotEligible

	// FailureDetailMalformedHTLC is used when we try to forward a HTLC out,
	// but our peer returns a malformed HTLC error.
	FailureDetailMalformedHTLC

	// FailureDetailInvoiceNotFound is returned when an attempt is made to pay an
	// unknown invoice.
	FailureDetailInvoiceNotFound
)

type LinkFailEvent struct {
	// HTLCKey contains the information which uniquely identifies the failed
	// HTLC.
	HTLCKey

	// HTLCInfo contains further information about the HTLC.
	HTLCInfo

	// FailReason is the reason that we failed the HTLC.
	FailReason lnwire.FailureMessage

	// FailDetail provides further information about the failure.
	FailDetail FailureDetail

	// IncomingFailed is true if the HTLC was failed on an incoming link.
	// If it failed on the outgoing link, it is false.
	IncomingFailed bool

	// Timestamp is the time when the link failure occurred.
	Timestamp time.Time
}

// ForwardingFailEvent represents a HTLC failure which occurred down the line
// after we forwarded the HTLC through our node. An error is not included in
// this event because errors returned down the route are encrypted.
type ForwardingFailEvent struct {
	// HTLCKey contains the information which uniquely identifies the original
	// HTLC forwarded event.
	HTLCKey

	// Timestamp is the time when the forwarding failure was received.
	Timestamp time.Time
}

// SettleEvent represents a HTLC that was forwarded by our node and successfully
// settled.
type SettleEvent struct {
	// HTLCKey contains the information which uniquely identifies the original
	// HTLC forwarded event.
	HTLCKey

	// HTLCInfo contains further information about the HTLC.
	HTLCInfo

	// Timestamp is the time when this HTLC was settled.
	Timestamp time.Time
}

// NotifyForwardingEvent notifies the HTLCNotifier than a HTLC has been
// forwarded.
//
// Note this is part of the Notifier interface.
func (h *HTLCNotifier) NotifyForwardingEvent(event ForwardingEvent) {
	log.Debugf("Notifying forward event from %v(%v) to %v(%v)",
		event.IncomingChannel, event.HTLCInIndex, event.OutgoingChannel,
		event.HTLCOutIndex)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send forwarding event: %v", err)
	}
}

// NotifyLinkFailEvent notifies the HTLCNotifier that we have failed a HTLC on
// one of our links.
//
// Note this is part of the Notifier interface.
func (h *HTLCNotifier) NotifyLinkFailEvent(event LinkFailEvent) {
	log.Debugf("Notifying link failure event from %v(%v) to %v(%v),"+
		"incoming link: %v, with reason: %v, details: %v", event.IncomingChannel,
		event.HTLCInIndex, event.OutgoingChannel, event.HTLCOutIndex,
		event.IncomingFailed, event.FailReason, event.FailDetail)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send link fail event: %v", err)
	}
}

// NotifyForwardingFailEvent notifies the HTLCNotifier that a HTLC we forwarded
// has failed down the line.
//
// Note this is part of the Notifier interface.
func (h *HTLCNotifier) NotifyForwardingFailEvent(event ForwardingFailEvent) {
	log.Debugf("Notifying forwarding failure event from %v(%v) to %v(%v)",
		event.IncomingChannel, event.HTLCInIndex, event.OutgoingChannel,
		event.HTLCOutIndex)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send forwarding fail event: %v", err)
	}
}

// NotifySettleEvent notifies the HTLCNotifier that a HTLC that we committed to
// as part of a forward or a receive to our node has been settled.
//
// Note this is part of the Notifier interface.
func (h *HTLCNotifier) NotifySettleEvent(event SettleEvent) {
	log.Debugf("Notifying settle event from %v(%v) to %v(%v)",
		event.IncomingChannel, event.HTLCInIndex, event.OutgoingChannel,
		event.HTLCOutIndex)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send settle event: %v", err)
	}
}
