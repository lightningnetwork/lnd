package lnwallet

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/fastsha256"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// TestCommitmentSpendValidation test the spendability of both outputs within
// the commitment transaction.
//
// The following spending cases are covered by this test:
//   * Alice's spend from the delayed output on her commitment transaction.
//   * Bob's spend from Alice's delayed output when she broadcasts a revoked
//     commitment transaction.
//   * Bob's spend from his unencumbered output within Alice's commitment
//     transaction.
func TestCommitmentSpendValidation(t *testing.T) {
	// We generate a fake output, and the corresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	fundingOut := &wire.OutPoint{
		Hash:  testHdSeed,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	// We also set up set some resources for the commitment transaction.
	// Each side currently has 1 BTC within the channel, with a total
	// channel capacity of 2BTC.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)
	channelBalance := btcutil.Amount(1 * 10e8)
	csvTimeout := uint32(5)
	revocationPreimage := testHdSeed[:]
	revokePubKey := DeriveRevocationPubkey(bobKeyPub, revocationPreimage)

	aliceSelfOutputSigner := &mockSigner{aliceKeyPriv}

	// With all the test data set up, we create the commitment transaction.
	// We only focus on a single party's transactions, as the scripts are
	// identical with the roles reversed.
	//
	// This is Alice's commitment transaction, so she must wait a CSV delay
	// of 5 blocks before sweeping the output, while bob can spend
	// immediately with either the revocation key, or his regular key.
	commitmentTx, err := CreateCommitTx(fakeFundingTxIn, aliceKeyPub,
		bobKeyPub, revokePubKey, csvTimeout, channelBalance, channelBalance)
	if err != nil {
		t.Fatalf("unable to create commitment transaction: %v", nil)
	}

	delayOutput := commitmentTx.TxOut[0]
	regularOutput := commitmentTx.TxOut[1]

	// We're testing an uncooperative close, output sweep, so construct a
	// transaction which sweeps the funds to a random address.
	targetOutput, err := commitScriptUnencumbered(aliceKeyPub)
	if err != nil {
		t.Fatalf("unable to create target output: %v")
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{commitmentTx.TxHash(), 0}, nil, nil))
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: targetOutput,
		Value:    0.5 * 10e8,
	})

	// First, we'll test spending with Alice's key after the timeout.
	delayScript, err := commitScriptToSelf(csvTimeout, aliceKeyPub, revokePubKey)
	if err != nil {
		t.Fatalf("unable to generate alice delay script: %v")
	}
	sweepTx.TxIn[0].Sequence = lockTimeToSequence(false, csvTimeout)
	signDesc := &SignDescriptor{
		WitnessScript: delayScript,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	aliceWitnessSpend, err := CommitSpendTimeout(aliceSelfOutputSigner,
		signDesc, sweepTx)
	if err != nil {
		t.Fatalf("unable to generate delay commit spend witness :%v")
	}
	sweepTx.TxIn[0].Witness = aliceWitnessSpend
	vm, err := txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("spend from delay output is invalid: %v", err)
	}

	// Next, we'll test bob spending with the derived revocation key to
	// simulate the scenario when alice broadcasts this commitment
	// transaction after it's been revoked.
	revokePrivKey := DeriveRevocationPrivKey(bobKeyPriv, revocationPreimage)
	bobRevokeSigner := &mockSigner{revokePrivKey}
	signDesc = &SignDescriptor{
		WitnessScript: delayScript,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value: int64(channelBalance),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	bobWitnessSpend, err := CommitSpendRevoke(bobRevokeSigner, signDesc,
		sweepTx)
	if err != nil {
		t.Fatalf("unable to generate revocation witness: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobWitnessSpend
	vm, err = txscript.NewEngine(delayOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("revocation spend is invalid: %v", err)
	}

	// Finally, we test bob sweeping his output as normal in the case that
	// alice broadcasts this commitment transaction.
	bobSigner := &mockSigner{bobKeyPriv}
	bobScriptp2wkh, err := commitScriptUnencumbered(bobKeyPub)
	if err != nil {
		t.Fatalf("unable to create bob p2wkh script: %v", err)
	}
	signDesc = &SignDescriptor{
		WitnessScript: bobScriptp2wkh,
		SigHashes:     txscript.NewTxSigHashes(sweepTx),
		Output: &wire.TxOut{
			Value:    int64(channelBalance),
			PkScript: bobScriptp2wkh,
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
	}
	bobRegularSpend, err := CommitSpendNoDelay(bobSigner, signDesc,
		sweepTx)
	if err != nil {
		t.Fatalf("unable to create bob regular spend: %v", err)
	}
	sweepTx.TxIn[0].Witness = bobRegularSpend
	vm, err = txscript.NewEngine(regularOutput.PkScript,
		sweepTx, 0, txscript.StandardVerifyFlags, nil,
		nil, int64(channelBalance))
	if err != nil {
		t.Fatalf("unable to create engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("bob p2wkh spend is invalid: %v", err)
	}
}

// TestRevocationKeyDerivation tests that given a public key, and a revocation
// hash, the homomorphic revocation public and private key derivation work
// properly.
func TestRevocationKeyDerivation(t *testing.T) {
	revocationPreimage := testHdSeed[:]

	priv, pub := btcec.PrivKeyFromBytes(btcec.S256(), testWalletPrivKey)

	revocationPub := DeriveRevocationPubkey(pub, revocationPreimage)

	revocationPriv := DeriveRevocationPrivKey(priv, revocationPreimage)
	x, y := btcec.S256().ScalarBaseMult(revocationPriv.D.Bytes())
	derivedRevPub := &btcec.PublicKey{
		Curve: btcec.S256(),
		X:     x,
		Y:     y,
	}

	// The the revocation public key derived from the original public key,
	// and the one derived from the private key should be identical.
	if !revocationPub.IsEqual(derivedRevPub) {
		t.Fatalf("derived public keys don't match!")
	}
}

// makeWitnessTestCase is a helper function used within test cases involving
// the validity of a crafted witness. This function is a wrapper function which
// allows constructing table-driven tests. In the case of an error while
// constructing the witness, the test fails fataly.
func makeWitnessTestCase(t *testing.T, f func() (wire.TxWitness, error)) func() wire.TxWitness {
	return func() wire.TxWitness {
		witness, err := f()
		if err != nil {
			t.Fatalf("unable to create witness test case: %v", err)
		}

		return witness
	}
}

// TestHTLCSenderSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the sender's commitment transaction for an
// outgoing HTLC.
//
// The following cases are exercised by this test:
// sender script:
//  * receiver spends
//    * revoke w/ sig
//    * HTLC with invalid preimage size
//    * HTLC with valid preimage size + sig
//  * sender spends
//    * invalid lock-time for CLTV
//    * invalid sequence for CSV
//    * valid lock-time+sequence, valid sig
func TestHTLCSenderSpendValidation(t *testing.T) {
	// TODO(roasbeef): eliminate duplication with other HTLC tests.

	// We generate a fake output, and the coresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	fundingOut := &wire.OutPoint{
		Hash:  testHdSeed,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	// Generate a payment and revocation preimage to be used below.
	revokePreimage := testHdSeed[:]
	revokeHash := fastsha256.Sum256(revokePreimage)
	paymentPreimage := revokeHash
	paymentPreimage[0] ^= 1
	paymentHash := fastsha256.Sum256(paymentPreimage[:])

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)
	paymentAmt := btcutil.Amount(1 * 10e8)
	cltvTimeout := uint32(8)
	csvTimeout := uint32(5)

	// Generate the raw HTLC redemption scripts, and its p2wsh counterpart.
	htlcScript, err := senderHTLCScript(cltvTimeout, csvTimeout,
		aliceKeyPub, bobKeyPub, revokeHash[:], paymentHash[:])
	if err != nil {
		t.Fatalf("unable to create htlc sender script: %v", err)
	}
	htlcWitnessScript, err := witnessScriptHash(htlcScript)
	if err != nil {
		t.Fatalf("unable to create p2wsh htlc script: %v", err)
	}

	// This will be Alice's commitment transaction. In this scenario Alice
	// is sending an HTLC to a node she has a a path to (could be Bob,
	// could be multiple hops down, it doesn't really matter).
	senderCommitTx := wire.NewMsgTx(2)
	senderCommitTx.AddTxIn(fakeFundingTxIn)
	senderCommitTx.AddTxOut(&wire.TxOut{
		Value:    int64(paymentAmt),
		PkScript: htlcWitnessScript,
	})

	prevOut := &wire.OutPoint{
		Hash:  senderCommitTx.TxHash(),
		Index: 0,
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(prevOut, nil, nil))
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)

	testCases := []struct {
		witness func() wire.TxWitness
		valid   bool
	}{
		{
			// revoke w/ sig
			// TODO(roasbeef): test invalid revoke
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendRevoke(htlcScript, paymentAmt,
					bobKeyPriv, sweepTx,
					revokePreimage)
			}),
			true,
		},
		{
			// HTLC with invalid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendRedeem(htlcScript, paymentAmt,
					bobKeyPriv, sweepTx,
					// Invalid preimage length
					bytes.Repeat([]byte{1}, 45))
			}),
			false,
		},
		{
			// HTLC with valid preimage size + sig
			// TODO(roabeef): invalid preimage
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendRedeem(htlcScript, paymentAmt,
					bobKeyPriv, sweepTx,
					paymentPreimage[:])
			}),
			true,
		},
		{
			// invalid lock-time for CLTV
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendTimeout(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, cltvTimeout-2, csvTimeout)
			}),
			false,
		},
		{
			// invalid sequence for CSV
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendTimeout(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, cltvTimeout, csvTimeout-2)
			}),
			false,
		},
		{
			// valid lock-time+sequence, valid sig
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return senderHtlcSpendTimeout(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, cltvTimeout, csvTimeout)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].Witness = testCase.witness()

		vm, err := txscript.NewEngine(htlcWitnessScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(paymentAmt))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}

		// This buffer will trace execution of the Script, only dumping
		// out to stdout in the case that a test fails.
		var debugBuf bytes.Buffer

		done := false
		for !done {
			dis, err := vm.DisasmPC()
			if err != nil {
				t.Fatalf("stepping (%v)\n", err)
			}
			debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

			done, err = vm.Step()
			if err != nil && testCase.valid {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v failed, spend should be valid: %v", i, err)
			} else if err == nil && !testCase.valid && done {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v succeed, spend should be invalid: %v", i, err)
			}

			debugBuf.WriteString(fmt.Sprintf("Stack: ", vm.GetStack()))
			debugBuf.WriteString(fmt.Sprintf("AltStack: ", vm.GetAltStack()))
		}
	}
}

