package revocation

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math"
	"sync"

	"github.com/btcsuite/btcd/wire"
)

const (
	maxIndex = 1<<64 - 1
)

// chainFragment...
type chainBranch struct {
	index uint64
	hash  [32]byte
}

// HyperShaChain...
// * https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt
type HyperShaChain struct {
	sync.RWMutex

	lastChainIndex uint64
	numValid       uint64

	chainBranches [64]chainBranch

	lastHash wire.ShaHash
}

// NewHyperShaChain...
func NewHyperShaChain(seed *[32]byte, deriveTo uint64) (*HyperShaChain, error) {
	var shaSeed *[32]byte

	// If no seed is specified, generate a new one.
	if seed == nil {
		_, err := rand.Read(shaSeed[:])
		if err != nil {
			return nil, err
		}
	}

	// The last possible value in the chain is our starting index.
	start := uint64(maxIndex)
	stop := deriveTo

	curHash := derive(start, stop, *shaSeed)

	// TODO(roasbeef): from/to or static size?
	return &HyperShaChain{lastChainIndex: deriveTo, lastHash: curHash}, nil
}

// derive...
func derive(from, to uint64, startingHash [32]byte) [32]byte {
	nextHash := startingHash

	numBranches := from ^ to
	toDerive := uint64(math.Log2(float64(numBranches))) // uh.....
	for i := toDerive - 1; i >= 0; i-- {
		if (numBranches>>i)&1 == 1 {
			// Flip the ith bit, then hash the current state to
			// advance down the tree.
			nextHash[i/8] ^= (1 << (i % 8))
			nextHash = sha256.Sum256(nextHash[:])
		}
	}

	return nextHash
}

// getHash...
// index should be commitment #
func (h *HyperShaChain) GetHash(index uint64) (*[32]byte, error) {
	for i := uint64(0); i < h.numValid; i++ {
		/* If we can get from key to index only by resetting bits,
		 * we can derive from it => index has no bits key doesn't. */
		if !canDerive(h.chainBranches[i].index, index) {
			continue
		}

		nextHash := derive(h.chainBranches[i].index, index,
			h.chainBranches[i].hash)

		return &nextHash, nil
	}

	return nil, fmt.Errorf("unable to derive hash # %v", index)
}

// canDerive...
func canDerive(from, to uint64) bool {
	return ^from&to == 1
}

// addHash
func (h *HyperShaChain) AddNextHash(hash [32]byte) error {
	nextIdx := h.lastChainIndex + 1
	// Hashes for a remote chain must be added in order.
	if nextIdx != h.lastChainIndex+1 || nextIdx == 0 && h.numValid != 0 {
		return fmt.Errorf("shachain values must be added in order, attempted"+
			"to add index %v, chain is at %v", nextIdx, h.lastChainIndex)
	}

	i := uint64(0)
	for ; i < h.numValid; i++ {
		if canDerive(nextIdx, h.chainBranches[i].index) {
			// Ensure we can actually derive this value.
			derivation := derive(nextIdx, h.chainBranches[i].index, hash)
			if !bytes.Equal(derivation[:], h.chainBranches[i].hash[:]) {
				// TODO(roasbeef): better err message
				return fmt.Errorf("chain corruption")
			}
			break
		}
	}

	h.chainBranches[i].index = nextIdx
	copy(h.chainBranches[i].hash[:], hash[:])
	copy(h.lastHash[:], hash[:])
	h.numValid = i + 1
	h.lastChainIndex = nextIdx
	return nil
}

// CurrentHash...
func (h *HyperShaChain) CurrentHash() (*wire.ShaHash, error) {
	h.Lock()
	defer h.Unlock()
	return &h.lastHash, nil
}

// MarshallBinary...
func (h *HyperShaChain) Encode(b bytes.Buffer) error {
	return nil
}

// UnmarshallBinary...
func (h *HyperShaChain) Decode(b bytes.Buffer) error {
	return nil
}
