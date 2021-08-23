//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"github.com/lightningnetwork/lnd/brontide"
)

// Fuzz_static_actone is a go-fuzz harness for ActOne in the brontide
// handshake.
func Fuzz_static_actone(data []byte) int {
	// Check if data is large enough.
	if len(data) < brontide.ActOneSize {
		return 1
	}

	// This will return brontide machines with static keys.
	_, responder := getStaticBrontideMachines()

	// Copy data into [ActOneSize]byte.
	var actOne [brontide.ActOneSize]byte
	copy(actOne[:], data)

	// Responder receives ActOne, should fail.
	if err := responder.RecvActOne(actOne); err == nil {
		nilAndPanic(nil, responder, nil)
	}

	return 1
}
