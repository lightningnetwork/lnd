package lnwire

import (
	"fmt"

	"github.com/roasbeef/btcd/btcec"
)

// serializeSigToWire serializes a *Signature to [64]byte in the format
// specified by the Lightning RFC.
func serializeSigToWire(b *[64]byte, e *btcec.Signature) error {

	// Serialize the signature with all the checks that entails.
	sig := e.Serialize()

	// Extract lengths of R and S. The DER representation is laid out as
	// 0x30 <length> 0x02 <length r> r 0x02 <length s> s
	// which means the length of R is the 4th byte and the length of S
	// is the second byte after R ends. 0x02 signifies a length-prefixed,
	// zero-padded, big-endian bigint. 0x30 sigifies a DER signature.
	// See the Serialize() method for btcec.Signature for details.
	rLen := sig[3]
	sLen := sig[5+rLen]

	// Check to make sure R and S can both fit into their intended buffers.
	// We check S first because these code blocks decrement sLen and
	// rLen in the case of a 33-byte 0-padded integer returned from
	// Serialize() and rLen is used in calculating array indices for
	// S. We can track this with additional variables, but it's more
	// efficient to just check S first.
	if sLen > 32 {
		if (sLen > 33) || (sig[6+rLen] != 0x00) {
			return fmt.Errorf("S is over 32 bytes long " +
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
			return fmt.Errorf("R is over 32 bytes long " +
				"without padding")
		}
		rLen--
		copy(b[32-rLen:], sig[5:5+rLen])
	} else {
		copy(b[32-rLen:], sig[4:4+rLen])
	}
	return nil
}

// deserializeSigFromWire deserializes a *Signature from [64]byte in the format
// specified by the Lightning RFC.
func deserializeSigFromWire(e **btcec.Signature, b [64]byte) error {

	// Extract canonically-padded bigint representations from buffer
	r := extractCanonicalPadding(b[0:32])
	s := extractCanonicalPadding(b[32:64])
	rLen := uint8(len(r))
	sLen := uint8(len(s))

	// Create a canonical serialized signature. DER format is:
	// 0x30 <length> 0x02 <length r> r 0x02 <length s> s
	sigBytes := make([]byte, 6+rLen+sLen, 6+rLen+sLen)
	sigBytes[0] = 0x30            // DER signature magic value
	sigBytes[1] = 4 + rLen + sLen // Length of rest of signature
	sigBytes[2] = 0x02            // Big integer magic value
	sigBytes[3] = rLen            // Length of R
	sigBytes[rLen+4] = 0x02       // Big integer magic value
	sigBytes[rLen+5] = sLen       // Length of S
	copy(sigBytes[4:], r)         // Copy R
	copy(sigBytes[rLen+6:], s)    // Copy S

	// Parse the signature with strict checks.
	sig, err := btcec.ParseDERSignature(sigBytes, btcec.S256())
	if err != nil {
		return err
	}
	*e = sig
	return nil
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
