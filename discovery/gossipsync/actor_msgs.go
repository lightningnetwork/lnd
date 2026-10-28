package gossipsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Ack is the empty response of actors that are only told things.
type Ack struct{}

// syncerMsg is the sealed message type of the peer syncer actor.
type syncerMsg interface {
	actor.Message
	syncerMsg()
}

// syncerEventMsg delivers a SyncerEvent to a peer syncer actor.
type syncerEventMsg struct {
	actor.BaseMessage

	// event is the event to apply.
	event SyncerEvent
}

// MessageType returns the message's type name.
func (m *syncerEventMsg) MessageType() string {
	return fmt.Sprintf("syncerEvent(%T)", m.event)
}

func (m *syncerEventMsg) syncerMsg() {}

// managerMsg is the sealed message type of the manager actor.
type managerMsg interface {
	actor.Message
	managerMsg()
}

// managerEventMsg delivers a ManagerEvent to the manager actor.
type managerEventMsg struct {
	actor.BaseMessage

	// event is the event to apply.
	event ManagerEvent
}

// MessageType returns the message's type name.
func (m *managerEventMsg) MessageType() string {
	return fmt.Sprintf("managerEvent(%T)", m.event)
}

func (m *managerEventMsg) managerMsg() {}

// connectMsg registers a new peer connection with the manager. It is sent
// with Ask, and answered once the peer's actors are running.
type connectMsg struct {
	actor.BaseMessage

	// conn is the connection.
	conn PeerConn

	// pinned is set if the operator pinned the peer.
	pinned bool
}

// MessageType returns the message's type name.
func (m *connectMsg) MessageType() string { return "connect" }

func (m *connectMsg) managerMsg() {}

// lifecycleMsg starts or stops the manager. Stop is sent with Ask, and
// answered once every peer's actors have been stopped.
type lifecycleMsg struct {
	actor.BaseMessage

	// start is set to start, and clear to stop.
	start bool
}

// MessageType returns the message's type name.
func (m *lifecycleMsg) MessageType() string { return "lifecycle" }

func (m *lifecycleMsg) managerMsg() {}

// roleQuery asks the manager for a peer's role. It is sent with Ask.
type roleQuery struct {
	actor.BaseMessage

	// peer is the peer to look up.
	peer route.Vertex
}

// MessageType returns the message's type name.
func (m *roleQuery) MessageType() string { return "roleQuery" }

func (m *roleQuery) managerMsg() {}

// ManagerResp is the manager actor's response to an Ask.
type ManagerResp struct {
	// Role is set in answer to a role query for a connected peer.
	Role fn.Option[SyncType]
}

// responderMsg is the sealed message type of the responder actor.
type responderMsg interface {
	actor.Message
	responderMsg()
}

// peerQueryMsg carries a query_channel_range or query_short_channel_ids the
// peer sent us.
type peerQueryMsg struct {
	actor.BaseMessage

	// query is the wire message.
	query lnwire.Message
}

// MessageType returns the message's type name.
func (m *peerQueryMsg) MessageType() string {
	return fmt.Sprintf("peerQuery(%T)", m.query)
}

func (m *peerQueryMsg) responderMsg() {}

// peerFilterMsg carries the gossip_timestamp_filter the peer sent us.
type peerFilterMsg struct {
	actor.BaseMessage

	// filter is the wire message.
	filter *lnwire.GossipTimestampRange
}

// MessageType returns the message's type name.
func (m *peerFilterMsg) MessageType() string { return "peerFilter" }

func (m *peerFilterMsg) responderMsg() {}

// ForwardMsg is a gossip message to forward to peers whose filter admits it.
type ForwardMsg struct {
	// Msg is the announcement or update.
	Msg lnwire.Message

	// Senders are the peers that sent it to us, which don't need it back.
	Senders map[route.Vertex]struct{}
}

// forwardMsg carries a batch of live gossip for the responder to filter and
// forward.
type forwardMsg struct {
	actor.BaseMessage

	// batch is the gossip to forward.
	batch []ForwardMsg
}

// MessageType returns the message's type name.
func (m *forwardMsg) MessageType() string { return "forward" }

func (m *forwardMsg) responderMsg() {}

// backlogPageMsg asks the responder to send the next page of a backlog
// replay. It is only ever sent by the responder to itself.
type backlogPageMsg struct {
	actor.BaseMessage

	// gen identifies the replay the page belongs to. A page for an
	// earlier replay is stale and ignored.
	gen uint64
}

// MessageType returns the message's type name.
func (m *backlogPageMsg) MessageType() string { return "backlogPage" }

func (m *backlogPageMsg) responderMsg() {}

// managerKey is the service key of the manager actor.
var managerKey = actor.NewServiceKey[managerMsg, ManagerResp](
	"gossipsync.manager",
)

// syncerKey returns the service key of a peer's syncer actor.
func syncerKey(pub route.Vertex) actor.ServiceKey[syncerMsg, Ack] {
	return actor.NewServiceKey[syncerMsg, Ack](
		"gossipsync.syncer." + pub.String(),
	)
}

// responderKey returns the service key of a peer's responder actor.
func responderKey(pub route.Vertex) actor.ServiceKey[responderMsg, Ack] {
	return actor.NewServiceKey[responderMsg, Ack](
		"gossipsync.responder." + pub.String(),
	)
}

// courier delivers messages that must not be lost to actors whose mailbox
// may be full, without ever blocking the sender. It first tries a
// non-blocking send. If the mailbox is full, it hands the message to the
// timeout actor as a reminder that fires at once, and the timeout actor
// retries delivery on a backoff until it lands or the target stops.
//
// A courier is owned by one actor and only used from its Receive method.
type courier struct {
	// timeouts is the timeout actor.
	timeouts actor.TellOnlyRef[timeout.Msg]

	// prefix makes this courier's reminder IDs unique.
	prefix string

	// seq numbers this courier's reminders.
	seq uint64
}

// deliver sends msg to target without blocking and without losing it.
func deliver[M actor.Message](ctx context.Context, c *courier,
	target actor.TellOnlyRef[M], msg M) {

	err := target.TryTell(ctx, msg)
	if err == nil || errors.Is(err, actor.ErrActorTerminated) {
		return
	}

	c.seq++
	c.timeouts.Tell(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeout.ID(fmt.Sprintf("%s/%d", c.prefix, c.seq)),
		Duration: 0,
		Callback: timeout.MapTimeoutExpired(
			target, func(timeout.ExpiredMsg) M {
				return msg
			},
		),
	})
}
