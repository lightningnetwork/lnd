package routerrpc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

// cfgMissionControl is a MissionControl that only stores its config.
type cfgMissionControl struct {
	MissionControl

	cfg *routing.MissionControlConfig
}

func (m *cfgMissionControl) GetConfig() *routing.MissionControlConfig {
	cfg := *m.cfg
	return &cfg
}

func (m *cfgMissionControl) SetConfig(cfg *routing.MissionControlConfig) error {
	m.cfg = cfg
	return nil
}

// TestSetMissionControlConfigFeeLevels tests that a config update without fee
// level parameters keeps the current fee level config, and that an update with
// them keeps the load option.
func TestSetMissionControlConfigFeeLevels(t *testing.T) {
	t.Parallel()

	current := routing.InterpretCfg{
		FeeLevelPPM:       100,
		FeeLevelMax:       1000,
		FeeLevelReach:     1,
		FeeLevelDecay:     time.Hour,
		FeeLevelSymmetric: true,
		FeeLevelLoad:      true,
	}
	mc := &cfgMissionControl{
		cfg: &routing.MissionControlConfig{InterpretCfg: current},
	}
	s := &Server{cfg: &Config{
		RouterBackend: &RouterBackend{MissionControl: mc},
	}}

	req := func(
		params *FeeLevelParameters) *SetMissionControlConfigRequest {

		return &SetMissionControlConfigRequest{
			Config: &MissionControlConfig{
				Model: MissionControlConfig_APRIORI,
				EstimatorConfig: &MissionControlConfig_Apriori{
					Apriori: &AprioriParameters{
						HalfLifeSeconds:  3600,
						HopProbability:   0.6,
						Weight:           0.5,
						CapacityFraction: 0.9,
					},
				},
				FeeLevelParameters: params,
			},
		}
	}

	ctx := t.Context()
	_, err := s.SetMissionControlConfig(ctx, req(nil))
	require.NoError(t, err)
	require.Equal(t, current, mc.cfg.InterpretCfg)

	_, err = s.SetMissionControlConfig(ctx, req(&FeeLevelParameters{
		LevelPpm:     200,
		Reach:        2,
		DecaySeconds: 60,
		Symmetric:    false,
		MaxLevelPpm:  500,
	}))
	require.NoError(t, err)
	require.Equal(t, routing.InterpretCfg{
		FeeLevelPPM:   200,
		FeeLevelMax:   500,
		FeeLevelReach: 2,
		FeeLevelDecay: time.Minute,
		FeeLevelLoad:  true,
	}, mc.cfg.InterpretCfg)
}
