package routerrpc

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
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

	// MPPConfig defines parameters for MPP enforcement.
	MPPConfig *MPPConfig `group:"mpp" namespace:"mpp" description:"configuration for MPP (Multi-Path Payment) enforcement"`
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

// MPPConfig defines parameters for MPP (Multi-Path Payment) enforcement.
//
//nolint:ll
type MPPConfig struct {
	// EnforcementMode sets the global MPP enforcement mode.
	EnforcementMode string `long:"enforcement-mode" choice:"legacy" choice:"warn" choice:"enforce" description:"Global MPP enforcement mode. Legacy disables validation, warn logs warnings, enforce rejects non-compliant payments."`

	// QueryRoutesMode overrides enforcement mode for QueryRoutes RPC.
	QueryRoutesMode string `long:"queryroutes-mode" choice:"legacy" choice:"warn" choice:"enforce" description:"Override enforcement mode for QueryRoutes RPC calls."`

	// SendToRouteMode overrides enforcement mode for SendToRoute RPC.
	SendToRouteMode string `long:"sendtoroute-mode" choice:"legacy" choice:"warn" choice:"enforce" description:"Override enforcement mode for SendToRoute RPC calls."`

	// BuildRouteMode overrides enforcement mode for BuildRoute RPC.
	BuildRouteMode string `long:"buildroute-mode" choice:"legacy" choice:"warn" choice:"enforce" description:"Override enforcement mode for BuildRoute RPC calls."`

	// MetricsEnabled controls whether MPP validation metrics are collected.
	MetricsEnabled bool `long:"metrics-enabled" description:"Enable collection of MPP validation metrics for monitoring and alerting."`

	// EmergencyOverride forces legacy mode when enabled, bypassing all validation.
	EmergencyOverride bool `long:"emergency-override" description:"Emergency override to disable all MPP validation. Use only in case of critical issues."`

	// GracePeriodDays specifies the number of days before auto-upgrading from legacy to warn mode.
	GracePeriodDays int `long:"grace-period-days" description:"Number of days grace period before automatically upgrading from legacy to warn mode. Set to 0 to disable."`

	// DisableAutoUpgrade prevents automatic mode upgrades based on time.
	DisableAutoUpgrade bool `long:"disable-auto-upgrade" description:"Disable automatic enforcement mode upgrades based on time or grace periods."`
}
