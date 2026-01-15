package miner

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/port"
)

// BitcoindMinerBackend implements MinerBackend using bitcoind.
type BitcoindMinerBackend struct {
	*testing.T

	// runCtx is a context with cancel method.
	//nolint:containedctx
	runCtx context.Context
	cancel context.CancelFunc

	// bitcoind process and configuration.
	cmd         *exec.Cmd
	dataDir     string
	rpcClient   *rpcclient.Client
	rpcHost     string
	rpcUser     string
	rpcPass     string
	p2pPort     int
	logPath     string
	logFilename string
}

type fundRawTransactionResp struct {
	Hex string `json:"hex"`
}

type signRawTransactionResp struct {
	Hex      string `json:"hex"`
	Complete bool   `json:"complete"`
}

type generateBlockResp struct {
	Hash string `json:"hash"`
}

type mempoolInfoResp struct {
	MempoolMinFee float64 `json:"mempoolminfee"`
}

type networkInfoResp struct {
	RelayFee float64 `json:"relayfee"`
}

func btcStringFromSats(sats int64) string {
	sign := ""
	if sats < 0 {
		sign = "-"
		sats = -sats
	}

	whole := sats / 1e8
	frac := sats % 1e8

	return fmt.Sprintf("%s%d.%08d", sign, whole, frac)
}

// NewBitcoindMinerBackend creates a new bitcoind miner backend.
func NewBitcoindMinerBackend(ctxb context.Context, t *testing.T,
	config *MinerConfig) *BitcoindMinerBackend {

	t.Helper()

	logDir := config.LogDir
	if logDir == "" {
		logDir = minerLogDir
	}

	logFilename := config.LogFilename
	if logFilename == "" {
		logFilename = "output_bitcoind_miner.log"
	}

	baseLogPath := fmt.Sprintf("%s/%s", node.GetLogDir(), logDir)

	ctxt, cancel := context.WithCancel(ctxb)

	return &BitcoindMinerBackend{
		T:           t,
		runCtx:      ctxt,
		cancel:      cancel,
		logPath:     baseLogPath,
		logFilename: logFilename,
		rpcUser:     "miner",
		rpcPass:     "minerpass",
	}
}

