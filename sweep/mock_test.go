package sweep

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
)

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
func (s *MockSweeperStore) IsOurTx(hash chainhash.Hash) bool {
	args := s.Called(hash)

	return args.Bool(0)
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
func (m *mockUtxoAggregator) ClusterInputs(inputs InputsMap) []InputSet {
	args := m.Called(inputs)

	return args.Get(0).([]InputSet)
}

// MockWallet is a mock implementation of the Wallet interface.
type MockWallet struct {
	mock.Mock
}

// Compile-time constraint to ensure MockWallet implements Wallet.
var _ Wallet = (*MockWallet)(nil)

// BackEnd returns a name for the wallet's backing chain service, which could
// be e.g. btcd, bitcoind, neutrino, or another consensus service.
func (m *MockWallet) BackEnd() string {
	args := m.Called()

	return args.String(0)
}

// CheckMempoolAcceptance checks if the transaction can be accepted to the
// mempool.
func (m *MockWallet) CheckMempoolAcceptance(tx *wire.MsgTx) error {
	args := m.Called(tx)

	return args.Error(0)
}

// PublishTransaction performs cursory validation (dust checks, etc) and
// broadcasts the passed transaction to the Bitcoin network.
func (m *MockWallet) PublishTransaction(tx *wire.MsgTx, label string) error {
	args := m.Called(tx, label)

	return args.Error(0)
}

// ListUnspentWitnessFromDefaultAccount returns all unspent outputs which are
// version 0 witness programs from the default wallet account.  The 'minConfs'
// and 'maxConfs' parameters indicate the minimum and maximum number of
// confirmations an output needs in order to be returned by this method.
func (m *MockWallet) ListUnspentWitnessFromDefaultAccount(
	minConfs, maxConfs int32) ([]*lnwallet.Utxo, error) {

	args := m.Called(minConfs, maxConfs)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*lnwallet.Utxo), args.Error(1)
}

// WithCoinSelectLock will execute the passed function closure in a
// synchronized manner preventing any coin selection operations from proceeding
// while the closure is executing. This can be seen as the ability to execute a
// function closure under an exclusive coin selection lock.
func (m *MockWallet) WithCoinSelectLock(f func() error) error {
	m.Called(f)

	return f()
}

// RemoveDescendants removes any wallet transactions that spends
// outputs created by the specified transaction.
func (m *MockWallet) RemoveDescendants(tx *wire.MsgTx) error {
	args := m.Called(tx)

	return args.Error(0)
}

// FetchTx returns the transaction that corresponds to the transaction
// hash passed in. If the transaction can't be found then a nil
// transaction pointer is returned.
func (m *MockWallet) FetchTx(txid chainhash.Hash) (*wire.MsgTx, error) {
	args := m.Called(txid)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*wire.MsgTx), args.Error(1)
}

// CancelRebroadcast is used to inform the rebroadcaster sub-system
// that it no longer needs to try to rebroadcast a transaction. This is
// used to ensure that invalid transactions (inputs spent) aren't
// retried in the background.
func (m *MockWallet) CancelRebroadcast(tx chainhash.Hash) {
	m.Called(tx)
}

// GetTransactionDetails returns a detailed description of a tx given its
// transaction hash.
func (m *MockWallet) GetTransactionDetails(txHash *chainhash.Hash) (
	*lnwallet.TransactionDetail, error) {

	args := m.Called(txHash)

	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*lnwallet.TransactionDetail), args.Error(1)
}

// MockInputSet is a mock implementation of the InputSet interface.
type MockInputSet struct {
	mock.Mock
}

// Compile-time constraint to ensure MockInputSet implements InputSet.
var _ InputSet = (*MockInputSet)(nil)

// Inputs returns the set of inputs that should be used to create a tx.
func (m *MockInputSet) Inputs() []input.Input {
	args := m.Called()

	if args.Get(0) == nil {
		return nil
	}

	return args.Get(0).([]input.Input)
}

// FeeRate returns the fee rate that should be used for the tx.
func (m *MockInputSet) FeeRate() chainfee.SatPerKWeight {
	args := m.Called()

	return args.Get(0).(chainfee.SatPerKWeight)
}

