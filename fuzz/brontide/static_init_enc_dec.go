// +build gofuzz

package brontidefuzz

import (
	"bytes"
	"math"
)

// Fuzz_static_init_enc_dec is a go-fuzz harness that tests round-trip
// encryption and decryption
// between the initiator and the responder.
func Fuzz_static_init_enc_dec(data []byte) int {
	// Ensure that length of message is not greater than max allowed size.
	if len(data) > math.MaxUint16 {
		return 0
	}

	// This will return brontide machines with static keys.
	initiator, responder := getStaticBrontideMachines()

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

	// Decrypt the ciphertext using ReadMessage w/ responder machine.
	plaintext, err := responder.ReadMessage(&b)
	if err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Check that the decrypted message and the original message are equal.
	if !bytes.Equal(data, plaintext) {
		nilAndPanic(initiator, responder, nil)
	}

	return 1
}
