package lntypes

import (
	"encoding/hex"
	"fmt"
)

// HashSize of array used to store hashes.
const HashSize = 32

// Hash is used in several of the lightning messages and common structures. It
// typically represents a payment hash.
type Hash [HashSize]byte

// String returns the Hash as a hexadecimal string.
func (hash Hash) String() string {
	return hex.EncodeToString(hash[:])
}

// NewHash returns a new Hash from a byte slice.  An error is returned if
// the number of bytes passed in is not HashSize.
func NewHash(newHash []byte) (*Hash, error) {
	nhlen := len(newHash)
	if nhlen != HashSize {
		return nil, fmt.Errorf("invalid hash length of %v, want %v",
			nhlen, HashSize)
	}

	var hash Hash
	copy(hash[:], newHash)

	return &hash, nil
}

// NewHashFromStr creates a Hash from a hex hash string.
func NewHashFromStr(newHash string) (*Hash, error) {
	// Return error if hash string is of incorrect length.
	if len(newHash) != HashSize*2 {
		return nil, fmt.Errorf("invalid hash string length of %v, "+
			"want %v", len(newHash), HashSize*2)
	}

	hash, err := hex.DecodeString(newHash)
	if err != nil {
		return nil, err
	}

	return NewHash(hash)
}
