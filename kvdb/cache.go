package kvdb

import (
	"container/list"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/google/btree"
)

const (
	// treeDeg is the degree of the B-trees we use in our cached buckets.
	treeDeg = 3
)

// cacheBucket is a bucket we use for storing cached items.
type cacheBucket struct {
	seq  *uint64
	tree *btree.BTree
}

// newReadThroughCacheBucket creates a cacheBucket that doesn't hold any data
// and is meant to work as a placeholder for top level buckets that we'll only
// allow on-demand read through (fetching all data from the DB every time).
func newReadThroughCacheBucket() *cacheBucket {
	return &cacheBucket{}
}

// newCacheBucket creates a new cacheBucket.
func newCacheBucket() *cacheBucket {
	return &cacheBucket{
		tree: btree.New(treeDeg),
	}
}

// cached returns true if this cacheBucket is actually cached.
func (c *cacheBucket) cached() bool {
	return c.tree != nil
}

// get returns the corresponding cachedItem for the key if there's any or nil
// otherwise.
func (c *cacheBucket) get(key []byte) *cachedItem {
	if key == nil {
		return nil
	}

	keyItem := &cachedItem{
		key: string(key),
	}

	if valItem := c.tree.Get(keyItem); valItem != nil {
		return valItem.(*cachedItem)
	}

	return nil
}

// put inserts or replaces the passed item.
func (c *cacheBucket) put(item *cachedItem) {
	c.tree.ReplaceOrInsert(item)
}

// del removes the passed item.
func (c *cacheBucket) del(item *cachedItem) {
	c.tree.Delete(item)
}

// cachedItem is holder for key/values or buckets that we store in the cache.
type cachedItem struct {
	key   string
	value string

	// bucket is non nil if this item is a bucket.
	bucket *cacheBucket
}

// Less implements a strict ordering operator in order to insert items into the
// cache's b-tree.
func (c *cachedItem) Less(than btree.Item) bool {
	return c.key < than.(*cachedItem).key
}

// pendingChange is a common interface for all pending changes to the cache.
type pendingChange interface {
	// Reverts the pending change.
	Revert()
}

// pendingAdd holds a new key/value/bucket added to the cache.
type pendingAdd struct {
	parent   *cachedItem
	newChild *cachedItem
}

// Revert reverts the cache to the state before the add.
func (p *pendingAdd) Revert() {
	p.parent.bucket.del(p.newChild)
}

// pendingUpdate holds an updated value.
type pendingUpdate struct {
	parent   *cachedItem
	oldValue string
}

// Revert reverts the cache to the state before the update.
func (p *pendingUpdate) Revert() {
	p.parent.value = p.oldValue
}

// pendingDelete holds a pending deleted key/value/bucket.
type pendingDelete struct {
	parent   *cachedItem
	oldChild *cachedItem
}

// Revert reverts back the cache to the state before the delete.
func (p *pendingDelete) Revert() {
	p.parent.bucket.put(p.oldChild)
}

// Cache is a simple write through cache implementing the kvdb.Backend
// interface. It's capable of recursively caching top-level buckets speeding up
// reads by reducing roundtrips to the actual DB backend while remaining
// consistend with DB state as long as the cache is used when mutating those
// buckets. It's also able to skip buckets in the tree structure keeping thos
// read/write-through, which is useful when we want to skip large buckets.
type Cache struct {
	mx sync.RWMutex

	// skipped tracks buckets that the cache tracks, but never fetches.
	skipped map[string]bool

	// topLevelBuckets stores prefetched top-level buckets.
	topLevelBuckets []string

	// currRwTx holds the current RW DB transaction.
	currRwTx *cacheReadWriteTx

	// root tracks the cache's top-level buckets and the buckets underneath
	// them.
	root *cachedItem

	// pending holds any pending changes before the transaction commit.
	pending *list.List

	// backend it the underlying DB backend.
	backend Backend
}

// Enforce that Cache implements the ExtendedBackend interface.
var _ walletdb.DB = (*Cache)(nil)

