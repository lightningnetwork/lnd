package peer

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/stretchr/testify/require"
)

// testMsgBytes is the on-the-wire size we charge the bucket per call in
// these tests. It is sized to approximate a spec-max onion message so
// that burst budgets scale naturally with the per-message cost.
const testMsgBytes = 32 * 1024

// stubIngressLimiter is a test double for onionmessage.IngressLimiter
// that records every call and delegates the accept/reject decision to
// caller-supplied predicates. It is used to exercise
// allowOnionMessage's composition logic (channel gate → limiter)
// without standing up a real token bucket.
type stubIngressLimiter struct {
	// decide is invoked for every AllowN call. It receives the peer
	// key and byte count and returns the error to embed in the
	// fn.Result — nil for accept.
	decide func(peer [33]byte, n int) error

	// freebieDecide is invoked for every AllowFreebie call. It has
	// the same shape as decide so tests can independently drive the
	// channel-peer and no-channel-peer admission paths against a
	// single stub.
	freebieDecide func(peer [33]byte, n int) error

	// calls counts every AllowN invocation and is load-bearing for
	// tests that assert the channel gate short-circuits before the
	// per-peer bucket is consulted.
	calls atomic.Uint64

	// freebieCalls counts every AllowFreebie invocation separately
	// so tests can assert which admission path was taken.
	freebieCalls atomic.Uint64
}

// AllowN records the call and dispatches to the configured predicate.
func (s *stubIngressLimiter) AllowN(peer [33]byte,
	n int) fn.Result[fn.Unit] {

	s.calls.Add(1)
	if err := s.decide(peer, n); err != nil {
		return fn.Err[fn.Unit](err)
	}

	return fn.Ok(fn.Unit{})
}

// FirstPeerDropClaim always returns true so the log-path test can
// observe the one-shot dispatch. Tests that care about the one-shot
// invariant use a real IngressLimiter instead.
func (s *stubIngressLimiter) FirstPeerDropClaim() bool { return true }

// FirstGlobalDropClaim always returns true for the same reason.
func (s *stubIngressLimiter) FirstGlobalDropClaim() bool { return true }

// AllowFreebie records the call and dispatches to the configured
// freebie predicate. A stub constructed via acceptAll has a
// freebieDecide that accepts; tests exercising freebie rejection
// install their own predicate.
func (s *stubIngressLimiter) AllowFreebie(peer [33]byte,
	n int) fn.Result[fn.Unit] {

	s.freebieCalls.Add(1)
	if err := s.freebieDecide(peer, n); err != nil {
		return fn.Err[fn.Unit](err)
	}

	return fn.Ok(fn.Unit{})
}

// FirstFreebieDropClaim always returns true so the log-path test can
// observe the one-shot dispatch. Tests that care about the one-shot
// invariant use a real IngressLimiter instead.
func (s *stubIngressLimiter) FirstFreebieDropClaim() bool { return true }

// acceptAll constructs a stubIngressLimiter whose AllowN and
// AllowFreebie both always accept.
func acceptAll() *stubIngressLimiter {
	return &stubIngressLimiter{
		decide:        func(_ [33]byte, _ int) error { return nil },
		freebieDecide: func(_ [33]byte, _ int) error { return nil },
	}
}

// TestAllowOnionMessageNilLimiter verifies that allowOnionMessage treats
// a nil IngressLimiter as "disabled" and unconditionally accepts
// messages, as long as the admission path has been selected.
func TestAllowOnionMessageNilLimiter(t *testing.T) {
	t.Parallel()

	var peer [33]byte
	result := allowOnionMessage(
		nil, peer, testMsgBytes, true, false, false,
	)
	require.NoError(t, result.Err())
}

// TestAllowOnionMessageNoChannel verifies that messages from a peer
// that does not have a fully open channel with us are dropped
// unconditionally with ErrNoChannel, even when a real IngressLimiter
// is configured. The stub records whether AllowN was consulted; it
// must remain at zero to prove the channel gate runs before the
// IngressLimiter.
func TestAllowOnionMessageNoChannel(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()

	var key [33]byte
	key[0] = 0x07

	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, false,
	)
	require.Error(t, result.Err())
	require.True(t, errors.Is(result.Err(), ErrNoChannel))
	require.Equal(t, uint64(0), limiter.calls.Load(),
		"no-channel drop must not consult the IngressLimiter")
	require.Equal(t, uint64(0), limiter.freebieCalls.Load(),
		"no-channel drop must not consult the freebie bucket")

	// Once the channel gate flips, the same key is accepted and the
	// limiter is now consulted exactly once.
	result = allowOnionMessage(
		limiter, key, testMsgBytes, true, false, false,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(1), limiter.calls.Load())
	require.Equal(t, uint64(0), limiter.freebieCalls.Load())
}

