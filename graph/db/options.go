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

// IteratorOption is a functional option used to change the per-call
// configuration for iterators.
type IteratorOption func(*iterConfig)

// iterConfig holds the configuration for graph operations.
type iterConfig struct {
	// chanUpdateIterBatchSize is the batch size to use when reading out
	// channel updates to send a peer a backlog.
	chanUpdateIterBatchSize int

	// nodeUpdateIterBatchSize is the batch size to use when reading out
	// node updates to send to a peer backlog.
	nodeUpdateIterBatchSize int

	// iterPublicNodes is used to make an iterator that only iterates over
	// public nodes.
	iterPublicNodes bool
}

// defaultIteratorConfig returns the default configuration.
func defaultIteratorConfig() *iterConfig {
	return &iterConfig{
		chanUpdateIterBatchSize: 1_000,
		nodeUpdateIterBatchSize: 1_000,
	}
}

// WithChanUpdateIterBatchSize sets the batch size for channel update
// iterators.
func WithChanUpdateIterBatchSize(size int) IteratorOption {
	return func(cfg *iterConfig) {
		if size > 0 {
			cfg.chanUpdateIterBatchSize = size
		}
	}
}

// WithNodeUpdateIterBatchSize set the batch size for node ann iterators.
func WithNodeUpdateIterBatchSize(size int) IteratorOption {
	return func(cfg *iterConfig) {
		if size > 0 {
			cfg.nodeUpdateIterBatchSize = size
		}
	}
}

// WithIterPublicNodesOnly is used to create an iterator that only iterates over
// public nodes.
func WithIterPublicNodesOnly() IteratorOption {
	return func(cfg *iterConfig) {
		cfg.iterPublicNodes = true
	}
}

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
}

// defaultChanGraphOptions returns a new chanGraphOptions instance populated
// with default values.
func defaultChanGraphOptions() *chanGraphOptions {
	return &chanGraphOptions{
		useGraphCache:         true,
		preAllocCacheNumNodes: DefaultPreAllocCacheNumNodes,
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
