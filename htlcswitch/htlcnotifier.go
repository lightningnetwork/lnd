package htlcswitch

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
)

// HtlcNotifier notifies clients of htlc forwards, failures and settles for
// htlcs that the switch handles. It takes subscriptions for its events and
// notifies them when htlc events occur. These are served on a best-effort
// basis; events are not persisted, delivery is not guaranteed (in the event
// of a crash in the switch, forward events may be lost) and some events may
// be replayed upon restart. Events consumed from this package should be
// de-duplicated by the htlc's unique combination of incoming+outgoing circuit
// and not relied upon for critical operations.
//
// The htlc notifier sends the following kinds of events:
// Forwarding Event:
// - Represents a htlc which is forwarded onward from our node.
// - Present for htlc forwards through our node and local sends.
//
// Link Failure Event:
//   - Indicates that a htlc has failed on our incoming or outgoing link,
//     with an incoming boolean which indicates where the failure occurred.
//   - Incoming link failures are present for failed attempts to pay one of
//     our invoices (insufficient amount or mpp timeout, for example) and for
//     forwards that we cannot decode to forward onwards.
//   - Outgoing link failures are present for forwards or local payments that
//     do not meet our outgoing link's policy (insufficient fees, for example)
//     and when we fail to forward the payment on (insufficient outgoing
//     capacity, or an unknown outgoing link).
//
// Forwarding Failure Event:
//   - Forwarding failures indicate that a htlc we forwarded has failed at
//     another node down the route.
//   - Present for local sends and htlc forwards which fail after they left
//     our node.
//
// Settle event:
//   - Settle events are present when a htlc which we added is settled through
//     the release of a preimage.
//   - Present for local receives, and successful local sends or forwards.
//
// Each htlc is identified by its incoming and outgoing circuit key. Htlcs,
// and their subsequent settles or fails, can be identified by the combination
// of incoming and outgoing circuits. Note that receives to our node will
// have a zero outgoing circuit key because the htlc terminates at our
// node, and sends from our node will have a zero incoming circuit key because
// the send originates at our node.
type HtlcNotifier struct {
	started sync.Once
	stopped sync.Once

	// now returns the current time, it is set in the htlcnotifier to allow
	// for timestamp mocking in tests.
	now func() time.Time

	ntfnServer *subscribe.Server
}

// NewHtlcNotifier creates a new HtlcNotifier which gets htlc forwarded,
// failed and settled events from links our node has established with peers
// and sends notifications to subscribing clients.
func NewHtlcNotifier(now func() time.Time) *HtlcNotifier {
	return &HtlcNotifier{
		now:        now,
		ntfnServer: subscribe.NewServer(),
	}
}

// Start starts the HtlcNotifier and all goroutines it needs to consume
// events and provide subscriptions to clients.
func (h *HtlcNotifier) Start() error {
	var err error
	h.started.Do(func() {
		log.Info("HtlcNotifier starting")
		err = h.ntfnServer.Start()
	})
	return err
}

// Stop signals the notifier for a graceful shutdown.
func (h *HtlcNotifier) Stop() error {
	var err error
	h.stopped.Do(func() {
		log.Info("HtlcNotifier shutting down...")
		defer log.Debug("HtlcNotifier shutdown complete")

		if err = h.ntfnServer.Stop(); err != nil {
			log.Warnf("error stopping htlc notifier: %v", err)
		}
	})
	return err
}

// SubscribeHtlcEvents returns a subscribe.Client that will receive updates
// any time the server is made aware of a new event.
func (h *HtlcNotifier) SubscribeHtlcEvents() (*subscribe.Client, error) {
	return h.ntfnServer.Subscribe()
}

// HtlcKey uniquely identifies the htlc.
type HtlcKey struct {
	// IncomingCircuit is the channel an htlc id of the incoming htlc.
	IncomingCircuit models.CircuitKey

	// OutgoingCircuit is the channel and htlc id of the outgoing htlc.
	OutgoingCircuit models.CircuitKey
}

// String returns a string representation of a htlc key.
func (k HtlcKey) String() string {
	switch {
	case k.IncomingCircuit.ChanID == hop.Source:
		return k.OutgoingCircuit.String()

	case k.OutgoingCircuit.ChanID == hop.Exit:
		return k.IncomingCircuit.String()

	default:
		return fmt.Sprintf("%v -> %v", k.IncomingCircuit,
			k.OutgoingCircuit)
	}
}

// HtlcInfo provides the details of a htlc that our node has processed. For
// forwards, incoming and outgoing values are set, whereas sends and receives
// will only have outgoing or incoming details set.
type HtlcInfo struct {
	// IncomingTimelock is the time lock of the htlc on our incoming
	// channel.
	IncomingTimeLock uint32

	// OutgoingTimelock is the time lock the htlc on our outgoing channel.
	OutgoingTimeLock uint32

	// IncomingAmt is the amount of the htlc on our incoming channel.
	IncomingAmt lnwire.MilliSatoshi

	// OutgoingAmt is the amount of the htlc on our outgoing channel.
	OutgoingAmt lnwire.MilliSatoshi
}