// NewCache constructs a new cache. Top level buckets are recursively read and
// all content is added to the cache. Skipped keys will be skipped on all levels.
func NewCache(backend Backend, topLevelBuckets [][]byte,
	skippedKeys [][]byte) *Cache {

	cache := &Cache{
		skipped:         make(map[string]bool),
		topLevelBuckets: make([]string, len(topLevelBuckets)),
		root: &cachedItem{
			bucket: newCacheBucket(),
		},
		pending: list.New(),
		backend: backend,
	}

	for _, skippedKey := range skippedKeys {
		cache.skipped[string(skippedKey)] = true
	}

	for i, bucket := range topLevelBuckets {
		cache.topLevelBuckets[i] = string(bucket)
	}

	return cache
}

// pendingAdd adds a new item to the parent.
func (c *Cache) pendingAdd(parent *cachedItem, newChild *cachedItem) {
	c.pending.PushBack(&pendingAdd{parent, newChild})
}

// pendingUpdate updates parent with a new value.
func (c *Cache) pendingUpdate(parent *cachedItem, oldValue string) {
	c.pending.PushBack(&pendingUpdate{parent, oldValue})
}

// pendingDelete deletes the child form the parent.
func (c *Cache) pendingDelete(parent *cachedItem, oldChild *cachedItem) {
	c.pending.PushBack(&pendingDelete{parent, oldChild})
}

// traversalHelper is just a struct we use to help the recursive traversal of
// top-level  buckets when they're added to the cache.
type traversalHelper struct {
	bucket walletdb.ReadBucket
	root   *cachedItem
}

// scanBucket scans and adds a bucket and its sub-buckets recursively to the
// cache.
func (c *Cache) scanBucket(bucket walletdb.ReadBucket, root *cachedItem) error {
	var queue []*traversalHelper
	currRoot := root

	for {
		err := bucket.ForEach(func(k, v []byte) error {
			item := &cachedItem{
				key: string(k),
			}

			// This is a value, fetch it.
			if v != nil {
				item.value = string(v)
				currRoot.bucket.put(item)
				return nil
			}

			// This is a bucket.
			if c.skipped[string(k)] {
				// Bucket is read-through, no need to fetch it.
				item.bucket = newReadThroughCacheBucket()
			} else {
				// We cache this bucket and its contents.
				item.bucket = newCacheBucket()

				bucket := bucket.NestedReadBucket(k)
				queue = append(
					queue,
					&traversalHelper{
						bucket: bucket,
						root:   item,
					},
				)

			}
			currRoot.bucket.put(item)

			return nil
		})

		if err != nil {
			return err
		}

		if len(queue) == 0 {
			break
		}

		bucket = queue[0].bucket
		currRoot = queue[0].root
		queue[0] = nil
		queue = queue[1:]
	}

	return nil
}

// addTopLevelBucket recursively reads the passed top-level bucket and adds
// all content below it to the cache.
func (c *Cache) addTopLevelBucket(key []byte) error {
	if c.skipped[string(key)] {
		c.root.bucket.put(&cachedItem{
			key:    string(key),
			bucket: newReadThroughCacheBucket(),
		})

		return nil
	}

	var root *cachedItem

	if err := View(c.backend, func(tx RTx) error {
		bucket := tx.ReadBucket(key)
		if bucket == nil {
			return nil
		}

		root = &cachedItem{
			key:    string(key),
			bucket: newCacheBucket(),
		}

		return c.scanBucket(bucket, root)
	}, func() {}); err != nil {
		return err
	}

	if root != nil {
		c.root.bucket.put(root)
	}

	return nil
}

// Wipe wipes the cache state.
func (c *Cache) Wipe() {
	c.mx.Lock()
	defer c.mx.Unlock()

	c.root = &cachedItem{
		bucket: newCacheBucket(),
	}
}

// Init refetches the tracked top-level buckets.
func (c *Cache) Init() error {
	c.mx.Lock()
	defer c.mx.Unlock()

	for _, bucket := range c.topLevelBuckets {
		if err := c.addTopLevelBucket([]byte(bucket)); err != nil {
			return err
		}
	}

	return nil
}