// AddWalletInputs adds wallet inputs to the set until a non-dust
// change output can be made. Return an error if there are not enough
// wallet inputs.
func (m *MockInputSet) AddWalletInputs(wallet Wallet) error {
	args := m.Called(wallet)

	return args.Error(0)
}

// NeedWalletInput returns true if the input set needs more wallet
// inputs.
func (m *MockInputSet) NeedWalletInput() bool {
	args := m.Called()

	return args.Bool(0)
}

// DeadlineHeight returns the deadline height for the set.
func (m *MockInputSet) DeadlineHeight() int32 {
	args := m.Called()

	return args.Get(0).(int32)
}

// Budget givens the total amount that can be used as fees by this input set.
func (m *MockInputSet) Budget() btcutil.Amount {
	args := m.Called()

	return args.Get(0).(btcutil.Amount)
}

// StartingFeeRate returns the max starting fee rate found in the inputs.
func (m *MockInputSet) StartingFeeRate() fn.Option[chainfee.SatPerKWeight] {
	args := m.Called()

	return args.Get(0).(fn.Option[chainfee.SatPerKWeight])
}

// Immediate returns whether the inputs should be swept immediately.
func (m *MockInputSet) Immediate() bool {
	args := m.Called()

	return args.Bool(0)
}

// MockBumper is a mock implementation of the interface Bumper.
type MockBumper struct {
	mock.Mock
}

// Compile-time constraint to ensure MockBumper implements Bumper.
var _ Bumper = (*MockBumper)(nil)

// Broadcast broadcasts the transaction to the network.
func (m *MockBumper) Broadcast(req *BumpRequest) <-chan *BumpResult {
	args := m.Called(req)

	if args.Get(0) == nil {
		return nil
	}

	return args.Get(0).(chan *BumpResult)
}

// MockFeeFunction is a mock implementation of the FeeFunction interface.
type MockFeeFunction struct {
	mock.Mock
}

// Compile-time constraint to ensure MockFeeFunction implements FeeFunction.
var _ FeeFunction = (*MockFeeFunction)(nil)

// FeeRate returns the current fee rate calculated by the fee function.
func (m *MockFeeFunction) FeeRate() chainfee.SatPerKWeight {
	args := m.Called()

	return args.Get(0).(chainfee.SatPerKWeight)
}

// Increment adds one delta to the current fee rate.
func (m *MockFeeFunction) Increment() (bool, error) {
	args := m.Called()

	return args.Bool(0), args.Error(1)
}

// IncreaseFeeRate increases the fee rate by one step.
func (m *MockFeeFunction) IncreaseFeeRate(confTarget uint32) (bool, error) {
	args := m.Called(confTarget)

	return args.Bool(0), args.Error(1)
}

type MockAuxSweeper struct {
	mock.Mock
}

// DeriveSweepAddr takes a set of inputs, and the change address we'd
// use to sweep them, and maybe results an extra sweep output that we
// should add to the sweeping transaction.
func (m *MockAuxSweeper) DeriveSweepAddr(_ []input.Input,
	_ lnwallet.AddrWithKey) fn.Result[SweepOutput] {

	return fn.Ok(SweepOutput{
		TxOut: wire.TxOut{
			Value:    123,
			PkScript: changePkScript.DeliveryAddress,
		},
		IsExtra:     true,
		InternalKey: fn.None[keychain.KeyDescriptor](),
	})
}

// ExtraBudgetForInputs is used to determine the extra budget that
// should be allocated to sweep the given set of inputs. This can be
// used to add extra funds to the sweep transaction, for example to
// cover fees for additional outputs of custom channels.
func (m *MockAuxSweeper) ExtraBudgetForInputs(
	_ []input.Input) fn.Result[btcutil.Amount] {

	args := m.Called()
	amt := args.Get(0)

	return amt.(fn.Result[btcutil.Amount])
}

// NotifyBroadcast is used to notify external callers of the broadcast
// of a sweep transaction, generated by the passed BumpRequest.
func (*MockAuxSweeper) NotifyBroadcast(_ *BumpRequest, _ *wire.MsgTx,
	_ btcutil.Amount, _ map[wire.OutPoint]int) error {

	return nil
}
