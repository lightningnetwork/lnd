package peer

import "golang.org/x/time/rate"

const (
	// pongReplyRate refills the reply budget quickly enough for normal
	// keepalives while bounding sustained amplification from remote Pings.
	pongReplyRate rate.Limit = 1

	// pongReplyBurst absorbs short keepalive bursts without suppressing a
	// reply before the sustained-rate policy has time to take effect.
	pongReplyBurst = 20

	// pingFloodRate admits substantially more inbound Pings than an honest
	// keepalive cadence while placing a finite bound on sustained floods.
	pingFloodRate rate.Limit = 10

	// pingFloodBurst tolerates transient bursts before the peer is treated
	// as a flood source and disconnected by the read loop.
	pingFloodBurst = 200
)

// pingLimits holds the stateful limiters for the two inbound Ping policies.
// Keeping them together makes their different outcomes explicit without
// exposing fixed denial-of-service thresholds as operator configuration.
type pingLimits struct {
	// pongLimiter controls whether a valid Ping receives a Pong. Exhausting
	// this limiter suppresses the reply but leaves the connection active.
	pongLimiter *rate.Limiter

	// pingLimiter counts every inbound Ping. Exhausting this limiter
	// disconnects the peer, including for Pings that request no reply.
	pingLimiter *rate.Limiter
}

// defaultPingLimits constructs independent limiter state for a new peer. The
// selected rates leave ample room above normal keepalive traffic while
// separating reply suppression from flood teardown.
func defaultPingLimits() pingLimits {
	return pingLimits{
		// Refill one Pong per second and absorb a 20-Ping burst,
		// leaving wide headroom above honest keepalives.
		pongLimiter: rate.NewLimiter(
			pongReplyRate, pongReplyBurst,
		),

		// Permit ten Pings per second and a burst of 200 before
		// treating the connection as a flood source.
		pingLimiter: rate.NewLimiter(
			pingFloodRate, pingFloodBurst,
		),
	}
}
