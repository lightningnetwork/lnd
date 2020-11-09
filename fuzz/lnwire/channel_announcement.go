// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_channel_announcement is used by go-fuzz.
func Fuzz_channel_announcement(data []byte) int {
	// Prefix with MsgChannelAnnouncement.
	data = prefixWithMsgType(data, lnwire.MsgChannelAnnouncement)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.ChannelAnnouncement{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
