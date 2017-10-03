package main

import (
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// The block height returned by the mock BlockChainIO's GetBestBlock.
const fundingBroadcastHeight = 123

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, txscript.SigHashAll,
		privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {
	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value,
		signDesc.Output.PkScript, txscript.SigHashAll, m.key, true)
	if err != nil {
		return nil, err
	}

	return &lnwallet.InputScript{
		Witness: witnessScript,
	}, nil
}

type mockNotfier struct {
	confChannel chan *chainntnfs.TxConfirmation
}

func (m *mockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs,
	heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChannel,
	}, nil
}
func (m *mockNotfier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent,
	error) {
	return nil, nil
}

func (m *mockNotfier) Start() error {
	return nil
}

func (m *mockNotfier) Stop() error {
	return nil
}
func (m *mockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

type mockChainIO struct{}

func (*mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return activeNetParams.GenesisHash, fundingBroadcastHeight, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint,
	heightHint uint32) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

// mockWalletController is used by the LightningWallet, and let us mock the
// interaction with the bitcoin network.
type mockWalletController struct {
	rootKey               *btcec.PrivateKey
	prevAddres            btcutil.Address
	publishedTransactions chan *wire.MsgTx
}

// FetchInputInfo will be called to get info about the inputs to the funding
// transaction.
func (*mockWalletController) FetchInputInfo(
	prevOut *wire.OutPoint) (*wire.TxOut, error) {
	txOut := &wire.TxOut{
		Value:    int64(10 * btcutil.SatoshiPerBitcoin),
		PkScript: []byte("dummy"),
	}
	return txOut, nil
}
func (*mockWalletController) ConfirmedBalance(confs int32,
	witness bool) (btcutil.Amount, error) {
	return 0, nil
}

// NewAddress is called to get new addresses for delivery, change etc.
func (m *mockWalletController) NewAddress(addrType lnwallet.AddressType,
	change bool) (btcutil.Address, error) {
	addr, _ := btcutil.NewAddressPubKey(
		m.rootKey.PubKey().SerializeCompressed(), &chaincfg.MainNetParams)
	return addr, nil
}
func (*mockWalletController) GetPrivKey(a btcutil.Address) (*btcec.PrivateKey, error) {
	return nil, nil
}

// NewRawKey will be called to get keys to be used for the funding tx and the
// commitment tx.
func (m *mockWalletController) NewRawKey() (*btcec.PublicKey, error) {
	return m.rootKey.PubKey(), nil
}

// FetchRootKey will be called to provide the wallet with a root key.
func (m *mockWalletController) FetchRootKey() (*btcec.PrivateKey, error) {
	return m.rootKey, nil
}
func (*mockWalletController) SendOutputs(outputs []*wire.TxOut) (*chainhash.Hash, error) {
	return nil, nil
}

// ListUnspentWitness is called by the wallet when doing coin selection. We just
// need one unspent for the funding transaction.
func (*mockWalletController) ListUnspentWitness(confirms int32) ([]*lnwallet.Utxo, error) {
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       btcutil.Amount(10 * btcutil.SatoshiPerBitcoin),
		PkScript:    make([]byte, 22),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: 0,
		},
	}
	var ret []*lnwallet.Utxo
	ret = append(ret, utxo)
	return ret, nil
}
func (*mockWalletController) ListTransactionDetails() ([]*lnwallet.TransactionDetail, error) {
	return nil, nil
}
func (*mockWalletController) LockOutpoint(o wire.OutPoint)   {}
func (*mockWalletController) UnlockOutpoint(o wire.OutPoint) {}
func (m *mockWalletController) PublishTransaction(tx *wire.MsgTx) error {
	m.publishedTransactions <- tx
	return nil
}
func (*mockWalletController) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	return nil, nil
}
func (*mockWalletController) IsSynced() (bool, error) {
	return true, nil
}
func (*mockWalletController) Start() error {
	return nil
}
func (*mockWalletController) Stop() error {
	return nil
}
