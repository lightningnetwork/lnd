package routerrpc

import (
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
)

// RoutingConfig contains the configurable parameters that control routing.
//
//nolint:ll
type RoutingConfig struct {
	// ProbabilityEstimatorType sets the estimator to use.
	ProbabilityEstimatorType string `long:"estimator" choice:"apriori" choice:"bimodal" description:"Probability estimator used for pathfinding." `

	// MinRouteProbability is the minimum required route success probability
	// to attempt the payment.
	MinRouteProbability float64 `long:"minrtprob" description:"Minimum required route success probability to attempt the payment"`

	// AttemptCost is the fixed virtual cost in path finding of a failed
	// payment attempt. It is used to trade off potentially better routes
	// against their probability of succeeding.
	AttemptCost btcutil.Amount `long:"attemptcost" description:"The fixed (virtual) cost in sats of a failed payment attempt"`

	// AttemptCostPPM is the proportional virtual cost in path finding of a
	// failed payment attempt. It is used to trade off potentially better
	// routes against their probability of succeeding. This parameter is
	// expressed in parts per million of the total payment amount.
	AttemptCostPPM int64 `long:"attemptcostppm" description:"The proportional (virtual) cost in sats of a failed payment attempt expressed in parts per million of the total payment amount"`

	// MaxMcHistory defines the maximum number of payment results that
	// are held on disk by mission control.
	MaxMcHistory int `long:"maxmchistory" description:"the maximum number of payment results that are held on disk by mission control"`

	// McFlushInterval defines the timer interval to use to flush mission
	// control state to the DB.
	McFlushInterval time.Duration `long:"mcflushinterval" description:"the timer interval to use to flush mission control state to the DB"`

	// AprioriConfig defines parameters for the apriori probability.
	AprioriConfig *AprioriConfig `group:"apriori" namespace:"apriori" description:"configuration for the apriori pathfinding probability estimator"`

	// BimodalConfig defines parameters for the bimodal probability.
	BimodalConfig *BimodalConfig `group:"bimodal" namespace:"bimodal" description:"configuration for the bimodal pathfinding probability estimator"`

	// FeeEstimationTimeout is the maximum time to wait for routing fees to be estimated.
	FeeEstimationTimeout time.Duration `long:"fee-estimation-timeout" description:"the maximum time to wait for routing fees to be estimated by payment probes"`

	// FeeLevelConfig controls the experimental fee level mechanism. It is a
	// value, so a config without it gives the zero config, which turns fee
	// levels off.
	FeeLevelConfig FeeLevelConfig `group:"fee-level" namespace:"fee-level" description:"EXPERIMENTAL: configuration for the fee level mechanism. It can pick routes with higher fees to make fewer payment attempts. Any routing node on a route can raise fee levels with temporary channel failures."`
}

// FeeLevelConfig controls the fee level mechanism. The mechanism is
// experimental: it can pick routes with higher fees to make fewer payment
// attempts.
//
//nolint:ll
type FeeLevelConfig struct {
	// PPM is the fee level increase for routing failures in terms of a fee
	// rate. Fee level increase most strongly for the failing node. Fee
	// levels of neighboring nodes along the route are increased less
	// strongly, controlled by Reach.
	PPM uint64 `long:"ppm" description:"EXPERIMENTAL: The fee level increase for routing failures. The fee level increases most strongly for the failing node to reach. The value 0 turns fee levels off. The recommended value is 50. It must not be above fee-level.max. A higher value can give routes with higher fees."`

	// Max is the largest fee level of a node in parts per million. It must
	// be at least PPM.
	Max uint64 `long:"max" description:"The largest fee level of a node in parts per million. It bounds the extra fee that a fee level can make pathfinding accept on one channel. It must be at least fee-level.ppm and at most 1000000."`

	// Reach is the correlation factor for fee levels of neighboring nodes
	// along the route in units of hops. The larger this value, the more
	// neighbors are affected by the fee level increase of the failing node.
	Reach float64 `long:"reach" description:"The correlation factor for fee levels of neighboring nodes along the route in units of hops."`

	// Decay is the time it takes for a fee level to decay to 50% of its
	// initial value. The decay is exponential.
	Decay time.Duration `long:"decay" description:"The time it takes for a fee level to decay to 50% of its initial value."`

	// Symmetric is the flag that also applies a fee level change to the
	// successful hops before the failing node. It is off by default. With
	// it on, any node on the route can raise the fee levels of the honest
	// nodes before it. It can be useful for rebalancing.
	Symmetric bool `long:"symmetric" description:"If true, the fee level is also applied to the successful hops before the failing node. This can be useful for rebalancing. Any routing node on a route can then raise the fee levels of the nodes before it with temporary channel failures."`

	// Load is the flag that indicates whether fee levels are rebuilt at
	// startup from the stored payment results of the last four decay
	// half-lives. If false, fee levels start at zero. The rebuild uses the
	// config at startup, so a change of PPM, Max, Reach, Decay or
	// Symmetric also changes the rebuilt fee levels.
	Load bool `long:"load" description:"If true, fee levels are rebuilt at startup from the stored payment results of the last four decay half-lives, with the current fee level config. If false, fee levels start at zero. Do not change fee-level.ppm, fee-level.max, fee-level.reach, fee-level.decay or fee-level.symmetric before a restart, because the rebuild applies the new values to past results."`
}

// AprioriConfig defines parameters for the apriori probability.
//
//nolint:ll
type AprioriConfig struct {
	// HopProbability is the assumed success probability of a hop in a route
	// when no other information is available.
	HopProbability float64 `long:"hopprob" description:"Assumed success probability of a hop in a route when no other information is available."`

	// Weight is a value in the range [0, 1] that defines to what extent
	// historical results should be extrapolated to untried connections.
	// Setting it to one will completely ignore historical results and
	// always assume the configured a priori probability for untried
	// connections. A value of zero will ignore the a priori probability
	// completely and only base the probability on historical results,
	// unless there are none available.
	Weight float64 `long:"weight" description:"Weight of the a priori probability in success probability estimation. Valid values are in [0, 1]."`

	// PenaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	PenaltyHalfLife time.Duration `long:"penaltyhalflife" description:"Defines the duration after which a penalized node or channel is back at 50% probability"`

	// CapacityFraction defines the fraction of channels' capacities that is considered liquid.
	CapacityFraction float64 `long:"capacityfraction" description:"Defines the fraction of channels' capacities that is considered liquid. Valid values are in [0.75, 1]."`
}

// BimodalConfig defines parameters for the bimodal probability.
//
//nolint:ll
type BimodalConfig struct {
	// Scale describes the scale over which channels still have some
	// liquidity left on both channel ends. A value of 0 means that we
	// assume perfectly unbalanced channels, a very high value means
	// randomly balanced channels.
	Scale int64 `long:"scale" description:"Defines the unbalancedness assumed for the network, the amount defined in msat."`

	// NodeWeight defines how strongly non-routed channels should be taken
	// into account for probability estimation. Valid values are in [0,1].
	NodeWeight float64 `long:"nodeweight" description:"Defines how strongly non-routed channels should be taken into account for probability estimation. Valid values are in [0, 1]."`

	// DecayTime is the scale for the exponential information decay over
	// time for previous successes or failures.
	DecayTime time.Duration `long:"decaytime" description:"Describes the information decay of knowledge about previous successes and failures in channels."`
}
