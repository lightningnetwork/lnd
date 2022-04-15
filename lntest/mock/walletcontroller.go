package mock

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
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	CoinPkScript, _ = hex.DecodeString("001431df1bde03c074d0cf21ea2529427e1499b8f1de")
)

// WalletController is a mock implementation of the WalletController
// interface. It let's us mock the interaction with the bitcoin network.
type WalletController struct {
	RootKey               *btcec.PrivateKey
	PublishedTransactions chan *wire.MsgTx
	index                 uint32
	Utxos                 []*lnwallet.Utxo
}

// BackEnd returns "mock" to signify a mock wallet controller.
func (w *WalletController) BackEnd() string {
	return "mock"
}

// FetchInputInfo will be called to get info about the inputs to the funding
// transaction.
func (w *WalletController) FetchInputInfo(
	prevOut *wire.OutPoint) (*lnwallet.Utxo, error) {

	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.WitnessPubKey,
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
func (w *WalletController) ScriptForOutput(*wire.TxOut) (
	waddrmgr.ManagedPubKeyAddress, []byte, []byte, error) {

	return nil, nil, nil, nil
}

// ConfirmedBalance currently returns dummy values.
func (w *WalletController) ConfirmedBalance(int32, string) (btcutil.Amount,
	error) {

	return 0, nil
}

// NewAddress is called to get new addresses for delivery, change etc.
func (w *WalletController) NewAddress(lnwallet.AddressType, bool,
	string) (btcutil.Address, error) {

	addr, _ := btcutil.NewAddressPubKey(
		w.RootKey.PubKey().SerializeCompressed(), &chaincfg.MainNetParams,
	)
	return addr, nil
}

// LastUnusedAddress currently returns dummy values.
func (w *WalletController) LastUnusedAddress(lnwallet.AddressType,
	string) (btcutil.Address, error) {

	return nil, nil
}

// IsOurAddress currently returns a dummy value.
func (w *WalletController) IsOurAddress(btcutil.Address) bool {
	return false
}

// AddressInfo currently returns a dummy value.
func (w *WalletController) AddressInfo(
	btcutil.Address) (waddrmgr.ManagedAddress, error) {

	return nil, nil
}

// ListAccounts currently returns a dummy value.
func (w *WalletController) ListAccounts(string,
	*waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties, error) {

	return nil, nil
}

// ImportAccount currently returns a dummy value.
func (w *WalletController) ImportAccount(string, *hdkeychain.ExtendedKey,
	uint32, *waddrmgr.AddressType, bool) (*waddrmgr.AccountProperties,
	[]btcutil.Address, []btcutil.Address, error) {

	return nil, nil, nil, nil
}

// ImportPublicKey currently returns a dummy value.
func (w *WalletController) ImportPublicKey(*btcec.PublicKey,
	waddrmgr.AddressType) error {

	return nil
}

// SendOutputs currently returns dummy values.
func (w *WalletController) SendOutputs([]*wire.TxOut,
	chainfee.SatPerKWeight, int32, string) (*wire.MsgTx, error) {

	return nil, nil
}

// CreateSimpleTx currently returns dummy values.
func (w *WalletController) CreateSimpleTx([]*wire.TxOut,
	chainfee.SatPerKWeight, int32, bool) (*txauthor.AuthoredTx, error) {

	return nil, nil
}

// ListUnspentWitness is called by the wallet when doing coin selection. We just
// need one unspent for the funding transaction.
func (w *WalletController) ListUnspentWitness(int32, int32,
	string) ([]*lnwallet.Utxo, error) {

	// If the mock already has a list of utxos, return it.
	if w.Utxos != nil {
		return w.Utxos, nil
	}

	// Otherwise create one to return.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       btcutil.Amount(10 * btcutil.SatoshiPerBitcoin),
		PkScript:    CoinPkScript,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: w.index,
		},
	}
	atomic.AddUint32(&w.index, 1)
	var ret []*lnwallet.Utxo
	ret = append(ret, utxo)
	return ret, nil
}

// ListTransactionDetails currently returns dummy values.
func (w *WalletController) ListTransactionDetails(int32, int32,
	string) ([]*lnwallet.TransactionDetail, error) {

	return nil, nil
}

// LockOutpoint currently does nothing.
func (w *WalletController) LockOutpoint(o wire.OutPoint) {}

// UnlockOutpoint currently does nothing.
func (w *WalletController) UnlockOutpoint(o wire.OutPoint) {}

// LeaseOutput returns the current time and a nil error.
func (w *WalletController) LeaseOutput(wtxmgr.LockID, wire.OutPoint,
	time.Duration) (time.Time, error) {

	return time.Now(), nil
}

// ReleaseOutput currently does nothing.
func (w *WalletController) ReleaseOutput(wtxmgr.LockID, wire.OutPoint) error {
	return nil
}

func (w *WalletController) ListLeasedOutputs() ([]*wtxmgr.LockedOutput, error) {
	return nil, nil
}

// FundPsbt currently does nothing.
func (w *WalletController) FundPsbt(*psbt.Packet, int32, chainfee.SatPerKWeight,
	string) (int32, error) {

	return 0, nil
}

// SignPsbt currently does nothing.
func (w *WalletController) SignPsbt(*psbt.Packet) error {
	return nil
}

// FinalizePsbt currently does nothing.
func (w *WalletController) FinalizePsbt(_ *psbt.Packet, _ string) error {
	return nil
}

// PublishTransaction sends a transaction to the PublishedTransactions chan.
func (w *WalletController) PublishTransaction(tx *wire.MsgTx, _ string) error {
	w.PublishedTransactions <- tx
	return nil
}

// LabelTransaction currently does nothing.
func (w *WalletController) LabelTransaction(chainhash.Hash, string,
	bool) error {

	return nil
}

// SubscribeTransactions currently does nothing.
func (w *WalletController) SubscribeTransactions() (lnwallet.TransactionSubscription,
	error) {

	return nil, nil
}

// IsSynced currently returns dummy values.
func (w *WalletController) IsSynced() (bool, int64, error) {
	return true, int64(0), nil
}

// GetRecoveryInfo currently returns dummy values.
func (w *WalletController) GetRecoveryInfo() (bool, float64, error) {
	return true, float64(1), nil
}

// Start currently does nothing.
func (w *WalletController) Start() error {
	return nil
}

// Stop currently does nothing.
func (w *WalletController) Stop() error {
	return nil
}

func (w *WalletController) FetchTx(chainhash.Hash) (*wire.MsgTx, error) {
	return nil, nil
}

func (w *WalletController) RemoveDescendants(*wire.MsgTx) error {
	return nil
}
