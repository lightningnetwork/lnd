package htlcswitch

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

// quiescer is a state machine that tracks progression through the quiescence
// protocol.
type quiescer struct {
	chanID   lnwire.ChannelID
	sent     bool
	received bool
}

// recvStfu is called when we receive an Stfu message from the remote. It
// returns a bool indicating whether or not the channel is now quiescent as a
// result.
func (q *quiescer) recvStfu() (bool, error) {
	if q.received {
		return false,
			fmt.Errorf(
				"stfu already received for channel %s",
				q.chanID.String(),
			)
	}

	q.received = true

	return q.sent, nil
}

// sendStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *quiescer) sendStfu() (fn.Option[lnwire.Stfu], error) {
	if q.sent {
		return fn.None[lnwire.Stfu](),
			fmt.Errorf(
				"stfu arleady sent for channel %s",
				q.chanID.String(),
			)
	}

	stfu := lnwire.Stfu{
		ChanID:    q.chanID,
		Initiator: !q.received,
	}

	return fn.Some(stfu), nil
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu.
func (q *quiescer) oweStfu() bool {
	return q.received && !q.sent
}

// needStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *quiescer) needStfu() bool {
	return q.sent && !q.received
}

// isQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *quiescer) isQuiescent() bool {
	return q.sent && q.received
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *quiescer) canSendUpdates() bool {
	return !q.sent
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *quiescer) canRecvUpdates() bool {
	return !q.received
}