// Start starts the bitcoind miner backend.
func (b *BitcoindMinerBackend) Start(setupChain bool,
	numMatureOutputs uint32) error {
	// Create temporary directory for bitcoind data.
	tempDir, err := os.MkdirTemp("", "bitcoind-miner")
	if err != nil {
		return fmt.Errorf("unable to create temp directory: %w", err)
	}
	b.dataDir = tempDir

	// Create log directory if it doesn't exist.
	if err := os.MkdirAll(b.logPath, 0700); err != nil {
		return fmt.Errorf("unable to create log directory: %w", err)
	}

	logFile, err := filepath.Abs(b.logPath + "/bitcoind.log")
	if err != nil {
		return fmt.Errorf("unable to get absolute log path: %w", err)
	}

	// Generate ports.
	rpcPort := port.NextAvailablePort()
	b.p2pPort = port.NextAvailablePort()
	b.rpcHost = fmt.Sprintf("127.0.0.1:%d", rpcPort)

	// Build bitcoind command arguments.
	cmdArgs := []string{
		"-datadir=" + b.dataDir,
		"-regtest",
		"-txindex",
		// Whitelist localhost to speed up relay.
		"-whitelist=127.0.0.1",
		fmt.Sprintf("-rpcuser=%s", b.rpcUser),
		fmt.Sprintf("-rpcpassword=%s", b.rpcPass),
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-bind=127.0.0.1:%d", b.p2pPort),
		"-rpcallowip=127.0.0.1",
		"-server",
		// Run in foreground for easier process management.
		"-daemon=0",
		// 0x20000002 signals SegWit activation (BIP141 bit 1).
		"-blockversion=536870914",
		"-debug",
		"-debuglogfile=" + logFile,
		// Set fallback fee for transaction creation.
		"-fallbackfee=0.00001",
	}

	// Start bitcoind process.
	b.cmd = exec.Command("bitcoind", cmdArgs...)

	// Discard stdout and stderr to prevent output noise in tests.
	// All debug output goes to the log file via -debuglogfile.
	b.cmd.Stdout = nil
	b.cmd.Stderr = nil

	if err := b.cmd.Start(); err != nil {
		_ = b.cleanup()
		return fmt.Errorf("couldn't start bitcoind: %w", err)
	}

	// Create RPC client config.
	rpcCfg := rpcclient.ConnConfig{
		Host:                 b.rpcHost,
		User:                 b.rpcUser,
		Pass:                 b.rpcPass,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}

	client, err := rpcclient.New(&rpcCfg, nil)
	if err != nil {
		_ = b.stopProcess()
		_ = b.cleanup()
		return fmt.Errorf("unable to create rpc client: %w", err)
	}
	b.rpcClient = client

	// Wait for bitcoind to be ready (with retries). Use GetBlockCount which
	// is more universally supported. Bitcoind can take a while to start,
	// especially on first run, so we give it up to 2 minutes.
	maxRetries := 120
	retryDelay := 1 * time.Second

	for i := 0; i < maxRetries; i++ {
		_, err = b.rpcClient.GetBlockCount()
		if err == nil {
			// Successfully connected!
			break
		}

		// Check if process is still running.
		if b.cmd.Process != nil {
			// Process is running, wait and retry.
			time.Sleep(retryDelay)
			continue
		}

		// Process died.
		_ = b.cleanup()

		return fmt.Errorf("bitcoind process died during startup")
	}

	if err != nil {
		_ = b.stopProcess()
		_ = b.cleanup()
		return fmt.Errorf("unable to connect to bitcoind after %d "+
			"retries: %w", maxRetries, err)
	}

	// Create a default wallet for the miner using raw RPC.
	_, err = b.rpcClient.RawRequest("createwallet", []json.RawMessage{
		[]byte(`"miner"`),
	})
	if err != nil {
		_ = b.stopProcess()
		_ = b.cleanup()
		return fmt.Errorf("unable to create wallet: %w", err)
	}

	if setupChain {
		// Generate initial blocks to fund the wallet with mature
		// coinbase outputs.
		//
		// Coinbase outputs mature after 100 confirmations. In order to
		// have numMatureOutputs mature outputs available, we mine:
		//   100 + numMatureOutputs
		// blocks.
		//
		// Use legacy addresses to ensure compatibility with btcd before
		// SegWit is fully activated.
		//
		// Params: label, address_type.
		addrResult, err := b.rpcClient.RawRequest(
			"getnewaddress", []json.RawMessage{
				[]byte(`""`),
				[]byte(`"legacy"`),
			},
		)
		if err != nil {
			_ = b.stopProcess()
			_ = b.cleanup()
			return fmt.Errorf("unable to get new address: %w", err)
		}

		var addrStr string
		if err := json.Unmarshal(addrResult, &addrStr); err != nil {
			_ = b.stopProcess()
			_ = b.cleanup()
			return fmt.Errorf("unable to parse address: %w", err)
		}

		addr, err := btcutil.DecodeAddress(addrStr, HarnessNetParams)
		if err != nil {
			_ = b.stopProcess()
			_ = b.cleanup()
			return fmt.Errorf("unable to decode address: %w", err)
		}

		initialBlocks := int64(100 + numMatureOutputs)
		_, err = b.rpcClient.GenerateToAddress(initialBlocks, addr, nil)
		if err != nil {
			_ = b.stopProcess()
			_ = b.cleanup()
			return fmt.Errorf("unable to generate initial blocks: "+
				"%w", err)
		}
	}

	return nil
}

func (b *BitcoindMinerBackend) minRelayFeeBTCPerKVb() float64 {
	// If we fail to query the min relay fee, return 0 so callers can
	// continue with their requested fee rate.
	if b.rpcClient == nil {
		return 0
	}

	var (
		relayFeeBTCPerKVb      float64
		mempoolMinFeeBTCPerKVb float64
	)

	networkInfoJSON, err := b.rpcClient.RawRequest("getnetworkinfo", nil)
	if err == nil {
		var ni networkInfoResp
		if json.Unmarshal(networkInfoJSON, &ni) == nil {
			relayFeeBTCPerKVb = ni.RelayFee
		}
	}

	mempoolInfoJSON, err := b.rpcClient.RawRequest("getmempoolinfo", nil)
	if err == nil {
		var mi mempoolInfoResp
		if json.Unmarshal(mempoolInfoJSON, &mi) == nil {
			mempoolMinFeeBTCPerKVb = mi.MempoolMinFee
		}
	}

	if relayFeeBTCPerKVb > mempoolMinFeeBTCPerKVb {
		return relayFeeBTCPerKVb
	}

	return mempoolMinFeeBTCPerKVb
}

