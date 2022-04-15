package lnwire

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightningnetwork/lnd/input"
)

// Sig is a fixed-sized ECDSA signature. Unlike Bitcoin, we use fixed sized
// signatures on the wire, instead of DER encoded signatures. This type
// provides several methods to convert to/from a regular Bitcoin DER encoded
// signature (raw bytes and *ecdsa.Signature).
type Sig [64]byte

var (
	errSigTooShort = errors.New("malformed signature: too short")
	errBadLength   = errors.New("malformed signature: bad length")
	errBadRLength  = errors.New("malformed signature: bogus R length")
	errBadSLength  = errors.New("malformed signature: bogus S length")
	errRTooLong    = errors.New("R is over 32 bytes long without padding")
	errSTooLong    = errors.New("S is over 32 bytes long without padding")
)

// NewSigFromRawSignature returns a Sig from a Bitcoin raw signature encoded in
// the canonical DER encoding.
func NewSigFromRawSignature(sig []byte) (Sig, error) {
	var b Sig

	// Check the total length is above the minimal.
	if len(sig) < ecdsa.MinSigLen {
		return b, errSigTooShort
	}

	// The DER representation is laid out as:
	//   0x30 <length> 0x02 <length r> r 0x02 <length s> s
	// which means the length of R is the 4th byte and the length of S is
	// the second byte after R ends. 0x02 signifies a length-prefixed,
	// zero-padded, big-endian bigint. 0x30 signifies a DER signature.
	// See the Serialize() method for ecdsa.Signature for details.

	// Reading <length>, remaining: [0x02 <length r> r 0x02 <length s> s]
	sigLen := int(sig[1])

	// siglen should be less than the entire message and greater than
	// the minimal message size.
	if sigLen+2 > len(sig) || sigLen+2 < ecdsa.MinSigLen {
		return b, errBadLength
	}

	// Reading <length r>, remaining: [r 0x02 <length s> s]
	rLen := int(sig[3])

	// rLen must be positive and must be able to fit in other elements.
	// Assuming s is one byte, then we have 0x30, <length>, 0x20,
	// <length r>, 0x20, <length s>, s, a total of 7 bytes.
	if rLen <= 0 || rLen+7 > len(sig) {
		return b, errBadRLength
	}

	// Reading <length s>, remaining: [s]
	sLen := int(sig[5+rLen])

	// S should be the rest of the string.
	// sLen must be positive and must be able to fit in other elements.
	// We know r is rLen bytes, and we have 0x30, <length>, 0x20,
	// <length r>, 0x20, <length s>, a total of rLen+6 bytes.
	if sLen <= 0 || sLen+rLen+6 > len(sig) {
		return b, errBadSLength
	}

	// Check to make sure R and S can both fit into their intended buffers.
	// We check S first because these code blocks decrement sLen and rLen
	// in the case of a 33-byte 0-padded integer returned from Serialize()
	// and rLen is used in calculating array indices for S. We can track
	// this with additional variables, but it's more efficient to just
	// check S first.
	if sLen > 32 {
		if (sLen > 33) || (sig[6+rLen] != 0x00) {
			return b, errSTooLong
		}
		sLen--
		copy(b[64-sLen:], sig[7+rLen:])
	} else {
		copy(b[64-sLen:], sig[6+rLen:])
	}

	// Do the same for R as we did for S
	if rLen > 32 {
		if (rLen > 33) || (sig[4] != 0x00) {
			return b, errRTooLong
		}
		rLen--
		copy(b[32-rLen:], sig[5:5+rLen])
	} else {
		copy(b[32-rLen:], sig[4:4+rLen])
	}

	return b, nil
}

// NewSigFromSignature creates a new signature as used on the wire, from an
// existing ecdsa.Signature.
func NewSigFromSignature(e input.Signature) (Sig, error) {
	if e == nil {
		return Sig{}, fmt.Errorf("cannot decode empty signature")
	}

	// Nil is still a valid interface, apparently. So we need a more
	// explicit check here.
	if ecsig, ok := e.(*ecdsa.Signature); ok && ecsig == nil {
		return Sig{}, fmt.Errorf("cannot decode empty signature")
	}

	// Serialize the signature with all the checks that entails.
	return NewSigFromRawSignature(e.Serialize())
}

// ToSignature converts the fixed-sized signature to a ecdsa.Signature objects
// which can be used for signature validation checks.
func (b *Sig) ToSignature() (*ecdsa.Signature, error) {
	// Parse the signature with strict checks.
	sigBytes := b.ToSignatureBytes()
	sig, err := ecdsa.ParseDERSignature(sigBytes)
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
