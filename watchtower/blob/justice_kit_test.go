package blob_test

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
)

func makePubKey(i uint64) blob.PubKey {
	var pk blob.PubKey
	pk[0] = 0x02
	if i%2 == 1 {
		pk[0] |= 0x01
	}
	binary.BigEndian.PutUint64(pk[1:9], i)
	return pk
}

func makeSig(i int) lnwire.Sig {
	var sig lnwire.Sig
	binary.BigEndian.PutUint64(sig[:8], uint64(i))
	return sig
}

func makeAddr(size int) []byte {
	addr := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, addr); err != nil {
		panic("unable to create addr")
	}

	return addr
}

type descriptorTest struct {
	name                 string
	encVersion           blob.Type
	decVersion           blob.Type
	sweepAddr            []byte
	revPubKey            blob.PubKey
	delayPubKey          blob.PubKey
	csvDelay             uint32
	commitToLocalSig     lnwire.Sig
	hasCommitToRemote    bool
	commitToRemotePubKey blob.PubKey
	commitToRemoteSig    lnwire.Sig
	encErr               error
	decErr               error
}

var rewardAndCommitType = blob.TypeFromFlags(
	blob.FlagReward, blob.FlagCommitOutputs,
)

var descriptorTests = []descriptorTest{
	{
		name:             "to-local only",
		encVersion:       blob.TypeDefault,
		decVersion:       blob.TypeDefault,
		sweepAddr:        makeAddr(22),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:                 "to-local and p2wkh",
		encVersion:           rewardAndCommitType,
		decVersion:           rewardAndCommitType,
		sweepAddr:            makeAddr(22),
		revPubKey:            makePubKey(0),
		delayPubKey:          makePubKey(1),
		csvDelay:             144,
		commitToLocalSig:     makeSig(1),
		hasCommitToRemote:    true,
		commitToRemotePubKey: makePubKey(2),
		commitToRemoteSig:    makeSig(2),
	},
	{
		name:             "unknown encrypt version",
		encVersion:       0,
		decVersion:       blob.TypeDefault,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		encErr:           blob.ErrUnknownBlobType,
	},
	{
		name:             "unknown decrypt version",
		encVersion:       blob.TypeDefault,
		decVersion:       0,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		decErr:           blob.ErrUnknownBlobType,
	},
	{
		name:             "sweep addr length zero",
		encVersion:       blob.TypeDefault,
		decVersion:       blob.TypeDefault,
		sweepAddr:        makeAddr(0),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr max size",
		encVersion:       blob.TypeDefault,
		decVersion:       blob.TypeDefault,
		sweepAddr:        makeAddr(blob.MaxSweepAddrSize),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr too long",
		encVersion:       blob.TypeDefault,
		decVersion:       blob.TypeDefault,
		sweepAddr:        makeAddr(blob.MaxSweepAddrSize + 1),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		encErr:           blob.ErrSweepAddressToLong,
	},
}

// TestBlobJusticeKitEncryptDecrypt asserts that encrypting and decrypting a
// plaintext blob produces the original. The tests include negative assertions
// when passed invalid combinations, and that all successfully encrypted blobs
// are of constant size.
func TestBlobJusticeKitEncryptDecrypt(t *testing.T) {
	for _, test := range descriptorTests {
		t.Run(test.name, func(t *testing.T) {
			testBlobJusticeKitEncryptDecrypt(t, test)
		})
	}
}

