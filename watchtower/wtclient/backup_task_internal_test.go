package wtclient

import (
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

const csvDelay uint32 = 144

var (
	zeroSig = makeSig(0)

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
)

type backupTaskTest struct {
	name             string
	chanID           lnwire.ChannelID
	breachInfo       *lnwallet.BreachRetribution
	expToLocalInput  input.Input
	expToRemoteInput input.Input
	expTotalAmt      btcutil.Amount
	expSweepAmt      int64
	expRewardAmt     int64
	expRewardScript  []byte
	session          *wtdb.ClientSessionBody
	bindErr          error
	expSweepScript   []byte
	signer           input.Signer
	chanType         channeldb.ChannelType
	commitType       blob.CommitmentType
}

// genTaskTest creates a instance of a backupTaskTest using the passed
// parameters. This method handles generating a breach transaction and its
// corresponding BreachInfo, as well as setting the wtpolicy.Policy of the given
// session.
func genTaskTest(
	t *testing.T,
	name string,
	stateNum uint64,
	toLocalAmt int64,
	toRemoteAmt int64,
	blobType blob.Type,
	sweepFeeRate chainfee.SatPerKWeight,
	rewardScript []byte,
	expSweepAmt int64,
	expRewardAmt int64,
	bindErr error,
	chanType channeldb.ChannelType) backupTaskTest {

	// Set the anchor or taproot flag in the blob type if the session needs
	// to support anchor or taproot channels.
	if chanType.IsTaproot() {
		blobType |= blob.Type(blob.FlagTaprootChannel)
	} else if chanType.HasAnchors() {
		blobType |= blob.Type(blob.FlagAnchorChannel)
	}

	// Parse the key pairs for all keys used in the test.
	revSK, revPK := btcec.PrivKeyFromBytes(revPrivBytes)
	_, toLocalPK := btcec.PrivKeyFromBytes(toLocalPrivBytes)
	toRemoteSK, toRemotePK := btcec.PrivKeyFromBytes(toRemotePrivBytes)

	commitType, err := blobType.CommitmentType(&chanType)
	require.NoError(t, err)

	// Create the signer, and add the revocation and to-remote privkeys.
	signer := wtmock.NewMockSigner()
	var (
		revKeyLoc      = signer.AddPrivKey(revSK)
		toRemoteKeyLoc = signer.AddPrivKey(toRemoteSK)
	)

	// First, we'll initialize a new breach transaction and the
	// corresponding breach retribution. The retribution stores a pointer to
	// the breach transaction, which we will continue to modify.
	breachTxn := wire.NewMsgTx(2)
	breachInfo := &lnwallet.BreachRetribution{
		RevokedStateNum: stateNum,
		BreachTxHash:    breachTxn.TxHash(),
		KeyRing: &lnwallet.CommitmentKeyRing{
			RevocationKey: revPK,
			ToLocalKey:    toLocalPK,
			ToRemoteKey:   toRemotePK,
		},
		RemoteDelay: csvDelay,
	}

	// Add the sign descriptors and outputs corresponding to the to-local
	// and to-remote outputs, respectively, if either input amount is
	// non-zero. Note that the naming here seems reversed, but both are
	// correct. For example, the to-remote output on the remote party's
	// commitment is an output that pays to us. Hence the retribution refers
	// to that output as local, though relative to their commitment, it is
	// paying to-the-remote party (which is us).
	if toLocalAmt > 0 {
		var toLocalSignDesc *input.SignDescriptor

		if chanType.IsTaproot() {
			scriptTree, _ := input.NewLocalCommitScriptTree(
				csvDelay, toLocalPK, revPK,
				fn.None[txscript.TapLeaf](),
			)

			pkScript, _ := input.PayToTaprootScript(
				scriptTree.TaprootKey,
			)

			revokeTapleafHash := txscript.NewBaseTapLeaf(
				scriptTree.RevocationLeaf.Script,
			).TapHash()

			tapTree := scriptTree.TapscriptTree
			revokeIdx := tapTree.LeafProofIndex[revokeTapleafHash]
			revokeMerkleProof := tapTree.LeafMerkleProofs[revokeIdx]
			revokeControlBlock := revokeMerkleProof.ToControlBlock(
				&input.TaprootNUMSKey,
			)
			ctrlBytes, _ := revokeControlBlock.ToBytes()

			toLocalSignDesc = &input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: revKeyLoc,
					PubKey:     revPK,
				},
				Output: &wire.TxOut{
					Value:    toLocalAmt,
					PkScript: pkScript,
				},
				WitnessScript: scriptTree.RevocationLeaf.Script,
				SignMethod:    input.TaprootScriptSpendSignMethod, //nolint:ll
				HashType:      txscript.SigHashDefault,
				ControlBlock:  ctrlBytes,
			}
		} else {
			toLocalSignDesc = &input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: revKeyLoc,
					PubKey:     revPK,
				},
				Output: &wire.TxOut{
					Value: toLocalAmt,
				},
				HashType: txscript.SigHashAll,
			}
		}

		breachInfo.RemoteOutputSignDesc = toLocalSignDesc
		breachTxn.AddTxOut(toLocalSignDesc.Output)
	}
	if toRemoteAmt > 0 {
		var toRemoteSignDesc *input.SignDescriptor

		if chanType.IsTaproot() {
			scriptTree, _ := input.NewRemoteCommitScriptTree(
				toRemotePK, fn.None[txscript.TapLeaf](),
			)

			pkScript, _ := input.PayToTaprootScript(
				scriptTree.TaprootKey,
			)

			revokeTapleafHash := txscript.NewBaseTapLeaf(
				scriptTree.SettleLeaf.Script,
			).TapHash()

			tapTree := scriptTree.TapscriptTree
			revokeIdx := tapTree.LeafProofIndex[revokeTapleafHash]
			revokeMerkleProof := tapTree.LeafMerkleProofs[revokeIdx]
			revokeControlBlock := revokeMerkleProof.ToControlBlock(
				&input.TaprootNUMSKey,
			)

			ctrlBytes, _ := revokeControlBlock.ToBytes()

			toRemoteSignDesc = &input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: toRemoteKeyLoc,
					PubKey:     toRemotePK,
				},
				WitnessScript: scriptTree.SettleLeaf.Script,
				SignMethod:    input.TaprootScriptSpendSignMethod, //nolint:ll
				Output: &wire.TxOut{
					Value:    toRemoteAmt,
					PkScript: pkScript,
				},
				HashType:     txscript.SigHashDefault,
				InputIndex:   1,
				ControlBlock: ctrlBytes,
			}
		} else {
			toRemoteSignDesc = &input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					KeyLocator: toRemoteKeyLoc,
					PubKey:     toRemotePK,
				},
				Output: &wire.TxOut{
					Value: toRemoteAmt,
				},
				HashType: txscript.SigHashAll,
			}
		}

		breachInfo.LocalOutputSignDesc = toRemoteSignDesc
		breachTxn.AddTxOut(toRemoteSignDesc.Output)
	}

	var (
		toLocalInput  input.Input
		toRemoteInput input.Input
	)

	// Now that the breach transaction has all its outputs, we can compute
	// its txid and inputs spending from it. We also generate the
	// input.Inputs that should be derived by the backup task.
	txid := breachTxn.TxHash()
	var index uint32
	if toLocalAmt > 0 {
		breachInfo.RemoteOutpoint = wire.OutPoint{
			Hash:  txid,
			Index: index,
		}
		toLocalInput, err = commitType.ToLocalInput(breachInfo)
		require.NoError(t, err)

		index++
	}
	if toRemoteAmt > 0 {
		breachInfo.LocalOutpoint = wire.OutPoint{
			Hash:  txid,
			Index: index,
		}

		toRemoteInput, err = commitType.ToRemoteInput(breachInfo)
		require.NoError(t, err)
	}

	return backupTaskTest{
		name:             name,
		breachInfo:       breachInfo,
		expToLocalInput:  toLocalInput,
		expToRemoteInput: toRemoteInput,
		expTotalAmt:      btcutil.Amount(toLocalAmt + toRemoteAmt),
		expSweepAmt:      expSweepAmt,
		expRewardAmt:     expRewardAmt,
		expRewardScript:  rewardScript,
		session: &wtdb.ClientSessionBody{
			Policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blobType,
					SweepFeeRate: sweepFeeRate,
					RewardRate:   10000,
				},
			},
			RewardPkScript: rewardScript,
		},
		bindErr:        bindErr,
		expSweepScript: sweepAddr,
		signer:         signer,
		chanType:       chanType,
		commitType:     commitType,
	}
}

