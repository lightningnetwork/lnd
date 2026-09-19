package routing

import "github.com/lightningnetwork/lnd/lnwire"

const (
	// DefaultPaymentRouter selects lnd's production routing stack, which is
	// Dijkstra over a probability estimator with mission control behind it,
	// and reactive halving of the shard amount when no route is found.
	DefaultPaymentRouter = "default"

	// IntervalPaymentRouter selects the interval router, which replaces
	// mission control with a per directed channel liquidity interval and
	// plans the shard amount and the route together.
	IntervalPaymentRouter = "interval"
)

// Search bounds of the interval router. The label setting search is more
// expensive than a single distance Dijkstra by construction, since a node may
// keep several incomparable labels and every rung of the shard ladder is priced
// with its own search, so each of these is a real ceiling rather than a
// formality.
const (
	// DefaultIntervalMaxRouteHops is the longest route the search will
	// build. It is well above the twenty hop limit an onion can express,
	// and the payload size check is what actually binds first.
	DefaultIntervalMaxRouteHops = 24

	// DefaultIntervalMaxLabels is how many Pareto-incomparable labels a
	// single node may keep.
	DefaultIntervalMaxLabels = 24

	// DefaultIntervalSearchLimit is how many node expansions a single
	// search may perform before it gives up and returns the best route it
	// has, if any.
	DefaultIntervalSearchLimit = 120000

	// DefaultIntervalAttemptLimit is how many HTLCs one payment may spend
	// before the session stops handing out routes. The payment lifecycle
	// has its own bounds, the payment timeout and the part limit among
	// them; this one exists so that a session which believes it can always
	// find one more route cannot spin forever.
	DefaultIntervalAttemptLimit = 80

	// DefaultIntervalMaxShards caps how many pieces the shard ladder will
	// consider cutting a payment into, independently of the part limit the
	// payment itself carries.
	DefaultIntervalMaxShards = 64

	// DefaultIntervalMaxLadderRungs caps how many candidate shard sizes are
	// priced for a single route request. Every rung costs a full search, so
	// this is the knob that decides what one call to RequestRoute costs.
	// The rungs are enumerated in order of how much they are worth pricing,
	// so a cap keeps the most informative ones.
	DefaultIntervalMaxLadderRungs = 16
)

// IntervalConfig holds the tunables of the interval router. The defaults are
// the values the algorithm was validated with, and they are exposed here so
// that they can be moved in a test rather than because an operator is expected
// to turn them.
type IntervalConfig struct {
	// MaxRouteHops is the longest route the search will build.
	MaxRouteHops uint16

	// MaxLabels is how many incomparable labels a node may keep.
	MaxLabels int

	// SearchLimit bounds the number of expansions of a single search.
	SearchLimit int

	// AttemptLimit bounds the number of HTLCs one payment may spend.
	AttemptLimit uint32

	// MaxShards caps the number of pieces the shard ladder considers.
	MaxShards uint32

	// MaxLadderRungs caps how many candidate shard sizes are priced for a
	// single route request.
	MaxLadderRungs int

	// MinShardAmt is the smallest shard the router will send. Below it, a
	// payment that still cannot be routed is given up on rather than cut
	// any finer.
	MinShardAmt lnwire.MilliSatoshi

	// DisableQuarantine stops the router holding an ambiguous failure as
	// soft evidence against the channels that could have caused it. With it
	// set, a failure that cannot name a hop leaves nothing behind in the
	// node wide store and is handled entirely within the payment, which is
	// what the router did before the quarantine existed.
	//
	// The sense is inverted so that the zero value keeps the behaviour the
	// router was validated with. The mechanism measured as a null on the
	// tiers built to reward it, so this exists to make turning it off a
	// configuration change rather than a patch.
	DisableQuarantine bool
}

// DefaultIntervalConfig returns the configuration the interval router was
// validated with.
func DefaultIntervalConfig() IntervalConfig {
	return IntervalConfig{
		MaxRouteHops:   DefaultIntervalMaxRouteHops,
		MaxLabels:      DefaultIntervalMaxLabels,
		SearchLimit:    DefaultIntervalSearchLimit,
		AttemptLimit:   DefaultIntervalAttemptLimit,
		MaxShards:      DefaultIntervalMaxShards,
		MaxLadderRungs: DefaultIntervalMaxLadderRungs,
		MinShardAmt:    DefaultShardMinAmt,
	}
}

// fillDefaults replaces any unset field with its default, so that a zero valued
// config is usable.
func (c *IntervalConfig) fillDefaults() {
	defaults := DefaultIntervalConfig()

	if c.MaxRouteHops == 0 {
		c.MaxRouteHops = defaults.MaxRouteHops
	}
	if c.MaxLabels <= 0 {
		c.MaxLabels = defaults.MaxLabels
	}
	if c.SearchLimit <= 0 {
		c.SearchLimit = defaults.SearchLimit
	}
	if c.AttemptLimit == 0 {
		c.AttemptLimit = defaults.AttemptLimit
	}
	if c.MaxShards == 0 {
		c.MaxShards = defaults.MaxShards
	}
	if c.MaxLadderRungs <= 0 {
		c.MaxLadderRungs = defaults.MaxLadderRungs
	}
	if c.MinShardAmt == 0 {
		c.MinShardAmt = defaults.MinShardAmt
	}
}
