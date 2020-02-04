// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_update_add_htlc is used by go-fuzz.
func Fuzz_update_add_htlc(data []byte) int {
	// Prefix with MsgUpdateAddHTLC.
	data = prefixWithMsgType(data, lnwire.MsgUpdateAddHTLC)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.UpdateAddHTLC{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
