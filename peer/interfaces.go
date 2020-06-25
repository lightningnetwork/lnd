package peer

import "github.com/lightningnetwork/lnd/lnwire"

// LinkUpdater is an interface implemented by most messages in BOLT 2 that are
// allowed to update the channel state.
type LinkUpdater interface {
	// TargetChanID returns the channel id of the link for which this message
	// is intended.
	TargetChanID() lnwire.ChannelID
}
