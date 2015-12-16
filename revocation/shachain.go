package revocation

import "github.com/btcsuite/btcd/wire"

// chainFragment...
type chainFragment struct {
	index uint64
	hash  wire.ShaHash
}

// ShaChain...
type ShaChain struct {
	lastChainIndex uint64

	chainFragments []chainFragment
}

// NewShaChain...
func NewShaChain(seed wire.ShaHash) *ShaChain {
	// TODO(roasbeef): from/to or static size?
	return nil
}

// NextHash...
func (s *ShaChain) NextHash() {
}

// GetHash...
func (s *ShaChain) GetHash(index uint64) {
}
