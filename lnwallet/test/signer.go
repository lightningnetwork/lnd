package test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// MockSigner mock of Signer implementation.
type MockSigner struct {
	key *btcec.PrivateKey
}

// MakeMockSigner creates MockSigner instance.
func MakeMockSigner(key *btcec.PrivateKey) *MockSigner {
	return &MockSigner{key: key}
}

// SignOutputRaw generates a signature for the passed transaction
// according to the data within the passed SignDescriptor.
func (m *MockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	if !privKey.PubKey().IsEqual(signDesc.KeyDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = lnwallet.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = lnwallet.DeriveRevocationPrivKey(privKey,
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

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed
// SignDescriptor.
func (m *MockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {

	// TODO(roasbeef): expose tweaked signer from lnwallet so don't need to
	// duplicate this code?

	privKey := m.key

	switch {
	case signDesc.SingleTweak != nil:
		privKey = lnwallet.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = lnwallet.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		signDesc.HashType, privKey, true)
	if err != nil {
		return nil, err
	}

	return &lnwallet.InputScript{
		Witness: witnessScript,
	}, nil
}

// MockNotfier is mock of ChainNotifier implementation.
type MockNotfier struct {
	ConfChannel chan *chainntnfs.TxConfirmation
}

// RegisterConfirmationsNtfn registers an intent to be notified once
// txid reaches numConfs confirmations.
func (m *MockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.ConfChannel,
	}, nil
}

// RegisterBlockEpochNtfn registers an intent to be notified of each
// new block connected to the tip of the main chain.
func (m *MockNotfier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {
	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

// Start is stub method.
func (m *MockNotfier) Start() error {
	return nil
}

// Stop is stub method.
func (m *MockNotfier) Stop() error {
	return nil
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint is successfully spent within a transaction.
func (m *MockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {
	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

// MockSpendNotifier extends the mockNotifier so that spend notifications can be
// triggered and delivered to subscribers.
type MockSpendNotifier struct {
	*MockNotfier
	spendMap map[wire.OutPoint][]chan *chainntnfs.SpendDetail
	mtx      sync.Mutex
}

// MakeMockSpendNotifier creates MockSpendNotifier instance.
func MakeMockSpendNotifier() *MockSpendNotifier {
	return &MockSpendNotifier{
		MockNotfier: &MockNotfier{
			ConfChannel: make(chan *chainntnfs.TxConfirmation),
		},
		spendMap: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
	}
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint is successfully spent within a transaction.
func (m *MockSpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	_ []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	spendChan := make(chan *chainntnfs.SpendDetail)
	m.spendMap[*outpoint] = append(m.spendMap[*outpoint], spendChan)
	return &chainntnfs.SpendEvent{
		Spend: spendChan,
		Cancel: func() {
		},
	}, nil
}

// Spend dispatches SpendDetails to all subscribers of the outpoint. The details
// will include the transaction and height provided by the caller.
func (m *MockSpendNotifier) Spend(outpoint *wire.OutPoint, height int32,
	txn *wire.MsgTx) {

	if spendChans, ok := m.spendMap[*outpoint]; ok {
		delete(m.spendMap, *outpoint)
		for _, spendChan := range spendChans {
			txnHash := txn.TxHash()
			spendChan <- &chainntnfs.SpendDetail{
				SpentOutPoint:     outpoint,
				SpendingHeight:    height,
				SpendingTx:        txn,
				SpenderTxHash:     &txnHash,
				SpenderInputIndex: outpoint.Index,
			}
		}
	}
}

// MockMultSigner is a simple implementation of the Signer interface. Each one has
// a set of private keys in a slice and can sign messages using the appropriate
// one.
type MockMultSigner struct {
	Privkeys  []*btcec.PrivateKey
	NetParams *chaincfg.Params
}

// SignOutputRaw generates a signature for the passed transaction
// according to the data within the passed SignDescriptor.
func (m *MockMultSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) ([]byte, error) {
	pubkey := signDesc.KeyDesc.PubKey
	switch {
	case signDesc.SingleTweak != nil:
		pubkey = lnwallet.TweakPubKeyWithTweak(pubkey, signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		pubkey = lnwallet.DeriveRevocationPubkey(pubkey,
			signDesc.DoubleTweak.PubKey())
	}

	hash160 := btcutil.Hash160(pubkey.SerializeCompressed())
	privKey := m.findKey(hash160, signDesc.SingleTweak, signDesc.DoubleTweak)
	if privKey == nil {
		return nil, fmt.Errorf("mock signer does not have key")
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.WitnessScript,
		txscript.SigHashAll, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed
// SignDescriptor.
func (m *MockMultSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *lnwallet.SignDescriptor) (*lnwallet.InputScript, error) {
	scriptType, addresses, _, err := txscript.ExtractPkScriptAddrs(
		signDesc.Output.PkScript, m.NetParams)
	if err != nil {
		return nil, err
	}

	switch scriptType {
	case txscript.PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("mock signer does not have key for "+
				"address %v", addresses[0])
		}

		scriptSig, err := txscript.SignatureScript(tx, signDesc.InputIndex,
			signDesc.Output.PkScript, txscript.SigHashAll, privKey, true)
		if err != nil {
			return nil, err
		}

		return &lnwallet.InputScript{ScriptSig: scriptSig}, nil

	case txscript.WitnessV0PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("mock signer does not have key for "+
				"address %v", addresses[0])
		}

		witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
			signDesc.InputIndex, signDesc.Output.Value,
			signDesc.Output.PkScript, txscript.SigHashAll, privKey, true)
		if err != nil {
			return nil, err
		}

		return &lnwallet.InputScript{Witness: witnessScript}, nil

	default:
		return nil, fmt.Errorf("unexpected script type: %v", scriptType)
	}
}

// findKey searches through all stored private keys and returns one
// corresponding to the hashed pubkey if it can be found. The public key may
// either correspond directly to the private key or to the private key with a
// tweak applied.
func (m *MockMultSigner) findKey(needleHash160 []byte, singleTweak []byte,
	doubleTweak *btcec.PrivateKey) *btcec.PrivateKey {

	for _, privkey := range m.Privkeys {
		// First check whether public key is directly derived from private key.
		hash160 := btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}

		// Otherwise check if public key is derived from tweaked private key.
		switch {
		case singleTweak != nil:
			privkey = lnwallet.TweakPrivKey(privkey, singleTweak)
		case doubleTweak != nil:
			privkey = lnwallet.DeriveRevocationPrivKey(privkey, doubleTweak)
		default:
			continue
		}
		hash160 = btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}
	}
	return nil
}

// MockPreimageCache is mock of PreimageCache implementation.
type MockPreimageCache struct {
	sync.Mutex
	PreimageMap map[[32]byte][]byte
}

// LookupPreimage attempts to look up a preimage according to its hash.
func (m *MockPreimageCache) LookupPreimage(hash []byte) ([]byte, bool) {
	m.Lock()
	defer m.Unlock()

	var h [32]byte
	copy(h[:], hash)

	p, ok := m.PreimageMap[h]
	return p, ok
}

// AddPreimage attempts to add a new preimage to the global cache.
func (m *MockPreimageCache) AddPreimage(preimage []byte) error {
	m.Lock()
	defer m.Unlock()

	m.PreimageMap[sha256.Sum256(preimage[:])] = preimage

	return nil
}
