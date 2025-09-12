package chanfitness

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/clock"
)

type eventType int

const (
	peerOnlineEvent eventType = iota
	peerOfflineEvent
)

// String provides string representations of channel events.
func (e eventType) String() string {
	switch e {
	case peerOnlineEvent:
		return "peer_online"

	case peerOfflineEvent:
		return "peer_offline"
	}

	return "unknown"
}

type event struct {
	timestamp time.Time
	eventType eventType
}

// peerLog tracks events for a peer and its channels. If we currently have no
// channels with the peer, it will simply track its current online state. If we
// do have channels open with the peer, it will track the peer's online and
// offline events so that we can calculate uptime for our channels. A single
// event log is used for these online and offline events, and uptime for a
// channel is calculated by examining a subsection of this log.
type peerLog struct {
	// clock allows creation of deterministic unit tests.
	clock clock.Clock

	// channels contains a set of currently open channels. Channels will be
	// added and removed from this map as they are opened and closed.
	channels map[wire.OutPoint]*channelInfo
}

// newPeerLog creates a new peerLog instance, using the provided clock.
func newPeerLog(clock clock.Clock) *peerLog {
	return &peerLog{
		clock:    clock,
		channels: make(map[wire.OutPoint]*channelInfo),
	}
}

// channelInfo contains information about a channel.
type channelInfo struct {
	// openedAt tracks the first time this channel was seen. This is not
	// necessarily the time that it confirmed on chain because channel
	// events are not persisted at present.
	openedAt time.Time
}

func newChannelInfo(openedAt time.Time) *channelInfo {
	return &channelInfo{
		openedAt: openedAt,
	}
}

// addChannel adds a channel to our log. If we have not tracked any online
// events for our peer yet, we create one with our peer's current online state
// so that we know the state that the peer had at channel start, which is
// required to calculate uptime over the channel's lifetime.
func (p *peerLog) addChannel(channelPoint wire.OutPoint) error {
	_, ok := p.channels[channelPoint]
	if ok {
		return fmt.Errorf("channel: %v already present", channelPoint)
	}

	openTime := p.clock.Now()
	p.channels[channelPoint] = newChannelInfo(openTime)

	return nil
}

// removeChannel removes a channel from our log. If we have no more channels
// with the peer after removing this one, we clear our list of events.
func (p *peerLog) removeChannel(channelPoint wire.OutPoint) error {
	_, ok := p.channels[channelPoint]
	if !ok {
		return fmt.Errorf("channel: %v not present", channelPoint)
	}

	delete(p.channels, channelPoint)

	return nil
}

// channelCount returns the number of channels that we currently have
// with the peer.
func (p *peerLog) channelCount() int {
	return len(p.channels)
}

// channelLifetime looks up a channel and returns the amount of time that the
// channel has been monitored for.
func (p *peerLog) channelLifetime(channelPoint wire.OutPoint) (
	time.Duration, error) {

	channel, ok := p.channels[channelPoint]
	if !ok {
		return 0, ErrChannelNotFound
	}

	now := p.clock.Now()

	return now.Sub(channel.openedAt), nil
}
