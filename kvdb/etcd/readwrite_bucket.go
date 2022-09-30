//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"strconv"

	"github.com/btcsuite/btcwallet/walletdb"
)

// readWriteBucket stores the bucket id and the buckets transaction.
type readWriteBucket struct {
	// id is used to identify the bucket and is created by
	// hashing the parent id with the bucket key. For each key/value,
	// sub-bucket or the bucket sequence the bucket id is used with the
	// appropriate prefix to prefix the key.
	id []byte

	// key is the bucket key.
	key []byte

	// tx holds the parent transaction.
	tx *readWriteTx
}

// newReadWriteBucket creates a new rw bucket with the passed transaction
// and bucket id.
func newReadWriteBucket(tx *readWriteTx, key, id []byte) *readWriteBucket {
	return &readWriteBucket{
		id:  id,
		key: key,
		tx:  tx,
	}
}

// NestedReadBucket retrieves a nested read bucket with the given key.
// Returns nil if the bucket does not exist.
func (b *readWriteBucket) NestedReadBucket(key []byte) walletdb.ReadBucket {
	return b.NestedReadWriteBucket(key)
}

// ForEach invokes the passed function with every key/value pair in
// the bucket. This includes nested buckets, in which case the value
// is nil, but it does not include the key/value pairs within those
// nested buckets.
func (b *readWriteBucket) ForEach(cb func(k, v []byte) error) error {
	prefix := string(b.id)

	// Get the first matching key that is in the bucket.
	kv, err := b.tx.stm.First(prefix)
	if err != nil {
		return err
	}

	for kv != nil {
		key, val := getKeyVal(kv)

		if err := cb(key, val); err != nil {
			return err
		}

		// Step to the next key.
		kv, err = b.tx.stm.Next(prefix, kv.key)
		if err != nil {
			return err
		}
	}

	return nil
}

// ForAll is an optimized version of ForEach for the case when we know we will
// fetch all (or almost all) items.
//
// NOTE: ForAll differs from ForEach in that no additional queries can
// be executed within the callback.
func (b *readWriteBucket) ForAll(cb func(k, v []byte) error) error {
	// When we opened this bucket, we fetched the bucket key using the STM
	// which put a revision "lock" in the read set. We can leverage this
	// by incrementing the revision on the bucket, making any transaction
	// retry that'd touch this same bucket. This way we can safely read all
	// keys from the bucket and not cache them in the STM.
	// To increment the bucket's revision, we simply put in the bucket key
	// value again (which is idempotent if the bucket has just been created).
	b.tx.stm.Put(string(b.key), string(b.id))

	// TODO(bhandras): page size should be configurable in ForAll.
	return b.tx.stm.FetchRangePaginatedRaw(
		string(b.id), 1000,
		func(kv KV) error {
			key, val := getKeyVal(&kv)
			return cb(key, val)
		},
	)
}

// Get returns the value for the given key. Returns nil if the key does
// not exist in this bucket.
func (b *readWriteBucket) Get(key []byte) []byte {
	// Return nil if the key is empty.
	if len(key) == 0 {
		return nil
	}

	// Fetch the associated value.
	val, err := b.tx.stm.Get(string(makeValueKey(b.id, key)))
	if err != nil {
		// TODO: we should return the error once the
		// kvdb inteface is extended.
		return nil
	}

	if val == nil {
		return nil
	}

	return val
}

func (b *readWriteBucket) ReadCursor() walletdb.ReadCursor {
	return newReadWriteCursor(b)
}

// NestedReadWriteBucket retrieves a nested bucket with the given key.
// Returns nil if the bucket does not exist.
func (b *readWriteBucket) NestedReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	if len(key) == 0 {
		return nil
	}

	// Get the bucket id (and return nil if bucket doesn't exist).
	bucketKey := makeBucketKey(b.id, key)
	bucketVal, err := b.tx.stm.Get(string(bucketKey))
	if err != nil {
		// TODO: we should return the error once the
		// kvdb inteface is extended.
		return nil
	}

	if !isValidBucketID(bucketVal) {
		return nil
	}

	// Return the bucket with the fetched bucket id.
	return newReadWriteBucket(b.tx, bucketKey, bucketVal)
}

