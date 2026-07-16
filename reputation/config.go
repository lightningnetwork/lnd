package reputation

import (
	"fmt"
	"time"
)

const (
	// defaultResolutionPeriod is the amount of time an HTLC is allowed to
	// resolve in that classifies as "good" behaviour. The protocol allows
	// for a 60s MPP timeout, so 90s is the recommended default (matches the
	// BOLT recommendation and the LDK reference).
	defaultResolutionPeriod = 90 * time.Second

	// defaultRevenueWindow is the largest cltv delta from the current block
	// height that a node will allow before failing with expiry_too_far,
	// expressed as a duration assuming 10 minute blocks (2016 blocks ~= 2
	// weeks).
	defaultRevenueWindow = 2016 * 10 * time.Minute

	// defaultReputationMultiplier is the multiplier applied to the revenue
	// window to determine the rolling window over which the outgoing
	// channel's forwarding history is considered (default 12 => ~24 weeks).
	// This sizes the outgoing-reputation window only.
	defaultReputationMultiplier = 12

	// defaultRevenueWindowCount is the number of rolling windows over which
	// the incoming-revenue aggregated average is measured (BOLT proposal
	// recommends >= 6). This is deliberately distinct from
	// defaultReputationMultiplier: the LDK reference conflates the two, but
	// the proposal keeps the revenue-aggregation window count separate from
	// the outgoing-reputation multiplier.
	defaultRevenueWindowCount = 6

	// blockInterval is the assumed seconds per block (10 minutes) used to
	// convert cltv deltas to durations.
	blockInterval = 10 * 60
)

// Config holds the tunable parameters of the reputation subsystem. The
// zero value is not valid; use DefaultConfig and override as needed.
type Config struct {
	// ResolutionPeriod is the duration within which an HTLC resolution is
	// considered "good" behaviour (no opportunity cost).
	ResolutionPeriod time.Duration

	// RevenueWindow is the rolling window over which incoming-channel
	// revenue is measured.
	RevenueWindow time.Duration

	// ReputationMultiplier scales RevenueWindow to give the (longer) window
	// over which outgoing-channel reputation is measured.
	ReputationMultiplier uint8

	// RevenueWindowCount is the number of rolling RevenueWindow-sized
	// windows over which the incoming-revenue aggregated average is
	// measured. The BOLT proposal recommends at least 6. It is
	// intentionally distinct from ReputationMultiplier, which sizes only
	// the outgoing-reputation window.
	RevenueWindowCount uint8
}

// DefaultConfig returns the recommended default configuration, matching the
// BOLT recommendation and the LDK reference implementation.
func DefaultConfig() Config {
	return Config{
		ResolutionPeriod:     defaultResolutionPeriod,
		RevenueWindow:        defaultRevenueWindow,
		ReputationMultiplier: defaultReputationMultiplier,
		RevenueWindowCount:   defaultRevenueWindowCount,
	}
}

// Validate ensures the configuration is internally consistent.
func (c Config) Validate() error {
	if c.ResolutionPeriod <= 0 {
		return fmt.Errorf("resolution period must be positive, got %v",
			c.ResolutionPeriod)
	}

	if c.RevenueWindow <= 0 {
		return fmt.Errorf("revenue window must be positive, got %v",
			c.RevenueWindow)
	}

	if c.ReputationMultiplier == 0 {
		return fmt.Errorf("reputation multiplier must be positive")
	}

	if c.RevenueWindowCount == 0 {
		return fmt.Errorf("revenue window count must be positive")
	}

	return nil
}

// reputationWindow returns the rolling window over which outgoing-channel
// reputation is tracked.
func (c Config) reputationWindow() time.Duration {
	return c.RevenueWindow * time.Duration(c.ReputationMultiplier)
}
