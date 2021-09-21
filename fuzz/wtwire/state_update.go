//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_state_update is used by go-fuzz.
func Fuzz_state_update(data []byte) int {
	// Prefix with MsgStateUpdate.
	data = prefixWithMsgType(data, wtwire.MsgStateUpdate)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.StateUpdate{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
