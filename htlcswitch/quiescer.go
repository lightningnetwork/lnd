package htlcswitch

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrInvalidStfu indicates that the Stfu we have received is invalid.
	// This can happen in instances where we have not sent Stfu but we have
	// received one with the initiator field set to false.
	ErrInvalidStfu = fmt.Errorf("stfu received is invalid")

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

// QuiescerCfg is a config structure used to initialize a quiescer giving it the
// appropriate functionality to interact with the channel state that the
// quiescer must syncrhonize with.
type QuiescerCfg struct {
	// chanID marks what channel we are managing the state machine for. This
	// is important because the quiescer needs to know the ChannelID to
	// construct the Stfu message.
	chanID lnwire.ChannelID

	// channelInitiator indicates which ChannelParty originally opened the
	// channel. This is used to break ties when both sides of the channel
	// send Stfu claiming to be the initiator.
	channelInitiator lntypes.ChannelParty

	// sendMsg is a function that can be used to send an Stfu message over
	// the wire.
	sendMsg func(lnwire.Stfu) error
}

// Quiescer is a state machine that tracks progression through the quiescence
// protocol.
type Quiescer struct {
	cfg QuiescerCfg

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

	sync.RWMutex
}

// NewQuiescer creates a new quiescer for the given channel.
func NewQuiescer(cfg QuiescerCfg) Quiescer {
	return Quiescer{
		cfg: cfg,
	}
}

// RecvStfu is called when we receive an Stfu message from the remote.
func (q *Quiescer) RecvStfu(msg lnwire.Stfu,
	numPendingRemoteUpdates uint64) error {

	q.Lock()
	defer q.Unlock()

	return q.recvStfu(msg, numPendingRemoteUpdates)
}

// recvStfu is called when we receive an Stfu message from the remote.
func (q *Quiescer) recvStfu(msg lnwire.Stfu,
	numPendingRemoteUpdates uint64) error {

	// At the time of this writing, this check that we have already received
	// an Stfu is not strictly necessary, according to the specification.
	// However, it is fishy if we do and it is unclear how we should handle
	// such a case so we will err on the side of caution.
	if q.received {
		return fmt.Errorf("%w for channel %v", ErrStfuAlreadyRcvd,
			q.cfg.chanID)
	}

	// We need to check that the Stfu we are receiving is valid.
	if !q.sent && !msg.Initiator {
		return fmt.Errorf("%w for channel %v", ErrInvalidStfu,
			q.cfg.chanID)
	}

	if !q.canRecvStfu(numPendingRemoteUpdates) {
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

// MakeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *Quiescer) MakeStfu(
	numPendingLocalUpdates uint64) fn.Result[lnwire.Stfu] {

	q.RLock()
	defer q.RUnlock()

	return q.makeStfu(numPendingLocalUpdates)
}

// makeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *Quiescer) makeStfu(
	numPendingLocalUpdates uint64) fn.Result[lnwire.Stfu] {

	if q.sent {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrStfuAlreadySent, q.cfg.chanID)
	}

	if !q.canSendStfu(numPendingLocalUpdates) {
		return fn.Errf[lnwire.Stfu]("%w for channel %v",
			ErrPendingLocalUpdates, q.cfg.chanID)
	}

	stfu := lnwire.Stfu{
		ChanID:    q.cfg.chanID,
		Initiator: q.localInit,
	}

	return fn.Ok(stfu)
}

// OweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *Quiescer) OweStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.oweStfu()
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *Quiescer) oweStfu() bool {
	return (q.received || q.localInit) && !q.sent
}

// NeedStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *Quiescer) NeedStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.needStfu()
}

// needStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *Quiescer) needStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.sent && !q.received
}

// IsQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *Quiescer) IsQuiescent() bool {
	q.RLock()
	defer q.RUnlock()

	return q.isQuiescent()
}

// isQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *Quiescer) isQuiescent() bool {
	return q.sent && q.received
}

// QuiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoQuiescenceInitiator.
func (q *Quiescer) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	q.RLock()
	defer q.RUnlock()

	return q.quiescenceInitiator()
}

// quiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoQuiescenceInitiator.
func (q *Quiescer) quiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	switch {
	case !q.isQuiescent():
		return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)

	case q.localInit && q.remoteInit:
		// In the case of a tie, the channel initiator wins.
		return fn.Ok(q.cfg.channelInitiator)

	case q.localInit:
		return fn.Ok(lntypes.Local)

	case q.remoteInit:
		return fn.Ok(lntypes.Remote)
	}

	// unreachable
	return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)
}

// CanSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *Quiescer) CanSendUpdates() bool {
	q.RLock()
	defer q.RUnlock()

	return q.canSendUpdates()
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *Quiescer) canSendUpdates() bool {
	return !q.sent && !q.localInit
}

// CanRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *Quiescer) CanRecvUpdates() bool {
	q.RLock()
	defer q.RUnlock()

	return q.canRecvUpdates()
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *Quiescer) canRecvUpdates() bool {
	return !q.received
}

// CanSendStfu returns true if we can send an Stfu.
func (q *Quiescer) CanSendStfu(numPendingLocalUpdates uint64) bool {
	q.RLock()
	defer q.RUnlock()

	return q.canSendStfu(numPendingLocalUpdates)
}

// canSendStfu returns true if we can send an Stfu.
func (q *Quiescer) canSendStfu(numPendingLocalUpdates uint64) bool {
	return numPendingLocalUpdates == 0 && !q.sent
}

// CanRecvStfu returns true if we can receive an Stfu.
func (q *Quiescer) CanRecvStfu(numPendingRemoteUpdates uint64) bool {
	q.RLock()
	defer q.RUnlock()

	return q.canRecvStfu(numPendingRemoteUpdates)
}

// canRecvStfu returns true if we can receive an Stfu.
func (q *Quiescer) canRecvStfu(numPendingRemoteUpdates uint64) bool {
	return numPendingRemoteUpdates == 0 && !q.received
}

// SendOwedStfu sends Stfu if it owes one. It returns an error if the state
// machine is in an invalid state.
func (q *Quiescer) SendOwedStfu(numPendingLocalUpdates uint64) error {
	q.Lock()
	defer q.Unlock()

	return q.sendOwedStfu(numPendingLocalUpdates)
}

// sendOwedStfu sends Stfu if it owes one. It returns an error if the state
// machine is in an invalid state.
func (q *Quiescer) sendOwedStfu(numPendingLocalUpdates uint64) error {
	if !q.oweStfu() || !q.canSendStfu(numPendingLocalUpdates) {
		return nil
	}

	err := q.makeStfu(numPendingLocalUpdates).Sink(q.cfg.sendMsg)

	if err == nil {
		q.sent = true

		// Since we just sent an Stfu, we may have a newly quiesced
		// state. If so, we will try to resolve any outstanding
		// StfuReqs.
		q.tryResolveStfuReq()
	}

	return err
}

// TryResolveStfuReq attempts to resolve the active quiescence request if the
// state machine has reached a quiescent state.
func (q *Quiescer) TryResolveStfuReq() {
	q.Lock()
	defer q.Unlock()

	q.tryResolveStfuReq()
}

// tryResolveStfuReq attempts to resolve the active quiescence request if the
// state machine has reached a quiescent state.
func (q *Quiescer) tryResolveStfuReq() {
	q.activeQuiescenceReq.WhenSome(
		func(req StfuReq) {
			if q.isQuiescent() {
				req.Resolve(q.quiescenceInitiator())
				q.activeQuiescenceReq = fn.None[StfuReq]()
			}
		},
	)
}

// InitStfu instructs the quiescer that we intend to begin a quiescence
// negotiation where we are the initiator. We don't yet send stfu yet because
// we need to wait for the link to give us a valid opportunity to do so.
func (q *Quiescer) InitStfu(req StfuReq) {
	q.Lock()
	defer q.Unlock()

	q.initStfu(req)
}

// initStfu instructs the quiescer that we intend to begin a quiescence
// negotiation where we are the initiator. We don't yet send stfu yet because
// we need to wait for the link to give us a valid opportunity to do so.
func (q *Quiescer) initStfu(req StfuReq) {
	if q.localInit {
		req.Resolve(fn.Errf[lntypes.ChannelParty](
			"quiescence already requested",
		))

		return
	}

	q.localInit = true
	q.activeQuiescenceReq = fn.Some(req)
}

// OnResume accepts a no return closure that will run when the quiescer is
// resumed.
func (q *Quiescer) OnResume(hook func()) {
	q.Lock()
	defer q.Unlock()

	q.onResume(hook)
}

// onResume accepts a no return closure that will run when the quiescer is
// resumed.
func (q *Quiescer) onResume(hook func()) {
	q.resumeQueue = append(q.resumeQueue, hook)
}

// Resume runs all of the deferred actions that have accumulated while the
// channel has been quiescent and then resets the quiescer state to its initial
// state.
func (q *Quiescer) Resume() {
	q.Lock()
	defer q.Unlock()

	q.resume()
}

// resume runs all of the deferred actions that have accumulated while the
// channel has been quiescent and then resets the quiescer state to its initial
// state.
func (q *Quiescer) resume() {
	for _, hook := range q.resumeQueue {
		hook()
	}
	q.localInit = false
	q.remoteInit = false
	q.sent = false
	q.received = false
	q.resumeQueue = nil
}
