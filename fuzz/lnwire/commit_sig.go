// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_commit_sig is used by go-fuzz.
func Fuzz_commit_sig(data []byte) int {
	// Prefix with MsgCommitSig.
	data = prefixWithMsgType(data, lnwire.MsgCommitSig)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.CommitSig{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
