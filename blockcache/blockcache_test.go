package blockcache

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/stretchr/testify/require"
)

type mockChainBackend struct {
	blocks         map[chainhash.Hash]*wire.MsgBlock
	chainCallCount int

	sync.RWMutex
}

func (m *mockChainBackend) addBlock(block *wire.MsgBlock, nonce uint32) {
	m.Lock()
	block.Header.Nonce = nonce
	hash := block.Header.BlockHash()
	m.blocks[hash] = block
	m.Unlock()
}
func (m *mockChainBackend) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	m.RLock()
	defer m.RUnlock()
	m.chainCallCount++

	block, ok := m.blocks[*blockHash]
	if !ok {
		return nil, fmt.Errorf("block not found")
	}

	return block, nil
}

func newMockChain() *mockChainBackend {
	return &mockChainBackend{
		blocks: make(map[chainhash.Hash]*wire.MsgBlock),
	}
}

func (m *mockChainBackend) resetChainCallCount() {
	m.RLock()
	defer m.RUnlock()

	m.chainCallCount = 0
}

// TestBlockCacheGetBlock tests that the block Cache works correctly as a LFU block
// Cache for the given max capacity.
func TestBlockCacheGetBlock(t *testing.T) {
	mc := newMockChain()
	getBlockImpl := mc.GetBlock

	block1 := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 1}}
	block2 := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 2}}
	block3 := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 3}}

	// Determine the size of one of the blocks.
	sz, _ := (&cache.CacheableBlock{Block: btcutil.NewBlock(block1)}).Size()

	// A new Cache is set up with a capacity of 2 blocks
	bc := NewBlockCache(2 * sz)

	blockhash1 := block1.BlockHash()
	blockhash2 := block2.BlockHash()
	blockhash3 := block3.BlockHash()

	mc.addBlock(&wire.MsgBlock{}, 1)
	mc.addBlock(&wire.MsgBlock{}, 2)
	mc.addBlock(&wire.MsgBlock{}, 3)

	// We expect the initial Cache to be empty
	require.Equal(t, 0, bc.Cache.Len())

	// After calling getBlock for block1, it is expected that the Cache
	// will have a size of 1 and will contain block1. One chain backends
	// call is expected to fetch the block.
	_, err := bc.GetBlock(&blockhash1, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 1, bc.Cache.Len())
	require.Equal(t, 1, mc.chainCallCount)
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(blockhash1)
	require.NoError(t, err)

	// After calling getBlock for block2, it is expected that the Cache
	// will have a size of 2 and will contain both block1 and block2.
	// One chain backends call is expected to fetch the block.
	_, err = bc.GetBlock(&blockhash2, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 1, mc.chainCallCount)
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(blockhash1)
	require.NoError(t, err)

	_, err = bc.Cache.Get(blockhash2)
	require.NoError(t, err)

	// getBlock is called again for block1 to make block2 the LFU block.
	// No call to the chain backend is expected since block 1 is already
	// in the Cache.
	_, err = bc.GetBlock(&blockhash1, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 0, mc.chainCallCount)
	mc.resetChainCallCount()

	// Since the Cache is now at its max capacity, it is expected that when
	// getBlock is called for a new block then the LFU block will be
	// evicted. It is expected that block2 will be evicted. After calling
	// Getblock for block3, it is expected that the Cache will have a
	// length of 2 and will contain block 1 and 3.
	_, err = bc.GetBlock(&blockhash3, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 1, mc.chainCallCount)
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(blockhash1)
	require.NoError(t, err)

	_, err = bc.Cache.Get(blockhash2)
	require.True(t, errors.Is(err, cache.ErrElementNotFound))

	_, err = bc.Cache.Get(blockhash3)
	require.NoError(t, err)
}
