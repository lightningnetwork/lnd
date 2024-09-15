package lncfg

import (
	"fmt"
	"time"
)

const (
	// DefaultMinUpdateTimeout represents the minimum interval in which a
	// WebAPIEstimator will request fresh fees from its API.
	DefaultMinUpdateTimeout = 5 * time.Minute

	// DefaultMaxUpdateTimeout represents the maximum interval in which a
	// WebAPIEstimator will request fresh fees from its API.
	DefaultMaxUpdateTimeout = 20 * time.Minute

	// DefaultMaxMinRelayFeeRate is the default maximum minimum relay fee
	// rate in sat/vbyte.
	DefaultMaxMinRelayFeeRate = 200

	// DefaultFallbackFeeRate is the default fallback fee rate in sat/vbyte
	// that will be used if the fee estimation method fails.
	DefaultFallbackFeeRate = 25
)

// Fee holds the configuration options for fee estimation.
//
//nolint:lll
type Fee struct {
	URL                string        `long:"url" description:"Optional URL for external fee estimation. If no URL is specified, the method for fee estimation will depend on the chosen backend and network. Must be set for neutrino on mainnet."`
	MinUpdateTimeout   time.Duration `long:"min-update-timeout" description:"The minimum interval in which fees will be updated from the specified fee URL."`
	MaxUpdateTimeout   time.Duration `long:"max-update-timeout" description:"The maximum interval in which fees will be updated from the specified fee URL."`
	MaxMinRelayFeeRate uint64        `long:"max-min-relay-feerate" description:"The maximum relay fee rate in sat/vbyte that will be used although the backend would return a higher fee rate. Must be large enough to ensure transaction propagation in a croweded mempool environment."`
	FallbackFeeRate    uint64        `long:"fallback-feerate" description:"The fee rate in sat/vbyte that will be used if the fee estimation method fails."`
}

// Validate checks the values configured for the fee estimator.
func (f *Fee) Validate() error {
	if f.MinUpdateTimeout < 0 {
		return fmt.Errorf("min-update-timeout must be positive")
	}

	if f.MaxUpdateTimeout < 0 {
		return fmt.Errorf("max-update-timeout must be positive")
	}

	if f.MaxMinRelayFeeRate < 10 {
		return fmt.Errorf("max-min-relay-feerate must be at least 10")
	}

	if f.FallbackFeeRate < 10 {
		return fmt.Errorf("fallback-feerate must be at least 10")
	}

	return nil
}
