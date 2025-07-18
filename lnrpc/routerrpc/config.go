package routerrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing"
)

// Config is the main configuration file for the router RPC server. It contains
// all the items required for the router RPC server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
//
//nolint:ll
type Config struct {
	RoutingConfig

	// UseStatusInitiated is a boolean that indicates whether the router
	// should use the new status code `Payment_INITIATED`.
	//
	// TODO(yy): remove this config after the new status code is fully
	// deployed to the network(v0.20.0).
	UseStatusInitiated bool `long:"usestatusinitiated" description:"If true, the router will send Payment_INITIATED for new payments, otherwise Payment_In_FLIGHT will be sent for compatibility concerns."`

	// RouterMacPath is the path for the router macaroon. If unspecified
	// then we assume that the macaroon will be found under the network
	// directory, named DefaultRouterMacFilename.
	RouterMacPath string `long:"routermacaroonpath" description:"Path to the router macaroon"`

	// NetworkDir is the main network directory wherein the router rpc
	// server will find the macaroon named DefaultRouterMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the Router rpc server.
	MacService *macaroons.Service

	// Router is the main channel router instance that backs this RPC
	// server.
	//
	// TODO(roasbeef): make into pkg lvl interface?
	//
	// TODO(roasbeef): assumes router handles saving payment state
	Router *routing.ChannelRouter

	// RouterBackend contains shared logic between this sub server and the
	// main rpc server.
	RouterBackend *RouterBackend

	// AliasMgr is the alias manager instance that is used to handle all the
	// SCID alias related information for channels.
	AliasMgr *aliasmgr.Manager

	// MPPValidator validates MPP record requirements for payments.
	MPPValidator *MPPValidator
}

// DefaultConfig defines the config defaults.
func DefaultConfig() *Config {
	defaultRoutingConfig := RoutingConfig{
		ProbabilityEstimatorType: routing.DefaultEstimator,
		MinRouteProbability:      routing.DefaultMinRouteProbability,

		AttemptCost:     routing.DefaultAttemptCost.ToSatoshis(),
		AttemptCostPPM:  routing.DefaultAttemptCostPPM,
		MaxMcHistory:    routing.DefaultMaxMcHistory,
		McFlushInterval: routing.DefaultMcFlushInterval,
		AprioriConfig: &AprioriConfig{
			HopProbability:   routing.DefaultAprioriHopProbability,
			Weight:           routing.DefaultAprioriWeight,
			PenaltyHalfLife:  routing.DefaultPenaltyHalfLife,
			CapacityFraction: routing.DefaultCapacityFraction,
		},
		BimodalConfig: &BimodalConfig{
			Scale:      int64(routing.DefaultBimodalScaleMsat),
			NodeWeight: routing.DefaultBimodalNodeWeight,
			DecayTime:  routing.DefaultBimodalDecayTime,
		},
		FeeEstimationTimeout: routing.DefaultFeeEstimationTimeout,
		MPPConfig: &MPPConfig{
			EnforcementMode:    "warn",
			MetricsEnabled:     true,
			EmergencyOverride:  false,
			GracePeriodDays:    90,
			DisableAutoUpgrade: false,
		},
	}

	return &Config{
		RoutingConfig: defaultRoutingConfig,
	}
}

// GetRoutingConfig returns the routing config based on this sub server config.
func GetRoutingConfig(cfg *Config) *RoutingConfig {
	return &RoutingConfig{
		ProbabilityEstimatorType: cfg.ProbabilityEstimatorType,
		MinRouteProbability:      cfg.MinRouteProbability,
		AttemptCost:              cfg.AttemptCost,
		AttemptCostPPM:           cfg.AttemptCostPPM,
		MaxMcHistory:             cfg.MaxMcHistory,
		McFlushInterval:          cfg.McFlushInterval,
		AprioriConfig: &AprioriConfig{
			HopProbability:   cfg.AprioriConfig.HopProbability,
			Weight:           cfg.AprioriConfig.Weight,
			PenaltyHalfLife:  cfg.AprioriConfig.PenaltyHalfLife,
			CapacityFraction: cfg.AprioriConfig.CapacityFraction,
		},
		BimodalConfig: &BimodalConfig{
			Scale:      cfg.BimodalConfig.Scale,
			NodeWeight: cfg.BimodalConfig.NodeWeight,
			DecayTime:  cfg.BimodalConfig.DecayTime,
		},
		FeeEstimationTimeout: cfg.FeeEstimationTimeout,
		MPPConfig: &MPPConfig{
			EnforcementMode:    cfg.MPPConfig.EnforcementMode,
			QueryRoutesMode:    cfg.MPPConfig.QueryRoutesMode,
			SendToRouteMode:    cfg.MPPConfig.SendToRouteMode,
			BuildRouteMode:     cfg.MPPConfig.BuildRouteMode,
			MetricsEnabled:     cfg.MPPConfig.MetricsEnabled,
			EmergencyOverride:  cfg.MPPConfig.EmergencyOverride,
			GracePeriodDays:    cfg.MPPConfig.GracePeriodDays,
			DisableAutoUpgrade: cfg.MPPConfig.DisableAutoUpgrade,
		},
	}
}

// GetMPPValidationConfig converts the routing config MPP settings to
// MPPValidationConfig.
func GetMPPValidationConfig(cfg *RoutingConfig) (*MPPValidationConfig, error) {
	if cfg.MPPConfig == nil {
		return DefaultMPPValidationConfig(), nil
	}

	// Parse global enforcement mode
	globalMode, err := ParseEnforcementMode(cfg.MPPConfig.EnforcementMode)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid global enforcement mode: %w", err)
	}

	// Parse per-RPC modes
	perRPCModes := make(map[string]EnforcementMode)

	if cfg.MPPConfig.QueryRoutesMode != "" {
		mode, err := ParseEnforcementMode(cfg.MPPConfig.QueryRoutesMode)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid QueryRoutes enforcement mode: %w", err)
		}
		perRPCModes["QueryRoutes"] = mode
	}

	if cfg.MPPConfig.SendToRouteMode != "" {
		mode, err := ParseEnforcementMode(cfg.MPPConfig.SendToRouteMode)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid SendToRoute enforcement mode: %w", err)
		}
		perRPCModes["SendToRoute"] = mode
	}

	if cfg.MPPConfig.BuildRouteMode != "" {
		mode, err := ParseEnforcementMode(cfg.MPPConfig.BuildRouteMode)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid BuildRoute enforcement mode: %w", err)
		}
		perRPCModes["BuildRoute"] = mode
	}

	return &MPPValidationConfig{
		GlobalMode:        globalMode,
		PerRPCModes:       perRPCModes,
		EmergencyOverride: cfg.MPPConfig.EmergencyOverride,
		MetricsEnabled:    cfg.MPPConfig.MetricsEnabled,
		GracePeriodDays:   cfg.MPPConfig.GracePeriodDays,
	}, nil
}
