package sweep

import (
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
)

// mockBackend simulates a chain backend for realistic behaviour in unit tests
// around double spends.
type mockBackend struct {
	t *testing.T

	lock sync.Mutex

	notifier *MockNotifier

	confirmedSpendInputs map[wire.OutPoint]struct{}

	unconfirmedTxes        map[chainhash.Hash]*wire.MsgTx
	unconfirmedSpendInputs map[wire.OutPoint]struct{}

	publishChan chan wire.MsgTx

	walletUtxos []*lnwallet.Utxo
	utxoCnt     int
}

func newMockBackend(t *testing.T, notifier *MockNotifier) *mockBackend {
	return &mockBackend{
		t:                      t,
		notifier:               notifier,
		unconfirmedTxes:        make(map[chainhash.Hash]*wire.MsgTx),
		confirmedSpendInputs:   make(map[wire.OutPoint]struct{}),
		unconfirmedSpendInputs: make(map[wire.OutPoint]struct{}),
		publishChan:            make(chan wire.MsgTx, 2),
	}
}

func (b *mockBackend) publishTransaction(tx *wire.MsgTx) error {
	b.lock.Lock()
	defer b.lock.Unlock()

	txHash := tx.TxHash()
	if _, ok := b.unconfirmedTxes[txHash]; ok {
		// Tx already exists
		testLog.Tracef("mockBackend duplicate tx %v", tx.TxHash())
		return lnwallet.ErrDoubleSpend
	}

	for _, in := range tx.TxIn {
		if _, ok := b.unconfirmedSpendInputs[in.PreviousOutPoint]; ok {
			// Double spend
			testLog.Tracef("mockBackend double spend tx %v",
				tx.TxHash())
			return lnwallet.ErrDoubleSpend
		}

		if _, ok := b.confirmedSpendInputs[in.PreviousOutPoint]; ok {
			// Already included in block
			testLog.Tracef("mockBackend already in block tx %v",
				tx.TxHash())
			return lnwallet.ErrDoubleSpend
		}
	}

	b.unconfirmedTxes[txHash] = tx
	for _, in := range tx.TxIn {
		b.unconfirmedSpendInputs[in.PreviousOutPoint] = struct{}{}
	}

	testLog.Tracef("mockBackend publish tx %v", tx.TxHash())

	return nil
}

func (b *mockBackend) PublishTransaction(tx *wire.MsgTx, _ string) error {
	log.Tracef("Publishing tx %v", tx.TxHash())
	err := b.publishTransaction(tx)
	select {
	case b.publishChan <- *tx:
	case <-time.After(defaultTestTimeout):
		b.t.Fatalf("unexpected tx published")
	}

	return err
}

func (b *mockBackend) ListUnspentWitnessFromDefaultAccount(minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	b.lock.Lock()
	defer b.lock.Unlock()

	// Each time we list output, we increment the utxo counter, to
	// ensure we don't return the same outpoint every time.
	b.utxoCnt++

	for i := range b.walletUtxos {
		b.walletUtxos[i].OutPoint.Hash[0] = byte(b.utxoCnt)
	}

	return b.walletUtxos, nil
}

func (b *mockBackend) WithCoinSelectLock(f func() error) error {
	return f()
}

func (b *mockBackend) deleteUnconfirmed(txHash chainhash.Hash) {
	b.lock.Lock()
	defer b.lock.Unlock()

	tx, ok := b.unconfirmedTxes[txHash]
	if !ok {
		// Tx already exists
		testLog.Errorf("mockBackend delete tx not existing %v", txHash)
		return
	}

	testLog.Tracef("mockBackend delete tx %v", tx.TxHash())
	delete(b.unconfirmedTxes, txHash)
	for _, in := range tx.TxIn {
		delete(b.unconfirmedSpendInputs, in.PreviousOutPoint)
	}
}

func (b *mockBackend) mine() {
	b.lock.Lock()
	defer b.lock.Unlock()

	notifications := make(map[wire.OutPoint]*wire.MsgTx)
	for _, tx := range b.unconfirmedTxes {
		testLog.Tracef("mockBackend mining tx %v", tx.TxHash())
		for _, in := range tx.TxIn {
			b.confirmedSpendInputs[in.PreviousOutPoint] = struct{}{}
			notifications[in.PreviousOutPoint] = tx
		}
	}
	b.unconfirmedSpendInputs = make(map[wire.OutPoint]struct{})
	b.unconfirmedTxes = make(map[chainhash.Hash]*wire.MsgTx)

	for outpoint, tx := range notifications {
		testLog.Tracef("mockBackend delivering spend ntfn for %v",
			outpoint)
		b.notifier.SpendOutpoint(outpoint, *tx)
	}
}

func (b *mockBackend) isDone() bool {
	return len(b.unconfirmedTxes) == 0
}

func (b *mockBackend) RemoveDescendants(*wire.MsgTx) error {
	return nil
}

func (b *mockBackend) FetchTx(chainhash.Hash) (*wire.MsgTx, error) {
	return nil, nil
}

