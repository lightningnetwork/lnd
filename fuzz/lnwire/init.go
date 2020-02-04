// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_init is used by go-fuzz.
func Fuzz_init(data []byte) int {
	// Prefix with MsgInit.
	data = prefixWithMsgType(data, lnwire.MsgInit)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.Init{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