func (c *Cache) BeginReadTx() (walletdb.ReadTx, error) {
	c.mx.RLock()

	dbTx, err := c.backend.BeginReadTx()
	if err != nil {
		c.mx.RUnlock()
		return nil, err
	}

	return newCacheReadTx(c, dbTx), nil
}

func (c *Cache) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	c.mx.Lock()

	dbTx, err := c.backend.BeginReadWriteTx()
	if err != nil {
		c.mx.Unlock()
		return nil, err
	}

	c.currRwTx = newCacheReadWriteTx(c, dbTx)
	return c.currRwTx, nil
}

func (c *Cache) Copy(w io.Writer) error {
	return fmt.Errorf("unavailable")
}

func (c *Cache) Close() error {
	err := c.backend.Close()
	c.Wipe()
	return err
}

func (c *Cache) PrintStats() string {
	return "<unavailable>"
}

func (c *Cache) View(f func(tx walletdb.ReadTx) error, reset func()) error {
	tx, err := c.BeginReadTx()
	if err != nil {
		return err
	}

	reset()

	err = f(tx)
	rollbackErr := tx.Rollback()

	if err != nil {
		return err
	}

	return rollbackErr
}

func (c *Cache) Update(f func(tx walletdb.ReadWriteTx) error,
	reset func()) error {

	tx, err := c.BeginReadWriteTx()
	if err != nil {
		return err
	}

	reset()

	// Apply the tx closure, rollback on error.
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	// Attempt to commit, rollback on error. Note that since we have
	// exclusive access Commit should only fail with database error and
	// never with any error that we'd normally retry on.
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}

	return nil
}

func (c *Cache) Batch(f func(tx walletdb.ReadWriteTx) error) error {
	return c.Update(f, func() {})
}

type cacheReadTx struct {
	cache  *Cache
	dbTx   walletdb.ReadTx
	active bool
}

var _ walletdb.ReadTx = (*cacheReadTx)(nil)

func newCacheReadTx(cache *Cache, dbTx walletdb.ReadTx) *cacheReadTx {
	return &cacheReadTx{
		cache:  cache,
		dbTx:   dbTx,
		active: true,
	}
}

func topLevelReadBucketImpl(dbTx walletdb.ReadTx,
	cache *Cache, key []byte) walletdb.ReadBucket {

	if root := cache.root.bucket.get(key); root != nil {
		if root.bucket.cached() {
			return newCacheReadBucket(dbTx, nil, cache, root)
		} else {
			// For "read-through" top level buckets we simply
			// return a DB ReadBucket that is independent from the
			// cached state.
			return dbTx.ReadBucket(key)
		}
	}

	return nil
}

func forEachBucketImpl(cache *Cache, f func(key []byte) error) error {
	cache.root.bucket.tree.Ascend(func(item btree.Item) bool {
		c := item.(*cachedItem)
		if f([]byte(c.key)) != nil {
			return false
		}

		return true
	})

	return nil
}

func (c *cacheReadTx) ReadBucket(key []byte) walletdb.ReadBucket {
	return topLevelReadBucketImpl(c.dbTx, c.cache, key)
}

func (c *cacheReadTx) ForEachBucket(f func(key []byte) error) error {
	return forEachBucketImpl(c.cache, f)
}

func (c *cacheReadTx) Rollback() error {
	if c.active {
		defer func() {
			c.active = false
			c.cache.mx.RUnlock()
		}()

		return c.dbTx.Rollback()
	}

	return nil
}

type cacheReadWriteTx struct {
	cache    *Cache
	dbTx     walletdb.ReadWriteTx
	active   bool
	onCommit func()
}

var _ walletdb.ReadWriteTx = (*cacheReadWriteTx)(nil)

func newCacheReadWriteTx(cache *Cache,
	dbTx walletdb.ReadWriteTx) *cacheReadWriteTx {

	return &cacheReadWriteTx{
		cache:  cache,
		dbTx:   dbTx,
		active: true,
	}
}

func (c *cacheReadWriteTx) ReadBucket(key []byte) walletdb.ReadBucket {
	return topLevelReadBucketImpl(c.dbTx, c.cache, key)
}