var (
	blobTypeCommitNoReward = blob.FlagCommitOutputs.Type()

	blobTypeCommitReward = (blob.FlagCommitOutputs | blob.FlagReward).Type()

	addr, _ = btcutil.DecodeAddress(
		"tb1pw8gzj8clt3v5lxykpgacpju5n8xteskt7gxhmudu6pa70nwfhe6s3unsyk",
		&chaincfg.TestNet3Params,
	)

	addrScript, _ = txscript.PayToAddrScript(addr)

	sweepAddrScript, _ = btcutil.DecodeAddress(
		"tb1qs3jyc9sf5kak3x0w99cav9u605aeu3t600xxx0",
		&chaincfg.TestNet3Params,
	)

	sweepAddr, _ = txscript.PayToAddrScript(sweepAddrScript)
)

// TestBackupTaskBind tests the initialization and binding of a backupTask to a
// ClientSession. After a successful bind, all parameters of the justice
// transaction should be solidified, so we assert there correctness. In an
// unsuccessful bind, the session-dependent parameters should be unmodified so
// that the backup task can be rescheduled if necessary. Finally, we assert that
// the backup task is able to encrypt a valid justice kit, and that we can
// decrypt it using the breach txid.
func TestBackupTask(t *testing.T) {
	t.Parallel()

	chanTypes := []channeldb.ChannelType{
		channeldb.SingleFunderBit,
		channeldb.SingleFunderTweaklessBit,
		channeldb.AnchorOutputsBit,
		channeldb.SimpleTaprootFeatureBit,
	}

	var backupTaskTests []backupTaskTest
	for _, chanType := range chanTypes {
		// Depending on whether the test is for anchor channels or
		// legacy (tweaked and non-tweaked) channels, adjust the
		// expected sweep amount to accommodate. These are different for
		// several reasons:
		//   - anchor to-remote outputs require a P2WSH sweep rather
		//     than a P2WKH sweep.
		//   - the to-local weight estimate fixes an off-by-one.
		// In tests related to the dust threshold, the size difference
		// between the channel types makes it so that the threshold fee
		// rate is slightly lower (since the transactions are heavier).
		var (
			expSweepCommitNoRewardBoth     int64                  = 299241
			expSweepCommitNoRewardLocal    int64                  = 199514
			expSweepCommitNoRewardRemote   int64                  = 99561
			expSweepCommitRewardBoth       int64                  = 296069
			expSweepCommitRewardLocal      int64                  = 197342
			expSweepCommitRewardRemote     int64                  = 98389
			sweepFeeRateNoRewardRemoteDust chainfee.SatPerKWeight = 227500
			sweepFeeRateRewardRemoteDust   chainfee.SatPerKWeight = 175350
		)
		if chanType.IsTaproot() {
			expSweepCommitNoRewardBoth = 299165
			expSweepCommitNoRewardLocal = 199468
			expSweepCommitNoRewardRemote = 99531
			sweepFeeRateNoRewardRemoteDust = 213200
			expSweepCommitRewardBoth = 295993
			expSweepCommitRewardLocal = 197296
			expSweepCommitRewardRemote = 98359
			sweepFeeRateRewardRemoteDust = 167000
		} else if chanType.HasAnchors() {
			expSweepCommitNoRewardBoth = 299236
			expSweepCommitNoRewardLocal = 199513
			expSweepCommitNoRewardRemote = 99557
			expSweepCommitRewardBoth = 296064
			expSweepCommitRewardLocal = 197341
			expSweepCommitRewardRemote = 98385
			sweepFeeRateNoRewardRemoteDust = 225400
			sweepFeeRateRewardRemoteDust = 174100
		}

		backupTaskTests = append(backupTaskTests, []backupTaskTest{
			genTaskTest(
				t,
				"commit no-reward, both outputs",
				100,                        // stateNum
				200000,                     // toLocalAmt
				100000,                     // toRemoteAmt
				blobTypeCommitNoReward,     // blobType
				1000,                       // sweepFeeRate
				nil,                        // rewardScript
				expSweepCommitNoRewardBoth, // expSweepAmt
				0,                          // expRewardAmt
				nil,                        // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit no-reward, to-local output only",
				1000,                        // stateNum
				200000,                      // toLocalAmt
				0,                           // toRemoteAmt
				blobTypeCommitNoReward,      // blobType
				1000,                        // sweepFeeRate
				nil,                         // rewardScript
				expSweepCommitNoRewardLocal, // expSweepAmt
				0,                           // expRewardAmt
				nil,                         // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit no-reward, to-remote output only",
				1,                            // stateNum
				0,                            // toLocalAmt
				100000,                       // toRemoteAmt
				blobTypeCommitNoReward,       // blobType
				1000,                         // sweepFeeRate
				nil,                          // rewardScript
				expSweepCommitNoRewardRemote, // expSweepAmt
				0,                            // expRewardAmt
				nil,                          // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit no-reward, to-remote output only, creates dust",
				1,                              // stateNum
				0,                              // toLocalAmt
				100000,                         // toRemoteAmt
				blobTypeCommitNoReward,         // blobType
				sweepFeeRateNoRewardRemoteDust, // sweepFeeRate
				nil,                            // rewardScript
				0,                              // expSweepAmt
				0,                              // expRewardAmt
				wtpolicy.ErrCreatesDust,        // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit no-reward, no outputs, fee rate exceeds inputs",
				300,                          // stateNum
				0,                            // toLocalAmt
				0,                            // toRemoteAmt
				blobTypeCommitNoReward,       // blobType
				1000,                         // sweepFeeRate
				nil,                          // rewardScript
				0,                            // expSweepAmt
				0,                            // expRewardAmt
				wtpolicy.ErrFeeExceedsInputs, // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit no-reward, no outputs, fee rate of 0 creates dust",
				300,                     // stateNum
				0,                       // toLocalAmt
				0,                       // toRemoteAmt
				blobTypeCommitNoReward,  // blobType
				0,                       // sweepFeeRate
				nil,                     // rewardScript
				0,                       // expSweepAmt
				0,                       // expRewardAmt
				wtpolicy.ErrCreatesDust, // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, both outputs",
				100,                      // stateNum
				200000,                   // toLocalAmt
				100000,                   // toRemoteAmt
				blobTypeCommitReward,     // blobType
				1000,                     // sweepFeeRate
				addrScript,               // rewardScript
				expSweepCommitRewardBoth, // expSweepAmt
				3000,                     // expRewardAmt
				nil,                      // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, to-local output only",
				1000,                      // stateNum
				200000,                    // toLocalAmt
				0,                         // toRemoteAmt
				blobTypeCommitReward,      // blobType
				1000,                      // sweepFeeRate
				addrScript,                // rewardScript
				expSweepCommitRewardLocal, // expSweepAmt
				2000,                      // expRewardAmt
				nil,                       // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, to-remote output only",
				1,                          // stateNum
				0,                          // toLocalAmt
				100000,                     // toRemoteAmt
				blobTypeCommitReward,       // blobType
				1000,                       // sweepFeeRate
				addrScript,                 // rewardScript
				expSweepCommitRewardRemote, // expSweepAmt
				1000,                       // expRewardAmt
				nil,                        // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, to-remote output only, creates dust",
				1,                            // stateNum
				0,                            // toLocalAmt
				108221,                       // toRemoteAmt
				blobTypeCommitReward,         // blobType
				sweepFeeRateRewardRemoteDust, // sweepFeeRate
				addrScript,                   // rewardScript
				0,                            // expSweepAmt
				0,                            // expRewardAmt
				wtpolicy.ErrCreatesDust,      // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, no outputs, fee rate exceeds inputs",
				300,                          // stateNum
				0,                            // toLocalAmt
				0,                            // toRemoteAmt
				blobTypeCommitReward,         // blobType
				1000,                         // sweepFeeRate
				addrScript,                   // rewardScript
				0,                            // expSweepAmt
				0,                            // expRewardAmt
				wtpolicy.ErrFeeExceedsInputs, // bindErr
				chanType,
			),
			genTaskTest(
				t,
				"commit reward, no outputs, fee rate of 0 creates dust",
				300,                     // stateNum
				0,                       // toLocalAmt
				0,                       // toRemoteAmt
				blobTypeCommitReward,    // blobType
				0,                       // sweepFeeRate
				addrScript,              // rewardScript
				0,                       // expSweepAmt
				0,                       // expRewardAmt
				wtpolicy.ErrCreatesDust, // bindErr
				chanType,
			),
		}...)
	}

	for _, test := range backupTaskTests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			testBackupTask(t, test)
		})
	}
}

