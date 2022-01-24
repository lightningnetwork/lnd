package shards

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/record"
)

// PaymentShard is an interface representing a shard tracked by the
// ShardTracker. It contains options that are specific to the given shard that
// might differ from the overall payment.
type PaymentShard interface {
	// Hash returns the hash used for the HTLC representing this shard.
	Hash() lntypes.Hash

	// MPP returns any extra MPP records that should be set for the final
	// hop on the route used by this shard.
	MPP() *record.MPP

	// AMP returns any extra AMP records that should be set for the final
	// hop on the route used by this shard.
	AMP() *record.AMP
}

// ShardTracker is an interface representing a tracker that keeps track of the
// inflight shards of a payment, and is able to assign new shards the correct
// options such as hash and extra records.
type ShardTracker interface {
	// NewShard registers a new attempt with the ShardTracker and returns a
	// new shard representing this attempt. This attempt's shard should be
	// canceled if it ends up not being used by the overall payment, i.e.
	// if the attempt fails.
	NewShard(uint64, bool) (PaymentShard, error)

	// CancelShard cancel's the shard corresponding to the given attempt
	// ID. This lets the ShardTracker free up any slots used by this shard,
	// and in case of AMP payments return the share used by this shard to
	// the root share.
	CancelShard(uint64) error

	// GetHash retrieves the hash used by the shard of the given attempt
	// ID. This will return an error if the attempt ID is unknown.
	GetHash(uint64) (lntypes.Hash, error)
}

// Shard is a struct used for simple shards where we only need to keep map it
// to a single hash.
type Shard struct {
	hash lntypes.Hash
}

// Hash returns the hash used for the HTLC representing this shard.
func (s *Shard) Hash() lntypes.Hash {
	return s.hash
}

// MPP returns any extra MPP records that should be set for the final hop on
// the route used by this shard.
func (s *Shard) MPP() *record.MPP {
	return nil
}

// AMP returns any extra AMP records that should be set for the final hop on
// the route used by this shard.
func (s *Shard) AMP() *record.AMP {
	return nil
}

// SimpleShardTracker is an implementation of the ShardTracker interface that
// simply maps attempt IDs to hashes. New shards will be given a static payment
// hash. This should be used for regular and MPP payments, in addition to
// resumed payments where all the attempt's hashes have already been created.
type SimpleShardTracker struct {
	hash   lntypes.Hash
	shards map[uint64]lntypes.Hash
	sync.Mutex
}

// A compile time check to ensure SimpleShardTracker implements the
// ShardTracker interface.
var _ ShardTracker = (*SimpleShardTracker)(nil)

// NewSimpleShardTracker creates a new instance of the SimpleShardTracker with
// the given payment hash and existing attempts.
func NewSimpleShardTracker(paymentHash lntypes.Hash,
	shards map[uint64]lntypes.Hash) ShardTracker {

	if shards == nil {
		shards = make(map[uint64]lntypes.Hash)
	}

	return &SimpleShardTracker{
		hash:   paymentHash,
		shards: shards,
	}
}

// NewShard registers a new attempt with the ShardTracker and returns a
// new shard representing this attempt. This attempt's shard should be canceled
// if it ends up not being used by the overall payment, i.e. if the attempt
// fails.
func (m *SimpleShardTracker) NewShard(id uint64, _ bool) (PaymentShard, error) {
	m.Lock()
	m.shards[id] = m.hash
	m.Unlock()

	return &Shard{
		hash: m.hash,
	}, nil
}

// CancelShard cancel's the shard corresponding to the given attempt ID.
func (m *SimpleShardTracker) CancelShard(id uint64) error {
	m.Lock()
	delete(m.shards, id)
	m.Unlock()

	return nil
}

// GetHash retrieves the hash used by the shard of the given attempt ID. This
// will return an error if the attempt ID is unknown.
func (m *SimpleShardTracker) GetHash(id uint64) (lntypes.Hash, error) {
	m.Lock()
	hash, ok := m.shards[id]
	m.Unlock()
	if !ok {
		return lntypes.Hash{}, fmt.Errorf("hash for attempt id %v "+
			"not found", id)
	}

	return hash, nil
}
