package reputation

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/chacha20"
)

// minGeneralBucketSlots is the floor on the number of general-bucket slots any
// single forwarding pair is assigned (matches the LDK reference).
const minGeneralBucketSlots = 5

// bucketAllocations holds the slot and liquidity split across the three
// buckets for a single channel.
type bucketAllocations struct {
	generalSlots        uint16
	generalLiquidity    uint64
	congestionSlots     uint16
	congestionLiquidity uint64
	protectedSlots      uint16
	protectedLiquidity  uint64
}

// computeBucketAllocations splits a channel's max accepted HTLCs and max
// in-flight liquidity across the general / congestion / protected buckets using
// the configured percentages (protected receives the remainder).
func computeBucketAllocations(maxAcceptedHTLCs uint16, maxInFlightMsat uint64,
	generalPct, congestionPct uint8) bucketAllocations {

	// Channels too small to split meaningfully across three buckets get a
	// single general bucket (basic DoS protection only), per the BOLT
	// recommendation for small channels. This also avoids degenerate
	// zero-slot congestion/protected buckets that would distort the
	// (log-only) decision tree — review finding B4.
	if maxAcceptedHTLCs < minChannelHTLCsForBuckets {
		return bucketAllocations{
			generalSlots:     maxAcceptedHTLCs,
			generalLiquidity: maxInFlightMsat,
		}
	}

	gSlots := maxAcceptedHTLCs * uint16(generalPct) / 100
	gLiq := maxInFlightMsat * uint64(generalPct) / 100

	cSlots := maxAcceptedHTLCs * uint16(congestionPct) / 100
	cLiq := maxInFlightMsat * uint64(congestionPct) / 100

	return bucketAllocations{
		generalSlots:        gSlots,
		generalLiquidity:    gLiq,
		congestionSlots:     cSlots,
		congestionLiquidity: cLiq,
		protectedSlots:      maxAcceptedHTLCs - gSlots - cSlots,
		protectedLiquidity:  maxInFlightMsat - gLiq - cLiq,
	}
}

// channelSlots records the general-bucket slot indices a forwarding pair is
// assigned, along with the salt used to derive them (persisted so the same
// slots can be regenerated deterministically after a restart).
type channelSlots struct {
	slots []uint16
	salt  [32]byte
}

// generalBucket implements the salted, per-forwarding-pair slot allocation of
// the general bucket. Each (incoming scid, outgoing scid) pair is
// deterministically assigned a fixed subset of the bucket's slots via a
// ChaCha20 stream, so an attacker cannot predict or target a victim pair's
// slots without controlling the salt.
type generalBucket struct {
	// scid is the incoming channel this bucket belongs to.
	scid uint64

	totalSlots     uint16
	totalLiquidity uint64

	perChannelSlots uint8
	perSlotMsat     uint64

	// slotsOccupied maps slot index -> occupying outgoing scid (or nil if
	// free).
	slotsOccupied []*uint64

	// channelsSlots maps outgoing scid -> its assigned slots + salt.
	channelsSlots map[uint64]channelSlots
}

// newGeneralBucket constructs a general bucket for the given incoming channel,
// sized from its slot and liquidity allocation.
func newGeneralBucket(scid uint64, slotsAllocated uint16,
	liquidityAllocated uint64) *generalBucket {

	perChannel := divCeil(uint32(slotsAllocated)*5, 100)
	if perChannel < minGeneralBucketSlots {
		perChannel = minGeneralBucketSlots
	}

	// A pair can never be assigned more distinct slots than the bucket has;
	// cap it so slot assignment can always succeed for small buckets
	// (review finding B4). A zero-slot bucket stays unusable (safe-fail).
	if perChannel > uint32(slotsAllocated) {
		perChannel = uint32(slotsAllocated)
	}

	var perSlotMsat uint64
	if slotsAllocated > 0 {
		perSlotMsat = liquidityAllocated * uint64(perChannel) /
			uint64(slotsAllocated)
	}

	return &generalBucket{
		scid:            scid,
		totalSlots:      slotsAllocated,
		totalLiquidity:  liquidityAllocated,
		perChannelSlots: uint8(perChannel),
		perSlotMsat:     perSlotMsat,
		slotsOccupied:   make([]*uint64, slotsAllocated),
		channelsSlots:   make(map[uint64]channelSlots),
	}
}

