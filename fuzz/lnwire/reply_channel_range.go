// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_reply_channel_range is used by go-fuzz.
func Fuzz_reply_channel_range(data []byte) int {
	// Prefix with MsgReplyChannelRange.
	data = prefixWithMsgType(data, lnwire.MsgReplyChannelRange)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.ReplyChannelRange{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
