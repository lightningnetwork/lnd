// Copyright 2013-2022 The btcsuite developers

package musig2v040

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

const (
	// PubNonceSize is the size of the public nonces. Each public nonce is
	// serialized the full compressed encoding, which uses 32 bytes for each
	// nonce.
	PubNonceSize = 66

	// SecNonceSize is the size of the secret nonces for musig2. The secret
	// nonces are the corresponding private keys to the public nonce points.
	SecNonceSize = 64
)

var (
	// NonceAuxTag is the tag used to optionally mix in the secret key with
	// the set of aux randomness.
	NonceAuxTag = []byte("MuSig/aux")

	// NonceGenTag is used to generate the value (from a set of required an
	// optional field) that will be used as the part of the secret nonce.
	NonceGenTag = []byte("MuSig/nonce")

	byteOrder = binary.BigEndian
)

// Nonces holds the public and secret nonces required for musig2.
//
// TODO(roasbeef): methods on this to help w/ parsing, etc?
type Nonces struct {
	// PubNonce holds the two 33-byte compressed encoded points that serve
	// as the public set of nonces.
	PubNonce [PubNonceSize]byte

	// SecNonce holds the two 32-byte scalar values that are the private
	// keys to the two public nonces.
	SecNonce [SecNonceSize]byte
}

// secNonceToPubNonce takes our two secrete nonces, and produces their two
// corresponding EC points, serialized in compressed format.
func secNonceToPubNonce(secNonce [SecNonceSize]byte) [PubNonceSize]byte {
	var k1Mod, k2Mod btcec.ModNScalar
	k1Mod.SetByteSlice(secNonce[:btcec.PrivKeyBytesLen])
	k2Mod.SetByteSlice(secNonce[btcec.PrivKeyBytesLen:])

	var r1, r2 btcec.JacobianPoint
	btcec.ScalarBaseMultNonConst(&k1Mod, &r1)
	btcec.ScalarBaseMultNonConst(&k2Mod, &r2)

	// Next, we'll convert the key in jacobian format to a normal public
	// key expressed in affine coordinates.
	r1.ToAffine()
	r2.ToAffine()
	r1Pub := btcec.NewPublicKey(&r1.X, &r1.Y)
	r2Pub := btcec.NewPublicKey(&r2.X, &r2.Y)

	var pubNonce [PubNonceSize]byte

	// The public nonces are serialized as: R1 || R2, where both keys are
	// serialized in compressed format.
	copy(pubNonce[:], r1Pub.SerializeCompressed())
	copy(
		pubNonce[btcec.PubKeyBytesLenCompressed:],
		r2Pub.SerializeCompressed(),
	)

	return pubNonce
}

// NonceGenOption is a function option that allows callers to modify how nonce
// generation happens.
type NonceGenOption func(*nonceGenOpts)

// nonceGenOpts is the set of options that control how nonce generation
// happens.
type nonceGenOpts struct {
	// randReader is what we'll use to generate a set of random bytes. If
	// unspecified, then the normal crypto/rand rand.Read method will be
	// used in place.
	randReader io.Reader

	// secretKey is an optional argument that's used to further augment the
	// generated nonce by xor'ing it with this secret key.
	secretKey []byte

	// combinedKey is an optional argument that if specified, will be
	// combined along with the nonce generation.
	combinedKey []byte

	// msg is an optional argument that will be mixed into the nonce
	// derivation algorithm.
	msg []byte

	// auxInput is an optional argument that will be mixed into the nonce
	// derivation algorithm.
	auxInput []byte
}

// cryptoRandAdapter is an adapter struct that allows us to pass in the package
// level Read function from crypto/rand into a context that accepts an
// io.Reader.
type cryptoRandAdapter struct {
}

// Read implements the io.Reader interface for the crypto/rand package.  By
// default, we always use the crypto/rand reader, but the caller is able to
// specify their own generation, which can be useful for deterministic tests.
func (c *cryptoRandAdapter) Read(p []byte) (n int, err error) {
	return rand.Read(p)
}

// defaultNonceGenOpts returns the default set of nonce generation options.
func defaultNonceGenOpts() *nonceGenOpts {
	return &nonceGenOpts{
		randReader: &cryptoRandAdapter{},
	}
}

// WithCustomRand allows a caller to use a custom random number generator in
// place for crypto/rand. This should only really be used to generate
// determinstic tests.
func WithCustomRand(r io.Reader) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.randReader = r
	}
}