// divCeil returns ceil(a/b) for unsigned integers.
func divCeil(a, b uint32) uint32 {
	if b == 0 {
		return 0
	}

	return (a + b - 1) / b
}

// assignSlotsForChannel deterministically derives the slot indices for an
// outgoing scid. If salt is nil a fresh random salt is generated; otherwise the
// provided salt is used (for deterministic regeneration on restart).
func (g *generalBucket) assignSlotsForChannel(outgoingScid uint64,
	salt *[32]byte) ([]uint16, error) {

	if existing, ok := g.channelsSlots[outgoingScid]; ok {
		return existing.slots, nil
	}

	if g.totalSlots == 0 {
		return nil, fmt.Errorf("general bucket has no slots")
	}

	var s [32]byte
	if salt != nil {
		s = *salt
	} else {
		if _, err := rand.Read(s[:]); err != nil {
			return nil, err
		}
	}

	// If the channel needs (at least) every slot in the bucket, assign them
	// all deterministically. Random distinct-slot selection is unnecessary
	// here and can fail to converge for tiny buckets (e.g. when
	// perChannelSlots == totalSlots), which would otherwise make assignment
	// probabilistic.
	if int(g.perChannelSlots) >= int(g.totalSlots) {
		slots := make([]uint16, g.totalSlots)
		for i := range slots {
			slots[i] = uint16(i)
		}
		g.channelsSlots[outgoingScid] = channelSlots{
			slots: slots, salt: s,
		}

		return slots, nil
	}

	// nonce = incoming_scid_be[0:4] || outgoing_scid_be[0:8].
	var nonce [12]byte
	var inBE, outBE [8]byte
	binary.BigEndian.PutUint64(inBE[:], g.scid)
	binary.BigEndian.PutUint64(outBE[:], outgoingScid)
	copy(nonce[:4], inBE[:4])
	copy(nonce[4:], outBE[:])

	cipher, err := chacha20.NewUnauthenticatedCipher(s[:], nonce[:])
	if err != nil {
		return nil, err
	}

	need := int(g.perChannelSlots)
	slots := make([]uint16, 0, need)
	buf := make([]byte, 4)
	zero := make([]byte, 4)
	maxAttempts := need * 2
	for i := 0; i < maxAttempts && len(slots) < need; i++ {
		// XOR a zero buffer to read raw keystream bytes.
		cipher.XORKeyStream(buf, zero)
		raw := binary.LittleEndian.Uint32(buf)
		idx := uint16(raw % uint32(g.totalSlots))
		if !containsUint16(slots, idx) {
			slots = append(slots, idx)
		}
	}

	if len(slots) < int(g.perChannelSlots) {
		return nil, fmt.Errorf("could not assign %d distinct slots",
			g.perChannelSlots)
	}

	g.channelsSlots[outgoingScid] = channelSlots{slots: slots, salt: s}

	return slots, nil
}

// slotsForAmount returns the free slot indices the outgoing scid could use for
// an HTLC of the given amount, or (nil, false) if insufficient free slots are
// available to its assigned set.
func (g *generalBucket) slotsForAmount(outgoingScid uint64,
	amtMsat uint64) ([]uint16, bool, error) {

	if g.totalSlots == 0 || g.perSlotMsat == 0 {
		return nil, false, nil
	}

	slotsNeeded := divCeilU64(amtMsat, g.perSlotMsat)
	if slotsNeeded < 1 {
		slotsNeeded = 1
	}

	assigned, ok := g.channelsSlots[outgoingScid]
	var slots []uint16
	if ok {
		slots = assigned.slots
	} else {
		var err error
		slots, err = g.assignSlotsForChannel(outgoingScid, nil)
		if err != nil {
			return nil, false, err
		}
	}

	free := make([]uint16, 0, slotsNeeded)
	for _, idx := range slots {
		if int(idx) >= len(g.slotsOccupied) {
			continue
		}
		if g.slotsOccupied[idx] == nil {
			free = append(free, idx)
			if uint64(len(free)) == slotsNeeded {
				break
			}
		}
	}

	if uint64(len(free)) < slotsNeeded {
		return nil, false, nil
	}

	return free, true, nil
}

