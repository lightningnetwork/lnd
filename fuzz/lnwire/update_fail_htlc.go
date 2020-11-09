// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_update_fail_htlc is used by go-fuzz.
func Fuzz_update_fail_htlc(data []byte) int {
	// Prefix with MsgUpdateFailHTLC.
	data = prefixWithMsgType(data, lnwire.MsgUpdateFailHTLC)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.UpdateFailHTLC{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
