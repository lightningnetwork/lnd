package channeldb

import "github.com/lightningnetwork/lnd/kvdb"

var (
	// initialChannelFwdingPolicyBucket is the database bucket used to store
	// the forwarding policy for each permanent channel that is currently
	// in the process of being opened.
	initialChannelFwdingPolicyBucket = []byte("initialChannelFwdingPolicy")
)

// SaveInitialFwdingPolicy saves the serialized forwarding policy for the
// provided permanent channel id to the initialChannelFwdingPolicyBucket.
func (c *ChannelStateDB) SaveInitialFwdingPolicy(chanID,
	forwardingPolicy []byte) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(
			initialChannelFwdingPolicyBucket,
		)
		if err != nil {
			return err
		}

		return bucket.Put(chanID, forwardingPolicy)
	}, func() {})
}

// GetInitialFwdingPolicy fetches the serialized forwarding policy for the
// provided channel id from the database, or returns ErrChannelNotFound if
// a forwarding policy for this channel id is not found.
func (c *ChannelStateDB) GetInitialFwdingPolicy(chanID []byte) ([]byte, error) {
	var serializedState []byte
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(initialChannelFwdingPolicyBucket)
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

// DeleteInitialFwdingPolicy removes the forwarding policy for a given channel
// from the database.
func (c *ChannelStateDB) DeleteInitialFwdingPolicy(chanID []byte) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(initialChannelFwdingPolicyBucket)
		if bucket == nil {
			return ErrChannelNotFound
		}

		return bucket.Delete(chanID)
	}, func() {})
}
