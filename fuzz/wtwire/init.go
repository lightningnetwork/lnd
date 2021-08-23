//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_init is used by go-fuzz.
func Fuzz_init(data []byte) int {
	// Prefix with MsgInit.
	data = prefixWithMsgType(data, wtwire.MsgInit)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.Init{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
