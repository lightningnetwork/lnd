package blob

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

	// V1PlaintextSize is the plaintext size of a version 1 encoded blob.
	//    sweep address length:            1 byte
	//    padded sweep address:           42 bytes
	//    revocation pubkey:              32 bytes
	//    local delay pubkey:             32 bytes
	//    commit to-local revocation sig: 64 bytes
	//    hash of to-local delay script:  32 bytes
	//    commit to-remote pubkey:        33 bytes, maybe blank
	//    commit to-remote sig:           64 bytes, maybe blank
	V1PlaintextSize = 300

	// MaxSweepAddrSize defines the maximum sweep address size that can be
	// encoded in a blob.
	MaxSweepAddrSize = 42
)

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

// Size returns the size of the encoded-and-encrypted blob in bytes.
//
//	nonce:                24 bytes
//	enciphered plaintext:  n bytes
//	MAC:                  16 bytes
func Size(kit JusticeKit) int {
	return NonceSize + kit.PlainTextSize() + CiphertextExpansion
}

// schnorrPubKey is a 32-byte serialized x-only public key.
type schnorrPubKey [32]byte

// toBlobSchnorrPubKey serializes the given public key into a schnorrPubKey that
// can be set as a field on a JusticeKit.
func toBlobSchnorrPubKey(pubKey *btcec.PublicKey) schnorrPubKey {
	var blobPubKey schnorrPubKey
	copy(blobPubKey[:], schnorr.SerializePubKey(pubKey))
	return blobPubKey
}

// pubKey is a 33-byte, serialized compressed public key.
type pubKey [33]byte

// toBlobPubKey serializes the given public key into a pubKey that can be set
// as a field on a JusticeKit.
func toBlobPubKey(pk *btcec.PublicKey) pubKey {
	var blobPubKey pubKey
	copy(blobPubKey[:], pk.SerializeCompressed())
	return blobPubKey
}

// justiceKitPacketV0 is lé Blob of Justice. The JusticeKit contains information
// required to construct a justice transaction, that sweeps a remote party's
// revoked commitment transaction. It supports encryption and decryption using
// chacha20poly1305, allowing the client to encrypt the contents of the blob,
// and for a watchtower to later decrypt if action must be taken.
type justiceKitPacketV0 struct {
	// sweepAddress is the witness program of the output where the client's
	// fund will be deposited. This value is included in the blobs, as
	// opposed to the session info, such that the sweep addresses can't be
	// correlated across sessions and/or towers.
	//
	// NOTE: This is chosen to be the length of a maximally sized witness
	// program.
	sweepAddress []byte

	// revocationPubKey is the compressed pubkey that guards the revocation
	// clause of the remote party's to-local output.
	revocationPubKey pubKey

	// localDelayPubKey is the compressed pubkey in the to-local script of
	// the remote party, which guards the path where the remote party
	// claims their commitment output.
	localDelayPubKey pubKey

	// csvDelay is the relative timelock in the remote party's to-local
	// output, which the remote party must wait out before sweeping their
	// commitment output.
	csvDelay uint32

	// commitToLocalSig is a signature under RevocationPubKey using
	// SIGHASH_ALL.
	commitToLocalSig lnwire.Sig

	// commitToRemotePubKey is the public key in the to-remote output of the
	// revoked commitment transaction.
	//
	// NOTE: This value is only used if it contains a valid compressed
	// public key.
	commitToRemotePubKey pubKey

	// commitToRemoteSig is a signature under CommitToRemotePubKey using
	// SIGHASH_ALL.
	//
	// NOTE: This value is only used if CommitToRemotePubKey contains a
	// valid compressed public key.
	commitToRemoteSig lnwire.Sig
}

// Encrypt encodes the blob of justice using encoding version, and then
// creates a ciphertext using chacha20poly1305 under the chosen (nonce, key)
// pair.
//
// NOTE: It is the caller's responsibility to ensure that this method is only
// called once for a given (nonce, key) pair.
func Encrypt(kit JusticeKit, key BreachKey) ([]byte, error) {
	// Encode the plaintext using the provided version, to obtain the
	// plaintext bytes.
	var ptxtBuf bytes.Buffer
	err := kit.encode(&ptxtBuf)
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
	ciphertext := make([]byte, Size(kit))

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
	blobType Type) (JusticeKit, error) {

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

	commitment, err := blobType.CommitmentType(nil)
	if err != nil {
		return nil, err
	}

	kit, err := commitment.EmptyJusticeKit()
	if err != nil {
		return nil, err
	}

	// If decryption succeeded, we will then decode the plaintext bytes
	// using the specified blob version.
	err = kit.decode(bytes.NewReader(plaintext))
	if err != nil {
		return nil, err
	}

	return kit, nil
}

