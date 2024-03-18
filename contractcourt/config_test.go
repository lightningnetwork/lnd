package contractcourt

import (
	"testing"

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