// String returns a string representation of a htlc.
func (h HtlcInfo) String() string {
	var details []string

	// If the incoming information is not zero, as is the case for a send,
	// we include the incoming amount and timelock.
	if h.IncomingAmt != 0 || h.IncomingTimeLock != 0 {
		str := fmt.Sprintf("incoming amount: %v, "+
			"incoming timelock: %v", h.IncomingAmt,
			h.IncomingTimeLock)

		details = append(details, str)
	}

	// If the outgoing information is not zero, as is the case for a
	// receive, we include the outgoing amount and timelock.
	if h.OutgoingAmt != 0 || h.OutgoingTimeLock != 0 {
		str := fmt.Sprintf("outgoing amount: %v, "+
			"outgoing timelock: %v", h.OutgoingAmt,
			h.OutgoingTimeLock)

		details = append(details, str)
	}

	return strings.Join(details, ", ")
}

// HtlcEventType represents the type of event that a htlc was part of.
type HtlcEventType int

const (
	// HtlcEventTypeSend represents a htlc that was part of a send from
	// our node.
	HtlcEventTypeSend HtlcEventType = iota

	// HtlcEventTypeReceive represents a htlc that was part of a receive
	// to our node.
	HtlcEventTypeReceive

	// HtlcEventTypeForward represents a htlc that was forwarded through
	// our node.
	HtlcEventTypeForward
)

// String returns a string representation of a htlc event type.
func (h HtlcEventType) String() string {
	switch h {
	case HtlcEventTypeSend:
		return "send"

	case HtlcEventTypeReceive:
		return "receive"

	case HtlcEventTypeForward:
		return "forward"

	default:
		return "unknown"
	}
}

// ForwardingEvent represents a htlc that was forwarded onwards from our node.
// Sends which originate from our node will report forward events with zero
// incoming circuits in their htlc key.
type ForwardingEvent struct {
	// HtlcKey uniquely identifies the htlc, and can be used to match the
	// forwarding event with subsequent settle/fail events.
	HtlcKey

	// HtlcInfo contains details about the htlc.
	HtlcInfo

	// HtlcEventType classifies the event as part of a local send or
	// receive, or as part of a forward.
	HtlcEventType

	// Timestamp is the time when this htlc was forwarded.
	Timestamp time.Time
}

// LinkFailEvent describes a htlc that failed on our incoming or outgoing
// link. The incoming bool is true for failures on incoming links, and false
// for failures on outgoing links. The failure reason is provided by a lnwire
// failure message which is enriched with a failure detail in the cases where
// the wire failure message does not contain full information about the
// failure.
type LinkFailEvent struct {
	// HtlcKey uniquely identifies the htlc.
	HtlcKey

	// HtlcInfo contains details about the htlc.
	HtlcInfo

	// HtlcEventType classifies the event as part of a local send or
	// receive, or as part of a forward.
	HtlcEventType

	// LinkError is the reason that we failed the htlc.
	LinkError *LinkError

	// Incoming is true if the htlc was failed on an incoming link.
	// If it failed on the outgoing link, it is false.
	Incoming bool

	// Timestamp is the time when the link failure occurred.
	Timestamp time.Time
}

// ForwardingFailEvent represents a htlc failure which occurred down the line
// after we forwarded a htlc onwards. An error is not included in this event
// because errors returned down the route are encrypted. HtlcInfo is not
// reliably available for forwarding failures, so it is omitted. These events
// should be matched with their corresponding forward event to obtain this
// information.
type ForwardingFailEvent struct {
	// HtlcKey uniquely identifies the htlc, and can be used to match the
	// htlc with its corresponding forwarding event.
	HtlcKey

	// HtlcEventType classifies the event as part of a local send or
	// receive, or as part of a forward.
	HtlcEventType

	// Timestamp is the time when the forwarding failure was received.
	Timestamp time.Time
}

// SettleEvent represents a htlc that was settled. HtlcInfo is not reliably
// available for forwarding failures, so it is omitted. These events should
// be matched with corresponding forward events or invoices (for receives)
// to obtain additional information about the htlc.
type SettleEvent struct {
	// HtlcKey uniquely identifies the htlc, and can be used to match
	// forwards with their corresponding forwarding event.
	HtlcKey

	// Preimage that was released for settling the htlc.
	Preimage lntypes.Preimage

	// HtlcEventType classifies the event as part of a local send or
	// receive, or as part of a forward.
	HtlcEventType

	// Timestamp is the time when this htlc was settled.
	Timestamp time.Time
}

type FinalHtlcEvent struct {
	CircuitKey

	Settled bool

	// Offchain is indicating whether the htlc was resolved off-chain.
	Offchain bool

	// Timestamp is the time when this htlc was settled.
	Timestamp time.Time
}

