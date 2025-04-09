package unittest

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/port"
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
	// NetParams are the default network parameters for the tests.
	NetParams = &chaincfg.RegressionNetParams
)

// NewMiner spawns testing harness backed by a btcd node that can serve as a
// miner.
func NewMiner(t *testing.T, netParams *chaincfg.Params, extraArgs []string,
	createChain bool, spendableOutputs uint32) *rpctest.Harness {

	t.Helper()

	args := []string{
		"--nobanning",
		"--debuglevel=debug",
		fmt.Sprintf("--trickleinterval=%v", TrickleInterval),

		// Don't disconnect if a reply takes too long.
		"--nostalldetect",
	}
	extraArgs = append(extraArgs, args...)

	node, err := rpctest.New(netParams, nil, extraArgs, "")
	require.NoError(t, err, "unable to create backend node")
	t.Cleanup(func() {
		require.NoError(t, node.TearDown())
	})

	// We want to overwrite some of the connection settings to make the
	// tests more robust. We might need to restart the backend while there
	// are already blocks present, which will take a bit longer than the
	// 1 second the default settings amount to. Doubling both values will
	// give us retries up to 4 seconds.
	node.MaxConnRetries = rpctest.DefaultMaxConnectionRetries * 2
	node.ConnectionRetryTimeout = rpctest.DefaultConnectionRetryTimeout * 2

	if err := node.SetUp(createChain, spendableOutputs); err != nil {
		t.Fatalf("unable to set up backend node: %v", err)
	}

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate.
	numBlocks := netParams.MinerConfirmationWindow*2 + 17
	_, err = node.Client.Generate(numBlocks)
	require.NoError(t, err, "failed to generate blocks")

	return node
}

