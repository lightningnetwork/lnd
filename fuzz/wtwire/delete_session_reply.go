//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_delete_session_reply is used by go-fuzz.
func Fuzz_delete_session_reply(data []byte) int {
	// Prefix with MsgDeleteSessionReply.
	data = prefixWithMsgType(data, wtwire.MsgDeleteSessionReply)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.DeleteSessionReply{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
