package input

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type testSenderHtlcScriptTree struct {
	preImage lntypes.Preimage

	senderKey *btcec.PrivateKey

	receiverKey *btcec.PrivateKey

	revokeKey *btcec.PrivateKey

	htlcTxOut *wire.TxOut

	*HtlcScriptTree

	rootHash []byte

	htlcAmt int64
}

func newTestSenderHtlcScriptTree(t *testing.T,
	auxLeaf AuxTapLeaf) *testSenderHtlcScriptTree {

	var preImage lntypes.Preimage
	_, err := rand.Read(preImage[:])
	require.NoError(t, err)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	revokeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	payHash := preImage.Hash()
	htlcScriptTree, err := SenderHTLCScriptTaproot(
		senderKey.PubKey(), receiverKey.PubKey(), revokeKey.PubKey(),
		payHash[:], lntypes.Remote, auxLeaf,
	)
	require.NoError(t, err)

	const htlcAmt = 100
	pkScript, err := PayToTaprootScript(htlcScriptTree.TaprootKey)
	require.NoError(t, err)

	targetTxOut := &wire.TxOut{
		Value:    htlcAmt,
		PkScript: pkScript,
	}

	return &testSenderHtlcScriptTree{
		preImage:       preImage,
		senderKey:      senderKey,
		receiverKey:    receiverKey,
		revokeKey:      revokeKey,
		htlcTxOut:      targetTxOut,
		htlcAmt:        htlcAmt,
		rootHash:       htlcScriptTree.TapscriptRoot,
		HtlcScriptTree: htlcScriptTree,
	}
}

type witnessGen func(*wire.MsgTx, *txscript.TxSigHashes,
	txscript.PrevOutputFetcher) (wire.TxWitness, error)

func htlcSenderRedeemValidWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testSenderHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		receiverKey := htlcScriptTree.receiverKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				receiverKey,
			},
		}

		successLeaf := htlcScriptTree.SuccessTapLeaf
		scriptTree := htlcScriptTree.HtlcScriptTree

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: receiverKey.PubKey(),
			},
			WitnessScript:     successLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return SenderHTLCScriptTaprootRedeem(
			signer, signDesc, spendTx,
			htlcScriptTree.preImage[:],
			htlcScriptTree.revokeKey.PubKey(),
			scriptTree.TapscriptTree,
		)
	}
}

func htlcSenderRevocationWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testSenderHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		revokeKey := htlcScriptTree.revokeKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				revokeKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: revokeKey.PubKey(),
			},
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootKeySpendSignMethod,
			TapTweak:          htlcScriptTree.TapscriptRoot,
			PrevOutputFetcher: prevOuts,
		}

		return SenderHTLCScriptTaprootRevoke(
			signer, signDesc, spendTx,
		)
	}
}

func htlcSenderTimeoutWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testSenderHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		timeoutLeaf := htlcScriptTree.TimeoutTapLeaf
		scriptTree := htlcScriptTree.HtlcScriptTree

		receiverSigner := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				htlcScriptTree.receiverKey,
			},
		}
		receiverDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: htlcScriptTree.receiverKey.PubKey(),
			},
			WitnessScript:     timeoutLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}
		receiverSig, err := receiverSigner.SignOutputRaw(
			spendTx, receiverDesc,
		)
		if err != nil {
			return nil, err
		}

		senderKey := htlcScriptTree.senderKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				senderKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: senderKey.PubKey(),
			},
			WitnessScript:     timeoutLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return SenderHTLCScriptTaprootTimeout(
			receiverSig, sigHash, signer, signDesc, spendTx,
			htlcScriptTree.revokeKey.PubKey(),
			scriptTree.TapscriptTree,
		)
	}
}

