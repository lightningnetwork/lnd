package chanstate

import "github.com/lightningnetwork/lnd/kvdb"

var (
	// channelOpeningStateBucket is the database bucket used to store the
	// channelOpeningState for each channel that is currently in the process
	// of being opened.
	channelOpeningStateBucket = []byte("channelOpeningState")
)

// ChannelOpeningStateBucketKey returns the top-level bucket key used to store
// serialized channel opening state.
func ChannelOpeningStateBucketKey() []byte {
	return channelOpeningStateBucket
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

// DeleteChannelOpeningState removes any state for outPoint from the database.
func DeleteChannelOpeningState(tx kvdb.RwTx, outPoint []byte) error {
	bucket := tx.ReadWriteBucket(channelOpeningStateBucket)
	if bucket == nil {
		return ErrChannelNotFound
	}

	return bucket.Delete(outPoint)
}
