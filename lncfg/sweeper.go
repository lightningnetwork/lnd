package lncfg

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
)

const (
	// MaxFeeRateFloor is the smallest config value allowed for the max fee
	// rate in sat/vb.
	MaxFeeRateFloor chainfee.SatPerVByte = 100

	// MaxAllowedFeeRate is the largest fee rate in sat/vb that we allow
	// when configuring the MaxFeeRate.
	MaxAllowedFeeRate = 10_000

	// DefaultBaseFeeRate is the default starting fee rate in sat/vb.
	DefaultBaseFeeRate chainfee.SatPerVByte = 1
)

// Sweeper holds configuration for the UTXO sweeper.
type Sweeper struct {
	// BatchWindowDuration is the duration of the sweep batch window.
	BatchWindowDuration time.Duration `long:"batchwindowduration" description:"Duration of the sweep batch window. The sweep is held back during the batch window to allow more inputs to be added and thereby lower the fee per input." hidden:"true"`

	// MaxFeeRate is the maximum fee rate in sat/vb allowed for sweeping.
	MaxFeeRate chainfee.SatPerVByte `long:"maxfeerate" description:"Maximum fee rate in sat/vb that the sweeper is allowed to use when sweeping funds, the fee rate derived from budgets are capped at this value. Setting this value too low can result in transactions not being confirmed in time, causing HTLCs to expire hence potentially losing funds."`

	// NoDeadlineConfTarget is the confirmation target for non-time-sensitive sweeps.
	NoDeadlineConfTarget uint32 `long:"nodeadlineconftarget" description:"The conf target to use when sweeping non-time-sensitive outputs. This is useful for sweeping outputs that are not time-sensitive, and can be swept at a lower fee rate."`

	// Budget configures automatic sweep fee estimation.
	Budget *contractcourt.BudgetConfig `group:"sweeper.budget" namespace:"budget" long:"budget" description:"An optional config group that's used for the automatic sweep fee estimation. The Budget config gives options to limits ones fee exposure when sweeping unilateral close outputs and the fee rate calculated from budgets is capped at sweeper.maxfeerate. Check the budget config options for more details."`

	// FeeFunctionType specifies the fee function type for sweeping.
	FeeFunctionType string `long:"feefunctiontype" description:"The type of fee function to use for sweeping: 'linear' (default), 'cubic_delay', or 'cubic_eager'."`

	// BaseFeeRate is the starting fee rate in sat/vb for the fee function.
	BaseFeeRate chainfee.SatPerVByte `long:"basefeerate" description:"The base fee rate in sat/vb to start the fee function from. Must be at least 1 sat/vb."`
}

// Validate checks the values configured for the sweeper.
func (s *Sweeper) Validate() error {
	if s.BatchWindowDuration < 0 {
		return fmt.Errorf("batchwindowduration must be positive")
	}

	if s.MaxFeeRate < MaxFeeRateFloor {
		return fmt.Errorf("maxfeerate must be >= 100 sat/vb")
	}

	if s.MaxFeeRate > MaxAllowedFeeRate {
		return fmt.Errorf("maxfeerate must be <= 10000 sat/vb")
	}

	if s.NoDeadlineConfTarget < 144 {
		return fmt.Errorf("nodeadlineconftarget must be at least 144")
	}

	if s.Budget != nil {
		if err := s.Budget.Validate(); err != nil {
			return fmt.Errorf("invalid budget config: %w", err)
		}
	}

	validFeeFunctions := map[string]bool{
		"linear":      true,
		"cubic_delay": true,
		"cubic_eager": true,
	}
	if s.FeeFunctionType == "" {
		return fmt.Errorf("feefunctiontype must not be empty")
	}
	if !validFeeFunctions[s.FeeFunctionType] {
		return fmt.Errorf("feefunctiontype must be one of: linear, cubic_delay, cubic_eager; got %v", s.FeeFunctionType)
	}

	if s.BaseFeeRate < 1 {
		return fmt.Errorf("basefeerate must be >= 1 sat/vb")
	}

	return nil
}

// DefaultSweeperConfig returns the default configuration for the sweeper.
func DefaultSweeperConfig() *Sweeper {
	return &Sweeper{
		MaxFeeRate:           sweep.DefaultMaxFeeRate,
		NoDeadlineConfTarget: uint32(sweep.DefaultDeadlineDelta),
		Budget:               contractcourt.DefaultBudgetConfig(),
		FeeFunctionType:      "linear",           // Default fee function.
		BaseFeeRate:          DefaultBaseFeeRate, // Default base fee rate.
	}
}
