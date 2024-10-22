package channeldb

import (
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// initialChannelForwardingPolicyBucket is the database bucket used to
	// store the forwarding policy for each permanent channel that is
	// currently in the process of being opened.
	initialChannelForwardingPolicyBucket = []byte(
		"initialChannelFwdingPolicy",
	)
)

// SaveInitialForwardingPolicy saves the serialized forwarding policy for the
// provided permanent channel id to the initialChannelForwardingPolicyBucket.
func (c *ChannelStateDB) SaveInitialForwardingPolicy(chanID lnwire.ChannelID,
	forwardingPolicy *models.ForwardingPolicy) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	scratch := make([]byte, 36)
	byteOrder.PutUint64(scratch[:8], uint64(forwardingPolicy.MinHTLCOut))
	byteOrder.PutUint64(scratch[8:16], uint64(forwardingPolicy.MaxHTLC))
	byteOrder.PutUint64(scratch[16:24], uint64(forwardingPolicy.BaseFee))
	byteOrder.PutUint64(scratch[24:32], uint64(forwardingPolicy.FeeRate))
	byteOrder.PutUint32(scratch[32:], forwardingPolicy.TimeLockDelta)

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
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
func (c *ChannelStateDB) GetInitialForwardingPolicy(
	chanID lnwire.ChannelID) (*models.ForwardingPolicy, error) {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	var forwardingPolicy *models.ForwardingPolicy
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
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

		forwardingPolicy = &models.ForwardingPolicy{
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
func (c *ChannelStateDB) DeleteInitialForwardingPolicy(
	chanID lnwire.ChannelID) error {

	chanIDCopy := make([]byte, 32)
	copy(chanIDCopy, chanID[:])

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(
			initialChannelForwardingPolicyBucket,
		)
		if bucket == nil {
			return ErrChannelNotFound
		}

		return bucket.Delete(chanIDCopy)
	}, func() {})
}
