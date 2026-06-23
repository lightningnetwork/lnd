package reputation

import (
	"reflect"
	"testing"
)

// TestBucketAllocations checks the 40/20/40 split with remainder to protected.
func TestBucketAllocations(t *testing.T) {
	t.Parallel()

	a := computeBucketAllocations(100, 1_000_000, 40, 20)
	if a.generalSlots != 40 || a.congestionSlots != 20 ||
		a.protectedSlots != 40 {

		t.Fatalf("slots: %+v", a)
	}
	if a.generalLiquidity != 400_000 || a.congestionLiquidity != 200_000 ||
		a.protectedLiquidity != 400_000 {

		t.Fatalf("liquidity: %+v", a)
	}
}

// TestBucketAllocationsSmallChannel verifies that a channel too small to split
// across three buckets is given a single general bucket rather than degenerate
// zero-slot congestion/protected buckets (review finding B4).
func TestBucketAllocationsSmallChannel(t *testing.T) {
	t.Parallel()

	// 2 HTLCs: under the default 40/20 split the general bucket would get 0
	// slots (2*40/100 = 0). All capacity must instead go to general.
	a := computeBucketAllocations(2, 1_000_000, 40, 20)
	if a.generalSlots != 2 || a.congestionSlots != 0 ||
		a.protectedSlots != 0 {

		t.Fatalf("small-channel slots: %+v", a)
	}
	if a.generalLiquidity != 1_000_000 || a.congestionLiquidity != 0 ||
		a.protectedLiquidity != 0 {

		t.Fatalf("small-channel liquidity: %+v", a)
	}

	// The general bucket built from this allocation must be usable (not
	// degenerate) and its slot assignment must succeed despite being tiny.
	g := newGeneralBucket(1, a.generalSlots, a.generalLiquidity)
	if g.totalSlots == 0 || g.perChannelSlots == 0 {
		t.Fatalf("degenerate general bucket: %+v", g)
	}
	if int(g.perChannelSlots) > int(g.totalSlots) {
		t.Fatalf("perChannelSlots %d exceeds totalSlots %d",
			g.perChannelSlots, g.totalSlots)
	}
	if _, err := g.assignSlotsForChannel(2, nil); err != nil {
		t.Fatalf("slot assignment failed for small bucket: %v", err)
	}
}

// TestGeneralBucketSlotDeterminism verifies that slot assignment is
// deterministic for a fixed salt and regenerates identically (the restart
// property).
func TestGeneralBucketSlotDeterminism(t *testing.T) {
	t.Parallel()

	var salt [32]byte
	for i := range salt {
		salt[i] = byte(i)
	}

	g1 := newGeneralBucket(1, 40, 400_000)
	slots1, err := g1.assignSlotsForChannel(2, &salt)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	// Same salt + scids on a fresh bucket yields the identical slot set.
	g2 := newGeneralBucket(1, 40, 400_000)
	slots2, err := g2.assignSlotsForChannel(2, &salt)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if !reflect.DeepEqual(slots1, slots2) {
		t.Fatalf("non-deterministic: %v vs %v", slots1, slots2)
	}

	if len(slots1) != int(g1.perChannelSlots) {
		t.Fatalf("got %d slots, want %d", len(slots1),
			g1.perChannelSlots)
	}

	// Re-requesting returns the cached set.
	slots3, err := g1.assignSlotsForChannel(2, nil)
	if err != nil {
		t.Fatalf("assign cached: %v", err)
	}
	if !reflect.DeepEqual(slots1, slots3) {
		t.Fatalf("cache mismatch: %v vs %v", slots1, slots3)
	}
}

// TestGeneralBucketOccupancy checks reserve/release of slots.
func TestGeneralBucketOccupancy(t *testing.T) {
	t.Parallel()

	g := newGeneralBucket(1, 40, 400_000)

	// per_slot_msat = 400_000 * perChannelSlots / 40. perChannelSlots = 5
	// => per_slot_msat = 50_000. A 10_000 msat HTLC needs 1 slot.
	ok, err := g.canAddHTLC(2, 10_000)
	if err != nil || !ok {
		t.Fatalf("canAddHTLC: ok=%v err=%v", ok, err)
	}

	if err := g.addHTLC(2, 10_000); err != nil {
		t.Fatalf("addHTLC: %v", err)
	}

	if err := g.removeHTLC(2, 10_000); err != nil {
		t.Fatalf("removeHTLC: %v", err)
	}

	// After release all slots are free again.
	for _, occ := range g.slotsOccupied {
		if occ != nil {
			t.Fatalf("slot still occupied after release")
		}
	}
}

// TestBucketResources checks the simple slot+liquidity counter.
func TestBucketResources(t *testing.T) {
	t.Parallel()

	b := newBucketResources(2, 1000)

	if !b.resourcesAvailable(500) {
		t.Fatalf("expected available")
	}
	if err := b.addHTLC(500); err != nil {
		t.Fatalf("addHTLC: %v", err)
	}
	if err := b.addHTLC(500); err != nil {
		t.Fatalf("addHTLC: %v", err)
	}
	// Slots exhausted.
	if b.resourcesAvailable(1) {
		t.Fatalf("expected no slots")
	}
	if err := b.addHTLC(1); err == nil {
		t.Fatalf("expected slot exhaustion error")
	}
	if err := b.removeHTLC(500); err != nil {
		t.Fatalf("removeHTLC: %v", err)
	}
	if !b.resourcesAvailable(500) {
		t.Fatalf("expected available after release")
	}
}
