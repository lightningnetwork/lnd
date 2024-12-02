package contractcourt

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
)

const (
	// MinBudgetValue is the minimal budget that we allow when configuring
	// the budget used in sweeping outputs. The actual budget can be lower
	// if the user decides to NOT set this value.
	//
	// NOTE: This value is chosen so the linear fee function can increase
	// at least 1 sat/kw per block.
	MinBudgetValue btcutil.Amount = 1008

	// MinBudgetRatio is the minimal ratio that we allow when configuring
	// the budget ratio used in sweeping outputs.
	MinBudgetRatio = 0.001

	// DefaultBudgetRatio defines a default budget ratio to be used when
	// sweeping inputs. This is a large value, which is fine as the final
	// fee rate is capped at the max fee rate configured.
	DefaultBudgetRatio = 0.5
)

// BudgetConfig is a struct that holds the configuration when offering outputs
// to the sweeper.
//
//nolint:ll
type BudgetConfig struct {
	ToLocal      btcutil.Amount `long:"tolocal" description:"The amount in satoshis to allocate as the budget to pay fees when sweeping the to_local output. If set, the budget calculated using the ratio (if set) will be capped at this value."`
	ToLocalRatio float64        `long:"tolocalratio" description:"The ratio of the value in to_local output to allocate as the budget to pay fees when sweeping it."`

	AnchorCPFP      btcutil.Amount `long:"anchorcpfp" description:"The amount in satoshis to allocate as the budget to pay fees when CPFPing a force close tx using the anchor output. If set, the budget calculated using the ratio (if set) will be capped at this value."`
	AnchorCPFPRatio float64        `long:"anchorcpfpratio" description:"The ratio of a special value to allocate as the budget to pay fees when CPFPing a force close tx using the anchor output. The special value is the sum of all time-sensitive HTLCs on this commitment subtracted by their budgets."`

	DeadlineHTLC      btcutil.Amount `long:"deadlinehtlc" description:"The amount in satoshis to allocate as the budget to pay fees when sweeping a time-sensitive (first-level) HTLC. If set, the budget calculated using the ratio (if set) will be capped at this value."`
	DeadlineHTLCRatio float64        `long:"deadlinehtlcratio" description:"The ratio of the value in a time-sensitive (first-level) HTLC to allocate as the budget to pay fees when sweeping it."`

	NoDeadlineHTLC      btcutil.Amount `long:"nodeadlinehtlc" description:"The amount in satoshis to allocate as the budget to pay fees when sweeping a non-time-sensitive (second-level) HTLC. If set, the budget calculated using the ratio (if set) will be capped at this value."`
	NoDeadlineHTLCRatio float64        `long:"nodeadlinehtlcratio" description:"The ratio of the value in a non-time-sensitive (second-level) HTLC to allocate as the budget to pay fees when sweeping it."`
}

// Validate checks the budget configuration for any invalid values.
func (b *BudgetConfig) Validate() error {
	// Exit early if no budget config is set.
	if b == nil {
		return fmt.Errorf("no budget config set")
	}

	// Sanity check all fields.
	if b.ToLocal != 0 && b.ToLocal < MinBudgetValue {
		return fmt.Errorf("tolocal must be at least %v",
			MinBudgetValue)
	}
	if b.ToLocalRatio != 0 && b.ToLocalRatio < MinBudgetRatio {
		return fmt.Errorf("tolocalratio must be at least %v",
			MinBudgetRatio)
	}

	if b.AnchorCPFP != 0 && b.AnchorCPFP < MinBudgetValue {
		return fmt.Errorf("anchorcpfp must be at least %v",
			MinBudgetValue)
	}
	if b.AnchorCPFPRatio != 0 && b.AnchorCPFPRatio < MinBudgetRatio {
		return fmt.Errorf("anchorcpfpratio must be at least %v",
			MinBudgetRatio)
	}

	if b.DeadlineHTLC != 0 && b.DeadlineHTLC < MinBudgetValue {
		return fmt.Errorf("deadlinehtlc must be at least %v",
			MinBudgetValue)
	}
	if b.DeadlineHTLCRatio != 0 && b.DeadlineHTLCRatio < MinBudgetRatio {
		return fmt.Errorf("deadlinehtlcratio must be at least %v",
			MinBudgetRatio)
	}

	if b.NoDeadlineHTLC != 0 && b.NoDeadlineHTLC < MinBudgetValue {
		return fmt.Errorf("nodeadlinehtlc must be at least %v",
			MinBudgetValue)
	}
	if b.NoDeadlineHTLCRatio != 0 &&
		b.NoDeadlineHTLCRatio < MinBudgetRatio {

		return fmt.Errorf("nodeadlinehtlcratio must be at least %v",
			MinBudgetRatio)
	}

	return nil
}

// String returns a human-readable description of the budget configuration.
func (b *BudgetConfig) String() string {
	return fmt.Sprintf("tolocal=%v tolocalratio=%v anchorcpfp=%v "+
		"anchorcpfpratio=%v deadlinehtlc=%v deadlinehtlcratio=%v "+
		"nodeadlinehtlc=%v nodeadlinehtlcratio=%v",
		b.ToLocal, b.ToLocalRatio, b.AnchorCPFP, b.AnchorCPFPRatio,
		b.DeadlineHTLC, b.DeadlineHTLCRatio, b.NoDeadlineHTLC,
		b.NoDeadlineHTLCRatio)
}

// DefaultSweeperConfig returns the default configuration for the sweeper.
func DefaultBudgetConfig() *BudgetConfig {
	return &BudgetConfig{
		ToLocalRatio:        DefaultBudgetRatio,
		AnchorCPFPRatio:     DefaultBudgetRatio,
		DeadlineHTLCRatio:   DefaultBudgetRatio,
		NoDeadlineHTLCRatio: DefaultBudgetRatio,
	}
}

// calculateBudget takes an output value, a configured ratio and budget value,
// and returns the budget to use for sweeping the output. If the budget value
// is set, it will be used as cap.
func calculateBudget(value btcutil.Amount, ratio float64,
	max btcutil.Amount) btcutil.Amount {

	// If ratio is not set, using the default value.
	if ratio == 0 {
		ratio = DefaultBudgetRatio
	}

	budget := value.MulF64(ratio)

	log.Tracef("Calculated budget=%v using value=%v, ratio=%v, cap=%v",
		budget, value, ratio, max)

	if max != 0 && budget > max {
		log.Debugf("Calculated budget=%v is capped at %v", budget, max)
		return max
	}

	return budget
}
