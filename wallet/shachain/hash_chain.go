package wallet

import "github.com/btcsuite/btcd/wire"

// TODO(roasbeef): port Rusty's hash-chain stuff
//  * or just use HD chains based off of CodeShark's proposal?

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
}

// NextHash...
func (s *ShaChain) NextHash() {
}

// GetHash...
func (s *ShaChain) GetHash(index uint64) {
}
