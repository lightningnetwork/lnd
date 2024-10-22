package channeldb

import (
	"github.com/lightningnetwork/lnd/clock"
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

	// DefaultPreAllocCacheNumNodes is the default number of channels we
	// assume for mainnet for pre-allocating the graph cache. As of
	// September 2021, there currently are 14k nodes in a strictly pruned
	// graph, so we choose a number that is slightly higher.
	DefaultPreAllocCacheNumNodes = 15000
)

// OptionalMiragtionConfig defines the flags used to signal whether a
// particular migration needs to be applied.
type OptionalMiragtionConfig struct {
	// PruneRevocationLog specifies that the revocation log migration needs
	// to be applied.
	PruneRevocationLog bool
}

// Options holds parameters for tuning and customizing a channeldb.DB.
type Options struct {
	OptionalMiragtionConfig

	// NoMigration specifies that underlying backend was opened in read-only
	// mode and migrations shouldn't be performed. This can be useful for
	// applications that use the channeldb package as a library.
	NoMigration bool

	// NoRevLogAmtData when set to true, indicates that amount data should
	// not be stored in the revocation log.
	NoRevLogAmtData bool

	// clock is the time source used by the database.
	clock clock.Clock

	// dryRun will fail to commit a successful migration when opening the
	// database if set to true.
	dryRun bool

	// keepFailedPaymentAttempts determines whether failed htlc attempts
	// are kept on disk or removed to save space.
	keepFailedPaymentAttempts bool

	// storeFinalHtlcResolutions determines whether to persistently store
	// the final resolution of incoming htlcs.
	storeFinalHtlcResolutions bool
}

// DefaultOptions returns an Options populated with default values.
func DefaultOptions() Options {
	return Options{
		OptionalMiragtionConfig: OptionalMiragtionConfig{},
		NoMigration:             false,
		clock:                   clock.NewDefaultClock(),
	}
}

// OptionModifier is a function signature for modifying the default Options.
type OptionModifier func(*Options)

// OptionNoRevLogAmtData sets the NoRevLogAmtData option to the given value. If
// it is set to true then amount data will not be stored in the revocation log.
func OptionNoRevLogAmtData(noAmtData bool) OptionModifier {
	return func(o *Options) {
		o.NoRevLogAmtData = noAmtData
	}
}

// OptionNoMigration allows the database to be opened in read only mode by
// disabling migrations.
func OptionNoMigration(b bool) OptionModifier {
	return func(o *Options) {
		o.NoMigration = b
	}
}

// OptionClock sets a non-default clock dependency.
func OptionClock(clock clock.Clock) OptionModifier {
	return func(o *Options) {
		o.clock = clock
	}
}

// OptionDryRunMigration controls whether or not to intentionally fail to commit a
// successful migration that occurs when opening the database.
func OptionDryRunMigration(dryRun bool) OptionModifier {
	return func(o *Options) {
		o.dryRun = dryRun
	}
}

// OptionKeepFailedPaymentAttempts controls whether failed payment attempts are
// kept on disk after a payment settles.
func OptionKeepFailedPaymentAttempts(keepFailedPaymentAttempts bool) OptionModifier {
	return func(o *Options) {
		o.keepFailedPaymentAttempts = keepFailedPaymentAttempts
	}
}

// OptionStoreFinalHtlcResolutions controls whether to persistently store the
// final resolution of incoming htlcs.
func OptionStoreFinalHtlcResolutions(
	storeFinalHtlcResolutions bool) OptionModifier {

	return func(o *Options) {
		o.storeFinalHtlcResolutions = storeFinalHtlcResolutions
	}
}

// OptionPruneRevocationLog specifies whether the migration for pruning
// revocation logs needs to be applied or not.
func OptionPruneRevocationLog(prune bool) OptionModifier {
	return func(o *Options) {
		o.OptionalMiragtionConfig.PruneRevocationLog = prune
	}
}
