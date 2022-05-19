package amp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"github.com/lightningnetwork/lnd/lnwire"
	"sync"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/shards"
)

// Shard is an implementation of the shards.PaymentShards interface specific
// to AMP payments.
type Shard struct {
	child *Child
	mpp   *record.MPP
	amp   *record.AMP
}

// A compile time check to ensure Shard implements the shards.PaymentShard
// interface.
var _ shards.PaymentShard = (*Shard)(nil)

// Hash returns the hash used for the HTLC representing this AMP shard.
func (s *Shard) Hash() lntypes.Hash {
	return s.child.Hash
}

// MPP returns any extra MPP records that should be set for the final hop on
// the route used by this shard.
func (s *Shard) MPP() *record.MPP {
	return s.mpp
}

// AMP returns any extra AMP records that should be set for the final hop on
// the route used by this shard.
func (s *Shard) AMP() *record.AMP {
	return s.amp
}

// ShardTracker is an implementation of the shards.ShardTracker interface
// that is able to generate payment shards according to the AMP splitting
// algorithm. It can be used to generate new hashes to use for HTLCs, and also
// cancel shares used for failed payment shards.
type ShardTracker struct {
	setID       [32]byte
	paymentAddr [32]byte
	//totalAmt    lnwire.MilliSatoshi
	/*obd update wxf*/
	totalAmt    lnwire.UnitPrec11
	assetId uint32

	sharer Sharer

	shards map[uint64]*Child
	sync.Mutex
}

// A compile time check to ensure ShardTracker implements the
// shards.ShardTracker interface.
var _ shards.ShardTracker = (*ShardTracker)(nil)

// NewShardTracker creates a new shard tracker to use for AMP payments. The
// root shard, setID, payment address and total amount must be correctly set in
// order for the TLV options to include with each shard to be created
// correctly.
func NewShardTracker(root, setID, payAddr [32]byte,
	//totalAmt lnwire.MilliSatoshi) *ShardTracker {
	totalAmt lnwire.UnitPrec11,assetId uint32) *ShardTracker {

	// Create a new seed sharer from this root.
	rootShare := Share(root)
	rootSharer := SeedSharerFromRoot(&rootShare)

	return &ShardTracker{
		setID:       setID,
		paymentAddr: payAddr,
		totalAmt:    totalAmt,
		sharer:      rootSharer,
		shards:      make(map[uint64]*Child),
	}
}

// NewShard registers a new attempt with the ShardTracker and returns a
// new shard representing this attempt. This attempt's shard should be canceled
// if it ends up not being used by the overall payment, i.e. if the attempt
// fails.
func (s *ShardTracker) NewShard(pid uint64, last bool) (shards.PaymentShard,
	error) {

	s.Lock()
	defer s.Unlock()

	// Use a random child index.
	var childIndex [4]byte
	if _, err := rand.Read(childIndex[:]); err != nil {
		return nil, err
	}
	idx := binary.BigEndian.Uint32(childIndex[:])

	// Depending on whether we are requesting the last shard or not, either
	// split the current share into two, or get a Child directly from the
	// current sharer.
	var child *Child
	if last {
		child = s.sharer.Child(idx)

		// If this was the last shard, set the current share to the
		// zero share to indicate we cannot split it further.
		s.sharer = s.sharer.Zero()
	} else {
		left, sharer, err := s.sharer.Split()
		if err != nil {
			return nil, err
		}

		s.sharer = sharer
		child = left.Child(idx)
	}

	// Track the new child and return the shard.
	s.shards[pid] = child

	mpp := record.NewMPP(s.totalAmt, s.paymentAddr,s.assetId)
	amp := record.NewAMP(
		child.ChildDesc.Share, s.setID, child.ChildDesc.Index,
	)

	return &Shard{
		child: child,
		mpp:   mpp,
		amp:   amp,
	}, nil
}

// CancelShard cancel's the shard corresponding to the given attempt ID.
func (s *ShardTracker) CancelShard(pid uint64) error {
	s.Lock()
	defer s.Unlock()

	c, ok := s.shards[pid]
	if !ok {
		return fmt.Errorf("pid not found")
	}
	delete(s.shards, pid)

	// Now that we are canceling this shard, we XOR the share back into our
	// current share.
	s.sharer = s.sharer.Merge(c)
	return nil
}

// GetHash retrieves the hash used by the shard of the given attempt ID. This
// will return an error if the attempt ID is unknown.
func (s *ShardTracker) GetHash(pid uint64) (lntypes.Hash, error) {
	s.Lock()
	defer s.Unlock()

	c, ok := s.shards[pid]
	if !ok {
		return lntypes.Hash{}, fmt.Errorf("AMP shard for attempt %v "+
			"not found", pid)
	}

	return c.Hash, nil
}
