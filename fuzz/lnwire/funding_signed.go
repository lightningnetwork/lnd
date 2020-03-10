// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_funding_signed is used by go-fuzz.
func Fuzz_funding_signed(data []byte) int {
	// Prefix with MsgFundingSigned.
	prefixWithMsgType(data, lnwire.MsgFundingSigned)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.FundingSigned{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
