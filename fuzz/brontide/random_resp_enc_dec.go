//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"bytes"
	"math"
)

// Fuzz_random_resp_enc_dec is a go-fuzz harness that tests round-trip
// encryption and decryption between the responder and the initiator.
func Fuzz_random_resp_enc_dec(data []byte) int {
	// Ensure that length of message is not greater than max allowed size.
	if len(data) > math.MaxUint16 {
		return 1
	}

	// This will return brontide machines with random keys.
	initiator, responder := getBrontideMachines()

	// Complete the brontide handshake.
	completeHandshake(initiator, responder)

	var b bytes.Buffer

	// Encrypt the message using WriteMessage w/ responder machine.
	if err := responder.WriteMessage(data); err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Flush the encrypted message w/ responder machine.
	if _, err := responder.Flush(&b); err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Decrypt the ciphertext using ReadMessage w/ initiator machine.
	plaintext, err := initiator.ReadMessage(&b)
	if err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Check that the decrypted message and the original message are equal.
	if !bytes.Equal(data, plaintext) {
		nilAndPanic(initiator, responder, nil)
	}

	return 1
}
