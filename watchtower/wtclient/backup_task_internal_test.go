package wtclient

import (
	"bytes"
	"crypto/rand"
	"io"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

const csvDelay uint32 = 144

var (
	zeroPK  [33]byte
	zeroSig [64]byte

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

func makeAddrSlice(size int) []byte {
	addr := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, addr); err != nil {
		panic("cannot make addr")
	}
	return addr
}

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
	session          *wtdb.ClientSession
	bindErr          error
	expSweepScript   []byte
	signer           input.Signer
}

// genTaskTest creates a instance of a backupTaskTest using the passed
// parameters. This method handles generating a breach transaction and its
// corresponding BreachInfo, as well as setting the wtpolicy.Policy of the given
// session.
func genTaskTest(
	name string,
	stateNum uint64,
	toLocalAmt int64,
	toRemoteAmt int64,
	blobType blob.Type,
	sweepFeeRate lnwallet.SatPerKWeight,
	rewardScript []byte,
	expSweepAmt int64,
	expRewardAmt int64,
	bindErr error) backupTaskTest {

	// Parse the key pairs for all keys used in the test.
	revSK, revPK := btcec.PrivKeyFromBytes(
		btcec.S256(), revPrivBytes,
	)
	_, toLocalPK := btcec.PrivKeyFromBytes(
		btcec.S256(), toLocalPrivBytes,
	)
	toRemoteSK, toRemotePK := btcec.PrivKeyFromBytes(
		btcec.S256(), toRemotePrivBytes,
	)

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
		RevokedStateNum:   stateNum,
		BreachTransaction: breachTxn,
		KeyRing: &lnwallet.CommitmentKeyRing{
			RevocationKey: revPK,
			DelayKey:      toLocalPK,
			NoDelayKey:    toRemotePK,
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
		toLocalSignDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: revKeyLoc,
				PubKey:     revPK,
			},
			Output: &wire.TxOut{
				Value: toLocalAmt,
			},
			HashType: txscript.SigHashAll,
		}
		breachInfo.RemoteOutputSignDesc = toLocalSignDesc
		breachTxn.AddTxOut(toLocalSignDesc.Output)
	}
	if toRemoteAmt > 0 {
		toRemoteSignDesc := &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: toRemoteKeyLoc,
				PubKey:     toRemotePK,
			},
			Output: &wire.TxOut{
				Value: toRemoteAmt,
			},
			HashType: txscript.SigHashAll,
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
		toLocalInput = input.NewBaseInput(
			&breachInfo.RemoteOutpoint,
			input.CommitmentRevoke,
			breachInfo.RemoteOutputSignDesc,
			0,
		)
		index++
	}
	if toRemoteAmt > 0 {
		breachInfo.LocalOutpoint = wire.OutPoint{
			Hash:  txid,
			Index: index,
		}
		toRemoteInput = input.NewBaseInput(
			&breachInfo.LocalOutpoint,
			input.CommitmentNoDelay,
			breachInfo.LocalOutputSignDesc,
			0,
		)
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
		session: &wtdb.ClientSession{
			Policy: wtpolicy.Policy{
				BlobType:     blobType,
				SweepFeeRate: sweepFeeRate,
				RewardRate:   10000,
			},
			RewardPkScript: rewardScript,
		},
		bindErr:        bindErr,
		expSweepScript: makeAddrSlice(22),
		signer:         signer,
	}
}

var (
	blobTypeCommitNoReward = blob.FlagCommitOutputs.Type()

	blobTypeCommitReward = (blob.FlagCommitOutputs | blob.FlagReward).Type()

	addr, _ = btcutil.DecodeAddress(
		"mrX9vMRYLfVy1BnZbc5gZjuyaqH3ZW2ZHz", &chaincfg.TestNet3Params,
	)

	addrScript, _ = txscript.PayToAddrScript(addr)
)

