package sweep

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
)

// TestTxInputSet tests adding various sized inputs to the set.
func TestTxInputSet(t *testing.T) {
	const (
		feeRate   = 1000
		relayFee  = 300
		maxInputs = 10
	)
	set := newTxInputSet(feeRate, relayFee, maxInputs)

	if set.dustLimit != 537 {
		t.Fatalf("incorrect dust limit")
	}

	// Create a 300 sat input. The fee to sweep this input to a P2WKH output
	// is 439 sats. That means that this input yields -139 sats and we
	// expect it not to be added.
	if set.add(createP2WKHInput(300)) {
		t.Fatal("expected add of negatively yielding input to fail")
	}

	// A 700 sat input should be accepted into the set, because it yields
	// positively.
	if !set.add(createP2WKHInput(700)) {
		t.Fatal("expected add of positively yielding input to succeed")
	}

	// The tx output should now be 700-439 = 261 sats. The dust limit isn't
	// reached yet.
	if set.outputValue != 261 {
		t.Fatal("unexpected output value")
	}
	if set.dustLimitReached() {
		t.Fatal("expected dust limit not yet to be reached")
	}

	// Add a 1000 sat input. This increases the tx fee to 712 sats. The tx
	// output should now be 1000+700 - 712 = 988 sats.
	if !set.add(createP2WKHInput(1000)) {
		t.Fatal("expected add of positively yielding input to succeed")
	}
	if set.outputValue != 988 {
		t.Fatal("unexpected output value")
	}
	if !set.dustLimitReached() {
		t.Fatal("expected dust limit to be reached")
	}
}

// createP2WKHInput returns a P2WKH test input with the specified amount.
func createP2WKHInput(amt btcutil.Amount) input.Input {
	input := createTestInput(int64(amt), input.WitnessKeyHash)
	return &input
}
