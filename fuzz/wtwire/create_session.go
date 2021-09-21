//go:build gofuzz
// +build gofuzz

package wtwirefuzz

import (
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// Fuzz_create_session is used by go-fuzz.
func Fuzz_create_session(data []byte) int {
	// Prefix with MsgCreateSession.
	data = prefixWithMsgType(data, wtwire.MsgCreateSession)

	// Create an empty message so that the FuzzHarness func can check if the
	// max payload constraint is violated.
	emptyMsg := wtwire.CreateSession{}

	// Pass the message into our general fuzz harness for wire messages!
	return harness(data, &emptyMsg)
}
