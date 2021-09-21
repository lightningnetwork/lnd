//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"github.com/lightningnetwork/lnd/brontide"
)

// Fuzz_random_acttwo is a go-fuzz harness for ActTwo in the brontide
// handshake.
func Fuzz_random_acttwo(data []byte) int {
	// Check if data is large enough.
	if len(data) < brontide.ActTwoSize {
		return 1
	}

	// This will return brontide machines with random keys.
	initiator, _ := getBrontideMachines()

	// Generate ActOne - this isn't sent to the responder because nothing is
	// done with the responder machine and this would slow down fuzzing.
	// GenActOne needs to be called to set the appropriate state in the
	// initiator machine.
	_, err := initiator.GenActOne()
	if err != nil {
		nilAndPanic(initiator, nil, err)
	}

	// Copy data into [ActTwoSize]byte.
	var actTwo [brontide.ActTwoSize]byte
	copy(actTwo[:], data)

	// Initiator receives ActTwo, should fail.
	if err := initiator.RecvActTwo(actTwo); err == nil {
		nilAndPanic(initiator, nil, nil)
	}

	return 1
}
