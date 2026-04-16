package peer

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/stretchr/testify/require"
)

// newCapturingLogger builds a btclog.Logger backed by an in-memory buffer
// so tests can assert whether a given log line was emitted.
func newCapturingLogger() (btclog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	handler := btclog.NewDefaultHandler(buf, btclog.WithNoTimestamp())
	return btclog.NewSLogger(handler), buf
}

// newRealIngressLimiter constructs a real ingressLimiter backed by real
// per-peer, global, and freebie limiters sized so the first message
// passes and every subsequent one trips the named side of the
// limiter. It is used by the log tests to exercise the one-shot
// claim path against real FirstDropClaim bookkeeping rather than a
// stub.
func newRealIngressLimiter(t *testing.T) onionmessage.IngressLimiter {
	t.Helper()

	// Burst == one max-sized message for all three sides; rate of
	// 1 Kbps ensures no bucket refills within the test window.
	peerLim := onionmessage.NewPeerRateLimiter(1, testMsgBytes)
	globalLim := onionmessage.NewGlobalLimiter(1, testMsgBytes)
	freebieLim := onionmessage.NewGlobalLimiter(1, testMsgBytes)

	return onionmessage.NewIngressLimiter(peerLim, globalLim, freebieLim)
}

// TestLogFirstOnionDropGlobalOneShot verifies that logFirstOnionDrop
// emits exactly one info-level line for the global limiter's first
// drop and is silent on subsequent drops, so operators get a single
// "engaged" signal without log flooding under sustained attack. The
// global first-drop line must land on the package-level logger, not
// the per-peer one, since a global drop is not attributable to any
// single peer.
func TestLogFirstOnionDropGlobalOneShot(t *testing.T) {
	t.Parallel()

	pkgLog, pkgBuf := newCapturingLogger()
	peerLog, peerBuf := newCapturingLogger()
	limiter := newRealIngressLimiter(t)

	// First drop log: must emit to the package-level logger.
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrGlobalRateLimit, limiter,
	)
	require.Contains(t, pkgBuf.String(), "global rate limiter")
	require.Empty(t, peerBuf.String(),
		"global drop must not land on the peer-prefix log")

	// Second drop log: must be silent (both buffer sizes unchanged).
	sizeAfterFirst := pkgBuf.Len()
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrGlobalRateLimit, limiter,
	)
	require.Equal(t, sizeAfterFirst, pkgBuf.Len(),
		"second drop must not re-log the first-drop line")
	require.Empty(t, peerBuf.String())
}

// TestLogFirstOnionDropPeerOneShot verifies the same one-shot property
// for the per-peer limiter and that the nil-limiter guard prevents a
// panic when onion message rate limiting is entirely disabled. The
// per-peer first-drop line must land on the peer-prefix logger so
// operators can see which peer tripped the limiter.
func TestLogFirstOnionDropPeerOneShot(t *testing.T) {
	t.Parallel()

	pkgLog, pkgBuf := newCapturingLogger()
	peerLog, peerBuf := newCapturingLogger()

	// Nil limiter: must not panic and must not log to either logger.
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrPeerRateLimit, nil,
	)
	require.Empty(t, pkgBuf.String())
	require.Empty(t, peerBuf.String())

	// Real limiter: emit once to the peer logger, then silent.
	limiter := newRealIngressLimiter(t)
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrPeerRateLimit, limiter,
	)
	require.Contains(t, peerBuf.String(), "per-peer rate limiter")
	require.Empty(t, pkgBuf.String(),
		"per-peer drop must not land on the package-level log")
	sizeAfterFirst := peerBuf.Len()
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrPeerRateLimit, limiter,
	)
	require.Equal(t, sizeAfterFirst, peerBuf.Len())
}

// TestLogFirstOnionDropFreebieOneShot verifies the same one-shot
// property for the freebie limiter. The freebie first-drop line must
// land on the peer-prefix logger because a freebie drop is still
// attributable to a specific peer identity (just one without a
// channel), so operators can see which stranger exhausted the slot.
func TestLogFirstOnionDropFreebieOneShot(t *testing.T) {
	t.Parallel()

	pkgLog, pkgBuf := newCapturingLogger()
	peerLog, peerBuf := newCapturingLogger()

	// Nil limiter: must not panic and must not log to either logger.
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrFreebieRateLimit, nil,
	)
	require.Empty(t, pkgBuf.String())
	require.Empty(t, peerBuf.String())

	// Real limiter: emit once to the peer logger, then silent.
	limiter := newRealIngressLimiter(t)
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrFreebieRateLimit, limiter,
	)
	require.Contains(t, peerBuf.String(), "freebie slot exhausted")
	require.Empty(t, pkgBuf.String(),
		"freebie drop must not land on the package-level log")
	sizeAfterFirst := peerBuf.Len()
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrFreebieRateLimit, limiter,
	)
	require.Equal(t, sizeAfterFirst, peerBuf.Len())
}

// TestLogFirstOnionDropUnknownReason verifies that an error that does
// not match any known drop reason is a no-op — neither limiter's
// first-drop flag is consumed. This guards against a typo or a new
// drop reason being added without a matching log case.
func TestLogFirstOnionDropUnknownReason(t *testing.T) {
	t.Parallel()

	pkgLog, pkgBuf := newCapturingLogger()
	peerLog, peerBuf := newCapturingLogger()
	limiter := newRealIngressLimiter(t)

	logFirstOnionDrop(pkgLog, peerLog, ErrNoChannel, limiter)
	require.Empty(t, pkgBuf.String())
	require.Empty(t, peerBuf.String())

	// Both limiters' first-drop flags must still be unclaimed, so a
	// follow-up call with a valid reason still emits the info line.
	logFirstOnionDrop(
		pkgLog, peerLog, onionmessage.ErrGlobalRateLimit, limiter,
	)
	require.Contains(t, pkgBuf.String(), "global rate limiter")
}