// encode encodes the JusticeKit using the version 0 encoding scheme to the
// provided io.Writer. The encoding supports sweeping of the commit to-local
// output, and  optionally the  commit to-remote output. The encoding produces a
// constant-size plaintext size of 274 bytes.
//
// blob version 0 plaintext encoding:
//
//	sweep address length:            1 byte
//	padded sweep address:           42 bytes
//	revocation pubkey:              33 bytes
//	local delay pubkey:             33 bytes
//	csv delay:                       4 bytes
//	commit to-local revocation sig: 64 bytes
//	commit to-remote pubkey:        33 bytes, maybe blank
//	commit to-remote sig:           64 bytes, maybe blank
func (b *justiceKitPacketV0) encode(w io.Writer) error {
	// Assert the sweep address length is sane.
	if len(b.sweepAddress) > MaxSweepAddrSize {
		return ErrSweepAddressToLong
	}

	// Write the actual length of the sweep address as a single byte.
	err := binary.Write(w, byteOrder, uint8(len(b.sweepAddress)))
	if err != nil {
		return err
	}

	// Pad the sweep address to our maximum length of 42 bytes.
	var sweepAddressBuf [MaxSweepAddrSize]byte
	copy(sweepAddressBuf[:], b.sweepAddress)

	// Write padded 42-byte sweep address.
	_, err = w.Write(sweepAddressBuf[:])
	if err != nil {
		return err
	}

	// Write 33-byte revocation public key.
	_, err = w.Write(b.revocationPubKey[:])
	if err != nil {
		return err
	}

	// Write 33-byte local delay public key.
	_, err = w.Write(b.localDelayPubKey[:])
	if err != nil {
		return err
	}

	// Write 4-byte CSV delay.
	err = binary.Write(w, byteOrder, b.csvDelay)
	if err != nil {
		return err
	}

	// Write 64-byte revocation signature for commit to-local output.
	_, err = w.Write(b.commitToLocalSig.RawBytes())
	if err != nil {
		return err
	}

	// Write 33-byte commit to-remote public key, which may be blank.
	_, err = w.Write(b.commitToRemotePubKey[:])
	if err != nil {
		return err
	}

	// Write 64-byte commit to-remote signature, which may be blank.
	_, err = w.Write(b.commitToRemoteSig.RawBytes())

	return err
}