func txFromHex(hexStr string) (*wire.MsgTx, error) {
	rawTxBytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("decode tx hex: %w", err)
	}

	tx := &wire.MsgTx{}
	err = tx.Deserialize(bytes.NewReader(rawTxBytes))
	if err != nil {
		return nil, fmt.Errorf("deserialize tx: %w", err)
	}

	return tx, nil
}

// Stop stops the bitcoind miner backend and performs cleanup.
func (b *BitcoindMinerBackend) Stop() error {
	b.cancel()

	// Close RPC client.
	if b.rpcClient != nil {
		b.rpcClient.Disconnect()
	}

	// Stop bitcoind process.
	_ = b.stopProcess()

	// Copy logs and cleanup.
	b.saveLogs()

	return b.cleanup()
}

// stopProcess stops the bitcoind process gracefully or forcefully.
func (b *BitcoindMinerBackend) stopProcess() error {
	if b.cmd == nil || b.cmd.Process == nil {
		return nil
	}

	// Try to stop bitcoind gracefully via RPC if client is available.
	if b.rpcClient != nil {
		_, _ = b.rpcClient.RawRequest("stop", nil)
		// Give it a moment to shutdown gracefully.
		time.Sleep(500 * time.Millisecond)
	}

	// Kill the process if it's still running.
	_ = b.cmd.Process.Kill()

	// Wait for the process to exit to ensure it releases file handles.
	_ = b.cmd.Wait()

	return nil
}

// cleanup removes temporary directories.
func (b *BitcoindMinerBackend) cleanup() error {
	if b.dataDir != "" {
		if err := os.RemoveAll(b.dataDir); err != nil {
			return fmt.Errorf("cannot remove data dir %s: %w",
				b.dataDir, err)
		}
	}

	return nil
}

// saveLogs copies the bitcoind log file.
func (b *BitcoindMinerBackend) saveLogs() {
	logFile := b.logPath + "/bitcoind.log"
	logDestination := fmt.Sprintf("%s/../%s", b.logPath, b.logFilename)

	err := node.CopyFile(logDestination, logFile)
	if err != nil {
		// Log error but don't fail.
		b.Logf("Unable to copy log file: %v", err)
	}

	err = os.RemoveAll(b.logPath)
	if err != nil {
		// Log error but don't fail.
		b.Logf("Cannot remove log dir %s: %v", b.logPath, err)
	}
}

// GetBestBlock returns the hash and height of the best block.
func (b *BitcoindMinerBackend) GetBestBlock() (*chainhash.Hash, int32, error) {
	// GetBestBlock is a btcd-specific method. For bitcoind, we need to use
	// GetBlockCount and GetBestBlockHash separately.
	hash, err := b.rpcClient.GetBestBlockHash()
	if err != nil {
		return nil, 0, err
	}

	height, err := b.rpcClient.GetBlockCount()
	if err != nil {
		return nil, 0, err
	}

	return hash, int32(height), nil
}

// GetRawMempool returns all transaction hashes in the mempool.
func (b *BitcoindMinerBackend) GetRawMempool() ([]*chainhash.Hash, error) {
	return b.rpcClient.GetRawMempool()
}

// Generate mines a specified number of blocks.
func (b *BitcoindMinerBackend) Generate(blocks uint32) ([]*chainhash.Hash,
	error) {

	// First create an address to mine to.
	addr, err := b.NewAddress()
	if err != nil {
		return nil, fmt.Errorf("unable to get new address: %w", err)
	}

	return b.rpcClient.GenerateToAddress(int64(blocks), addr, nil)
}

