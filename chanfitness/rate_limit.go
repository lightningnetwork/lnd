package chanfitness

import "time"

// rateLimitScale is the number of events we allow per rate limited tier.
// Increasing this value makes our rate limiting more lenient, decreasing it
// makes us less lenient.
const rateLimitScale = 200

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