// decode reconstructs a JusticeKit from the io.Reader, using version 0
// encoding scheme. This will parse a constant size input stream of 274 bytes to
// recover information for the commit to-local output, and possibly the commit
// to-remote output.
//
// blob version 0 plaintext encoding:
//
//	sweep address length:            1 byte
//	padded sweep address:           42 bytes
//	revocation pubkey:              33 bytes
//	local delay pubkey:             33 bytes
//	csv delay:                       4 bytes
//	commit to-local revocation sig: 64 bytes
//	commit to-remote pubkey:        33 bytes, maybe blank
//	commit to-remote sig:           64 bytes, maybe blank
func (b *justiceKitPacketV0) decode(r io.Reader) error {
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
	b.sweepAddress = make([]byte, sweepAddrLen)
	copy(b.sweepAddress, sweepAddressBuf[:])

	// Read 33-byte revocation public key.
	_, err = io.ReadFull(r, b.revocationPubKey[:])
	if err != nil {
		return err
	}

	// Read 33-byte local delay public key.
	_, err = io.ReadFull(r, b.localDelayPubKey[:])
	if err != nil {
		return err
	}

	// Read 4-byte CSV delay.
	err = binary.Read(r, byteOrder, &b.csvDelay)
	if err != nil {
		return err
	}

	// Read 64-byte revocation signature for commit to-local output.
	var localSig [64]byte
	_, err = io.ReadFull(r, localSig[:])
	if err != nil {
		return err
	}

	b.commitToLocalSig, err = lnwire.NewSigFromWireECDSA(localSig[:])
	if err != nil {
		return err
	}

	var (
		commitToRemotePubkey pubKey
		commitToRemoteSig    [64]byte
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
		b.commitToRemotePubKey = commitToRemotePubkey
		b.commitToRemoteSig, err = lnwire.NewSigFromWireECDSA(
			commitToRemoteSig[:],
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// justiceKitPacketV1 is the Blob of Justice for taproot channels.
type justiceKitPacketV1 struct {
	// sweepAddress is the witness program of the output where the client's
	// fund will be deposited. This value is included in the blobs, as
	// opposed to the session info, such that the sweep addresses can't be
	// correlated across sessions and/or towers.
	//
	// NOTE: This is chosen to be the length of a maximally sized witness
	// program.
	sweepAddress []byte

	// revocationPubKey is the x-only pubkey that guards the revocation
	// clause of the remote party's to-local output.
	revocationPubKey schnorrPubKey

	// localDelayPubKey is the x-only pubkey in the to-local script of
	// the remote party, which guards the path where the remote party
	// claims their commitment output.
	localDelayPubKey schnorrPubKey

	// delayScriptHash is the hash of the to_local delay script that is used
	// in the TapTree.
	delayScriptHash [chainhash.HashSize]byte

	// commitToLocalSig is a signature under revocationPubKey using
	// SIGHASH_DEFAULT.
	commitToLocalSig lnwire.Sig

	// commitToRemotePubKey is the public key in the to-remote output of the
	// revoked commitment transaction. This uses a 33-byte compressed pubkey
	// encoding unlike the other public keys because it will not always be
	// present and so this gives us an easy way to check if it is present or
	// not.
	//
	// NOTE: This value is only used if it contains a valid compressed
	// public key.
	commitToRemotePubKey pubKey

	// commitToRemoteSig is a signature under commitToRemotePubKey using
	// SIGHASH_DEFAULT.
	//
	// NOTE: This value is only used if commitToRemotePubKey contains a
	// valid compressed public key.
	commitToRemoteSig lnwire.Sig
}

// encode encodes the justiceKitPacketV1 to the provided io.Writer. The encoding
// supports sweeping of the commit to-local output, and optionally the commit
// to-remote output. The encoding produces a constant-size plaintext size of
// 300 bytes.
//
// blob version 1 plaintext encoding:
//
//	sweep address length:            1 byte
//	padded sweep address:           42 bytes
//	revocation pubkey:              32 bytes
//	local delay pubkey:             32 bytes
//	commit to-local revocation sig: 64 bytes
//	hash of to-local delay script:  32 bytes
//	commit to-remote pubkey:        33 bytes, maybe blank
//	commit to-remote sig:           64 bytes, maybe blank
func (t *justiceKitPacketV1) encode(w io.Writer) error {
	// Assert the sweep address length is sane.
	if len(t.sweepAddress) > MaxSweepAddrSize {
		return ErrSweepAddressToLong
	}

	// Write the actual length of the sweep address as a single byte.
	err := binary.Write(w, byteOrder, uint8(len(t.sweepAddress)))
	if err != nil {
		return err
	}

	// Pad the sweep address to our maximum length of 42 bytes.
	var sweepAddressBuf [MaxSweepAddrSize]byte
	copy(sweepAddressBuf[:], t.sweepAddress)

	// Write padded 42-byte sweep address.
	_, err = w.Write(sweepAddressBuf[:])
	if err != nil {
		return err
	}

	// Write 32-byte revocation public key.
	_, err = w.Write(t.revocationPubKey[:])
	if err != nil {
		return err
	}

	// Write 32-byte local delay public key.
	_, err = w.Write(t.localDelayPubKey[:])
	if err != nil {
		return err
	}

	// Write 64-byte revocation signature for commit to-local output.
	_, err = w.Write(t.commitToLocalSig.RawBytes())
	if err != nil {
		return err
	}

	// Write 32-byte hash of the to-local delay script.
	_, err = w.Write(t.delayScriptHash[:])
	if err != nil {
		return err
	}

	// Write 33-byte commit to-remote public key, which may be blank.
	_, err = w.Write(t.commitToRemotePubKey[:])
	if err != nil {
		return err
	}

	// Write 64-byte commit to-remote signature, which may be blank.
	_, err = w.Write(t.commitToRemoteSig.RawBytes())

	return err
}

// decode reconstructs a justiceKitPacketV1 from the io.Reader, using version 1
// encoding scheme. This will parse a constant size input stream of 300 bytes to
// recover information for the commit to-local output, and possibly the commit
// to-remote output.
//
// blob version 1 plaintext encoding:
//
//	sweep address length:            1 byte
//	padded sweep address:           42 bytes
//	revocation pubkey:              32 bytes
//	local delay pubkey:             32 bytes
//	commit to-local revocation sig: 64 bytes
//	hash of to-local delay script:  32 bytes
//	commit to-remote pubkey:        33 bytes, maybe blank
//	commit to-remote sig:           64 bytes, maybe blank
func (t *justiceKitPacketV1) decode(r io.Reader) error {
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
	t.sweepAddress = make([]byte, sweepAddrLen)
	copy(t.sweepAddress, sweepAddressBuf[:])

	// Read 32-byte revocation public key.
	_, err = io.ReadFull(r, t.revocationPubKey[:])
	if err != nil {
		return err
	}

	// Read 32-byte local delay public key.
	_, err = io.ReadFull(r, t.localDelayPubKey[:])
	if err != nil {
		return err
	}

	// Read 64-byte revocation signature for commit to-local output.
	var localSig [64]byte
	_, err = io.ReadFull(r, localSig[:])
	if err != nil {
		return err
	}

	// Read 32-byte to-local delay script hash.
	_, err = io.ReadFull(r, t.delayScriptHash[:])
	if err != nil {
		return err
	}

	t.commitToLocalSig, err = lnwire.NewSigFromSchnorrRawSignature(
		localSig[:],
	)
	if err != nil {
		return err
	}
	var (
		commitToRemotePubkey pubKey
		commitToRemoteSig    [64]byte
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
	if !btcec.IsCompressedPubKey(commitToRemotePubkey[:]) {
		return nil
	}

	t.commitToRemotePubKey = commitToRemotePubkey
	t.commitToRemoteSig, err = lnwire.NewSigFromSchnorrRawSignature(
		commitToRemoteSig[:],
	)
	if err != nil {
		return err
	}

	return nil
}
