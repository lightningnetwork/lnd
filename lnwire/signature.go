package lnwire

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightningnetwork/lnd/input"
)

var (
	errSigTooShort = errors.New("malformed signature: too short")
	errBadLength   = errors.New("malformed signature: bad length")
	errBadRLength  = errors.New("malformed signature: bogus R length")
	errBadSLength  = errors.New("malformed signature: bogus S length")
	errRTooLong    = errors.New("R is over 32 bytes long without padding")
	errSTooLong    = errors.New("S is over 32 bytes long without padding")
)

// sigType...
type sigType uint

const (
	// sigTypeECDSA...
	sigTypeECDSA sigType = iota

	// sigTypeSchnorr...
	sigTypeSchnorr
)

// TODO(roasbef): make into interface after all?

// Sig is a fixed-sized ECDSA signature or 64-byte schnorr signature. For the
// ECDSA sig, unlike Bitcoin, we use fixed sized signatures on the wire,
// instead of DER encoded signatures. This type provides several methods to
// convert to/from a regular Bitcoin DER encoded signature (raw bytes and
// *ecdsa.Signature).
type Sig struct {
	bytes [64]byte

	sigType sigType
}

// ForceSchnorr...
func (s *Sig) ForceSchnorr() {
	s.sigType = sigTypeSchnorr
}

// RawBytes...
func (s *Sig) RawBytes() []byte {
	return s.bytes[:]
}

// Copy...
func (s *Sig) Copy() Sig {
	var sCopy Sig
	copy(sCopy.bytes[:], s.bytes[:])
	sCopy.sigType = s.sigType

	return sCopy
}

// NewSigFromWireECDSA returns a Sig instance based on an ECDSA signature
// that's already in the 64-byte format we expect.
func NewSigFromWireECDSA(sig []byte) (Sig, error) {
	if len(sig) != 64 {
		return Sig{}, fmt.Errorf("%w: %v bytes", errSigTooShort,
			len(sig))
	}

	var s Sig
	copy(s.bytes[:], sig)

	return s, nil
}

// NewSigFromECDSARawSignature returns a Sig from a Bitcoin raw signature
// encoded in the canonical DER encoding.
func NewSigFromECDSARawSignature(sig []byte) (Sig, error) {
	var b [64]byte

	// Check the total length is above the minimal.
	if len(sig) < ecdsa.MinSigLen {
		return Sig{}, errSigTooShort
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
		return Sig{}, errBadLength
	}

	// Reading <length r>, remaining: [r 0x02 <length s> s]
	rLen := int(sig[3])

	// rLen must be positive and must be able to fit in other elements.
	// Assuming s is one byte, then we have 0x30, <length>, 0x20,
	// <length r>, 0x20, <length s>, s, a total of 7 bytes.
	if rLen <= 0 || rLen+7 > len(sig) {
		return Sig{}, errBadRLength
	}

	// Reading <length s>, remaining: [s]
	sLen := int(sig[5+rLen])

	// S should be the rest of the string.
	// sLen must be positive and must be able to fit in other elements.
	// We know r is rLen bytes, and we have 0x30, <length>, 0x20,
	// <length r>, 0x20, <length s>, a total of rLen+6 bytes.
	if sLen <= 0 || sLen+rLen+6 > len(sig) {
		return Sig{}, errBadSLength
	}

	// Check to make sure R and S can both fit into their intended buffers.
	// We check S first because these code blocks decrement sLen and rLen
	// in the case of a 33-byte 0-padded integer returned from Serialize()
	// and rLen is used in calculating array indices for S. We can track
	// this with additional variables, but it's more efficient to just
	// check S first.
	if sLen > 32 {
		if (sLen > 33) || (sig[6+rLen] != 0x00) {
			return Sig{}, errSTooLong
		}
		sLen--
		copy(b[64-sLen:], sig[7+rLen:])
	} else {
		copy(b[64-sLen:], sig[6+rLen:])
	}

	// Do the same for R as we did for S
	if rLen > 32 {
		if (rLen > 33) || (sig[4] != 0x00) {
			return Sig{}, errRTooLong
		}
		rLen--
		copy(b[32-rLen:], sig[5:5+rLen])
	} else {
		copy(b[32-rLen:], sig[4:4+rLen])
	}

	return Sig{
		bytes:   b,
		sigType: sigTypeECDSA,
	}, nil
}

// NewSigFromSchnorrRawSignature converts a raw schnorr signature into an
// lnwire.Sig.
func NewSigFromSchnorrRawSignature(sig []byte) (Sig, error) {
	var s Sig
	copy(s.bytes[:], sig)
	s.sigType = sigTypeSchnorr

	return s, nil
}

// NewSigFromSignature creates a new signature as used on the wire, from an
// existing ecdsa.Signature or schnorr.Signature.
func NewSigFromSignature(e input.Signature) (Sig, error) {
	if e == nil {
		return Sig{}, fmt.Errorf("cannot decode empty signature")
	}

	// Nil is still a valid interface, apparently. So we need a more
	// explicit check here.
	if ecsig, ok := e.(*ecdsa.Signature); ok && ecsig == nil {
		return Sig{}, fmt.Errorf("cannot decode empty signature")
	}

	switch ecSig := e.(type) {
	// If this is a schnorr signature, then we can just pack it as normal,
	// since the default encoding is already 64 bytes.
	case *schnorr.Signature:
		var sigBytes [64]byte
		copy(sigBytes[:], e.Serialize())

		return Sig{
			bytes:   sigBytes,
			sigType: sigTypeSchnorr,
		}, nil

	// For ECDSA signatures, we'll need to do a bit more work to map the
	// signature into a compact 64 byte form.
	case *ecdsa.Signature:
		// Serialize the signature with all the checks that entails.
		return NewSigFromECDSARawSignature(e.Serialize())

	default:
		return Sig{}, fmt.Errorf("unknown wire sig type: %T", ecSig)
	}
}

// ToSignature converts the fixed-sized signature to a input.Signature which
// can be used for signature validation checks.
func (b *Sig) ToSignature() (input.Signature, error) {
	switch b.sigType {
	case sigTypeSchnorr:
		return schnorr.ParseSignature(b.bytes[:])

	case sigTypeECDSA:
		// Parse the signature with strict checks.
		sigBytes := b.ToSignatureBytes()
		sig, err := ecdsa.ParseDERSignature(sigBytes)
		if err != nil {
			return nil, err
		}

		return sig, nil

	default:
		return nil, fmt.Errorf("unknown sig type: %v", b.sigType)
	}
}

// ToSignatureBytes serializes the target fixed-sized signature into the
// encoding of the primary domain for the signature. For ECDSA signatures, this
// is the raw bytes of a DER encoding.
func (b *Sig) ToSignatureBytes() []byte {
	switch b.sigType {
	// For ECDSA signatures, we'll convert to DER encoding.
	case sigTypeECDSA:
		// Extract canonically-padded bigint representations from buffer
		r := extractCanonicalPadding(b.bytes[0:32])
		s := extractCanonicalPadding(b.bytes[32:64])
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

	// For schnorr signatures, we can use the same internal 64 bytes.
	case sigTypeSchnorr:
		// We'll make a copy of the signature so we don't return a
		// refrence into the raw slice.
		var sig [64]byte
		copy(sig[:], b.bytes[:])
		return sig[:]

	default:
		// TODO(roasbeef): can only be called via public methods so
		// never reachable?
		panic("sig type not set")
	}
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