func (c *cacheReadWriteTx) ForEachBucket(f func(key []byte) error) error {
	return forEachBucketImpl(c.cache, f)
}

func (c *cacheReadWriteTx) Rollback() error {
	if c.active {
		defer func() {
			c.active = false
			c.cache.mx.Unlock()
		}()

		// First revert changes to the cache itself.
		for e := c.cache.pending.Back(); e != nil; e = e.Prev() {
			e.Value.(pendingChange).Revert()
		}

		// Now that we got back the old cache state, we can reset the
		// pending change list and revert the DB transaction.
		c.cache.pending = list.New()
		return c.dbTx.Rollback()
	}

	return nil
}

func (c *cacheReadWriteTx) ReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	root := c.cache.root.bucket.get(key)

	// Bucket is not known.
	if root == nil {
		return nil
	}

	dbBucket := c.dbTx.ReadWriteBucket(key)
	if dbBucket == nil {
		return nil
	}

	// We cache the bucket state.
	if root.bucket.cached() {
		return newCacheReadWriteBucket(c.cache, root, dbBucket)
	}

	// Read-through bucket.
	return dbBucket
}

func (c *cacheReadWriteTx) CreateTopLevelBucket(key []byte) (
	walletdb.ReadWriteBucket, error) {

	// First we need to make sure the DB is able to find/create this top
	// level bucket.
	dbBucket, err := c.dbTx.CreateTopLevelBucket(key)
	if err != nil {
		return nil, err
	}

	// Now check if we already track this bucket in the cache.
	root := c.cache.root.bucket.get(key)
	if root != nil {
		// Bucket is tracked and contents are cached too.
		if root.bucket.cached() {
			return newCacheReadWriteBucket(
				c.cache, root, dbBucket,
			), nil
		}

		// Bucket is tracked but we don't cache the contents.
		return dbBucket, nil
	}

	// Bucket is not yet tracked, we need to add it to the cache.
	root = &cachedItem{
		key:    string(key),
		bucket: newCacheBucket(),
	}

	c.cache.root.bucket.put(root)
	c.cache.pendingAdd(c.cache.root, root)

	return newCacheReadWriteBucket(c.cache, root, dbBucket), nil
}

func (c *cacheReadWriteTx) DeleteTopLevelBucket(key []byte) error {
	if err := c.dbTx.DeleteTopLevelBucket(key); err != nil {
		return err
	}

	return deleteFromCache(c.cache, c.cache.root, key, true)
}

func (c *cacheReadWriteTx) Commit() error {
	if err := c.dbTx.Commit(); err != nil {
		return err
	}

	defer func() {
		c.active = false
		c.cache.mx.Unlock()
	}()

	c.cache.pending = list.New()
	if c.onCommit != nil {
		c.onCommit()
	}

	return nil
}

func (c *cacheReadWriteTx) OnCommit(f func()) {
	c.onCommit = f
}

func forEachImpl(root *cachedItem, f func(k, v []byte) error) error {
	var err error

	root.bucket.tree.Ascend(func(item btree.Item) bool {
		c := item.(*cachedItem)
		var val []byte

		if c.bucket == nil {
			val = []byte(c.value)
		}

		if err = f([]byte(c.key), val); err != nil {
			return false
		}

		return true
	})

	return err
}

func getImpl(root *cachedItem, key []byte) []byte {
	cacheItem := root.bucket.get(key)
	if cacheItem != nil {
		if cacheItem.bucket == nil {
			return []byte(cacheItem.value)
		}

		return nil
	}

	return nil
}

// cacheReadBucket is a walletdb.ReadBucket compatible bucket implementation
// operating on already cached values or reading on demand for skipped
// (read-through) sub buckets.
type cacheReadBucket struct {
	parent *cacheReadBucket
	cache  *Cache
	root   *cachedItem
	dbTx   walletdb.ReadTx

	// dbBucket tracks the DB ReadBucket and is intentionally nil and only
	// fetched if needed.
	dbBucket walletdb.ReadBucket
}

