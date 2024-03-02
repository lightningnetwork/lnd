package contractcourt

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestBudgetConfigValidate checks that the budget config validation works as
// expected.
func TestBudgetConfigValidate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		cfg            *BudgetConfig
		expectedErrStr string
	}{
		{
			name: "valid config",
			cfg:  DefaultBudgetConfig(),
		},
		{
			name:           "nil config",
			cfg:            nil,
			expectedErrStr: "no budget config set",
		},
		{
			name:           "invalid tolocal",
			cfg:            &BudgetConfig{ToLocal: -1},
			expectedErrStr: "tolocal",
		},
		{
			name:           "invalid tolocalratio",
			cfg:            &BudgetConfig{ToLocalRatio: -1},
			expectedErrStr: "tolocalratio",
		},
		{
			name:           "invalid anchorcpfp",
			cfg:            &BudgetConfig{AnchorCPFP: -1},
			expectedErrStr: "anchorcpfp",
		},
		{
			name:           "invalid anchorcpfpratio",
			cfg:            &BudgetConfig{AnchorCPFPRatio: -1},
			expectedErrStr: "anchorcpfpratio",
		},
		{
			name:           "invalid deadlinehtlc",
			cfg:            &BudgetConfig{DeadlineHTLC: -1},
			expectedErrStr: "deadlinehtlc",
		},
		{
			name:           "invalid deadlinehtlcratio",
			cfg:            &BudgetConfig{DeadlineHTLCRatio: -1},
			expectedErrStr: "deadlinehtlcratio",
		},

		{
			name:           "invalid nodeadlinehtlc",
			cfg:            &BudgetConfig{NoDeadlineHTLC: -1},
			expectedErrStr: "nodeadlinehtlc",
		},
		{
			name:           "invalid nodeadlinehtlcratio",
			cfg:            &BudgetConfig{NoDeadlineHTLCRatio: -1},
			expectedErrStr: "nodeadlinehtlcratio",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()

			if tc.expectedErrStr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tc.expectedErrStr)
		})
	}
}

// TestCalculateBudget checks that the budget calculation works as expected.
func TestCalculateBudget(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		value    btcutil.Amount
		ratio    float64
		max      btcutil.Amount
		expected btcutil.Amount
	}{
		{
			// When the ratio is not specified, the default 0.5
			// should be used.
			name:     "use default ratio",
			value:    btcutil.Amount(1000),
			ratio:    0,
			max:      0,
			expected: btcutil.Amount(500),
		},
		{
			// When the ratio is specified, the default is not
			// used.
			name:     "use specified ratio",
			value:    btcutil.Amount(1000),
			ratio:    0.1,
			max:      0,
			expected: btcutil.Amount(100),
		},
		{
			// When the max is specified, the budget should be
			// capped at that value.
			name:     "budget capped at max",
			value:    btcutil.Amount(1000),
			ratio:    0.1,
			max:      btcutil.Amount(1),
			expected: btcutil.Amount(1),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			budget := calculateBudget(tc.value, tc.ratio, tc.max)
			require.Equal(t, tc.expected, budget)
		})
	}
}