// assertNoValue checks if the value for the passed key exists.
func (b *readWriteBucket) assertNoValue(key []byte) error {
	if !etcdDebug {
		return nil
	}

	val, err := b.tx.stm.Get(string(makeValueKey(b.id, key)))
	if err != nil {
		return err
	}

	if val != nil {
		return walletdb.ErrIncompatibleValue
	}

	return nil
}

// assertNoBucket checks if the bucket for the passed key exists.
func (b *readWriteBucket) assertNoBucket(key []byte) error {
	if !etcdDebug {
		return nil
	}

	val, err := b.tx.stm.Get(string(makeBucketKey(b.id, key)))
	if err != nil {
		return err
	}

	if val != nil {
		return walletdb.ErrIncompatibleValue
	}

	return nil
}

// CreateBucket creates and returns a new nested bucket with the given
// key. Returns ErrBucketExists if the bucket already exists,
// ErrBucketNameRequired if the key is empty, or ErrIncompatibleValue
// if the key value is otherwise invalid for the particular database
// implementation.  Other errors are possible depending on the
// implementation.
func (b *readWriteBucket) CreateBucket(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	// Check if the bucket already exists.
	bucketKey := makeBucketKey(b.id, key)

	bucketVal, err := b.tx.stm.Get(string(bucketKey))
	if err != nil {
		return nil, err
	}

	if isValidBucketID(bucketVal) {
		return nil, walletdb.ErrBucketExists
	}

	if err := b.assertNoValue(key); err != nil {
		return nil, err
	}

	// Create a deterministic bucket id from the bucket key.
	newID := makeBucketID(bucketKey)

	// Create the bucket.
	b.tx.stm.Put(string(bucketKey), string(newID[:]))

	return newReadWriteBucket(b.tx, bucketKey, newID[:]), nil
}

// CreateBucketIfNotExists creates and returns a new nested bucket with
// the given key if it does not already exist.  Returns
// ErrBucketNameRequired if the key is empty or ErrIncompatibleValue
// if the key value is otherwise invalid for the particular database
// backend.  Other errors are possible depending on the implementation.
func (b *readWriteBucket) CreateBucketIfNotExists(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	// Check for the bucket and create if it doesn't exist.
	bucketKey := makeBucketKey(b.id, key)

	bucketVal, err := b.tx.stm.Get(string(bucketKey))
	if err != nil {
		return nil, err
	}

	if !isValidBucketID(bucketVal) {
		if err := b.assertNoValue(key); err != nil {
			return nil, err
		}

		newID := makeBucketID(bucketKey)
		b.tx.stm.Put(string(bucketKey), string(newID[:]))

		return newReadWriteBucket(b.tx, bucketKey, newID[:]), nil
	}

	// Otherwise return the bucket with the fetched bucket id.
	return newReadWriteBucket(b.tx, bucketKey, bucketVal), nil
}

// DeleteNestedBucket deletes the nested bucket and its sub-buckets
// pointed to by the passed key. All values in the bucket and sub-buckets
// will be deleted as well.
func (b *readWriteBucket) DeleteNestedBucket(key []byte) error {
	// TODO shouldn't empty key return ErrBucketNameRequired ?
	if len(key) == 0 {
		return walletdb.ErrIncompatibleValue
	}

	// Get the bucket first.
	bucketKey := string(makeBucketKey(b.id, key))

	bucketVal, err := b.tx.stm.Get(bucketKey)
	if err != nil {
		return err
	}

	if !isValidBucketID(bucketVal) {
		return walletdb.ErrBucketNotFound
	}

	// Enqueue the top level bucket id.
	queue := [][]byte{bucketVal}

	// Traverse the buckets breadth first.
	for len(queue) != 0 {
		if !isValidBucketID(queue[0]) {
			return walletdb.ErrBucketNotFound
		}

		id := queue[0]
		queue = queue[1:]

		kv, err := b.tx.stm.First(string(id))
		if err != nil {
			return err
		}

		for kv != nil {
			b.tx.stm.Del(kv.key)

			if isBucketKey(kv.key) {
				queue = append(queue, []byte(kv.val))
			}

			kv, err = b.tx.stm.Next(string(id), kv.key)
			if err != nil {
				return err
			}
		}

		// Finally delete the sequence key for the bucket.
		b.tx.stm.Del(string(makeSequenceKey(id)))
	}

	// Delete the top level bucket and sequence key.
	b.tx.stm.Del(bucketKey)
	b.tx.stm.Del(string(makeSequenceKey(bucketVal)))

	return nil
}