// NotifyForwardingEvent notifies the HtlcNotifier than a htlc has been
// forwarded.
//
// Note this is part of the htlcNotifier interface.
func (h *HtlcNotifier) NotifyForwardingEvent(key HtlcKey, info HtlcInfo,
	eventType HtlcEventType) {

	event := &ForwardingEvent{
		HtlcKey:       key,
		HtlcInfo:      info,
		HtlcEventType: eventType,
		Timestamp:     h.now(),
	}

	log.Tracef("Notifying forward event: %v over %v, %v", eventType, key,
		info)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send forwarding event: %v", err)
	}
}

// NotifyLinkFailEvent notifies that a htlc has failed on our incoming
// or outgoing link.
//
// Note this is part of the htlcNotifier interface.
func (h *HtlcNotifier) NotifyLinkFailEvent(key HtlcKey, info HtlcInfo,
	eventType HtlcEventType, linkErr *LinkError, incoming bool) {

	event := &LinkFailEvent{
		HtlcKey:       key,
		HtlcInfo:      info,
		HtlcEventType: eventType,
		LinkError:     linkErr,
		Incoming:      incoming,
		Timestamp:     h.now(),
	}

	log.Tracef("Notifying link failure event: %v over %v, %v", eventType,
		key, info)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send link fail event: %v", err)
	}
}

// NotifyForwardingFailEvent notifies the HtlcNotifier that a htlc we
// forwarded has failed down the line.
//
// Note this is part of the htlcNotifier interface.
func (h *HtlcNotifier) NotifyForwardingFailEvent(key HtlcKey,
	eventType HtlcEventType) {

	event := &ForwardingFailEvent{
		HtlcKey:       key,
		HtlcEventType: eventType,
		Timestamp:     h.now(),
	}

	log.Tracef("Notifying forwarding failure event: %v over %v", eventType,
		key)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send forwarding fail event: %v", err)
	}
}

// NotifySettleEvent notifies the HtlcNotifier that a htlc that we committed
// to as part of a forward or a receive to our node has been settled.
//
// Note this is part of the htlcNotifier interface.
func (h *HtlcNotifier) NotifySettleEvent(key HtlcKey,
	preimage lntypes.Preimage, eventType HtlcEventType) {

	event := &SettleEvent{
		HtlcKey:       key,
		Preimage:      preimage,
		HtlcEventType: eventType,
		Timestamp:     h.now(),
	}

	log.Tracef("Notifying settle event: %v over %v", eventType, key)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send settle event: %v", err)
	}
}

// NotifyFinalHtlcEvent notifies the HtlcNotifier that the final outcome for an
// htlc has been determined.
//
// Note this is part of the htlcNotifier interface.
func (h *HtlcNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) {

	event := &FinalHtlcEvent{
		CircuitKey: key,
		Settled:    info.Settled,
		Offchain:   info.Offchain,
		Timestamp:  h.now(),
	}

	log.Tracef("Notifying final settle event: %v", key)

	if err := h.ntfnServer.SendUpdate(event); err != nil {
		log.Warnf("Unable to send settle event: %v", err)
	}
}

// newHtlc key returns a htlc key for the packet provided. If the packet
// has a zero incoming channel ID, the packet is for one of our own sends,
// which has the payment id stashed in the incoming htlc id. If this is the
// case, we replace the incoming htlc id with zero so that the notifier
// consistently reports zero circuit keys for events that terminate or
// originate at our node.
func newHtlcKey(pkt *htlcPacket) HtlcKey {
	htlcKey := HtlcKey{
		IncomingCircuit: models.CircuitKey{
			ChanID: pkt.incomingChanID,
			HtlcID: pkt.incomingHTLCID,
		},
		OutgoingCircuit: CircuitKey{
			ChanID: pkt.outgoingChanID,
			HtlcID: pkt.outgoingHTLCID,
		},
	}

	// If the packet has a zero incoming channel ID, it is a send that was
	// initiated at our node. If this is the case, our internal pid is in
	// the incoming htlc ID, so we overwrite it with 0 for notification
	// purposes.
	if pkt.incomingChanID == hop.Source {
		htlcKey.IncomingCircuit.HtlcID = 0
	}

	return htlcKey
}

// newHtlcInfo returns HtlcInfo for the packet provided.
func newHtlcInfo(pkt *htlcPacket) HtlcInfo {
	return HtlcInfo{
		IncomingTimeLock: pkt.incomingTimeout,
		OutgoingTimeLock: pkt.outgoingTimeout,
		IncomingAmt:      pkt.incomingAmount,
		OutgoingAmt:      pkt.amount,
	}
}

// getEventType returns the htlc type based on the fields set in the htlc
// packet. Sends that originate at our node have the source (zero) incoming
// channel ID. Receives to our node have the exit (zero) outgoing channel ID
// and forwards have both fields set.
func getEventType(pkt *htlcPacket) HtlcEventType {
	switch {
	case pkt.incomingChanID == hop.Source:
		return HtlcEventTypeSend

	case pkt.outgoingChanID == hop.Exit:
		return HtlcEventTypeReceive

	default:
		return HtlcEventTypeForward
	}
}
