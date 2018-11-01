package blob_test

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"reflect"
	"testing"

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
	encVersion           uint16
	decVersion           uint16
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

var descriptorTests = []descriptorTest{
	{
		name:             "to-local only",
		encVersion:       0,
		decVersion:       0,
		sweepAddr:        makeAddr(22),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:                 "to-local and p2wkh",
		encVersion:           0,
		decVersion:           0,
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
		encVersion:       1,
		decVersion:       0,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		encErr:           blob.ErrUnknownBlobVersion,
	},
	{
		name:             "unknown decrypt version",
		encVersion:       0,
		decVersion:       1,
		sweepAddr:        makeAddr(34),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		decErr:           blob.ErrUnknownBlobVersion,
	},
	{
		name:             "sweep addr length zero",
		encVersion:       0,
		decVersion:       0,
		sweepAddr:        makeAddr(0),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr max size",
		encVersion:       0,
		decVersion:       0,
		sweepAddr:        makeAddr(blob.MaxSweepAddrSize),
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:             "sweep addr too long",
		encVersion:       0,
		decVersion:       0,
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

	nonce := make([]byte, blob.NonceSize)
	_, err = io.ReadFull(rand.Reader, nonce)
	if err != nil {
		t.Fatalf("unable to generate nonce nonce: %v", err)
	}

	// Encrypt the blob plaintext using the generated key and
	// target version for this test.
	ctxt, err := boj.Encrypt(nonce, key, test.encVersion)
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
	boj2, err := blob.Decrypt(nonce, key, ctxt, test.decVersion)
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
