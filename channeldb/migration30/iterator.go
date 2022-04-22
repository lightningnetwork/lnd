package migration30

import (
	"bytes"
	"errors"
	"fmt"

	mig25 "github.com/lightningnetwork/lnd/channeldb/migration25"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	openChannelBucket = []byte("open-chan-bucket")

	// errExit is returned when the callback function used in iterator
	// needs to exit the iteration.
	errExit = errors.New("exit condition met")
)

// updateLocator defines a locator that can be used to find the next record to
// be migrated. This is useful when an interrupted migration that leads to a
// mixed revocation log formats saved in our database, we can then restart the
// migration using the locator to continue migrating the rest.
type updateLocator struct {
	// nodePub, chainHash and fundingOutpoint are used to locate the
	// channel bucket.
	nodePub         []byte
	chainHash       []byte
	fundingOutpoint []byte

	// nextHeight is used to locate the next old revocation log to be
	// migrated. A nil value means we've finished the migration.
	nextHeight []byte
}

// fetchChanBucket is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on.
func (ul *updateLocator) locateChanBucket(rootBucket kvdb.RwBucket) (
	kvdb.RwBucket, error) {

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodeChanBucket := rootBucket.NestedReadWriteBucket(ul.nodePub)
	if nodeChanBucket == nil {
		return nil, mig25.ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadWriteBucket(ul.chainHash)
	if chainBucket == nil {
		return nil, mig25.ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	chanBucket := chainBucket.NestedReadWriteBucket(ul.fundingOutpoint)
	if chanBucket == nil {
		return nil, mig25.ErrChannelNotFound
	}

	return chanBucket, nil
}

// findNextMigrateHeight finds the next commit height that's not migrated. It
// returns the commit height bytes found. A nil return value means the
// migration has been completed for this particular channel bucket.
func findNextMigrateHeight(chanBucket kvdb.RwBucket) ([]byte, error) {
	// Read the old log bucket. The old bucket doesn't exist, indicating
	// either we don't have any old logs for this channel, or the migration
	// has been finished and the old bucket has been deleted.
	oldBucket := chanBucket.NestedReadBucket(
		revocationLogBucketDeprecated,
	)
	if oldBucket == nil {
		return nil, nil
	}

	// Acquire a read cursor for the old bucket.
	oldCursor := oldBucket.ReadCursor()

	// Read the new log bucket. The sub-bucket hasn't been created yet,
	// indicating we haven't migrated any logs under this channel. In this
	// case, we'll return the first commit height found from the old
	// revocation log bucket as the next height.
	logBucket := chanBucket.NestedReadBucket(revocationLogBucket)
	if logBucket == nil {
		nextHeight, _ := oldCursor.First()
		return nextHeight, nil
	}

	// Acquire a read cursor for the new bucket.
	cursor := logBucket.ReadCursor()

	// Read the last migrated record. If the key is nil, we haven't
	// migrated any logs yet. In this case we return the first commit
	// height found from the old revocation log bucket.
	migratedHeight, _ := cursor.Last()
	if migratedHeight == nil {
		nextHeight, _ := oldCursor.First()
		return nextHeight, nil
	}

	// Read the last height from the old log bucket. If the height of the
	// last old revocation equals to the migrated height, we've done
	// migrating for this channel.
	endHeight, _ := oldCursor.Last()
	if bytes.Equal(migratedHeight, endHeight) {
		return nil, nil
	}

	// Now point the cursor to the migratedHeight. If we cannot find this
	// key from the old log bucket, the database might be corrupted. In
	// this case, we would return the first key so that we would redo the
	// migration for this chan bucket.
	matchedHeight, _ := oldCursor.Seek(migratedHeight)
	if matchedHeight == nil {
		// Now return the first height found in the old bucket so we
		// can redo the migration.
		nextHeight, _ := oldCursor.First()
		return nextHeight, nil
	}

	// Otherwise, find the next height to be migrated.
	nextHeight, _ := oldCursor.Next()

	return nextHeight, nil
}

// locateNextUpdateNum returns a locator that's used to start our migration. A
// nil locator means the migration has been finished.
func locateNextUpdateNum(openChanBucket kvdb.RwBucket) (*updateLocator, error) {
	locator := &updateLocator{}

	// cb is the callback function to be used when iterating the buckets.
	cb := func(chanBucket kvdb.RwBucket, l *updateLocator) error {
		locator = l

		updateNum, err := findNextMigrateHeight(chanBucket)
		if err != nil {
			return err
		}

		// We've found the next commit height and can now exit.
		if updateNum != nil {
			locator.nextHeight = updateNum
			return errExit
		}
		return nil
	}

	// Iterate the buckets. If we received an exit signal, return the
	// locator.
	err := iterateBuckets(openChanBucket, nil, cb)
	if err == errExit {
		log.Debugf("found locator: nodePub=%x, fundingOutpoint=%x, "+
			"nextHeight=%x", locator.nodePub, locator.chainHash,
			locator.nextHeight)
		return locator, nil
	}

	// If the err is nil, we've iterated all the sub-buckets and the
	// migration is finished.
	return nil, err
}

// callback defines a type that's used by the iterator.
type callback func(k, v []byte) error

// iterator is a helper function that iterates a given bucket and performs the
// callback function on each key. If a seeker is specified, it will move the
// cursor to the given position otherwise it will start from the first item.
func iterator(bucket kvdb.RBucket, seeker []byte, cb callback) error {
	c := bucket.ReadCursor()
	k, v := c.First()

	// Move the cursor to the specified position if seeker is non-nil.
	if seeker != nil {
		k, v = c.Seek(seeker)
	}

	// Start the iteration and exit on condition.
	for k, v := k, v; k != nil; k, v = c.Next() {
		// cb might return errExit to signal exiting the iteration.
		if err := cb(k, v); err != nil {
			return err
		}
	}
	return nil
}

// step defines the callback type that's used when iterating the buckets.
type step func(bucket kvdb.RwBucket, l *updateLocator) error

// iterateBuckets locates the cursor at a given position specified by the
// updateLocator and starts the iteration. If a nil locator is passed, it will
// start the iteration from the beginning. During each iteration, the callback
// function is called and it may exit the iteration when the callback returns
// an errExit to signal an exit condition.
func iterateBuckets(openChanBucket kvdb.RwBucket,
	l *updateLocator, cb step) error {

	// If the locator is nil, we will initiate an empty one, which is
	// further used by the iterator.
	if l == nil {
		l = &updateLocator{}
	}

	// iterChanBucket iterates the chain bucket to act on each of the
	// channel buckets.
	iterChanBucket := func(chain kvdb.RwBucket,
		k1, k2, _ []byte, cb step) error {

		return iterator(
			chain, l.fundingOutpoint,
			func(k3, _ []byte) error {
				// Read the sub-bucket level 3.
				chanBucket := chain.NestedReadWriteBucket(k3)
				if chanBucket == nil {
					return fmt.Errorf("no bucket for "+
						"chanPoint=%x", k3)
				}

				// Construct a new locator at this position.
				locator := &updateLocator{
					nodePub:         k1,
					chainHash:       k2,
					fundingOutpoint: k3,
				}

				// Set the seeker to nil so it won't affect
				// other buckets.
				l.fundingOutpoint = nil

				return cb(chanBucket, locator)
			})
	}

	return iterator(openChanBucket, l.nodePub, func(k1, v []byte) error {
		// Read the sub-bucket level 1.
		node := openChanBucket.NestedReadWriteBucket(k1)
		if node == nil {
			return fmt.Errorf("no bucket for node %x", k1)
		}

		return iterator(node, l.chainHash, func(k2, v []byte) error {
			// Read the sub-bucket level 2.
			chain := node.NestedReadWriteBucket(k2)
			if chain == nil {
				return fmt.Errorf("no bucket for chain=%x", k2)
			}

			// Set the seeker to nil so it won't affect other
			// buckets.
			l.chainHash = nil

			return iterChanBucket(chain, k1, k2, v, cb)
		})
	})
}
