package sweep

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainnotif"
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
	confChannel map[chainhash.Hash]chan *chainnotif.TxConfirmation
	epochChan   map[chan *chainnotif.BlockEpoch]int32
	spendChan   map[wire.OutPoint][]chan *chainnotif.SpendDetail
	spends      map[wire.OutPoint]*wire.MsgTx
	mutex       sync.RWMutex
	t           *testing.T
}

// NewMockNotifier instantiates a new mock notifier.
func NewMockNotifier(t *testing.T) *MockNotifier {
	confChannel := make(map[chainhash.Hash]chan *chainnotif.TxConfirmation)
	epochChan := make(map[chan *chainnotif.BlockEpoch]int32)
	spendChan := make(map[wire.OutPoint][]chan *chainnotif.SpendDetail)
	spends := make(map[wire.OutPoint]*wire.MsgTx)
	return &MockNotifier{
		confChannel: confChannel,
		epochChan:   epochChan,
		spendChan:   spendChan,
		spends:      spends,
		t:           t,
	}
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
		case epochChan <- &chainnotif.BlockEpoch{Height: height}:
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
		case epochChan <- &chainnotif.BlockEpoch{
			Height: height,
		}:
		case <-time.After(defaultTestTimeout):
			m.t.Fatal("epoch event not consumed")
		}
	}
}

// ConfirmTx simulates a tx confirming.
func (m *MockNotifier) ConfirmTx(txid *chainhash.Hash, height uint32) error {
	confirm := &chainnotif.TxConfirmation{
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

	log.Debugf("Spending outpoint %v", outpoint)

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

func (m *MockNotifier) sendSpend(channel chan *chainnotif.SpendDetail,
	outpoint *wire.OutPoint,
	spendingTx *wire.MsgTx) {

	log.Debugf("Notifying spend of outpoint %v", outpoint)

	spenderTxHash := spendingTx.TxHash()
	channel <- &chainnotif.SpendDetail{
		SpenderTxHash: &spenderTxHash,
		SpendingTx:    spendingTx,
		SpentOutPoint: outpoint,
	}
}

// RegisterConfirmationsNtfn registers for tx confirm notifications.
func (m *MockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32,
	_ ...chainnotif.NotifierOption) (*chainnotif.ConfirmationEvent,
	error) {

	return &chainnotif.ConfirmationEvent{
		Confirmed: m.getConfChannel(txid),
	}, nil
}

func (m *MockNotifier) getConfChannel(
	txid *chainhash.Hash) chan *chainnotif.TxConfirmation {

	m.mutex.Lock()
	defer m.mutex.Unlock()

	channel, ok := m.confChannel[*txid]
	if ok {
		return channel
	}
	channel = make(chan *chainnotif.TxConfirmation)
	m.confChannel[*txid] = channel

	return channel
}

// RegisterBlockEpochNtfn registers a block notification.
func (m *MockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainnotif.BlockEpoch) (*chainnotif.BlockEpochEvent, error) {

	log.Tracef("Mock block ntfn registered")

	m.mutex.Lock()
	epochChan := make(chan *chainnotif.BlockEpoch, 1)

	// The real notifier returns a notification with the current block hash
	// and height immediately if no best block hash or height is specified
	// in the request. We want to emulate this behaviour as well for the
	// mock.
	switch {
	case bestBlock == nil:
		epochChan <- &chainnotif.BlockEpoch{
			Hash:   mockChainHash,
			Height: mockChainHeight,
		}
		m.epochChan[epochChan] = mockChainHeight
	default:
		m.epochChan[epochChan] = bestBlock.Height
	}
	m.mutex.Unlock()

	return &chainnotif.BlockEpochEvent{
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
	_ []byte, _ uint32) (*chainnotif.SpendEvent, error) {

	log.Debugf("RegisterSpendNtfn for outpoint %v", outpoint)

	// Add channel to global spend ntfn map.
	m.mutex.Lock()

	channels, ok := m.spendChan[*outpoint]
	if !ok {
		channels = make([]chan *chainnotif.SpendDetail, 0)
	}

	channel := make(chan *chainnotif.SpendDetail, 1)
	channels = append(channels, channel)
	m.spendChan[*outpoint] = channels

	// Check if this output has already been spent.
	spendingTx, spent := m.spends[*outpoint]

	m.mutex.Unlock()

	// If output has been spent already, signal now. Do this outside the
	// lock to prevent a deadlock.
	if spent {
		m.sendSpend(channel, outpoint, spendingTx)
	}

	return &chainnotif.SpendEvent{
		Spend: channel,
		Cancel: func() {
			log.Infof("Cancelling RegisterSpendNtfn for %v",
				outpoint)

			m.mutex.Lock()
			defer m.mutex.Unlock()
			channels := m.spendChan[*outpoint]
			for i, c := range channels {
				if c == channel {
					channels[i] = channels[len(channels)-1]
					m.spendChan[*outpoint] =
						channels[:len(channels)-1]
				}
			}

			close(channel)

			log.Infof("Spend ntfn channel closed for %v",
				outpoint)
		},
	}, nil
}
