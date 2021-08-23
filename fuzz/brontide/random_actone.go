//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"github.com/lightningnetwork/lnd/brontide"
)

// Fuzz_random_actone is a go-fuzz harness for ActOne in the brontide
// handshake.
func Fuzz_random_actone(data []byte) int {
	// Check if data is large enough.
	if len(data) < brontide.ActOneSize {
		return 1
	}

	// This will return brontide machines with random keys.
	_, responder := getBrontideMachines()

	// Copy data into [ActOneSize]byte.
	var actOne [brontide.ActOneSize]byte
	copy(actOne[:], data)

	// Responder receives ActOne, should fail on the MAC check.
	if err := responder.RecvActOne(actOne); err == nil {
		nilAndPanic(nil, responder, nil)
	}

	return 1
}
