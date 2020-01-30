// +build gofuzz

package brontidefuzz

import (
	"bytes"
)

// Fuzz_static_resp_decrypt is a go-fuzz harness that decrypts arbitrary data
// with the responder.
func Fuzz_static_resp_decrypt(data []byte) int {
	// This will return brontide machines with static keys.
	initiator, responder := getStaticBrontideMachines()

	// Complete the brontide handshake.
	completeHandshake(initiator, responder)

	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	// Decrypt the encrypted message using ReadMessage w/ responder machine.
	if _, err := responder.ReadMessage(r); err == nil {
		nilAndPanic(initiator, responder, nil)
	}

	return 1
}
