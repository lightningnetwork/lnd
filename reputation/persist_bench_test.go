package reputation

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

// benchChannelCounts are the channel counts the persistence benchmarks run
// at: a small node, a mid-sized routing node and a large one.
var benchChannelCounts = []int{10, 100, 1000}

// forwardsPerFlush is how many forwards the amortised benchmark drives between
// two flushes, standing in for a minute of traffic on a busy node.
const forwardsPerFlush = 10_000

// newBenchSQLManager builds a manager on a fresh SQL store with numChannels
// channels that each forwarded and settled one HTLC, so all of them hold state
// and are dirty.
func newBenchSQLManager(b *testing.B, numChannels int) (*Manager, Store) {
	b.Helper()

	store, _ := newTestSQLStore(b)
	m, err := NewManager(
		DefaultConfig(), clock.NewTestClock(time.Unix(1_000_000, 0)),
		store,
	)
	if err != nil {
		b.Fatalf("NewManager: %v", err)
	}

	for i := range numChannels {
		in := circuit(uint64(i), 0)
		m.OnForward(
			in, chanID(uint64(i)), chanID(uint64(i+1)), 2000, 1000,
			1000, 200, 100, false,
		)
		m.OnSettle(in)
	}

	return m, store
}

// BenchmarkFlush measures one flush of the manager with every channel dirty:
// a single SQL transaction upserting one row per channel. This is the
// periodic write the persistence adds, and it runs off the forwarding path.
func BenchmarkFlush(b *testing.B) {
	for _, numChannels := range benchChannelCounts {
		name := fmt.Sprintf("channels=%d", numChannels)
		b.Run(name, func(b *testing.B) {
			m, _ := newBenchSQLManager(b, numChannels)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				m.mu.Lock()
				for id := range m.channels {
					m.dirty[id] = struct{}{}
				}
				m.mu.Unlock()
				b.StartTimer()

				if err := m.flush(); err != nil {
					b.Fatalf("flush: %v", err)
				}
			}
		})
	}
}

// BenchmarkLoad measures restoring every channel from the store on startup.
func BenchmarkLoad(b *testing.B) {
	for _, numChannels := range benchChannelCounts {
		name := fmt.Sprintf("channels=%d", numChannels)
		b.Run(name, func(b *testing.B) {
			seed, store := newBenchSQLManager(b, numChannels)
			if err := seed.flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				clk := clock.NewTestClock(time.Unix(2e6, 0))
				m, err := NewManager(
					DefaultConfig(), clk, store,
				)
				if err != nil {
					b.Fatalf("NewManager: %v", err)
				}
				if err := m.load(); err != nil {
					b.Fatalf("load: %v", err)
				}
				if len(m.channels) != numChannels+1 {
					b.Fatalf("loaded %d channels, want %d",
						len(m.channels), numChannels+1)
				}
			}
		})
	}
}

// BenchmarkForwardResolvePersisted measures the per-forward cost with the SQL
// store attached and a flush every forwardsPerFlush forwards, so the reported
// time is the hot-path cost plus the write cost amortised over the forwards
// that caused it. Forwards are spread over random channel pairs so each flush
// writes the whole channel set.
func BenchmarkForwardResolvePersisted(b *testing.B) {
	for _, numChannels := range benchChannelCounts {
		name := fmt.Sprintf("channels=%d", numChannels)
		b.Run(name, func(b *testing.B) {
			m, _ := newBenchSQLManager(b, numChannels)
			if err := m.flush(); err != nil {
				b.Fatalf("flush: %v", err)
			}

			rng := rand.New(rand.NewSource(1))

			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				inIdx := uint64(rng.Intn(numChannels))
				outIdx := (inIdx + 1 +
					uint64(rng.Intn(numChannels-1))) %
					uint64(numChannels)

				in := circuit(inIdx, uint64(i))
				m.OnForward(
					in, chanID(inIdx), chanID(outIdx), 2000,
					1000, 1000, 200, 100, false,
				)
				m.OnSettle(in)

				if (i+1)%forwardsPerFlush == 0 {
					if err := m.flush(); err != nil {
						b.Fatalf("flush: %v", err)
					}
				}
			}
		})
	}
}