// TestAllowOnionMessageRelayAll verifies that enabling relayAll skips
// the channel-presence gate: a peer with no fully open channel is
// admitted into the IngressLimiter instead of being rejected at the
// gate. Exercising the same (key, hasChannel=false) input with
// relayAll flipped on and off proves the flag is the only thing that
// decides the gate outcome.
func TestAllowOnionMessageRelayAll(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()

	var key [33]byte
	key[0] = 0x08

	// Gate enforced: no-channel peer is rejected without consulting
	// the IngressLimiter.
	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, false,
	)
	require.Error(t, result.Err())
	require.True(t, errors.Is(result.Err(), ErrNoChannel))
	require.Equal(t, uint64(0), limiter.calls.Load())

	// Gate skipped: the same no-channel peer is now admitted and the
	// IngressLimiter is consulted.
	result = allowOnionMessage(
		limiter, key, testMsgBytes, false, true, false,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(1), limiter.calls.Load())

	// relayAll with a peer that also has a channel: the gate is
	// trivially satisfied and the limiter is consulted again.
	result = allowOnionMessage(
		limiter, key, testMsgBytes, true, true, false,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(2), limiter.calls.Load())

	// Nil limiter with relayAll is still accepted: disabled limiter +
	// skipped gate = unconditional accept.
	result = allowOnionMessage(
		nil, key, testMsgBytes, false, true, false,
	)
	require.NoError(t, result.Err())
}

// TestAllowOnionMessagePeerRejectsFirst verifies that a real
// IngressLimiter consults the per-peer limiter before the global
// limiter: once the per-peer bucket is drained, the global bucket
// must not be touched on subsequent calls, preserving the shared
// budget against a hostile peer burning global tokens via rejected
// attempts.
func TestAllowOnionMessagePeerRejectsFirst(t *testing.T) {
	t.Parallel()

	// Real per-peer limiter with burst of exactly one message; very
	// low rate so it does not refill during the test.
	peerLim := onionmessage.NewPeerRateLimiter(1, testMsgBytes)

	// Stub "global" that records whether it was consulted. It wraps
	// the global side of the IngressLimiter.
	globalCalls := atomic.Uint64{}
	global := &countingGlobalStub{
		allow: func() bool { return true },
		calls: &globalCalls,
	}

	limiter := onionmessage.NewIngressLimiter(peerLim, global, nil)

	var key [33]byte
	key[0] = 0x03

	// First call drains the per-peer bucket; both limiters are
	// consulted so global.calls bumps to 1.
	result := allowOnionMessage(
		limiter, key, testMsgBytes, true, false, false,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(1), globalCalls.Load())

	// Second call trips the per-peer limiter and must NOT consult
	// the global limiter — globalCalls stays at 1.
	result = allowOnionMessage(
		limiter, key, testMsgBytes, true, false, false,
	)
	require.Error(t, result.Err())
	require.True(t,
		errors.Is(result.Err(), onionmessage.ErrPeerRateLimit),
	)
	require.Equal(t, uint64(1), peerLim.Dropped())
	require.Equal(t, uint64(1), globalCalls.Load(),
		"global limiter must not be consulted when per-peer rejects")
}

// countingGlobalStub is a minimal RateLimiter test double that counts
// calls to AllowN and delegates the accept/reject decision to a
// caller-supplied predicate. It exists so tests can feed a real
// ingressLimiter a controllable global side.
type countingGlobalStub struct {
	allow func() bool
	calls *atomic.Uint64
}

func (s *countingGlobalStub) AllowN(_ int) bool {
	s.calls.Add(1)

	return s.allow()
}

