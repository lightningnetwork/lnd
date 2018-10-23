package sweep

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"sync"
)

var (
	defaultTestTimeout = 5 * time.Second
	mockChainIOHeight  = int32(100)
)

type mockSigner struct {
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {

	return []byte{}, nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {

	return &lnwallet.InputScript{}, nil
}

// MockNotifier simulates the chain notifier for test purposes.
type MockNotifier struct {
	confChannel map[chainhash.Hash]chan *chainntnfs.TxConfirmation
	epochChan   map[chan *chainntnfs.BlockEpoch]struct{}
	spendChan   map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	spends      map[wire.OutPoint]*wire.MsgTx
	mutex       sync.RWMutex
	t           *testing.T
}

// NewMockNotifier instantiates a new mock notifier.
func NewMockNotifier(t *testing.T) *MockNotifier {
	return &MockNotifier{
		confChannel: make(map[chainhash.Hash]chan *chainntnfs.TxConfirmation),
		epochChan:   make(map[chan *chainntnfs.BlockEpoch]struct{}),
		spendChan:   make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
		spends:      make(map[wire.OutPoint]*wire.MsgTx),
		t:           t,
	}
}

// NotifyEpoch ...
func (m *MockNotifier) NotifyEpoch(height int32) {
	for epochChan := range m.epochChan {
		select {
		case epochChan <- &chainntnfs.BlockEpoch{
			Height: height,
		}:
		case <-time.After(defaultTestTimeout):
			m.t.Fatal("epoch event not consumed")
		}
	}
}

// ConfirmTx ...
func (m *MockNotifier) ConfirmTx(txid *chainhash.Hash, height uint32) error {
	confirm := &chainntnfs.TxConfirmation{
		BlockHeight: height,
	}
	select {
	case m.getConfChannel(txid) <- confirm:
	case <-time.After(defaultTestTimeout):
		return fmt.Errorf("confirmation not consumed")
	}
	return nil
}

// SpendOutpoint ...
func (m *MockNotifier) SpendOutpoint(outpoint wire.OutPoint,
	spendingTx wire.MsgTx) {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channels, ok := m.spendChan[outpoint]
	if ok {
		for _, channel := range channels {
			m.sendSpend(channel, &outpoint, &spendingTx)
		}
	}

	m.spends[outpoint] = &spendingTx
}

// SpendOutpoint ...
func (m *MockNotifier) sendSpend(channel chan *chainntnfs.SpendDetail,
	outpoint *wire.OutPoint,
	spendingTx *wire.MsgTx) {

	spenderTxHash := spendingTx.TxHash()
	channel <- &chainntnfs.SpendDetail{
		SpenderTxHash: &spenderTxHash,
		SpendingTx:    spendingTx,
		SpentOutPoint: outpoint,
	}
}

// RegisterConfirmationsNtfn ...
func (m *MockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.getConfChannel(txid),
	}, nil
}

func (m *MockNotifier) getConfChannel(
	txid *chainhash.Hash) chan *chainntnfs.TxConfirmation {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channel, ok := m.confChannel[*txid]
	if ok {
		return channel
	}
	channel = make(chan *chainntnfs.TxConfirmation)
	m.confChannel[*txid] = channel

	return channel
}

// RegisterBlockEpochNtfn registers a block notification.
func (m *MockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	log.Tracef("Mock block ntfn registered")

	m.mutex.Lock()
	epochChan := make(chan *chainntnfs.BlockEpoch)
	m.epochChan[epochChan] = struct{}{}
	m.mutex.Unlock()

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {
			log.Tracef("Mock block ntfn cancelled")
			m.mutex.Lock()
			delete(m.epochChan, epochChan)
			m.mutex.Unlock()
		},
	}, nil
}

// Start ...
func (m *MockNotifier) Start() error {
	return nil
}

// Stop ...
func (m *MockNotifier) Stop() error {
	return nil
}

// RegisterSpendNtfn ...
func (m *MockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channels, ok := m.spendChan[*outpoint]
	if !ok {
		channels = make([]chan *chainntnfs.SpendDetail, 0)
	}

	channel := make(chan *chainntnfs.SpendDetail, 1)
	channels = append(channels, channel)
	m.spendChan[*outpoint] = channels

	if spendingTx, ok := m.spends[*outpoint]; ok {
		m.sendSpend(channel, outpoint, spendingTx)
	}

	return &chainntnfs.SpendEvent{
		Spend: channel,
		Cancel: func() {
			log.Infof("Cancelling RegisterSpendNtfn for %v",
				outpoint)
		},
	}, nil
}

type mockChainIO struct{}

func (m *mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, mockChainIOHeight, nil
}

func (m *mockChainIO) GetUtxo(op *wire.OutPoint, pkScript []byte,
	heightHint uint32) (*wire.TxOut, error) {

	return nil, nil
}

func (m *mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (m *mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}