func testTaprootSenderHtlcSpend(t *testing.T, auxLeaf AuxTapLeaf) {
	// First, create a new test script tree.
	htlcScriptTree := newTestSenderHtlcScriptTree(t, auxLeaf)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: htlcScriptTree.htlcAmt,
	})

	testCases := []struct {
		name string

		// TODO(roasbeef): use sighash slice

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		valid bool
	}{
		// Valid redeem with the pre-image, and the spending
		// transaction set to CSV 1 to enforce the required delay.
		{
			name: "redeem success valid sighash all",
			witnessGen: htlcSenderRedeemValidWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Valid with pre-image, using sighash default.
		{
			name: "redeem success valid sighash default",
			witnessGen: htlcSenderRedeemValidWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Valid with pre-image, using sighash single+anyonecanpay.
		{
			name: "redeem success valid sighash " +
				"single|anyonecanpay",
			witnessGen: htlcSenderRedeemValidWitnessGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Invalid spend, the witness is correct, but the spending tx
		// doesn't have a sequence of 1 set. This uses the CSV 0 trick:
		// 0 > 0 -> false.
		{
			name: "redeem success invalid wrong sequence",
			witnessGen: htlcSenderRedeemValidWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: false,
		},

		// Valid spend with the revocation key, sighash all.
		{
			name: "revocation spend valid sighash all",
			witnessGen: htlcSenderRevocationWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		// Valid spend with the revocation key, sighash default.
		{
			name: "revocation spend valid sighash default",
			witnessGen: htlcSenderRevocationWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			valid: true,
		},

		// Valid spend with the revocation key, sighash single+anyone
		// can pay.
		{
			name: "revocation spend valid sighash " +
				"single|anyonecanpay",
			witnessGen: htlcSenderRevocationWitnessGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			valid: true,
		},

		// Invalid spend with the revocation key. The witness mutator
		// modifies the sig.
		{
			name: "revocation spend invalid",
			witnessGen: htlcSenderRevocationWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		// Valid spend of the timeout path, sighash default.
		{
			name: "timeout spend valid",
			witnessGen: htlcSenderTimeoutWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			valid: true,
		},

		// Valid spend of the timeout path, sighash all.
		{
			name: "timeout spend valid sighash all",
			witnessGen: htlcSenderTimeoutWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		// Valid spend of the timeout path, sighash single.
		{
			name: "timeout spend valid sighash single",
			witnessGen: htlcSenderTimeoutWitnessGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			valid: true,
		},

		// Invalid spend of timeout path, invalid receiver sig.
		{
			name: "timeout spend invalid receiver sig",
			witnessGen: htlcSenderTimeoutWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		// Invalid spend of timeout path, invalid sender sig.
		{
			name: "timeout spend invalid sender sig",
			witnessGen: htlcSenderTimeoutWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[1][0] ^= 1
			},
			valid: false,
		},
	}

	for i, testCase := range testCases {
		i := i
		testCase := testCase

		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				htlcScriptTree.htlcTxOut.PkScript,
				htlcScriptTree.htlcAmt,
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					htlcScriptTree.htlcTxOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags,
					nil, hashCache, htlcScriptTree.htlcAmt,
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

// TestTaprootSenderHtlcSpend tests that all the positive and negative paths
// for the sender HTLC tapscript tree work as expected.
func TestTaprootSenderHtlcSpend(t *testing.T) {
	t.Parallel()

	for _, hasAuxLeaf := range []bool{true, false} {
		name := fmt.Sprintf("aux_leaf=%v", hasAuxLeaf)
		t.Run(name, func(t *testing.T) {
			var auxLeaf AuxTapLeaf
			if hasAuxLeaf {
				auxLeaf = fn.Some(txscript.NewBaseTapLeaf(
					bytes.Repeat([]byte{0x01}, 32),
				))
			}

			testTaprootSenderHtlcSpend(t, auxLeaf)
		})
	}
}

type testReceiverHtlcScriptTree struct {
	preImage lntypes.Preimage

	senderKey *btcec.PrivateKey

	receiverKey *btcec.PrivateKey

	revokeKey btcec.PrivateKey

	htlcTxOut *wire.TxOut

	*HtlcScriptTree

	rootHash []byte

	htlcAmt int64

	lockTime int32
}

func newTestReceiverHtlcScriptTree(t *testing.T,
	auxLeaf AuxTapLeaf) *testReceiverHtlcScriptTree {

	var preImage lntypes.Preimage
	_, err := rand.Read(preImage[:])
	require.NoError(t, err)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	revokeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const cltvExpiry = 144

	payHash := preImage.Hash()
	htlcScriptTree, err := ReceiverHTLCScriptTaproot(
		cltvExpiry, senderKey.PubKey(), receiverKey.PubKey(),
		revokeKey.PubKey(), payHash[:], lntypes.Remote, auxLeaf,
	)
	require.NoError(t, err)

	const htlcAmt = 100
	pkScript, err := PayToTaprootScript(htlcScriptTree.TaprootKey)
	require.NoError(t, err)

	targetTxOut := &wire.TxOut{
		Value:    htlcAmt,
		PkScript: pkScript,
	}

	return &testReceiverHtlcScriptTree{
		preImage:       preImage,
		senderKey:      senderKey,
		receiverKey:    receiverKey,
		revokeKey:      *revokeKey,
		htlcTxOut:      targetTxOut,
		htlcAmt:        htlcAmt,
		rootHash:       htlcScriptTree.TapscriptRoot,
		lockTime:       cltvExpiry,
		HtlcScriptTree: htlcScriptTree,
	}
}

func htlcReceiverTimeoutWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testReceiverHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		senderKey := htlcScriptTree.senderKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				senderKey,
			},
		}

		timeoutLeaf := htlcScriptTree.TimeoutTapLeaf

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: senderKey.PubKey(),
			},
			WitnessScript:     timeoutLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		// With the lock time in place, we can now generate the timeout
		// witness.
		return ReceiverHTLCScriptTaprootTimeout(
			signer, signDesc, spendTx, htlcScriptTree.lockTime,
			htlcScriptTree.revokeKey.PubKey(),
			htlcScriptTree.TapscriptTree,
		)
	}
}

func htlcReceiverRevocationWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testReceiverHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		revokeKey := htlcScriptTree.revokeKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				&revokeKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: revokeKey.PubKey(),
			},
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootKeySpendSignMethod,
			TapTweak:          htlcScriptTree.TapscriptRoot,
			PrevOutputFetcher: prevOuts,
		}

		return ReceiverHTLCScriptTaprootRevoke(
			signer, signDesc, spendTx,
		)
	}
}

