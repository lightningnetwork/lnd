// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_ping is used by go-fuzz.
func Fuzz_ping(data []byte) int {
	// Prefix with MsgPing.
	data = prefixWithMsgType(data, lnwire.MsgPing)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.Ping{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
