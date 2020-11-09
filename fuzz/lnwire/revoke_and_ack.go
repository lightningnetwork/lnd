// +build gofuzz

package lnwirefuzz

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// Fuzz_revoke_and_ack is used by go-fuzz.
func Fuzz_revoke_and_ack(data []byte) int {
	// Prefix with MsgRevokeAndAck.
	data = prefixWithMsgType(data, lnwire.MsgRevokeAndAck)

	// Create an empty message so that the FuzzHarness func can check
	// if the max payload constraint is violated.
	emptyMsg := lnwire.RevokeAndAck{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
