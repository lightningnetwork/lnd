package lnwallet

import (
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	CoinPkScript, _ = hex.DecodeString(
		"001431df1bde03c074d0cf21ea2529427e1499b8f1de",
	)
)

// mockWalletController is a mock implementation of the WalletController
// interface. It let's us mock the interaction with the bitcoin network.
type mockWalletController struct {
	RootKey               *btcec.PrivateKey
	PublishedTransactions chan *wire.MsgTx
	index                 uint32
	Utxos                 []*Utxo
}

// BackEnd returns "mock" to signify a mock wallet controller.
func (w *mockWalletController) BackEnd() string {
	return "mock"
}

// FetchInputInfo will be called to get info about the inputs to the funding
// transaction.
func (w *mockWalletController) FetchInputInfo(
	prevOut *wire.OutPoint) (*Utxo, error) {

	utxo := &Utxo{
		AddressType:   WitnessPubKey,
		Value:         10 * btcutil.SatoshiPerBitcoin,
		PkScript:      []byte("dummy"),
		Confirmations: 1,
		OutPoint:      *prevOut,
	}

	return utxo, nil
}

// ScriptForOutput returns the address, witness program and redeem script for a
// given UTXO. An error is returned if the UTXO does not belong to our wallet or
// it is not a managed pubKey address.
func (w *mockWalletController) ScriptForOutput(*wire.TxOut) (
	waddrmgr.ManagedPubKeyAddress, []byte, []byte, error) {

	return nil, nil, nil, nil
}

// ConfirmedBalance currently returns dummy values.
func (w *mockWalletController) ConfirmedBalance(int32, string) (btcutil.Amount,
	error) {

	return 0, nil
}

// NewAddress is called to get new addresses for delivery, change etc.
func (w *mockWalletController) NewAddress(AddressType, bool,
	string) (btcutil.Address, error) {

	addr, _ := btcutil.NewAddressPubKey(
		w.RootKey.PubKey().SerializeCompressed(),
		&chaincfg.MainNetParams,
	)

	return addr, nil
}

// LastUnusedAddress currently returns dummy values.
func (w *mockWalletController) LastUnusedAddress(AddressType,
	string) (btcutil.Address, error) {

	return nil, nil
}

// IsOurAddress currently returns a dummy value.
func (w *mockWalletController) IsOurAddress(btcutil.Address) bool {
	return false
}

// AddressInfo currently returns a dummy value.
func (w *mockWalletController) AddressInfo(
	btcutil.Address) (waddrmgr.ManagedAddress, error) {

	return nil, nil
}

// ListAccounts currently returns a dummy value.
func (w *mockWalletController) ListAccounts(string,
	*waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties, error) {

	return nil, nil
}

// RequiredReserve currently returns a dummy value.
func (w *mockWalletController) RequiredReserve(uint32) btcutil.Amount {
	return 0
}

// ListAddresses currently returns a dummy value.
func (w *mockWalletController) ListAddresses(string,
	bool) (AccountAddressMap, error) {

	return nil, nil
}

// ImportAccount currently returns a dummy value.
func (w *mockWalletController) ImportAccount(string, *hdkeychain.ExtendedKey,
	uint32, *waddrmgr.AddressType, bool) (*waddrmgr.AccountProperties,
	[]btcutil.Address, []btcutil.Address, error) {

	return nil, nil, nil, nil
}

// ImportPublicKey currently returns a dummy value.
func (w *mockWalletController) ImportPublicKey(*btcec.PublicKey,
	waddrmgr.AddressType) error {

	return nil
}

// ImportTaprootScript currently returns a dummy value.
func (w *mockWalletController) ImportTaprootScript(waddrmgr.KeyScope,
	*waddrmgr.Tapscript) (waddrmgr.ManagedAddress, error) {

	return nil, nil
}

// SendOutputs currently returns dummy values.
func (w *mockWalletController) SendOutputs([]*wire.TxOut,
	chainfee.SatPerKWeight, int32, string) (*wire.MsgTx, error) {

	return nil, nil
}

// CreateSimpleTx currently returns dummy values.
func (w *mockWalletController) CreateSimpleTx([]*wire.TxOut,
	chainfee.SatPerKWeight, int32, bool) (*txauthor.AuthoredTx, error) {

	return nil, nil
}

