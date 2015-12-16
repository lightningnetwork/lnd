package wallet

import "github.com/btcsuite/btcd/wire"
import "fmt" //TODO(j) remove me later...

// TODO(roasbeef): port Rusty's hash-chain stuff
//http://ccodearchive.net/info/crypto/shachain.html
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

func Derive(from uint64, to uint64, fromHash wire.ShaHash, toHash wire.ShaHash) {
	var branches uint64
	branches = from ^ to

	//TODO(j) remove me Dumb test
	fmt.Println(branches)
	fmt.Println(from)
	fmt.Println(to)
}

//Generate a new ShaChain from a seed
func NewShaChainFromSeed(seed wire.ShaHash, index uint64, hash wire.ShaHash) {
	Derive(0, ^uint64(0), seed, hash)
}

// NewShaChain...
func NewShaChain(seed wire.ShaHash) *ShaChain {
	// TODO(roasbeef): from/to or static size?
	return new(ShaChain)
}

// NextHash...
func (s *ShaChain) NextHash() {
}

// GetHash...
func (s *ShaChain) GetHash(index uint64) {
}