var backupTaskTests = []backupTaskTest{
	genTaskTest(
		"commit no-reward, both outputs",
		100,                    // stateNum
		200000,                 // toLocalAmt
		100000,                 // toRemoteAmt
		blobTypeCommitNoReward, // blobType
		1000,                   // sweepFeeRate
		nil,                    // rewardScript
		299241,                 // expSweepAmt
		0,                      // expRewardAmt
		nil,                    // bindErr
	),
	genTaskTest(
		"commit no-reward, to-local output only",
		1000,                   // stateNum
		200000,                 // toLocalAmt
		0,                      // toRemoteAmt
		blobTypeCommitNoReward, // blobType
		1000,                   // sweepFeeRate
		nil,                    // rewardScript
		199514,                 // expSweepAmt
		0,                      // expRewardAmt
		nil,                    // bindErr
	),
	genTaskTest(
		"commit no-reward, to-remote output only",
		1,                      // stateNum
		0,                      // toLocalAmt
		100000,                 // toRemoteAmt
		blobTypeCommitNoReward, // blobType
		1000,                   // sweepFeeRate
		nil,                    // rewardScript
		99561,                  // expSweepAmt
		0,                      // expRewardAmt
		nil,                    // bindErr
	),
	genTaskTest(
		"commit no-reward, to-remote output only, creates dust",
		1,                       // stateNum
		0,                       // toLocalAmt
		100000,                  // toRemoteAmt
		blobTypeCommitNoReward,  // blobType
		227500,                  // sweepFeeRate
		nil,                     // rewardScript
		0,                       // expSweepAmt
		0,                       // expRewardAmt
		wtpolicy.ErrCreatesDust, // bindErr
	),
	genTaskTest(
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
	),
	genTaskTest(
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
	),
	genTaskTest(
		"commit reward, both outputs",
		100,                  // stateNum
		200000,               // toLocalAmt
		100000,               // toRemoteAmt
		blobTypeCommitReward, // blobType
		1000,                 // sweepFeeRate
		addrScript,           // rewardScript
		296117,               // expSweepAmt
		3000,                 // expRewardAmt
		nil,                  // bindErr
	),
	genTaskTest(
		"commit reward, to-local output only",
		1000,                 // stateNum
		200000,               // toLocalAmt
		0,                    // toRemoteAmt
		blobTypeCommitReward, // blobType
		1000,                 // sweepFeeRate
		addrScript,           // rewardScript
		197390,               // expSweepAmt
		2000,                 // expRewardAmt
		nil,                  // bindErr
	),
	genTaskTest(
		"commit reward, to-remote output only",
		1,                    // stateNum
		0,                    // toLocalAmt
		100000,               // toRemoteAmt
		blobTypeCommitReward, // blobType
		1000,                 // sweepFeeRate
		addrScript,           // rewardScript
		98437,                // expSweepAmt
		1000,                 // expRewardAmt
		nil,                  // bindErr
	),
	genTaskTest(
		"commit reward, to-remote output only, creates dust",
		1,                       // stateNum
		0,                       // toLocalAmt
		100000,                  // toRemoteAmt
		blobTypeCommitReward,    // blobType
		175000,                  // sweepFeeRate
		addrScript,              // rewardScript
		0,                       // expSweepAmt
		0,                       // expRewardAmt
		wtpolicy.ErrCreatesDust, // bindErr
	),
	genTaskTest(
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
	),
	genTaskTest(
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
	),
}

// TestBackupTaskBind tests the initialization and binding of a backupTask to a
// ClientSession. After a successful bind, all parameters of the justice
// transaction should be solidified, so we assert there correctness. In an
// unsuccessful bind, the session-dependent parameters should be unmodified so
// that the backup task can be rescheduled if necessary. Finally, we assert that
// the backup task is able to encrypt a valid justice kit, and that we can
// decrypt it using the breach txid.
func TestBackupTask(t *testing.T) {
	t.Parallel()

	for _, test := range backupTaskTests {
		t.Run(test.name, func(t *testing.T) {
			testBackupTask(t, test)
		})
	}
}