// Put updates the value for the passed key.
// Returns ErrKeyRequired if the passed key is empty.
func (b *readWriteBucket) Put(key, value []byte) error {
	if len(key) == 0 {
		return walletdb.ErrKeyRequired
	}

	if err := b.assertNoBucket(key); err != nil {
		return err
	}

	// Update the transaction with the new value.
	b.tx.stm.Put(string(makeValueKey(b.id, key)), string(value))

	return nil
}

// Delete deletes the key/value pointed to by the passed key.
// Returns ErrKeyRequired if the passed key is empty.
func (b *readWriteBucket) Delete(key []byte) error {
	if key == nil {
		return nil
	}
	if len(key) == 0 {
		return walletdb.ErrKeyRequired
	}

	// Update the transaction to delete the key/value.
	b.tx.stm.Del(string(makeValueKey(b.id, key)))

	return nil
}

// ReadWriteCursor returns a new read-write cursor for this bucket.
func (b *readWriteBucket) ReadWriteCursor() walletdb.ReadWriteCursor {
	return newReadWriteCursor(b)
}

// Tx returns the buckets transaction.
func (b *readWriteBucket) Tx() walletdb.ReadWriteTx {
	return b.tx
}

// NextSequence returns an auto-incrementing sequence number for this bucket.
// Note that this is not a thread safe function and as such it must not be used
// for synchronization.
func (b *readWriteBucket) NextSequence() (uint64, error) {
	seq := b.Sequence() + 1

	return seq, b.SetSequence(seq)
}

// SetSequence updates the sequence number for the bucket.
func (b *readWriteBucket) SetSequence(v uint64) error {
	// Convert the number to string.
	val := strconv.FormatUint(v, 10)

	// Update the transaction with the new value for the sequence key.
	b.tx.stm.Put(string(makeSequenceKey(b.id)), val)

	return nil
}

// Sequence returns the current sequence number for this bucket without
// incrementing it.
func (b *readWriteBucket) Sequence() uint64 {
	val, err := b.tx.stm.Get(string(makeSequenceKey(b.id)))
	if err != nil {
		// TODO: This update kvdb interface such that error
		// may be returned here.
		return 0
	}

	if val == nil {
		// If the sequence number is not yet
		// stored, then take the default value.
		return 0
	}

	// Otherwise try to parse a 64-bit unsigned integer from the value.
	num, _ := strconv.ParseUint(string(val), 10, 64)

	return num
}

func flattenMap(m map[string]struct{}) []string {
	result := make([]string, len(m))
	i := 0

	for key := range m {
		result[i] = key
		i++
	}

	return result
}

// Prefetch will prefetch all keys in the passed-in paths as well as all bucket
// keys along the paths.
func (b *readWriteBucket) Prefetch(paths ...[]string) {
	keys := make(map[string]struct{})
	ranges := make(map[string]struct{})

	for _, path := range paths {
		parent := b.id
		for _, bucket := range path {
			bucketKey := makeBucketKey(parent, []byte(bucket))
			keys[string(bucketKey[:])] = struct{}{}

			id := makeBucketID(bucketKey)
			parent = id[:]
		}

		ranges[string(parent)] = struct{}{}
	}

	b.tx.stm.Prefetch(flattenMap(keys), flattenMap(ranges))
}