func htlcReceiverSuccessWitnessGen(sigHash txscript.SigHashType,
	htlcScriptTree *testReceiverHtlcScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		successsLeaf := htlcScriptTree.SuccessTapLeaf
		scriptTree := htlcScriptTree.HtlcScriptTree

		senderSigner := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				htlcScriptTree.senderKey,
			},
		}
		senderDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: htlcScriptTree.senderKey.PubKey(),
			},
			WitnessScript:     successsLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}
		senderSig, err := senderSigner.SignOutputRaw(
			spendTx, senderDesc,
		)
		if err != nil {
			return nil, err
		}

		receiverKey := htlcScriptTree.receiverKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				receiverKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: receiverKey.PubKey(),
			},
			WitnessScript:     successsLeaf.Script,
			Output:            htlcScriptTree.htlcTxOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return ReceiverHTLCScriptTaprootRedeem(
			senderSig, sigHash, htlcScriptTree.preImage[:],
			signer, signDesc, spendTx,
			htlcScriptTree.revokeKey.PubKey(),
			scriptTree.TapscriptTree,
		)
	}
}

func testTaprootReceiverHtlcSpend(t *testing.T, auxLeaf AuxTapLeaf) {
	// We'll start by creating the HTLC script tree (contains all 3 valid
	// spend paths), and also a mock spend transaction that we'll be
	// signing below.
	htlcScriptTree := newTestReceiverHtlcScriptTree(t, auxLeaf)

	// TODO(roasbeef): issue with revoke key??? ctrl block even/odd

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: htlcScriptTree.htlcAmt,
	})

	testCases := []struct {
		name string

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		txMutator func(tx *wire.MsgTx)

		valid bool
	}{
		// Valid timeout by the sender after the timeout period has
		// passed. We also use a sequence of 1 as the sender must wait
		// a single block before being able to sweep the HTLC.
		{
			name: "timeout valid sig hash all",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Valid timeout like above, but sighash default.
		{
			name: "timeout valid sig hash default",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Valid timeout like above, but sighash single.
		{
			name: "timeout valid sig hash single",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		// Invalid timeout case, the sequence of the spending
		// transaction isn't set to 1.
		{
			name: "timeout invalid wrong sequence",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: false,
		},

		// Invalid timeout case, the lock time is set to the wrong
		// value.
		{
			name: "timeout invalid wrong lock time",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			txMutator: func(tx *wire.MsgTx) {
				tx.LockTime = 0
			},
			valid: false,
		},

		// Invalid timeout case, the signature is invalid.
		{
			name: "timeout invalid wrong sig",
			witnessGen: htlcReceiverTimeoutWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		// Valid spend of the revocation path.
		{
			name: "revocation spend valid",
			witnessGen: htlcReceiverRevocationWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		// Invalid spend of the revocation path.
		{
			name: "revocation spend valid",
			witnessGen: htlcReceiverRevocationWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		// Valid success spend w/ pre-image and sender sig.
		{
			name: "success spend valid",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		// Valid succcess spend sighash default.
		{
			name: "success spend valid sighash default",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		// Valid succcess spend sighash default.
		{
			name: "success spend valid sig hash default",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			valid: true,
		},

		// Valid succcess spend sighash single.
		{
			name: "success spend valid sighash single",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			valid: true,
		},

		// Invalid success spend, wrong pre-image.
		{
			name: "success spend invalid preimage",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				// The pre-image is the 3rd item (starting from
				// the "bottom") of the witness stack).
				wit[2][0] ^= 1
			},
			valid: false,
		},

		// Invalid success spend, invalid sender sig.
		{
			name: "success spend invalid sender sig",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				// Flip a bit in the sender sig which is the
				// first element of the witness stack.
				wit[0][0] ^= 1
			},
			valid: false,
		},

		// Invalid success spend, invalid receiver sig.
		{
			name: "success spend invalid receiver sig",
			witnessGen: htlcReceiverSuccessWitnessGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				// Flip a bit in the receiver sig which is the
				// second element of the witness stack.
				wit[1][0] ^= 1
			},
			valid: false,
		},
	}
	for i, testCase := range testCases {
		i := i
		testCase := testCase
		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			// TODO(roasbeef): consolidate w/ above

			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				htlcScriptTree.htlcTxOut.PkScript,
				htlcScriptTree.htlcAmt,
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.txMutator != nil {
				testCase.txMutator(spendTxCopy)
			}

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					htlcScriptTree.htlcTxOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags,
					nil, hashCache, htlcScriptTree.htlcAmt,
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

// TestTaprootReceiverHtlcSpend tests that all possible paths for redeeming an
// accepted HTLC (on the commitment transaction) of the receiver work properly.
func TestTaprootReceiverHtlcSpend(t *testing.T) {
	t.Parallel()

	for _, hasAuxLeaf := range []bool{true, false} {
		name := fmt.Sprintf("aux_leaf=%v", hasAuxLeaf)
		t.Run(name, func(t *testing.T) {
			var auxLeaf AuxTapLeaf
			if hasAuxLeaf {
				auxLeaf = fn.Some(
					txscript.NewBaseTapLeaf(
						bytes.Repeat([]byte{0x01}, 32),
					),
				)
			}

			testTaprootReceiverHtlcSpend(t, auxLeaf)
		})
	}
}

type testCommitScriptTree struct {
	csvDelay uint32

	selfKey *btcec.PrivateKey

	revokeKey *btcec.PrivateKey

	selfAmt btcutil.Amount

	txOut *wire.TxOut

	*CommitScriptTree
}

func newTestCommitScriptTree(local bool,
	auxLeaf AuxTapLeaf) (*testCommitScriptTree, error) {

	selfKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	revokeKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	const (
		csvDelay = 6
		selfAmt  = 1000
	)

	var commitScriptTree *CommitScriptTree
	if local {
		commitScriptTree, err = NewLocalCommitScriptTree(
			csvDelay, selfKey.PubKey(), revokeKey.PubKey(),
			auxLeaf,
		)
	} else {
		commitScriptTree, err = NewRemoteCommitScriptTree(
			selfKey.PubKey(), auxLeaf,
		)
	}
	if err != nil {
		return nil, err
	}

	pkScript, err := PayToTaprootScript(commitScriptTree.TaprootKey)
	if err != nil {
		return nil, err
	}

	return &testCommitScriptTree{
		csvDelay:  csvDelay,
		selfKey:   selfKey,
		revokeKey: revokeKey,
		selfAmt:   selfAmt,
		txOut: &wire.TxOut{
			PkScript: pkScript,
			Value:    selfAmt,
		},
		CommitScriptTree: commitScriptTree,
	}, nil
}

func localCommitSweepWitGen(sigHash txscript.SigHashType,
	commitScriptTree *testCommitScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		selfKey := commitScriptTree.selfKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				selfKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: selfKey.PubKey(),
			},
			WitnessScript:     commitScriptTree.SettleLeaf.Script,
			Output:            commitScriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootCommitSpendSuccess(
			signer, signDesc, spendTx,
			commitScriptTree.TapscriptTree,
		)
	}
}

func localCommitRevokeWitGen(sigHash txscript.SigHashType,
	commitScriptTree *testCommitScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		revokeKey := commitScriptTree.revokeKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				revokeKey,
			},
		}

		revScript := commitScriptTree.RevocationLeaf.Script
		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: revokeKey.PubKey(),
			},
			WitnessScript:     revScript,
			Output:            commitScriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootCommitSpendRevoke(
			signer, signDesc, spendTx,
			commitScriptTree.TapscriptTree,
		)
	}
}

