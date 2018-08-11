package lnwallet_test

import (
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// TestTxWeightEstimator tests that transaction weight estimates are calculated
// correctly by comparing against an actual (though invalid) transaction
// matching the template.
func TestTxWeightEstimator(t *testing.T) {
	netParams := &chaincfg.MainNetParams

	p2pkhAddr, err := btcutil.NewAddressPubKeyHash(
		make([]byte, 20), netParams)
	if err != nil {
		t.Fatalf("Failed to generate address: %v", err)
	}
	p2pkhScript, err := txscript.PayToAddrScript(p2pkhAddr)
	if err != nil {
		t.Fatalf("Failed to generate scriptPubKey: %v", err)
	}

	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), netParams)
	if err != nil {
		t.Fatalf("Failed to generate address: %v", err)
	}
	p2wkhScript, err := txscript.PayToAddrScript(p2wkhAddr)
	if err != nil {
		t.Fatalf("Failed to generate scriptPubKey: %v", err)
	}

	p2wshAddr, err := btcutil.NewAddressWitnessScriptHash(
		make([]byte, 32), netParams)
	if err != nil {
		t.Fatalf("Failed to generate address: %v", err)
	}
	p2wshScript, err := txscript.PayToAddrScript(p2wshAddr)
	if err != nil {
		t.Fatalf("Failed to generate scriptPubKey: %v", err)
	}

	p2shAddr, err := btcutil.NewAddressScriptHash([]byte{0}, netParams)
	if err != nil {
		t.Fatalf("Failed to generate address: %v", err)
	}
	p2shScript, err := txscript.PayToAddrScript(p2shAddr)
	if err != nil {
		t.Fatalf("Failed to generate scriptPubKey: %v", err)
	}

	testCases := []struct {
		numP2PKHInputs       int
		numP2WKHInputs       int
		numP2WSHInputs       int
		numNestedP2WKHInputs int
		numNestedP2WSHInputs int
		numP2PKHOutputs      int
		numP2WKHOutputs      int
		numP2WSHOutputs      int
		numP2SHOutputs       int
	}{
		{
			numP2PKHInputs:  1,
			numP2PKHOutputs: 2,
		},
		{
			numP2PKHInputs:  1,
			numP2WKHInputs:  1,
			numP2WKHOutputs: 1,
			numP2WSHOutputs: 1,
		},
		{
			numP2WKHInputs:  1,
			numP2WKHOutputs: 1,
			numP2WSHOutputs: 1,
		},
		{
			numP2WKHInputs:  2,
			numP2WKHOutputs: 1,
			numP2WSHOutputs: 1,
		},
		{
			numP2WSHInputs:  1,
			numP2WKHOutputs: 1,
		},
		{
			numP2PKHInputs: 1,
			numP2SHOutputs: 1,
		},
		{
			numNestedP2WKHInputs: 1,
			numP2WKHOutputs:      1,
		},
		{
			numNestedP2WSHInputs: 1,
			numP2WKHOutputs:      1,
		},
	}

	for i, test := range testCases {
		var weightEstimate lnwallet.TxWeightEstimator
		tx := wire.NewMsgTx(1)

		for j := 0; j < test.numP2PKHInputs; j++ {
			weightEstimate.AddP2PKHInput()

			signature := make([]byte, 73)
			compressedPubKey := make([]byte, 33)
			scriptSig, err := txscript.NewScriptBuilder().AddData(signature).
				AddData(compressedPubKey).Script()
			if err != nil {
				t.Fatalf("Failed to generate scriptSig: %v", err)
			}

			tx.AddTxIn(&wire.TxIn{SignatureScript: scriptSig})
		}
		for j := 0; j < test.numP2WKHInputs; j++ {
			weightEstimate.AddP2WKHInput()

			signature := make([]byte, 73)
			compressedPubKey := make([]byte, 33)
			witness := wire.TxWitness{signature, compressedPubKey}
			tx.AddTxIn(&wire.TxIn{Witness: witness})
		}
		for j := 0; j < test.numP2WSHInputs; j++ {
			weightEstimate.AddWitnessInput(42)

			witnessScript := make([]byte, 40)
			witness := wire.TxWitness{witnessScript}
			tx.AddTxIn(&wire.TxIn{Witness: witness})
		}
		for j := 0; j < test.numNestedP2WKHInputs; j++ {
			weightEstimate.AddNestedP2WKHInput()

			signature := make([]byte, 73)
			compressedPubKey := make([]byte, 33)
			witness := wire.TxWitness{signature, compressedPubKey}
			scriptSig, err := txscript.NewScriptBuilder().AddData(p2wkhScript).
				Script()
			if err != nil {
				t.Fatalf("Failed to generate scriptSig: %v", err)
			}

			tx.AddTxIn(&wire.TxIn{SignatureScript: scriptSig, Witness: witness})
		}
		for j := 0; j < test.numNestedP2WSHInputs; j++ {
			weightEstimate.AddNestedP2WSHInput(42)

			witnessScript := make([]byte, 40)
			witness := wire.TxWitness{witnessScript}
			scriptSig, err := txscript.NewScriptBuilder().AddData(p2wshScript).
				Script()
			if err != nil {
				t.Fatalf("Failed to generate scriptSig: %v", err)
			}

			tx.AddTxIn(&wire.TxIn{SignatureScript: scriptSig, Witness: witness})
		}
		for j := 0; j < test.numP2PKHOutputs; j++ {
			weightEstimate.AddP2PKHOutput()
			tx.AddTxOut(&wire.TxOut{PkScript: p2pkhScript})
		}
		for j := 0; j < test.numP2WKHOutputs; j++ {
			weightEstimate.AddP2WKHOutput()
			tx.AddTxOut(&wire.TxOut{PkScript: p2wkhScript})
		}
		for j := 0; j < test.numP2WSHOutputs; j++ {
			weightEstimate.AddP2WSHOutput()
			tx.AddTxOut(&wire.TxOut{PkScript: p2wshScript})
		}
		for j := 0; j < test.numP2SHOutputs; j++ {
			weightEstimate.AddP2SHOutput()
			tx.AddTxOut(&wire.TxOut{PkScript: p2shScript})
		}

		expectedWeight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
		if weightEstimate.Weight() != int(expectedWeight) {
			t.Errorf("Case %d: Got wrong weight: expected %d, got %d",
				i, expectedWeight, weightEstimate.Weight())
		}
	}
}
