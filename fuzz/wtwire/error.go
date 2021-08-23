//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_error is used by go-fuzz.
func Fuzz_error(data []byte) int {
	// Prefix with MsgError.
	data = prefixWithMsgType(data, wtwire.MsgError)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.Error{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
