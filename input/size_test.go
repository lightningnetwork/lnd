package input_test

import (
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	testCSVDelay = (1 << 31) - 1

	testCLTVExpiry = 500000000

	// maxDERSignatureSize is the largest possible DER-encoded signature
	// without the trailing sighash flag.
	maxDERSignatureSize = 72

	testAmt = btcutil.MaxSatoshi
)

var (
	testPubkeyBytes = make([]byte, 33)

	testHash160  = make([]byte, 20)
	testPreimage = make([]byte, 32)

	// testPubkey is a pubkey used in script size calculation.
	testPubkey = &btcec.PublicKey{}

	testPrivkey, _ = btcec.PrivKeyFromBytes(make([]byte, 32))

	testTx = wire.NewMsgTx(2)

	testOutPoint = wire.OutPoint{
		Hash:  chainhash.Hash{},
		Index: 1,
	}
)

// TestTxWeightEstimator tests that transaction weight estimates are calculated
// correctly by comparing against an actual (though invalid) transaction
// matching the template.
func TestTxWeightEstimator(t *testing.T) {
	netParams := &chaincfg.MainNetParams

	p2pkhAddr, err := btcutil.NewAddressPubKeyHash(
		make([]byte, 20), netParams)
	require.NoError(t, err, "Failed to generate address")
	p2pkhScript, err := txscript.PayToAddrScript(p2pkhAddr)
	require.NoError(t, err, "Failed to generate scriptPubKey")

	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), netParams)
	require.NoError(t, err, "Failed to generate address")
	p2wkhScript, err := txscript.PayToAddrScript(p2wkhAddr)
	require.NoError(t, err, "Failed to generate scriptPubKey")

	p2wshAddr, err := btcutil.NewAddressWitnessScriptHash(
		make([]byte, 32), netParams)
	require.NoError(t, err, "Failed to generate address")
	p2wshScript, err := txscript.PayToAddrScript(p2wshAddr)
	require.NoError(t, err, "Failed to generate scriptPubKey")

	p2shAddr, err := btcutil.NewAddressScriptHash([]byte{0}, netParams)
	require.NoError(t, err, "Failed to generate address")
	p2shScript, err := txscript.PayToAddrScript(p2shAddr)
	require.NoError(t, err, "Failed to generate scriptPubKey")

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
		// Assert base txn size.
		{},

		// Assert single input/output sizes.
		{
			numP2PKHInputs: 1,
		},
		{
			numP2WKHInputs: 1,
		},
		{
			numP2WSHInputs: 1,
		},
		{
			numNestedP2WKHInputs: 1,
		},
		{
			numNestedP2WSHInputs: 1,
		},
		{
			numP2WKHOutputs: 1,
		},
		{
			numP2PKHOutputs: 1,
		},
		{
			numP2WSHOutputs: 1,
		},
		{
			numP2SHOutputs: 1,
		},

		// Assert each input/output increments input/output counts.
		{
			numP2PKHInputs: 253,
		},
		{
			numP2WKHInputs: 253,
		},
		{
			numP2WSHInputs: 253,
		},
		{
			numNestedP2WKHInputs: 253,
		},
		{
			numNestedP2WSHInputs: 253,
		},
		{
			numP2WKHOutputs: 253,
		},
		{
			numP2PKHOutputs: 253,
		},
		{
			numP2WSHOutputs: 253,
		},
		{
			numP2SHOutputs: 253,
		},

		// Assert basic combinations of inputs and outputs.
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

		// Assert disparate input/output types increment total
		// input/output counts.
		{
			numP2PKHInputs:       50,
			numP2WKHInputs:       50,
			numP2WSHInputs:       51,
			numNestedP2WKHInputs: 51,
			numNestedP2WSHInputs: 51,
			numP2WKHOutputs:      1,
		},
		{
			numP2WKHInputs:  1,
			numP2WKHOutputs: 63,
			numP2PKHOutputs: 63,
			numP2WSHOutputs: 63,
			numP2SHOutputs:  64,
		},
		{
			numP2PKHInputs:       50,
			numP2WKHInputs:       50,
			numP2WSHInputs:       51,
			numNestedP2WKHInputs: 51,
			numNestedP2WSHInputs: 51,
			numP2WKHOutputs:      63,
			numP2PKHOutputs:      63,
			numP2WSHOutputs:      63,
			numP2SHOutputs:       64,
		},
	}

	for i, test := range testCases {
		var weightEstimate input.TxWeightEstimator
		tx := wire.NewMsgTx(1)

		for j := 0; j < test.numP2PKHInputs; j++ {
			weightEstimate.AddP2PKHInput()

			signature := make([]byte, maxDERSignatureSize+1)
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

			signature := make([]byte, maxDERSignatureSize+1)
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

			signature := make([]byte, maxDERSignatureSize+1)
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

type maxDERSignature struct{}

func (s *maxDERSignature) Serialize() []byte {
	// Always return worst-case signature length, excluding the one byte
	// sighash flag.
	return make([]byte, maxDERSignatureSize)
}

func (s *maxDERSignature) Verify(_ []byte, _ *btcec.PublicKey) bool {
	return true
}

// dummySigner is a fake signer used for size (upper bound) calculations.
type dummySigner struct {
	input.Signer
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
func (s *dummySigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	return &maxDERSignature{}, nil
}

type witnessSizeTest struct {
	name       string
	expSize    int
	genWitness func(t *testing.T) wire.TxWitness
}

var witnessSizeTests = []witnessSizeTest{
	{
		name:    "funding",
		expSize: input.MultiSigWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witnessScript, _, err := input.GenFundingPkScript(
				testPubkeyBytes, testPubkeyBytes, 1,
			)
			if err != nil {
				t.Fatal(err)
			}

			return input.SpendMultiSig(
				witnessScript,
				testPubkeyBytes, &maxDERSignature{},
				testPubkeyBytes, &maxDERSignature{},
			)
		},
	},
	{
		name:    "to local timeout",
		expSize: input.ToLocalTimeoutWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witnessScript, err := input.CommitScriptToSelf(
				testCSVDelay, testPubkey, testPubkey,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witnessScript,
			}

			witness, err := input.CommitSpendTimeout(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "to local revoke",
		expSize: input.ToLocalPenaltyWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witnessScript, err := input.CommitScriptToSelf(
				testCSVDelay, testPubkey, testPubkey,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witnessScript,
			}

			witness, err := input.CommitSpendRevoke(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "to remote confirmed",
		expSize: input.ToRemoteConfirmedWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.CommitScriptToRemoteConfirmed(
				testPubkey,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
			}

			witness, err := input.CommitSpendToRemoteConfirmed(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "anchor",
		expSize: input.AnchorWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.CommitScriptAnchor(
				testPubkey,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
			}

			witness, err := input.CommitSpendAnchor(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "anchor anyone",
		expSize: 43,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.CommitScriptAnchor(
				testPubkey,
			)
			if err != nil {
				t.Fatal(err)
			}

			witness, _ := input.CommitSpendAnchorAnyone(witScript)

			return witness
		},
	},
	{
		name:    "offered htlc revoke",
		expSize: input.OfferedHtlcPenaltyWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
				DoubleTweak: testPrivkey,
			}

			witness, err := input.SenderHtlcSpendRevoke(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "offered htlc revoke confirmed",
		expSize: input.OfferedHtlcPenaltyWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			hash := make([]byte, 20)

			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				hash, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
				DoubleTweak: testPrivkey,
			}

			witness, err := input.SenderHtlcSpendRevoke(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "offered htlc timeout",
		expSize: input.OfferedHtlcTimeoutWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.SenderHtlcSpendTimeout(
				&maxDERSignature{}, txscript.SigHashAll,
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "offered htlc timeout confirmed",
		expSize: input.OfferedHtlcTimeoutWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				testHash160, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.SenderHtlcSpendTimeout(
				&maxDERSignature{}, txscript.SigHashAll,
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "offered htlc success",
		expSize: input.OfferedHtlcSuccessWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.SenderHtlcSpendRedeem(
				&dummySigner{}, signDesc, testTx, testPreimage,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "offered htlc success confirmed",
		expSize: input.OfferedHtlcSuccessWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.SenderHTLCScript(
				testPubkey, testPubkey, testPubkey,
				testHash160, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.SenderHtlcSpendRedeem(
				&dummySigner{}, signDesc, testTx, testPreimage,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc revoke",
		expSize: input.AcceptedHtlcPenaltyWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
				DoubleTweak: testPrivkey,
			}

			witness, err := input.ReceiverHtlcSpendRevoke(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc revoke confirmed",
		expSize: input.AcceptedHtlcPenaltyWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
				DoubleTweak: testPrivkey,
			}

			witness, err := input.ReceiverHtlcSpendRevoke(
				&dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc timeout",
		expSize: input.AcceptedHtlcTimeoutWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {

			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.ReceiverHtlcSpendTimeout(
				&dummySigner{}, signDesc, testTx,
				testCLTVExpiry,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc timeout confirmed",
		expSize: input.AcceptedHtlcTimeoutWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
			}

			witness, err := input.ReceiverHtlcSpendTimeout(
				&dummySigner{}, signDesc, testTx,
				testCLTVExpiry,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc success",
		expSize: input.AcceptedHtlcSuccessWitnessSize,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, false,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
			}

			witness, err := input.ReceiverHtlcSpendRedeem(
				&maxDERSignature{}, txscript.SigHashAll,
				testPreimage, &dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
	{
		name:    "accepted htlc success confirmed",
		expSize: input.AcceptedHtlcSuccessWitnessSizeConfirmed,
		genWitness: func(t *testing.T) wire.TxWitness {
			witScript, err := input.ReceiverHTLCScript(
				testCLTVExpiry, testPubkey, testPubkey,
				testPubkey, testHash160, true,
			)
			if err != nil {
				t.Fatal(err)
			}

			signDesc := &input.SignDescriptor{
				WitnessScript: witScript,
				KeyDesc: keychain.KeyDescriptor{
					PubKey: testPubkey,
				},
			}

			witness, err := input.ReceiverHtlcSpendRedeem(
				&maxDERSignature{}, txscript.SigHashAll,
				testPreimage, &dummySigner{}, signDesc, testTx,
			)
			if err != nil {
				t.Fatal(err)
			}

			return witness
		},
	},
}

// TestWitnessSizes asserts the correctness of our magic witness constants.
// Witnesses involving signatures will have maxDERSignatures injected so that we
// can determine upper bounds for the witness sizes. These constants are
// predominately used for fee estimation, so we want to be certain that we
// aren't under estimating or our transactions could get stuck.
func TestWitnessSizes(t *testing.T) {
	for _, test := range witnessSizeTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			size := test.genWitness(t).SerializeSize()
			if size != test.expSize {
				t.Fatalf("size mismatch, want: %v, got: %v",
					test.expSize, size)
			}
		})
	}
}

// genTimeoutTx creates a signed HTLC second level timeout tx.
func genTimeoutTx(chanType channeldb.ChannelType) (*wire.MsgTx, error) {
	// Create the unsigned timeout tx.
	timeoutTx, err := lnwallet.CreateHtlcTimeoutTx(
		chanType, false, testOutPoint, testAmt, testCLTVExpiry,
		testCSVDelay, 0, testPubkey, testPubkey,
	)
	if err != nil {
		return nil, err
	}

	// In order to sign the transcation, generate the script for the output
	// it spends.
	witScript, err := input.SenderHTLCScript(
		testPubkey, testPubkey, testPubkey, testHash160,
		chanType.HasAnchors(),
	)
	if err != nil {
		return nil, err
	}

	signDesc := &input.SignDescriptor{
		WitnessScript: witScript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: testPubkey,
		},
	}

	// Sign the timeout tx and add the witness.
	sigHashType := lnwallet.HtlcSigHashType(chanType)
	timeoutWitness, err := input.SenderHtlcSpendTimeout(
		&maxDERSignature{}, sigHashType, &dummySigner{},
		signDesc, timeoutTx,
	)
	if err != nil {
		return nil, err
	}
	timeoutTx.TxIn[0].Witness = timeoutWitness

	return timeoutTx, nil
}

// genSuccessTx creates a signed HTLC second level success tx.
func genSuccessTx(chanType channeldb.ChannelType) (*wire.MsgTx, error) {
	// Create the unisgned success tx.
	successTx, err := lnwallet.CreateHtlcSuccessTx(
		chanType, false, testOutPoint, testAmt, testCSVDelay, 0,
		testPubkey, testPubkey,
	)
	if err != nil {
		return nil, err
	}

	// In order to sign the transcation, generate the script for the output
	// it spends.
	witScript, err := input.ReceiverHTLCScript(
		testCLTVExpiry, testPubkey, testPubkey,
		testPubkey, testHash160, chanType.HasAnchors(),
	)
	if err != nil {
		return nil, err
	}

	signDesc := &input.SignDescriptor{
		WitnessScript: witScript,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: testPubkey,
		},
	}

	// Sign the success tx and add the witness.
	sigHashType := lnwallet.HtlcSigHashType(channeldb.SingleFunderBit)
	successWitness, err := input.ReceiverHtlcSpendRedeem(
		&maxDERSignature{}, sigHashType, testPreimage,
		&dummySigner{}, signDesc, successTx,
	)
	if err != nil {
		return nil, err
	}
	successTx.TxIn[0].Witness = successWitness

	return successTx, nil
}

type txSizeTest struct {
	name      string
	expWeight int64
	genTx     func(t *testing.T) *wire.MsgTx
}

var txSizeTests = []txSizeTest{
	{
		name:      "htlc timeout regular ",
		expWeight: input.HtlcTimeoutWeight,
		genTx: func(t *testing.T) *wire.MsgTx {
			tx, err := genTimeoutTx(channeldb.SingleFunderBit)
			require.NoError(t, err)

			return tx
		},
	},
	{
		name:      "htlc timeout confirmed",
		expWeight: input.HtlcTimeoutWeightConfirmed,
		genTx: func(t *testing.T) *wire.MsgTx {
			tx, err := genTimeoutTx(channeldb.AnchorOutputsBit)
			require.NoError(t, err)

			return tx
		},
	},

	{
		name: "htlc success regular",
		// The weight estimate from the spec is off by one, but it's
		// okay since we overestimate the weight.
		expWeight: input.HtlcSuccessWeight - 1,
		genTx: func(t *testing.T) *wire.MsgTx {
			tx, err := genSuccessTx(channeldb.SingleFunderBit)
			require.NoError(t, err)

			return tx
		},
	},
	{
		name: "htlc success confirmed",
		// The weight estimate from the spec is off by one, but it's
		// okay since we overestimate the weight.
		expWeight: input.HtlcSuccessWeightConfirmed - 1,
		genTx: func(t *testing.T) *wire.MsgTx {
			tx, err := genSuccessTx(channeldb.AnchorOutputsBit)
			require.NoError(t, err)

			return tx
		},
	},
}

// TestWitnessSizes asserts the correctness of our magic tx size constants.
func TestTxSizes(t *testing.T) {
	for _, test := range txSizeTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			tx := test.genTx(t)

			weight := blockchain.GetTransactionWeight(btcutil.NewTx(tx))
			if weight != test.expWeight {
				t.Fatalf("size mismatch, want: %v, got: %v",
					test.expWeight, weight)
			}
		})
	}
}
