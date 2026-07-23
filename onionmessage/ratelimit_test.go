package onionmessage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
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

// TestPeerRateLimiterEvictsOnlyFullBuckets verifies the core invariant of
// the sweep: a bucket that has refilled to its full burst is evicted, while
// a bucket that still owes tokens (is currently draining) is retained. The
// latter is what preserves the anti-reset property across disconnects.
func TestPeerRateLimiterEvictsOnlyFullBuckets(t *testing.T) {
	t.Parallel()

	p := NewPeerRateLimiter(1, msgBytes)

	var fullPeer, drainedPeer [33]byte
	fullPeer[0] = 0x01
	drainedPeer[0] = 0x02

	// A freshly constructed limiter starts at full burst.
	p.peers.Store(fullPeer, rate.NewLimiter(p.rate, p.burst))

	// Drain the second bucket completely so it owes its entire burst.
	// With a per-peer rate of 1 Kbps it will not refill within the test.
	drained := rate.NewLimiter(p.rate, p.burst)
	require.True(t, drained.AllowN(time.Now(), p.burst))
	p.peers.Store(drainedPeer, drained)
	p.numPeers.Store(2)

	p.evictFullBuckets()

	// The full bucket is gone; the draining bucket is kept.
	_, ok := p.peers.Load(fullPeer)
	require.False(t, ok, "full bucket should be evicted")
	_, ok = p.peers.Load(drainedPeer)
	require.True(t, ok, "draining bucket must be retained")

	require.Equal(t, 1, peerMapLen(p))
	require.Equal(t, int64(1), p.numPeers.Load())
}

// TestPeerRateLimiterSweepAccountingUnderRace runs evictFullBuckets
// concurrently with AllowN on a shared key set and asserts that, once quiesced,
// numPeers still matches the registry size. It guards against a sweep leaving
// the counter drifted above the map, which would eventually pin numPeers over
// maxPeers and force an O(N) sweep on every new peer.
func TestPeerRateLimiterSweepAccountingUnderRace(t *testing.T) {
	t.Parallel()

	// A single-message burst with a high rate snaps each bucket between
	// full and empty: buckets read full most of the time (the sweep's
	// check passes) but a debit landing mid-sweep drops one clearly below
	// full. That is the interleaving that can desync numPeers.
	p := NewPeerRateLimiter(1_000_000, msgBytes)

	// Fewer keys than workers concentrates goroutines on the same key, and
	// a low high-water mark keeps AllowN triggering sweeps that evict and
	// recreate buckets.
	const numKeys = 8
	p.maxPeers = 4

	keys := make([][33]byte, numKeys)
	for i := range keys {
		keys[i][0] = byte(i)
	}

	const (
		workers = 16
		rounds  = 4000
		sweeps  = 3
	)

	var wg sync.WaitGroup

	// Worker goroutines hammer AllowN across the shared key set.
	for w := range workers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for r := range rounds {
				p.AllowN(keys[(seed+r)%numKeys], msgBytes)
			}
		}(w)
	}

	// Dedicated sweeper goroutines race the workers.
	for range sweeps {
		wg.Go(func() {
			for range rounds {
				p.evictFullBuckets()
			}
		})
	}

	wg.Wait()

	// At quiescence every map entry must be counted exactly once.
	require.Equal(t, int64(peerMapLen(p)), p.numPeers.Load(),
		"numPeers drifted from the registry size under concurrent "+
			"sweeps")
}

// TestPeerRateLimiterBoundedUnderIdentityChurn exercises the sweep under a
// large number of distinct pubkeys that each send a single small onion
// message, as can happen when the channel gate is disabled via
// --onion-msg-relay-all. Without eviction the registry would grow one entry
// per identity; with it, the map stays bounded because each near-full bucket
// refills and is reclaimed on a subsequent sweep.
func TestPeerRateLimiterBoundedUnderIdentityChurn(t *testing.T) {
	t.Parallel()

	// A high per-peer rate means the tiny per-message debit refills
	// essentially instantly, so buckets from earlier identities read as
	// full by the time a later insertion triggers a sweep.
	p := NewPeerRateLimiter(1_000_000, msgBytes)

	// Lower the high-water mark so the test drives the sweep without
	// allocating tens of thousands of entries.
	p.maxPeers = 64

	const identities = 100_000
	for i := 0; i < identities; i++ {
		var key [33]byte
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		key[2] = byte(i >> 16)

		// One minimal onion message from a fresh identity.
		require.True(t, p.AllowN(key, 1))
	}

	// After 100k distinct identities, the registry must remain bounded near
	// the high-water mark rather than retaining an entry per identity. A
	// small multiple of the threshold covers the entries created since the
	// most recent sweep.
	require.LessOrEqual(
		t, int64(peerMapLen(p)), 2*p.maxPeers,
		"registry grew unbounded under identity churn",
	)
}
