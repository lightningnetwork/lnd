//nolint:unused
package htlcswitch

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrStfuAlreadySent indicates that this channel has already sent an
	// Stfu message for this negotiation.
	ErrStfuAlreadySent = fmt.Errorf("stfu already sent")

	// ErrStfuAlreadyRcvd indicates that this channel has already received
	// an Stfu message for this negotiation.
	ErrStfuAlreadyRcvd = fmt.Errorf("stfu already received")
)

// quiescer is a state machine that tracks progression through the quiescence
// protocol.
type quiescer struct {
	// chanID marks what channel we are managing the state machine for. This
	// is important because the quiescer is responsible for constructing the
	// messages we send out and the ChannelID is a key field in that
	// message.
	chanID lnwire.ChannelID

	// channelInitiator indicates which ChannelParty originally opened the
	// channel. This is used to break ties when both sides of the channel
	// send Stfu claiming to be the initiator.
	channelInitiator lntypes.ChannelParty

	// localInit indicates whether our path through this state machine was
	// initiated by our node. This can be true or false independently of
	// remoteInit.
	localInit bool

	// remoteInit indicates whether we received Stfu from our peer where the
	// message indicated that the remote node believes it was the initiator.
	// This can be true or false independently of localInit.
	remoteInit bool

	// sent tracks whether or not we have emitted Stfu for sending.
	sent bool

	// received tracks whether or not we have received Stfu from our peer.
	received bool
}

// recvStfu is called when we receive an Stfu message from the remote.
func (q *quiescer) recvStfu(msg lnwire.Stfu) error {
	if q.received {
		return fmt.Errorf("%w for channel %v", ErrStfuAlreadyRcvd,
			q.chanID)
	}

	q.received = true

	// If the remote party set the initiator bit to true then we will
	// remember that they are making a claim to the initiator role. This
	// does not necessarily mean they will get it, though.
	q.remoteInit = msg.Initiator

	return nil
}

// sendStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *quiescer) sendStfu() fn.Result[lnwire.Stfu] {
	if q.sent {
		return fn.Err[lnwire.Stfu](
			fmt.Errorf("%w for channel %v", ErrStfuAlreadySent,
				q.chanID),
		)
	}

	stfu := lnwire.Stfu{
		ChanID:    q.chanID,
		Initiator: q.localInit,
	}

	q.sent = true

	return fn.Ok(stfu)
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu.
func (q *quiescer) oweStfu() bool {
	return (q.received || q.localInit) && !q.sent
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

// downstreamLeader determines which ChannelParty is the initiator of quiescence
// for the purposes of downstream protocols. If the channel is not currently
// quiescent, this method will return None.
func (q *quiescer) downstreamLeader() fn.Option[lntypes.ChannelParty] {
	if !q.isQuiescent() {
		return fn.None[lntypes.ChannelParty]()
	}

	// We assume it is impossible for both to be false, if the channel is
	// quiescent. However, we use the same tie-breaking scheme no matter
	// what.
	if q.localInit == q.remoteInit {
		return fn.Some(q.channelInitiator)
	}

	// In this case we know that only one of the values is set so we just
	// return the one that indicates whether we end up as the initiator.
	if q.localInit {
		return fn.Some(lntypes.Local)
	}

	return fn.Some(lntypes.Remote)
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *quiescer) canSendUpdates() bool {
	return !q.sent && !q.localInit
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *quiescer) canRecvUpdates() bool {
	return !q.received
}

// initStfu instructs the quiescer that we intend to begin a quiescence
// negotiation where we are the initiator. We don't yet send stfu yet because
// we need to wait for the link to give us a valid opportunity to do so.
func (q *quiescer) initStfu() error {
	if q.localInit {
		return fmt.Errorf("quiescence already requested")
	}

	q.localInit = true

	return nil
}
