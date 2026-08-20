package sweep

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

var (
	defaultTestTimeout = 5 * time.Second
	processingDelay    = 1 * time.Second
	mockChainHash, _   = chainhash.NewHashFromStr("00aabbccddeeff")
	mockChainHeight    = int32(100)
)

// MockNotifier simulates the chain notifier for test purposes. This type is
// exported because it is used in nursery tests.
type MockNotifier struct {
	confChannel         map[chainhash.Hash]chan *chainntnfs.TxConfirmation
	negativeConfChannel map[chainhash.Hash]chan int32
	epochChan           map[chan *chainntnfs.BlockEpoch]int32
	spendChan           map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	spendReorgChan      map[wire.OutPoint][]chan struct{}
	spends              map[wire.OutPoint]*wire.MsgTx
	spendHeights        map[wire.OutPoint]int32
	mutex               sync.RWMutex
	t                   *testing.T
}

// NewMockNotifier instantiates a new mock notifier.
func NewMockNotifier(t *testing.T) *MockNotifier {
	m := &MockNotifier{
		confChannel: make(map[chainhash.Hash]chan *chainntnfs.TxConfirmation),
		epochChan:   make(map[chan *chainntnfs.BlockEpoch]int32),
		spendChan:   make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
		spends:      make(map[wire.OutPoint]*wire.MsgTx),
		t:           t,
	}
	m.negativeConfChannel = make(map[chainhash.Hash]chan int32)
	m.spendReorgChan = make(map[wire.OutPoint][]chan struct{})
	m.spendHeights = make(map[wire.OutPoint]int32)

	return m
}

// NotifyEpochNonBlocking simulates a new epoch arriving without blocking when
// the epochChan is not read.
func (m *MockNotifier) NotifyEpochNonBlocking(height int32) {
	m.t.Helper()

	for epochChan, chanHeight := range m.epochChan {
		// Only send notifications if the height is greater than the
		// height the caller passed into the register call.
		if chanHeight >= height {
			continue
		}

		log.Debugf("Notifying height %v to listener", height)

		select {
		case epochChan <- &chainntnfs.BlockEpoch{Height: height}:
		default:
		}
	}
}

// NotifyEpoch simulates a new epoch arriving.
func (m *MockNotifier) NotifyEpoch(height int32) {
	m.t.Helper()

	for epochChan, chanHeight := range m.epochChan {
		// Only send notifications if the height is greater than the
		// height the caller passed into the register call.
		if chanHeight >= height {
			continue
		}

		log.Debugf("Notifying height %v to listener", height)

		select {
		case epochChan <- &chainntnfs.BlockEpoch{
			Height: height,
		}:
		case <-time.After(defaultTestTimeout):
			m.t.Fatal("epoch event not consumed")
		}
	}
}

// ConfirmTx simulates a tx confirming.
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

// SpendOutpoint simulates a utxo being spent.
func (m *MockNotifier) SpendOutpoint(outpoint wire.OutPoint,
	spendingTx wire.MsgTx) {
	m.SpendOutpointAtHeight(outpoint, spendingTx, mockChainHeight)
}

// SpendOutpointAtHeight simulates a utxo being spent at the given height.
func (m *MockNotifier) SpendOutpointAtHeight(outpoint wire.OutPoint,
	spendingTx wire.MsgTx, height int32) {

	log.Debugf("Spending outpoint %v", outpoint)

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channels, ok := m.spendChan[outpoint]
	if ok {
		for _, channel := range channels {
			m.sendSpend(channel, &outpoint, &spendingTx, height)
		}
	}

	m.spends[outpoint] = &spendingTx
	m.spendHeights[outpoint] = height
}

// sendSpend delivers a spend detail to a registered test subscriber.
func (m *MockNotifier) sendSpend(channel chan *chainntnfs.SpendDetail,
	outpoint *wire.OutPoint,
	spendingTx *wire.MsgTx, height int32) {

	log.Debugf("Notifying spend of outpoint %v", outpoint)

	spenderTxHash := spendingTx.TxHash()
	channel <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &spenderTxHash,
		SpendingTx:     spendingTx,
		SpentOutPoint:  outpoint,
		SpendingHeight: height,
	}
}

// ReorgOutpoint simulates removal of an outpoint's spending transaction.
func (m *MockNotifier) ReorgOutpoint(outpoint wire.OutPoint) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, reorg := range m.spendReorgChan[outpoint] {
		select {
		case reorg <- struct{}{}:
		default:
		}
	}
	delete(m.spends, outpoint)
	delete(m.spendHeights, outpoint)
}

