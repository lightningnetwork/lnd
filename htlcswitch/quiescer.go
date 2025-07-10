package htlcswitch

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
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

	// ErrQuiescenceTimeout indicates that the quiescer has been quiesced
	// beyond the allotted time.
	ErrQuiescenceTimeout = fmt.Errorf("quiescence timeout")
)

type StfuReq = fn.Req[fn.Unit, fn.Result[lntypes.ChannelParty]]

// Quiescer is the public interface of the quiescence mechanism. Callers of the
// quiescence API should not need any methods besides the ones detailed here.
type Quiescer interface {
	// IsQuiescent returns true if the state machine has been driven all the
	// way to completion. If this returns true, processes that depend on
	// channel quiescence may proceed.
	IsQuiescent() bool

	// QuiescenceInitiator determines which ChannelParty is the initiator of
	// quiescence for the purposes of downstream protocols. If the channel
	// is not currently quiescent, this method will return
	// ErrNoDownstreamLeader.
	QuiescenceInitiator() fn.Result[lntypes.ChannelParty]

	// InitStfu instructs the quiescer that we intend to begin a quiescence
	// negotiation where we are the initiator. We don't yet send stfu yet
	// because we need to wait for the link to give us a valid opportunity
	// to do so.
	InitStfu(req StfuReq)

	// RecvStfu is called when we receive an Stfu message from the remote.
	RecvStfu(stfu lnwire.Stfu) error

	// CanRecvUpdates returns true if we haven't yet received an Stfu which
	// would mark the end of the remote's ability to send updates.
	CanRecvUpdates() bool

	// CanSendUpdates returns true if we haven't yet sent an Stfu which
	// would mark the end of our ability to send updates.
	CanSendUpdates() bool

	// SendOwedStfu sends Stfu if it owes one. It returns an error if the
	// state machine is in an invalid state.
	SendOwedStfu() error

	// OnResume accepts a no return closure that will run when the quiescer
	// is resumed.
	OnResume(hook func())

	// Resume runs all of the deferred actions that have accumulated while
	// the channel has been quiescent and then resets the quiescer state to
	// its initial state.
	Resume()
}

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

	// timeoutDuration is the Duration that we will wait from the moment the
	// channel is considered quiescent before we call the onTimeout function
	timeoutDuration time.Duration

	// onTimeout is a function that will be called in the event that the
	// Quiescer has not been resumed before the timeout is reached. If
	// Quiescer.Resume is called before the timeout has been raeached, then
	// onTimeout will not be called until the quiescer reaches a quiescent
	// state again.
	onTimeout func()
}

// QuiescerLive is a state machine that tracks progression through the
// quiescence protocol.
type QuiescerLive struct {
	cfg QuiescerCfg

	// log is a quiescer-scoped logging instance.
	log btclog.Logger

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

	// timeoutTimer is a field that is used to hold onto the timeout job
	// when we reach quiescence.
	timeoutTimer *time.Timer

	sync.RWMutex
}

// NewQuiescer creates a new quiescer for the given channel.
func NewQuiescer(cfg QuiescerCfg) Quiescer {
	logPrefix := fmt.Sprintf("Quiescer(%v):", cfg.chanID)

	return &QuiescerLive{
		cfg: cfg,
		log: log.WithPrefix(logPrefix),
	}
}

// RecvStfu is called when we receive an Stfu message from the remote.
func (q *QuiescerLive) RecvStfu(msg lnwire.Stfu) error {
	q.Lock()
	defer q.Unlock()

	return q.recvStfu(msg)
}

// recvStfu is called when we receive an Stfu message from the remote.
func (q *QuiescerLive) recvStfu(msg lnwire.Stfu) error {
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

	if q.isQuiescent() {
		q.startTimeout()
	}

	return nil
}

// MakeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *QuiescerLive) MakeStfu() fn.Result[lnwire.Stfu] {
	q.RLock()
	defer q.RUnlock()

	return q.makeStfu()
}

