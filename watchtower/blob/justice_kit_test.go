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

var descriptorTests = []struct {
	name                 string
	encVersion           uint16
	decVersion           uint16
	revPubKey            blob.PubKey
	delayPubKey          blob.PubKey
	csvDelay             uint32
	commitToLocalSig     lnwire.Sig
	hasCommitToRemote    bool
	commitToRemotePubKey blob.PubKey
	commitToRemoteSig    lnwire.Sig
	encErr               error
	decErr               error
}{
	{
		name:             "to-local only",
		encVersion:       0,
		decVersion:       0,
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
	},
	{
		name:                 "to-local and p2wkh",
		encVersion:           0,
		decVersion:           0,
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
		revPubKey:        makePubKey(0),
		delayPubKey:      makePubKey(1),
		csvDelay:         144,
		commitToLocalSig: makeSig(1),
		decErr:           blob.ErrUnknownBlobVersion,
	},
}

// TestBlobJusticeKitEncryptDecrypt asserts that encrypting and decrypting a
// plaintext blob produces the original. The tests include negative assertions
// when passed invalid combinations, and that all successfully encrypted blobs
// are of constant size.
func TestBlobJusticeKitEncryptDecrypt(t *testing.T) {
	for i, test := range descriptorTests {
		boj := &blob.JusticeKit{
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
			t.Fatalf("test #%d %s -- unable to generate blob "+
				"encryption key: %v", i, test.name, err)
		}

		nonce := make([]byte, blob.NonceSize)
		_, err = io.ReadFull(rand.Reader, nonce)
		if err != nil {
			t.Fatalf("test #%d %s -- unable to generate nonce "+
				"nonce: %v", i, test.name, err)
		}

		// Encrypt the blob plaintext using the generated key and
		// target version for this test.
		ctxt, err := boj.Encrypt(nonce, key, test.encVersion)
		if err != test.encErr {
			t.Fatalf("test #%d %s -- unable to encrypt blob: %v",
				i, test.name, err)
		} else if test.encErr != nil {
			// If the test expected an encryption failure, we can
			// continue to the next test.
			continue
		}

		// Ensure that all encrypted blobs are padded out to the same
		// size: 282 bytes for version 0.
		if len(ctxt) != blob.Size(test.encVersion) {
			t.Fatalf("test #%d %s -- expected blob to have "+
				"size %d, got %d instead", i, test.name,
				blob.Size(test.encVersion), len(ctxt))

		}

		// Decrypt the encrypted blob, reconstructing the original
		// blob plaintext from the decrypted contents. We use the target
		// decryption version specified by this test case.
		boj2, err := blob.Decrypt(nonce, key, ctxt, test.decVersion)
		if err != test.decErr {
			t.Fatalf("test #%d %s -- unable to decrypt blob: %v",
				i, test.name, err)
		} else if test.decErr != nil {
			// If the test expected an decryption failure, we can
			// continue to the next test.
			continue
		}

		// Check that the decrypted blob properly reports whether it has
		// a to-remote output or not.
		if boj2.HasCommitToRemoteOutput() != test.hasCommitToRemote {
			t.Fatalf("test #%d %s -- expected blob has_to_remote "+
				"to be %v, got %v", i, test.name,
				test.hasCommitToRemote,
				boj2.HasCommitToRemoteOutput())
		}

		// Check that the original blob plaintext matches the
		// one reconstructed from the encrypted blob.
		if !reflect.DeepEqual(boj, boj2) {
			t.Fatalf("test #%d %s -- decrypted plaintext does not "+
				"match original, want: %v, got %v",
				i, test.name, boj, boj2)
		}
	}
}
