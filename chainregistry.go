package main

import (
	"sync"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// chainCode is an enum-like structure for keeping track of the chains currently
// supported within lnd.
type chainCode uint32

const (
	// bitcoinChain is Bitcoin's testnet chain.
	bitcoinChain chainCode = iota

	// litecoinChain is Litecoin's testnet chain.
	litecoinChain
)

// String returns a string representation of the target chainCode.
func (c chainCode) String() string {
	switch c {
	case bitcoinChain:
		return "bitcoin"
	case litecoinChain:
		return "litecoin"
	default:
		return "kekcoin"
	}
}

// chainControl couples the three primary interfaces lnd utilizes for a
// particular chain together. A single chainControl instance will exist for all
// the chains lnd is currently active on.
type chainControl struct {
	chainIO       lnwallet.BlockChainIO
	chainNotifier chainntnfs.ChainNotifier

	wallet *lnwallet.LightningWallet
}

var (
	// bitcoinGenesis is the genesis hash of Bitcoin's testnet chain.
	bitcoinGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0x6f, 0xe2, 0x8c, 0x0a, 0xb6, 0xf1, 0xb3, 0x72,
		0xc1, 0xa6, 0xa2, 0x46, 0xae, 0x63, 0xf7, 0x4f,
		0x93, 0x1e, 0x83, 0x65, 0xe1, 0x5a, 0x08, 0x9c,
		0x68, 0xd6, 0x19, 0x00, 0x00, 0x00, 0x00, 0x00,
	})

	// litecoinGenesis is the genesis hash of Litecoin's testnet4 chain.
	litecoinGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xa0, 0x29, 0x3e, 0x4e, 0xeb, 0x3d, 0xa6, 0xe6,
		0xf5, 0x6f, 0x81, 0xed, 0x59, 0x5f, 0x57, 0x88,
		0x0d, 0x1a, 0x21, 0x56, 0x9e, 0x13, 0xee, 0xfd,
		0xd9, 0x51, 0x28, 0x4b, 0x5a, 0x62, 0x66, 0x49,
	})

	// chainMap is a simple index that maps a chain's genesis hash to the
	// chainCode enum for that chain.
	chainMap = map[chainhash.Hash]chainCode{
		bitcoinGenesis:  bitcoinChain,
		litecoinGenesis: litecoinChain,
	}

	// reverseChainMap is the inverse of the chainMap above: it maps the
	// chain enum for a chain to its genesis hash.
	reverseChainMap = map[chainCode]chainhash.Hash{
		bitcoinChain:  bitcoinGenesis,
		litecoinChain: litecoinGenesis,
	}
)

// chainRegistry keeps track of the current chains
type chainRegistry struct {
	sync.RWMutex

	activeChains map[chainCode]*chainControl
	netParams    map[chainCode]*bitcoinNetParams

	primaryChain chainCode
}

// newChainRegistry creates a new chainRegistry.
func newChainRegistry() *chainRegistry {
	return &chainRegistry{
		activeChains: make(map[chainCode]*chainControl),
		netParams:    make(map[chainCode]*bitcoinNetParams),
	}
}

// RegisterChain assigns an active chainControl instance to a target chain
// identified by its chainCode.
func (c *chainRegistry) RegisterChain(newChain chainCode, cc *chainControl) {
	c.Lock()
	c.activeChains[newChain] = cc
	c.Unlock()
}

// LookupChain attempts to lookup an active chainControl instance for the
// target chain.
func (c *chainRegistry) LookupChain(targetChain chainCode) (*chainControl, bool) {
	c.RLock()
	cc, ok := c.activeChains[targetChain]
	c.RUnlock()
	return cc, ok
}

// LookupChainByHash attempts to look up an active chainControl which
// corresponds to the passed genesis hash.
func (c *chainRegistry) LookupChainByHash(chainHash chainhash.Hash) (*chainControl, bool) {
	c.RLock()
	defer c.RUnlock()

	targetChain, ok := chainMap[chainHash]
	if !ok {
		return nil, ok
	}

	cc, ok := c.activeChains[targetChain]
	return cc, ok
}

// RegisterPrimaryChain sets a target chain as the "home chain" for lnd.
func (c *chainRegistry) RegisterPrimaryChain(cc chainCode) {
	c.Lock()
	defer c.Unlock()

	c.primaryChain = cc
}

// PrimaryChain returns the primary chain for this running lnd instance. The
// primary chain is considered the "home base" while the other registered
// chains are treated as secondary chains.
func (c *chainRegistry) PrimaryChain() chainCode {
	c.RLock()
	defer c.RUnlock()

	return c.primaryChain
}

// ActiveChains returns the total number of active chains.
func (c *chainRegistry) ActiveChains() []chainCode {
	c.RLock()
	defer c.RUnlock()

	chains := make([]chainCode, 0, len(c.activeChains))
	for activeChain := range c.activeChains {
		chains = append(chains, activeChain)
	}

	return chains
}

// NumActiveChains returns the total number of active chains.
func (c *chainRegistry) NumActiveChains() uint32 {
	c.RLock()
	defer c.RUnlock()

	return uint32(len(c.activeChains))
}
