// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_closing_signed is used by go-fuzz.
func Fuzz_closing_signed(data []byte) int {
	// Prefix with MsgClosingSigned.
	data = prefixWithMsgType(data, lnwire.MsgClosingSigned)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.ClosingSigned{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
