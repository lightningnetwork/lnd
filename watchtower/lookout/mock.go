package lookout

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

type MockBackend struct {
	mu sync.RWMutex

	blocks chan *chainntnfs.BlockEpoch
	epochs map[chainhash.Hash]*wire.MsgBlock
	quit   chan struct{}
}

func NewMockBackend() *MockBackend {
	return &MockBackend{
		blocks: make(chan *chainntnfs.BlockEpoch),
		epochs: make(map[chainhash.Hash]*wire.MsgBlock),
		quit:   make(chan struct{}),
	}
}

func (m *MockBackend) RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: m.blocks,
	}, nil
}

func (m *MockBackend) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	block, ok := m.epochs[*hash]
	if !ok {
		return nil, fmt.Errorf("unknown block for hash %x", hash)
	}

	return block, nil
}

func (m *MockBackend) ConnectEpoch(epoch *chainntnfs.BlockEpoch,
	block *wire.MsgBlock) {

	m.mu.Lock()
	m.epochs[*epoch.Hash] = block
	m.mu.Unlock()

	select {
	case m.blocks <- epoch:
	case <-m.quit:
	}
}
