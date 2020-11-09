// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_gossip_timestamp_range is used by go-fuzz.
func Fuzz_gossip_timestamp_range(data []byte) int {
	// Prefix with MsgGossipTimestampRange.
	data = prefixWithMsgType(data, lnwire.MsgGossipTimestampRange)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.GossipTimestampRange{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