// ListUnspentWitness is called by the wallet when doing coin selection. We just
// need one unspent for the funding transaction.
func (w *mockWalletController) ListUnspentWitness(int32, int32,
	string) ([]*Utxo, error) {

	// If the mock already has a list of utxos, return it.
	if w.Utxos != nil {
		return w.Utxos, nil
	}

	// Otherwise create one to return.
	utxo := &Utxo{
		AddressType: WitnessPubKey,
		Value:       btcutil.Amount(10 * btcutil.SatoshiPerBitcoin),
		PkScript:    CoinPkScript,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: w.index,
		},
	}
	atomic.AddUint32(&w.index, 1)
	var ret []*Utxo
	ret = append(ret, utxo)

	return ret, nil
}

// ListTransactionDetails currently returns dummy values.
func (w *mockWalletController) ListTransactionDetails(int32, int32,
	string) ([]*TransactionDetail, error) {

	return nil, nil
}

// LockOutpoint currently does nothing.
func (w *mockWalletController) LockOutpoint(o wire.OutPoint) {}

// UnlockOutpoint currently does nothing.
func (w *mockWalletController) UnlockOutpoint(o wire.OutPoint) {}

// LeaseOutput returns the current time and a nil error.
func (w *mockWalletController) LeaseOutput(wtxmgr.LockID, wire.OutPoint,
	time.Duration) (time.Time, []byte, btcutil.Amount, error) {

	return time.Now(), nil, 0, nil
}

// ReleaseOutput currently does nothing.
func (w *mockWalletController) ReleaseOutput(wtxmgr.LockID,
	wire.OutPoint) error {

	return nil
}

func (w *mockWalletController) ListLeasedOutputs() ([]*base.ListLeasedOutputResult, //nolint:lll
	error) {

	return nil, nil
}

// FundPsbt currently does nothing.
func (w *mockWalletController) FundPsbt(*psbt.Packet, int32,
	chainfee.SatPerKWeight, string, *waddrmgr.KeyScope) (int32, error) {

	return 0, nil
}

// SignPsbt currently does nothing.
func (w *mockWalletController) SignPsbt(*psbt.Packet) ([]uint32, error) {
	return nil, nil
}

// FinalizePsbt currently does nothing.
func (w *mockWalletController) FinalizePsbt(_ *psbt.Packet, _ string) error {
	return nil
}

// PublishTransaction sends a transaction to the PublishedTransactions chan.
func (w *mockWalletController) PublishTransaction(tx *wire.MsgTx,
	_ string) error {

	w.PublishedTransactions <- tx
	return nil
}

// LabelTransaction currently does nothing.
func (w *mockWalletController) LabelTransaction(chainhash.Hash, string,
	bool) error {

	return nil
}

// SubscribeTransactions currently does nothing.
func (w *mockWalletController) SubscribeTransactions() (TransactionSubscription,
	error) {

	return nil, nil
}

// IsSynced currently returns dummy values.
func (w *mockWalletController) IsSynced() (bool, int64, error) {
	return true, int64(0), nil
}

// GetRecoveryInfo currently returns dummy values.
func (w *mockWalletController) GetRecoveryInfo() (bool, float64, error) {
	return true, float64(1), nil
}

// Start currently does nothing.
func (w *mockWalletController) Start() error {
	return nil
}

// Stop currently does nothing.
func (w *mockWalletController) Stop() error {
	return nil
}

func (w *mockWalletController) FetchTx(chainhash.Hash) (*wire.MsgTx, error) {
	return nil, nil
}

func (w *mockWalletController) RemoveDescendants(*wire.MsgTx) error {
	return nil
}

// mockChainNotifier is a mock implementation of the ChainNotifier interface.
type mockChainNotifier struct {
	SpendChan chan *chainntnfs.SpendDetail
	EpochChan chan *chainntnfs.BlockEpoch
	ConfChan  chan *chainntnfs.TxConfirmation
}

// RegisterConfirmationsNtfn returns a ConfirmationEvent that contains a channel
// that the tx confirmation will go over.
func (c *mockChainNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed: c.ConfChan,
		Cancel:    func() {},
	}, nil
}

// RegisterSpendNtfn returns a SpendEvent that contains a channel that the spend
// details will go over.
func (c *mockChainNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend:  c.SpendChan,
		Cancel: func() {},
	}, nil
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent that contains a channel that
// block epochs will go over.
func (c *mockChainNotifier) RegisterBlockEpochNtfn(
	blockEpoch *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent,
	error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: c.EpochChan,
		Cancel: func() {},
	}, nil
}

// Start currently returns a dummy value.
func (c *mockChainNotifier) Start() error {
	return nil
}

// Started currently returns a dummy value.
func (c *mockChainNotifier) Started() bool {
	return true
}

// Stop currently returns a dummy value.
func (c *mockChainNotifier) Stop() error {
	return nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, 0, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {

	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock,
	error) {

	return nil, nil
}