func testTaprootCommitScriptToSelf(t *testing.T, auxLeaf AuxTapLeaf) {
	commitScriptTree, err := newTestCommitScriptTree(true, auxLeaf)
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: int64(commitScriptTree.selfAmt),
	})

	testCases := []struct {
		name string

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		valid bool
	}{
		{
			name: "valid sweep to self",
			witnessGen: localCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = commitScriptTree.csvDelay
			},
			valid: true,
		},

		{
			name: "valid sweep to self sighash default",
			witnessGen: localCommitSweepWitGen(
				txscript.SigHashDefault, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = commitScriptTree.csvDelay
			},
			valid: true,
		},

		{
			name: "valid sweep to self sighash single",
			witnessGen: localCommitSweepWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = commitScriptTree.csvDelay
			},
			valid: true,
		},

		{
			name: "invalid sweep to self wrong sequence",
			witnessGen: localCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: false,
		},

		{
			name: "invalid sweep to self bad sig",
			witnessGen: localCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		{
			name: "valid revocation sweep",
			witnessGen: localCommitRevokeWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			valid: true,
		},

		{
			name: "valid revocation sweep sighash default",
			witnessGen: localCommitRevokeWitGen(
				txscript.SigHashDefault, commitScriptTree,
			),
			valid: true,
		},

		{
			name: "valid revocation sweep sighash single",
			witnessGen: localCommitRevokeWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				commitScriptTree,
			),
			valid: true,
		},

		{
			name: "invalid revocation sweep bad sig",
			witnessGen: localCommitRevokeWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},
	}

	for i, testCase := range testCases {
		i := i
		testCase := testCase
		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				commitScriptTree.txOut.PkScript,
				int64(commitScriptTree.selfAmt),
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					commitScriptTree.txOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags, nil,
					hashCache,
					int64(commitScriptTree.selfAmt),
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

// TestTaprootCommitScriptToSelf tests that the taproot script for redeeming
// one's output after a force close behaves as expected.
func TestTaprootCommitScriptToSelf(t *testing.T) {
	t.Parallel()

	for _, hasAuxLeaf := range []bool{true, false} {
		name := fmt.Sprintf("aux_leaf=%v", hasAuxLeaf)
		t.Run(name, func(t *testing.T) {
			var auxLeaf AuxTapLeaf
			if hasAuxLeaf {
				auxLeaf = fn.Some(txscript.NewBaseTapLeaf(
					bytes.Repeat([]byte{0x01}, 32),
				))
			}

			testTaprootCommitScriptToSelf(t, auxLeaf)
		})
	}
}

func remoteCommitSweepWitGen(sigHash txscript.SigHashType,
	commitScriptTree *testCommitScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		selfKey := commitScriptTree.selfKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				selfKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: selfKey.PubKey(),
			},
			WitnessScript:     commitScriptTree.SettleLeaf.Script,
			Output:            commitScriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootCommitRemoteSpend(
			signer, signDesc, spendTx,
			commitScriptTree.TapscriptTree,
		)
	}
}