var _ walletdb.ReadBucket = (*cacheReadBucket)(nil)

func newCacheReadBucket(dbTx walletdb.ReadTx, parent *cacheReadBucket,
	cache *Cache, root *cachedItem) *cacheReadBucket {

	return &cacheReadBucket{
		parent: parent,
		cache:  cache,
		root:   root,
		dbTx:   dbTx,
	}
}

// fetchBucet is a helper function to "fetch" all DB buckets up to the top from
// the current one (if not yet fetched). This is necessary when using
// "read-through" buckets. The return value the DB ReadBucket for this
// cacheReadBucket.
func (c *cacheReadBucket) fetchBucket() walletdb.ReadBucket {
	if c.dbBucket != nil {
		return c.dbBucket
	}

	if c.parent != nil {
		c.dbBucket = c.parent.fetchBucket().NestedReadBucket(
			[]byte(c.root.key),
		)
	} else {
		// This is a top level ReadBucket.
		c.dbBucket = c.dbTx.ReadBucket([]byte(c.root.key))
	}

	return c.dbBucket
}

func (c *cacheReadBucket) NestedReadBucket(key []byte) walletdb.ReadBucket {
	if root := c.root.bucket.get(key); root != nil {
		if root.bucket != nil {
			if root.bucket.cached() {
				return newCacheReadBucket(
					c.dbTx, c, c.cache, root,
				)
			} else {
				return c.fetchBucket().NestedReadBucket(key)
			}
		}
	}

	return nil
}

func (c *cacheReadBucket) ForEach(f func(k, v []byte) error) error {
	return forEachImpl(c.root, f)
}

func (c *cacheReadBucket) Get(key []byte) []byte {
	return getImpl(c.root, key)
}

func (c *cacheReadBucket) ReadCursor() walletdb.ReadCursor {
	return newCacheReadCursor(c)
}

func deleteFromCache(cache *Cache, root *cachedItem, key []byte,
	bucket bool) error {

	if cacheItem := root.bucket.get(key); cacheItem != nil {
		// Sanity checks.
		if bucket && cacheItem.bucket == nil {
			return walletdb.ErrIncompatibleValue
		}

		if !bucket && cacheItem.bucket != nil {
			return walletdb.ErrIncompatibleValue
		}

		cache.pendingDelete(root, cacheItem)
		root.bucket.del(cacheItem)
	}

	return nil

}

// cacheReadWriteBucket is a walletdb.ReadWriteBucket compatible bucket
// implementation operating on already cached values or reading on demand for
// skipped (read-through) sub buckets. Updates on this bucket or sub buckets
// will be added to the cache unless in a skipped (write-through) bucket.
type cacheReadWriteBucket struct {
	cache    *Cache
	root     *cachedItem
	dbBucket walletdb.ReadWriteBucket
}

var _ walletdb.ReadWriteBucket = (*cacheReadWriteBucket)(nil)

func newCacheReadWriteBucket(cache *Cache, root *cachedItem,
	dbBucket walletdb.ReadWriteBucket) *cacheReadWriteBucket {

	return &cacheReadWriteBucket{
		cache:    cache,
		root:     root,
		dbBucket: dbBucket,
	}
}

func (c *cacheReadWriteBucket) NestedReadBucket(key []byte) walletdb.ReadBucket {
	return c.NestedReadWriteBucket(key)
}

func (c *cacheReadWriteBucket) ForEach(f func(k, v []byte) error) error {
	return forEachImpl(c.root, f)
}

func (c *cacheReadWriteBucket) Get(key []byte) []byte {
	return getImpl(c.root, key)
}

func (c *cacheReadWriteBucket) ReadCursor() walletdb.ReadCursor {
	return newCacheReadWriteCursor(c)
}

func (c *cacheReadWriteBucket) NestedReadWriteBucket(
	key []byte) walletdb.ReadWriteBucket {

	if root := c.root.bucket.get(key); root != nil {
		if root.bucket == nil {
			return nil
		}

		dbBucket := c.dbBucket.NestedReadWriteBucket(key)

		// The bucket is cached.
		if dbBucket != nil && root.bucket.cached() {
			return newCacheReadWriteBucket(c.cache, root, dbBucket)
		}

		return dbBucket
	}

	return nil
}

