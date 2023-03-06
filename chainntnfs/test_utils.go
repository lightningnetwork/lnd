//go:build dev
// +build dev

package chainntnfs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

var (
	// TrickleInterval is the interval at which the miner should trickle
	// transactions to its peers. We'll set it small to ensure the miner
	// propagates transactions quickly in the tests.
	TrickleInterval = 10 * time.Millisecond
)

var (
	NetParams = &chaincfg.RegressionNetParams
)

// randPubKeyHashScript generates a P2PKH script that pays to the public key of
// a randomly-generated private key.
func randPubKeyHashScript() ([]byte, *btcec.PrivateKey, error) {
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, err
	}

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addrScript, err := btcutil.NewAddressPubKeyHash(pubKeyHash, NetParams)
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
		return nil, nil, fmt.Errorf("unable to generate pkScript: %v", err)
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

	spendingTx := wire.NewMsgTx(1)
	spendingTx.AddTxIn(&wire.TxIn{PreviousOutPoint: *prevOutPoint})
	spendingTx.AddTxOut(&wire.TxOut{Value: 1e8, PkScript: prevOutput.PkScript})

	sigScript, err := txscript.SignatureScript(
		spendingTx, 0, prevOutput.PkScript, txscript.SigHashAll,
		privKey, true,
	)
	require.NoError(t, err, "unable to sign tx")
	spendingTx.TxIn[0].SignatureScript = sigScript

	return spendingTx
}

// NewMiner spawns testing harness backed by a btcd node that can serve as a
// miner.
func NewMiner(t *testing.T, extraArgs []string, createChain bool,
	spendableOutputs uint32) *rpctest.Harness {

	t.Helper()

	// Add the trickle interval argument to the extra args.
	trickle := fmt.Sprintf("--trickleinterval=%v", TrickleInterval)
	extraArgs = append(extraArgs, trickle)

	node, err := rpctest.New(NetParams, nil, extraArgs, "")
	require.NoError(t, err, "unable to create backend node")
	t.Cleanup(func() {
		require.NoError(t, node.TearDown())
	})

	if err := node.SetUp(createChain, spendableOutputs); err != nil {
		t.Fatalf("unable to set up backend node: %v", err)
	}

	return node
}

// NewBitcoindBackend spawns a new bitcoind node that connects to a miner at the
// specified address. The txindex boolean can be set to determine whether the
// backend node should maintain a transaction index. The rpcpolling boolean
// can be set to determine whether bitcoind's RPC polling interface should be
// used for block and tx notifications or if its ZMQ interface should be used.
// A connection to the newly spawned bitcoind node is returned.
func NewBitcoindBackend(t *testing.T, minerAddr string, txindex,
	rpcpolling bool) *chain.BitcoindConn {

	t.Helper()

	// We use ioutil.TempDir here instead of t.TempDir because some versions
	// of bitcoind complain about the zmq connection string formats when the
	// t.TempDir directory string is used.
	tempBitcoindDir, err := ioutil.TempDir("", "bitcoind")
	require.NoError(t, err, "unable to create temp dir")

	rpcPort := rand.Intn(65536-1024) + 1024
	zmqBlockHost := "ipc:///" + tempBitcoindDir + "/blocks.socket"
	zmqTxHost := "ipc:///" + tempBitcoindDir + "/tx.socket"

	args := []string{
		"-connect=" + minerAddr,
		"-datadir=" + tempBitcoindDir,
		"-regtest",
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6fd$507c670e800a952" +
			"84294edb5773b05544b220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		"-disablewallet",
		"-zmqpubrawblock=" + zmqBlockHost,
		"-zmqpubrawtx=" + zmqTxHost,
	}
	if txindex {
		args = append(args, "-txindex")
	}

	bitcoind := exec.Command("bitcoind", args...)
	if err := bitcoind.Start(); err != nil {
		t.Fatalf("unable to start bitcoind: %v", err)
	}
	t.Cleanup(func() {
		_ = bitcoind.Process.Kill()
		_ = bitcoind.Wait()
	})

	// Wait for the bitcoind instance to start up.
	host := fmt.Sprintf("127.0.0.1:%d", rpcPort)
	cfg := &chain.BitcoindConfig{
		ChainParams: NetParams,
		Host:        host,
		User:        "weks",
		Pass:        "weks",
		// Fields only required for pruned nodes, not needed for these
		// tests.
		Dialer:             nil,
		PrunedModeMaxPeers: 0,
	}

	if rpcpolling {
		cfg.PollingConfig = &chain.PollingConfig{
			BlockPollingInterval: time.Millisecond * 20,
			TxPollingInterval:    time.Millisecond * 20,
		}
	} else {
		cfg.ZMQConfig = &chain.ZMQConfig{
			ZMQBlockHost:    zmqBlockHost,
			ZMQTxHost:       zmqTxHost,
			ZMQReadDeadline: 5 * time.Second,
		}
	}

	var conn *chain.BitcoindConn
	err = wait.NoError(func() error {
		var err error
		conn, err = chain.NewBitcoindConn(cfg)
		if err != nil {
			return err
		}

		return conn.Start()
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to establish connection to bitcoind: %v", err)
	}
	t.Cleanup(conn.Stop)

	return conn
}

// NewNeutrinoBackend spawns a new neutrino node that connects to a miner at
// the specified address.
func NewNeutrinoBackend(t *testing.T, minerAddr string) *neutrino.ChainService {
	t.Helper()

	spvDir := t.TempDir()

	dbName := filepath.Join(spvDir, "neutrino.db")
	spvDatabase, err := walletdb.Create(
		"bdb", dbName, true, kvdb.DefaultDBTimeout,
	)
	if err != nil {
		t.Fatalf("unable to create walletdb: %v", err)
	}
	t.Cleanup(func() {
		spvDatabase.Close()
	})

	// Create an instance of neutrino connected to the running btcd
	// instance.
	spvConfig := neutrino.Config{
		DataDir:      spvDir,
		Database:     spvDatabase,
		ChainParams:  *NetParams,
		ConnectPeers: []string{minerAddr},
	}
	spvNode, err := neutrino.NewChainService(spvConfig)
	if err != nil {
		t.Fatalf("unable to create neutrino: %v", err)
	}

	// We'll also wait for the instance to sync up fully to the chain
	// generated by the btcd instance.
	spvNode.Start()
	for !spvNode.IsCurrent() {
		time.Sleep(time.Millisecond * 100)
	}
	t.Cleanup(func() {
		spvNode.Stop()
	})

	return spvNode
}