// WithNonceSecretKeyAux allows a caller to optionally specify a secret key
// that should be used to augment the randomness used to generate the nonces.
func WithNonceSecretKeyAux(secKey *btcec.PrivateKey) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.secretKey = secKey.Serialize()
	}
}

// WithNonceCombinedKeyAux allows a caller to optionally specify the combined
// key used in this signing session to further augment the randomness used to
// generate nonces.
func WithNonceCombinedKeyAux(combinedKey *btcec.PublicKey) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.combinedKey = schnorr.SerializePubKey(combinedKey)
	}
}

// WithNonceMessageAux allows a caller to optionally specify a message to be
// mixed into the randomness generated to create the nonce.
func WithNonceMessageAux(msg [32]byte) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.msg = msg[:]
	}
}

// WithNonceAuxInput is a set of auxiliary randomness, similar to BIP 340 that
// can be used to further augment the nonce generation process.
func WithNonceAuxInput(aux []byte) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.auxInput = aux
	}
}

// withCustomOptions allows a caller to pass a complete set of custom
// nonceGenOpts, without needing to create custom and checked structs such as
// *btcec.PrivateKey. This is mainly used to match the testcases provided by
// the MuSig2 BIP.
func withCustomOptions(customOpts nonceGenOpts) NonceGenOption {
	return func(o *nonceGenOpts) {
		o.randReader = customOpts.randReader
		o.secretKey = customOpts.secretKey
		o.combinedKey = customOpts.combinedKey
		o.msg = customOpts.msg
		o.auxInput = customOpts.auxInput
	}
}

// lengthWriter is a function closure that allows a caller to control how the
// length prefix of a byte slice is written.
type lengthWriter func(w io.Writer, b []byte) error

// uint8Writer is an implementation of lengthWriter that writes the length of
// the byte slice using 1 byte.
func uint8Writer(w io.Writer, b []byte) error {
	return binary.Write(w, byteOrder, uint8(len(b)))
}

// uint32Writer is an implementation of lengthWriter that writes the length of
// the byte slice using 4 bytes.
func uint32Writer(w io.Writer, b []byte) error {
	return binary.Write(w, byteOrder, uint32(len(b)))
}

// writeBytesPrefix is used to write out: len(b) || b, to the passed io.Writer.
// The lengthWriter function closure is used to allow the caller to specify the
// precise byte packing of the length.
func writeBytesPrefix(w io.Writer, b []byte, lenWriter lengthWriter) error {
	// Write out the length of the byte first, followed by the set of bytes
	// itself.
	if err := lenWriter(w, b); err != nil {
		return err
	}

	if _, err := w.Write(b); err != nil {
		return err
	}

	return nil
}

// genNonceAuxBytes writes out the full byte string used to derive a secret
// nonce based on some initial randomness as well as the series of optional
// fields. The byte string used for derivation is:
//   - tagged_hash("MuSig/nonce", rand || len(aggpk) || aggpk || len(m)
//     || m || len(in) || in || i).
//
// where i is the ith secret nonce being generated.
func genNonceAuxBytes(rand []byte, i int,
	opts *nonceGenOpts) (*chainhash.Hash, error) {

	var w bytes.Buffer

	// First, write out the randomness generated in the prior step.
	if _, err := w.Write(rand); err != nil {
		return nil, err
	}

	// Next, we'll write out: len(aggpk) || aggpk.
	err := writeBytesPrefix(&w, opts.combinedKey, uint8Writer)
	if err != nil {
		return nil, err
	}

	// Next, we'll write out the length prefixed message.
	err = writeBytesPrefix(&w, opts.msg, uint8Writer)
	if err != nil {
		return nil, err
	}

	// Finally we'll write out the auxiliary input.
	err = writeBytesPrefix(&w, opts.auxInput, uint32Writer)
	if err != nil {
		return nil, err
	}

	// Next we'll write out the interaction/index number which will
	// uniquely generate two nonces given the rest of the possibly static
	// parameters.
	if err := binary.Write(&w, byteOrder, uint8(i)); err != nil {
		return nil, err
	}

	// With the message buffer complete, we'll now derive the tagged hash
	// using our set of params.
	return chainhash.TaggedHash(NonceGenTag, w.Bytes()), nil
}

