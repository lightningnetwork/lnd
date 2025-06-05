package migration1

import (
	"github.com/lightningnetwork/lnd/kvdb"
)

var (

	// sharedHashBucket is a bucket which houses the first HashPrefixSize
	// bytes of a received HTLC's hashed shared secret as the key and the
	// HTLC's CLTV expiry as the value.
	sharedHashBucket = []byte("shared-hash")

	// batchReplayBucket is a bucket that maps batch identifiers to
	// serialized ReplaySets. This is used to give idempotency in the event
	// that a batch is processed more than once.
	batchReplayBucket = []byte("batch-replay")
)

// Migrate is a function that migrates the decayed log database from version 0
// to version 1.
//
// NOTE: We drop both tables because the cost of migrating existing decayed
// log entries is too high. Instead we are going to drop the tables and make
// sure we do garbage collect the whole db after the locktime for HTLCs expires.
func Migrate(tx kvdb.RwTx) error {
	log.Infof("Deleting top-level bucket: %x ...", sharedHashBucket)
	err := tx.DeleteTopLevelBucket(sharedHashBucket)
	if err != nil && err != kvdb.ErrBucketNotFound {
		return err
	}
	log.Infof("Deleted top-level bucket: %x", sharedHashBucket)

	log.Infof("Deleting top-level bucket: %x ...", batchReplayBucket)
	err = tx.DeleteTopLevelBucket(batchReplayBucket)
	if err != nil && err != kvdb.ErrBucketNotFound {
		return err
	}
	log.Infof("Deleted top-level bucket: %x", batchReplayBucket)

	return nil
}
