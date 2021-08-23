//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"github.com/lightningnetwork/lnd/brontide"
)

// Fuzz_static_actthree is a go-fuzz harness for ActThree in the brontide
// handshake.
func Fuzz_static_actthree(data []byte) int {
	// Check if data is large enough.
	if len(data) < brontide.ActThreeSize {
		return 1
	}

	// This will return brontide machines with static keys.
	initiator, responder := getStaticBrontideMachines()

	// Generate ActOne and send to the responder.
	actOne, err := initiator.GenActOne()
	if err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Receiving ActOne should succeed, so we panic on error.
	if err := responder.RecvActOne(actOne); err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Generate ActTwo - this is not sent to the initiator because nothing is
	// done with the initiator after this point and it would slow down fuzzing.
	// GenActTwo needs to be called to set the appropriate state in the responder
	// machine.
	_, err = responder.GenActTwo()
	if err != nil {
		nilAndPanic(initiator, responder, err)
	}

	// Copy data into [ActThreeSize]byte.
	var actThree [brontide.ActThreeSize]byte
	copy(actThree[:], data)

	// Responder receives ActThree, should fail.
	if err := responder.RecvActThree(actThree); err == nil {
		nilAndPanic(initiator, responder, nil)
	}

	return 1
}
