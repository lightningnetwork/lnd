// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_update_fail_malformed_htlc is used by go-fuzz.
func Fuzz_update_fail_malformed_htlc(data []byte) int {
	// Prefix with MsgUpdateFailMalformedHTLC.
	data = prefixWithMsgType(data, lnwire.MsgUpdateFailMalformedHTLC)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.UpdateFailMalformedHTLC{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
