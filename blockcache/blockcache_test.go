package blockcache

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/cache"
	"github.com/stretchr/testify/require"
)

type mockChainBackend struct {
	blocks         map[chainhash.Hash]*wire.MsgBlock
	chainCallCount int

	sync.RWMutex
}

func newMockChain() *mockChainBackend {
	return &mockChainBackend{
		blocks: make(map[chainhash.Hash]*wire.MsgBlock),
	}
}

// GetBlock is a mock implementation of block fetching that tracks the number
// of backend calls and returns the block found for the given hash or an error.
func (m *mockChainBackend) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	m.Lock()
	defer m.Unlock()

	m.chainCallCount++

	block, ok := m.blocks[*blockHash]
	if !ok {
		return nil, fmt.Errorf("block not found")
	}

	return block, nil
}

func (m *mockChainBackend) getChainCallCount() int {
	m.RLock()
	defer m.RUnlock()

	return m.chainCallCount
}

func (m *mockChainBackend) addBlock(block *wire.MsgBlock, nonce uint32) {
	m.Lock()
	defer m.Unlock()

	block.Header.Nonce = nonce
	hash := block.Header.BlockHash()
	m.blocks[hash] = block
}

func (m *mockChainBackend) resetChainCallCount() {
	m.Lock()
	defer m.Unlock()

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

	blockhash1 := block1.BlockHash()
	blockhash2 := block2.BlockHash()
	blockhash3 := block3.BlockHash()

	inv1 := wire.NewInvVect(wire.InvTypeWitnessBlock, &blockhash1)
	inv2 := wire.NewInvVect(wire.InvTypeWitnessBlock, &blockhash2)
	inv3 := wire.NewInvVect(wire.InvTypeWitnessBlock, &blockhash3)

	// Determine the size of one of the blocks.
	sz, _ := (&neutrino.CacheableBlock{
		Block: btcutil.NewBlock(block1),
	}).Size()

	// A new Cache is set up with a capacity of 2 blocks
	bc := NewBlockCache(2 * sz)

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
	require.Equal(t, 1, mc.getChainCallCount())
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(*inv1)
	require.NoError(t, err)

	// After calling getBlock for block2, it is expected that the Cache
	// will have a size of 2 and will contain both block1 and block2.
	// One chain backends call is expected to fetch the block.
	_, err = bc.GetBlock(&blockhash2, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 1, mc.getChainCallCount())
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(*inv1)
	require.NoError(t, err)

	_, err = bc.Cache.Get(*inv2)
	require.NoError(t, err)

	// getBlock is called again for block1 to make block2 the LFU block.
	// No call to the chain backend is expected since block 1 is already
	// in the Cache.
	_, err = bc.GetBlock(&blockhash1, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 0, mc.getChainCallCount())
	mc.resetChainCallCount()

	// Since the Cache is now at its max capacity, it is expected that when
	// getBlock is called for a new block then the LFU block will be
	// evicted. It is expected that block2 will be evicted. After calling
	// Getblock for block3, it is expected that the Cache will have a
	// length of 2 and will contain block 1 and 3.
	_, err = bc.GetBlock(&blockhash3, getBlockImpl)
	require.NoError(t, err)
	require.Equal(t, 2, bc.Cache.Len())
	require.Equal(t, 1, mc.getChainCallCount())
	mc.resetChainCallCount()

	_, err = bc.Cache.Get(*inv1)
	require.NoError(t, err)

	_, err = bc.Cache.Get(*inv2)
	require.True(t, errors.Is(err, cache.ErrElementNotFound))

	_, err = bc.Cache.Get(*inv3)
	require.NoError(t, err)
}

// TestBlockCacheMutexes is used to test that concurrent calls to GetBlock with
// the same block hash does not result in multiple calls to the chain backend.
// In other words this tests the HashMutex.
func TestBlockCacheMutexes(t *testing.T) {
	mc := newMockChain()
	getBlockImpl := mc.GetBlock

	block1 := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 1}}
	block2 := &wire.MsgBlock{Header: wire.BlockHeader{Nonce: 2}}

	blockhash1 := block1.BlockHash()
	blockhash2 := block2.BlockHash()

	// Determine the size of the block.
	sz, _ := (&neutrino.CacheableBlock{
		Block: btcutil.NewBlock(block1),
	}).Size()

	// A new Cache is set up with a capacity of 2 blocks
	bc := NewBlockCache(2 * sz)

	mc.addBlock(&wire.MsgBlock{}, 1)
	mc.addBlock(&wire.MsgBlock{}, 2)

	// Spin off multiple go routines and ensure that concurrent calls to the
	// GetBlock method does not result in multiple calls to the chain
	// backend.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(e int) {
			if e%2 == 0 {
				_, err := bc.GetBlock(&blockhash1, getBlockImpl)
				require.NoError(t, err)
			} else {
				_, err := bc.GetBlock(&blockhash2, getBlockImpl)
				require.NoError(t, err)
			}

			wg.Done()
		}(i)
	}

	wg.Wait()
	require.Equal(t, 2, mc.getChainCallCount())
}
