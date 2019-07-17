// +build !routerrpc

package routerrpc

import "github.com/lightningnetwork/lnd/routing"

// Config is the default config struct for the package. When the build tag isn't
// specified, then we output a blank config.
type Config struct{}

// DefaultConfig defines the config defaults. Without the sub server enabled,
// there are no defaults to set.
func DefaultConfig() *Config {
	return &Config{}
}

// GetRoutingConfig returns the routing config based on this sub server config.
func GetRoutingConfig(cfg *Config) *RoutingConfig {
	return &RoutingConfig{
		AprioriHopProbability: routing.DefaultAprioriHopProbability,
		MinRouteProbability:   routing.DefaultMinRouteProbability,
		PaymentAttemptPenalty: routing.DefaultPaymentAttemptPenalty,
		PenaltyHalfLife:       routing.DefaultPenaltyHalfLife,
	}
}