// GenNonces generates the secret nonces, as well as the public nonces which
// correspond to an EC point generated using the secret nonce as a private key.
func GenNonces(options ...NonceGenOption) (*Nonces, error) {
	opts := defaultNonceGenOpts()
	for _, opt := range options {
		opt(opts)
	}

	// First, we'll start out by generating 32 random bytes drawn from our
	// CSPRNG.
	var randBytes [32]byte
	if _, err := opts.randReader.Read(randBytes[:]); err != nil {
		return nil, err
	}

	// If the options contain a secret key, we XOR it with with the tagged
	// random bytes.
	if len(opts.secretKey) == 32 {
		taggedHash := chainhash.TaggedHash(NonceAuxTag, randBytes[:])

		for i := 0; i < chainhash.HashSize; i++ {
			randBytes[i] = opts.secretKey[i] ^ taggedHash[i]
		}
	}

	// Using our randomness and the set of optional params, generate our
	// two secret nonces: k1 and k2.
	k1, err := genNonceAuxBytes(randBytes[:], 0, opts)
	if err != nil {
		return nil, err
	}
	k2, err := genNonceAuxBytes(randBytes[:], 1, opts)
	if err != nil {
		return nil, err
	}

	var k1Mod, k2Mod btcec.ModNScalar
	k1Mod.SetBytes((*[32]byte)(k1))
	k2Mod.SetBytes((*[32]byte)(k2))

	// The secret nonces are serialized as the concatenation of the two 32
	// byte secret nonce values.
	var nonces Nonces
	k1Mod.PutBytesUnchecked(nonces.SecNonce[:])
	k2Mod.PutBytesUnchecked(nonces.SecNonce[btcec.PrivKeyBytesLen:])

	// Next, we'll generate R_1 = k_1*G and R_2 = k_2*G. Along the way we
	// need to map our nonce values into mod n scalars so we can work with
	// the btcec API.
	nonces.PubNonce = secNonceToPubNonce(nonces.SecNonce)

	return &nonces, nil
}

// AggregateNonces aggregates the set of a pair of public nonces for each party
// into a single aggregated nonces to be used for multi-signing.
func AggregateNonces(pubNonces [][PubNonceSize]byte) ([PubNonceSize]byte, error) {
	// combineNonces is a helper function that aggregates (adds) up a
	// series of nonces encoded in compressed format. It uses a slicing
	// function to extra 33 bytes at a time from the packed 2x public
	// nonces.
	type nonceSlicer func([PubNonceSize]byte) []byte
	combineNonces := func(slicer nonceSlicer) (btcec.JacobianPoint, error) {
		// Convert the set of nonces into jacobian coordinates we can
		// use to accumulate them all into each other.
		pubNonceJs := make([]*btcec.JacobianPoint, len(pubNonces))
		for i, pubNonceBytes := range pubNonces {
			// Using the slicer, extract just the bytes we need to
			// decode.
			var nonceJ btcec.JacobianPoint

			nonceJ, err := btcec.ParseJacobian(slicer(pubNonceBytes))
			if err != nil {
				return btcec.JacobianPoint{}, err
			}

			pubNonceJs[i] = &nonceJ
		}

		// Now that we have the set of complete nonces, we'll aggregate
		// them: R = R_i + R_i+1 + ... + R_i+n.
		var aggregateNonce btcec.JacobianPoint
		for _, pubNonceJ := range pubNonceJs {
			btcec.AddNonConst(
				&aggregateNonce, pubNonceJ, &aggregateNonce,
			)
		}

		aggregateNonce.ToAffine()
		return aggregateNonce, nil
	}

	// The final nonce public nonce is actually two nonces, one that
	// aggregate the first nonce of all the parties, and the other that
	// aggregates the second nonce of all the parties.
	var finalNonce [PubNonceSize]byte
	combinedNonce1, err := combineNonces(func(n [PubNonceSize]byte) []byte {
		return n[:btcec.PubKeyBytesLenCompressed]
	})
	if err != nil {
		return finalNonce, err
	}

	combinedNonce2, err := combineNonces(func(n [PubNonceSize]byte) []byte {
		return n[btcec.PubKeyBytesLenCompressed:]
	})
	if err != nil {
		return finalNonce, err
	}

	copy(finalNonce[:], btcec.JacobianToByteSlice(combinedNonce1))
	copy(
		finalNonce[btcec.PubKeyBytesLenCompressed:],
		btcec.JacobianToByteSlice(combinedNonce2),
	)

	return finalNonce, nil
}
