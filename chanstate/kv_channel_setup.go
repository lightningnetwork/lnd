package chanstate

import (
	graphmodels "github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// channelOpeningStateBucket is the database bucket used to store the
	// channelOpeningState for each channel that is currently in the process
	// of being opened.
	channelOpeningStateBucket = []byte("channelOpeningState")

	// initialChannelForwardingPolicyBucket is the database bucket used to
	// store the forwarding policy for each permanent channel that is
	// currently in the process of being opened.
	initialChannelForwardingPolicyBucket = []byte(
		"initialChannelFwdingPolicy",
	)
)

// ChannelOpeningStateBucketKey returns the top-level bucket key used to store
// serialized channel opening state.
func ChannelOpeningStateBucketKey() []byte {
	return channelOpeningStateBucket
}

// InitialChannelForwardingPolicyBucketKey returns the top-level bucket key used
// to store initial channel forwarding policies.
func InitialChannelForwardingPolicyBucketKey() []byte {
	return initialChannelForwardingPolicyBucket
}

// SaveChannelOpeningState saves the serialized channel state for the provided
// chanPoint to the channelOpeningStateBucket.
func SaveChannelOpeningState(tx kvdb.RwTx, outPoint,
	serializedState []byte) error {

	bucket, err := tx.CreateTopLevelBucket(channelOpeningStateBucket)
	if err != nil {
		return err
	}

	return bucket.Put(outPoint, serializedState)
}

// SaveChannelOpeningState saves the serialized channel state for the provided
// chanPoint to the channelOpeningStateBucket.
func (s *KVStore) SaveChannelOpeningState(outPoint,
	serializedState []byte) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		return SaveChannelOpeningState(tx, outPoint, serializedState)
	}, func() {})
}

// GetChannelOpeningState fetches the serialized channel state for the provided
// outPoint from the database, or returns ErrChannelNotFound if the channel is
// not found.
func GetChannelOpeningState(tx kvdb.RTx, outPoint []byte) ([]byte, error) {
	bucket := tx.ReadBucket(channelOpeningStateBucket)
	if bucket == nil {
		// If the bucket does not exist, it means we never added
		//  a channel to the db, so return ErrChannelNotFound.
		return nil, ErrChannelNotFound
	}

	stateBytes := bucket.Get(outPoint)
	if stateBytes == nil {
		return nil, ErrChannelNotFound
	}

	var serializedState []byte
	serializedState = append(serializedState, stateBytes...)

	return serializedState, nil
}

// GetChannelOpeningState fetches the serialized channel state for the provided
// outPoint from the database, or returns ErrChannelNotFound if the channel is
// not found.
func (s *KVStore) GetChannelOpeningState(outPoint []byte) ([]byte, error) {
	var serializedState []byte

	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		var err error
		serializedState, err = GetChannelOpeningState(tx, outPoint)

		return err
	}, func() {
		serializedState = nil
	})

	return serializedState, err
}

// DeleteChannelOpeningState removes any state for outPoint from the database.
func DeleteChannelOpeningState(tx kvdb.RwTx, outPoint []byte) error {
	bucket := tx.ReadWriteBucket(channelOpeningStateBucket)
	if bucket == nil {
		return ErrChannelNotFound
	}

	return bucket.Delete(outPoint)
}

// DeleteChannelOpeningState removes any state for outPoint from the database.
func (s *KVStore) DeleteChannelOpeningState(outPoint []byte) error {
	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		return DeleteChannelOpeningState(tx, outPoint)
	}, func() {})
}

// SaveInitialForwardingPolicy saves the serialized forwarding policy for the
// provided permanent channel id to the initialChannelForwardingPolicyBucket.
func (s *KVStore) SaveInitialForwardingPolicy(chanID lnwire.ChannelID,
	forwardingPolicy *graphmodels.ForwardingPolicy) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	scratch := make([]byte, 36)
	byteOrder.PutUint64(scratch[:8], uint64(forwardingPolicy.MinHTLCOut))
	byteOrder.PutUint64(scratch[8:16], uint64(forwardingPolicy.MaxHTLC))
	byteOrder.PutUint64(scratch[16:24], uint64(forwardingPolicy.BaseFee))
	byteOrder.PutUint64(scratch[24:32], uint64(forwardingPolicy.FeeRate))
	byteOrder.PutUint32(scratch[32:], forwardingPolicy.TimeLockDelta)

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(
			initialChannelForwardingPolicyBucket,
		)
		if err != nil {
			return err
		}

		return bucket.Put(chanIDCopy, scratch)
	}, func() {})
}

// GetInitialForwardingPolicy fetches the serialized forwarding policy for the
// provided channel id from the database, or returns ErrChannelNotFound if
// a forwarding policy for this channel id is not found.
func (s *KVStore) GetInitialForwardingPolicy(
	chanID lnwire.ChannelID) (*graphmodels.ForwardingPolicy, error) {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	var forwardingPolicy *graphmodels.ForwardingPolicy
	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(initialChannelForwardingPolicyBucket)
		if bucket == nil {
			// If the bucket does not exist, it means we
			// never added a channel fees to the db, so
			// return ErrChannelNotFound.
			return ErrChannelNotFound
		}

		stateBytes := bucket.Get(chanIDCopy)
		if stateBytes == nil {
			return ErrChannelNotFound
		}

		forwardingPolicy = &graphmodels.ForwardingPolicy{
			MinHTLCOut: lnwire.MilliSatoshi(
				byteOrder.Uint64(stateBytes[:8]),
			),
			MaxHTLC: lnwire.MilliSatoshi(
				byteOrder.Uint64(stateBytes[8:16]),
			),
			BaseFee: lnwire.MilliSatoshi(
				byteOrder.Uint64(stateBytes[16:24]),
			),
			FeeRate: lnwire.MilliSatoshi(
				byteOrder.Uint64(stateBytes[24:32]),
			),
			TimeLockDelta: byteOrder.Uint32(stateBytes[32:36]),
		}

		return nil
	}, func() {
		forwardingPolicy = nil
	})

	return forwardingPolicy, err
}

// DeleteInitialForwardingPolicy removes the forwarding policy for a given
// channel from the database.
func (s *KVStore) DeleteInitialForwardingPolicy(
	chanID lnwire.ChannelID) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(
			initialChannelForwardingPolicyBucket,
		)
		if bucket == nil {
			return ErrChannelNotFound
		}

		return bucket.Delete(chanIDCopy)
	}, func() {})
}