// GetBlock returns the block for the given block hash.
func (b *BitcoindMinerBackend) GetBlock(blockHash *chainhash.Hash) (
	*wire.MsgBlock, error) {

	return b.rpcClient.GetBlock(blockHash)
}

// GetRawTransaction returns the raw transaction for the given txid.
func (b *BitcoindMinerBackend) GetRawTransaction(txid *chainhash.Hash) (
	*btcutil.Tx, error) {

	return b.rpcClient.GetRawTransaction(txid)
}

// InvalidateBlock marks a block as invalid, triggering a reorg.
func (b *BitcoindMinerBackend) InvalidateBlock(
	blockHash *chainhash.Hash) error {

	_, err := b.rpcClient.RawRequest(
		"invalidateblock",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", blockHash.String())),
		},
	)

	return err
}

// NotifyNewTransactions registers for new transaction notifications. Bitcoind
// doesn't expose btcd-style tx notifications through the btcd rpcclient
// wrapper, so this is a no-op.
func (b *BitcoindMinerBackend) NotifyNewTransactions(_ bool) error {
	return nil
}

// SendOutputsWithoutChange creates and broadcasts a transaction with the given
// outputs using the specified fee rate.
func (b *BitcoindMinerBackend) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*chainhash.Hash, error) {

	tx, err := b.CreateTransaction(outputs, feeRate)
	if err != nil {
		return nil, err
	}

	return b.SendRawTransaction(tx, true)
}

