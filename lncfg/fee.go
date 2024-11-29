package lncfg

import "time"

// DefaultMinUpdateTimeout represents the minimum interval in which a
// WebAPIEstimator will request fresh fees from its API.
const DefaultMinUpdateTimeout = 5 * time.Minute

// DefaultMaxUpdateTimeout represents the maximum interval in which a
// WebAPIEstimator will request fresh fees from its API.
const DefaultMaxUpdateTimeout = 20 * time.Minute

// Fee holds the configuration options for fee estimation.
//
//nolint:ll
type Fee struct {
	URL              string        `long:"url" description:"Optional URL for external fee estimation. If no URL is specified, the method for fee estimation will depend on the chosen backend and network. Must be set for neutrino on mainnet."`
	MinUpdateTimeout time.Duration `long:"min-update-timeout" description:"The minimum interval in which fees will be updated from the specified fee URL."`
	MaxUpdateTimeout time.Duration `long:"max-update-timeout" description:"The maximum interval in which fees will be updated from the specified fee URL."`
}
