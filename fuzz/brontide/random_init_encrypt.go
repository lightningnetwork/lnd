//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"bytes"
	"math"
)

// Fuzz_random_init_encrypt is a go-fuzz harness that encrypts arbitrary data
// with the initiator.
func Fuzz_random_init_encrypt(data []byte) int {
	// Ensure that length of message is not greater than max allowed size.
	if len(data) > math.MaxUint16 {
		return 1
	}

	// This will return brontide machines with random keys.
	initiator, responder := getBrontideMachines()

	// Complete the brontide handshake.
	completeHandshake(initiator, responder)

	var b bytes.Buffer

	// Encrypt the message using WriteMessage w/ initiator machine.
	if err := initiator.WriteMessage(data); err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Flush the encrypted message w/ initiator machine.
	if _, err := initiator.Flush(&b); err != nil {
		nilAndPanic(initiator, responder, err)
	}

	return 1
}
