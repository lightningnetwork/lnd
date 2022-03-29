package blob

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// NonceSize is the length of a chacha20poly1305 nonce, 24 bytes.
	NonceSize = chacha20poly1305.NonceSizeX

	// KeySize is the length of a chacha20poly1305 key, 32 bytes.
	KeySize = chacha20poly1305.KeySize

	// CiphertextExpansion is the number of bytes padded to a plaintext
	// encrypted with chacha20poly1305, which comes from a 16-byte MAC.
	CiphertextExpansion = 16

	// V0PlaintextSize is the plaintext size of a version 0 encoded blob.
	//    sweep address length:            1 byte
	//    padded sweep address:           42 bytes
	//    revocation pubkey:              33 bytes
	//    local delay pubkey:             33 bytes
	//    csv delay:                       4 bytes
	//    commit to-local revocation sig: 64 bytes
	//    commit to-remote pubkey:        33 bytes, maybe blank
	//    commit to-remote sig:           64 bytes, maybe blank
	V0PlaintextSize = 274

	// MaxSweepAddrSize defines the maximum sweep address size that can be
	// encoded in a blob.
	MaxSweepAddrSize = 42
)

// Size returns the size of the encoded-and-encrypted blob in bytes.
//   nonce:                24 bytes
//   enciphered plaintext:  n bytes
//   MAC:                  16 bytes
func Size(blobType Type) int {
	return NonceSize + PlaintextSize(blobType) + CiphertextExpansion
}

// PlaintextSize returns the size of the encoded-but-unencrypted blob in bytes.
func PlaintextSize(blobType Type) int {
	switch {
	case blobType.Has(FlagCommitOutputs):
		return V0PlaintextSize
	default:
		return 0
	}
}

var (
	// byteOrder specifies a big-endian encoding of all integer values.
	byteOrder = binary.BigEndian

	// ErrUnknownBlobType signals that we don't understand the requested
	// blob encoding scheme.
	ErrUnknownBlobType = errors.New("unknown blob type")

	// ErrCiphertextTooSmall is a decryption error signaling that the
	// ciphertext is smaller than the ciphertext expansion factor.
	ErrCiphertextTooSmall = errors.New(
		"ciphertext is too small for chacha20poly1305",
	)

	// ErrNoCommitToRemoteOutput is returned when trying to retrieve the
	// commit to-remote output from the blob, though none exists.
	ErrNoCommitToRemoteOutput = errors.New(
		"cannot obtain commit to-remote p2wkh output script from blob",
	)

	// ErrSweepAddressToLong is returned when trying to encode or decode a
	// sweep address with length greater than the maximum length of 42
	// bytes, which supports p2wkh and p2sh addresses.
	ErrSweepAddressToLong = fmt.Errorf(
		"sweep address must be less than or equal to %d bytes long",
		MaxSweepAddrSize,
	)
)

// PubKey is a 33-byte, serialized compressed public key.
type PubKey [33]byte

// JusticeKit is lé Blob of Justice. The JusticeKit contains information
// required to construct a justice transaction, that sweeps a remote party's
// revoked commitment transaction. It supports encryption and decryption using
// chacha20poly1305, allowing the client to encrypt the contents of the blob,
// and for a watchtower to later decrypt if action must be taken. The encoding
// format is versioned to allow future extensions.
type JusticeKit struct {
	// BlobType encodes a bitfield that inform the tower of various features
	// requested by the client when resolving a breach. Examples include
	// whether the justice transaction contains a reward for the tower, or
	// whether the channel is a legacy or anchor channel.
	//
	// NOTE: This value is not serialized in the encrypted payload. It is
	// stored separately and added to the JusticeKit after decryption.
	BlobType Type

	// SweepAddress is the witness program of the output where the client's
	// fund will be deposited. This value is included in the blobs, as
	// opposed to the session info, such that the sweep addresses can't be
	// correlated across sessions and/or towers.
	//
	// NOTE: This is chosen to be the length of a maximally sized witness
	// program.
	SweepAddress []byte

	// RevocationPubKey is the compressed pubkey that guards the revocation
	// clause of the remote party's to-local output.
	RevocationPubKey PubKey

	// LocalDelayPubKey is the compressed pubkey in the to-local script of
	// the remote party, which guards the path where the remote party
	// claims their commitment output.
	LocalDelayPubKey PubKey

	// CSVDelay is the relative timelock in the remote party's to-local
	// output, which the remote party must wait out before sweeping their
	// commitment output.
	CSVDelay uint32

	// CommitToLocalSig is a signature under RevocationPubKey using
	// SIGHASH_ALL.
	CommitToLocalSig lnwire.Sig

	// CommitToRemotePubKey is the public key in the to-remote output of the revoked
	// commitment transaction.
	//
	// NOTE: This value is only used if it contains a valid compressed
	// public key.
	CommitToRemotePubKey PubKey

	// CommitToRemoteSig is a signature under CommitToRemotePubKey using SIGHASH_ALL.
	//
	// NOTE: This value is only used if CommitToRemotePubKey contains a valid
	// compressed public key.
	CommitToRemoteSig lnwire.Sig
}

