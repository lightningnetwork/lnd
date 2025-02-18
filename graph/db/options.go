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

// KVStoreOptions holds parameters for tuning and customizing a graph.DB.
type KVStoreOptions struct {
	// RejectCacheSize is the maximum number of rejectCacheEntries to hold
	// in the rejection cache.
	RejectCacheSize int

	// ChannelCacheSize is the maximum number of ChannelEdges to hold in the
	// channel cache.
	ChannelCacheSize int

	// BatchCommitInterval is the maximum duration the batch schedulers will
	// wait before attempting to commit a pending set of updates.
	BatchCommitInterval time.Duration

	// PreAllocCacheNumNodes is the number of nodes we expect to be in the
	// graph cache, so we can pre-allocate the map accordingly.
	PreAllocCacheNumNodes int

	// UseGraphCache denotes whether the in-memory graph cache should be
	// used or a fallback version that uses the underlying database for
	// path finding.
	UseGraphCache bool

	// NoMigration specifies that underlying backend was opened in read-only
	// mode and migrations shouldn't be performed. This can be useful for
	// applications that use the channeldb package as a library.
	NoMigration bool
}

// DefaultOptions returns an KVStoreOptions populated with default values.
func DefaultOptions() *KVStoreOptions {
	return &KVStoreOptions{
		RejectCacheSize:       DefaultRejectCacheSize,
		ChannelCacheSize:      DefaultChannelCacheSize,
		PreAllocCacheNumNodes: DefaultPreAllocCacheNumNodes,
		UseGraphCache:         true,
		NoMigration:           false,
	}
}

// KVStoreOptionModifier is a function signature for modifying the default
// KVStoreOptions.
type KVStoreOptionModifier func(*KVStoreOptions)

// WithRejectCacheSize sets the RejectCacheSize to n.
func WithRejectCacheSize(n int) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.RejectCacheSize = n
	}
}

// WithChannelCacheSize sets the ChannelCacheSize to n.
func WithChannelCacheSize(n int) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.ChannelCacheSize = n
	}
}

// WithPreAllocCacheNumNodes sets the PreAllocCacheNumNodes to n.
func WithPreAllocCacheNumNodes(n int) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.PreAllocCacheNumNodes = n
	}
}

// WithBatchCommitInterval sets the batch commit interval for the interval batch
// schedulers.
func WithBatchCommitInterval(interval time.Duration) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.BatchCommitInterval = interval
	}
}

// WithUseGraphCache sets the UseGraphCache option to the given value.
func WithUseGraphCache(use bool) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.UseGraphCache = use
	}
}
