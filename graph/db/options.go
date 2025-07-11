package graphdb

import "time"

const (
	// DefaultRejectCacheSize is the default number of rejectCacheEntries to
	// cache for use in the rejection cache of incoming gossip traffic. This
	// produces a cache size of around 1MB.
	DefaultRejectCacheSize = 50000

	// DefaultChannelCacheSize is the default number of ChannelEdges cached
	// in order to reply to gossip queries. This produces a cache size of
	// around 40MB.
	DefaultChannelCacheSize = 20000

	// DefaultPreAllocCacheNumNodes is the default number of channels we
	// assume for mainnet for pre-allocating the graph cache. As of
	// September 2021, there currently are 14k nodes in a strictly pruned
	// graph, so we choose a number that is slightly higher.
	DefaultPreAllocCacheNumNodes = 15000
)

// chanGraphOptions holds parameters for tuning and customizing the
// ChannelGraph.
type chanGraphOptions struct {
	// useGraphCache denotes whether the in-memory graph cache should be
	// used or a fallback version that uses the underlying database for
	// path finding.
	useGraphCache bool

	// preAllocCacheNumNodes is the number of nodes we expect to be in the
	// graph cache, so we can pre-allocate the map accordingly.
	preAllocCacheNumNodes int

	// asyncGraphCachePopulation indicates whether the graph cache
	// should be populated asynchronously or if the Start method should
	// block until the cache is fully populated.
	asyncGraphCachePopulation bool
}

// defaultChanGraphOptions returns a new chanGraphOptions instance populated
// with default values.
func defaultChanGraphOptions() *chanGraphOptions {
	return &chanGraphOptions{
		useGraphCache:             true,
		asyncGraphCachePopulation: true,
		preAllocCacheNumNodes:     DefaultPreAllocCacheNumNodes,
	}
}

// ChanGraphOption describes the signature of a functional option that can be
// used to customize a ChannelGraph instance.
type ChanGraphOption func(*chanGraphOptions)

// WithUseGraphCache sets whether the in-memory graph cache should be used.
func WithUseGraphCache(use bool) ChanGraphOption {
	return func(o *chanGraphOptions) {
		o.useGraphCache = use
	}
}

// WithPreAllocCacheNumNodes sets the number of nodes we expect to be in the
// graph cache, so we can pre-allocate the map accordingly.
func WithPreAllocCacheNumNodes(n int) ChanGraphOption {
	return func(o *chanGraphOptions) {
		o.preAllocCacheNumNodes = n
	}
}

// WithAsyncGraphCachePopulation sets whether the graph cache should be
// populated asynchronously or if the Start method should block until the
// cache is fully populated.
func WithAsyncGraphCachePopulation(async bool) ChanGraphOption {
	return func(o *chanGraphOptions) {
		o.asyncGraphCachePopulation = async
	}
}

// WithSyncGraphCachePopulation will cause the ChannelGraph to block
// until the graph cache is fully populated before returning from the Start
// method. This is useful for tests that need to ensure the graph cache is
// fully populated before proceeding with further operations.
func WithSyncGraphCachePopulation() ChanGraphOption {
	return func(o *chanGraphOptions) {
		o.asyncGraphCachePopulation = false
	}
}

// StoreOptions holds parameters for tuning and customizing a graph DB.
type StoreOptions struct {
	// RejectCacheSize is the maximum number of rejectCacheEntries to hold
	// in the rejection cache.
	RejectCacheSize int

	// ChannelCacheSize is the maximum number of ChannelEdges to hold in the
	// channel cache.
	ChannelCacheSize int

	// BatchCommitInterval is the maximum duration the batch schedulers will
	// wait before attempting to commit a pending set of updates.
	BatchCommitInterval time.Duration

	// NoMigration specifies that underlying backend was opened in read-only
	// mode and migrations shouldn't be performed. This can be useful for
	// applications that use the channeldb package as a library.
	NoMigration bool
}

// DefaultOptions returns a StoreOptions populated with default values.
func DefaultOptions() *StoreOptions {
	return &StoreOptions{
		RejectCacheSize:  DefaultRejectCacheSize,
		ChannelCacheSize: DefaultChannelCacheSize,
		NoMigration:      false,
	}
}

// StoreOptionModifier is a function signature for modifying the default
// StoreOptions.
type StoreOptionModifier func(*StoreOptions)

// WithRejectCacheSize sets the RejectCacheSize to n.
func WithRejectCacheSize(n int) StoreOptionModifier {
	return func(o *StoreOptions) {
		o.RejectCacheSize = n
	}
}

// WithChannelCacheSize sets the ChannelCacheSize to n.
func WithChannelCacheSize(n int) StoreOptionModifier {
	return func(o *StoreOptions) {
		o.ChannelCacheSize = n
	}
}

// WithBatchCommitInterval sets the batch commit interval for the interval batch
// schedulers.
func WithBatchCommitInterval(interval time.Duration) StoreOptionModifier {
	return func(o *StoreOptions) {
		o.BatchCommitInterval = interval
	}
}
