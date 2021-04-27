package sweep

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// TestTxInputSet tests adding various sized inputs to the set.
func TestTxInputSet(t *testing.T) {
	const (
		feeRate   = 1000
		relayFee  = 300
		maxInputs = 10
	)
	set := newTxInputSet(nil, feeRate, relayFee, maxInputs)

	if set.dustLimit != 537 {
		t.Fatalf("incorrect dust limit")
	}

	// Create a 300 sat input. The fee to sweep this input to a P2WKH output
	// is 439 sats. That means that this input yields -139 sats and we
	// expect it not to be added.
	if set.add(createP2WKHInput(300), constraintsRegular) {
		t.Fatal("expected add of negatively yielding input to fail")
	}

	// A 700 sat input should be accepted into the set, because it yields
	// positively.
	if !set.add(createP2WKHInput(700), constraintsRegular) {
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
	if !set.add(createP2WKHInput(1000), constraintsRegular) {
		t.Fatal("expected add of positively yielding input to succeed")
	}
	if set.outputValue != 988 {
		t.Fatal("unexpected output value")
	}
	if !set.dustLimitReached() {
		t.Fatal("expected dust limit to be reached")
	}
}

// TestTxInputSetFromWallet tests adding a wallet input to a TxInputSet to reach
// the dust limit.
func TestTxInputSetFromWallet(t *testing.T) {
	const (
		feeRate   = 500
		relayFee  = 300
		maxInputs = 10
	)

	wallet := &mockWallet{}
	set := newTxInputSet(wallet, feeRate, relayFee, maxInputs)

	// Add a 700 sat input to the set. It yields positively, but doesn't
	// reach the output dust limit.
	if !set.add(createP2WKHInput(700), constraintsRegular) {
		t.Fatal("expected add of positively yielding input to succeed")
	}
	if set.dustLimitReached() {
		t.Fatal("expected dust limit not yet to be reached")
	}

	// Expect that adding a negative yield input fails.
	if set.add(createP2WKHInput(50), constraintsRegular) {
		t.Fatal("expected negative yield input add to fail")
	}

	// Force add the negative yield input. It should succeed.
	if !set.add(createP2WKHInput(50), constraintsForce) {
		t.Fatal("expected forced add to succeed")
	}

	err := set.tryAddWalletInputsIfNeeded()
	if err != nil {
		t.Fatal(err)
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

type mockWallet struct {
	Wallet
}

func (m *mockWallet) ListUnspentWitness(minconfirms, maxconfirms int32) (
	[]*lnwallet.Utxo, error) {

	return []*lnwallet.Utxo{
		{
			AddressType: lnwallet.WitnessPubKey,
			Value:       10000,
		},
	}, nil
}
