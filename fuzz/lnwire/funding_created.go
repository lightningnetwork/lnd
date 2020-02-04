// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_funding_created is used by go-fuzz.
func Fuzz_funding_created(data []byte) int {
	// Prefix with MsgFundingCreated.
	data = prefixWithMsgType(data, lnwire.MsgFundingCreated)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.FundingCreated{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
