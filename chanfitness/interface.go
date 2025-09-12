package chanfitness

import (
	"time"

	"github.com/btcsuite/btcd/wire"
)

// peerMonitor is an interface implemented by entities that monitor our peers
// online events and the channels we currently have open with them.
type peerMonitor interface {
	// addChannel adds a new channel.
	addChannel(channelPoint wire.OutPoint) error

	// removeChannel removes a channel.
	removeChannel(channelPoint wire.OutPoint) error

	// channelCount returns the number of channels that we currently have
	// with the peer.
	channelCount() int

	// channelLifetime looks up a channel and returns the amount of time
	// that the channel has been monitored for.
	channelLifetime(channelPoint wire.OutPoint) (
		time.Duration, error)
}
