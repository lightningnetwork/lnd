// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_funding_locked is used by go-fuzz.
func Fuzz_funding_locked(data []byte) int {
	// Prefix with MsgFundingLocked.
	data = prefixWithMsgType(data, lnwire.MsgFundingLocked)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.FundingLocked{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