func (b *mockBackend) CancelRebroadcast(tx chainhash.Hash) {
}

// mockFeeEstimator implements a mock fee estimator. It closely resembles
// lnwallet.StaticFeeEstimator with the addition that fees can be changed for
// testing purposes in a thread safe manner.
//
// TODO(yy): replace it with chainfee.MockEstimator once it's merged.
type mockFeeEstimator struct {
	feePerKW chainfee.SatPerKWeight

	relayFee chainfee.SatPerKWeight

	blocksToFee map[uint32]chainfee.SatPerKWeight

	// A closure that when set is used instead of the
	// mockFeeEstimator.EstimateFeePerKW method.
	estimateFeePerKW func(numBlocks uint32) (chainfee.SatPerKWeight, error)

	lock sync.Mutex
}

func newMockFeeEstimator(feePerKW,
	relayFee chainfee.SatPerKWeight) *mockFeeEstimator {

	return &mockFeeEstimator{
		feePerKW:    feePerKW,
		relayFee:    relayFee,
		blocksToFee: make(map[uint32]chainfee.SatPerKWeight),
	}
}

func (e *mockFeeEstimator) updateFees(feePerKW,
	relayFee chainfee.SatPerKWeight) {

	e.lock.Lock()
	defer e.lock.Unlock()

	e.feePerKW = feePerKW
	e.relayFee = relayFee
}

func (e *mockFeeEstimator) EstimateFeePerKW(numBlocks uint32) (
	chainfee.SatPerKWeight, error) {

	e.lock.Lock()
	defer e.lock.Unlock()

	if e.estimateFeePerKW != nil {
		return e.estimateFeePerKW(numBlocks)
	}

	if fee, ok := e.blocksToFee[numBlocks]; ok {
		return fee, nil
	}

	return e.feePerKW, nil
}

func (e *mockFeeEstimator) RelayFeePerKW() chainfee.SatPerKWeight {
	e.lock.Lock()
	defer e.lock.Unlock()

	return e.relayFee
}

func (e *mockFeeEstimator) Start() error {
	return nil
}

func (e *mockFeeEstimator) Stop() error {
	return nil
}

var _ chainfee.Estimator = (*mockFeeEstimator)(nil)

// MockSweeperStore is a mock implementation of sweeper store. This type is
// exported, because it is currently used in nursery tests too.
type MockSweeperStore struct {
	mock.Mock
}

// NewMockSweeperStore returns a new instance.
func NewMockSweeperStore() *MockSweeperStore {
	return &MockSweeperStore{}
}

// IsOurTx determines whether a tx is published by us, based on its hash.
func (s *MockSweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	args := s.Called(hash)

	return args.Bool(0), args.Error(1)
}

// StoreTx stores a tx we are about to publish.
func (s *MockSweeperStore) StoreTx(tr *TxRecord) error {
	args := s.Called(tr)
	return args.Error(0)
}

// ListSweeps lists all the sweeps we have successfully published.
func (s *MockSweeperStore) ListSweeps() ([]chainhash.Hash, error) {
	args := s.Called()

	return args.Get(0).([]chainhash.Hash), args.Error(1)
}

// GetTx queries the database to find the tx that matches the given txid.
// Returns ErrTxNotFound if it cannot be found.
func (s *MockSweeperStore) GetTx(hash chainhash.Hash) (*TxRecord, error) {
	args := s.Called(hash)

	tr := args.Get(0)
	if tr != nil {
		return args.Get(0).(*TxRecord), args.Error(1)
	}

	return nil, args.Error(1)
}

// DeleteTx removes the given tx from db.
func (s *MockSweeperStore) DeleteTx(txid chainhash.Hash) error {
	args := s.Called(txid)

	return args.Error(0)
}

// Compile-time constraint to ensure MockSweeperStore implements SweeperStore.
var _ SweeperStore = (*MockSweeperStore)(nil)

type MockFeePreference struct {
	mock.Mock
}

// Compile-time constraint to ensure MockFeePreference implements FeePreference.
var _ FeePreference = (*MockFeePreference)(nil)

func (m *MockFeePreference) String() string {
	return "mock fee preference"
}

func (m *MockFeePreference) Estimate(estimator chainfee.Estimator,
	maxFeeRate chainfee.SatPerKWeight) (chainfee.SatPerKWeight, error) {

	args := m.Called(estimator, maxFeeRate)

	if args.Get(0) == nil {
		return 0, args.Error(1)
	}

	return args.Get(0).(chainfee.SatPerKWeight), args.Error(1)
}

type mockUtxoAggregator struct {
	mock.Mock
}

// Compile-time constraint to ensure mockUtxoAggregator implements
// UtxoAggregator.
var _ UtxoAggregator = (*mockUtxoAggregator)(nil)

// ClusterInputs takes a list of inputs and groups them into clusters.
func (m *mockUtxoAggregator) ClusterInputs(inputs pendingInputs) []InputSet {
	args := m.Called(inputs)

	return args.Get(0).([]InputSet)
}
