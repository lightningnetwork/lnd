package blockcache

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/lightninglabs/neutrino/cache/lru"
)

type BlockCache struct {
	Cache *lru.Cache
}

func NewBlockCache(capactity uint64) *BlockCache {
	return &BlockCache{
		Cache: lru.NewCache(capactity),
	}
}

func (bc *BlockCache) GetBlock(hash *chainhash.Hash,
	getBlockImpl func(hash *chainhash.Hash) (*wire.MsgBlock,
		error)) (*wire.MsgBlock, error) {

	var block *wire.MsgBlock

	// Check if the block corresponding to the given hash is already
	// stored in the blockCache and return it if it is.
	cacheBlock, err := bc.Cache.Get(*hash)
	if err != nil && err != cache.ErrElementNotFound {
		return nil, err
	}
	if cacheBlock != nil {
		return cacheBlock.(*cache.CacheableBlock).MsgBlock(), nil
	}

	// Fetch the block from the chain backends.
	block, err = getBlockImpl(hash)
	if err != nil {
		return nil, err
	}

	// Add the new block to blockCache. If the Cache is at its maximum
	// capacity then the LFU item will be evicted in favour of this new
	// block.
	_, err = bc.Cache.Put(
		*hash, &cache.CacheableBlock{
			Block: btcutil.NewBlock(block),
		},
	)
	if err != nil {
		return nil, err
	}

	return block, nil
}
