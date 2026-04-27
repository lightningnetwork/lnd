package onionmessage

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// msgBytes is the byte count used as the per-Allow token charge across the
// tests. It is intentionally close to the spec-max onion message size so
// that burst budgets in tests closely match real-world worst-case behavior.
const msgBytes = 32 * 1024

// TestGlobalLimiterDisabled verifies that constructing a global limiter with
// a zero rate or zero burst yields a noop limiter that always allows
// traffic regardless of the requested byte count.
func TestGlobalLimiterDisabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		kbps       uint64
		burstBytes uint64
	}{
		{"zero kbps", 0, 1024},
		{"zero burst", 1024, 0},
		{"both zero", 0, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lim := NewGlobalLimiter(tc.kbps, tc.burstBytes)
			for i := 0; i < 1000; i++ {
				require.True(t, lim.AllowN(msgBytes))
			}
			// Disabled limiters must be noopLimiters, not
			// countingLimiters, so the disabled sentinel is
			// observable at the type level.
			_, isNoop := lim.(noopLimiter)
			require.True(t, isNoop)
		})
	}
}

// TestGlobalLimiterBurstExhaustion verifies that the global limiter permits
// exactly the configured burst worth of immediate bytes and rejects
// subsequent calls until the bucket refills.
func TestGlobalLimiterBurstExhaustion(t *testing.T) {
	t.Parallel()

	// Burst just large enough for five max-size messages; a very low rate
	// ensures the bucket does not refill within the test window so the
	// burst boundary is observable.
	const burstMessages = 5
	lim := NewGlobalLimiter(1, burstMessages*msgBytes)

	for i := 0; i < burstMessages; i++ {
		require.True(t, lim.AllowN(msgBytes),
			"burst slot %d should pass", i)
	}
	require.False(t, lim.AllowN(msgBytes), "post-burst call should drop")

	cl, ok := lim.(*countingLimiter)
	require.True(t, ok)
	require.Equal(t, uint64(1), cl.Dropped())
}

// TestPeerRateLimiterDisabled verifies that a per-peer limiter constructed
// with a zero rate or zero burst permits all traffic and never allocates
// per-peer state.
func TestPeerRateLimiterDisabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		kbps       uint64
		burstBytes uint64
	}{
		{"zero kbps", 0, 1024},
		{"zero burst", 1024, 0},
		{"both zero", 0, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPeerRateLimiter(tc.kbps, tc.burstBytes)
			var peer [33]byte
			peer[0] = 0x02

			for i := 0; i < 1000; i++ {
				require.True(t, p.AllowN(peer, msgBytes))
			}
			require.Equal(t, uint64(0), p.Dropped())
			// No state should have been recorded for the peer.
			require.Equal(t, 0, peerMapLen(p))
		})
	}
}

// TestPeerRateLimiterIsolation verifies that exhausting one peer's bucket
// does not affect a different peer's allowance.
func TestPeerRateLimiterIsolation(t *testing.T) {
	t.Parallel()

	const burstMessages = 3
	p := NewPeerRateLimiter(1, burstMessages*msgBytes)

	var peerA, peerB [33]byte
	peerA[0] = 0x02
	peerB[0] = 0x03

	// Drain peer A's bucket.
	for i := 0; i < burstMessages; i++ {
		require.True(t, p.AllowN(peerA, msgBytes))
	}
	require.False(t, p.AllowN(peerA, msgBytes),
		"peer A should be exhausted")

	// Peer B should still have its full burst.
	for i := 0; i < burstMessages; i++ {
		require.True(t, p.AllowN(peerB, msgBytes),
			"peer B slot %d", i)
	}
	require.False(t, p.AllowN(peerB, msgBytes))

	require.Equal(t, uint64(2), p.Dropped())
}

// TestCountingLimiterFirstDropClaimOnce verifies that FirstDropClaim on a
// countingLimiter returns true exactly once and false on every subsequent
// call, across concurrent goroutines, so that the first-drop info log is
// emitted at most once.
func TestCountingLimiterFirstDropClaimOnce(t *testing.T) {
	t.Parallel()

	// A tiny bucket so that repeated AllowN calls quickly produce drops.
	lim, ok := NewGlobalLimiter(1, msgBytes).(*countingLimiter)
	require.True(t, ok)

	// Drain the bucket.
	require.True(t, lim.AllowN(msgBytes))
	require.False(t, lim.AllowN(msgBytes))

	const workers = 32
	var wins atomic.Uint64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lim.FirstDropClaim() {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, uint64(1), wins.Load(),
		"FirstDropClaim must be winnable exactly once")

	// A subsequent serial call must also return false.
	require.False(t, lim.FirstDropClaim())
}

// TestPeerRateLimiterFirstDropClaimOnce verifies the same single-win
// guarantee for the per-peer limiter's FirstDropClaim.
func TestPeerRateLimiterFirstDropClaimOnce(t *testing.T) {
	t.Parallel()

	p := NewPeerRateLimiter(1, msgBytes)

	const workers = 32
	var wins atomic.Uint64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.FirstDropClaim() {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, uint64(1), wins.Load())
	require.False(t, p.FirstDropClaim())
}

// TestFirstGlobalDropClaimNoopLimiter verifies that
// IngressLimiter.FirstGlobalDropClaim returns false when the composed
// global limiter is a noop (disabled) limiter: a disabled limiter never
// produces drops and must not claim the first-drop flag.
func TestFirstGlobalDropClaimNoopLimiter(t *testing.T) {
	t.Parallel()

	ingress := NewIngressLimiter(nil, NewGlobalLimiter(0, 0))
	require.False(t, ingress.FirstGlobalDropClaim())
}

// TestPeerRateLimiterConcurrentAllowN exercises concurrent AllowN calls
// across many distinct peers to give the race detector an opportunity to
// observe any missing synchronization around the per-peer registry's
// Load / LoadOrStore path. With the registry now retained for the
// process lifetime, the final entry count should equal the number of
// distinct peers exactly.
func TestPeerRateLimiterConcurrentAllowN(t *testing.T) {
	t.Parallel()

	p := NewPeerRateLimiter(100_000, 8*msgBytes)

	const workers = 8
	const iters = 200

	var wg sync.WaitGroup
	var ops atomic.Uint64
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			var key [33]byte
			key[0] = byte(w)
			for i := 0; i < iters; i++ {
				p.AllowN(key, msgBytes)
				ops.Add(1)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, uint64(workers*iters), ops.Load())
	// Each worker uses a distinct peer key and entries are never
	// removed, so the registry must contain exactly one entry per
	// worker.
	require.Equal(t, workers, peerMapLen(p))
}

// peerMapLen returns the number of entries in the per-peer registry. It
// exists solely for tests; production code has no need for the registry
// size since each per-peer bucket's own AllowN call already tracks the
// accounting it cares about.
func peerMapLen(p *PeerRateLimiter) int {
	return p.peers.Len()
}
