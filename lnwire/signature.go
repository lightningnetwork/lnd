package lnwire

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/input"
)

// Sig is a fixed-sized ECDSA signature. Unlike Bitcoin, we use fixed sized
// signatures on the wire, instead of DER encoded signatures. This type
// provides several methods to convert to/from a regular Bitcoin DER encoded
// signature (raw bytes and *btcec.Signature).
type Sig [64]byte

// NewSigFromRawSignature returns a Sig from a Bitcoin raw signature encoded in
// the canonical DER encoding.
func NewSigFromRawSignature(sig []byte) (Sig, error) {
	var b Sig

	if len(sig) == 0 {
		return b, fmt.Errorf("cannot decode empty signature")
	}

	// Extract lengths of R and S. The DER representation is laid out as
	// 0x30 <length> 0x02 <length r> r 0x02 <length s> s
	// which means the length of R is the 4th byte and the length of S
	// is the second byte after R ends. 0x02 signifies a length-prefixed,
	// zero-padded, big-endian bigint. 0x30 signifies a DER signature.
	// See the Serialize() method for btcec.Signature for details.
	rLen := sig[3]
	sLen := sig[5+rLen]

	// Check to make sure R and S can both fit into their intended buffers.
	// We check S first because these code blocks decrement sLen and rLen
	// in the case of a 33-byte 0-padded integer returned from Serialize()
	// and rLen is used in calculating array indices for S. We can track
	// this with additional variables, but it's more efficient to just
	// check S first.
	if sLen > 32 {
		if (sLen > 33) || (sig[6+rLen] != 0x00) {
			return b, fmt.Errorf("S is over 32 bytes long " +
				"without padding")
		}
		sLen--
		copy(b[64-sLen:], sig[7+rLen:])
	} else {
		copy(b[64-sLen:], sig[6+rLen:])
	}

	// Do the same for R as we did for S
	if rLen > 32 {
		if (rLen > 33) || (sig[4] != 0x00) {
			return b, fmt.Errorf("R is over 32 bytes long " +
				"without padding")
		}
		rLen--
		copy(b[32-rLen:], sig[5:5+rLen])
	} else {
		copy(b[32-rLen:], sig[4:4+rLen])
	}

	return b, nil
}

// NewSigFromSignature creates a new signature as used on the wire, from an
// existing btcec.Signature.
func NewSigFromSignature(e input.Signature) (Sig, error) {
	if e == nil {
		return Sig{}, fmt.Errorf("cannot decode empty signature")
	}

	// Serialize the signature with all the checks that entails.
	return NewSigFromRawSignature(e.Serialize())
}

// ToSignature converts the fixed-sized signature to a btcec.Signature objects
// which can be used for signature validation checks.
func (b *Sig) ToSignature() (*btcec.Signature, error) {
	// Parse the signature with strict checks.
	sigBytes := b.ToSignatureBytes()
	sig, err := btcec.ParseDERSignature(sigBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	return sig, nil
}

// ToSignatureBytes serializes the target fixed-sized signature into the raw
// bytes of a DER encoding.
func (b *Sig) ToSignatureBytes() []byte {
	// Extract canonically-padded bigint representations from buffer
	r := extractCanonicalPadding(b[0:32])
	s := extractCanonicalPadding(b[32:64])
	rLen := uint8(len(r))
	sLen := uint8(len(s))

	// Create a canonical serialized signature. DER format is:
	// 0x30 <length> 0x02 <length r> r 0x02 <length s> s
	sigBytes := make([]byte, 6+rLen+sLen)
	sigBytes[0] = 0x30            // DER signature magic value
	sigBytes[1] = 4 + rLen + sLen // Length of rest of signature
	sigBytes[2] = 0x02            // Big integer magic value
	sigBytes[3] = rLen            // Length of R
	sigBytes[rLen+4] = 0x02       // Big integer magic value
	sigBytes[rLen+5] = sLen       // Length of S
	copy(sigBytes[4:], r)         // Copy R
	copy(sigBytes[rLen+6:], s)    // Copy S

	return sigBytes
}

// extractCanonicalPadding is a utility function to extract the canonical
// padding of a big-endian integer from the wire encoding (a 0-padded
// big-endian integer) such that it passes btcec.canonicalPadding test.
func extractCanonicalPadding(b []byte) []byte {
	for i := 0; i < len(b); i++ {
		// Found first non-zero byte.
		if b[i] > 0 {
			// If the MSB is set, we need zero padding.
			if b[i]&0x80 == 0x80 {
				return append([]byte{0x00}, b[i:]...)
			}
			return b[i:]
		}
	}
	return []byte{0x00}
}
