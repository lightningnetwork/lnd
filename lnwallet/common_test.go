package lnwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

// mockSigner is a simple implementation of the Signer interface. Each one has
// a set of private keys in a slice and can sign messages using the appropriate
// one.
type mockSigner struct {
	privkeys  []*btcec.PrivateKey
	netParams *chaincfg.Params
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx, signDesc *SignDescriptor) ([]byte, error) {
	pubkey := signDesc.PubKey
	switch {
	case signDesc.SingleTweak != nil:
		pubkey = TweakPubKeyWithTweak(pubkey, signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		pubkey = DeriveRevocationPubkey(pubkey, signDesc.DoubleTweak.PubKey())
	}

	hash160 := btcutil.Hash160(pubkey.SerializeCompressed())
	privKey := m.findKey(hash160, signDesc.SingleTweak, signDesc.DoubleTweak)
	if privKey == nil {
		return nil, fmt.Errorf("Mock signer does not have key")
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.WitnessScript,
		signDesc.HashType, privKey)
	if err != nil {
		return nil, err
	}

	return sig[:len(sig)-1], nil
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx, signDesc *SignDescriptor) (*InputScript, error) {
	scriptType, addresses, _, err := txscript.ExtractPkScriptAddrs(
		signDesc.Output.PkScript, m.netParams)
	if err != nil {
		return nil, err
	}

	switch scriptType {
	case txscript.PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("Mock signer does not have key for "+
				"address %v", addresses[0])
		}

		scriptSig, err := txscript.SignatureScript(tx, signDesc.InputIndex,
			signDesc.Output.PkScript, signDesc.HashType, privKey, true)
		if err != nil {
			return nil, err
		}

		return &InputScript{ScriptSig: scriptSig}, nil

	case txscript.WitnessV0PubKeyHashTy:
		privKey := m.findKey(addresses[0].ScriptAddress(), signDesc.SingleTweak,
			signDesc.DoubleTweak)
		if privKey == nil {
			return nil, fmt.Errorf("Mock signer does not have key for "+
				"address %v", addresses[0])
		}

		witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
			signDesc.InputIndex, signDesc.Output.Value,
			signDesc.Output.PkScript, signDesc.HashType, privKey, true)
		if err != nil {
			return nil, err
		}

		return &InputScript{Witness: witnessScript}, nil

	default:
		return nil, fmt.Errorf("Unexpected script type: %v", scriptType)
	}
}

// findKey searches through all stored private keys and returns one
// corresponding to the hashed pubkey if it can be found. The public key may
// either correspond directly to the private key or to the private key with a
// tweak applied.
func (m *mockSigner) findKey(needleHash160 []byte, singleTweak []byte,
	doubleTweak *btcec.PrivateKey) *btcec.PrivateKey {

	for _, privkey := range m.privkeys {
		// First check whether public key is directly derived from private key.
		hash160 := btcutil.Hash160(privkey.PubKey().SerializeCompressed())
		if bytes.Equal(hash160, needleHash160) {
			return privkey
		}

		// Otherwise check if public key is derived from tweaked private key.
		switch {
		case singleTweak != nil:
			privkey = TweakPrivKey(privkey, singleTweak)
		case doubleTweak != nil:
			privkey = DeriveRevocationPrivKey(privkey, doubleTweak)
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

type mockNotfier struct {
}

func (m *mockNotfier) RegisterConfirmationsNtfn(txid *chainhash.Hash, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return nil, nil
}
func (m *mockNotfier) RegisterBlockEpochNtfn() (*chainntnfs.BlockEpochEvent, error) {
	return nil, nil
}

func (m *mockNotfier) Start() error {
	return nil
}

func (m *mockNotfier) Stop() error {
	return nil
}
func (m *mockNotfier) RegisterSpendNtfn(outpoint *wire.OutPoint, heightHint uint32) (*chainntnfs.SpendEvent, error) {
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
}

func makeMockSpendNotifier() *mockSpendNotifier {
	return &mockSpendNotifier{
		spendMap: make(map[wire.OutPoint][]chan *chainntnfs.SpendDetail),
	}
}

func (m *mockSpendNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	m.spendMap[*outpoint] = append(m.spendMap[*outpoint], spendChan)
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

// pubkeyFromHex parses a Bitcoin public key from a hex encoded string.
func pubkeyFromHex(keyHex string) (*btcec.PublicKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(bytes, btcec.S256())
}

// privkeyFromHex parses a Bitcoin private key from a hex encoded string.
func privkeyFromHex(keyHex string) (*btcec.PrivateKey, error) {
	bytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}
	key, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes)
	return key, nil

}

// pubkeyToHex serializes a Bitcoin public key to a hex encoded string.
func pubkeyToHex(key *btcec.PublicKey) string {
	return hex.EncodeToString(key.SerializeCompressed())
}

// privkeyFromHex serializes a Bitcoin private key to a hex encoded string.
func privkeyToHex(key *btcec.PrivateKey) string {
	return hex.EncodeToString(key.Serialize())
}

// signatureFromHex parses a Bitcoin signature from a hex encoded string.
func signatureFromHex(sigHex string) (*btcec.Signature, error) {
	bytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}
	return btcec.ParseSignature(bytes, btcec.S256())
}

// blockFromHex parses a full Bitcoin block from a hex encoded string.
func blockFromHex(blockHex string) (*btcutil.Block, error) {
	bytes, err := hex.DecodeString(blockHex)
	if err != nil {
		return nil, err
	}
	return btcutil.NewBlockFromBytes(bytes)
}

// txFromHex parses a full Bitcoin transaction from a hex encoded string.
func txFromHex(txHex string) (*btcutil.Tx, error) {
	bytes, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}
	return btcutil.NewTxFromBytes(bytes)
}
