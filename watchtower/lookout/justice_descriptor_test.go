package lookout_test

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/lookout"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

const csvDelay uint32 = 144

var (
	revPrivBytes = []byte{
		0x8f, 0x4b, 0x51, 0x83, 0xa9, 0x34, 0xbd, 0x5f,
		0x74, 0x6c, 0x9d, 0x5c, 0xae, 0x88, 0x2d, 0x31,
		0x06, 0x90, 0xdd, 0x8c, 0x9b, 0x31, 0xbc, 0xd1,
		0x78, 0x91, 0x88, 0x2a, 0xf9, 0x74, 0xa0, 0xef,
	}

	toLocalPrivBytes = []byte{
		0xde, 0x17, 0xc1, 0x2f, 0xdc, 0x1b, 0xc0, 0xc6,
		0x59, 0x5d, 0xf9, 0xc1, 0x3e, 0x89, 0xbc, 0x6f,
		0x01, 0x85, 0x45, 0x76, 0x26, 0xce, 0x9c, 0x55,
		0x3b, 0xc9, 0xec, 0x3d, 0xd8, 0x8b, 0xac, 0xa8,
	}

	toRemotePrivBytes = []byte{
		0x28, 0x59, 0x6f, 0x36, 0xb8, 0x9f, 0x19, 0x5d,
		0xcb, 0x07, 0x48, 0x8a, 0xe5, 0x89, 0x71, 0x74,
		0x70, 0x4c, 0xff, 0x1e, 0x9c, 0x00, 0x93, 0xbe,
		0xe2, 0x2e, 0x68, 0x08, 0x4c, 0xb4, 0x0f, 0x4f,
	}

	rewardCommitType = blob.TypeFromFlags(
		blob.FlagReward, blob.FlagCommitOutputs,
	)

	altruistCommitType = blob.FlagCommitOutputs.Type()

	altruistAnchorCommitType = blob.TypeAltruistAnchorCommit
)

// TestJusticeDescriptor asserts that a JusticeDescriptor is able to produce the
// correct justice transaction for different blob types.
func TestJusticeDescriptor(t *testing.T) {
	tests := []struct {
		name     string
		blobType blob.Type
	}{
		{
			name:     "reward and commit type",
			blobType: rewardCommitType,
		},
		{
			name:     "altruist and commit type",
			blobType: altruistCommitType,
		},
		{
			name:     "altruist anchor commit type",
			blobType: altruistAnchorCommitType,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testJusticeDescriptor(t, test.blobType)
		})
	}
}

