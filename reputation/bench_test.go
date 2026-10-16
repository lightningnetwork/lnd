package reputation

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

// BenchmarkForwardResolve measures the full per-HTLC cost the reputation
// subsystem adds to a forward: the synchronous OnForward hook (record pending +
// compute the decision) plus the OnSettle hook (resolve + update the averages).
// When the subsystem is disabled the switch skips these hooks behind a single
// nil check, so this is the "enabled minus disabled" overhead per forwarded
// HTLC.
func BenchmarkForwardResolve(b *testing.B) {
	m, err := NewManager(
		DefaultConfig(), clock.NewTestClock(time.Unix(1_000_000, 0)),
	)
	if err != nil {
		b.Fatalf("NewManager: %v", err)
	}

	in := circuit(1, 0)
	inChan, outChan := chanID(1), chanID(2)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.OnForward(
			in, inChan, outChan, 2000, 1000, 1000, 200, 100, false,
		)
		m.OnSettle(in)
	}
}

// BenchmarkOnForward measures the cost of the forwarding hook alone (record
// pending + compute the reputation decision), which is the work that sits on
// the switch's forwarding path. Each iteration uses a distinct HTLC id and is
// resolved immediately so the pending map stays bounded.
func BenchmarkOnForward(b *testing.B) {
	m, err := NewManager(
		DefaultConfig(), clock.NewTestClock(time.Unix(1_000_000, 0)),
	)
	if err != nil {
		b.Fatalf("NewManager: %v", err)
	}

	inChan, outChan := chanID(1), chanID(2)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		in := circuit(1, uint64(i))
		m.OnForward(
			in, inChan, outChan, 2000, 1000, 1000, 200, 100, false,
		)

		b.StopTimer()
		m.OnSettle(in)
		b.StartTimer()
	}
}

// BenchmarkDecayingAverageAdd measures the core decaying-average update, the
// hot primitive underlying every reputation and revenue mutation.
func BenchmarkDecayingAverageAdd(b *testing.B) {
	d := newDecayingAverage(testStart, DefaultConfig().reputationWindow())

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		ts := testStart.Add(time.Duration(i) * time.Second)
		d.add(1000, ts)
	}
}

// BenchmarkForwardResolveManyChannels is BenchmarkForwardResolve on a node
// with many channels: each forward picks a pseudo-random (incoming, outgoing)
// pair out of numChannels, so the per-channel map and the HTLC index are
// populated rather than holding a single entry. The per-HTLC cost should stay
// flat as the channel count grows.
func BenchmarkForwardResolveManyChannels(b *testing.B) {
	for _, numChannels := range []uint64{10, 100, 1000} {
		name := fmt.Sprintf("channels=%d", numChannels)
		b.Run(name, func(b *testing.B) {
			m, err := NewManager(
				DefaultConfig(),
				clock.NewTestClock(time.Unix(1_000_000, 0)),
			)
			if err != nil {
				b.Fatalf("NewManager: %v", err)
			}

			rng := rand.New(rand.NewSource(1))

			// Warm every channel up so the maps are fully populated
			// before timing starts.
			for c := uint64(1); c <= numChannels; c++ {
				in := circuit(c, 0)
				m.OnForward(
					in, chanID(c), chanID(c%numChannels+1),
					2000, 1000, 1000, 200, 100, false,
				)
				m.OnSettle(in)
			}

			b.ReportAllocs()
			b.ResetTimer()
			n := int64(numChannels)
			for i := range b.N {
				inChan := uint64(rng.Int63n(n)) + 1
				outChan := uint64(rng.Int63n(n)) + 1

				in := circuit(inChan, uint64(i))
				m.OnForward(
					in, chanID(inChan), chanID(outChan),
					2000, 1000, 1000, 200, 100, false,
				)
				m.OnSettle(in)
			}
		})
	}
}