func testTaprootCommitScriptRemote(t *testing.T, auxLeaf AuxTapLeaf) {
	commitScriptTree, err := newTestCommitScriptTree(false, auxLeaf)
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: int64(commitScriptTree.selfAmt),
	})

	testCases := []struct {
		name string

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		valid bool
	}{
		{
			name: "valid remote sweep",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		{
			name: "valid remote sweep sighash default",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashDefault, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		{
			name: "valid remote sweep sighash single",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			valid: true,
		},

		{
			name: "invalid remote sweep wrong sequence",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 0
			},
			valid: false,
		},

		{
			name: "invalid bad sig",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},

		{
			name: "invalid bad sig right sequence",
			witnessGen: remoteCommitSweepWitGen(
				txscript.SigHashAll, commitScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
			witnessMutator: func(wit wire.TxWitness) {
				wit[0][0] ^= 1
			},
			valid: false,
		},
	}

	for i, testCase := range testCases {
		i := i
		testCase := testCase
		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				commitScriptTree.txOut.PkScript,
				int64(commitScriptTree.selfAmt),
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					commitScriptTree.txOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags, nil,
					hashCache,
					int64(commitScriptTree.selfAmt),
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

// TestTaprootCommitScriptRemote tests that the remote party can properly sweep
// their output after force close.
func TestTaprootCommitScriptRemote(t *testing.T) {
	t.Parallel()

	for _, hasAuxLeaf := range []bool{true, false} {
		name := fmt.Sprintf("aux_leaf=%v", hasAuxLeaf)
		t.Run(name, func(t *testing.T) {
			var auxLeaf AuxTapLeaf
			if hasAuxLeaf {
				auxLeaf = fn.Some(txscript.NewBaseTapLeaf(
					bytes.Repeat([]byte{0x01}, 32),
				))
			}

			testTaprootCommitScriptRemote(t, auxLeaf)
		})
	}
}

type testAnchorScriptTree struct {
	sweepKey *btcec.PrivateKey

	amt btcutil.Amount

	txOut *wire.TxOut

	*AnchorScriptTree
}

func newTestAnchorScripTree() (*testAnchorScriptTree, error) {
	sweepKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	anchorScriptTree, err := NewAnchorScriptTree(sweepKey.PubKey())
	if err != nil {
		return nil, err
	}

	const amt = 1_000

	pkScript, err := PayToTaprootScript(anchorScriptTree.TaprootKey)
	if err != nil {
		return nil, err
	}

	return &testAnchorScriptTree{
		sweepKey: sweepKey,
		amt:      amt,
		txOut: &wire.TxOut{
			PkScript: pkScript,
			Value:    amt,
		},
		AnchorScriptTree: anchorScriptTree,
	}, nil
}

func anchorSweepWitGen(sigHash txscript.SigHashType,
	anchorScriptTree *testAnchorScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		sweepKey := anchorScriptTree.sweepKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				sweepKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: sweepKey.PubKey(),
			},
			Output:            anchorScriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootKeySpendSignMethod,
			TapTweak:          anchorScriptTree.TapscriptRoot,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootAnchorSpend(
			signer, signDesc, spendTx,
		)
	}
}

func anchorAnySweepWitGen(sigHash txscript.SigHashType,
	anchorScriptTree *testAnchorScriptTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		return TaprootAnchorSpendAny(
			anchorScriptTree.sweepKey.PubKey(),
		)
	}
}

