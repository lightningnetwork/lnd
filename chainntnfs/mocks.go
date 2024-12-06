package chainntnfs

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
)

// MockMempoolWatcher is a mock implementation of the MempoolWatcher interface.
// This is used by other subsystems to mock the behavior of the mempool
// watcher.
type MockMempoolWatcher struct {
	mock.Mock
}

// NewMockMempoolWatcher returns a new instance of a mock mempool watcher.
func NewMockMempoolWatcher() *MockMempoolWatcher {
	return &MockMempoolWatcher{}
}

// Compile-time check to ensure MockMempoolWatcher implements MempoolWatcher.
var _ MempoolWatcher = (*MockMempoolWatcher)(nil)

// SubscribeMempoolSpent implements the MempoolWatcher interface.
func (m *MockMempoolWatcher) SubscribeMempoolSpent(
	op wire.OutPoint) (*MempoolSpendEvent, error) {

	args := m.Called(op)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MempoolSpendEvent), args.Error(1)
}

// CancelMempoolSpendEvent implements the MempoolWatcher interface.
func (m *MockMempoolWatcher) CancelMempoolSpendEvent(
	sub *MempoolSpendEvent) {

	m.Called(sub)
}

// LookupInputMempoolSpend looks up the mempool to find a spending tx which
// spends the given outpoint.
func (m *MockMempoolWatcher) LookupInputMempoolSpend(
	op wire.OutPoint) fn.Option[wire.MsgTx] {

	args := m.Called(op)

	return args.Get(0).(fn.Option[wire.MsgTx])
}

// MockNotifier is a mock implementation of the ChainNotifier interface.
type MockChainNotifier struct {
	mock.Mock
}

// Compile-time check to ensure MockChainNotifier implements ChainNotifier.
var _ ChainNotifier = (*MockChainNotifier)(nil)

// RegisterConfirmationsNtfn registers an intent to be notified once txid
// reaches numConfs confirmations.
func (m *MockChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...NotifierOption) (*ConfirmationEvent, error) {

	args := m.Called(txid, pkScript, numConfs, heightHint)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ConfirmationEvent), args.Error(1)
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint is successfully spent within a transaction.
func (m *MockChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*SpendEvent, error) {

	args := m.Called(outpoint, pkScript, heightHint)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*SpendEvent), args.Error(1)
}

// RegisterBlockEpochNtfn registers an intent to be notified of each new block
// connected to the tip of the main chain.
func (m *MockChainNotifier) RegisterBlockEpochNtfn(epoch *BlockEpoch) (
	*BlockEpochEvent, error) {

	args := m.Called(epoch)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*BlockEpochEvent), args.Error(1)
}

// Start the ChainNotifier. Once started, the implementation should be ready,
// and able to receive notification registrations from clients.
func (m *MockChainNotifier) Start() error {
	args := m.Called()

	return args.Error(0)
}

// Started returns true if this instance has been started, and false otherwise.
func (m *MockChainNotifier) Started() bool {
	args := m.Called()

	return args.Bool(0)
}

// Stops the concrete ChainNotifier.
func (m *MockChainNotifier) Stop() error {
	args := m.Called()

	return args.Error(0)
}