func testBlobJusticeKitEncryptDecrypt(t *testing.T, test descriptorTest) {
	boj := &blob.JusticeKit{
		SweepAddress:         test.sweepAddr,
		RevocationPubKey:     test.revPubKey,
		LocalDelayPubKey:     test.delayPubKey,
		CSVDelay:             test.csvDelay,
		CommitToLocalSig:     test.commitToLocalSig,
		CommitToRemotePubKey: test.commitToRemotePubKey,
		CommitToRemoteSig:    test.commitToRemoteSig,
	}

	// Generate a random encryption key for the blob. The key is
	// sized at 32 byte, as in practice we will be using the remote
	// party's commitment txid as the key.
	key := make([]byte, blob.KeySize)
	_, err := io.ReadFull(rand.Reader, key)
	if err != nil {
		t.Fatalf("unable to generate blob encryption key: %v", err)
	}

	// Encrypt the blob plaintext using the generated key and
	// target version for this test.
	ctxt, err := boj.Encrypt(key, test.encVersion)
	if err != test.encErr {
		t.Fatalf("unable to encrypt blob: %v", err)
	} else if test.encErr != nil {
		// If the test expected an encryption failure, we can
		// continue to the next test.
		return
	}

	// Ensure that all encrypted blobs are padded out to the same
	// size: 282 bytes for version 0.
	if len(ctxt) != blob.Size(test.encVersion) {
		t.Fatalf("expected blob to have size %d, got %d instead",
			blob.Size(test.encVersion), len(ctxt))

	}

	// Decrypt the encrypted blob, reconstructing the original
	// blob plaintext from the decrypted contents. We use the target
	// decryption version specified by this test case.
	boj2, err := blob.Decrypt(key, ctxt, test.decVersion)
	if err != test.decErr {
		t.Fatalf("unable to decrypt blob: %v", err)
	} else if test.decErr != nil {
		// If the test expected an decryption failure, we can
		// continue to the next test.
		return
	}

	// Check that the decrypted blob properly reports whether it has
	// a to-remote output or not.
	if boj2.HasCommitToRemoteOutput() != test.hasCommitToRemote {
		t.Fatalf("expected blob has_to_remote to be %v, got %v",
			test.hasCommitToRemote, boj2.HasCommitToRemoteOutput())
	}

	// Check that the original blob plaintext matches the
	// one reconstructed from the encrypted blob.
	if !reflect.DeepEqual(boj, boj2) {
		t.Fatalf("decrypted plaintext does not match original, "+
			"want: %v, got %v", boj, boj2)
	}
}

// TestJusticeKitRemoteWitnessConstruction tests that a JusticeKit returns the
// proper to-remote witnes script and to-remote witness stack. This should be
// equivalent to p2wkh spend.
func TestJusticeKitRemoteWitnessConstruction(t *testing.T) {
	// Generate the to-remote pubkey.
	toRemotePrivKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate to-remote priv key: %v", err)
	}

	// Copy the to-remote pubkey into the format expected by our justice
	// kit.
	var toRemotePubKey blob.PubKey
	copy(toRemotePubKey[:], toRemotePrivKey.PubKey().SerializeCompressed())

	// Sign a message using the to-remote private key. The exact message
	// doesn't matter as we won't be validating the signature's validity.
	digest := bytes.Repeat([]byte("a"), 32)
	rawToRemoteSig, err := toRemotePrivKey.Sign(digest)
	if err != nil {
		t.Fatalf("unable to generate to-remote signature: %v", err)
	}

	// Convert the DER-encoded signature into a fixed-size sig.
	commitToRemoteSig, err := lnwire.NewSigFromSignature(rawToRemoteSig)
	if err != nil {
		t.Fatalf("unable to convert raw to-remote signature to "+
			"Sig: %v", err)
	}

	// Populate the justice kit fields relevant to the to-remote output.
	justiceKit := &blob.JusticeKit{
		CommitToRemotePubKey: toRemotePubKey,
		CommitToRemoteSig:    commitToRemoteSig,
	}

	// Now, compute the to-remote witness script returned by the justice
	// kit.
	toRemoteScript, err := justiceKit.CommitToRemoteWitnessScript()
	if err != nil {
		t.Fatalf("unable to compute to-remote witness script: %v", err)
	}

	// Assert this is exactly the to-remote, compressed pubkey.
	if !bytes.Equal(toRemoteScript, toRemotePubKey[:]) {
		t.Fatalf("to-remote witness script should be equal to "+
			"to-remote pubkey, want: %x, got %x",
			toRemotePubKey[:], toRemoteScript)
	}

	// Next, compute the to-remote witness stack, which should be a p2wkh
	// witness stack consisting solely of a signature.
	toRemoteWitnessStack, err := justiceKit.CommitToRemoteWitnessStack()
	if err != nil {
		t.Fatalf("unable to compute to-remote witness stack: %v", err)
	}

	// Assert that the witness stack only has one element.
	if len(toRemoteWitnessStack) != 1 {
		t.Fatalf("to-remote witness stack should be of length 1, is %d",
			len(toRemoteWitnessStack))
	}

	// Compute the expected first element, by appending a sighash all byte
	// to our raw DER-encoded signature.
	rawToRemoteSigWithSigHash := append(
		rawToRemoteSig.Serialize(), byte(txscript.SigHashAll),
	)

	// Assert that the expected signature matches the first element in the
	// witness stack.
	if !bytes.Equal(rawToRemoteSigWithSigHash, toRemoteWitnessStack[0]) {
		t.Fatalf("mismatched sig in to-remote witness stack, want: %v, "+
			"got: %v", rawToRemoteSigWithSigHash,
			toRemoteWitnessStack[0])
	}

	// Finally, set the CommitToRemotePubKey to be a blank value.
	justiceKit.CommitToRemotePubKey = blob.PubKey{}

	// When trying to compute the witness script, this should now return
	// ErrNoCommitToRemoteOutput since a valid pubkey could not be parsed
	// from CommitToRemotePubKey.
	_, err = justiceKit.CommitToRemoteWitnessScript()
	if err != blob.ErrNoCommitToRemoteOutput {
		t.Fatalf("expected ErrNoCommitToRemoteOutput, got: %v", err)
	}
}

