package channeldb

import "github.com/lightningnetwork/lnd/kvdb"

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
func (c *ChannelStateDB) SaveInitialForwardingPolicy(chanID,
	forwardingPolicy []byte) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(
			initialChannelForwardingPolicyBucket,
		)
		if err != nil {
			return err
		}

		return bucket.Put(chanID, forwardingPolicy)
	}, func() {})
}

// GetInitialForwardingPolicy fetches the serialized forwarding policy for the
// provided channel id from the database, or returns ErrChannelNotFound if
// a forwarding policy for this channel id is not found.
func (c *ChannelStateDB) GetInitialForwardingPolicy(chanID []byte) ([]byte,
	error) {

	var serializedState []byte
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(initialChannelForwardingPolicyBucket)
		if bucket == nil {
			// If the bucket does not exist, it means we
			// never added a channel fees to the db, so
			// return ErrChannelNotFound.
			return ErrChannelNotFound
		}

		stateBytes := bucket.Get(chanID)
		if stateBytes == nil {
			return ErrChannelNotFound
		}

		serializedState = append(serializedState, stateBytes...)

		return nil
	}, func() {
		serializedState = nil
	})
	return serializedState, err
}

// DeleteInitialForwardingPolicy removes the forwarding policy for a given
// channel from the database.
func (c *ChannelStateDB) DeleteInitialForwardingPolicy(chanID []byte) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(
			initialChannelForwardingPolicyBucket,
		)
		if bucket == nil {
			return ErrChannelNotFound
		}

		return bucket.Delete(chanID)
	}, func() {})
}
