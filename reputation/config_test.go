package reputation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConfigValidate exercises the config validation table.
func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "default is valid",
			cfg:  DefaultConfig(),
		},
		{
			name: "zero resolution period invalid",
			cfg: Config{
				RevenueWindow:        time.Hour,
				ReputationMultiplier: 12,
				RevenueWindowCount:   6,
			},
			wantErr: true,
		},
		{
			name: "zero revenue window invalid",
			cfg: Config{
				ResolutionPeriod:     time.Second,
				ReputationMultiplier: 12,
				RevenueWindowCount:   6,
			},
			wantErr: true,
		},
		{
			name: "zero multiplier invalid",
			cfg: Config{
				ResolutionPeriod:   time.Second,
				RevenueWindow:      time.Hour,
				RevenueWindowCount: 6,
			},
			wantErr: true,
		},
		{
			name: "zero revenue window count invalid",
			cfg: Config{
				ResolutionPeriod:     time.Second,
				RevenueWindow:        time.Hour,
				ReputationMultiplier: 12,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}
