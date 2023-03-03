package routerrpc

import (
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing"
)

// Config is the main configuration file for the router RPC server. It contains
// all the items required for the router RPC server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	RoutingConfig

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
	}
}
