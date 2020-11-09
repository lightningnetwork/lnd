// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_shutdown is used by go-fuzz.
func Fuzz_shutdown(data []byte) int {
	// Prefix with MsgShutdown.
	data = prefixWithMsgType(data, lnwire.MsgShutdown)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.Shutdown{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
