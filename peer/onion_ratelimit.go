package peer

import (
	"errors"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/onionmessage"
)

// ErrNoChannel is the sentinel error returned by allowOnionMessage when
// the incoming peer has no fully open channel with us and neither the
// relay-all nor the freebie-slot admission policy is active. It is the
// primary Sybil-resistance layer on top of the byte-granular rate
// limiters: an attacker that can cheaply spin up new identities cannot
// burn any per-peer or global token budget because the channel gate
// runs before the IngressLimiter is consulted at all.
var ErrNoChannel = errors.New("peer has no open channel")

// allowOnionMessage routes an incoming onion message through one of
// three admission paths and returns the outcome as a fn.Result. The
// paths are, in order of evaluation:
//
//  1. Channel gate passes (hasChannel == true): the peer has at least
//     one fully open channel with us, so the message is admitted to
//     the IngressLimiter's per-peer-then-global byte-granular check
//     via AllowN. A rejection wraps onionmessage.ErrPeerRateLimit or
//     onionmessage.ErrGlobalRateLimit.
//
//  2. Relay-all (relayAll == true): the channel gate is skipped
//     entirely and the message is admitted to the IngressLimiter via
//     AllowN regardless of hasChannel. This is the opt-in policy for
//     operators who want to accept onion messages from peers with no
//     channel, trading the Sybil-resistance property of the gate for
//     broader reachability.
//
//  3. Freebie (freebieEnabled == true, hasChannel == false, relayAll
//     == false): the message is admitted to the IngressLimiter's
//     shared freebie bucket via AllowFreebie, which also debits the
//     global bucket on a freebie pass so the freebie lane stays a
//     sub-cap of the global cap. A rejection wraps
//     onionmessage.ErrFreebieRateLimit or
//     onionmessage.ErrGlobalRateLimit.
//
//  4. Otherwise (no channel, no relay-all, no freebie): the message
//     is dropped at the gate with ErrNoChannel and no rate-limiter
//     state is allocated for the no-channel peer.
//
// Callers distinguish the drop reason via errors.Is against the
// sentinels above so they can pick the right log, metric, or drop
// path. The invariant that relayAll and freebieEnabled are never both
// true is enforced at startup config validation; allowOnionMessage
// gives relayAll priority if both ever reach it.
//
// A nil IngressLimiter is treated as "disabled" and always accepts
// the message once the admission path has been selected. This
// preserves the behavior of test and disabled-onion-messaging
// configurations without forcing callers to construct a real limiter.
func allowOnionMessage(limiter onionmessage.IngressLimiter,
	peerKey [33]byte, msgBytes int,
	hasChannel, relayAll, freebieEnabled bool) fn.Result[fn.Unit] {

	switch {
	case hasChannel, relayAll:
		if limiter == nil {
			return fn.Ok(fn.Unit{})
		}

		return limiter.AllowN(peerKey, msgBytes)

	case freebieEnabled:
		if limiter == nil {
			return fn.Ok(fn.Unit{})
		}

		return limiter.AllowFreebie(peerKey, msgBytes)

	default:
		return fn.Err[fn.Unit](ErrNoChannel)
	}
}

// logFirstOnionDrop emits a one-shot info log the first time the limiter
// identified by err trips. Per-peer and freebie drops go to peerLog
// (caller's peer-prefixed log) so the operator can see which peer first
// tripped the limiter; a freebie drop is still attributable to a
// specific peer identity (just one without a channel). Global drops go
// to pkgLog (typically the package-level peerLog) since they are not
// attributable to any single peer.
func logFirstOnionDrop(pkgLog, peerLog btclog.Logger, err error,
	limiter onionmessage.IngressLimiter) {

	if limiter == nil {
		return
	}

	switch {
	case errors.Is(err, onionmessage.ErrGlobalRateLimit):
		if limiter.FirstGlobalDropClaim() {
			pkgLog.Infof("onion message global rate limiter " +
				"engaged; further drops logged at trace")
		}

	case errors.Is(err, onionmessage.ErrPeerRateLimit):
		if limiter.FirstPeerDropClaim() {
			peerLog.Infof("onion message per-peer rate limiter " +
				"engaged; further drops logged at trace")
		}

	case errors.Is(err, onionmessage.ErrFreebieRateLimit):
		if limiter.FirstFreebieDropClaim() {
			peerLog.Infof("onion message freebie slot " +
				"exhausted; further no-channel peer " +
				"messages will be dropped until the bucket " +
				"refills")
		}
	}
}
