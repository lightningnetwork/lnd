package htlcswitch

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// guardedReputationManager wraps a ReputationManager so that a panic in any of
// its hooks can never propagate into the switch's forwarding goroutine. The
// reputation subsystem is log-only and MUST NOT be able to degrade forwarding;
// if a hook panics we log it and carry on forwarding.
//
// The hooks run synchronously on the switch's forwarding goroutine, so this
// boundary keeps a subsystem bug, such as a nil deref or an arithmetic panic,
// from taking down the node's HTLC forwarding.
type guardedReputationManager struct {
	inner ReputationManager
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
func (g *guardedReputationManager) OnForward(incoming CircuitKey,
	outgoing lnwire.ShortChannelID, incomingAmt, outgoingAmt,
	advertisedFee lnwire.MilliSatoshi, incomingCltv, height uint32,
	accountable bool) {

	defer g.recoverHook("OnForward")

	g.inner.OnForward(
		incoming, outgoing, incomingAmt, outgoingAmt, advertisedFee,
		incomingCltv, height, accountable,
	)
}

// OnSettle forwards the observation to the wrapped manager behind a panic
// boundary.
func (g *guardedReputationManager) OnSettle(incoming CircuitKey,
	outgoing lnwire.ShortChannelID) {

	defer g.recoverHook("OnSettle")

	g.inner.OnSettle(incoming, outgoing)
}

// OnFail forwards the observation to the wrapped manager behind a panic
// boundary.
func (g *guardedReputationManager) OnFail(incoming CircuitKey,
	outgoing lnwire.ShortChannelID) {

	defer g.recoverHook("OnFail")

	g.inner.OnFail(incoming, outgoing)
}

// recoverHook recovers from a panic in a reputation hook and logs it, so that a
// bug in the log-only subsystem cannot affect forwarding.
func (g *guardedReputationManager) recoverHook(method string) {
	if r := recover(); r != nil {
		log.Errorf("Reputation %s hook panicked (forwarding is "+
			"unaffected): %v", method, r)
	}
}