// CreateTransaction creates a transaction with the given outputs.
func (b *BitcoindMinerBackend) CreateTransaction(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*wire.MsgTx, error) {

	// Extract destinations (address -> amount) in BTC.
	destinations := make(map[string]json.RawMessage, len(outputs))
	for _, output := range outputs {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			output.PkScript, HarnessNetParams,
		)
		if err != nil {
			return nil, fmt.Errorf("extract address: %w", err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no address in output")
		}

		destinations[addrs[0].String()] = []byte(
			btcStringFromSats(output.Value),
		)
	}

	destinationsJSON, err := json.Marshal(destinations)
	if err != nil {
		return nil, fmt.Errorf("marshal destinations: %w", err)
	}

	// 1) Create raw tx with no inputs.
	createResp, err := b.rpcClient.RawRequest(
		"createrawtransaction",
		[]json.RawMessage{
			[]byte(`[]`),
			destinationsJSON,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("createrawtransaction: %w", err)
	}

	var rawHex string
	if err := json.Unmarshal(createResp, &rawHex); err != nil {
		return nil, fmt.Errorf("parse createrawtransaction resp: %w",
			err)
	}

	// 2) Fund the tx using the miner wallet without broadcasting.
	//
	// The fee rate coming from lntest is in sat/kw. Bitcoind expects
	// BTC/kvB.
	//
	// sat/kw -> sat/kvB: multiply by 4 (1000 weight units is 250 vbytes).
	feeRateSatPerKVb := int64(feeRate) * 4
	minFeeRateBTCPerKVb := b.minRelayFeeBTCPerKVb()
	minFeeSatPerKVb := int64(math.Ceil(minFeeRateBTCPerKVb * 1e8))
	if feeRateSatPerKVb < minFeeSatPerKVb {
		feeRateSatPerKVb = minFeeSatPerKVb
	}

	feeRateOpt := btcStringFromSats(feeRateSatPerKVb)
	fundOpts, err := json.Marshal(map[string]interface{}{
		// Bitcoin Core supports two distinct fee rate options:
		// - fee_rate: sat/vB
		// - feeRate: BTC/kvB
		//
		// We use feeRate (BTC/kvB) because lntest uses btcutil.Amount
		// and we already clamp against getnetworkinfo/getmempoolinfo
		// which are expressed in BTC/kvB.
		"feeRate":        json.RawMessage(feeRateOpt),
		"changePosition": len(outputs),
		"lockUnspents":   true,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal fundrawtransaction opts: %w",
			err)
	}

	fundResp, err := b.rpcClient.RawRequest(
		"fundrawtransaction",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", rawHex)), fundOpts,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("fundrawtransaction (outputs=%s, "+
			"fee_rate=%s, changePosition=%d): %w",
			string(destinationsJSON), feeRateOpt, len(outputs), err)
	}

	var funded fundRawTransactionResp
	if err := json.Unmarshal(fundResp, &funded); err != nil {
		return nil, fmt.Errorf("parse fundrawtransaction resp: %w",
			err)
	}

	// 3) Sign the funded tx using the miner wallet.
	signResp, err := b.rpcClient.RawRequest(
		"signrawtransactionwithwallet",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", funded.Hex)),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("signrawtransactionwithwallet: %w", err)
	}

	var signed signRawTransactionResp
	if err := json.Unmarshal(signResp, &signed); err != nil {
		return nil, fmt.Errorf("parse signrawtransaction resp: %w",
			err)
	}
	if !signed.Complete {
		return nil, fmt.Errorf("signrawtransactionwithwallet " +
			"incomplete")
	}

	return txFromHex(signed.Hex)
}

// SendOutputs creates and broadcasts a transaction with the given outputs.
func (b *BitcoindMinerBackend) SendOutputs(outputs []*wire.TxOut,
	feeRate btcutil.Amount) (*chainhash.Hash, error) {

	return b.SendOutputsWithoutChange(outputs, feeRate)
}

// GenerateAndSubmitBlock generates a block with the given transactions.
func (b *BitcoindMinerBackend) GenerateAndSubmitBlock(txes []*btcutil.Tx,
	blockVersion int32, blockTime time.Time) (*btcutil.Block, error) {

	_ = blockVersion
	_ = blockTime

	// Generate a block that includes only the specified transactions.
	addr, err := b.NewAddress()
	if err != nil {
		return nil, fmt.Errorf("unable to get new address: %w", err)
	}

	// `generateblock` is available on Bitcoin Core regtest, and lets us
	// mine blocks without pulling in arbitrary mempool transactions.
	//
	// `generateblock` has existed in multiple forms across Bitcoin Core
	// versions. We try the following strategies, in order:
	//
	//  1. Pass raw tx hex strings (doesn't require mempool acceptance,
	//     avoids policy issues like RBF replacement checks).
	//  2. Pass txids after submitting to mempool.
	//  3. Fallback to `generatetoaddress`.
	var (
		resp           json.RawMessage
		generateErrHex error
		generateErrID  error
	)

	// Strategy 1: try `generateblock` with raw tx hex strings.
	rawTxs := make([]string, 0, len(txes))
	for _, tx := range txes {
		var buf bytes.Buffer
		if err := tx.MsgTx().Serialize(&buf); err != nil {
			return nil, fmt.Errorf("serialize tx %s: %w",
				tx.Hash(), err)
		}
		rawTxs = append(rawTxs, hex.EncodeToString(buf.Bytes()))
	}

	rawTxsJSON, err := json.Marshal(rawTxs)
	if err != nil {
		return nil, fmt.Errorf("marshal raw txs: %w", err)
	}

	resp, generateErrHex = b.rpcClient.RawRequest(
		"generateblock",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", addr.EncodeAddress())),
			rawTxsJSON,
		},
	)

	if generateErrHex != nil {
		// Strategy 2: submit to mempool, then try `generateblock` with
		// txids.
		txids := make([]string, 0, len(txes))
		for _, tx := range txes {
			txid := tx.Hash().String()
			txids = append(txids, txid)

			_, err := b.rpcClient.SendRawTransaction(
				tx.MsgTx(), true,
			)
			if err != nil {
				// Ignore already-in-mempool errors.
				if !strings.Contains(err.Error(), "already") &&
					!strings.Contains(
						err.Error(),
						"txn-already-known",
					) {

					return nil, fmt.Errorf("unable to "+
						"send tx %s to mempool: %w",
						txid, err)
				}
			}
		}

		txidsJSON, err := json.Marshal(txids)
		if err != nil {
			return nil, fmt.Errorf("marshal txids: %w", err)
		}

		resp, generateErrID = b.rpcClient.RawRequest(
			"generateblock",
			[]json.RawMessage{
				[]byte(fmt.Sprintf("%q", addr.EncodeAddress())),
				txidsJSON,
			},
		)
	}

	if generateErrHex != nil && generateErrID != nil {
		// Fall back to `generatetoaddress` for older bitcoind versions.
		//
		// Note: this fallback may include additional mempool
		// transactions.
		blockHashes, genErr := b.rpcClient.GenerateToAddress(
			1, addr, nil,
		)
		if genErr != nil {
			return nil, fmt.Errorf("generateblock (hex): %v; "+
				"generateblock (txid): %v; fallback "+
				"generatetoaddress: %v", generateErrHex,
				generateErrID, genErr)
		}
		if len(blockHashes) == 0 {
			return nil, fmt.Errorf("no block generated")
		}

		block, getErr := b.rpcClient.GetBlock(blockHashes[0])
		if getErr != nil {
			return nil, fmt.Errorf("unable to get generated "+
				"block: %w", getErr)
		}

		return btcutil.NewBlock(block), nil
	}

	// `generateblock` returns either a hash string or an object with a
	// `hash` field, depending on the version.
	var blockHashStr string
	if unmarshalErr := json.Unmarshal(
		resp, &blockHashStr,
	); unmarshalErr != nil {
		var respObj generateBlockResp
		if unmarshalErr2 := json.Unmarshal(
			resp, &respObj,
		); unmarshalErr2 != nil {
			return nil, fmt.Errorf("parse generateblock resp: "+
				"%v; %v", unmarshalErr, unmarshalErr2)
		}
		blockHashStr = respObj.Hash
	}

	blockHash, err := chainhash.NewHashFromStr(blockHashStr)
	if err != nil {
		return nil, fmt.Errorf("invalid generateblock hash %q: %w",
			blockHashStr, err)
	}

	block, err := b.rpcClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("unable to get generated block: %w", err)
	}

	return btcutil.NewBlock(block), nil
}