// CommitToLocalWitnessScript returns the serialized witness script for the
// commitment to-local output.
func (b *JusticeKit) CommitToLocalWitnessScript() ([]byte, error) {
	revocationPubKey, err := btcec.ParsePubKey(
		b.RevocationPubKey[:],
	)
	if err != nil {
		return nil, err
	}

	localDelayedPubKey, err := btcec.ParsePubKey(
		b.LocalDelayPubKey[:],
	)
	if err != nil {
		return nil, err
	}

	return input.CommitScriptToSelf(
		b.CSVDelay, localDelayedPubKey, revocationPubKey,
	)
}

// CommitToLocalRevokeWitnessStack constructs a witness stack spending the
// revocation clause of the commitment to-local output.
//   <revocation-sig> 1
func (b *JusticeKit) CommitToLocalRevokeWitnessStack() ([][]byte, error) {
	toLocalSig, err := b.CommitToLocalSig.ToSignature()
	if err != nil {
		return nil, err
	}

	witnessStack := make([][]byte, 2)
	witnessStack[0] = append(toLocalSig.Serialize(),
		byte(txscript.SigHashAll))
	witnessStack[1] = []byte{1}

	return witnessStack, nil
}

// HasCommitToRemoteOutput returns true if the blob contains a to-remote p2wkh
// pubkey.
func (b *JusticeKit) HasCommitToRemoteOutput() bool {
	return btcec.IsCompressedPubKey(b.CommitToRemotePubKey[:])
}

// CommitToRemoteWitnessScript returns the witness script for the commitment
// to-remote output given the blob type. The script returned will either be for
// a p2wpkh to-remote output or an p2wsh anchor to-remote output which includes
// a CSV delay.
func (b *JusticeKit) CommitToRemoteWitnessScript() ([]byte, error) {
	if !btcec.IsCompressedPubKey(b.CommitToRemotePubKey[:]) {
		return nil, ErrNoCommitToRemoteOutput
	}

	// If this is a blob for an anchor channel, we'll return the p2wsh
	// output containing a CSV delay of 1.
	if b.BlobType.IsAnchorChannel() {
		pk, err := btcec.ParsePubKey(b.CommitToRemotePubKey[:])
		if err != nil {
			return nil, err
		}

		return input.CommitScriptToRemoteConfirmed(pk)
	}

	return b.CommitToRemotePubKey[:], nil
}

// CommitToRemoteWitnessStack returns a witness stack spending the commitment
// to-remote output, which consists of a single signature satisfying either the
// legacy or anchor witness scripts.
//   <to-remote-sig>
func (b *JusticeKit) CommitToRemoteWitnessStack() ([][]byte, error) {
	toRemoteSig, err := b.CommitToRemoteSig.ToSignature()
	if err != nil {
		return nil, err
	}

	witnessStack := make([][]byte, 1)
	witnessStack[0] = append(toRemoteSig.Serialize(),
		byte(txscript.SigHashAll))

	return witnessStack, nil
}

