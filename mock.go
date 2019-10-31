package lnd

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet/txauthor"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// The block height returned by the mock BlockChainIO's GetBestBlock.
const fundingBroadcastHeight = 123

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	if !privKey.PubKey().IsEqual(signDesc.KeyDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, signDesc.HashType,
		privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	// TODO(roasbeef): expose tweaked signer from lnwallet so don't need to
	// duplicate this code?

	privKey := m.key

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		signDesc.HashType, privKey, true)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness: witnessScript,
	}, nil
}

type mockNotfier struct {
	confChannel chan *chainntnfs.TxConfirmation
}

func (m *mockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChannel,
	}, nil
}
func (m *mockNotfier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

func (m *mockNotfier) Start() error {
	return nil
}

func (m *mockNotfier) Stop() error {
	return nil
}
func (m *mockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

// mockSpendNotifier extends the mockNotifier so that spend notifications can be
// triggered and delivered to subscribers.
type mockSpendNotifier struct {
	*mockNotfier
	spendMap map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	spends   map[wire.OutPoint]*chainntnfs.SpendDetail
	mtx      sync.Mutex
}

func makeMockSpendNotifier() *mockSpendNotifier {
	return &mockSpendNotifier{
		mockNotfier: &mockNotfier{
			confChannel: make(chan *chainntnfs.TxConfirmation),
		},
		spendMap: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
		spends:   make(map[wire.OutPoint]*chainntnfs.SpendDetail),
	}
}

func (m *mockSpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	if detail, ok := m.spends[*outpoint]; ok {
		// Deliver spend immediately if details are already known.
		spendChan <- &chainntnfs.SpendDetail{
			SpentOutPoint:     detail.SpentOutPoint,
			SpendingHeight:    detail.SpendingHeight,
			SpendingTx:        detail.SpendingTx,
			SpenderTxHash:     detail.SpenderTxHash,
			SpenderInputIndex: detail.SpenderInputIndex,
		}
	} else {
		// Otherwise, queue the notification for delivery if the spend
		// is ever received.
		m.spendMap[*outpoint] = append(m.spendMap[*outpoint], spendChan)
	}

	return &chainntnfs.SpendEvent{
		Spend: spendChan,
		Cancel: func() {
		},
	}, nil
}

// Spend dispatches SpendDetails to all subscribers of the outpoint. The details
// will include the transaction and height provided by the caller.
func (m *mockSpendNotifier) Spend(outpoint *wire.OutPoint, height int32,
	txn *wire.MsgTx) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	txnHash := txn.TxHash()
	details := &chainntnfs.SpendDetail{
		SpentOutPoint:     outpoint,
		SpendingHeight:    height,
		SpendingTx:        txn,
		SpenderTxHash:     &txnHash,
		SpenderInputIndex: outpoint.Index,
	}

	// Cache details in case of late registration.
	if _, ok := m.spends[*outpoint]; !ok {
		m.spends[*outpoint] = details
	}

	// Deliver any backlogged spend notifications.
	if spendChans, ok := m.spendMap[*outpoint]; ok {
		delete(m.spendMap, *outpoint)
		for _, spendChan := range spendChans {
			spendChan <- &chainntnfs.SpendDetail{
				SpentOutPoint:     details.SpentOutPoint,
				SpendingHeight:    details.SpendingHeight,
				SpendingTx:        details.SpendingTx,
				SpenderTxHash:     details.SpenderTxHash,
				SpenderInputIndex: details.SpenderInputIndex,
			}
		}
	}
}

type mockChainIO struct {
	bestHeight int32
}

var _ lnwallet.BlockChainIO = (*mockChainIO)(nil)

func (m *mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return activeNetParams.GenesisHash, m.bestHeight, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {
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
	publishedTransactions chan *wire.MsgTx
	index                 uint32
	utxos                 []*lnwallet.Utxo
}

// BackEnd returns "mock" to signify a mock wallet controller.
func (*mockWalletController) BackEnd() string {
	return "mock"
}

// FetchInputInfo will be called to get info about the inputs to the funding
// transaction.
func (*mockWalletController) FetchInputInfo(
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
func (*mockWalletController) ConfirmedBalance(confs int32) (btcutil.Amount, error) {
	return 0, nil
}

// NewAddress is called to get new addresses for delivery, change etc.
func (m *mockWalletController) NewAddress(addrType lnwallet.AddressType,
	change bool) (btcutil.Address, error) {
	addr, _ := btcutil.NewAddressPubKey(
		m.rootKey.PubKey().SerializeCompressed(), &chaincfg.MainNetParams)
	return addr, nil
}
func (*mockWalletController) LastUnusedAddress(addrType lnwallet.AddressType) (
	btcutil.Address, error) {
	return nil, nil
}

func (*mockWalletController) IsOurAddress(a btcutil.Address) bool {
	return false
}

func (*mockWalletController) SendOutputs(outputs []*wire.TxOut,
	_ chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	return nil, nil
}

func (*mockWalletController) CreateSimpleTx(outputs []*wire.TxOut,
	_ chainfee.SatPerKWeight, _ bool) (*txauthor.AuthoredTx, error) {

	return nil, nil
}

// ListUnspentWitness is called by the wallet when doing coin selection. We just
// need one unspent for the funding transaction.
func (m *mockWalletController) ListUnspentWitness(minconfirms,
	maxconfirms int32) ([]*lnwallet.Utxo, error) {

	// If the mock already has a list of utxos, return it.
	if m.utxos != nil {
		return m.utxos, nil
	}

	// Otherwise create one to return.
	utxo := &lnwallet.Utxo{
		AddressType: lnwallet.WitnessPubKey,
		Value:       btcutil.Amount(10 * btcutil.SatoshiPerBitcoin),
		PkScript:    make([]byte, 22),
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{},
			Index: m.index,
		},
	}
	atomic.AddUint32(&m.index, 1)
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
func (*mockWalletController) IsSynced() (bool, int64, error) {
	return true, int64(0), nil
}
func (*mockWalletController) Start() error {
	return nil
}
func (*mockWalletController) Stop() error {
	return nil
}

type mockSecretKeyRing struct {
	rootKey *btcec.PrivateKey
}

func (m *mockSecretKeyRing) DeriveNextKey(keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {
	return keychain.KeyDescriptor{
		PubKey: m.rootKey.PubKey(),
	}, nil
}

func (m *mockSecretKeyRing) DeriveKey(keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {
	return keychain.KeyDescriptor{
		PubKey: m.rootKey.PubKey(),
	}, nil
}

func (m *mockSecretKeyRing) DerivePrivKey(keyDesc keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
	return m.rootKey, nil
}

func (m *mockSecretKeyRing) ScalarMult(keyDesc keychain.KeyDescriptor,
	pubKey *btcec.PublicKey) ([]byte, error) {
	return nil, nil
}