// TestTaprootCommitScript tests that a channel peer can properly spend the
// anchor, and that anyone can spend it after 16 blocks.
func TestTaprootAnchorScript(t *testing.T) {
	t.Parallel()

	anchorScriptTree, err := newTestAnchorScripTree()
	require.NoError(t, err)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: int64(anchorScriptTree.amt),
	})

	testCases := []struct {
		name string

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		valid bool
	}{
		{
			name: "valid anchor sweep",
			witnessGen: anchorSweepWitGen(
				txscript.SigHashAll, anchorScriptTree,
			),
			valid: true,
		},

		{
			name: "valid anchor sweep sighash default",
			witnessGen: anchorSweepWitGen(
				txscript.SigHashDefault, anchorScriptTree,
			),
			valid: true,
		},

		{
			name: "valid anchor sweep single",
			witnessGen: anchorSweepWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				anchorScriptTree,
			),
			valid: true,
		},

		{
			name: "invalid anchor sweep bad sig",
			witnessGen: anchorSweepWitGen(
				txscript.SigHashAll, anchorScriptTree,
			),
			witnessMutator: func(witness wire.TxWitness) {
				witness[0][0] ^= 1
			},
			valid: false,
		},

		{
			name: "valid 3rd party sweep",
			witnessGen: anchorAnySweepWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				anchorScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 16
			},
			valid: true,
		},

		{
			name: "invalid 3rd party sweep",
			witnessGen: anchorAnySweepWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				anchorScriptTree,
			),
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 2
			},
			valid: false,
		},
	}

	for i, testCase := range testCases {
		i := i
		testCase := testCase
		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				anchorScriptTree.txOut.PkScript,
				int64(anchorScriptTree.amt),
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					anchorScriptTree.txOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags,
					nil, hashCache,
					int64(anchorScriptTree.amt),
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

type testSecondLevelHtlcTree struct {
	delayKey *btcec.PrivateKey

	revokeKey *btcec.PrivateKey

	csvDelay uint32

	amt btcutil.Amount

	txOut *wire.TxOut

	scriptTree *txscript.IndexedTapScriptTree

	tapScriptRoot []byte
}

func newTestSecondLevelHtlcTree(t *testing.T,
	auxLeaf AuxTapLeaf) *testSecondLevelHtlcTree {

	delayKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	revokeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const csvDelay = 6

	scriptTree, err := SecondLevelHtlcTapscriptTree(
		delayKey.PubKey(), csvDelay, auxLeaf,
	)
	require.NoError(t, err)

	tapScriptRoot := scriptTree.RootNode.TapHash()

	htlcKey := txscript.ComputeTaprootOutputKey(
		revokeKey.PubKey(), tapScriptRoot[:],
	)

	pkScript, err := PayToTaprootScript(htlcKey)
	require.NoError(t, err)

	const amt = 100

	return &testSecondLevelHtlcTree{
		delayKey:  delayKey,
		revokeKey: revokeKey,
		csvDelay:  csvDelay,
		txOut: &wire.TxOut{
			PkScript: pkScript,
			Value:    amt,
		},
		amt:           amt,
		scriptTree:    scriptTree,
		tapScriptRoot: tapScriptRoot[:],
	}
}

func secondLevelHtlcSuccessWitGen(sigHash txscript.SigHashType,
	scriptTree *testSecondLevelHtlcTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		selfKey := scriptTree.delayKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				selfKey,
			},
		}

		tapLeaf := scriptTree.scriptTree.LeafMerkleProofs[0].TapLeaf
		witnessScript := tapLeaf.Script
		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: selfKey.PubKey(),
			},
			WitnessScript:     witnessScript,
			Output:            scriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootScriptSpendSignMethod,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootHtlcSpendSuccess(
			signer, signDesc, spendTx,
			scriptTree.revokeKey.PubKey(), scriptTree.scriptTree,
		)
	}
}

