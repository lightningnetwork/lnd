// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_reply_short_chan_ids_end is used by go-fuzz.
func Fuzz_reply_short_chan_ids_end(data []byte) int {
	// Prefix with MsgReplyShortChanIDsEnd.
	data = prefixWithMsgType(data, lnwire.MsgReplyShortChanIDsEnd)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.ReplyShortChanIDsEnd{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
