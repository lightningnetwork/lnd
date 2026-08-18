package reputation

import (
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
	out := scid(2)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.OnForward(in, out, 2000, 1000, 1000, 200, 100, false)
		m.OnSettle(in, out)
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

	out := scid(2)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := circuit(1, uint64(i))
		m.OnForward(in, out, 2000, 1000, 1000, 200, 100, false)

		b.StopTimer()
		m.OnSettle(in, out)
		b.StartTimer()
	}
}

// BenchmarkDecayingAverageAdd measures the core decaying-average update, the
// hot primitive underlying every reputation and revenue mutation.
func BenchmarkDecayingAverageAdd(b *testing.B) {
	d := newDecayingAverage(testStart, DefaultConfig().reputationWindow())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts := testStart.Add(time.Duration(i) * time.Second)
		if _, err := d.add(1000, ts); err != nil {
			b.Fatalf("add: %v", err)
		}
	}
}