// TestHTLCReceiverSpendValidation tests all possible valid+invalid redemption
// paths in the script used within the receiver's commitment transaction for an
// incoming HTLC.
//
// The following cases are exercised by this test:
//  * receiver spends
//     * HTLC redemption w/ invalid preimage size
//     * HTLC redemption w/ invalid sequence
//     * HTLC redemption w/ valid preimage size
//  * sender spends
//     * revoke w/ sig
//     * refund w/ invalid lock time
//     * refund w/ valid lock time
func TestHTLCReceiverSpendValidation(t *testing.T) {
	// We generate a fake output, and the coresponding txin. This output
	// doesn't need to exist, as we'll only be validating spending from the
	// transaction that references this.
	fundingOut := &wire.OutPoint{
		Hash:  testHdSeed,
		Index: 50,
	}
	fakeFundingTxIn := wire.NewTxIn(fundingOut, nil, nil)

	// Generate a payment and revocation preimage to be used below.
	revokePreimage := testHdSeed[:]
	revokeHash := fastsha256.Sum256(revokePreimage)
	paymentPreimage := revokeHash
	paymentPreimage[0] ^= 1
	paymentHash := fastsha256.Sum256(paymentPreimage[:])

	// We'll also need some tests keys for alice and bob, and metadata of
	// the HTLC output.
	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		testWalletPrivKey)
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(btcec.S256(),
		bobsPrivKey)
	paymentAmt := btcutil.Amount(1 * 10e8)
	cltvTimeout := uint32(8)
	csvTimeout := uint32(5)

	// Generate the raw HTLC redemption scripts, and its p2wsh counterpart.
	htlcScript, err := receiverHTLCScript(cltvTimeout, csvTimeout,
		aliceKeyPub, bobKeyPub, revokeHash[:], paymentHash[:])
	if err != nil {
		t.Fatalf("unable to create htlc sender script: %v", err)
	}
	htlcWitnessScript, err := witnessScriptHash(htlcScript)
	if err != nil {
		t.Fatalf("unable to create p2wsh htlc script: %v", err)
	}

	// This will be Bob's commitment transaction. In this scenario Alice
	// is sending an HTLC to a node she has a a path to (could be Bob,
	// could be multiple hops down, it doesn't really matter).
	receiverCommitTx := wire.NewMsgTx(2)
	receiverCommitTx.AddTxIn(fakeFundingTxIn)
	receiverCommitTx.AddTxOut(&wire.TxOut{
		Value:    int64(paymentAmt),
		PkScript: htlcWitnessScript,
	})

	prevOut := &wire.OutPoint{
		Hash:  receiverCommitTx.TxHash(),
		Index: 0,
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(wire.NewTxIn(prevOut, nil, nil))
	sweepTx.AddTxOut(
		&wire.TxOut{
			PkScript: []byte("doesn't matter"),
			Value:    1 * 10e8,
		},
	)

	testCases := []struct {
		witness func() wire.TxWitness
		valid   bool
	}{
		{
			// HTLC redemption w/ invalid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendRedeem(htlcScript,
					paymentAmt, bobKeyPriv, sweepTx,
					bytes.Repeat([]byte{1}, 45), csvTimeout,
				)
			}),
			false,
		},
		{
			// HTLC redemption w/ invalid sequence
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendRedeem(htlcScript,
					paymentAmt, bobKeyPriv, sweepTx,
					paymentPreimage[:], csvTimeout-2,
				)
			}),
			false,
		},
		{
			// HTLC redemption w/ valid preimage size
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendRedeem(htlcScript,
					paymentAmt, bobKeyPriv, sweepTx,
					paymentPreimage[:], csvTimeout,
				)
			}),
			true,
		},
		{
			// revoke w/ sig
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendRevoke(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, revokePreimage[:],
				)
			}),
			true,
		},
		{
			// refund w/ invalid lock time
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendTimeout(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, cltvTimeout-2)
			}),
			false,
		},
		{
			// refund w/ valid lock time
			makeWitnessTestCase(t, func() (wire.TxWitness, error) {
				return receiverHtlcSpendTimeout(htlcScript, paymentAmt,
					aliceKeyPriv, sweepTx, cltvTimeout)
			}),
			true,
		},
	}

	for i, testCase := range testCases {
		sweepTx.TxIn[0].Witness = testCase.witness()

		vm, err := txscript.NewEngine(htlcWitnessScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(paymentAmt))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}

		// This buffer will trace execution of the Script, only dumping
		// out to stdout in the case that a test fails.
		var debugBuf bytes.Buffer

		done := false
		for !done {
			dis, err := vm.DisasmPC()
			if err != nil {
				t.Fatalf("stepping (%v)\n", err)
			}
			debugBuf.WriteString(fmt.Sprintf("stepping %v\n", dis))

			done, err = vm.Step()
			if err != nil && testCase.valid {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v failed, spend should be valid: %v", i, err)
			} else if err == nil && !testCase.valid && done {
				fmt.Println(debugBuf.String())
				t.Fatalf("spend test case #%v succeed, spend should be invalid: %v", i, err)
			}

			debugBuf.WriteString(fmt.Sprintf("Stack: ", vm.GetStack()))
			debugBuf.WriteString(fmt.Sprintf("AltStack: ", vm.GetAltStack()))
		}
	}
}

func TestCommitTxStateHint(t *testing.T) {
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&wire.TxIn{})

	var obsfucator [StateHintSize]byte
	copy(obsfucator[:], testHdSeed[:StateHintSize])

	for i := 0; i < 10000; i++ {
		stateNum := uint32(i)

		err := SetStateNumHint(commitTx, stateNum, obsfucator)
		if err != nil {
			t.Fatalf("unable to set state num %v: %v", i, err)
		}

		extractedStateNum := GetStateNumHint(commitTx, obsfucator)
		if extractedStateNum != stateNum {
			t.Fatalf("state number mismatched, expected %v, got %v",
				stateNum, extractedStateNum)
		}
	}
}