// TestAllowOnionMessageGlobalRejects verifies that when the per-peer
// limiter permits traffic but the global bucket is exhausted,
// allowOnionMessage surfaces ErrGlobalRateLimit.
func TestAllowOnionMessageGlobalRejects(t *testing.T) {
	t.Parallel()

	peerLim := onionmessage.NewPeerRateLimiter(
		1_000_000, 100*testMsgBytes,
	)

	globalCalls := atomic.Uint64{}
	global := &countingGlobalStub{
		allow: func() bool { return false },
		calls: &globalCalls,
	}
	limiter := onionmessage.NewIngressLimiter(peerLim, global, nil)

	var key [33]byte
	key[0] = 0x02

	result := allowOnionMessage(
		limiter, key, testMsgBytes, true, false, false,
	)
	require.Error(t, result.Err())
	require.True(t,
		errors.Is(result.Err(), onionmessage.ErrGlobalRateLimit),
	)
	require.Equal(t, uint64(0), peerLim.Dropped())
	require.Equal(t, uint64(1), globalCalls.Load())
}

// TestAllowOnionMessageHappyPath verifies that a fully-configured
// IngressLimiter accepts a stream of messages when neither bucket is
// under pressure.
func TestAllowOnionMessageHappyPath(t *testing.T) {
	t.Parallel()

	peerLim := onionmessage.NewPeerRateLimiter(
		1_000_000, 100*testMsgBytes,
	)
	globalCalls := atomic.Uint64{}
	global := &countingGlobalStub{
		allow: func() bool { return true },
		calls: &globalCalls,
	}
	limiter := onionmessage.NewIngressLimiter(peerLim, global, nil)

	var key [33]byte
	key[0] = 0x04

	for i := 0; i < 10; i++ {
		result := allowOnionMessage(
			limiter, key, testMsgBytes, true, false, false,
		)
		require.NoError(t, result.Err(), "iter %d", i)
	}
	require.Equal(t, uint64(0), peerLim.Dropped())
}

// TestAllowOnionMessagePeerIsolation verifies at the peer-package level
// that exhausting one peer's bucket through allowOnionMessage does not
// affect a different peer's allowance — guarding against a regression
// where the helper might key the bucket incorrectly.
func TestAllowOnionMessagePeerIsolation(t *testing.T) {
	t.Parallel()

	peerLim := onionmessage.NewPeerRateLimiter(1, 2*testMsgBytes)
	globalCalls := atomic.Uint64{}
	global := &countingGlobalStub{
		allow: func() bool { return true },
		calls: &globalCalls,
	}
	limiter := onionmessage.NewIngressLimiter(peerLim, global, nil)

	var keyA, keyB [33]byte
	keyA[0] = 0x02
	keyB[0] = 0x03

	// Drain peer A.
	for i := 0; i < 2; i++ {
		result := allowOnionMessage(
			limiter, keyA, testMsgBytes, true, false, false,
		)
		require.NoError(t, result.Err())
	}
	result := allowOnionMessage(
		limiter, keyA, testMsgBytes, true, false, false,
	)
	require.Error(t, result.Err())

	// Peer B must still have its full burst available.
	for i := 0; i < 2; i++ {
		result := allowOnionMessage(
			limiter, keyB, testMsgBytes, true, false, false,
		)
		require.NoError(t, result.Err(), "peer B slot %d", i)
	}
}

// TestAllowOnionMessageConcurrent exercises concurrent access to
// allowOnionMessage across many goroutines. It asserts that the sum of
// accepted calls plus the per-peer dropped counter equals the total
// number of attempts, and that no race or panic occurs. Run with -race
// for the strongest signal.
func TestAllowOnionMessageConcurrent(t *testing.T) {
	t.Parallel()

	const burstMessages = 32
	peerLim := onionmessage.NewPeerRateLimiter(
		1, burstMessages*testMsgBytes,
	)
	globalCalls := atomic.Uint64{}
	global := &countingGlobalStub{
		allow: func() bool { return true },
		calls: &globalCalls,
	}
	limiter := onionmessage.NewIngressLimiter(peerLim, global, nil)

	var key [33]byte
	key[0] = 0x05

	const workers = 16
	const perWorker = 64
	var wg sync.WaitGroup
	var accepted atomic.Uint64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				result := allowOnionMessage(
					limiter, key, testMsgBytes,
					true, false, false,
				)
				if result.Err() == nil {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	total := uint64(workers * perWorker)
	require.Equal(
		t, total, accepted.Load()+peerLim.Dropped(),
		"every attempt must be counted as accepted or dropped",
	)
	// With a near-zero refill rate the bucket can only issue at most
	// burstMessages accepts before refill; since the test runs much
	// faster than the refill interval, accepted should equal the
	// burst.
	require.Equal(t, uint64(burstMessages), accepted.Load())
}

// TestAllowOnionMessageFreebieAccepts verifies that when the channel
// gate is closed, relay-all is off, and freebie is enabled, the
// message is routed through AllowFreebie (not AllowN) and on a
// freebie-accept the caller receives Ok.
func TestAllowOnionMessageFreebieAccepts(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()

	var key [33]byte
	key[0] = 0x09

	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, true,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(1), limiter.freebieCalls.Load(),
		"freebie path must consult AllowFreebie exactly once")
	require.Equal(t, uint64(0), limiter.calls.Load(),
		"freebie path must not consult AllowN")
}

