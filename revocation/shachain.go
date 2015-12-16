package revocation

import (
	"sync"

	"github.com/btcsuite/btcd/wire"
)

// chainFragment...
type chainFragment struct {
	index uint64
	hash  wire.ShaHash
}

// HyperShaChain...
// * https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt
type HyperShaChain struct {
	sync.RWMutex

	lastChainIndex uint64

	chainFragments []chainFragment
}

// NewHyperShaChain...
func NewHyperShaChain(seed wire.ShaHash) *HyperShaChain {
	// TODO(roasbeef): from/to or static size?
	return nil
}

// NextHash...
func (s *HyperShaChain) NextHash() {
}

// GetHash...
func (s *HyperShaChain) GetHash(index uint64) {
}