// NewAddress generates a new address.
func (b *BitcoindMinerBackend) NewAddress() (btcutil.Address, error) {
	// Use legacy addresses for compatibility with btcd.
	//
	// Params: label, address_type.
	addrResult, err := b.rpcClient.RawRequest(
		"getnewaddress", []json.RawMessage{
			[]byte(`""`),
			[]byte(`"legacy"`),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to get new address: %w", err)
	}

	var addrStr string
	if err := json.Unmarshal(addrResult, &addrStr); err != nil {
		return nil, fmt.Errorf("unable to parse address: %w", err)
	}

	return btcutil.DecodeAddress(addrStr, HarnessNetParams)
}

// P2PAddress returns the P2P address of the miner.
func (b *BitcoindMinerBackend) P2PAddress() string {
	return fmt.Sprintf("127.0.0.1:%d", b.p2pPort)
}

// Name returns the name of the backend implementation.
func (b *BitcoindMinerBackend) Name() string {
	return "bitcoind"
}

// ConnectMiner connects this miner to another node.
func (b *BitcoindMinerBackend) ConnectMiner(address string) error {
	// Use "onetry" so we don't persist peer connections across tests.
	_, err := b.rpcClient.RawRequest(
		"addnode",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", address)),
			[]byte(`"onetry"`),
		},
	)

	return err
}

// DisconnectMiner disconnects this miner from another node.
func (b *BitcoindMinerBackend) DisconnectMiner(address string) error {
	// `addnode remove` removes from the addnode list, but doesn't reliably
	// disconnect an existing connection. Use `disconnectnode` first.
	_, err := b.rpcClient.RawRequest(
		"disconnectnode",
		[]json.RawMessage{
			[]byte(fmt.Sprintf("%q", address)),
		},
	)
	if err != nil {
		return err
	}

	// Best-effort cleanup of any persistent addnode state. `ConnectMiner`
	// uses "onetry", so `addnode remove` can return an error if the peer
	// was never added to the addnode list.
	_ = b.rpcClient.AddNode(address, rpcclient.ANRemove)

	return nil
}

// SendRawTransaction sends a raw transaction to the network.
func (b *BitcoindMinerBackend) SendRawTransaction(tx *wire.MsgTx,
	allowHighFees bool) (*chainhash.Hash, error) {

	return b.rpcClient.SendRawTransaction(tx, allowHighFees)
}
