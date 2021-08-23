//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_delete_session is used by go-fuzz.
func Fuzz_delete_session(data []byte) int {
	// Prefix with MsgDeleteSession.
	data = prefixWithMsgType(data, wtwire.MsgDeleteSession)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.DeleteSession{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