func testBackupTask(t *testing.T, test backupTaskTest) {
	// Create a new backupTask from the channel id and breach info.
	id := wtdb.BackupID{
		ChanID:       test.chanID,
		CommitHeight: test.breachInfo.RevokedStateNum,
	}
	task := newBackupTask(id, test.expSweepScript)

	// getBreachInfo is a helper closure that returns the breach retribution
	// info and channel type for the given channel and commit height.
	getBreachInfo := func(id lnwire.ChannelID, commitHeight uint64) (
		*lnwallet.BreachRetribution, channeldb.ChannelType, error) {

		return test.breachInfo, test.chanType, nil
	}

	// Reconstruct the expected input.Inputs that will be returned by the
	// task's inputs() method.
	expInputs := make(map[wire.OutPoint]input.Input)
	if task.toLocalInput != nil {
		expInputs[task.toLocalInput.OutPoint()] = task.toLocalInput
	}
	if task.toRemoteInput != nil {
		expInputs[task.toRemoteInput.OutPoint()] = task.toRemoteInput
	}

	// Assert that the inputs method returns the correct slice of
	// input.Inputs.
	inputs := task.inputs()
	require.Equal(t, expInputs, inputs)

	// Now, bind the session to the task. If successful, this locks in the
	// session's negotiated parameters and allows the backup task to derive
	// the final free variables in the justice transaction.
	err := task.bindSession(test.session, getBreachInfo)
	require.ErrorIs(t, err, test.bindErr)

	// Assert that all parameters set during after binding the backup task
	// are properly populated.
	require.Equal(t, test.chanID, task.id.ChanID)
	require.Equal(t, test.breachInfo.RevokedStateNum, task.id.CommitHeight)
	require.Equal(t, test.expTotalAmt, task.totalAmt)
	require.Equal(t, test.breachInfo, task.breachInfo)
	require.Equal(t, test.expToLocalInput, task.toLocalInput)
	require.Equal(t, test.expToRemoteInput, task.toRemoteInput)

	// Exit early if the bind was supposed to fail. But first, we check that
	// all fields set during a bind are still unset. This ensure that a
	// failed bind doesn't have side-effects if the task is retried with a
	// different session.
	if test.bindErr != nil {
		require.Zerof(t, task.blobType, "blob type should not be set "+
			"on failed bind, found: %s", task.blobType)

		require.Nilf(t, task.outputs, "justice outputs should not be "+
			" set on failed bind, found: %v", task.outputs)

		return
	}

	// Otherwise, the binding succeeded. Assert that all values set during
	// the bind are properly populated.
	policy := test.session.Policy
	require.Equal(t, policy.BlobType, task.blobType)

	// Compute the expected outputs on the justice transaction.
	var expOutputs = []*wire.TxOut{
		{
			PkScript: test.expSweepScript,
			Value:    test.expSweepAmt,
		},
	}

	// If the policy specifies a reward output, add it to the expected list
	// of outputs.
	if test.session.Policy.BlobType.Has(blob.FlagReward) {
		expOutputs = append(expOutputs, &wire.TxOut{
			PkScript: test.expRewardScript,
			Value:    test.expRewardAmt,
		})
	}

	// Assert that the computed outputs match our expected outputs.
	require.Equal(t, expOutputs, task.outputs)

	// Now, we'll construct, sign, and encrypt the blob containing the parts
	// needed to reconstruct the justice transaction.
	hint, encBlob, err := task.craftSessionPayload(test.signer)
	require.NoError(t, err, "unable to craft session payload")

	// Verify that the breach hint matches the prefix of SHA256(txid).
	breachTxID := test.breachInfo.BreachTxHash
	expHint := blob.NewBreachHintFromHash(&breachTxID)
	require.Equal(t, expHint, hint)

	// Decrypt the return blob to obtain the JusticeKit containing its
	// contents.
	key := blob.NewBreachKeyFromHash(&breachTxID)
	jKit, err := blob.Decrypt(key, encBlob, policy.BlobType)
	require.NoError(t, err, "unable to decrypt blob")

	keyRing := test.breachInfo.KeyRing
	expToLocalPK := keyRing.ToLocalKey
	expRevPK := keyRing.RevocationKey
	expToRemotePK := keyRing.ToRemoteKey

	breachInfo := &lnwallet.BreachRetribution{
		RemoteDelay: csvDelay,
		KeyRing: &lnwallet.CommitmentKeyRing{
			ToLocalKey:    expToLocalPK,
			RevocationKey: expRevPK,
			ToRemoteKey:   expToRemotePK,
		},
	}

	expectedKit, err := test.commitType.NewJusticeKit(
		test.expSweepScript, breachInfo, test.expToRemoteInput != nil,
	)
	require.NoError(t, err)

	jKit.AddToLocalSig(zeroSig)
	jKit.AddToRemoteSig(zeroSig)

	require.Equal(t, expectedKit, jKit)
}

func makeSig(i int) lnwire.Sig {
	var sigBytes [64]byte
	binary.BigEndian.PutUint64(sigBytes[:8], uint64(i))

	sig, _ := lnwire.NewSigFromWireECDSA(sigBytes[:])

	return sig
}
