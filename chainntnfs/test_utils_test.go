// +build dev

package chainntnfs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino"
)

var (
	// trickleInterval is the interval at which the miner should trickle
	// transactions to its peers. We'll set it small to ensure the miner
	// propagates transactions quickly in the tests.
	trickleInterval = 10 * time.Millisecond
)

var (
	NetParams = &chaincfg.RegressionNetParams

	testPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), testPrivKey)
	addrPk, _       = btcutil.NewAddressPubKey(
		pubKey.SerializeCompressed(), NetParams,
	)
	testAddr = addrPk.AddressPubKeyHash()
)

// GetTestTxidAndScript generate a new test transaction and returns its txid and
// the script of the output being generated.
func GetTestTxidAndScript(h *rpctest.Harness) (*chainhash.Hash, []byte, error) {
	script, err := txscript.PayToAddrScript(testAddr)
	if err != nil {
		return nil, nil, err
	}

	output := &wire.TxOut{Value: 2e8, PkScript: script}
	txid, err := h.SendOutputs([]*wire.TxOut{output}, 10)
	if err != nil {
		return nil, nil, err
	}

	return txid, script, nil
}

// WaitForMempoolTx waits for the txid to be seen in the miner's mempool.
func WaitForMempoolTx(miner *rpctest.Harness, txid *chainhash.Hash) error {
	timeout := time.After(10 * time.Second)
	trickle := time.After(2 * trickleInterval)
	for {
		// Check for the harness' knowledge of the txid.
		tx, err := miner.Node.GetRawTransaction(txid)
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
func CreateSpendableOutput(t *testing.T, miner *rpctest.Harness) (*wire.OutPoint, []byte) {
	t.Helper()

	// Create a transaction that only has one output, the one destined for
	// the recipient.
	script, err := txscript.PayToAddrScript(testAddr)
	if err != nil {
		t.Fatalf("unable to create p2pkh script: %v", err)
	}
	output := &wire.TxOut{Value: 2e8, PkScript: script}
	txid, err := miner.SendOutputsWithoutChange([]*wire.TxOut{output}, 10)
	if err != nil {
		t.Fatalf("unable to create tx: %v", err)
	}

	// Mine the transaction to mark the output as spendable.
	if err := WaitForMempoolTx(miner, txid); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate single block: %v", err)
	}

	return wire.NewOutPoint(txid, 0), script
}

// CreateSpendTx creates a transaction spending the specified output.
func CreateSpendTx(t *testing.T, outpoint *wire.OutPoint, pkScript []byte) *wire.MsgTx {
	t.Helper()

	spendingTx := wire.NewMsgTx(1)
	spendingTx.AddTxIn(&wire.TxIn{PreviousOutPoint: *outpoint})
	spendingTx.AddTxOut(&wire.TxOut{Value: 1e8, PkScript: pkScript})

	sigScript, err := txscript.SignatureScript(
		spendingTx, 0, pkScript, txscript.SigHashAll, privKey, true,
	)
	if err != nil {
		t.Fatalf("unable to sign tx: %v", err)
	}
	spendingTx.TxIn[0].SignatureScript = sigScript

	return spendingTx
}

// NewMiner spawns testing harness backed by a btcd node that can serve as a
// miner.
func NewMiner(t *testing.T, extraArgs []string, createChain bool,
	spendableOutputs uint32) (*rpctest.Harness, func()) {

	t.Helper()

	// Add the trickle interval argument to the extra args.
	trickle := fmt.Sprintf("--trickleinterval=%v", trickleInterval)
	extraArgs = append(extraArgs, trickle)

	node, err := rpctest.New(NetParams, nil, extraArgs)
	if err != nil {
		t.Fatalf("unable to create backend node: %v", err)
	}
	if err := node.SetUp(createChain, spendableOutputs); err != nil {
		node.TearDown()
		t.Fatalf("unable to set up backend node: %v", err)
	}

	return node, func() { node.TearDown() }
}

// NewBitcoindBackend spawns a new bitcoind node that connects to a miner at the
// specified address. The txindex boolean can be set to determine whether the
// backend node should maintain a transaction index. A connection to the newly
// spawned bitcoind node is returned.
func NewBitcoindBackend(t *testing.T, minerAddr string,
	txindex bool) (*chain.BitcoindConn, func()) {

	t.Helper()

	tempBitcoindDir, err := ioutil.TempDir("", "bitcoind")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

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
		os.RemoveAll(tempBitcoindDir)
		t.Fatalf("unable to start bitcoind: %v", err)
	}

	// Wait for the bitcoind instance to start up.
	time.Sleep(time.Second)

	host := fmt.Sprintf("127.0.0.1:%d", rpcPort)
	conn, err := chain.NewBitcoindConn(
		NetParams, host, "weks", "weks", zmqBlockHost, zmqTxHost,
		100*time.Millisecond,
	)
	if err != nil {
		bitcoind.Process.Kill()
		bitcoind.Wait()
		os.RemoveAll(tempBitcoindDir)
		t.Fatalf("unable to establish connection to bitcoind: %v", err)
	}
	if err := conn.Start(); err != nil {
		bitcoind.Process.Kill()
		bitcoind.Wait()
		os.RemoveAll(tempBitcoindDir)
		t.Fatalf("unable to establish connection to bitcoind: %v", err)
	}

	return conn, func() {
		conn.Stop()
		bitcoind.Process.Kill()
		bitcoind.Wait()
		os.RemoveAll(tempBitcoindDir)
	}
}

// NewNeutrinoBackend spawns a new neutrino node that connects to a miner at
// the specified address.
func NewNeutrinoBackend(t *testing.T, minerAddr string) (*neutrino.ChainService, func()) {
	t.Helper()

	spvDir, err := ioutil.TempDir("", "neutrino")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	dbName := filepath.Join(spvDir, "neutrino.db")
	spvDatabase, err := walletdb.Create("bdb", dbName)
	if err != nil {
		os.RemoveAll(spvDir)
		t.Fatalf("unable to create walletdb: %v", err)
	}

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
		os.RemoveAll(spvDir)
		spvDatabase.Close()
		t.Fatalf("unable to create neutrino: %v", err)
	}

	// We'll also wait for the instance to sync up fully to the chain
	// generated by the btcd instance.
	spvNode.Start()
	for !spvNode.IsCurrent() {
		time.Sleep(time.Millisecond * 100)
	}

	return spvNode, func() {
		spvNode.Stop()
		spvDatabase.Close()
		os.RemoveAll(spvDir)
	}
}
