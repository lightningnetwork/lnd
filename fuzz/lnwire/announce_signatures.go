// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_announce_signatures is used by go-fuzz.
func Fuzz_announce_signatures(data []byte) int {
	// Prefix with MsgAnnounceSignatures.
	data = prefixWithMsgType(data, lnwire.MsgAnnounceSignatures)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.AnnounceSignatures{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
