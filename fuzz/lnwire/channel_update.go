// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_channel_update is used by go-fuzz.
func Fuzz_channel_update(data []byte) int {
	// Prefix with MsgChannelUpdate.
	data = prefixWithMsgType(data, lnwire.MsgChannelUpdate)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.ChannelUpdate{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
