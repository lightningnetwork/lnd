package netann

import (
	"time"

	"github.com/btcsuite/btcd/wire"
)

// ChanStatus is a type that enumerates the possible states a ChanStatusManager
// tracks for its known channels.
type ChanStatus uint8

const (
	// ChanStatusEnabled indicates that the channel's last announcement has
	// the disabled bit cleared.
	ChanStatusEnabled ChanStatus = iota

	// ChanStatusPendingDisabled indicates that the channel's last
	// announcement has the disabled bit cleared, but that the channel was
	// detected in an inactive state. Channels in this state will have a
	// disabling announcement sent after the ChanInactiveTimeout expires
	// from the time of the first detection--unless the channel is
	// explicitly reenabled before the disabling occurs.
	ChanStatusPendingDisabled

	// ChanStatusDisabled indicates that the channel's last announcement has
	// the disabled bit set.
	ChanStatusDisabled
)

// ChannelState describes the ChanStatusManager's view of a channel, and
// describes the current state the channel's disabled status on the network.
type ChannelState struct {
	// Status is the channel's current ChanStatus from the POV of the
	// ChanStatusManager.
	Status ChanStatus

	// SendDisableTime is the earliest time at which the ChanStatusManager
	// will passively send a new disable announcement on behalf of this
	// channel.
	//
	// NOTE: This field is only non-zero if status is
	// ChanStatusPendingDisabled.
	SendDisableTime time.Time
}

// channelStates is a map of channel outpoints to their channelState. All
// changes made after setting an entry initially should be made using receiver
// methods below.
type channelStates map[wire.OutPoint]ChannelState

// markEnabled creates a channelState using ChanStatusEnabled.
func (s *channelStates) markEnabled(outpoint wire.OutPoint) {
	(*s)[outpoint] = ChannelState{
		Status: ChanStatusEnabled,
	}
}

// markDisabled creates a channelState using ChanStatusDisabled.
func (s *channelStates) markDisabled(outpoint wire.OutPoint) {
	(*s)[outpoint] = ChannelState{
		Status: ChanStatusDisabled,
	}
}

// markPendingDisabled creates a channelState using ChanStatusPendingDisabled
// and sets the ChannelState's SendDisableTime to sendDisableTime.
func (s *channelStates) markPendingDisabled(outpoint wire.OutPoint,
	sendDisableTime time.Time) {

	(*s)[outpoint] = ChannelState{
		Status:          ChanStatusPendingDisabled,
		SendDisableTime: sendDisableTime,
	}
}
