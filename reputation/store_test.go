package reputation

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
)

// TestSnapshotRoundTrip checks binary encode/decode of a channel snapshot.
func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	snap := ChannelSnapshot{
		SCID:             42,
		MaxInFlightMsat:  100_000_000,
		MaxAcceptedHTLCs: 100,
		reputation: avgSnapshot{
			value:       12345,
			lastUpdated: 1_000_000,
			windowSecs:  1209600,
		},
		revenue: revenueSnapshot{
			start:          1_000_000,
			windowCount:    12,
			windowDuration: 1209600,
			inner: avgSnapshot{
				value: 678, lastUpdated: 1_000_500,
				windowSecs: 14515200,
			},
		},
		salts:  map[uint64][32]byte{7: {1, 2, 3}},
		misuse: map[uint64]uint64{9: 1_000_123},
	}

	var buf bytes.Buffer
	if err := encodeSnapshot(&buf, &snap); err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := decodeSnapshot(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.SCID != snap.SCID ||
		got.MaxInFlightMsat != snap.MaxInFlightMsat ||
		got.MaxAcceptedHTLCs != snap.MaxAcceptedHTLCs {

		t.Fatalf("header mismatch: %+v", got)
	}
	if got.reputation != snap.reputation {
		t.Fatalf("reputation mismatch: %+v vs %+v", got.reputation,
			snap.reputation)
	}
	if got.revenue != snap.revenue {
		t.Fatalf("revenue mismatch: %+v vs %+v", got.revenue,
			snap.revenue)
	}
	if got.salts[7] != snap.salts[7] || got.misuse[9] != snap.misuse[9] {
		t.Fatalf("map mismatch: %+v", got)
	}
}

// newTestStore returns a kvdb-backed store on a temp bbolt db.
func newTestStore(t *testing.T) Store {
	t.Helper()

	db, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "reputation")
	if err != nil {
		t.Fatalf("GetTestBackend: %v", err)
	}
	t.Cleanup(cleanup)

	return NewKVDBStore(db)
}

// TestKVDBStorePersistLoad checks the kvdb store round-trips state and that a
// warm restart reconstructs reputation/revenue.
func TestKVDBStorePersistLoad(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	const start = 1_000_000

	src := newFakeChannelSource(
		chanInfo(1, 100, 100_000_000),
		chanInfo(2, 100, 100_000_000),
	)

	m1, err := NewManager(
		DefaultConfig(),
		WithClock(newTestClock(start)),
		WithHeightSource(func() uint32 { return 100 }),
		WithChannelSource(src),
		WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m1.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Forward + settle to accrue reputation/revenue, then flush.
	in, out := circuit(1, 0), circuit(2, 0)
	m1.OnForward(in, out, 2000, 1000, 200, false)
	m1.OnSettle(in, out)

	if err := m1.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = m1.Stop()

	// New manager over the same store: warm restart.
	m2, err := NewManager(
		DefaultConfig(),
		WithClock(newTestClock(start)),
		WithHeightSource(func() uint32 { return 100 }),
		WithChannelSource(newFakeChannelSource()),
		WithStore(store),
	)
	if err != nil {
		t.Fatalf("NewManager 2: %v", err)
	}
	if err := m2.Start(); err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	t.Cleanup(func() { _ = m2.Stop() })

	m2.mu.Lock()
	defer m2.mu.Unlock()

	outChan, ok := m2.channels[2]
	if !ok {
		t.Fatalf("outgoing channel not restored")
	}
	rep, err := outChan.outgoingReputation.valueAt(start)
	if err != nil {
		t.Fatalf("valueAt: %v", err)
	}
	if rep != 1000 {
		t.Fatalf("restored reputation: got %d, want 1000", rep)
	}
}

// TestEmptyStoreLoad ensures loading an empty store is a clean no-op.
func TestEmptyStoreLoad(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	snaps, err := store.LoadChannels()
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("expected empty, got %d", len(snaps))
	}
}

// TestReplayInFlight checks that in-flight HTLCs are rebuilt into pending state
// and bucket occupancy at startup (P9).
func TestReplayInFlight(t *testing.T) {
	t.Parallel()

	m, _ := buildManager(t, 1_000_000, 100)

	m.ReplayInFlight([]InFlightHTLC{
		{
			Incoming:     circuit(1, 5),
			Outgoing:     circuit(2, 5),
			IncomingAmt:  2000,
			OutgoingAmt:  1000,
			IncomingCltv: 200,
			Accountable:  false,
		},
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.channels[2].pendingHTLCs) != 1 {
		t.Fatalf("replayed pending not present")
	}
}