// Encrypt encodes the blob of justice using encoding version, and then
// creates a ciphertext using chacha20poly1305 under the chosen (nonce, key)
// pair.
//
// NOTE: It is the caller's responsibility to ensure that this method is only
// called once for a given (nonce, key) pair.
func (b *JusticeKit) Encrypt(key BreachKey) ([]byte, error) {
	// Encode the plaintext using the provided version, to obtain the
	// plaintext bytes.
	var ptxtBuf bytes.Buffer
	err := b.encode(&ptxtBuf, b.BlobType)
	if err != nil {
		return nil, err
	}

	// Create a new chacha20poly1305 cipher, using a 32-byte key.
	cipher, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}

	// Allocate the ciphertext, which will contain the nonce, encrypted
	// plaintext and MAC.
	plaintext := ptxtBuf.Bytes()
	ciphertext := make([]byte, Size(b.BlobType))

	// Generate a random  24-byte nonce in the ciphertext's prefix.
	nonce := ciphertext[:NonceSize]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// Finally, encrypt the plaintext using the given nonce, storing the
	// result in the ciphertext buffer.
	cipher.Seal(ciphertext[NonceSize:NonceSize], nonce, plaintext, nil)

	return ciphertext, nil
}

// Decrypt unenciphers a blob of justice by decrypting the ciphertext using
// chacha20poly1305 with the chosen (nonce, key) pair. The internal plaintext is
// then deserialized using the given encoding version.
func Decrypt(key BreachKey, ciphertext []byte,
	blobType Type) (*JusticeKit, error) {

	// Fail if the blob's overall length is less than required for the nonce
	// and expansion factor.
	if len(ciphertext) < NonceSize+CiphertextExpansion {
		return nil, ErrCiphertextTooSmall
	}

	// Create a new chacha20poly1305 cipher, using a 32-byte key.
	cipher, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}

	// Allocate the final buffer that will contain the blob's plaintext
	// bytes, which is computed by subtracting the ciphertext expansion
	// factor from the blob's length.
	plaintext := make([]byte, len(ciphertext)-CiphertextExpansion)

	// Decrypt the ciphertext, placing the resulting plaintext in our
	// plaintext buffer.
	nonce := ciphertext[:NonceSize]
	_, err = cipher.Open(plaintext[:0], nonce, ciphertext[NonceSize:], nil)
	if err != nil {
		return nil, err
	}

	// If decryption succeeded, we will then decode the plaintext bytes
	// using the specified blob version.
	boj := &JusticeKit{
		BlobType: blobType,
	}
	err = boj.decode(bytes.NewReader(plaintext), blobType)
	if err != nil {
		return nil, err
	}

	return boj, nil
}

// encode serializes the JusticeKit according to the version, returning an
// error if the version is unknown.
func (b *JusticeKit) encode(w io.Writer, blobType Type) error {
	switch {
	case blobType.Has(FlagCommitOutputs):
		return b.encodeV0(w)
	default:
		return ErrUnknownBlobType
	}
}

// decode deserializes the JusticeKit according to the version, returning an
// error if the version is unknown.
func (b *JusticeKit) decode(r io.Reader, blobType Type) error {
	switch {
	case blobType.Has(FlagCommitOutputs):
		return b.decodeV0(r)
	default:
		return ErrUnknownBlobType
	}
}

