package chanfitness

import (
	"time"

	"github.com/btcsuite/btcd/wire"
)

// peerMonitor is an interface implemented by entities that monitor our peers
// online events and the channels we currently have open with them.
type peerMonitor interface {
	// event adds an online or offline event.
	onlineEvent(online bool)

	// addChannel adds a new channel.
	addChannel(channelPoint wire.OutPoint) error

	// removeChannel removes a channel.
	removeChannel(channelPoint wire.OutPoint) error

	// channelCount returns the number of channels that we currently have
	// with the peer.
	channelCount() int

	// channelUptime looks up a channel and returns the amount of time that
	// the channel has been monitored for and its uptime over this period.
	channelUptime(channelPoint wire.OutPoint) (time.Duration,
		time.Duration, error)

	// getFlapCount returns the peer's flap count and the timestamp that we
	// last recorded a flap, which may be nil if we have never recorded a
	// flap for this peer.
	getFlapCount() (int, *time.Time)
}
