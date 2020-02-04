// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_query_short_chan_ids is used by go-fuzz.
func Fuzz_query_short_chan_ids(data []byte) int {
	// Prefix with MsgQueryShortChanIDs.
	data = prefixWithMsgType(data, lnwire.MsgQueryShortChanIDs)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.QueryShortChanIDs{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