// makeStfu is called when we are ready to send an Stfu message. It returns the
// Stfu message to be sent.
func (q *QuiescerLive) makeStfu() fn.Result[lnwire.Stfu] {
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

// OweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *QuiescerLive) OweStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.oweStfu()
}

// oweStfu returns true if we owe the other party an Stfu. We owe the remote an
// Stfu when we have received but not yet sent an Stfu, or we are the initiator
// but have not yet sent an Stfu.
func (q *QuiescerLive) oweStfu() bool {
	return (q.received || q.localInit) && !q.sent
}

// NeedStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *QuiescerLive) NeedStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.needStfu()
}

// needStfu returns true if the remote owes us an Stfu. They owe us an Stfu when
// we have sent but not yet received an Stfu.
func (q *QuiescerLive) needStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.sent && !q.received
}

// IsQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *QuiescerLive) IsQuiescent() bool {
	q.RLock()
	defer q.RUnlock()

	return q.isQuiescent()
}

// isQuiescent returns true if the state machine has been driven all the way to
// completion. If this returns true, processes that depend on channel quiescence
// may proceed.
func (q *QuiescerLive) isQuiescent() bool {
	return q.sent && q.received
}

// QuiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoQuiescenceInitiator.
func (q *QuiescerLive) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	q.RLock()
	defer q.RUnlock()

	return q.quiescenceInitiator()
}

// quiescenceInitiator determines which ChannelParty is the initiator of
// quiescence for the purposes of downstream protocols. If the channel is not
// currently quiescent, this method will return ErrNoQuiescenceInitiator.
func (q *QuiescerLive) quiescenceInitiator() fn.Result[lntypes.ChannelParty] {
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
func (q *QuiescerLive) CanSendUpdates() bool {
	q.RLock()
	defer q.RUnlock()

	return q.canSendUpdates()
}

// canSendUpdates returns true if we haven't yet sent an Stfu which would mark
// the end of our ability to send updates.
func (q *QuiescerLive) canSendUpdates() bool {
	return !q.sent && !q.localInit
}

// CanRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *QuiescerLive) CanRecvUpdates() bool {
	q.RLock()
	defer q.RUnlock()

	return q.canRecvUpdates()
}

// canRecvUpdates returns true if we haven't yet received an Stfu which would
// mark the end of the remote's ability to send updates.
func (q *QuiescerLive) canRecvUpdates() bool {
	return !q.received
}

// CanSendStfu returns true if we can send an Stfu.
func (q *QuiescerLive) CanSendStfu(numPendingLocalUpdates uint64) bool {
	q.RLock()
	defer q.RUnlock()

	return q.canSendStfu()
}

// canSendStfu returns true if we can send an Stfu.
func (q *QuiescerLive) canSendStfu() bool {
	return !q.sent
}

// CanRecvStfu returns true if we can receive an Stfu.
func (q *QuiescerLive) CanRecvStfu() bool {
	q.RLock()
	defer q.RUnlock()

	return q.canRecvStfu()
}

// canRecvStfu returns true if we can receive an Stfu.
func (q *QuiescerLive) canRecvStfu() bool {
	return !q.received
}

// SendOwedStfu sends Stfu if it owes one. It returns an error if the state
// machine is in an invalid state.
func (q *QuiescerLive) SendOwedStfu() error {
	q.Lock()
	defer q.Unlock()

	return q.sendOwedStfu()
}

// sendOwedStfu sends Stfu if it owes one. It returns an error if the state
// machine is in an invalid state.
func (q *QuiescerLive) sendOwedStfu() error {
	if !q.oweStfu() || !q.canSendStfu() {
		return nil
	}

	err := q.makeStfu().Sink(q.cfg.sendMsg)

	if err == nil {
		q.sent = true

		// Since we just sent an Stfu, we may have a newly quiesced
		// state. If so, we will try to resolve any outstanding
		// StfuReqs.
		q.tryResolveStfuReq()

		if q.isQuiescent() {
			q.startTimeout()
		}
	}

	return err
}