// encodeV0 encodes the JusticeKit using the version 0 encoding scheme to the
// provided io.Writer. The encoding supports sweeping of the commit to-local
// output, and  optionally the  commit to-remote output. The encoding produces a
// constant-size plaintext size of 274 bytes.
//
// blob version 0 plaintext encoding:
//    sweep address length:            1 byte
//    padded sweep address:           42 bytes
//    revocation pubkey:              33 bytes
//    local delay pubkey:             33 bytes
//    csv delay:                       4 bytes
//    commit to-local revocation sig: 64 bytes
//    commit to-remote pubkey:        33 bytes, maybe blank
//    commit to-remote sig:           64 bytes, maybe blank
func (b *JusticeKit) encodeV0(w io.Writer) error {
	// Assert the sweep address length is sane.
	if len(b.SweepAddress) > MaxSweepAddrSize {
		return ErrSweepAddressToLong
	}

	// Write the actual length of the sweep address as a single byte.
	err := binary.Write(w, byteOrder, uint8(len(b.SweepAddress)))
	if err != nil {
		return err
	}

	// Pad the sweep address to our maximum length of 42 bytes.
	var sweepAddressBuf [MaxSweepAddrSize]byte
	copy(sweepAddressBuf[:], b.SweepAddress)

	// Write padded 42-byte sweep address.
	_, err = w.Write(sweepAddressBuf[:])
	if err != nil {
		return err
	}

	// Write 33-byte revocation public key.
	_, err = w.Write(b.RevocationPubKey[:])
	if err != nil {
		return err
	}

	// Write 33-byte local delay public key.
	_, err = w.Write(b.LocalDelayPubKey[:])
	if err != nil {
		return err
	}

	// Write 4-byte CSV delay.
	err = binary.Write(w, byteOrder, b.CSVDelay)
	if err != nil {
		return err
	}

	// Write 64-byte revocation signature for commit to-local output.
	_, err = w.Write(b.CommitToLocalSig[:])
	if err != nil {
		return err
	}

	// Write 33-byte commit to-remote public key, which may be blank.
	_, err = w.Write(b.CommitToRemotePubKey[:])
	if err != nil {
		return err
	}

	// Write 64-byte commit to-remote signature, which may be blank.
	_, err = w.Write(b.CommitToRemoteSig[:])
	return err
}

// decodeV0 reconstructs a JusticeKit from the io.Reader, using version 0
// encoding scheme. This will parse a constant size input stream of 274 bytes to
// recover information for the commit to-local output, and possibly the commit
// to-remote output.
//
// blob version 0 plaintext encoding:
//    sweep address length:            1 byte
//    padded sweep address:           42 bytes
//    revocation pubkey:              33 bytes
//    local delay pubkey:             33 bytes
//    csv delay:                       4 bytes
//    commit to-local revocation sig: 64 bytes
//    commit to-remote pubkey:        33 bytes, maybe blank
//    commit to-remote sig:           64 bytes, maybe blank
func (b *JusticeKit) decodeV0(r io.Reader) error {
	// Read the sweep address length as a single byte.
	var sweepAddrLen uint8
	err := binary.Read(r, byteOrder, &sweepAddrLen)
	if err != nil {
		return err
	}

	// Assert the sweep address length is sane.
	if sweepAddrLen > MaxSweepAddrSize {
		return ErrSweepAddressToLong
	}

	// Read padded 42-byte sweep address.
	var sweepAddressBuf [MaxSweepAddrSize]byte
	_, err = io.ReadFull(r, sweepAddressBuf[:])
	if err != nil {
		return err
	}

	// Parse sweep address from padded buffer.
	b.SweepAddress = make([]byte, sweepAddrLen)
	copy(b.SweepAddress, sweepAddressBuf[:])

	// Read 33-byte revocation public key.
	_, err = io.ReadFull(r, b.RevocationPubKey[:])
	if err != nil {
		return err
	}

	// Read 33-byte local delay public key.
	_, err = io.ReadFull(r, b.LocalDelayPubKey[:])
	if err != nil {
		return err
	}

	// Read 4-byte CSV delay.
	err = binary.Read(r, byteOrder, &b.CSVDelay)
	if err != nil {
		return err
	}

	// Read 64-byte revocation signature for commit to-local output.
	_, err = io.ReadFull(r, b.CommitToLocalSig[:])
	if err != nil {
		return err
	}

	var (
		commitToRemotePubkey PubKey
		commitToRemoteSig    lnwire.Sig
	)

	// Read 33-byte commit to-remote public key, which may be discarded.
	_, err = io.ReadFull(r, commitToRemotePubkey[:])
	if err != nil {
		return err
	}

	// Read 64-byte commit to-remote signature, which may be discarded.
	_, err = io.ReadFull(r, commitToRemoteSig[:])
	if err != nil {
		return err
	}

	// Only populate the commit to-remote fields in the decoded blob if a
	// valid compressed public key was read from the reader.
	if btcec.IsCompressedPubKey(commitToRemotePubkey[:]) {
		b.CommitToRemotePubKey = commitToRemotePubkey
		b.CommitToRemoteSig = commitToRemoteSig
	}

	return nil
}
