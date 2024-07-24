//go:build dev
// +build dev

package chainntnfs

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/stretchr/testify/require"
)

var (
	// TrickleInterval is the interval at which the miner should trickle
	// transactions to its peers. We'll set it small to ensure the miner
	// propagates transactions quickly in the tests.
	TrickleInterval = 10 * time.Millisecond
)

// randPubKeyHashScript generates a P2PKH script that pays to the public key of
// a randomly-generated private key.
func randPubKeyHashScript() ([]byte, *btcec.PrivateKey, error) {
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, err
	}

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addrScript, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, unittest.NetParams,
	)
	if err != nil {
		return nil, nil, err
	}

	pkScript, err := txscript.PayToAddrScript(addrScript)
	if err != nil {
		return nil, nil, err
	}

	return pkScript, privKey, nil
}

// GetTestTxidAndScript generate a new test transaction and returns its txid and
// the script of the output being generated.
func GetTestTxidAndScript(h *rpctest.Harness) (*chainhash.Hash, []byte, error) {
	pkScript, _, err := randPubKeyHashScript()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to generate pkScript: %w",
			err)
	}
	output := &wire.TxOut{Value: 2e8, PkScript: pkScript}
	txid, err := h.SendOutputs([]*wire.TxOut{output}, 10)
	if err != nil {
		return nil, nil, err
	}

	return txid, pkScript, nil
}

// WaitForMempoolTx waits for the txid to be seen in the miner's mempool.
func WaitForMempoolTx(miner *rpctest.Harness, txid *chainhash.Hash) error {
	timeout := time.After(10 * time.Second)
	trickle := time.After(2 * TrickleInterval)
	for {
		// Check for the harness' knowledge of the txid.
		tx, err := miner.Client.GetRawTransaction(txid)
		if err != nil {
			jsonErr, ok := err.(*btcjson.RPCError)
			if ok && jsonErr.Code == btcjson.ErrRPCNoTxInfo {
				continue
			}
			return err
		}

		if tx != nil && tx.Hash().IsEqual(txid) {
			break
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			return errors.New("timed out waiting for tx")
		}
	}

	// To ensure any transactions propagate from the miner to the peers
	// before returning, ensure we have waited for at least
	// 2*trickleInterval before returning.
	select {
	case <-trickle:
	case <-timeout:
		return errors.New("timeout waiting for trickle interval. " +
			"Trickle interval to large?")
	}

	return nil
}

// CreateSpendableOutput creates and returns an output that can be spent later
// on.
func CreateSpendableOutput(t *testing.T,
	miner *rpctest.Harness) (*wire.OutPoint, *wire.TxOut, *btcec.PrivateKey) {

	t.Helper()

	// Create a transaction that only has one output, the one destined for
	// the recipient.
	pkScript, privKey, err := randPubKeyHashScript()
	require.NoError(t, err, "unable to generate pkScript")
	output := &wire.TxOut{Value: 2e8, PkScript: pkScript}
	txid, err := miner.SendOutputsWithoutChange([]*wire.TxOut{output}, 10)
	require.NoError(t, err, "unable to create tx")

	// Mine the transaction to mark the output as spendable.
	if err := WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}
	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	return wire.NewOutPoint(txid, 0), output, privKey
}

// CreateSpendTx creates a transaction spending the specified output.
func CreateSpendTx(t *testing.T, prevOutPoint *wire.OutPoint,
	prevOutput *wire.TxOut, privKey *btcec.PrivateKey) *wire.MsgTx {

	t.Helper()

	// Create a new output.
	outputAmt := int64(1e8)
	witnessProgram, _, err := randPubKeyHashScript()
	require.NoError(t, err, "unable to generate pkScript")
	output := wire.NewTxOut(outputAmt, witnessProgram)

	// Create a new tx.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(prevOutPoint, nil, nil))
	tx.AddTxOut(output)

	// Generate the witness.
	sigHashes := input.NewTxSigHashesV0Only(tx)
	witnessScript, err := txscript.WitnessSignature(
		tx, sigHashes, 0, prevOutput.Value, prevOutput.PkScript,
		txscript.SigHashAll, privKey, true,
	)
	require.NoError(t, err, "unable to sign tx")

	tx.TxIn[0].Witness = witnessScript

	return tx
}