// canAddHTLC reports whether an HTLC of the given amount could be placed in the
// general bucket for the outgoing scid.
func (g *generalBucket) canAddHTLC(outgoingScid, amtMsat uint64) (bool, error) {
	_, ok, err := g.slotsForAmount(outgoingScid, amtMsat)

	return ok, err
}

// addHTLC reserves the slots required for an HTLC of the given amount.
func (g *generalBucket) addHTLC(outgoingScid, amtMsat uint64) error {
	slots, ok, err := g.slotsForAmount(outgoingScid, amtMsat)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no general slots available")
	}

	for _, idx := range slots {
		scid := outgoingScid
		g.slotsOccupied[idx] = &scid
	}

	return nil
}

// removeHTLC releases the slots reserved for an HTLC of the given amount.
func (g *generalBucket) removeHTLC(outgoingScid, amtMsat uint64) error {
	assigned, ok := g.channelsSlots[outgoingScid]
	if !ok {
		return fmt.Errorf("no slots assigned for scid")
	}

	slotsNeeded := divCeilU64(amtMsat, g.perSlotMsat)
	if slotsNeeded < 1 {
		slotsNeeded = 1
	}

	var released uint64
	for _, idx := range assigned.slots {
		if released == slotsNeeded {
			break
		}
		if g.occupiedBy(idx, outgoingScid) {
			g.slotsOccupied[idx] = nil
			released++
		}
	}

	if released < slotsNeeded {
		return fmt.Errorf("released fewer slots than required")
	}

	return nil
}

// occupiedBy reports whether general-bucket slot idx is currently held by the
// given outgoing scid.
func (g *generalBucket) occupiedBy(idx uint16, scid uint64) bool {
	return int(idx) < len(g.slotsOccupied) &&
		g.slotsOccupied[idx] != nil && *g.slotsOccupied[idx] == scid
}

// removeChannelSlots frees all slots and the assignment for an outgoing scid,
// used when that channel is removed.
func (g *generalBucket) removeChannelSlots(outgoingScid uint64) {
	assigned, ok := g.channelsSlots[outgoingScid]
	if !ok {
		return
	}

	for _, idx := range assigned.slots {
		if g.occupiedBy(idx, outgoingScid) {
			g.slotsOccupied[idx] = nil
		}
	}

	delete(g.channelsSlots, outgoingScid)
}

// bucketResources is the simple slot+liquidity counter backing the congestion
// and protected buckets.
type bucketResources struct {
	slotsAllocated     uint16
	slotsUsed          uint16
	liquidityAllocated uint64
	liquidityUsed      uint64
}

// newBucketResources constructs an empty bucket with the given allocation.
func newBucketResources(slots uint16, liquidity uint64) *bucketResources {
	return &bucketResources{
		slotsAllocated:     slots,
		liquidityAllocated: liquidity,
	}
}

// resourcesAvailable reports whether an HTLC of the given amount fits.
func (b *bucketResources) resourcesAvailable(amtMsat uint64) bool {
	return b.liquidityUsed+amtMsat <= b.liquidityAllocated &&
		b.slotsUsed < b.slotsAllocated
}

// addHTLC reserves a slot and liquidity for an HTLC of the given amount.
func (b *bucketResources) addHTLC(amtMsat uint64) error {
	if !b.resourcesAvailable(amtMsat) {
		return fmt.Errorf("bucket resources unavailable")
	}

	b.slotsUsed++
	b.liquidityUsed += amtMsat

	return nil
}

// removeHTLC releases a slot and liquidity previously reserved.
func (b *bucketResources) removeHTLC(amtMsat uint64) error {
	if b.slotsUsed == 0 || b.liquidityUsed < amtMsat {
		return fmt.Errorf("bucket underflow on remove")
	}

	b.slotsUsed--
	b.liquidityUsed -= amtMsat

	return nil
}

// divCeilU64 returns ceil(a/b) for uint64.
func divCeilU64(a, b uint64) uint64 {
	if b == 0 {
		return 0
	}

	return (a + b - 1) / b
}

// containsUint16 reports whether s contains v.
func containsUint16(s []uint16, v uint16) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}

	return false
}
