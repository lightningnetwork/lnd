package reputation

import "github.com/lightningnetwork/lnd/lnwire"

// ChannelInfo carries the per-channel capacity limits the reputation subsystem
// needs to size its resource buckets. These are read from the holder
// (local) side of the channel, matching the LDK reference's get_holder_max_*.
type ChannelInfo struct {
	// SCID is the short channel id of the channel.
	SCID lnwire.ShortChannelID

	// MaxAcceptedHTLCs is the maximum number of HTLCs allowed in flight.
	MaxAcceptedHTLCs uint16

	// MaxInFlightMsat is the maximum total HTLC value in flight.
	MaxInFlightMsat lnwire.MilliSatoshi
}

// ChannelEvent reports that a channel was opened or closed.
type ChannelEvent struct {
	// Info describes the channel.
	Info ChannelInfo

	// Open is true for an open event, false for a close.
	Open bool
}

// ChannelSource is the read-only seam through which the reputation subsystem
// learns about the node's channels and their capacity limits. It is the
// analogue of LDK's add_channel/remove_channel lifecycle. The server adapter
// wraps channelnotifier.ChannelNotifier and the channel database to implement
// it; tests provide a fake.
type ChannelSource interface {
	// ActiveChannels returns the currently open channels and their limits,
	// used to seed bucket sizing at startup.
	ActiveChannels() ([]ChannelInfo, error)

	// SubscribeChannelEvents returns a stream of open/close events and a
	// cancel func to tear the subscription down. The stream must be drained
	// until cancel is called.
	SubscribeChannelEvents() (<-chan ChannelEvent, func(), error)
}