// TestJusticeKitToLocalWitnessConstruction tests that a JusticeKit returns the
// proper to-local witness script and to-local witness stack for spending the
// revocation path.
func TestJusticeKitToLocalWitnessConstruction(t *testing.T) {
	csvDelay := uint32(144)

	// Generate the revocation and delay private keys.
	revPrivKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate revocation priv key: %v", err)
	}

	delayPrivKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate delay priv key: %v", err)
	}

	// Copy the revocation and delay pubkeys into the format expected by our
	// justice kit.
	var revPubKey blob.PubKey
	copy(revPubKey[:], revPrivKey.PubKey().SerializeCompressed())

	var delayPubKey blob.PubKey
	copy(delayPubKey[:], delayPrivKey.PubKey().SerializeCompressed())

	// Sign a message using the revocation private key. The exact message
	// doesn't matter as we won't be validating the signature's validity.
	digest := bytes.Repeat([]byte("a"), 32)
	rawRevSig, err := revPrivKey.Sign(digest)
	if err != nil {
		t.Fatalf("unable to generate revocation signature: %v", err)
	}

	// Convert the DER-encoded signature into a fixed-size sig.
	commitToLocalSig, err := lnwire.NewSigFromSignature(rawRevSig)
	if err != nil {
		t.Fatalf("unable to convert raw revocation signature to "+
			"Sig: %v", err)
	}

	// Populate the justice kit with fields relevant to the to-local output.
	justiceKit := &blob.JusticeKit{
		CSVDelay:         csvDelay,
		RevocationPubKey: revPubKey,
		LocalDelayPubKey: delayPubKey,
		CommitToLocalSig: commitToLocalSig,
	}

	// Compute the expected to-local script, which is a function of the CSV
	// delay, revocation pubkey and delay pubkey.
	expToLocalScript, err := input.CommitScriptToSelf(
		csvDelay, delayPrivKey.PubKey(), revPrivKey.PubKey(),
	)
	if err != nil {
		t.Fatalf("unable to generate expected to-local script: %v", err)
	}

	// Compute the to-local script that is returned by the justice kit.
	toLocalScript, err := justiceKit.CommitToLocalWitnessScript()
	if err != nil {
		t.Fatalf("unable to compute to-local witness script: %v", err)
	}

	// Assert that the expected to-local script matches the actual script.
	if !bytes.Equal(expToLocalScript, toLocalScript) {
		t.Fatalf("mismatched to-local witness script, want: %v, got %v",
			expToLocalScript, toLocalScript)
	}

	// Next, compute the to-local witness stack returned by the justice kit.
	toLocalWitnessStack, err := justiceKit.CommitToLocalRevokeWitnessStack()
	if err != nil {
		t.Fatalf("unable to compute to-local witness stack: %v", err)
	}

	// A valid witness that spends the revocation path should have exactly
	// two elements on the stack.
	if len(toLocalWitnessStack) != 2 {
		t.Fatalf("to-local witness stack should be of length 2, is %d",
			len(toLocalWitnessStack))
	}

	// First, we'll verify that the top element is 0x01, which triggers the
	// revocation path within the to-local witness script.
	if !bytes.Equal(toLocalWitnessStack[1], []byte{0x01}) {
		t.Fatalf("top item on witness stack should be 0x01, found: %v",
			toLocalWitnessStack[1])
	}

	// Next, compute the expected signature in the bottom element of the
	// stack, by appending a sighash all flag to the raw DER signature.
	rawRevSigWithSigHash := append(
		rawRevSig.Serialize(), byte(txscript.SigHashAll),
	)

	// Assert that the second element on the stack matches our expected
	// signature under the revocation pubkey.
	if !bytes.Equal(rawRevSigWithSigHash, toLocalWitnessStack[0]) {
		t.Fatalf("mismatched sig in to-local witness stack, want: %v, "+
			"got: %v", rawRevSigWithSigHash, toLocalWitnessStack[0])
	}
}