func secondLevelHtlcRevokeWitnessgen(sigHash txscript.SigHashType,
	scriptTree *testSecondLevelHtlcTree) witnessGen {

	return func(spendTx *wire.MsgTx, hashCache *txscript.TxSigHashes,
		prevOuts txscript.PrevOutputFetcher) (wire.TxWitness, error) {

		revokeKey := scriptTree.revokeKey
		signer := &MockSigner{
			Privkeys: []*btcec.PrivateKey{
				revokeKey,
			},
		}

		signDesc := &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: revokeKey.PubKey(),
			},
			Output:            scriptTree.txOut,
			HashType:          sigHash,
			InputIndex:        0,
			SigHashes:         hashCache,
			SignMethod:        TaprootKeySpendSignMethod,
			TapTweak:          scriptTree.tapScriptRoot,
			PrevOutputFetcher: prevOuts,
		}

		return TaprootHtlcSpendRevoke(
			signer, signDesc, spendTx,
		)
	}
}

func testTaprootSecondLevelHtlcScript(t *testing.T, auxLeaf AuxTapLeaf) {
	htlcScriptTree := newTestSecondLevelHtlcTree(t, auxLeaf)

	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(&wire.TxIn{})
	spendTx.AddTxOut(&wire.TxOut{
		Value: int64(htlcScriptTree.amt),
	})

	testCases := []struct {
		name string

		witnessGen witnessGen

		txInMutator func(txIn *wire.TxIn)

		witnessMutator func(witness wire.TxWitness)

		valid bool
	}{
		{
			name: "valid success sweep",
			witnessGen: secondLevelHtlcSuccessWitGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = htlcScriptTree.csvDelay
			},
		},

		{
			name: "valid success sweep sighash default",
			witnessGen: secondLevelHtlcSuccessWitGen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			valid: true,
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = htlcScriptTree.csvDelay
			},
		},

		{
			name: "valid success sweep sighash single",
			witnessGen: secondLevelHtlcSuccessWitGen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			valid: true,
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = htlcScriptTree.csvDelay
			},
		},

		{
			name: "invalid success sweep bad sig",
			witnessGen: secondLevelHtlcSuccessWitGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: false,
			witnessMutator: func(witness wire.TxWitness) {
				witness[0][0] ^= 0x01
			},
		},

		{
			name: "invalid success sweep bad sequence",
			witnessGen: secondLevelHtlcSuccessWitGen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: false,
			txInMutator: func(txIn *wire.TxIn) {
				txIn.Sequence = 1
			},
		},

		{
			name: "valid revocation sweep",
			witnessGen: secondLevelHtlcRevokeWitnessgen(
				txscript.SigHashAll, htlcScriptTree,
			),
			valid: true,
		},

		{
			name: "valid revocation sweep sig hash default",
			witnessGen: secondLevelHtlcRevokeWitnessgen(
				txscript.SigHashDefault, htlcScriptTree,
			),
			valid: true,
		},

		{
			name: "valid revocation sweep single",
			witnessGen: secondLevelHtlcRevokeWitnessgen(
				txscript.SigHashSingle|
					txscript.SigHashAnyOneCanPay,
				htlcScriptTree,
			),
			valid: true,
		},

		{
			name: "invalid revocation sweep",
			witnessGen: secondLevelHtlcRevokeWitnessgen(
				txscript.SigHashAll, htlcScriptTree,
			),
			witnessMutator: func(witness wire.TxWitness) {
				witness[0][0] ^= 0x01
			},
			valid: false,
		},
	}

	for i, testCase := range testCases {
		i := i
		testCase := testCase
		spendTxCopy := spendTx.Copy()

		t.Run(testCase.name, func(t *testing.T) {
			if testCase.txInMutator != nil {
				testCase.txInMutator(spendTxCopy.TxIn[0])
			}

			prevOuts := txscript.NewCannedPrevOutputFetcher(
				htlcScriptTree.txOut.PkScript,
				int64(htlcScriptTree.amt),
			)
			hashCache := txscript.NewTxSigHashes(
				spendTxCopy, prevOuts,
			)

			var err error
			spendTxCopy.TxIn[0].Witness, err = testCase.witnessGen(
				spendTxCopy, hashCache, prevOuts,
			)
			require.NoError(t, err)

			if testCase.witnessMutator != nil {
				testCase.witnessMutator(
					spendTxCopy.TxIn[0].Witness,
				)
			}

			// With the witness generated, we'll now check for
			// script validity.
			newEngine := func() (*txscript.Engine, error) {
				return txscript.NewEngine(
					htlcScriptTree.txOut.PkScript,
					spendTxCopy, 0,
					txscript.StandardVerifyFlags,
					nil, hashCache,
					int64(htlcScriptTree.amt),
					prevOuts,
				)
			}
			assertEngineExecution(t, i, testCase.valid, newEngine)
		})
	}
}

// TestTaprootSecondLevelHtlcScript tests that a channel peer can properly
// spend the second level HTLC script to resolve HTLCs.
func TestTaprootSecondLevelHtlcScript(t *testing.T) {
	t.Parallel()

	for _, hasAuxLeaf := range []bool{true, false} {
		name := fmt.Sprintf("aux_leaf=%v", hasAuxLeaf)
		t.Run(name, func(t *testing.T) {
			var auxLeaf AuxTapLeaf
			if hasAuxLeaf {
				auxLeaf = fn.Some(txscript.NewBaseTapLeaf(
					bytes.Repeat([]byte{0x01}, 32),
				))
			}

			testTaprootSecondLevelHtlcScript(t, auxLeaf)
		})
	}
}
