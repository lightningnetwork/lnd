//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_state_update_reply is used by go-fuzz.
func Fuzz_state_update_reply(data []byte) int {
	// Prefix with MsgStateUpdateReply.
	data = prefixWithMsgType(data, wtwire.MsgStateUpdateReply)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.StateUpdateReply{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
