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
	// Note: the LDK reference (PR #4468) also uses this multiplier as the
	// window count for the incoming-revenue aggregated average, so we
	// mirror that here.
	defaultReputationMultiplier = 12

	// defaultGeneralPct / defaultCongestionPct are the share of a channel's
	// slots and liquidity assigned to the general and congestion buckets
	// respectively. The protected bucket receives the remainder.
	defaultGeneralPct    = 40
	defaultCongestionPct = 20

	// protocolMaxAcceptedHTLCs is the protocol maximum number of HTLCs a
	// channel may have in flight. Channels reporting more than this have
	// their limit clamped by the sizing logic (mirrors the LDK reference
	// upper bound; the protocol caps real channels at this value anyway).
	protocolMaxAcceptedHTLCs = 483

	// minChannelHTLCsForBuckets is the smallest max_accepted_htlcs
	// for which splitting resources across three buckets is
	// meaningful. Below this a channel is given a single general
	// bucket (basic DoS protection only), per the BOLT recommendation
	// for small channels; this also avoids degenerate zero-slot
	// buckets that would distort the decision tree (review finding
	// B4). 13 ensures the general bucket gets >= the per-pair slot
	// floor (5) under the default 40% split.
	minChannelHTLCsForBuckets = 13

	// blockInterval is the assumed seconds per block (10 minutes) used to
	// convert cltv deltas to durations.
	blockInterval = 10 * 60
)

// Config holds the tunable parameters of the reputation subsystem. The
// zero value is not valid; use DefaultConfig and override as needed.
type Config struct {
	// GeneralPct is the percentage of a channel's slots and liquidity
	// assigned to the general bucket.
	GeneralPct uint8

	// CongestionPct is the percentage of a channel's slots and liquidity
	// assigned to the congestion bucket.
	CongestionPct uint8

	// ResolutionPeriod is the duration within which an HTLC resolution is
	// considered "good" behaviour (no opportunity cost).
	ResolutionPeriod time.Duration

	// RevenueWindow is the rolling window over which incoming-channel
	// revenue is measured.
	RevenueWindow time.Duration

	// ReputationMultiplier scales RevenueWindow to give the (longer) window
	// over which outgoing-channel reputation is measured.
	ReputationMultiplier uint8
}

// DefaultConfig returns the recommended default configuration, matching the
// BOLT recommendation and the LDK reference implementation.
func DefaultConfig() Config {
	return Config{
		GeneralPct:           defaultGeneralPct,
		CongestionPct:        defaultCongestionPct,
		ResolutionPeriod:     defaultResolutionPeriod,
		RevenueWindow:        defaultRevenueWindow,
		ReputationMultiplier: defaultReputationMultiplier,
	}
}

// Validate ensures the configuration is internally consistent.
func (c Config) Validate() error {
	if c.GeneralPct+c.CongestionPct >= 100 {
		return fmt.Errorf("general (%d) + congestion (%d) bucket "+
			"percentages must be < 100", c.GeneralPct,
			c.CongestionPct)
	}

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

	return nil
}

// reputationWindow returns the rolling window over which outgoing-channel
// reputation is tracked.
func (c Config) reputationWindow() time.Duration {
	return c.RevenueWindow * time.Duration(c.ReputationMultiplier)
}
