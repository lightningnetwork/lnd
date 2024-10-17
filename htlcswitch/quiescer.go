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

	// ErrNoQuiescenceInitiator indicates that the caller has requested the
	// quiescence initiator for a channel that is not yet quiescent.
	ErrNoQuiescenceInitiator = fmt.Errorf(
		"indeterminate quiescence initiator: channel is not quiescent",
	)

	// ErrPendingRemoteUpdates indicates that we have received an Stfu while
	// the remote party has issued updates that are not yet bilaterally
	// committed.
	ErrPendingRemoteUpdates = fmt.Errorf(
		"stfu received with pending remote updates",
	)

	// ErrPendingLocalUpdates indicates that we are attempting to send an
	// Stfu while we have issued updates that are not yet bilaterally
	// committed.
	ErrPendingLocalUpdates = fmt.Errorf(
		"stfu send attempted with pending local updates",
	)
)

type StfuReq = fn.Req[fn.Unit, fn.Result[lntypes.ChannelParty]]

type quiescer interface {
	// isQuiescent returns true if the state machine has been driven all the
	// way to completion. If this returns true, processes that depend on
	// channel quiescence may proceed.
	isQuiescent() bool

	// initStfu instructs the quiescer that we intend to begin a quiescence
	// negotiation where we are the initiator. We don't yet send stfu yet
	// because we need to wait for the link to give us a valid opportunity
	// to do so.
	initStfu(req StfuReq)

	// recvStfu is called when we receive an Stfu message from the remote.
	recvStfu(stfu lnwire.Stfu) error

	// canRecvUpdates returns true if we haven't yet received an Stfu which
	// would mark the end of the remote's ability to send updates.
	canRecvUpdates() bool

	// canSendUpdates returns true if we haven't yet sent an Stfu which
	// would mark the end of our ability to send updates.
	canSendUpdates() bool

	// drive drives the quiescence machine forward. It returns an error if
	// the state machine is in an invalid state.
	drive() error

	// onResume accepts a no return closure that will run when the quiescer
	// is resumed.
	onResume(hook func())

	// resume runs all of the deferred actions that have accumulated while
	// the channel has been quiescent and then resets the quiescer state to
	// its initial state.
	resume()
}

type quiescerCfg struct {
	// chanID marks what channel we are managing the state machine for. This
	// is important because the quiescer is responsible for constructing the
	// messages we send out and the ChannelID is a key field in that
	// message.
	chanID lnwire.ChannelID

	// channelInitiator indicates which ChannelParty originally opened the
	// channel. This is used to break ties when both sides of the channel
	// send Stfu claiming to be the initiator.
	channelInitiator lntypes.ChannelParty

	// numPendingUpdates is a function that returns the number of pending
	// originated by the party in the first argument that have yet to be
	// committed to the commitment transaction held by the party in the
	// second argument.
	numPendingUpdates func(lntypes.ChannelParty,
		lntypes.ChannelParty) uint64

	// sendMsg is a function that can be used to send an Stfu message over
	// the wire.
	sendMsg func(lnwire.Stfu) error
}

// quiescerLive is a state machine that tracks progression through the
// quiescence protocol.
type quiescerLive struct {
	cfg quiescerCfg

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

	// activeQuiescenceRequest is a possibly None Request that we should
	// resolve when we complete quiescence.
	activeQuiescenceReq fn.Option[StfuReq]

	// resumeQueue is a slice of hooks that will be called when the quiescer
	// is resumed. These are actions that needed to be deferred while the
	// channel was quiescent.
	resumeQueue []func()
}

// newQuiescer creates a new quiescer for the given channel.
func newQuiescer(cfg quiescerCfg) quiescer {
	return &quiescerLive{
		cfg: cfg,
	}
}

// recvStfu is called when we receive an Stfu message from the remote.
func (q *quiescerLive) recvStfu(msg lnwire.Stfu) error {
	// At the time of this writing, this check that we have already received
	// an Stfu is not strictly necessary, according to the specification.
	// However, it is fishy if we do and it is unclear how we should handle
	// such a case so we will err on the side of caution.
	if q.received {
		return fmt.Errorf("%w for channel %v", ErrStfuAlreadyRcvd,
			q.cfg.chanID)
	}

	if !q.canRecvStfu() {
		return fmt.Errorf("%w for channel %v", ErrPendingRemoteUpdates,
			q.cfg.chanID)
	}

	q.received = true

	// If the remote party sets the initiator bit to true then we will
	// remember that they are making a claim to the initiator role. This
	// does not necessarily mean they will get it, though.
	q.remoteInit = msg.Initiator

	// Since we just received an Stfu, we may have a newly quiesced state.
	// If so, we will try to resolve any outstanding StfuReqs.
	q.tryResolveStfuReq()

	return nil
}

// makeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *quiescerLive) makeStfu() fn.Result[lnwire.Stfu] {
	if q.sent {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrStfuAlreadySent, q.cfg.chanID)
	}

	if !q.canSendStfu() {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrPendingLocalUpdates, q.cfg.chanID)
	}

	stfu := lnwire.Stfu{
		ChanID:    q.cfg.chanID,
		Initiator: q.localInit,
	}

	return fn.Ok(stfu)
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *quiescerLive) oweStfu() bool {
	return (q.received || q.localInit) && !q.sent
}

// needStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *quiescerLive) needStfu() bool {
	return q.sent && !q.received
}

// isQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *quiescerLive) isQuiescent() bool {
	return q.sent && q.received
}

// quiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoDownstreamLeader.
func (q *quiescerLive) quiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	switch {
	case !q.isQuiescent():
		return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)

	case q.localInit && q.remoteInit:
		// In the case of a tie, the channel initiator wins.
		return fn.Ok(q.cfg.channelInitiator)

	case !q.localInit && !q.remoteInit:
		// We assume it is impossible for both to be false if the
		// channel is quiescent.
		panic("impossible: one party must have initiated quiescence")

	case q.localInit:
		return fn.Ok(lntypes.Local)

	case q.remoteInit:
		return fn.Ok(lntypes.Remote)
	}

	panic("impossible: non-exhaustive switch quiescer.downstreamLeader")
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *quiescerLive) canSendUpdates() bool {
	return !q.sent && !q.localInit
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *quiescerLive) canRecvUpdates() bool {
	return !q.received
}

// canSendStfu returns true if we can send an Stfu.
func (q *quiescerLive) canSendStfu() bool {
	return q.cfg.numPendingUpdates(lntypes.Local, lntypes.Local) == 0 &&
		q.cfg.numPendingUpdates(lntypes.Local, lntypes.Remote) == 0
}

// canRecvStfu returns true if we can receive an Stfu.
func (q *quiescerLive) canRecvStfu() bool {
	return q.cfg.numPendingUpdates(lntypes.Remote, lntypes.Local) == 0 &&
		q.cfg.numPendingUpdates(lntypes.Remote, lntypes.Remote) == 0
}

// drive drives the quiescence machine forward. It returns an error if the state
// machine is in an invalid state.
func (q *quiescerLive) drive() error {
	if !q.oweStfu() || !q.canSendStfu() {
		return nil
	}

	stfu, err := q.makeStfu().Unpack()
	if err != nil {
		return err
	}

	err = q.cfg.sendMsg(stfu)
	if err != nil {
		return err
	}

	q.sent = true

	// Since we just sent an Stfu, we may have a newly quiesced state.
	// If so, we will try to resolve any outstanding StfuReqs.
	q.tryResolveStfuReq()

	return nil
}

// tryResolveStfuReq attempts to resolve the active quiescence request if the
// state machine has reached a quiescent state.
func (q *quiescerLive) tryResolveStfuReq() {
	q.activeQuiescenceReq.WhenSome(
		func(req StfuReq) {
			if q.isQuiescent() {
				req.Resolve(q.quiescenceInitiator())
				q.activeQuiescenceReq = fn.None[StfuReq]()
			}
		},
	)
}

// initStfu instructs the quiescer that we intend to begin a quiescence
// negotiation where we are the initiator. We don't yet send stfu yet because
// we need to wait for the link to give us a valid opportunity to do so.
func (q *quiescerLive) initStfu(req StfuReq) {
	if q.localInit {
		req.Resolve(fn.Errf[lntypes.ChannelParty](
			"quiescence already requested",
		))

		return
	}

	q.localInit = true
	q.activeQuiescenceReq = fn.Some(req)
}

// onResume accepts a no return closure that will run when the quiescer is
// resumed.
func (q *quiescerLive) onResume(hook func()) {
	q.resumeQueue = append(q.resumeQueue, hook)
}

// resume runs all of the deferred actions that have accumulated while the
// channel has been quiescent and then resets the quiescer state to its initial
// state.
func (q *quiescerLive) resume() {
	for _, hook := range q.resumeQueue {
		hook()
	}
	q.localInit = false
	q.remoteInit = false
	q.sent = false
	q.received = false
	q.resumeQueue = nil
}

type quiescerNoop struct{}

var _ quiescer = (*quiescerNoop)(nil)

func (q *quiescerNoop) initStfu(req StfuReq) {
	req.Resolve(fn.Errf[lntypes.ChannelParty]("quiescence not supported"))
}
func (q *quiescerNoop) recvStfu(_ lnwire.Stfu) error { return nil }
func (q *quiescerNoop) canRecvUpdates() bool         { return true }
func (q *quiescerNoop) canSendUpdates() bool         { return true }
func (q *quiescerNoop) drive() error                 { return nil }
func (q *quiescerNoop) isQuiescent() bool            { return false }
func (q *quiescerNoop) onResume(hook func())         { hook() }
func (q *quiescerNoop) resume()                      {}
