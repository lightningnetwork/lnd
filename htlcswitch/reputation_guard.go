package htlcswitch

import (
	"sync/atomic"

	"github.com/lightningnetwork/lnd/lnwire"
)

// guardedReputationManager wraps a ReputationManager so that a panic in any of
// its hooks can never propagate into the switch's forwarding goroutine. The
// reputation subsystem is log-only and MUST NOT be able to degrade forwarding;
// if a hook panics we log it, permanently disable the subsystem (fail open),
// and continue forwarding unaffected.
//
// The hooks run synchronously on the switch's forwarding goroutine, so this is
// the boundary that keeps a subsystem bug — a nil deref, a bad map access, an
// arithmetic panic — from taking down the node's HTLC forwarding.
type guardedReputationManager struct {
	inner    ReputationManager
	disabled atomic.Bool
}

// NewGuardedReputationManager wraps the given ReputationManager with a panic
// boundary. It returns nil when inner is nil, so the switch's existing nil
// check still short-circuits a disabled subsystem with zero overhead.
func NewGuardedReputationManager(inner ReputationManager) ReputationManager {
	if inner == nil {
		return nil
	}

	return &guardedReputationManager{inner: inner}
}

// OnForward forwards the observation to the wrapped manager behind a panic
// boundary.
func (g *guardedReputationManager) OnForward(incoming, outgoing CircuitKey,
	incomingAmt, outgoingAmt lnwire.MilliSatoshi, incomingCltv uint32,
	accountable bool) {

	if g.disabled.Load() {
		return
	}
	defer g.recoverHook("OnForward")

	g.inner.OnForward(
		incoming, outgoing, incomingAmt, outgoingAmt, incomingCltv,
		accountable,
	)
}

// OnSettle forwards the observation to the wrapped manager behind a panic
// boundary.
func (g *guardedReputationManager) OnSettle(incoming, outgoing CircuitKey) {
	if g.disabled.Load() {
		return
	}
	defer g.recoverHook("OnSettle")

	g.inner.OnSettle(incoming, outgoing)
}

// OnFail forwards the observation to the wrapped manager behind a panic
// boundary.
func (g *guardedReputationManager) OnFail(incoming, outgoing CircuitKey) {
	if g.disabled.Load() {
		return
	}
	defer g.recoverHook("OnFail")

	g.inner.OnFail(incoming, outgoing)
}

// recoverHook recovers from a panic in a reputation hook, logging it and
// permanently disabling the subsystem so a deterministic bug cannot panic on
// every forwarded HTLC. Forwarding is never affected.
func (g *guardedReputationManager) recoverHook(method string) {
	if r := recover(); r != nil {
		log.Errorf("Reputation %s hook panicked; disabling reputation "+
			"subsystem (forwarding is unaffected): %v", method, r)
		g.disabled.Store(true)
	}
}
