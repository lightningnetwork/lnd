package reputation

import (
	"testing"
	"time"
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
			name: "pct sum at 100 invalid",
			cfg: Config{
				GeneralPct: 60, CongestionPct: 40,
				ResolutionPeriod:     time.Second,
				RevenueWindow:        time.Hour,
				ReputationMultiplier: 12,
			},
			wantErr: true,
		},
		{
			name: "pct sum over 100 invalid",
			cfg: Config{
				GeneralPct: 70, CongestionPct: 40,
				ResolutionPeriod:     time.Second,
				RevenueWindow:        time.Hour,
				ReputationMultiplier: 12,
			},
			wantErr: true,
		},
		{
			name: "zero resolution period invalid",
			cfg: Config{
				GeneralPct: 40, CongestionPct: 20,
				RevenueWindow:        time.Hour,
				ReputationMultiplier: 12,
			},
			wantErr: true,
		},
		{
			name: "zero revenue window invalid",
			cfg: Config{
				GeneralPct: 40, CongestionPct: 20,
				ResolutionPeriod:     time.Second,
				ReputationMultiplier: 12,
			},
			wantErr: true,
		},
		{
			name: "zero multiplier invalid",
			cfg: Config{
				GeneralPct: 40, CongestionPct: 20,
				ResolutionPeriod: time.Second,
				RevenueWindow:    time.Hour,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestManagerStartStop is a smoke test for the lifecycle with no dependencies.
func TestManagerStartStop(t *testing.T) {
	t.Parallel()

	m, err := NewManager(DefaultConfig(), WithClock(newTestClock(1000)))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Hooks on an empty manager must be safe no-ops (other than lazy
	// channel creation) — they must never panic.
	m.OnSettle(circuit(1, 0), circuit(2, 0))
	m.OnFail(circuit(1, 0), circuit(2, 0))

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