func testJusticeDescriptor(t *testing.T, blobType blob.Type) {
	isAnchorChannel := blobType.IsAnchorChannel()

	const (
		localAmount  = btcutil.Amount(100000)
		remoteAmount = btcutil.Amount(200000)
		totalAmount  = localAmount + remoteAmount
	)

	// Parse the key pairs for all keys used in the test.
	revSK, revPK := btcec.PrivKeyFromBytes(
		revPrivBytes,
	)
	_, toLocalPK := btcec.PrivKeyFromBytes(
		toLocalPrivBytes,
	)
	toRemoteSK, toRemotePK := btcec.PrivKeyFromBytes(
		toRemotePrivBytes,
	)

	// Create the signer, and add the revocation and to-remote privkeys.
	signer := wtmock.NewMockSigner()
	var (
		revKeyLoc      = signer.AddPrivKey(revSK)
		toRemoteKeyLoc = signer.AddPrivKey(toRemoteSK)
	)

	// Construct the to-local witness script.
	toLocalScript, err := input.CommitScriptToSelf(
		csvDelay, toLocalPK, revPK,
	)
	require.Nil(t, err)

	// Compute the to-local witness script hash.
	toLocalScriptHash, err := input.WitnessScriptHash(toLocalScript)
	require.Nil(t, err)

	// Compute the to-remote redeem script, witness script hash, and
	// sequence numbers.
	//
	// NOTE: This is pretty subtle.
	//
	// The actual redeem script for a p2wkh output is just the pubkey, but
	// the witness sighash calculation injects the classic p2kh script:
	// OP_DUP OP_HASH160 <pubkey-hash160> OP_EQUALVERIFY OP_CHECKSIG. When
	// signing for p2wkh we don't pass the raw pubkey as the witness script
	// to the sign descriptor (since that's also not a valid script).
	// Instead we give it the _pkscript_ of the form OP_0 <pubkey-hash160>
	// from which pubkey-hash160 is extracted during sighash calculation.
	//
	// On the other hand, signing for the anchor p2wsh to-remote outputs
	// requires the sign descriptor to contain the redeem script ver batim.
	// This difference in behavior forces us to use a distinct
	// toRemoteSigningScript to handle both cases.
	var (
		toRemoteSequence      uint32
		toRemoteRedeemScript  []byte
		toRemoteScriptHash    []byte
		toRemoteSigningScript []byte
	)
	if isAnchorChannel {
		toRemoteSequence = 1
		toRemoteRedeemScript, err = input.CommitScriptToRemoteConfirmed(
			toRemotePK,
		)
		require.Nil(t, err)

		toRemoteScriptHash, err = input.WitnessScriptHash(
			toRemoteRedeemScript,
		)
		require.Nil(t, err)

		// As it should be.
		toRemoteSigningScript = toRemoteRedeemScript
	} else {
		toRemoteRedeemScript = toRemotePK.SerializeCompressed()
		toRemoteScriptHash, err = input.CommitScriptUnencumbered(
			toRemotePK,
		)
		require.Nil(t, err)

		// NOTE: This is the _pkscript_.
		toRemoteSigningScript = toRemoteScriptHash
	}

	// Construct the breaching commitment txn, containing the to-local and
	// to-remote outputs. We don't need any inputs for this test.
	breachTxn := &wire.MsgTx{
		Version: 2,
		TxIn:    []*wire.TxIn{},
		TxOut: []*wire.TxOut{
			{
				Value:    int64(localAmount),
				PkScript: toLocalScriptHash,
			},
			{
				Value:    int64(remoteAmount),
				PkScript: toRemoteScriptHash,
			},
		},
	}
	breachTxID := breachTxn.TxHash()

	// Compute the weight estimate for our justice transaction.
	var weightEstimate input.TxWeightEstimator

	// An older ToLocalPenaltyWitnessSize constant used to underestimate the
	// size by one byte. The diferrence in weight can cause different output
	// values on the sweep transaction, so we mimic the original bug and
	// create signatures using the original weight estimate. For anchor
	// channels we fix this and use the correct witness size.
	if isAnchorChannel {
		weightEstimate.AddWitnessInput(input.ToLocalPenaltyWitnessSize)
	} else {
		weightEstimate.AddWitnessInput(input.ToLocalPenaltyWitnessSize - 1)
	}

	if isAnchorChannel {
		weightEstimate.AddWitnessInput(input.ToRemoteConfirmedWitnessSize)
	} else {
		weightEstimate.AddWitnessInput(input.P2WKHWitnessSize)
	}
	weightEstimate.AddP2WKHOutput()
	if blobType.Has(blob.FlagReward) {
		weightEstimate.AddP2WKHOutput()
	}
	txWeight := weightEstimate.Weight()

	// Create a session info so that simulate agreement of the sweep
	// parameters that should be used in constructing the justice
	// transaction.
	policy := wtpolicy.Policy{
		TxPolicy: wtpolicy.TxPolicy{
			BlobType:     blobType,
			SweepFeeRate: 2000,
			RewardRate:   900000,
		},
	}
	sessionInfo := &wtdb.SessionInfo{
		Policy:        policy,
		RewardAddress: makeAddrSlice(22),
	}

	// Begin to assemble the justice kit, starting with the sweep address,
	// pubkeys, and csv delay.
	justiceKit := &blob.JusticeKit{
		BlobType:     blobType,
		SweepAddress: makeAddrSlice(22),
		CSVDelay:     csvDelay,
	}
	copy(justiceKit.RevocationPubKey[:], revPK.SerializeCompressed())
	copy(justiceKit.LocalDelayPubKey[:], toLocalPK.SerializeCompressed())
	copy(justiceKit.CommitToRemotePubKey[:], toRemotePK.SerializeCompressed())

	// Create a transaction spending from the outputs of the breach
	// transaction created earlier. The inputs are always ordered w/
	// to-local and then to-remote. The outputs are always added as the
	// sweep address then reward address.
	justiceTxn := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  breachTxID,
					Index: 0,
				},
			},
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  breachTxID,
					Index: 1,
				},
				Sequence: toRemoteSequence,
			},
		},
	}

	outputs, err := policy.ComputeJusticeTxOuts(
		totalAmount, int64(txWeight), justiceKit.SweepAddress,
		sessionInfo.RewardAddress,
	)
	require.Nil(t, err)

	// Attach the txouts and BIP69 sort the resulting transaction.
	justiceTxn.TxOut = outputs
	txsort.InPlaceSort(justiceTxn)

	hashCache := input.NewTxSigHashesV0Only(justiceTxn)

	// Create the sign descriptor used to sign for the to-local input.
	toLocalSignDesc := &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: revKeyLoc,
		},
		WitnessScript: toLocalScript,
		Output:        breachTxn.TxOut[0],
		SigHashes:     hashCache,
		InputIndex:    0,
		HashType:      txscript.SigHashAll,
	}

	// Create the sign descriptor used to sign for the to-remote input.
	toRemoteSignDesc := &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: toRemoteKeyLoc,
			PubKey:     toRemotePK,
		},
		WitnessScript: toRemoteSigningScript,
		Output:        breachTxn.TxOut[1],
		SigHashes:     hashCache,
		InputIndex:    1,
		HashType:      txscript.SigHashAll,
	}

	// Verify that our test justice transaction is sane.
	btx := btcutil.NewTx(justiceTxn)
	err = blockchain.CheckTransactionSanity(btx)
	require.Nil(t, err)

	// Compute a DER-encoded signature for the to-local input.
	toLocalSigRaw, err := signer.SignOutputRaw(justiceTxn, toLocalSignDesc)
	require.Nil(t, err)

	// Compute the witness for the to-remote input. The first element is a
	// DER-encoded signature under the to-remote pubkey. The sighash flag is
	// also present, so we trim it.
	toRemoteSigRaw, err := signer.SignOutputRaw(justiceTxn, toRemoteSignDesc)
	require.Nil(t, err)

	// Convert the DER to-local sig into a fixed-size signature.
	toLocalSig, err := lnwire.NewSigFromSignature(toLocalSigRaw)
	require.Nil(t, err)

	// Convert the DER to-remote sig into a fixed-size signature.
	toRemoteSig, err := lnwire.NewSigFromSignature(toRemoteSigRaw)
	require.Nil(t, err)

	// Complete our justice kit by copying the signatures into the payload.
	copy(justiceKit.CommitToLocalSig[:], toLocalSig[:])
	copy(justiceKit.CommitToRemoteSig[:], toRemoteSig[:])

	justiceDesc := &lookout.JusticeDescriptor{
		BreachedCommitTx: breachTxn,
		SessionInfo:      sessionInfo,
		JusticeKit:       justiceKit,
	}

	// Construct a breach punisher that will feed published transactions
	// over the buffered channel.
	publications := make(chan *wire.MsgTx, 1)
	punisher := lookout.NewBreachPunisher(&lookout.PunisherConfig{
		PublishTx: func(tx *wire.MsgTx, _ string) error {
			publications <- tx
			return nil
		},
	})

	// Exact retribution on the offender. If no error is returned, we expect
	// the justice transaction to be published via the channel.
	err = punisher.Punish(justiceDesc, nil)
	require.Nil(t, err)

	// Retrieve the published justice transaction.
	var wtJusticeTxn *wire.MsgTx
	select {
	case wtJusticeTxn = <-publications:
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("punisher did not publish justice txn")
	}

	// Construct the test's to-local witness.
	justiceTxn.TxIn[0].Witness = make([][]byte, 3)
	justiceTxn.TxIn[0].Witness[0] = append(toLocalSigRaw.Serialize(),
		byte(txscript.SigHashAll))
	justiceTxn.TxIn[0].Witness[1] = []byte{1}
	justiceTxn.TxIn[0].Witness[2] = toLocalScript

	// Construct the test's to-remote witness.
	justiceTxn.TxIn[1].Witness = make([][]byte, 2)
	justiceTxn.TxIn[1].Witness[0] = append(toRemoteSigRaw.Serialize(),
		byte(txscript.SigHashAll))
	justiceTxn.TxIn[1].Witness[1] = toRemoteRedeemScript

	// Assert that the watchtower derives the same justice txn.
	require.Equal(t, justiceTxn, wtJusticeTxn)
}
