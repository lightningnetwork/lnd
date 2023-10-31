package lncfg

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// MaxFeeRateFloor is the smallest config value allowed for the max fee
	// rate in sat/vb.
	MaxFeeRateFloor chainfee.SatPerVByte = 100

	// MaxAllowedFeeRate is the largest fee rate in sat/vb that we allow
	// when configuring the MaxFeeRate.
	MaxAllowedFeeRate = 10_000
)

//nolint:lll
type Sweeper struct {
	BatchWindowDuration             time.Duration        `long:"batchwindowduration" description:"Duration of the sweep batch window. The sweep is held back during the batch window to allow more inputs to be added and thereby lower the fee per input."`
	MaxFeeRate                      chainfee.SatPerVByte `long:"maxfeerate" description:"Maximum fee rate in sat/vb that the sweeper is allowed to use when sweeping funds. Setting this value too low can result in transactions not being confirmed in time, causing HTLCs to expire hence potentially losing funds."`
	MaxNonTimeSensitiveSweepFeeRate chainfee.SatPerVByte `long:"max-non-time-sensitive-feerate" description:"The maximum fee rate in sat/vbyte that will be used for non time sensitive sweeps of unilateral channel closures."`
	MaxTimeSensitiveSweepFeeRate    chainfee.SatPerVByte `long:"max-time-sensitive-feerate" description:"The maximum fee rate in sat/vbyte that will be used for time sensitive sweeps of unilateral channel closures."`
	MaxAnchorFeerate                chainfee.SatPerVByte `long:"max-anchor-feerate" description:"The maximum fee rate in sat/vbyte that will be used to CPFP a commitment tx until unconfirmed."`
}

// Validate checks the values configured for the sweeper.
func (s *Sweeper) Validate() error {
	if s.BatchWindowDuration < 0 {
		return fmt.Errorf("batchwindowduration must be positive")
	}

	// We require the max fee rate to be at least 100 sat/vbyte.
	if s.MaxFeeRate < MaxFeeRateFloor {
		return fmt.Errorf("maxfeerate must be >= 100 sat/vb")
	}

	// We require the max fee rate to be no greater than 10_000 sat/vbyte.
	if s.MaxFeeRate > MaxAllowedFeeRate {
		return fmt.Errorf("maxfeerate must be <= 10000 sat/vb")
	}

	return nil
}
