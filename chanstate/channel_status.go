package chanstate

import (
	"strconv"
	"strings"
)

// ChannelStatus is a bit vector used to indicate whether an OpenChannel is in
// the default usable state, or a state where it shouldn't be used.
type ChannelStatus uint64

var (
	// ChanStatusDefault is the normal state of an open channel.
	ChanStatusDefault ChannelStatus

	// ChanStatusBorked indicates that the channel has entered an
	// irreconcilable state, triggered by a state desynchronization or
	// channel breach.  Channels in this state should never be added to the
	// htlc switch.
	ChanStatusBorked ChannelStatus = 1

	// ChanStatusCommitBroadcasted indicates that a commitment for this
	// channel has been broadcasted.
	ChanStatusCommitBroadcasted ChannelStatus = 1 << 1

	// ChanStatusLocalDataLoss indicates that we have lost channel state
	// for this channel, and broadcasting our latest commitment might be
	// considered a breach.
	//
	// TODO(halseh): actually enforce that we are not force closing such a
	// channel.
	ChanStatusLocalDataLoss ChannelStatus = 1 << 2

	// ChanStatusRestored is a status flag that signals that the channel
	// has been restored, and doesn't have all the fields a typical channel
	// will have.
	ChanStatusRestored ChannelStatus = 1 << 3

	// ChanStatusCoopBroadcasted indicates that a cooperative close for
	// this channel has been broadcasted. Older cooperatively closed
	// channels will only have this status set. Newer ones will also have
	// close initiator information stored using the local/remote initiator
	// status. This status is set in conjunction with the initiator status
	// so that we do not need to check multiple channel statues for
	// cooperative closes.
	ChanStatusCoopBroadcasted ChannelStatus = 1 << 4

	// ChanStatusLocalCloseInitiator indicates that we initiated closing
	// the channel.
	ChanStatusLocalCloseInitiator ChannelStatus = 1 << 5

	// ChanStatusRemoteCloseInitiator indicates that the remote node
	// initiated closing the channel.
	ChanStatusRemoteCloseInitiator ChannelStatus = 1 << 6
)

// chanStatusStrings maps a ChannelStatus to a human friendly string that
// describes that status.
var chanStatusStrings = map[ChannelStatus]string{
	ChanStatusDefault:              "ChanStatusDefault",
	ChanStatusBorked:               "ChanStatusBorked",
	ChanStatusCommitBroadcasted:    "ChanStatusCommitBroadcasted",
	ChanStatusLocalDataLoss:        "ChanStatusLocalDataLoss",
	ChanStatusRestored:             "ChanStatusRestored",
	ChanStatusCoopBroadcasted:      "ChanStatusCoopBroadcasted",
	ChanStatusLocalCloseInitiator:  "ChanStatusLocalCloseInitiator",
	ChanStatusRemoteCloseInitiator: "ChanStatusRemoteCloseInitiator",
}

// orderedChanStatusFlags is an in-order list of all that channel status flags.
var orderedChanStatusFlags = []ChannelStatus{
	ChanStatusBorked,
	ChanStatusCommitBroadcasted,
	ChanStatusLocalDataLoss,
	ChanStatusRestored,
	ChanStatusCoopBroadcasted,
	ChanStatusLocalCloseInitiator,
	ChanStatusRemoteCloseInitiator,
}

// String returns a human-readable representation of the ChannelStatus.
func (c ChannelStatus) String() string {
	// If no flags are set, then this is the default case.
	if c == ChanStatusDefault {
		return chanStatusStrings[ChanStatusDefault]
	}

	// Add individual bit flags.
	statusStr := ""
	for _, flag := range orderedChanStatusFlags {
		if c&flag == flag {
			statusStr += chanStatusStrings[flag] + "|"
			c -= flag
		}
	}

	// Remove anything to the right of the final bar, including it as well.
	statusStr = strings.TrimRight(statusStr, "|")

	// Add any remaining flags which aren't accounted for as hex.
	if c != 0 {
		statusStr += "|0x" + strconv.FormatUint(uint64(c), 16)
	}

	// If this was purely an unknown flag, then remove the extra bar at the
	// start of the string.
	statusStr = strings.TrimLeft(statusStr, "|")

	return statusStr
}
