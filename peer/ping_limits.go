package peer

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/time/rate"
)

const (
	// pingFloodRate admits substantially more inbound Pings than an honest
	// keepalive cadence while placing a finite bound on sustained floods.
	pingFloodRate rate.Limit = 10

	// pingFloodBurst tolerates transient bursts before the peer is treated
	// as a flood source and disconnected by the read loop.
	pingFloodBurst = 200

	// pingResponseBytesPerToken scales admission by the requested
	// Pong size. The quantum maps the largest valid response to ten tokens,
	// preserving the former worst-case reply bandwidth without penalizing
	// lnd's normal requests of at most 4,096 bytes.
	pingResponseBytesPerToken = 6554
)

// defaultPingLimiter constructs independent flood state for a new peer. The
// fixed rate leaves ample room above normal keepalive traffic while bounding
// sustained request floods without exposing a redundant policy wrapper.
func defaultPingLimiter() *rate.Limiter {
	// Refill ten tokens per second and allow a burst of 200 before treating
	// the connection as a flood source.
	return rate.NewLimiter(pingFloodRate, pingFloodBurst)
}

// calcPingCost scales admitted Ping work by requested Pong size so one limiter
// bounds both request rate and response bandwidth. Oversized requests use one
// token because BOLT 1 requires no reply, but still consume flood capacity.
func calcPingCost(ping *lnwire.Ping) int {
	if ping.NumPongBytes > lnwire.MaxPongBytes {
		return 1
	}

	requestedBytes := int(ping.NumPongBytes)

	return max(
		1, (requestedBytes+pingResponseBytesPerToken-1)/
			pingResponseBytesPerToken,
	)
}