// NewBitcoindBackend spawns a new bitcoind node that connects to a miner at the
// specified address. The txindex boolean can be set to determine whether the
// backend node should maintain a transaction index. The rpcpolling boolean
// can be set to determine whether bitcoind's RPC polling interface should be
// used for block and tx notifications or if its ZMQ interface should be used.
// A connection to the newly spawned bitcoind node is returned once the bitcoind
// is synced to the miner's best height.
func NewBitcoindBackend(t *testing.T, netParams *chaincfg.Params,
	miner *rpctest.Harness, txindex, rpcpolling bool) *chain.BitcoindConn {

	t.Helper()

	tempBitcoindDir := t.TempDir()

	rpcPort := port.NextAvailablePort()
	torBindPort := port.NextAvailablePort()
	zmqBlockPort := port.NextAvailablePort()
	zmqTxPort := port.NextAvailablePort()
	zmqBlockHost := fmt.Sprintf("tcp://127.0.0.1:%d", zmqBlockPort)
	zmqTxHost := fmt.Sprintf("tcp://127.0.0.1:%d", zmqTxPort)

	// TODO(yy): Make this configurable via `chain.BitcoindConfig` and
	// replace the default P2P port when set.
	p2pPort := port.NextAvailablePort()
	netParams.DefaultPort = fmt.Sprintf("%d", p2pPort)

	args := []string{
		"-datadir=" + tempBitcoindDir,
		"-regtest",
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6fd$507c670e800a95" +
			"284294edb5773b05544b220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-bind=127.0.0.1:%d=onion", torBindPort),
		fmt.Sprintf("-port=%d", p2pPort),
		"-disablewallet",
		"-zmqpubrawblock=" + zmqBlockHost,
		"-zmqpubrawtx=" + zmqTxHost,

		// whitelist localhost to speed up relay.
		"-whitelist=127.0.0.1",

		// Disable v2 transport as btcd doesn't support it yet.
		//
		// TODO(yy): Remove this line once v2 conn is supported in
		// `btcd`.
		"-v2transport=0",
	}
	if txindex {
		args = append(args, "-txindex")
	}

	bitcoind := exec.Command("bitcoind", args...)
	err := bitcoind.Start()
	require.NoError(t, err, "unable to start bitcoind")

	t.Cleanup(func() {
		// Kill `bitcoind` and assert there's no error.
		err = bitcoind.Process.Kill()
		require.NoError(t, err)

		err = bitcoind.Wait()
		if strings.Contains(err.Error(), "signal: killed") {
			return
		}

		require.NoError(t, err)
	})

	// Wait for the bitcoind instance to start up.
	time.Sleep(time.Second)

	host := fmt.Sprintf("127.0.0.1:%d", rpcPort)
	cfg := &chain.BitcoindConfig{
		ChainParams: netParams,
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
		t.Fatalf("unable to establish connection to bitcoind at %v: "+
			"%v", tempBitcoindDir, err)
	}
	t.Cleanup(conn.Stop)

	// Assert that the connection with the miner is made.
	//
	// Create a new RPC client.
	rpcCfg := rpcclient.ConnConfig{
		Host:                 cfg.Host,
		User:                 cfg.User,
		Pass:                 cfg.Pass,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}

	rpcClient, err := rpcclient.New(&rpcCfg, nil)
	require.NoError(t, err, "failed to create RPC client")

	// Connect to the miner node.
	err = rpcClient.AddNode(miner.P2PAddress(), rpcclient.ANAdd)
	require.NoError(t, err, "failed to connect to miner")

	// Get the network info and assert the num of outbound connections is 1.
	err = wait.NoError(func() error {
		result, err := rpcClient.GetNetworkInfo()
		require.NoError(t, err)

		if int(result.Connections) != 1 {
			return fmt.Errorf("want 1 conn, got %d",
				result.Connections)
		}

		if int(result.ConnectionsOut) != 1 {
			return fmt.Errorf("want 1 outbound conn, got %d",
				result.Connections)
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(t, err, "timeout connecting to the miner")

	// Assert the chain backend is synced to the miner.
	syncBitcoindWithMiner(t, rpcClient, miner, p2pPort)

	// Tear down the rpc client.
	rpcClient.Shutdown()

	return conn
}

// syncBitcoindWithMiner waits until the bitcoind node is synced with the miner.
func syncBitcoindWithMiner(t *testing.T, notifier *rpcclient.Client,
	miner *rpctest.Harness, p2pPort int) uint32 {

	_, minerHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve miner's current height")

	timeout := time.After(10 * time.Second)
	for {
		info, err := notifier.GetBlockChainInfo()
		require.NoError(t, err)

		bitcoindHeight := info.Blocks

		t.Logf("miner height=%v, bitcoind height=%v", minerHeight,
			bitcoindHeight)

		if bitcoindHeight == minerHeight {
			return uint32(bitcoindHeight)
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			t.Fatalf("timed out in syncNotifierWithMiner, got "+
				"err=%v, minerHeight=%v, bitcoindHeight=%v",
				err, minerHeight, bitcoindHeight)
		}

		// Get the num of connections the miner has. We expect it to
		// have at least one connection with the chain backend.
		count, err := miner.Client.GetConnectionCount()
		require.NoError(t, err)
		if count != 0 {
			continue
		}

		// Reconnect the miner and the chain backend.
		//
		// NOTE: The connection should have been made before we perform
		// the `syncNotifierWithMiner`. However, due unknown reason, the
		// miner may refuse to process the inbound connection made by
		// the bitcoind node, causing the connection to fail. It's
		// possible there's a bug in the handshake between the two
		// nodes.
		//
		// A normal flow is, bitcoind starts a v2 handshake flow, which
		// btcd will fail and disconnect. Upon seeing this
		// disconnection, bitcoind will try a v1 handshake and succeeds.
		// The failed flow is, upon seeing the v2 handshake, btcd
		// doesn't seem to perform the disconnect. Instead an EOF
		// websocket error is found.
		//
		// TODO(yy): Fix the above bug in `btcd`. This can be reproduced
		// using `make flakehunter-unit pkg=$pkg case=$case`, with,
		// `case=TestHistoricalConfDetailsNoTxIndex/rpc_polling_enabled`
		// `pkg=chainntnfs/bitcoindnotify`.
		// Also need to modify the temp dir logic so we can save the
		// debug logs.
		// This bug is likely to be fixed when we implement the
		// encrypted p2p conn, or when we properly fix the shutdown
		// issues in all our RPC conns.
		t.Log("Expected to the chain backend to have one conn with " +
			"the miner, instead it's disconnected!")

		// We now ask the miner to add the chain backend back.
		host := fmt.Sprintf("127.0.0.1:%d", p2pPort)

		// NOTE:AddNode must take a host that has the format
		// `host:port`, otherwise the default port will be used. Check
		// `normalizeAddress` in btcd for details.
		err = miner.Client.AddNode(host, rpcclient.ANAdd)
		require.NoError(t, err, "Failed to connect miner to the chain "+
			"backend")
	}
}

// NewNeutrinoBackend spawns a new neutrino node that connects to a miner at
// the specified address.
func NewNeutrinoBackend(t *testing.T, netParams *chaincfg.Params,
	minerAddr string) *neutrino.ChainService {

	t.Helper()

	spvDir := t.TempDir()

	dbName := filepath.Join(spvDir, "neutrino.db")
	spvDatabase, err := walletdb.Create(
		kvdb.BoltBackendName, dbName, true, kvdb.DefaultDBTimeout,
		false,
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
		ChainParams:  *netParams,
		ConnectPeers: []string{minerAddr},
	}
	spvNode, err := neutrino.NewChainService(spvConfig)
	if err != nil {
		t.Fatalf("unable to create neutrino: %v", err)
	}

	// We'll also wait for the instance to sync up fully to the chain
	// generated by the btcd instance.
	_ = spvNode.Start()
	for !spvNode.IsCurrent() {
		time.Sleep(time.Millisecond * 100)
	}
	t.Cleanup(func() {
		_ = spvNode.Stop()
	})

	return spvNode
}

func init() {
	// Before we start any node, we need to make sure that any btcd or
	// bitcoind node that is started through the RPC harness uses a unique
	// port as well to avoid any port collisions.
	rpctest.ListenAddressGenerator =
		port.GenerateSystemUniqueListenerAddresses
}
