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

	// ChanStatusManuallyDisabled indicates that the channel's last
	// announcement had the disabled bit set, and that a user manually
	// requested disabling the channel. Channels in this state will ignore
	// automatic / background attempts to re-enable the channel.
	//
	// Note that there's no corresponding ChanStatusManuallyEnabled state
	// because even if a user manually requests enabling a channel, we still
	// DO want to allow automatic / background processes to disable it.
	// Otherwise, the network might be cluttered with channels that are
	// advertised as enabled, but don't actually work or even exist.
	ChanStatusManuallyDisabled

	// ChanStatusBandwidthDisabled indicates that the channel's last
	// announcement had the disabled bit set, and that it was automatically
	// set due to a lower than required bandwidth on the channel.
	//
	// Note that returning to an enabled state due to sufficient bandwidth
	// will only happen if the the last announcement was a ChanStatusBandwidth,
	// as we otherwise expect manual or automatic channel managemtn to have
	// taken precedence instead (such as due to a peer not available).
	ChanStatusBandwidthDisabled
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

// markManuallyDisabled creates a channelState using
// ChanStatusManuallyDisabled.
func (s *channelStates) markManuallyDisabled(outpoint wire.OutPoint) {
	(*s)[outpoint] = ChannelState{
		Status: ChanStatusManuallyDisabled,
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

// markEnabled creates a channelState using ChanStatusEnabled.
func (s *channelStates) markBandwidthDisabled(outpoint wire.OutPoint) {
	(*s)[outpoint] = ChannelState{
		Status: ChanStatusBandwidthDisabled,
	}
}