// TestAllowOnionMessageFreebieRejects verifies that when the freebie
// bucket rejects, allowOnionMessage surfaces ErrFreebieRateLimit
// unchanged and never falls through to the channel-peer AllowN path.
func TestAllowOnionMessageFreebieRejects(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()
	limiter.freebieDecide = func(_ [33]byte, _ int) error {
		return onionmessage.ErrFreebieRateLimit
	}

	var key [33]byte
	key[0] = 0x0a

	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, true,
	)
	require.Error(t, result.Err())
	require.True(t,
		errors.Is(result.Err(), onionmessage.ErrFreebieRateLimit),
	)
	require.Equal(t, uint64(1), limiter.freebieCalls.Load())
	require.Equal(t, uint64(0), limiter.calls.Load())
}

// TestAllowOnionMessageFreebieGlobalRejects verifies that when the
// freebie bucket passes but the subsequently-debited global bucket is
// drained, AllowFreebie surfaces ErrGlobalRateLimit and that error
// propagates through allowOnionMessage unchanged.
func TestAllowOnionMessageFreebieGlobalRejects(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()
	limiter.freebieDecide = func(_ [33]byte, _ int) error {
		return onionmessage.ErrGlobalRateLimit
	}

	var key [33]byte
	key[0] = 0x0b

	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, true,
	)
	require.Error(t, result.Err())
	require.True(t,
		errors.Is(result.Err(), onionmessage.ErrGlobalRateLimit),
	)
	require.Equal(t, uint64(1), limiter.freebieCalls.Load())
	require.Equal(t, uint64(0), limiter.calls.Load())
}

// TestAllowOnionMessageFreebieChannelPeer verifies that a peer with an
// open channel is routed through AllowN regardless of whether freebie
// is enabled: the channel gate takes priority over the freebie path.
func TestAllowOnionMessageFreebieChannelPeer(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()

	var key [33]byte
	key[0] = 0x0c

	result := allowOnionMessage(
		limiter, key, testMsgBytes, true, false, true,
	)
	require.NoError(t, result.Err())
	require.Equal(t, uint64(1), limiter.calls.Load(),
		"channel peer must use AllowN even when freebie is enabled")
	require.Equal(t, uint64(0), limiter.freebieCalls.Load(),
		"channel peer must not consult the freebie bucket")
}

// TestAllowOnionMessageFreebieDisabled verifies that when neither
// relay-all nor freebie is enabled, a no-channel peer is dropped with
// ErrNoChannel without consulting any rate-limiter state.
func TestAllowOnionMessageFreebieDisabled(t *testing.T) {
	t.Parallel()

	limiter := acceptAll()

	var key [33]byte
	key[0] = 0x0d

	result := allowOnionMessage(
		limiter, key, testMsgBytes, false, false, false,
	)
	require.Error(t, result.Err())
	require.True(t, errors.Is(result.Err(), ErrNoChannel))
	require.Equal(t, uint64(0), limiter.calls.Load())
	require.Equal(t, uint64(0), limiter.freebieCalls.Load())
}

// TestAllowOnionMessageFreebieNilLimiter verifies that a nil
// IngressLimiter short-circuits the freebie path the same way it
// short-circuits the channel-peer path: a no-channel peer with
// freebie enabled is accepted because "no limiter" means "accept
// everything once the admission path is selected".
func TestAllowOnionMessageFreebieNilLimiter(t *testing.T) {
	t.Parallel()

	var key [33]byte
	key[0] = 0x0e

	result := allowOnionMessage(
		nil, key, testMsgBytes, false, false, true,
	)
	require.NoError(t, result.Err())
}