func (c *cacheReadWriteBucket) createBucketImpl(key []byte) (
	walletdb.ReadWriteBucket, error) {

	dbBucket, err := c.dbBucket.CreateBucket(key)
	if err != nil {
		return nil, err
	}

	root := &cachedItem{
		key: string(key),
	}

	skipped := c.cache.skipped[string(key)]
	if skipped {
		// We add the bucket reference even though we'll be reading
		// through it.
		root.bucket = newReadThroughCacheBucket()
	} else {
		root.bucket = newCacheBucket()
	}

	c.root.bucket.put(root)
	c.cache.pendingAdd(c.root, root)

	if !skipped {
		return newCacheReadWriteBucket(c.cache, root, dbBucket), nil
	}

	return dbBucket, nil
}

func (c *cacheReadWriteBucket) CreateBucket(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if root := c.root.bucket.get(key); root != nil {
		return nil, ErrBucketExists
	}

	return c.createBucketImpl(key)
}

func (c *cacheReadWriteBucket) CreateBucketIfNotExists(key []byte) (
	walletdb.ReadWriteBucket, error) {

	dbBucket, err := c.dbBucket.CreateBucketIfNotExists(key)
	if err != nil {
		return nil, err
	}

	// Return existing bucket if exists.
	if root := c.root.bucket.get(key); root != nil {
		if root.bucket == nil {
			return nil, walletdb.ErrIncompatibleValue
		}

		if root.bucket.cached() {
			return newCacheReadWriteBucket(
				c.cache, root, dbBucket,
			), nil
		}

		return dbBucket, nil
	}

	// Insert new bucket otherwise.
	root := &cachedItem{
		key: string(key),
	}

	// We do add this new bucket reference even if though we won't cache
	// its contents.
	skipped := c.cache.skipped[string(key)]
	if skipped {
		root.bucket = newReadThroughCacheBucket()
	} else {
		root.bucket = newCacheBucket()
	}

	c.root.bucket.put(root)
	c.cache.pendingAdd(c.root, root)

	if !skipped {
		return newCacheReadWriteBucket(c.cache, root, dbBucket), nil
	}

	return dbBucket, nil
}

func (c *cacheReadWriteBucket) DeleteNestedBucket(key []byte) error {
	if err := c.dbBucket.DeleteNestedBucket(key); err != nil {
		return err
	}

	return deleteFromCache(c.cache, c.root, key, true)
}

func (c *cacheReadWriteBucket) Put(key, value []byte) error {
	if err := c.dbBucket.Put(key, value); err != nil {
		return err
	}

	if cacheItem := c.root.bucket.get(key); cacheItem != nil {
		if cacheItem.bucket != nil {
			return walletdb.ErrIncompatibleValue
		}

		c.cache.pendingUpdate(cacheItem, cacheItem.value)
		cacheItem.value = string(value)
	} else {
		newItem := &cachedItem{
			key:   string(key),
			value: string(value),
		}
		c.root.bucket.put(newItem)
		c.cache.pendingAdd(c.root, newItem)
	}

	return nil
}

func (c *cacheReadWriteBucket) Delete(key []byte) error {
	if err := c.dbBucket.Delete(key); err != nil {
		return err
	}

	return deleteFromCache(c.cache, c.root, key, false)
}

func (c *cacheReadWriteBucket) ReadWriteCursor() walletdb.ReadWriteCursor {
	return newCacheReadWriteCursor(c)
}

func (c *cacheReadWriteBucket) Tx() walletdb.ReadWriteTx {
	return c.cache.currRwTx
}

func (c *cacheReadWriteBucket) NextSequence() (uint64, error) {
	next, err := c.dbBucket.NextSequence()
	if err != nil {
		return 0, err
	}

	c.root.bucket.seq = &next
	return *c.root.bucket.seq, nil
}

