package channeldb

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/kvdb"
)

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
	kvdb.BoltBackendConfig

	// RejectCacheSize is the maximum number of rejectCacheEntries to hold
	// in the rejection cache.
	RejectCacheSize int

	// ChannelCacheSize is the maximum number of ChannelEdges to hold in the
	// channel cache.
	ChannelCacheSize int

	// BatchCommitInterval is the maximum duration the batch schedulers will
	// wait before attempting to commit a pending set of updates.
	BatchCommitInterval time.Duration

	// clock is the time source used by the database.
	clock clock.Clock

	// dryRun will fail to commit a successful migration when opening the
	// database if set to true.
	dryRun bool
}

// DefaultOptions returns an Options populated with default values.
func DefaultOptions() Options {
	return Options{
		BoltBackendConfig: kvdb.BoltBackendConfig{
			NoFreelistSync:    true,
			AutoCompact:       false,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		},
		RejectCacheSize:  DefaultRejectCacheSize,
		ChannelCacheSize: DefaultChannelCacheSize,
		clock:            clock.NewDefaultClock(),
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

// OptionAutoCompact turns on automatic database compaction on startup.
func OptionAutoCompact() OptionModifier {
	return func(o *Options) {
		o.AutoCompact = true
	}
}

// OptionAutoCompactMinAge sets the minimum age for automatic database
// compaction.
func OptionAutoCompactMinAge(minAge time.Duration) OptionModifier {
	return func(o *Options) {
		o.AutoCompactMinAge = minAge
	}
}

// OptionSetBatchCommitInterval sets the batch commit interval for the internval
// batch schedulers.
func OptionSetBatchCommitInterval(interval time.Duration) OptionModifier {
	return func(o *Options) {
		o.BatchCommitInterval = interval
	}
}

// OptionClock sets a non-default clock dependency.
func OptionClock(clock clock.Clock) OptionModifier {
	return func(o *Options) {
		o.clock = clock
	}
}

// OptionDryRunMigration controls whether or not to intentially fail to commit a
// successful migration that occurs when opening the database.
func OptionDryRunMigration(dryRun bool) OptionModifier {
	return func(o *Options) {
		o.dryRun = dryRun
	}
}
