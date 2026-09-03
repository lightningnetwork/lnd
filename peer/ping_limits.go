package peer

import "golang.org/x/time/rate"

const (
	// pingFloodRate admits substantially more inbound Pings than an honest
	// keepalive cadence while placing a finite bound on sustained floods.
	pingFloodRate rate.Limit = 10

	// pingFloodBurst tolerates transient bursts before the peer is treated
	// as a flood source and disconnected by the read loop.
	pingFloodBurst = 200
)

// defaultPingLimiter constructs independent flood state for a new peer. The
// fixed rate leaves ample room above normal keepalive traffic while bounding
// sustained request floods without exposing a redundant policy wrapper.
func defaultPingLimiter() *rate.Limiter {
	// Permit ten Pings per second and a burst of 200 before treating the
	// connection as a flood source.
	return rate.NewLimiter(pingFloodRate, pingFloodBurst)
}