// RegisterConfirmationsNtfn registers for tx confirm notifications.
func (m *MockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32,
	opt ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent, error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed:    m.getConfChannel(txid),
		NegativeConf: m.getNegativeConfChannel(txid),
		Cancel:       func() {},
	}, nil
}

// getConfChannel returns the channel for confirmation notifications.
func (m *MockNotifier) getConfChannel(
	txid *chainhash.Hash) chan *chainntnfs.TxConfirmation {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channel, ok := m.confChannel[*txid]
	if ok {
		return channel
	}
	channel = make(chan *chainntnfs.TxConfirmation, 1)
	m.confChannel[*txid] = channel

	return channel
}

// getNegativeConfChannel returns the channel for negative confirmations.
func (m *MockNotifier) getNegativeConfChannel(
	txid *chainhash.Hash) chan int32 {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channel, ok := m.negativeConfChannel[*txid]
	if ok {
		return channel
	}
	channel = make(chan int32, 1)
	m.negativeConfChannel[*txid] = channel

	return channel
}

// RegisterBlockEpochNtfn registers a block notification.
func (m *MockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	log.Tracef("Mock block ntfn registered")

	m.mutex.Lock()
	epochChan := make(chan *chainntnfs.BlockEpoch, 1)

	// The real notifier returns a notification with the current block hash
	// and height immediately if no best block hash or height is specified
	// in the request. We want to emulate this behaviour as well for the
	// mock.
	switch {
	case bestBlock == nil:
		epochChan <- &chainntnfs.BlockEpoch{
			Hash:   mockChainHash,
			Height: mockChainHeight,
		}
		m.epochChan[epochChan] = mockChainHeight
	default:
		m.epochChan[epochChan] = bestBlock.Height
	}
	m.mutex.Unlock()

	return &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {
			log.Tracef("Mock block ntfn canceled")
			m.mutex.Lock()
			delete(m.epochChan, epochChan)
			m.mutex.Unlock()
		},
	}, nil
}

// Start the notifier.
func (m *MockNotifier) Start() error {
	return nil
}

// Started checks if started.
func (m *MockNotifier) Started() bool {
	return true
}

// Stop the notifier.
func (m *MockNotifier) Stop() error {
	return nil
}

// RegisterSpendNtfn registers for spend notifications.
func (m *MockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	log.Debugf("RegisterSpendNtfn for outpoint %v", outpoint)

	// Add channel to global spend ntfn map.
	m.mutex.Lock()

	channels, ok := m.spendChan[*outpoint]
	if !ok {
		channels = make([]chan *chainntnfs.SpendDetail, 0)
	}

	channel := make(chan *chainntnfs.SpendDetail, 1)
	reorgChannel := make(chan struct{}, 1)
	channels = append(channels, channel)
	m.spendChan[*outpoint] = channels
	m.spendReorgChan[*outpoint] = append(
		m.spendReorgChan[*outpoint], reorgChannel,
	)

	// Check if this output has already been spent.
	spendingTx, spent := m.spends[*outpoint]
	spendingHeight := m.spendHeights[*outpoint]

	m.mutex.Unlock()

	// If output has been spent already, signal now. Do this outside the
	// lock to prevent a deadlock.
	if spent {
		m.sendSpend(channel, outpoint, spendingTx, spendingHeight)
	}

	return &chainntnfs.SpendEvent{
		Spend: channel,
		Reorg: reorgChannel,
		Cancel: sync.OnceFunc(func() {
			log.Infof("Cancelling RegisterSpendNtfn for %v",
				outpoint)

			m.mutex.Lock()
			defer m.mutex.Unlock()
			channels := m.spendChan[*outpoint]
			reorgChannels := m.spendReorgChan[*outpoint]
			for i, c := range channels {
				if c == channel {
					last := len(reorgChannels) - 1
					channels[i] = channels[last]
					reorgChannels[i] = reorgChannels[last]
					m.spendChan[*outpoint] =
						channels[:len(channels)-1]
					m.spendReorgChan[*outpoint] =
						reorgChannels[:last]

					break
				}
			}

			close(channel)
			close(reorgChannel)

			log.Infof("Spend ntfn channel closed for %v",
				outpoint)
		}),
	}, nil
}
