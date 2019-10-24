package migration_01_to_11

const (
	// DefaultRejectCacheSize is the default number of rejectCacheEntries to
	// cache for use in the rejection cache of incoming gossip traffic. This
	// produces a cache size of around 1MB.
	DefaultRejectCacheSize = 50000

	// DefaultChannelCacheSize is the default number of ChannelEdges cached
	// in order to reply to gossip queries. This produces a cache size of
	// around 40MB.
	DefaultChannelCacheSize = 20000
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
}

// DefaultOptions returns an Options populated with default values.
func DefaultOptions() Options {
	return Options{
		RejectCacheSize:  DefaultRejectCacheSize,
		ChannelCacheSize: DefaultChannelCacheSize,
		NoFreelistSync:   true,
	}
}

// OptionModifier is a function signature for modifying the default Options.
type OptionModifier func(*Options)

// OptionSetRejectCacheSize sets the RejectCacheSize to n.
func OptionSetRejectCacheSize(n int) OptionModifier {
	return func(o *Options) {
		o.RejectCacheSize = n
	}
}

// OptionSetChannelCacheSize sets the ChannelCacheSize to n.
func OptionSetChannelCacheSize(n int) OptionModifier {
	return func(o *Options) {
		o.ChannelCacheSize = n
	}
}

// OptionSetSyncFreelist allows the database to sync its freelist.
func OptionSetSyncFreelist(b bool) OptionModifier {
	return func(o *Options) {
		o.NoFreelistSync = !b
	}
}
