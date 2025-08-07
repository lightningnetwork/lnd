package channeldb

import (
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

	// DefaultPreAllocCacheNumNodes is the default number of channels we
	// assume for mainnet for pre-allocating the graph cache. As of
	// September 2021, there currently are 14k nodes in a strictly pruned
	// graph, so we choose a number that is slightly higher.
	DefaultPreAllocCacheNumNodes = 15000
)

// OptionalMiragtionConfig defines the flags used to signal whether a
// particular migration needs to be applied.
type OptionalMiragtionConfig struct {
	// MigrationFlags is an array of booleans indicating which optional
	// migrations should be run. The index in the array corresponds to the
	// migration number in optionalVersions.
	MigrationFlags []bool

	// DecayedLog is a reference to the decayed log database. The channeldb
	// is inherently part of the optional migration flow so there is no need
	// to specify it here. The DecayedLog is a separate database in case the
	// kvdb backend is set to `bbolt`. And also for the kvdb SQL backend
	// case it is a separate table therefore we need to reference it here
	// as well to use the right query to access the decayed log.
	DecayedLog kvdb.Backend
}

// NewOptionalMiragtionConfig creates a new OptionalMiragtionConfig with the
// default migration flags.
func NewOptionalMiragtionConfig() OptionalMiragtionConfig {
	return OptionalMiragtionConfig{
		MigrationFlags: make([]bool, len(optionalVersions)),
	}
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

	// storeFinalHtlcResolutions determines whether to persistently store
	// the final resolution of incoming htlcs.
	storeFinalHtlcResolutions bool
}

// DefaultOptions returns an Options populated with default values.
func DefaultOptions() Options {
	return Options{
		OptionalMiragtionConfig: NewOptionalMiragtionConfig(),
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
		o.OptionalMiragtionConfig.MigrationFlags[0] = prune
	}
}

// OptionWithDecayedLogDB sets the decayed log database reference which might
// be used for some migrations because generally we only touch the channeldb
// databases in the migrations, this is a way to allow also access to the
// decayed log database.
func OptionWithDecayedLogDB(decayedLog kvdb.Backend) OptionModifier {
	return func(o *Options) {
		o.OptionalMiragtionConfig.DecayedLog = decayedLog
	}
}

// OptionGcDecayedLog specifies whether the decayed log migration has to
// take place.
func OptionGcDecayedLog(noGc bool) OptionModifier {
	return func(o *Options) {
		o.OptionalMiragtionConfig.MigrationFlags[1] = !noGc
	}
}
