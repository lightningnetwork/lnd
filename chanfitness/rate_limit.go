package chanfitness

import (
	"math"
	"time"
)

const (
	// rateLimitScale is the number of events we allow per rate limited
	// tier. Increasing this value makes our rate limiting more lenient,
	// decreasing it makes us less lenient.
	rateLimitScale = 200

	// flapCountCooldownFactor is the factor by which we decrease a peer's
	// flap count if they have not flapped for the cooldown period.
	flapCountCooldownFactor = 0.95

	// flapCountCooldownPeriod is the amount of time that we require a peer
	// has not flapped for before we reduce their all time flap count using
	// our cooldown factor.
	flapCountCooldownPeriod = time.Hour * 8
)

// rateLimits is the set of rate limit tiers we apply to our peers based on
// their flap count. A peer can be placed in their tier by dividing their flap
// count by the rateLimitScale and returning the value at that index.
var rateLimits = []time.Duration{
	time.Second,
	time.Second * 5,
	time.Second * 30,
	time.Minute,
	time.Minute * 30,
	time.Hour,
}

// getRateLimit returns the value of the rate limited tier that we are on based
// on current flap count. If a peer's flap count exceeds the top tier, we just
// return our highest tier.
func getRateLimit(flapCount int) time.Duration {
	// Figure out the tier we fall into based on our current flap count.
	tier := flapCount / rateLimitScale

	// If we have more events than our number of tiers, we just use the
	// last tier
	tierLen := len(rateLimits)
	if tier >= tierLen {
		tier = tierLen - 1
	}

	return rateLimits[tier]
}

// cooldownFlapCount takes a timestamped flap count, and returns its value
// scaled down by our cooldown factor if at least our cooldown period has
// elapsed since the peer last flapped. We do this because we store all-time
// flap count for peers, and want to allow downgrading of peers that have not
// flapped for a long time.
func cooldownFlapCount(now time.Time, flapCount int,
	lastFlap time.Time) int {

	// Calculate time since our last flap, and the number of times we need
	// to apply our cooldown factor.
	timeSinceFlap := now.Sub(lastFlap)

	// If our cooldown period has not elapsed yet, we just return our flap
	// count. We allow fractional cooldown periods once this period has
	// elapsed, so we do not want to apply a fractional cooldown before the
	// full cooldown period has elapsed.
	if timeSinceFlap < flapCountCooldownPeriod {
		return flapCount
	}

	// Get the factor by which we need to cooldown our flap count. If
	// insufficient time has passed to cooldown our flap count. Use use a
	// float so that we allow fractional cooldown periods.
	cooldownPeriods := float64(timeSinceFlap) /
		float64(flapCountCooldownPeriod)

	effectiveFactor := math.Pow(flapCountCooldownFactor, cooldownPeriods)

	return int(float64(flapCount) * effectiveFactor)
}