func testBackupTask(t *testing.T, test backupTaskTest) {
	// Create a new backupTask from the channel id and breach info.
	task := newBackupTask(&test.chanID, test.breachInfo, test.expSweepScript)

	// Assert that all parameters set during initialization are properly
	// populated.
	if task.id.ChanID != test.chanID {
		t.Fatalf("channel id mismatch, want: %s, got: %s",
			test.chanID, task.id.ChanID)
	}

	if task.id.CommitHeight != test.breachInfo.RevokedStateNum {
		t.Fatalf("commit height mismatch, want: %d, got: %d",
			test.breachInfo.RevokedStateNum, task.id.CommitHeight)
	}

	if task.totalAmt != test.expTotalAmt {
		t.Fatalf("total amount mismatch, want: %d, got: %v",
			test.expTotalAmt, task.totalAmt)
	}

	if !reflect.DeepEqual(task.breachInfo, test.breachInfo) {
		t.Fatalf("breach info mismatch, want: %v, got: %v",
			test.breachInfo, task.breachInfo)
	}

	if !reflect.DeepEqual(task.toLocalInput, test.expToLocalInput) {
		t.Fatalf("to-local input mismatch, want: %v, got: %v",
			test.expToLocalInput, task.toLocalInput)
	}

	if !reflect.DeepEqual(task.toRemoteInput, test.expToRemoteInput) {
		t.Fatalf("to-local input mismatch, want: %v, got: %v",
			test.expToRemoteInput, task.toRemoteInput)
	}

	// Reconstruct the expected input.Inputs that will be returned by the
	// task's inputs() method.
	expInputs := make(map[wire.OutPoint]input.Input)
	if task.toLocalInput != nil {
		expInputs[*task.toLocalInput.OutPoint()] = task.toLocalInput
	}
	if task.toRemoteInput != nil {
		expInputs[*task.toRemoteInput.OutPoint()] = task.toRemoteInput
	}

	// Assert that the inputs method returns the correct slice of
	// input.Inputs.
	inputs := task.inputs()
	if !reflect.DeepEqual(expInputs, inputs) {
		t.Fatalf("inputs mismatch, want: %v, got: %v",
			expInputs, inputs)
	}

	// Now, bind the session to the task. If successful, this locks in the
	// session's negotiated parameters and allows the backup task to derive
	// the final free variables in the justice transaction.
	err := task.bindSession(test.session)
	if err != test.bindErr {
		t.Fatalf("expected: %v when binding session, got: %v",
			test.bindErr, err)
	}

	// Exit early if the bind was supposed to fail. But first, we check that
	// all fields set during a bind are still unset. This ensure that a
	// failed bind doesn't have side-effects if the task is retried with a
	// different session.
	if test.bindErr != nil {
		if task.blobType != 0 {
			t.Fatalf("blob type should not be set on failed bind, "+
				"found: %s", task.blobType)
		}

		if task.outputs != nil {
			t.Fatalf("justice outputs should not be set on failed bind, "+
				"found: %v", task.outputs)
		}

		return
	}

	// Otherwise, the binding succeeded. Assert that all values set during
	// the bind are properly populated.
	policy := test.session.Policy
	if task.blobType != policy.BlobType {
		t.Fatalf("blob type mismatch, want: %s, got %s",
			policy.BlobType, task.blobType)
	}

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
	if !reflect.DeepEqual(expOutputs, task.outputs) {
		t.Fatalf("justice txn output mismatch, want: %v,\ngot: %v",
			spew.Sdump(expOutputs), spew.Sdump(task.outputs))
	}

	// Now, we'll construct, sign, and encrypt the blob containing the parts
	// needed to reconstruct the justice transaction.
	hint, encBlob, err := task.craftSessionPayload(test.signer)
	if err != nil {
		t.Fatalf("unable to craft session payload: %v", err)
	}

	// Verify that the breach hint matches the breach txid's prefix.
	breachTxID := test.breachInfo.BreachTransaction.TxHash()
	expHint := wtdb.NewBreachHintFromHash(&breachTxID)
	if hint != expHint {
		t.Fatalf("breach hint mismatch, want: %x, got: %v",
			expHint, hint)
	}

	// Decrypt the return blob to obtain the JusticeKit containing its
	// contents.
	jKit, err := blob.Decrypt(breachTxID[:], encBlob, policy.BlobType)
	if err != nil {
		t.Fatalf("unable to decrypt blob: %v", err)
	}

	keyRing := test.breachInfo.KeyRing
	expToLocalPK := keyRing.DelayKey.SerializeCompressed()
	expRevPK := keyRing.RevocationKey.SerializeCompressed()
	expToRemotePK := keyRing.NoDelayKey.SerializeCompressed()

	// Assert that the blob contained the serialized revocation and to-local
	// pubkeys.
	if !bytes.Equal(jKit.RevocationPubKey[:], expRevPK) {
		t.Fatalf("revocation pk mismatch, want: %x, got: %x",
			expRevPK, jKit.RevocationPubKey[:])
	}
	if !bytes.Equal(jKit.LocalDelayPubKey[:], expToLocalPK) {
		t.Fatalf("revocation pk mismatch, want: %x, got: %x",
			expToLocalPK, jKit.LocalDelayPubKey[:])
	}

	// Determine if the breach transaction has a to-remote output and/or
	// to-local output to spend from. Note the seemingly-reversed
	// nomenclature.
	hasToRemote := test.breachInfo.LocalOutputSignDesc != nil
	hasToLocal := test.breachInfo.RemoteOutputSignDesc != nil

	// If the to-remote output is present, assert that the to-remote public
	// key was included in the blob.
	if hasToRemote &&
		!bytes.Equal(jKit.CommitToRemotePubKey[:], expToRemotePK) {
		t.Fatalf("mismatch to-remote pubkey, want: %x, got: %x",
			expToRemotePK, jKit.CommitToRemotePubKey)
	}

	// Otherwise if the to-local output is not present, assert that a blank
	// public key was inserted.
	if !hasToRemote &&
		!bytes.Equal(jKit.CommitToRemotePubKey[:], zeroPK[:]) {
		t.Fatalf("mismatch to-remote pubkey, want: %x, got: %x",
			zeroPK, jKit.CommitToRemotePubKey)
	}

	// Assert that the CSV is encoded in the blob.
	if jKit.CSVDelay != test.breachInfo.RemoteDelay {
		t.Fatalf("mismatch remote delay, want: %d, got: %v",
			test.breachInfo.RemoteDelay, jKit.CSVDelay)
	}

	// Assert that the sweep pkscript is included.
	if !bytes.Equal(jKit.SweepAddress, test.expSweepScript) {
		t.Fatalf("sweep pkscript mismatch, want: %x, got: %x",
			test.expSweepScript, jKit.SweepAddress)
	}

	// Finally, verify that the signatures are encoded in the justice kit.
	// We don't validate the actual signatures produced here, since at the
	// moment, it is tested indirectly by other packages and integration
	// tests.
	// TODO(conner): include signature validation checks

	emptyToLocalSig := bytes.Equal(jKit.CommitToLocalSig[:], zeroSig[:])
	switch {
	case hasToLocal && emptyToLocalSig:
		t.Fatalf("to-local signature should not be empty")
	case !hasToLocal && !emptyToLocalSig:
		t.Fatalf("to-local signature should be empty")
	}

	emptyToRemoteSig := bytes.Equal(jKit.CommitToRemoteSig[:], zeroSig[:])
	switch {
	case hasToRemote && emptyToRemoteSig:
		t.Fatalf("to-remote signature should not be empty")
	case !hasToRemote && !emptyToRemoteSig:
		t.Fatalf("to-remote signature should be empty")
	}
}
