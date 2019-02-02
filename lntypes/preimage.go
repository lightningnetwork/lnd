package lntypes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// PreimageSize of array used to store preimagees.
const PreimageSize = 32

// Preimage is used in several of the lightning messages and common structures. It
// represents a payment preimage.
type Preimage [PreimageSize]byte

// String returns the Preimage as a hexadecimal string.
func (p Preimage) String() string {
	return hex.EncodeToString(p[:])
}

// NewPreimage returns a new Preimage from a byte slice.  An error is returned if
// the number of bytes passed in is not PreimageSize.
func NewPreimage(newPreimage []byte) (*Preimage, error) {
	nhlen := len(newPreimage)
	if nhlen != PreimageSize {
		return nil, fmt.Errorf("invalid preimage length of %v, want %v",
			nhlen, PreimageSize)
	}

	var preimage Preimage
	copy(preimage[:], newPreimage)

	return &preimage, nil
}

// NewPreimageFromStr creates a Preimage from a hex preimage string.
func NewPreimageFromStr(newPreimage string) (*Preimage, error) {
	// Return error if preimage string is of incorrect length.
	if len(newPreimage) != PreimageSize*2 {
		return nil, fmt.Errorf("invalid preimage string length of %v, "+
			"want %v", len(newPreimage), PreimageSize*2)
	}

	preimage, err := hex.DecodeString(newPreimage)
	if err != nil {
		return nil, err
	}

	return NewPreimage(preimage)
}

// Hash returns the sha256 hash of the preimage.
func (p *Preimage) Hash() Hash {
	return Hash(sha256.Sum256(p[:]))
}