func (c *cacheReadWriteBucket) SetSequence(v uint64) error {
	if err := c.dbBucket.SetSequence(v); err != nil {
		return err
	}

	c.root.bucket.seq = &v
	return nil
}

func (c *cacheReadWriteBucket) Sequence() uint64 {
	if c.root.bucket.seq == nil {
		seq := c.dbBucket.Sequence()
		c.root.bucket.seq = &seq
	}

	return *c.root.bucket.seq
}

// cacheCursor implements common functions used in the cacheReadCursor and
// cacheReadWriteCursor, technically implementing the walletdb.ReadWriteCursor
// for cached buckets.
type cacheCursor struct {
	root    *cachedItem
	currKey string
}

func (c *cacheCursor) First() (key, value []byte) {
	valItem := c.root.bucket.tree.Min()
	if valItem != nil {
		cacheItem := valItem.(*cachedItem)
		c.currKey = cacheItem.key
		if cacheItem.bucket == nil {
			value = []byte(cacheItem.value)
		}

		return []byte(cacheItem.key), value
	}

	return nil, nil
}

func (c *cacheCursor) Last() (key, value []byte) {
	valItem := c.root.bucket.tree.Max()
	if valItem != nil {
		cacheItem := valItem.(*cachedItem)
		c.currKey = cacheItem.key

		if cacheItem.bucket == nil {
			value = []byte(cacheItem.value)
		}

		return []byte(cacheItem.key), value
	}

	return nil, nil
}

func (c *cacheCursor) next(seekKey string, includeSeekKey bool) (
	key, value []byte) {

	keyItem := &cachedItem{
		key: seekKey,
	}

	c.root.bucket.tree.AscendGreaterOrEqual(
		keyItem,
		func(nextItem btree.Item) bool {
			cacheItem := nextItem.(*cachedItem)
			if !includeSeekKey && cacheItem.key == seekKey {
				return true
			}

			key = []byte(cacheItem.key)
			if cacheItem.bucket == nil {
				value = []byte(cacheItem.value)
			}

			return false
		},
	)

	if key != nil {
		c.currKey = string(key)
	}

	return key, value
}

func (c *cacheCursor) Next() (key, value []byte) {
	return c.next(c.currKey, false)
}

func (c *cacheCursor) Seek(seek []byte) (key, value []byte) {
	return c.next(string(seek), true)
}

func (c *cacheCursor) Prev() ([]byte, []byte) {
	keyItem := &cachedItem{
		key: c.currKey,
	}

	var key, value []byte
	c.root.bucket.tree.DescendLessOrEqual(
		keyItem,
		func(nextItem btree.Item) bool {
			cacheItem := nextItem.(*cachedItem)
			if cacheItem.key == c.currKey {
				return true
			}

			key = []byte(cacheItem.key)
			if cacheItem.bucket == nil {
				value = []byte(cacheItem.value)
			}

			return false
		},
	)

	if key != nil {
		c.currKey = string(key)
	}

	return key, value
}

// cacheReadCursor is a walletdb.ReadCursor compatible cursor for
// cached buckets.
type cacheReadCursor struct {
	cacheCursor
}

var _ walletdb.ReadCursor = (*cacheReadCursor)(nil)

func newCacheReadCursor(cacheBucket *cacheReadBucket) *cacheReadCursor {
	return &cacheReadCursor{
		cacheCursor: cacheCursor{
			root: cacheBucket.root,
		},
	}
}

// cacheReadWriteCursor is a walletdb.ReadWriteCursor compatible cursor for
// cached buckets.
type cacheReadWriteCursor struct {
	cacheCursor
	cacheBucket *cacheReadWriteBucket
}

var _ walletdb.ReadWriteCursor = (*cacheReadWriteCursor)(nil)

func newCacheReadWriteCursor(
	cacheBucket *cacheReadWriteBucket) *cacheReadWriteCursor {

	return &cacheReadWriteCursor{
		cacheCursor: cacheCursor{
			root: cacheBucket.root,
		},
		cacheBucket: cacheBucket,
	}
}

func (c *cacheReadWriteCursor) Delete() error {
	return c.cacheBucket.Delete([]byte(c.currKey))
}
