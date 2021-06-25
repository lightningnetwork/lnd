package routerrpc

import (
	"time"

	"github.com/btcsuite/btcutil"
)

// RoutingConfig contains the configurable parameters that control routing.
type RoutingConfig struct {
	// MinRouteProbability is the minimum required route success probability
	// to attempt the payment.
	MinRouteProbability float64 `long:"minrtprob" description:"Minimum required route success probability to attempt the payment"`

	// AprioriHopProbability is the assumed success probability of a hop in
	// a route when no other information is available.
	AprioriHopProbability float64 `long:"apriorihopprob" description:"Assumed success probability of a hop in a route when no other information is available."`

	// AprioriWeight is a value in the range [0, 1] that defines to what
	// extent historical results should be extrapolated to untried
	// connections. Setting it to one will completely ignore historical
	// results and always assume the configured a priori probability for
	// untried connections. A value of zero will ignore the a priori
	// probability completely and only base the probability on historical
	// results, unless there are none available.
	AprioriWeight float64 `long:"aprioriweight" description:"Weight of the a priori probability in success probability estimation. Valid values are in [0, 1]."`

	// PenaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	PenaltyHalfLife time.Duration `long:"penaltyhalflife" description:"Defines the duration after which a penalized node or channel is back at 50% probability"`

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
}
