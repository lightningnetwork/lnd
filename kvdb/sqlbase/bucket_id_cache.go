//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import "sync"

// maxCachedBucketIDs is the maximum number of bucket ids cached within a
// single transaction. Transactions that resolve the same buckets over and
// over only touch a handful of distinct buckets, whereas a transaction that
// walks a large number of buckets, e.g. all payments, has nothing to gain
// from the cache, so we bound it rather than let it grow with the database.
const maxCachedBucketIDs = 10_000

// bucketIDKey identifies a nested bucket by its parent bucket id and key.
type bucketIDKey struct {
	// parentID is the id of the parent bucket. It is only meaningful if
	// hasParent is true, otherwise the bucket is a top level bucket.
	parentID  int64
	hasParent bool

	key string
}

// newBucketIDKey creates the cache key for the bucket with the given key in
// the bucket identified by parentID. A nil parentID refers to the root
// bucket.
func newBucketIDKey(parentID *int64, key []byte) bucketIDKey {
	if parentID == nil {
		return bucketIDKey{key: string(key)}
	}

	return bucketIDKey{
		parentID:  *parentID,
		hasParent: true,
		key:       string(key),
	}
}

// bucketIDCache caches the ids of nested buckets looked up within a single
// transaction.
//
// Resolving a nested bucket costs a round trip to the database, whereas with
// bbolt it is an in-memory lookup. Callers are written with the bbolt cost in
// mind and resolve the same bucket path over and over within a transaction,
// btcwallet for example does so for every address and account it reads.
//
// Within a transaction, the id of a bucket can only change through that
// transaction's own writes, as it reads from a single snapshot. Creating a
// bucket adds it to the cache. Deleting a bucket cascades to an unknown set of
// sub-buckets, and SQLite may hand out the ids of deleted buckets again, so a
// delete clears the whole cache. Values can't replace buckets or be replaced
// by them, so writing values never affects cached ids.
type bucketIDCache struct {
	// ids maps a bucket to its id. A nil id records that the bucket does
	// not exist.
	ids map[bucketIDKey]*int64

	// mu guards ids. Transactions aren't meant to be shared between
	// goroutines, but the underlying sql.Tx can be, and unguarded
	// concurrent map access would be a fatal error rather than a failed
	// query.
	mu sync.Mutex
}

// newBucketIDCache creates an empty bucket id cache.
func newBucketIDCache() *bucketIDCache {
	return &bucketIDCache{
		ids: make(map[bucketIDKey]*int64),
	}
}

// get returns the cached id of the bucket with the given key in the given
// parent bucket. The returned id is nil if the bucket is known not to exist,
// and ok is false if nothing is cached for the bucket.
func (c *bucketIDCache) get(parentID *int64, key []byte) (*int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok := c.ids[newBucketIDKey(parentID, key)]

	return id, ok
}

// put records the id of the bucket with the given key in the given parent
// bucket. A nil id records that the bucket does not exist. Once the cache is
// full, only buckets that are already cached are updated.
func (c *bucketIDCache) put(parentID *int64, key []byte, id *int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cacheKey := newBucketIDKey(parentID, key)
	if _, ok := c.ids[cacheKey]; !ok && len(c.ids) >= maxCachedBucketIDs {
		return
	}

	c.ids[cacheKey] = id
}

// clear drops all cached bucket ids.
func (c *bucketIDCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.ids)
}
