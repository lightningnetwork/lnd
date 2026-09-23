package gossipsync

import (
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// SyncType is the role a peer plays in keeping our graph up to date.
type SyncType uint8

const (
	// PassiveSync means we don't ask the peer for new gossip. We still
	// answer its queries, and may run a historical sync with it.
	PassiveSync SyncType = iota

	// ActiveSync means we ask the peer for all new gossip, by sending a
	// timestamp filter that starts now and never ends.
	ActiveSync

	// PinnedSync is an ActiveSync peer that the operator configured to
	// always stay active. It is never rotated to passive.
	PinnedSync
)

// String returns a human readable name for the sync type.
func (t SyncType) String() string {
	switch t {
	case PassiveSync:
		return "PassiveSync"
	case ActiveSync:
		return "ActiveSync"
	case PinnedSync:
		return "PinnedSync"
	default:
		return fmt.Sprintf("SyncType(%d)", uint8(t))
	}
}

// wantsGossip reports whether a peer with this sync type should be sending
// us new gossip.
func (t SyncType) wantsGossip() bool {
	return t == ActiveSync || t == PinnedSync
}

// AttemptID identifies one historical sync attempt. The manager assigns a
// fresh ID to every attempt, and the peer syncer echoes it in the outcome, so
// an outcome for an attempt the manager no longer cares about is recognized
// by a single comparison.
type AttemptID uint64

// OutcomeKind says how a historical sync attempt ended.
type OutcomeKind uint8

const (
	// OutcomeCompleted means the peer answered the whole range query and
	// every SCID query, and we now know every channel it told us about.
	OutcomeCompleted OutcomeKind = iota

	// OutcomePeerFault means the peer broke the protocol, sent more than
	// we accept, or stopped answering.
	OutcomePeerFault

	// OutcomeLocalFault means a local lookup failed. The peer did nothing
	// wrong.
	OutcomeLocalFault

	// OutcomeBusy means the syncer could not start the attempt because a
	// previous exchange with the peer is still in flight.
	OutcomeBusy
)

// String returns a human readable name for the outcome kind.
func (k OutcomeKind) String() string {
	switch k {
	case OutcomeCompleted:
		return "Completed"
	case OutcomePeerFault:
		return "PeerFault"
	case OutcomeLocalFault:
		return "LocalFault"
	case OutcomeBusy:
		return "Busy"
	default:
		return fmt.Sprintf("OutcomeKind(%d)", uint8(k))
	}
}

// SyncOutcome is the result of one historical sync attempt. A peer fault is a
// value here rather than a Go error, since it is an expected event on a p2p
// interface.
type SyncOutcome struct {
	// Kind says how the attempt ended.
	Kind OutcomeKind

	// Reason is set for every kind but OutcomeCompleted.
	Reason error
}

// ErrReplyTimeout is the reason given when a peer stops answering our query.
var ErrReplyTimeout = errors.New("peer did not reply in time")

// SyncerEvent is the sealed set of inputs to the peer syncer state machine.
type SyncerEvent interface {
	syncerEvent()
}

// StartHistoricalSync asks the syncer to learn every channel the peer knows,
// by querying the whole chain's channel range. It comes from the manager.
type StartHistoricalSync struct {
	// Attempt is echoed back in the outcome.
	Attempt AttemptID
}

// SetSyncType asks the syncer to change the peer's role, which it does by
// sending a new gossip_timestamp_filter. It comes from the manager, and is
// valid in every state.
type SetSyncType struct {
	// Type is the new role.
	Type SyncType
}

// RangeReplyReceived carries a reply_channel_range from the peer.
type RangeReplyReceived struct {
	// Reply is the wire message.
	Reply *lnwire.ReplyChannelRange
}

// SCIDsEndReceived carries a reply_short_channel_ids_end from the peer.
type SCIDsEndReceived struct {
	// End is the wire message.
	End *lnwire.ReplyShortChanIDsEnd
}

// ReplyTimerFired is delivered when a reply timer armed by the syncer
// expires. Only the timer with the current sequence number means anything;
// every earlier one is stale and ignored.
type ReplyTimerFired struct {
	// Seq is the sequence number the timer was armed with.
	Seq uint64
}

func (*StartHistoricalSync) syncerEvent() {}
func (*SetSyncType) syncerEvent()         {}
func (*RangeReplyReceived) syncerEvent()  {}
func (*SCIDsEndReceived) syncerEvent()    {}
func (*ReplyTimerFired) syncerEvent()     {}

// SyncerOutbox is the sealed set of side effects the peer syncer can request.
type SyncerOutbox interface {
	syncerOutbox()
}

// SendToPeer asks for messages to be sent to the peer, in order.
type SendToPeer struct {
	// Msgs are the messages to send.
	Msgs []lnwire.Message
}

// ReportOutcome asks for an attempt's outcome to be delivered to the manager.
type ReportOutcome struct {
	// Attempt is the attempt that ended.
	Attempt AttemptID

	// Outcome says how it ended.
	Outcome SyncOutcome
}

// ArmReplyTimer asks for a timer that delivers ReplyTimerFired{Seq} after
// the given duration. Arming a new timer supersedes the previous one; the
// executor may cancel it, and the state machine ignores it if it fires.
type ArmReplyTimer struct {
	// Seq is carried back in ReplyTimerFired.
	Seq uint64

	// After is how long to wait.
	After time.Duration
}

// DisarmReplyTimer asks for the current reply timer to be cancelled, because
// the syncer is no longer waiting on the peer.
type DisarmReplyTimer struct{}

func (*SendToPeer) syncerOutbox()       {}
func (*ReportOutcome) syncerOutbox()    {}
func (*ArmReplyTimer) syncerOutbox()    {}
func (*DisarmReplyTimer) syncerOutbox() {}
