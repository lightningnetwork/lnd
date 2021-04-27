package migration_01_to_11

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

	// DefaultDBTimeout specifies the default timeout value when opening
	// the bbolt database.
	DefaultDBTimeout = time.Second * 60
)

// Options holds parameters for tuning and customizing a channeldb.DB.
type Options struct {
	// RejectCacheSize is the maximum number of rejectCacheEntries to hold
	// in the rejection cache.
	RejectCacheSize int

	// ChannelCacheSize is the maximum number of ChannelEdges to hold in the
	// channel cache.
	ChannelCacheSize int

	// NoFreelistSync, if true, prevents the database from syncing its
	// freelist to disk, resulting in improved performance at the expense of
	// increased startup time.
	NoFreelistSync bool

	// DBTimeout specifies the timeout value to use when opening the wallet
	// database.
	DBTimeout time.Duration
}

// DefaultOptions returns an Options populated with default values.
func DefaultOptions() Options {
	return Options{
		RejectCacheSize:  DefaultRejectCacheSize,
		ChannelCacheSize: DefaultChannelCacheSize,
		NoFreelistSync:   true,
		DBTimeout:        DefaultDBTimeout,
	}
}

// OptionModifier is a function signature for modifying the default Options.
type OptionModifier func(*Options)