// TryResolveStfuReq attempts to resolve the active quiescence request if the
// state machine has reached a quiescent state.
func (q *QuiescerLive) TryResolveStfuReq() {
	q.Lock()
	defer q.Unlock()

	q.tryResolveStfuReq()
}

// tryResolveStfuReq attempts to resolve the active quiescence request if the
// state machine has reached a quiescent state.
func (q *QuiescerLive) tryResolveStfuReq() {
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
func (q *QuiescerLive) InitStfu(req StfuReq) {
	q.Lock()
	defer q.Unlock()

	q.initStfu(req)
}

// initStfu instructs the quiescer that we intend to begin a quiescence
// negotiation where we are the initiator. We don't yet send stfu yet because
// we need to wait for the link to give us a valid opportunity to do so.
func (q *QuiescerLive) initStfu(req StfuReq) {
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
func (q *QuiescerLive) OnResume(hook func()) {
	q.Lock()
	defer q.Unlock()

	q.onResume(hook)
}

// onResume accepts a no return closure that will run when the quiescer is
// resumed.
func (q *QuiescerLive) onResume(hook func()) {
	q.resumeQueue = append(q.resumeQueue, hook)
}

// Resume runs all of the deferred actions that have accumulated while the
// channel has been quiescent and then resets the quiescer state to its initial
// state.
func (q *QuiescerLive) Resume() {
	q.Lock()
	defer q.Unlock()

	q.resume()
}

// resume runs all of the deferred actions that have accumulated while the
// channel has been quiescent and then resets the quiescer state to its initial
// state.
func (q *QuiescerLive) resume() {
	q.log.Debug("quiescence terminated, resuming htlc traffic")

	// since we are resuming we want to cancel the quiescence timeout
	// action.
	q.cancelTimeout()

	for _, hook := range q.resumeQueue {
		hook()
	}
	q.localInit = false
	q.remoteInit = false
	q.sent = false
	q.received = false
	q.resumeQueue = nil
}

// startTimeout starts the timeout function that fires if the quiescer remains
// in a quiesced state for too long. If this function is called multiple times
// only the last one will have an effect.
func (q *QuiescerLive) startTimeout() {
	if q.cfg.onTimeout == nil {
		return
	}

	old := q.timeoutTimer

	q.timeoutTimer = time.AfterFunc(q.cfg.timeoutDuration, q.cfg.onTimeout)

	if old != nil {
		old.Stop()
	}
}

// cancelTimeout cancels the timeout function that would otherwise fire if the
// quiescer remains in a quiesced state too long. If this function is called
// before startTimeout or after another call to cancelTimeout, the effect will
// be a noop.
func (q *QuiescerLive) cancelTimeout() {
	if q.timeoutTimer != nil {
		q.timeoutTimer.Stop()
		q.timeoutTimer = nil
	}
}

type quiescerNoop struct{}

var _ Quiescer = (*quiescerNoop)(nil)

func (q *quiescerNoop) InitStfu(req StfuReq) {
	req.Resolve(fn.Errf[lntypes.ChannelParty]("quiescence not supported"))
}
func (q *quiescerNoop) RecvStfu(_ lnwire.Stfu) error { return nil }
func (q *quiescerNoop) CanRecvUpdates() bool         { return true }
func (q *quiescerNoop) CanSendUpdates() bool         { return true }
func (q *quiescerNoop) SendOwedStfu() error          { return nil }
func (q *quiescerNoop) IsQuiescent() bool            { return false }
func (q *quiescerNoop) OnResume(hook func())         { hook() }
func (q *quiescerNoop) Resume()                      {}
func (q *quiescerNoop) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	return fn.Err[lntypes.ChannelParty](ErrNoQuiescenceInitiator)
}
