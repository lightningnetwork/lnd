// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_update_fee is used by go-fuzz.
func Fuzz_update_fee(data []byte) int {
	// Prefix with MsgUpdateFee.
	data = prefixWithMsgType(data, lnwire.MsgUpdateFee)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.UpdateFee{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
