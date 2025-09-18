package lntest

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
)

const (
	// defaultMinerFeeRate specifies the fee rate in sats when sending
	// outputs from the miner.
	defaultMinerFeeRate = 7500

	// numBlocksSendOutput specifies the number of blocks to mine after
	// sending outputs from the miner.
	numBlocksSendOutput = 2

	// numBlocksOpenChannel specifies the number of blocks mined when
	// opening a channel.
	numBlocksOpenChannel = 6

	// lndErrorChanSize specifies the buffer size used to receive errors
	// from lnd process.
	lndErrorChanSize = 10

	// maxBlocksAllowed specifies the max allowed value to be used when
	// mining blocks.
	maxBlocksAllowed = 100

	finalCltvDelta  = routing.MinCLTVDelta // 18.
	thawHeightDelta = finalCltvDelta * 2   // 36.
)

var (
	// MaxBlocksMinedPerTest is the maximum number of blocks that we allow
	// a test to mine. This is an exported global variable so it can be
	// overwritten by other projects that don't have the same constraints.
	MaxBlocksMinedPerTest = 50
)

// TestCase defines a test case that's been used in the integration test.
type TestCase struct {
	// Name specifies the test name.
	Name string

	// TestFunc is the test case wrapped in a function.
	TestFunc func(t *HarnessTest)
}

// HarnessTest builds on top of a testing.T with enhanced error detection. It
// is responsible for managing the interactions among different nodes, and
// providing easy-to-use assertions.
type HarnessTest struct {
	*testing.T

	// miner is a reference to a running full node that can be used to
	// create new blocks on the network.
	miner *miner.HarnessMiner

	// manager handles the start and stop of a given node.
	manager *nodeManager

	// feeService is a web service that provides external fee estimates to
	// lnd.
	feeService WebFeeService

	// Channel for transmitting stderr output from failed lightning node
	// to main process.
	lndErrorChan chan error

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context //nolint:containedctx
	cancel context.CancelFunc

	// stopChainBackend points to the cleanup function returned by the
	// chainBackend.
	stopChainBackend func()

	// cleaned specifies whether the cleanup has been applied for the
	// current HarnessTest.
	cleaned bool

	// currentHeight is the current height of the chain backend.
	currentHeight uint32
}

// harnessOpts contains functional option to modify the behavior of the various
// harness calls.
type harnessOpts struct {
	useAMP bool
}

// defaultHarnessOpts returns a new instance of the harnessOpts with default
// values specified.
func defaultHarnessOpts() harnessOpts {
	return harnessOpts{
		useAMP: false,
	}
}

// HarnessOpt is a functional option that can be used to modify the behavior of
// harness functionality.
type HarnessOpt func(*harnessOpts)

// WithAMP is a functional option that can be used to enable the AMP feature
// for sending payments.
func WithAMP() HarnessOpt {
	return func(h *harnessOpts) {
		h.useAMP = true
	}
}

// NewHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func NewHarnessTest(t *testing.T, lndBinary string, feeService WebFeeService,
	dbBackend node.DatabaseBackend, nativeSQL bool) *HarnessTest {

	t.Helper()

	// Create the run context.
	ctxt, cancel := context.WithCancel(t.Context())

	manager := newNodeManager(lndBinary, dbBackend, nativeSQL)

	return &HarnessTest{
		T:          t,
		manager:    manager,
		feeService: feeService,
		runCtx:     ctxt,
		cancel:     cancel,
		// We need to use buffered channel here as we don't want to
		// block sending errors.
		lndErrorChan: make(chan error, lndErrorChanSize),
	}
}

// Start will assemble the chain backend and the miner for the HarnessTest. It
// also starts the fee service and watches lnd process error.
func (h *HarnessTest) Start(chain node.BackendConfig,
	miner *miner.HarnessMiner) {

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// case should naturally as a result and we log the server error here
	// to help debug.
	go func() {
		select {
		case err, more := <-h.lndErrorChan:
			if !more {
				return
			}
			h.Logf("lnd finished with error (stderr):\n%v", err)

		case <-h.runCtx.Done():
			return
		}
	}()

	// Start the fee service.
	err := h.feeService.Start()
	require.NoError(h, err, "failed to start fee service")

	// Assemble the node manager with chainBackend and feeServiceURL.
	h.manager.chainBackend = chain
	h.manager.feeServiceURL = h.feeService.URL()

	// Assemble the miner.
	h.miner = miner

	// Update block height.
	h.updateCurrentHeight()
}

// ChainBackendName returns the chain backend name used in the test.
func (h *HarnessTest) ChainBackendName() string {
	return h.manager.chainBackend.Name()
}

// Context returns the run context used in this test. Usaually it should be
// managed by the test itself otherwise undefined behaviors will occur. It can
// be used, however, when a test needs to have its own context being managed
// differently. In that case, instead of using a background context, the run
// context should be used such that the test context scope can be fully
// controlled.
func (h *HarnessTest) Context() context.Context {
	return h.runCtx
}

// setupWatchOnlyNode initializes a node with the watch-only accounts of an
// associated remote signing instance.
func (h *HarnessTest) setupWatchOnlyNode(name string,
	signerNode *node.HarnessNode, password []byte) *node.HarnessNode {

	// Prepare arguments for watch-only node connected to the remote signer.
	remoteSignerArgs := []string{
		"--remotesigner.enable",
		fmt.Sprintf("--remotesigner.rpchost=localhost:%d",
			signerNode.Cfg.RPCPort),
		fmt.Sprintf("--remotesigner.tlscertpath=%s",
			signerNode.Cfg.TLSCertPath),
		fmt.Sprintf("--remotesigner.macaroonpath=%s",
			signerNode.Cfg.AdminMacPath),
	}

	// Fetch watch-only accounts from the signer node.
	resp := signerNode.RPC.ListAccounts(&walletrpc.ListAccountsRequest{})
	watchOnlyAccounts, err := walletrpc.AccountsToWatchOnly(resp.Accounts)
	require.NoErrorf(h, err, "unable to find watch only accounts for %s",
		name)

	// Create a new watch-only node with remote signer configuration.
	return h.NewNodeRemoteSigner(
		name, remoteSignerArgs, password,
		&lnrpc.WatchOnly{
			MasterKeyBirthdayTimestamp: 0,
			MasterKeyFingerprint:       nil,
			Accounts:                   watchOnlyAccounts,
		},
	)
}

// createAndSendOutput send amt satoshis from the internal mining node to the
// targeted lightning node using a P2WKH address. No blocks are mined so
// transactions will sit unconfirmed in mempool.
func (h *HarnessTest) createAndSendOutput(target *node.HarnessNode,
	amt btcutil.Amount, addrType lnrpc.AddressType) {

	req := &lnrpc.NewAddressRequest{Type: addrType}
	resp := target.RPC.NewAddress(req)
	addr := h.DecodeAddress(resp.Address)
	addrScript := h.PayToAddrScript(addr)

	output := &wire.TxOut{
		PkScript: addrScript,
		Value:    int64(amt),
	}
	h.miner.SendOutput(output, defaultMinerFeeRate)
}

// Stop stops the test harness.
func (h *HarnessTest) Stop() {
	// Do nothing if it's not started.
	if h.runCtx == nil {
		h.Log("HarnessTest is not started")
		return
	}

	h.shutdownAllNodes()

	close(h.lndErrorChan)

	// Stop the fee service.
	err := h.feeService.Stop()
	require.NoError(h, err, "failed to stop fee service")

	// Stop the chainBackend.
	h.stopChainBackend()

	// Stop the miner.
	h.miner.Stop()
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *HarnessTest) RunTestCase(testCase *TestCase) {
	defer func() {
		if r := recover(); r != nil {
			// Wrap the recovered panic in an error.
			var err error
			switch v := r.(type) {
			case error:
				err = v
			default:
				err = fmt.Errorf("%v", v)
			}

			// Capture and print the stack trace.
			stack := debug.Stack()

			// Fail the test with panic info and stack.
			h.Fatalf("Failed: (%v) panic with: %v\n%s",
				testCase.Name, err, stack)
		}
	}()

	testCase.TestFunc(h)
}

// Subtest creates a child HarnessTest, which inherits the harness net and
// stand by nodes created by the parent test. It will return a cleanup function
// which resets  all the standby nodes' configs back to its original state and
// create snapshots of each nodes' internal state.
func (h *HarnessTest) Subtest(t *testing.T) *HarnessTest {
	t.Helper()

	st := &HarnessTest{
		T:            t,
		manager:      h.manager,
		miner:        h.miner,
		feeService:   h.feeService,
		lndErrorChan: make(chan error, lndErrorChanSize),
	}

	// Inherit context from the main test.
	st.runCtx, st.cancel = context.WithCancel(h.runCtx)

	// Inherit the subtest for the miner.
	st.miner.T = st.T

	// Reset fee estimator.
	st.feeService.Reset()

	// Record block height.
	h.updateCurrentHeight()
	startHeight := int32(h.CurrentHeight())

	st.Cleanup(func() {
		// Make sure the test is not consuming too many blocks.
		st.checkAndLimitBlocksMined(startHeight)

		// Don't bother run the cleanups if the test is failed.
		if st.Failed() {
			st.Log("test failed, skipped cleanup")
			st.shutdownNodesNoAssert()
			return
		}

		// Don't run cleanup if it's already done. This can happen if
		// we have multiple level inheritance of the parent harness
		// test. For instance, a `Subtest(st)`.
		if st.cleaned {
			st.Log("test already cleaned, skipped cleanup")
			return
		}

		// If found running nodes, shut them down.
		st.shutdownAllNodes()

		// We require the mempool to be cleaned from the test.
		require.Empty(st, st.miner.GetRawMempool(), "mempool not "+
			"cleaned, please mine blocks to clean them all.")

		// Finally, cancel the run context. We have to do it here
		// because we need to keep the context alive for the above
		// assertions used in cleanup.
		st.cancel()

		// We now want to mark the parent harness as cleaned to avoid
		// running cleanup again since its internal state has been
		// cleaned up by its child harness tests.
		h.cleaned = true
	})

	return st
}

// checkAndLimitBlocksMined asserts that the blocks mined in a single test
// doesn't exceed 50, which implicitly discourage table-drive tests, which are
// hard to maintain and take a long time to run.
func (h *HarnessTest) checkAndLimitBlocksMined(startHeight int32) {
	_, endHeight := h.GetBestBlock()
	blocksMined := endHeight - startHeight

	h.Logf("finished test: %s, start height=%d, end height=%d, mined "+
		"blocks=%d", h.manager.currentTestCase, startHeight, endHeight,
		blocksMined)

	// If the number of blocks is less than 40, we consider the test
	// healthy.
	if blocksMined < 40 {
		return
	}

	// Otherwise log a warning if it's mining more than 40 blocks.
	desc := "!============================================!\n"

	desc += fmt.Sprintf("Too many blocks (%v) mined in one test! Tips:\n",
		blocksMined)

	desc += "1. break test into smaller individual tests, especially if " +
		"this is a table-drive test.\n" +
		"2. use smaller CSV via `--bitcoin.defaultremotedelay=1.`\n" +
		"3. use smaller CLTV via `--bitcoin.timelockdelta=18.`\n" +
		"4. remove unnecessary CloseChannel when test ends.\n" +
		"5. use `CreateSimpleNetwork` for efficient channel creation.\n"
	h.Log(desc)

	// We enforce that the test should not mine more than
	// MaxBlocksMinedPerTest (50 by default) blocks, which is more than
	// enough to test a multi hop force close scenario.
	require.LessOrEqualf(
		h, int(blocksMined), MaxBlocksMinedPerTest,
		"cannot mine more than %d blocks in one test",
		MaxBlocksMinedPerTest,
	)
}

// shutdownNodesNoAssert will shutdown all running nodes without assertions.
// This is used when the test has already failed, we don't want to log more
// errors but focusing on the original error.
func (h *HarnessTest) shutdownNodesNoAssert() {
	for _, node := range h.manager.activeNodes {
		_ = h.manager.shutdownNode(node)
	}
}

// shutdownAllNodes will shutdown all running nodes.
func (h *HarnessTest) shutdownAllNodes() {
	var lastErr error
	for _, node := range h.manager.activeNodes {
		err := h.manager.shutdownNode(node)
		if err == nil {
			continue
		}

		// Instead of returning the error, we will log it instead. This
		// is needed so other nodes can continue their shutdown
		// processes.
		h.Logf("unable to shutdown %s, got err: %v", node.Name(), err)

		lastErr = err
	}

	require.NoError(h, lastErr, "failed to shutdown all nodes")
}

// cleanupStandbyNode is a function should be called with defer whenever a
// subtest is created. It will reset the standby nodes configs, snapshot the
// states, and validate the node has a clean state.
func (h *HarnessTest) cleanupStandbyNode(hn *node.HarnessNode) {
	// Remove connections made from this test.
	h.removeConnectionns(hn)

	// Delete all payments made from this test.
	hn.RPC.DeleteAllPayments()

	// Check the node's current state with timeout.
	//
	// NOTE: we need to do this in a `wait` because it takes some time for
	// the node to update its internal state. Once the RPCs are synced we
	// can then remove this wait.
	err := wait.NoError(func() error {
		// Update the node's internal state.
		hn.UpdateState()

		// Check the node is in a clean state for the following tests.
		return h.validateNodeState(hn)
	}, wait.DefaultTimeout)
	require.NoError(h, err, "timeout checking node's state")
}

// removeConnectionns will remove all connections made on the standby nodes
// expect the connections between Alice and Bob.
func (h *HarnessTest) removeConnectionns(hn *node.HarnessNode) {
	resp := hn.RPC.ListPeers()
	for _, peer := range resp.Peers {
		hn.RPC.DisconnectPeer(peer.PubKey)
	}
}

// SetTestName set the test case name.
func (h *HarnessTest) SetTestName(name string) {
	cleanTestCaseName := strings.ReplaceAll(name, " ", "_")
	h.manager.currentTestCase = cleanTestCaseName
}

// NewNode creates a new node and asserts its creation. The node is guaranteed
// to have finished its initialization and all its subservers are started.
func (h *HarnessTest) NewNode(name string,
	extraArgs []string) *node.HarnessNode {

	node, err := h.manager.newNode(h.T, name, extraArgs, nil, false)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	// Start the node.
	err = node.Start(h.runCtx)
	require.NoError(h, err, "failed to start node %s", node.Name())

	// Get the miner's best block hash.
	bestBlock, err := h.miner.Client.GetBestBlockHash()
	require.NoError(h, err, "unable to get best block hash")

	// Wait until the node's chain backend is synced to the miner's best
	// block.
	h.WaitForBlockchainSyncTo(node, *bestBlock)

	return node
}

// NewNodeWithCoins creates a new node and asserts its creation. The node is
// guaranteed to have finished its initialization and all its subservers are
// started. In addition, 5 UTXO of 1 BTC each are sent to the node.
func (h *HarnessTest) NewNodeWithCoins(name string,
	extraArgs []string) *node.HarnessNode {

	node := h.NewNode(name, extraArgs)

	// Load up the wallets of the node with 5 outputs of 1 BTC each.
	const (
		numOutputs  = 5
		fundAmount  = 1 * btcutil.SatoshiPerBitcoin
		totalAmount = fundAmount * numOutputs
	)

	for i := 0; i < numOutputs; i++ {
		h.createAndSendOutput(
			node, fundAmount,
			lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		)
	}

	// Mine a block to confirm the transactions.
	h.MineBlocksAndAssertNumTxes(1, numOutputs)

	// Now block until the wallet have fully synced up.
	h.WaitForBalanceConfirmed(node, totalAmount)

	return node
}

// Shutdown shuts down the given node and asserts that no errors occur.
func (h *HarnessTest) Shutdown(node *node.HarnessNode) {
	err := h.manager.shutdownNode(node)
	require.NoErrorf(h, err, "unable to shutdown %v in %v", node.Name(),
		h.manager.currentTestCase)
}

// SuspendNode stops the given node and returns a callback that can be used to
// start it again.
func (h *HarnessTest) SuspendNode(node *node.HarnessNode) func() error {
	err := node.Stop()
	require.NoErrorf(h, err, "failed to stop %s", node.Name())

	// Remove the node from active nodes.
	delete(h.manager.activeNodes, node.Cfg.NodeID)

	return func() error {
		h.manager.registerNode(node)

		if err := node.Start(h.runCtx); err != nil {
			return err
		}
		h.WaitForBlockchainSync(node)

		return nil
	}
}

// RestartNode restarts a given node, unlocks it and asserts it's successfully
// started.
func (h *HarnessTest) RestartNode(hn *node.HarnessNode) {
	err := h.manager.restartNode(h.runCtx, hn, nil)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())

	err = h.manager.unlockNode(hn)
	require.NoErrorf(h, err, "failed to unlock node %s", hn.Name())

	if !hn.Cfg.SkipUnlock {
		// Give the node some time to catch up with the chain before we
		// continue with the tests.
		h.WaitForBlockchainSync(hn)
	}
}

// RestartNodeNoUnlock restarts a given node without unlocking its wallet.
func (h *HarnessTest) RestartNodeNoUnlock(hn *node.HarnessNode) {
	err := h.manager.restartNode(h.runCtx, hn, nil)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// RestartNodeWithChanBackups restarts a given node with the specified channel
// backups.
func (h *HarnessTest) RestartNodeWithChanBackups(hn *node.HarnessNode,
	chanBackups ...*lnrpc.ChanBackupSnapshot) {

	err := h.manager.restartNode(h.runCtx, hn, nil)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())

	err = h.manager.unlockNode(hn, chanBackups...)
	require.NoErrorf(h, err, "failed to unlock node %s", hn.Name())

	// Give the node some time to catch up with the chain before we
	// continue with the tests.
	h.WaitForBlockchainSync(hn)
}

// RestartNodeWithExtraArgs updates the node's config and restarts it.
func (h *HarnessTest) RestartNodeWithExtraArgs(hn *node.HarnessNode,
	extraArgs []string) {

	hn.SetExtraArgs(extraArgs)
	h.RestartNode(hn)
}

// NewNodeWithSeed fully initializes a new HarnessNode after creating a fresh
// aezeed. The provided password is used as both the aezeed password and the
// wallet password. The generated mnemonic is returned along with the
// initialized harness node.
func (h *HarnessTest) NewNodeWithSeed(name string,
	extraArgs []string, password []byte,
	statelessInit bool) (*node.HarnessNode, []string, []byte) {

	// Create a request to generate a new aezeed. The new seed will have
	// the same password as the internal wallet.
	req := &lnrpc.GenSeedRequest{
		AezeedPassphrase: password,
		SeedEntropy:      nil,
	}

	return h.newNodeWithSeed(name, extraArgs, req, statelessInit)
}

// newNodeWithSeed creates and initializes a new HarnessNode such that it'll be
// ready to accept RPC calls. A `GenSeedRequest` is needed to generate the
// seed.
func (h *HarnessTest) newNodeWithSeed(name string,
	extraArgs []string, req *lnrpc.GenSeedRequest,
	statelessInit bool) (*node.HarnessNode, []string, []byte) {

	node, err := h.manager.newNode(
		h.T, name, extraArgs, req.AezeedPassphrase, true,
	)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	// Start the node with seed only, which will only create the `State`
	// and `WalletUnlocker` clients.
	err = node.StartWithNoAuth(h.runCtx)
	require.NoErrorf(h, err, "failed to start node %s", node.Name())

	// Generate a new seed.
	genSeedResp := node.RPC.GenSeed(req)

	// With the seed created, construct the init request to the node,
	// including the newly generated seed.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     req.AezeedPassphrase,
		CipherSeedMnemonic: genSeedResp.CipherSeedMnemonic,
		AezeedPassphrase:   req.AezeedPassphrase,
		StatelessInit:      statelessInit,
	}

	// Pass the init request via rpc to finish unlocking the node. This
	// will also initialize the macaroon-authenticated LightningClient.
	adminMac, err := h.manager.initWalletAndNode(node, initReq)
	require.NoErrorf(h, err, "failed to unlock and init node %s",
		node.Name())

	// In stateless initialization mode we get a macaroon back that we have
	// to return to the test, otherwise gRPC calls won't be possible since
	// there are no macaroon files created in that mode.
	// In stateful init the admin macaroon will just be nil.
	return node, genSeedResp.CipherSeedMnemonic, adminMac
}

// RestoreNodeWithSeed fully initializes a HarnessNode using a chosen mnemonic,
// password, recovery window, and optionally a set of static channel backups.
// After providing the initialization request to unlock the node, this method
// will finish initializing the LightningClient such that the HarnessNode can
// be used for regular rpc operations.
func (h *HarnessTest) RestoreNodeWithSeed(name string, extraArgs []string,
	password []byte, mnemonic []string, rootKey string,
	recoveryWindow int32,
	chanBackups *lnrpc.ChanBackupSnapshot) *node.HarnessNode {

	n, err := h.manager.newNode(h.T, name, extraArgs, password, true)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	// Start the node with seed only, which will only create the `State`
	// and `WalletUnlocker` clients.
	err = n.StartWithNoAuth(h.runCtx)
	require.NoErrorf(h, err, "failed to start node %s", n.Name())

	// Create the wallet.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic,
		AezeedPassphrase:   password,
		ExtendedMasterKey:  rootKey,
		RecoveryWindow:     recoveryWindow,
		ChannelBackups:     chanBackups,
	}
	_, err = h.manager.initWalletAndNode(n, initReq)
	require.NoErrorf(h, err, "failed to unlock and init node %s",
		n.Name())

	return n
}

// NewNodeEtcd starts a new node with seed that'll use an external etcd
// database as its storage. The passed cluster flag indicates that we'd like
// the node to join the cluster leader election. We won't wait until RPC is
// available (this is useful when the node is not expected to become the leader
// right away).
func (h *HarnessTest) NewNodeEtcd(name string, etcdCfg *etcd.Config,
	password []byte, cluster bool,
	leaderSessionTTL int) *node.HarnessNode {

	// We don't want to use the embedded etcd instance.
	h.manager.dbBackend = node.BackendBbolt

	extraArgs := node.ExtraArgsEtcd(
		etcdCfg, name, cluster, leaderSessionTTL,
	)
	node, err := h.manager.newNode(h.T, name, extraArgs, password, true)
	require.NoError(h, err, "failed to create new node with etcd")

	// Start the node daemon only.
	err = node.StartLndCmd(h.runCtx)
	require.NoError(h, err, "failed to start node %s", node.Name())

	return node
}

// NewNodeWithSeedEtcd starts a new node with seed that'll use an external etcd
// database as its storage. The passed cluster flag indicates that we'd like
// the node to join the cluster leader election.
func (h *HarnessTest) NewNodeWithSeedEtcd(name string, etcdCfg *etcd.Config,
	password []byte, statelessInit, cluster bool,
	leaderSessionTTL int) (*node.HarnessNode, []string, []byte) {

	// We don't want to use the embedded etcd instance.
	h.manager.dbBackend = node.BackendBbolt

	// Create a request to generate a new aezeed. The new seed will have
	// the same password as the internal wallet.
	req := &lnrpc.GenSeedRequest{
		AezeedPassphrase: password,
		SeedEntropy:      nil,
	}

	extraArgs := node.ExtraArgsEtcd(
		etcdCfg, name, cluster, leaderSessionTTL,
	)

	return h.newNodeWithSeed(name, extraArgs, req, statelessInit)
}

// NewNodeRemoteSigner creates a new remote signer node and asserts its
// creation.
func (h *HarnessTest) NewNodeRemoteSigner(name string, extraArgs []string,
	password []byte, watchOnly *lnrpc.WatchOnly) *node.HarnessNode {

	hn, err := h.manager.newNode(h.T, name, extraArgs, password, true)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	err = hn.StartWithNoAuth(h.runCtx)
	require.NoError(h, err, "failed to start node %s", name)

	// With the seed created, construct the init request to the node,
	// including the newly generated seed.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword: password,
		WatchOnly:      watchOnly,
	}

	// Pass the init request via rpc to finish unlocking the node. This
	// will also initialize the macaroon-authenticated LightningClient.
	_, err = h.manager.initWalletAndNode(hn, initReq)
	require.NoErrorf(h, err, "failed to init node %s", name)

	return hn
}

// KillNode kills the node and waits for the node process to stop.
func (h *HarnessTest) KillNode(hn *node.HarnessNode) {
	delete(h.manager.activeNodes, hn.Cfg.NodeID)

	h.Logf("Manually killing the node %s", hn.Name())
	require.NoErrorf(h, hn.KillAndWait(), "%s: kill got error", hn.Name())
}

// SetFeeEstimate sets a fee rate to be returned from fee estimator.
//
// NOTE: this method will set the fee rate for a conf target of 1, which is the
// fallback fee rate for a `WebAPIEstimator` if a higher conf target's fee rate
// is not set. This means if the fee rate for conf target 6 is set, the fee
// estimator will use that value instead.
func (h *HarnessTest) SetFeeEstimate(fee chainfee.SatPerKWeight) {
	h.feeService.SetFeeRate(fee, 1)
}

// SetFeeEstimateWithConf sets a fee rate of a specified conf target to be
// returned from fee estimator.
func (h *HarnessTest) SetFeeEstimateWithConf(
	fee chainfee.SatPerKWeight, conf uint32) {

	h.feeService.SetFeeRate(fee, conf)
}

// SetMinRelayFeerate sets a min relay fee rate to be returned from fee
// estimator.
func (h *HarnessTest) SetMinRelayFeerate(fee chainfee.SatPerKVByte) {
	h.feeService.SetMinRelayFeerate(fee)
}

// validateNodeState checks that the node doesn't have any uncleaned states
// which will affect its following tests.
func (h *HarnessTest) validateNodeState(hn *node.HarnessNode) error {
	errStr := func(subject string) error {
		return fmt.Errorf("%s: found %s channels, please close "+
			"them properly", hn.Name(), subject)
	}
	// If the node still has open channels, it's most likely that the
	// current test didn't close it properly.
	if hn.State.OpenChannel.Active != 0 {
		return errStr("active")
	}
	if hn.State.OpenChannel.Public != 0 {
		return errStr("public")
	}
	if hn.State.OpenChannel.Private != 0 {
		return errStr("private")
	}
	if hn.State.OpenChannel.Pending != 0 {
		return errStr("pending open")
	}

	// The number of pending force close channels should be zero.
	if hn.State.CloseChannel.PendingForceClose != 0 {
		return errStr("pending force")
	}

	// The number of waiting close channels should be zero.
	if hn.State.CloseChannel.WaitingClose != 0 {
		return errStr("waiting close")
	}

	// Ths number of payments should be zero.
	if hn.State.Payment.Total != 0 {
		return fmt.Errorf("%s: found uncleaned payments, please "+
			"delete all of them properly", hn.Name())
	}

	// The number of public edges should be zero.
	if hn.State.Edge.Public != 0 {
		return fmt.Errorf("%s: found active public egdes, please "+
			"clean them properly", hn.Name())
	}

	// The number of edges should be zero.
	if hn.State.Edge.Total != 0 {
		return fmt.Errorf("%s: found active edges, please "+
			"clean them properly", hn.Name())
	}

	return nil
}

// GetChanPointFundingTxid takes a channel point and converts it into a chain
// hash.
func (h *HarnessTest) GetChanPointFundingTxid(
	cp *lnrpc.ChannelPoint) chainhash.Hash {

	txid, err := lnrpc.GetChanPointFundingTxid(cp)
	require.NoError(h, err, "unable to get txid")

	return *txid
}

// OutPointFromChannelPoint creates an outpoint from a given channel point.
func (h *HarnessTest) OutPointFromChannelPoint(
	cp *lnrpc.ChannelPoint) wire.OutPoint {

	txid := h.GetChanPointFundingTxid(cp)
	return wire.OutPoint{
		Hash:  txid,
		Index: cp.OutputIndex,
	}
}

// OpenChannelParams houses the params to specify when opening a new channel.
type OpenChannelParams struct {
	// Amt is the local amount being put into the channel.
	Amt btcutil.Amount

	// PushAmt is the amount that should be pushed to the remote when the
	// channel is opened.
	PushAmt btcutil.Amount

	// Private is a boolan indicating whether the opened channel should be
	// private.
	Private bool

	// SpendUnconfirmed is a boolean indicating whether we can utilize
	// unconfirmed outputs to fund the channel.
	SpendUnconfirmed bool

	// MinHtlc is the htlc_minimum_msat value set when opening the channel.
	MinHtlc lnwire.MilliSatoshi

	// RemoteMaxHtlcs is the remote_max_htlcs value set when opening the
	// channel, restricting the number of concurrent HTLCs the remote party
	// can add to a commitment.
	RemoteMaxHtlcs uint16

	// FundingShim is an optional funding shim that the caller can specify
	// in order to modify the channel funding workflow.
	FundingShim *lnrpc.FundingShim

	// SatPerVByte is the amount of satoshis to spend in chain fees per
	// virtual byte of the transaction.
	SatPerVByte btcutil.Amount

	// ConfTarget is the number of blocks that the funding transaction
	// should be confirmed in.
	ConfTarget fn.Option[int32]

	// CommitmentType is the commitment type that should be used for the
	// channel to be opened.
	CommitmentType lnrpc.CommitmentType

	// ZeroConf is used to determine if the channel will be a zero-conf
	// channel. This only works if the explicit negotiation is used with
	// anchors or script enforced leases.
	ZeroConf bool

	// ScidAlias denotes whether the channel will be an option-scid-alias
	// channel type negotiation.
	ScidAlias bool

	// BaseFee is the channel base fee applied during the channel
	// announcement phase.
	BaseFee uint64

	// FeeRate is the channel fee rate in ppm applied during the channel
	// announcement phase.
	FeeRate uint64

	// UseBaseFee, if set, instructs the downstream logic to apply the
	// user-specified channel base fee to the channel update announcement.
	// If set to false it avoids applying a base fee of 0 and instead
	// activates the default configured base fee.
	UseBaseFee bool

	// UseFeeRate, if set, instructs the downstream logic to apply the
	// user-specified channel fee rate to the channel update announcement.
	// If set to false it avoids applying a fee rate of 0 and instead
	// activates the default configured fee rate.
	UseFeeRate bool

	// FundMax is a boolean indicating whether the channel should be funded
	// with the maximum possible amount from the wallet.
	FundMax bool

	// An optional note-to-self containing some useful information about the
	// channel. This is stored locally only, and is purely for reference. It
	// has no bearing on the channel's operation. Max allowed length is 500
	// characters.
	Memo string

	// Outpoints is a list of client-selected outpoints that should be used
	// for funding a channel. If Amt is specified then this amount is
	// allocated from the sum of outpoints towards funding. If the
	// FundMax flag is specified the entirety of selected funds is
	// allocated towards channel funding.
	Outpoints []*lnrpc.OutPoint

	// CloseAddress sets the upfront_shutdown_script parameter during
	// channel open. It is expected to be encoded as a bitcoin address.
	CloseAddress string
}

// prepareOpenChannel waits for both nodes to be synced to chain and returns an
// OpenChannelRequest.
func (h *HarnessTest) prepareOpenChannel(srcNode, destNode *node.HarnessNode,
	p OpenChannelParams) *lnrpc.OpenChannelRequest {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	h.WaitForBlockchainSync(srcNode)
	h.WaitForBlockchainSync(destNode)

	// Specify the minimal confirmations of the UTXOs used for channel
	// funding.
	minConfs := int32(1)
	if p.SpendUnconfirmed {
		minConfs = 0
	}

	// Get the requested conf target. If not set, default to 6.
	confTarget := p.ConfTarget.UnwrapOr(6)

	// If there's fee rate set, unset the conf target.
	if p.SatPerVByte != 0 {
		confTarget = 0
	}

	// Prepare the request.
	return &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(p.Amt),
		PushSat:            int64(p.PushAmt),
		Private:            p.Private,
		TargetConf:         confTarget,
		MinConfs:           minConfs,
		SpendUnconfirmed:   p.SpendUnconfirmed,
		MinHtlcMsat:        int64(p.MinHtlc),
		RemoteMaxHtlcs:     uint32(p.RemoteMaxHtlcs),
		FundingShim:        p.FundingShim,
		SatPerVbyte:        uint64(p.SatPerVByte),
		CommitmentType:     p.CommitmentType,
		ZeroConf:           p.ZeroConf,
		ScidAlias:          p.ScidAlias,
		BaseFee:            p.BaseFee,
		FeeRate:            p.FeeRate,
		UseBaseFee:         p.UseBaseFee,
		UseFeeRate:         p.UseFeeRate,
		FundMax:            p.FundMax,
		Memo:               p.Memo,
		Outpoints:          p.Outpoints,
		CloseAddress:       p.CloseAddress,
	}
}

// OpenChannelAssertPending attempts to open a channel between srcNode and
// destNode with the passed channel funding parameters. Once the `OpenChannel`
// is called, it will consume the first event it receives from the open channel
// client and asserts it's a channel pending event.
func (h *HarnessTest) openChannelAssertPending(srcNode,
	destNode *node.HarnessNode,
	p OpenChannelParams) (*lnrpc.PendingUpdate, rpc.OpenChanClient) {

	// Prepare the request and open the channel.
	openReq := h.prepareOpenChannel(srcNode, destNode, p)
	respStream := srcNode.RPC.OpenChannel(openReq)

	// Consume the "channel pending" update. This waits until the node
	// notifies us that the final message in the channel funding workflow
	// has been sent to the remote node.
	resp := h.ReceiveOpenChannelUpdate(respStream)

	// Check that the update is channel pending.
	update, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.Truef(h, ok, "expected channel pending: update, instead got %v",
		resp)

	return update.ChanPending, respStream
}

// OpenChannelAssertPending attempts to open a channel between srcNode and
// destNode with the passed channel funding parameters. Once the `OpenChannel`
// is called, it will consume the first event it receives from the open channel
// client and asserts it's a channel pending event. It returns the
// `PendingUpdate`.
func (h *HarnessTest) OpenChannelAssertPending(srcNode,
	destNode *node.HarnessNode, p OpenChannelParams) *lnrpc.PendingUpdate {

	resp, _ := h.openChannelAssertPending(srcNode, destNode, p)
	return resp
}

// OpenChannelAssertStream attempts to open a channel between srcNode and
// destNode with the passed channel funding parameters. Once the `OpenChannel`
// is called, it will consume the first event it receives from the open channel
// client and asserts it's a channel pending event. It returns the open channel
// stream.
func (h *HarnessTest) OpenChannelAssertStream(srcNode,
	destNode *node.HarnessNode, p OpenChannelParams) rpc.OpenChanClient {

	_, stream := h.openChannelAssertPending(srcNode, destNode, p)
	return stream
}

// OpenChannel attempts to open a channel with the specified parameters
// extended from Alice to Bob. Additionally, for public channels, it will mine
// extra blocks so they are announced to the network. In specific, the
// following items are asserted,
//   - for non-zero conf channel, 1 blocks will be mined to confirm the funding
//     tx.
//   - both nodes should see the channel edge update in their network graph.
//   - both nodes can report the status of the new channel from ListChannels.
//   - extra blocks are mined if it's a public channel.
func (h *HarnessTest) OpenChannel(alice, bob *node.HarnessNode,
	p OpenChannelParams) *lnrpc.ChannelPoint {

	// First, open the channel without announcing it.
	cp := h.OpenChannelNoAnnounce(alice, bob, p)

	// If this is a private channel, there's no need to mine extra blocks
	// since it will never be announced to the network.
	if p.Private {
		return cp
	}

	// Mine extra blocks to announce the channel.
	if p.ZeroConf {
		// For a zero-conf channel, no blocks have been mined so we
		// need to mine 6 blocks.
		//
		// Mine 1 block to confirm the funding transaction.
		h.MineBlocksAndAssertNumTxes(numBlocksOpenChannel, 1)
	} else {
		// For a regular channel, 1 block has already been mined to
		// confirm the funding transaction, so we mine 5 blocks.
		h.MineBlocks(numBlocksOpenChannel - 1)
	}

	return cp
}

// OpenChannelNoAnnounce attempts to open a channel with the specified
// parameters extended from Alice to Bob without mining the necessary blocks to
// announce the channel. Additionally, the following items are asserted,
//   - for non-zero conf channel, 1 blocks will be mined to confirm the funding
//     tx.
//   - both nodes should see the channel edge update in their network graph.
//   - both nodes can report the status of the new channel from ListChannels.
func (h *HarnessTest) OpenChannelNoAnnounce(alice, bob *node.HarnessNode,
	p OpenChannelParams) *lnrpc.ChannelPoint {

	chanOpenUpdate := h.OpenChannelAssertStream(alice, bob, p)

	// Open a zero conf channel.
	if p.ZeroConf {
		return h.openChannelZeroConf(alice, bob, chanOpenUpdate)
	}

	// Open a non-zero conf channel.
	return h.openChannel(alice, bob, chanOpenUpdate)
}

// openChannel attempts to open a channel with the specified parameters
// extended from Alice to Bob. Additionally, the following items are asserted,
//   - 1 block is mined and the funding transaction should be found in it.
//   - both nodes should see the channel edge update in their network graph.
//   - both nodes can report the status of the new channel from ListChannels.
func (h *HarnessTest) openChannel(alice, bob *node.HarnessNode,
	stream rpc.OpenChanClient) *lnrpc.ChannelPoint {

	// Mine 1 block to confirm the funding transaction.
	block := h.MineBlocksAndAssertNumTxes(1, 1)[0]

	// Wait for the channel open event.
	fundingChanPoint := h.WaitForChannelOpenEvent(stream)

	// Check that the funding tx is found in the first block.
	fundingTxID := h.GetChanPointFundingTxid(fundingChanPoint)
	h.AssertTxInBlock(block, fundingTxID)

	// Check that both alice and bob have seen the channel from their
	// network topology.
	h.AssertChannelInGraph(alice, fundingChanPoint)
	h.AssertChannelInGraph(bob, fundingChanPoint)

	// Check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, fundingChanPoint)
	h.AssertChannelExists(bob, fundingChanPoint)

	return fundingChanPoint
}

// openChannelZeroConf attempts to open a channel with the specified parameters
// extended from Alice to Bob. Additionally, the following items are asserted,
//   - both nodes should see the channel edge update in their network graph.
//   - both nodes can report the status of the new channel from ListChannels.
func (h *HarnessTest) openChannelZeroConf(alice, bob *node.HarnessNode,
	stream rpc.OpenChanClient) *lnrpc.ChannelPoint {

	// Wait for the channel open event.
	fundingChanPoint := h.WaitForChannelOpenEvent(stream)

	// Check that both alice and bob have seen the channel from their
	// network topology.
	h.AssertChannelInGraph(alice, fundingChanPoint)
	h.AssertChannelInGraph(bob, fundingChanPoint)

	// Finally, check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, fundingChanPoint)
	h.AssertChannelExists(bob, fundingChanPoint)

	return fundingChanPoint
}

// OpenChannelAssertErr opens a channel between node srcNode and destNode,
// asserts that the expected error is returned from the channel opening.
func (h *HarnessTest) OpenChannelAssertErr(srcNode, destNode *node.HarnessNode,
	p OpenChannelParams, expectedErr error) {

	// Prepare the request and open the channel.
	openReq := h.prepareOpenChannel(srcNode, destNode, p)
	respStream := srcNode.RPC.OpenChannel(openReq)

	// Receive an error to be sent from the stream.
	_, err := h.receiveOpenChannelUpdate(respStream)
	require.NotNil(h, err, "expected channel opening to fail")

	// Use string comparison here as we haven't codified all the RPC errors
	// yet.
	require.Containsf(h, err.Error(), expectedErr.Error(), "unexpected "+
		"error returned, want %v, got %v", expectedErr, err)
}

// closeChannelOpts holds the options for closing a channel.
type closeChannelOpts struct {
	feeRate fn.Option[chainfee.SatPerVByte]

	// localTxOnly is a boolean indicating if we should only attempt to
	// consume close pending notifications for the local transaction.
	localTxOnly bool

	// skipMempoolCheck is a boolean indicating if we should skip the normal
	// mempool check after a coop close.
	skipMempoolCheck bool

	// errString is an expected error. If this is non-blank, then we'll
	// assert that the coop close wasn't possible, and returns an error that
	// contains this err string.
	errString string
}

// CloseChanOpt is a functional option to modify the way we close a channel.
type CloseChanOpt func(*closeChannelOpts)

// WithCoopCloseFeeRate is a functional option to set the fee rate for a coop
// close attempt.
func WithCoopCloseFeeRate(rate chainfee.SatPerVByte) CloseChanOpt {
	return func(o *closeChannelOpts) {
		o.feeRate = fn.Some(rate)
	}
}

// WithLocalTxNotify is a functional option to indicate that we should only
// notify for the local txn. This is useful for the RBF coop close type, as
// it'll notify for both local and remote txns.
func WithLocalTxNotify() CloseChanOpt {
	return func(o *closeChannelOpts) {
		o.localTxOnly = true
	}
}

// WithSkipMempoolCheck is a functional option to indicate that we should skip
// the mempool check. This can be used when a coop close iteration may not
// result in a newly broadcast transaction.
func WithSkipMempoolCheck() CloseChanOpt {
	return func(o *closeChannelOpts) {
		o.skipMempoolCheck = true
	}
}

// WithExpectedErrString is a functional option that can be used to assert that
// an error occurs during the coop close process.
func WithExpectedErrString(errString string) CloseChanOpt {
	return func(o *closeChannelOpts) {
		o.errString = errString
	}
}

// defaultCloseOpts returns the set of default close options.
func defaultCloseOpts() *closeChannelOpts {
	return &closeChannelOpts{}
}

// CloseChannelAssertPending attempts to close the channel indicated by the
// passed channel point, initiated by the passed node. Once the CloseChannel
// rpc is called, it will consume one event and assert it's a close pending
// event. In addition, it will check that the closing tx can be found in the
// mempool.
func (h *HarnessTest) CloseChannelAssertPending(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, force bool,
	opts ...CloseChanOpt) (rpc.CloseChanClient, *lnrpc.CloseStatusUpdate) {

	closeOpts := defaultCloseOpts()
	for _, optFunc := range opts {
		optFunc(closeOpts)
	}

	// Calls the rpc to close the channel.
	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: cp,
		Force:        force,
		NoWait:       true,
	}

	closeOpts.feeRate.WhenSome(func(feeRate chainfee.SatPerVByte) {
		closeReq.SatPerVbyte = uint64(feeRate)
	})

	var (
		stream rpc.CloseChanClient
		event  *lnrpc.CloseStatusUpdate
		err    error
	)

	// Consume the "channel close" update in order to wait for the closing
	// transaction to be broadcast, then wait for the closing tx to be seen
	// within the network.
	stream = hn.RPC.CloseChannel(closeReq)
	_, err = h.ReceiveCloseChannelUpdate(stream)
	require.NoError(h, err, "close channel update got error: %v", err)

	var closeTxid *chainhash.Hash
	for {
		event, err = h.ReceiveCloseChannelUpdate(stream)
		if err != nil {
			h.Logf("Test: %s, close channel got error: %v",
				h.manager.currentTestCase, err)
		}
		if err != nil && closeOpts.errString == "" {
			require.NoError(h, err, "retry closing channel failed")
		} else if err != nil && closeOpts.errString != "" {
			require.ErrorContains(h, err, closeOpts.errString)
			return nil, nil
		}

		pendingClose, ok := event.Update.(*lnrpc.CloseStatusUpdate_ClosePending) //nolint:ll
		require.Truef(h, ok, "expected channel close "+
			"update, instead got %v", pendingClose)

		if !pendingClose.ClosePending.LocalCloseTx &&
			closeOpts.localTxOnly {

			continue
		}

		notifyRate := pendingClose.ClosePending.FeePerVbyte
		if closeOpts.localTxOnly &&
			notifyRate != int64(closeReq.SatPerVbyte) {

			continue
		}

		closeTxid, err = chainhash.NewHash(
			pendingClose.ClosePending.Txid,
		)
		require.NoErrorf(h, err, "unable to decode closeTxid: %v",
			pendingClose.ClosePending.Txid)

		break
	}

	if !closeOpts.skipMempoolCheck {
		// Assert the closing tx is in the mempool.
		h.miner.AssertTxInMempool(*closeTxid)
	}

	return stream, event
}

// CloseChannel attempts to coop close a non-anchored channel identified by the
// passed channel point owned by the passed harness node. The following items
// are asserted,
//  1. a close pending event is sent from the close channel client.
//  2. the closing tx is found in the mempool.
//  3. the node reports the channel being waiting to close.
//  4. a block is mined and the closing tx should be found in it.
//  5. the node reports zero waiting close channels.
//  6. the node receives a topology update regarding the channel close.
func (h *HarnessTest) CloseChannel(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) chainhash.Hash {

	stream, _ := h.CloseChannelAssertPending(hn, cp, false)

	return h.AssertStreamChannelCoopClosed(hn, cp, false, stream)
}

// ForceCloseChannel attempts to force close a non-anchored channel identified
// by the passed channel point owned by the passed harness node. The following
// items are asserted,
//  1. a close pending event is sent from the close channel client.
//  2. the closing tx is found in the mempool.
//  3. the node reports the channel being waiting to close.
//  4. a block is mined and the closing tx should be found in it.
//  5. the node reports zero waiting close channels.
//  6. the node receives a topology update regarding the channel close.
//  7. mine DefaultCSV-1 blocks.
//  8. the node reports zero pending force close channels.
func (h *HarnessTest) ForceCloseChannel(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint) chainhash.Hash {

	stream, _ := h.CloseChannelAssertPending(hn, cp, true)

	closingTxid := h.AssertStreamChannelForceClosed(hn, cp, false, stream)

	// Cleanup the force close.
	h.CleanupForceClose(hn)

	return closingTxid
}

// CloseChannelAssertErr closes the given channel and asserts an error
// returned.
func (h *HarnessTest) CloseChannelAssertErr(hn *node.HarnessNode,
	req *lnrpc.CloseChannelRequest) error {

	// Calls the rpc to close the channel.
	stream := hn.RPC.CloseChannel(req)

	// Consume the "channel close" update in order to wait for the closing
	// transaction to be broadcast, then wait for the closing tx to be seen
	// within the network.
	_, err := h.ReceiveCloseChannelUpdate(stream)
	require.Errorf(h, err, "%s: expect close channel to return an error",
		hn.Name())

	return err
}

// IsNeutrinoBackend returns a bool indicating whether the node is using a
// neutrino as its backend. This is useful when we want to skip certain tests
// which cannot be done with a neutrino backend.
func (h *HarnessTest) IsNeutrinoBackend() bool {
	return h.manager.chainBackend.Name() == NeutrinoBackendName
}

// fundCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node. The confirmed boolean indicates whether the
// transaction that pays to the target should confirm. For neutrino backend,
// the `confirmed` param is ignored.
func (h *HarnessTest) fundCoins(amt btcutil.Amount, target *node.HarnessNode,
	addrType lnrpc.AddressType, confirmed bool) *wire.MsgTx {

	initialBalance := target.RPC.WalletBalance()

	// First, obtain an address from the target lightning node, preferring
	// to receive a p2wkh address s.t the output can immediately be used as
	// an input to a funding transaction.
	req := &lnrpc.NewAddressRequest{Type: addrType}
	resp := target.RPC.NewAddress(req)
	addr := h.DecodeAddress(resp.Address)
	addrScript := h.PayToAddrScript(addr)

	// Generate a transaction which creates an output to the target
	// pkScript of the desired amount.
	output := &wire.TxOut{
		PkScript: addrScript,
		Value:    int64(amt),
	}
	txid := h.miner.SendOutput(output, defaultMinerFeeRate)

	// Get the funding tx.
	tx := h.GetRawTransaction(*txid)
	msgTx := tx.MsgTx()

	// Since neutrino doesn't support unconfirmed outputs, skip this check.
	if !h.IsNeutrinoBackend() {
		expectedBalance := btcutil.Amount(
			initialBalance.UnconfirmedBalance,
		) + amt
		h.WaitForBalanceUnconfirmed(target, expectedBalance)
	}

	// If the transaction should remain unconfirmed, then we'll wait until
	// the target node's unconfirmed balance reflects the expected balance
	// and exit.
	if !confirmed {
		return msgTx
	}

	// Otherwise, we'll generate 1 new blocks to ensure the output gains a
	// sufficient number of confirmations and wait for the balance to
	// reflect what's expected.
	h.MineBlockWithTx(msgTx)

	expectedBalance := btcutil.Amount(initialBalance.ConfirmedBalance) + amt
	h.WaitForBalanceConfirmed(target, expectedBalance)

	return msgTx
}

// FundCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node using a P2WKH address. 1 blocks are mined after in
// order to confirm the transaction.
func (h *HarnessTest) FundCoins(amt btcutil.Amount,
	hn *node.HarnessNode) *wire.MsgTx {

	return h.fundCoins(amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, true)
}

// FundCoinsUnconfirmed attempts to send amt satoshis from the internal mining
// node to the targeted lightning node using a P2WKH address. No blocks are
// mined after and the UTXOs are unconfirmed.
func (h *HarnessTest) FundCoinsUnconfirmed(amt btcutil.Amount,
	hn *node.HarnessNode) *wire.MsgTx {

	return h.fundCoins(
		amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
}

// FundCoinsNP2WKH attempts to send amt satoshis from the internal mining node
// to the targeted lightning node using a NP2WKH address.
func (h *HarnessTest) FundCoinsNP2WKH(amt btcutil.Amount,
	target *node.HarnessNode) *wire.MsgTx {

	return h.fundCoins(
		amt, target, lnrpc.AddressType_NESTED_PUBKEY_HASH, true,
	)
}

// FundCoinsP2TR attempts to send amt satoshis from the internal mining node to
// the targeted lightning node using a P2TR address.
func (h *HarnessTest) FundCoinsP2TR(amt btcutil.Amount,
	target *node.HarnessNode) *wire.MsgTx {

	return h.fundCoins(amt, target, lnrpc.AddressType_TAPROOT_PUBKEY, true)
}

// FundNumCoins attempts to send the given number of UTXOs from the internal
// mining node to the targeted lightning node using a P2WKH address. Each UTXO
// has an amount of 1 BTC. 1 blocks are mined to confirm the tx.
func (h *HarnessTest) FundNumCoins(hn *node.HarnessNode, num int) {
	// Get the initial balance first.
	resp := hn.RPC.WalletBalance()
	initialBalance := btcutil.Amount(resp.ConfirmedBalance)

	const fundAmount = 1 * btcutil.SatoshiPerBitcoin

	// Send out the outputs from the miner.
	for i := 0; i < num; i++ {
		h.createAndSendOutput(
			hn, fundAmount, lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		)
	}

	// Wait for ListUnspent to show the correct number of unconfirmed
	// UTXOs.
	//
	// Since neutrino doesn't support unconfirmed outputs, skip this check.
	if !h.IsNeutrinoBackend() {
		h.AssertNumUTXOsUnconfirmed(hn, num)
	}

	// Mine a block to confirm the transactions.
	h.MineBlocksAndAssertNumTxes(1, num)

	// Now block until the wallet have fully synced up.
	totalAmount := btcutil.Amount(fundAmount * num)
	expectedBalance := initialBalance + totalAmount
	h.WaitForBalanceConfirmed(hn, expectedBalance)
}

// completePaymentRequestsAssertStatus sends payments from a node to complete
// all payment requests. This function does not return until all payments
// have reached the specified status.
func (h *HarnessTest) completePaymentRequestsAssertStatus(hn *node.HarnessNode,
	paymentRequests []string, status lnrpc.Payment_PaymentStatus,
	opts ...HarnessOpt) {

	payOpts := defaultHarnessOpts()
	for _, opt := range opts {
		opt(&payOpts)
	}

	// Create a buffered chan to signal the results.
	results := make(chan rpc.PaymentClient, len(paymentRequests))

	// send sends a payment and asserts if it doesn't succeeded.
	send := func(payReq string) {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: int32(wait.PaymentTimeout.Seconds()),
			FeeLimitMsat:   noFeeLimitMsat,
			Amp:            payOpts.useAMP,
		}
		stream := hn.RPC.SendPayment(req)

		// Signal sent succeeded.
		results <- stream
	}

	// Launch all payments simultaneously.
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		go send(payReqCopy)
	}

	// Wait for all payments to report the expected status.
	timer := time.After(wait.PaymentTimeout)
	select {
	case stream := <-results:
		h.AssertPaymentStatusFromStream(stream, status)

	case <-timer:
		require.Failf(h, "timeout", "%s: waiting payment results "+
			"timeout", hn.Name())
	}
}

// CompletePaymentRequests sends payments from a node to complete all payment
// requests. This function does not return until all payments successfully
// complete without errors.
func (h *HarnessTest) CompletePaymentRequests(hn *node.HarnessNode,
	paymentRequests []string, opts ...HarnessOpt) {

	h.completePaymentRequestsAssertStatus(
		hn, paymentRequests, lnrpc.Payment_SUCCEEDED, opts...,
	)
}

// CompletePaymentRequestsNoWait sends payments from a node to complete all
// payment requests without waiting for the results. Instead, it checks the
// number of updates in the specified channel has increased.
func (h *HarnessTest) CompletePaymentRequestsNoWait(hn *node.HarnessNode,
	paymentRequests []string, chanPoint *lnrpc.ChannelPoint) {

	// We start by getting the current state of the client's channels. This
	// is needed to ensure the payments actually have been committed before
	// we return.
	oldResp := h.GetChannelByChanPoint(hn, chanPoint)

	// Send payments and assert they are in-flight.
	h.completePaymentRequestsAssertStatus(
		hn, paymentRequests, lnrpc.Payment_IN_FLIGHT,
	)

	// We are not waiting for feedback in the form of a response, but we
	// should still wait long enough for the server to receive and handle
	// the send before cancelling the request. We wait for the number of
	// updates to one of our channels has increased before we return.
	err := wait.NoError(func() error {
		newResp := h.GetChannelByChanPoint(hn, chanPoint)

		// If this channel has an increased number of updates, we
		// assume the payments are committed, and we can return.
		if newResp.NumUpdates > oldResp.NumUpdates {
			return nil
		}

		// Otherwise return an error as the NumUpdates are not
		// increased.
		return fmt.Errorf("%s: channel:%v not updated after sending "+
			"payments, old updates: %v, new updates: %v", hn.Name(),
			chanPoint, oldResp.NumUpdates, newResp.NumUpdates)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking for channel updates")
}

// OpenChannelPsbt attempts to open a channel between srcNode and destNode with
// the passed channel funding parameters. It will assert if the expected step
// of funding the PSBT is not received from the source node.
func (h *HarnessTest) OpenChannelPsbt(srcNode, destNode *node.HarnessNode,
	p OpenChannelParams) (rpc.OpenChanClient, []byte) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	h.WaitForBlockchainSync(srcNode)
	h.WaitForBlockchainSync(destNode)

	// Send the request to open a channel to the source node now. This will
	// open a long-lived stream where we'll receive status updates about
	// the progress of the channel.
	// respStream := h.OpenChannelStreamAndAssert(srcNode, destNode, p)
	req := &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(p.Amt),
		PushSat:            int64(p.PushAmt),
		Private:            p.Private,
		SpendUnconfirmed:   p.SpendUnconfirmed,
		MinHtlcMsat:        int64(p.MinHtlc),
		FundingShim:        p.FundingShim,
		CommitmentType:     p.CommitmentType,
	}
	respStream := srcNode.RPC.OpenChannel(req)

	// Consume the "PSBT funding ready" update. This waits until the node
	// notifies us that the PSBT can now be funded.
	resp := h.ReceiveOpenChannelUpdate(respStream)
	upd, ok := resp.Update.(*lnrpc.OpenStatusUpdate_PsbtFund)
	require.Truef(h, ok, "expected PSBT funding update, got %v", resp)

	// Make sure the channel funding address has the correct type for the
	// given commitment type.
	fundingAddr, err := btcutil.DecodeAddress(
		upd.PsbtFund.FundingAddress, miner.HarnessNetParams,
	)
	require.NoError(h, err)

	switch p.CommitmentType {
	case lnrpc.CommitmentType_SIMPLE_TAPROOT:
		require.IsType(h, &btcutil.AddressTaproot{}, fundingAddr)

	default:
		require.IsType(
			h, &btcutil.AddressWitnessScriptHash{}, fundingAddr,
		)
	}

	return respStream, upd.PsbtFund.Psbt
}

// CleanupForceClose mines blocks to clean up the force close process. This is
// used for tests that are not asserting the expected behavior is found during
// the force close process, e.g., num of sweeps, etc. Instead, it provides a
// shortcut to move the test forward with a clean mempool.
func (h *HarnessTest) CleanupForceClose(hn *node.HarnessNode) {
	// Wait for the channel to be marked pending force close.
	h.AssertNumPendingForceClose(hn, 1)

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel. The commit sweep resolver is offers the input to the
	// sweeper when it's force closed, and broadcast the sweep tx at
	// defaulCSV-1.
	//
	// NOTE: we might empty blocks here as we don't know the exact number
	// of blocks to mine. This may end up mining more blocks than needed.
	h.MineEmptyBlocks(node.DefaultCSV - 1)

	// Assert there is one pending sweep.
	h.AssertNumPendingSweeps(hn, 1)

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	h.MineBlocksAndAssertNumTxes(1, 1)

	// Mine blocks to get any second level HTLC resolved. If there are no
	// HTLCs, this will behave like h.AssertNumPendingCloseChannels.
	h.mineTillForceCloseResolved(hn)
}

// CreatePayReqs is a helper method that will create a slice of payment
// requests for the given node.
func (h *HarnessTest) CreatePayReqs(hn *node.HarnessNode,
	paymentAmt btcutil.Amount, numInvoices int,
	routeHints ...*lnrpc.RouteHint) ([]string, [][]byte, []*lnrpc.Invoice) {

	payReqs := make([]string, numInvoices)
	rHashes := make([][]byte, numInvoices)
	invoices := make([]*lnrpc.Invoice, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := h.Random32Bytes()

		invoice := &lnrpc.Invoice{
			Memo:       "testing",
			RPreimage:  preimage,
			Value:      int64(paymentAmt),
			RouteHints: routeHints,
		}
		resp := hn.RPC.AddInvoice(invoice)

		// Set the payment address in the invoice so the caller can
		// properly use it.
		invoice.PaymentAddr = resp.PaymentAddr

		payReqs[i] = resp.PaymentRequest
		rHashes[i] = resp.RHash
		invoices[i] = invoice
	}

	return payReqs, rHashes, invoices
}

// BackupDB creates a backup of the current database. It will stop the node
// first, copy the database files, and restart the node.
func (h *HarnessTest) BackupDB(hn *node.HarnessNode) {
	restart := h.SuspendNode(hn)

	err := hn.BackupDB()
	require.NoErrorf(h, err, "%s: failed to backup db", hn.Name())

	err = restart()
	require.NoErrorf(h, err, "%s: failed to restart", hn.Name())
}

// RestartNodeAndRestoreDB restarts a given node with a callback to restore the
// db.
func (h *HarnessTest) RestartNodeAndRestoreDB(hn *node.HarnessNode) {
	cb := func() error { return hn.RestoreDB() }
	err := h.manager.restartNode(h.runCtx, hn, cb)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())

	err = h.manager.unlockNode(hn)
	require.NoErrorf(h, err, "failed to unlock node %s", hn.Name())

	// Give the node some time to catch up with the chain before we
	// continue with the tests.
	h.WaitForBlockchainSync(hn)
}

// CleanShutDown is used to quickly end a test by shutting down all non-standby
// nodes and mining blocks to empty the mempool.
//
// NOTE: this method provides a faster exit for a test that involves force
// closures as the caller doesn't need to mine all the blocks to make sure the
// mempool is empty.
func (h *HarnessTest) CleanShutDown() {
	// First, shutdown all nodes to prevent new transactions being created
	// and fed into the mempool.
	h.shutdownAllNodes()

	// Now mine blocks till the mempool is empty.
	h.cleanMempool()
}

// QueryChannelByChanPoint tries to find a channel matching the channel point
// and asserts. It returns the channel found.
func (h *HarnessTest) QueryChannelByChanPoint(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint,
	opts ...ListChannelOption) *lnrpc.Channel {

	channel, err := h.findChannel(hn, chanPoint, opts...)
	require.NoError(h, err, "failed to query channel")

	return channel
}

// SendPaymentAndAssertStatus sends a payment from the passed node and asserts
// the desired status is reached.
func (h *HarnessTest) SendPaymentAndAssertStatus(hn *node.HarnessNode,
	req *routerrpc.SendPaymentRequest,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	stream := hn.RPC.SendPayment(req)
	return h.AssertPaymentStatusFromStream(stream, status)
}

// SendPaymentAssertFail sends a payment from the passed node and asserts the
// payment is failed with the specified failure reason .
func (h *HarnessTest) SendPaymentAssertFail(hn *node.HarnessNode,
	req *routerrpc.SendPaymentRequest,
	reason lnrpc.PaymentFailureReason) *lnrpc.Payment {

	payment := h.SendPaymentAndAssertStatus(hn, req, lnrpc.Payment_FAILED)
	require.Equal(h, reason, payment.FailureReason,
		"payment failureReason not matched")

	return payment
}

// SendPaymentAssertSettled sends a payment from the passed node and asserts the
// payment is settled.
func (h *HarnessTest) SendPaymentAssertSettled(hn *node.HarnessNode,
	req *routerrpc.SendPaymentRequest) *lnrpc.Payment {

	return h.SendPaymentAndAssertStatus(hn, req, lnrpc.Payment_SUCCEEDED)
}

// SendPaymentAssertInflight sends a payment from the passed node and asserts
// the payment is inflight.
func (h *HarnessTest) SendPaymentAssertInflight(hn *node.HarnessNode,
	req *routerrpc.SendPaymentRequest) *lnrpc.Payment {

	return h.SendPaymentAndAssertStatus(hn, req, lnrpc.Payment_IN_FLIGHT)
}

// OpenChannelRequest is used to open a channel using the method
// OpenMultiChannelsAsync.
type OpenChannelRequest struct {
	// Local is the funding node.
	Local *node.HarnessNode

	// Remote is the receiving node.
	Remote *node.HarnessNode

	// Param is the open channel params.
	Param OpenChannelParams

	// stream is the client created after calling OpenChannel RPC.
	stream rpc.OpenChanClient

	// result is a channel used to send the channel point once the funding
	// has succeeded.
	result chan *lnrpc.ChannelPoint
}

// OpenMultiChannelsAsync takes a list of OpenChannelRequest and opens them in
// batch. The channel points are returned in same the order of the requests
// once all of the channel open succeeded.
//
// NOTE: compared to open multiple channel sequentially, this method will be
// faster as it doesn't need to mine 6 blocks for each channel open. However,
// it does make debugging the logs more difficult as messages are intertwined.
func (h *HarnessTest) OpenMultiChannelsAsync(
	reqs []*OpenChannelRequest) []*lnrpc.ChannelPoint {

	// openChannel opens a channel based on the request.
	openChannel := func(req *OpenChannelRequest) {
		stream := h.OpenChannelAssertStream(
			req.Local, req.Remote, req.Param,
		)
		req.stream = stream
	}

	// assertChannelOpen is a helper closure that asserts a channel is
	// open.
	assertChannelOpen := func(req *OpenChannelRequest) {
		// Wait for the channel open event from the stream.
		cp := h.WaitForChannelOpenEvent(req.stream)

		if !req.Param.Private {
			// Check that both alice and bob have seen the channel
			// from their channel watch request.
			h.AssertChannelInGraph(req.Local, cp)
			h.AssertChannelInGraph(req.Remote, cp)
		}

		// Finally, check that the channel can be seen in their
		// ListChannels.
		h.AssertChannelExists(req.Local, cp)
		h.AssertChannelExists(req.Remote, cp)

		req.result <- cp
	}

	// Go through the requests and make the OpenChannel RPC call.
	for _, r := range reqs {
		openChannel(r)
	}

	// Mine one block to confirm all the funding transactions.
	h.MineBlocksAndAssertNumTxes(1, len(reqs))

	// Mine 5 more blocks so all the public channels are announced to the
	// network.
	h.MineBlocks(numBlocksOpenChannel - 1)

	// Once the blocks are mined, we fire goroutines for each of the
	// request to watch for the channel openning.
	for _, r := range reqs {
		r.result = make(chan *lnrpc.ChannelPoint, 1)
		go assertChannelOpen(r)
	}

	// Finally, collect the results.
	channelPoints := make([]*lnrpc.ChannelPoint, 0)
	for _, r := range reqs {
		select {
		case cp := <-r.result:
			channelPoints = append(channelPoints, cp)

		case <-time.After(wait.ChannelOpenTimeout):
			require.Failf(h, "timeout", "wait channel point "+
				"timeout for channel %s=>%s", r.Local.Name(),
				r.Remote.Name())
		}
	}

	// Assert that we have the expected num of channel points.
	require.Len(h, channelPoints, len(reqs),
		"returned channel points not match")

	return channelPoints
}

// ReceiveInvoiceUpdate waits until a message is received on the subscribe
// invoice stream or the timeout is reached.
func (h *HarnessTest) ReceiveInvoiceUpdate(
	stream rpc.InvoiceUpdateClient) *lnrpc.Invoice {

	chanMsg := make(chan *lnrpc.Invoice)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving invoice update")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// CalculateTxFee retrieves parent transactions and reconstructs the fee paid.
func (h *HarnessTest) CalculateTxFee(tx *wire.MsgTx) btcutil.Amount {
	var balance btcutil.Amount
	for _, in := range tx.TxIn {
		parentHash := in.PreviousOutPoint.Hash
		rawTx := h.miner.GetRawTransaction(parentHash)
		parent := rawTx.MsgTx()
		value := parent.TxOut[in.PreviousOutPoint.Index].Value

		balance += btcutil.Amount(value)
	}

	for _, out := range tx.TxOut {
		balance -= btcutil.Amount(out.Value)
	}

	return balance
}

// CalculateTxWeight calculates the weight for a given tx.
//
// TODO(yy): use weight estimator to get more accurate result.
func (h *HarnessTest) CalculateTxWeight(tx *wire.MsgTx) lntypes.WeightUnit {
	utx := btcutil.NewTx(tx)
	return lntypes.WeightUnit(blockchain.GetTransactionWeight(utx))
}

// CalculateTxFeeRate calculates the fee rate for a given tx.
func (h *HarnessTest) CalculateTxFeeRate(
	tx *wire.MsgTx) chainfee.SatPerKWeight {

	w := h.CalculateTxWeight(tx)
	fee := h.CalculateTxFee(tx)

	return chainfee.NewSatPerKWeight(fee, w)
}

// CalculateTxesFeeRate takes a list of transactions and estimates the fee rate
// used to sweep them.
//
// NOTE: only used in current test file.
func (h *HarnessTest) CalculateTxesFeeRate(txns []*wire.MsgTx) int64 {
	const scale = 1000

	var totalWeight, totalFee int64
	for _, tx := range txns {
		utx := btcutil.NewTx(tx)
		totalWeight += blockchain.GetTransactionWeight(utx)

		fee := h.CalculateTxFee(tx)
		totalFee += int64(fee)
	}
	feeRate := totalFee * scale / totalWeight

	return feeRate
}

// AssertSweepFound looks up a sweep in a nodes list of broadcast sweeps and
// asserts it's found.
//
// NOTE: Does not account for node's internal state.
func (h *HarnessTest) AssertSweepFound(hn *node.HarnessNode,
	sweep string, verbose bool, startHeight int32) {

	req := &walletrpc.ListSweepsRequest{
		Verbose:     verbose,
		StartHeight: startHeight,
	}

	err := wait.NoError(func() error {
		// List all sweeps that alice's node had broadcast.
		sweepResp := hn.RPC.ListSweeps(req)

		var found bool
		if verbose {
			found = findSweepInDetails(h, sweep, sweepResp)
		} else {
			found = findSweepInTxids(h, sweep, sweepResp)
		}

		if found {
			return nil
		}

		return fmt.Errorf("sweep tx %v not found in resp %v", sweep,
			sweepResp)
	}, wait.DefaultTimeout)
	require.NoError(h, err, "%s: timeout checking sweep tx", hn.Name())
}

func findSweepInTxids(ht *HarnessTest, sweepTxid string,
	sweepResp *walletrpc.ListSweepsResponse) bool {

	sweepTxIDs := sweepResp.GetTransactionIds()
	require.NotNil(ht, sweepTxIDs, "expected transaction ids")
	require.Nil(ht, sweepResp.GetTransactionDetails())

	// Check that the sweep tx we have just produced is present.
	for _, tx := range sweepTxIDs.TransactionIds {
		if tx == sweepTxid {
			return true
		}
	}

	return false
}

func findSweepInDetails(ht *HarnessTest, sweepTxid string,
	sweepResp *walletrpc.ListSweepsResponse) bool {

	sweepDetails := sweepResp.GetTransactionDetails()
	require.NotNil(ht, sweepDetails, "expected transaction details")
	require.Nil(ht, sweepResp.GetTransactionIds())

	for _, tx := range sweepDetails.Transactions {
		if tx.TxHash == sweepTxid {
			return true
		}
	}

	return false
}

// QueryRoutesAndRetry attempts to keep querying a route until timeout is
// reached.
//
// NOTE: when a channel is opened, we may need to query multiple times to get
// it in our QueryRoutes RPC. This happens even after we check the channel is
// heard by the node using ht.AssertChannelOpen. Deep down, this is because our
// GraphTopologySubscription and QueryRoutes give different results regarding a
// specific channel, with the formal reporting it being open while the latter
// not, resulting GraphTopologySubscription acting "faster" than QueryRoutes.
// TODO(yy): make sure related subsystems share the same view on a given
// channel.
func (h *HarnessTest) QueryRoutesAndRetry(hn *node.HarnessNode,
	req *lnrpc.QueryRoutesRequest) *lnrpc.QueryRoutesResponse {

	var routes *lnrpc.QueryRoutesResponse
	err := wait.NoError(func() error {
		ctxt, cancel := context.WithCancel(h.runCtx)
		defer cancel()

		resp, err := hn.RPC.LN.QueryRoutes(ctxt, req)
		if err != nil {
			return fmt.Errorf("%s: failed to query route: %w",
				hn.Name(), err)
		}

		routes = resp

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout querying routes")

	return routes
}

// ReceiveHtlcInterceptor waits until a message is received on the htlc
// interceptor stream or the timeout is reached.
func (h *HarnessTest) ReceiveHtlcInterceptor(
	stream rpc.InterceptorClient) *routerrpc.ForwardHtlcInterceptRequest {

	chanMsg := make(chan *routerrpc.ForwardHtlcInterceptRequest)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout intercepting htlc")

	case err := <-errChan:
		require.Failf(h, "err from HTLC interceptor stream",
			"received err from HTLC interceptor stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// ReceiveInvoiceHtlcModification waits until a message is received on the
// invoice HTLC modifier stream or the timeout is reached.
func (h *HarnessTest) ReceiveInvoiceHtlcModification(
	stream rpc.InvoiceHtlcModifierClient) *invoicesrpc.HtlcModifyRequest {

	chanMsg := make(chan *invoicesrpc.HtlcModifyRequest)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout invoice HTLC modifier")

	case err := <-errChan:
		require.Failf(h, "err from invoice HTLC modifier stream",
			"received err from invoice HTLC modifier stream: %v",
			err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// ReceiveChannelEvent waits until a message is received from the
// ChannelEventsClient stream or the timeout is reached.
func (h *HarnessTest) ReceiveChannelEvent(
	stream rpc.ChannelEventsClient) *lnrpc.ChannelEventUpdate {

	chanMsg := make(chan *lnrpc.ChannelEventUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving channel events")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// GetOutputIndex returns the output index of the given address in the given
// transaction.
func (h *HarnessTest) GetOutputIndex(txid chainhash.Hash, addr string) int {
	// We'll then extract the raw transaction from the mempool in order to
	// determine the index of the p2tr output.
	tx := h.miner.GetRawTransaction(txid)

	p2trOutputIndex := -1
	for i, txOut := range tx.MsgTx().TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, h.miner.ActiveNet,
		)
		require.NoError(h, err)

		if addrs[0].String() == addr {
			p2trOutputIndex = i
		}
	}
	require.Greater(h, p2trOutputIndex, -1)

	return p2trOutputIndex
}

// SendCoins sends a coin from node A to node B with the given amount, returns
// the sending tx.
func (h *HarnessTest) SendCoins(a, b *node.HarnessNode,
	amt btcutil.Amount) *wire.MsgTx {

	// Create an address for Bob receive the coins.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	}
	resp := b.RPC.NewAddress(req)

	// Send the coins from Alice to Bob. We should expect a tx to be
	// broadcast and seen in the mempool.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:       resp.Address,
		Amount:     int64(amt),
		TargetConf: 6,
	}
	a.RPC.SendCoins(sendReq)
	tx := h.GetNumTxsFromMempool(1)[0]

	return tx
}

// SendCoins sends all coins from node A to node B, returns the sending tx.
func (h *HarnessTest) SendAllCoins(a, b *node.HarnessNode) *wire.MsgTx {
	// Create an address for Bob receive the coins.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	}
	resp := b.RPC.NewAddress(req)

	// Send the coins from Alice to Bob. We should expect a tx to be
	// broadcast and seen in the mempool.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:             resp.Address,
		TargetConf:       6,
		SendAll:          true,
		SpendUnconfirmed: true,
	}
	a.RPC.SendCoins(sendReq)
	tx := h.GetNumTxsFromMempool(1)[0]

	return tx
}

// CreateSimpleNetwork creates the number of nodes specified by the number of
// configs and makes a topology of `node1 -> node2 -> node3...`. Each node is
// created using the specified config, the neighbors are connected, and the
// channels are opened. Each node will be funded with a single UTXO of 1 BTC
// except the last one.
//
// For instance, to create a network with 2 nodes that share the same node
// config,
//
//	cfg := []string{"--protocol.anchors"}
//	cfgs := [][]string{cfg, cfg}
//	params := OpenChannelParams{...}
//	chanPoints, nodes := ht.CreateSimpleNetwork(cfgs, params)
//
// This will create two nodes and open an anchor channel between them.
func (h *HarnessTest) CreateSimpleNetwork(nodeCfgs [][]string,
	p OpenChannelParams) ([]*lnrpc.ChannelPoint, []*node.HarnessNode) {

	// Create new nodes.
	nodes := h.createNodes(nodeCfgs)

	var resp []*lnrpc.ChannelPoint

	// Open zero-conf channels if specified.
	if p.ZeroConf {
		resp = h.openZeroConfChannelsForNodes(nodes, p)
	} else {
		// Open channels between the nodes.
		resp = h.openChannelsForNodes(nodes, p)
	}

	return resp, nodes
}

// acceptChannel is used to accept a single channel that comes across. This
// should be run in a goroutine and is used to test nodes with the zero-conf
// feature bit.
func acceptChannel(t *testing.T, zeroConf bool, stream rpc.AcceptorClient) {
	req, err := stream.Recv()
	require.NoError(t, err)

	resp := &lnrpc.ChannelAcceptResponse{
		Accept:        true,
		PendingChanId: req.PendingChanId,
		ZeroConf:      zeroConf,
	}
	err = stream.Send(resp)
	require.NoError(t, err)
}

// nodeNames defines a slice of human-reable names for the nodes created in the
// `createNodes` method. 8 nodes are defined here as by default we can only
// create this many nodes in one test.
var nodeNames = []string{
	"Alice", "Bob", "Carol", "Dave", "Eve", "Frank", "Grace", "Heidi",
}

// createNodes creates the number of nodes specified by the number of configs.
// Each node is created using the specified config, the neighbors are
// connected.
func (h *HarnessTest) createNodes(nodeCfgs [][]string) []*node.HarnessNode {
	// Get the number of nodes.
	numNodes := len(nodeCfgs)

	// Make sure we are creating a reasonable number of nodes.
	require.LessOrEqual(h, numNodes, len(nodeNames), "too many nodes")

	// Make a slice of nodes.
	nodes := make([]*node.HarnessNode, numNodes)

	// Create new nodes.
	for i, nodeCfg := range nodeCfgs {
		nodeName := nodeNames[i]
		n := h.NewNode(nodeName, nodeCfg)
		nodes[i] = n
	}

	// Connect the nodes in a chain.
	for i := 1; i < len(nodes); i++ {
		nodeA := nodes[i-1]
		nodeB := nodes[i]
		h.EnsureConnected(nodeA, nodeB)
	}

	// Fund all the nodes expect the last one.
	for i := 0; i < len(nodes)-1; i++ {
		node := nodes[i]
		h.FundCoinsUnconfirmed(btcutil.SatoshiPerBitcoin, node)
	}

	// Mine 1 block to get the above coins confirmed.
	h.MineBlocksAndAssertNumTxes(1, numNodes-1)

	return nodes
}

// openChannelsForNodes takes a list of nodes and makes a topology of `node1 ->
// node2 -> node3...`.
func (h *HarnessTest) openChannelsForNodes(nodes []*node.HarnessNode,
	p OpenChannelParams) []*lnrpc.ChannelPoint {

	// Sanity check the params.
	require.Greater(h, len(nodes), 1, "need at least 2 nodes")

	// attachFundingShim is a helper closure that optionally attaches a
	// funding shim to the open channel params and returns it.
	attachFundingShim := func(
		nodeA, nodeB *node.HarnessNode) OpenChannelParams {

		// If this channel is not a script enforced lease channel,
		// we'll do nothing and return the params.
		leasedType := lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE
		if p.CommitmentType != leasedType {
			return p
		}

		// Otherwise derive the funding shim, attach it to the original
		// open channel params and return it.
		minerHeight := h.CurrentHeight()
		thawHeight := minerHeight + thawHeightDelta
		fundingShim, _ := h.DeriveFundingShim(
			nodeA, nodeB, p.Amt, thawHeight, true, leasedType,
		)

		p.FundingShim = fundingShim

		return p
	}

	// Open channels in batch to save blocks mined.
	reqs := make([]*OpenChannelRequest, 0, len(nodes)-1)
	for i := 0; i < len(nodes)-1; i++ {
		nodeA := nodes[i]
		nodeB := nodes[i+1]

		// Optionally attach a funding shim to the open channel params.
		p = attachFundingShim(nodeA, nodeB)

		req := &OpenChannelRequest{
			Local:  nodeA,
			Remote: nodeB,
			Param:  p,
		}
		reqs = append(reqs, req)
	}
	resp := h.OpenMultiChannelsAsync(reqs)

	// If the channels are private, make sure the channel participants know
	// the relevant channels.
	if p.Private {
		for i, chanPoint := range resp {
			// Get the channel participants - for n channels we
			// would have n+1 nodes.
			nodeA, nodeB := nodes[i], nodes[i+1]
			h.AssertChannelInGraph(nodeA, chanPoint)
			h.AssertChannelInGraph(nodeB, chanPoint)
		}
	} else {
		// Make sure the all nodes know all the channels if they are
		// public.
		for _, node := range nodes {
			for _, chanPoint := range resp {
				h.AssertChannelInGraph(node, chanPoint)
			}

			// Make sure every node has updated its cached graph
			// about the edges as indicated in `DescribeGraph`.
			h.AssertNumEdges(node, len(resp), false)
		}
	}

	return resp
}

// openZeroConfChannelsForNodes takes a list of nodes and makes a topology of
// `node1 -> node2 -> node3...` with zero-conf channels.
func (h *HarnessTest) openZeroConfChannelsForNodes(nodes []*node.HarnessNode,
	p OpenChannelParams) []*lnrpc.ChannelPoint {

	// Sanity check the params.
	require.True(h, p.ZeroConf, "zero-conf channels must be enabled")
	require.Greater(h, len(nodes), 1, "need at least 2 nodes")

	// We are opening numNodes-1 channels.
	cancels := make([]context.CancelFunc, 0, len(nodes)-1)

	// Create the channel acceptors.
	for _, node := range nodes[1:] {
		acceptor, cancel := node.RPC.ChannelAcceptor()
		go acceptChannel(h.T, true, acceptor)

		cancels = append(cancels, cancel)
	}

	// Open channels between the nodes.
	resp := h.openChannelsForNodes(nodes, p)

	for _, cancel := range cancels {
		cancel()
	}

	return resp
}

// DeriveFundingShim creates a channel funding shim by deriving the necessary
// keys on both sides.
func (h *HarnessTest) DeriveFundingShim(alice, bob *node.HarnessNode,
	chanSize btcutil.Amount, thawHeight uint32, publish bool,
	commitType lnrpc.CommitmentType) (*lnrpc.FundingShim,
	*lnrpc.ChannelPoint) {

	keyLoc := &signrpc.KeyLocator{KeyFamily: 9999}
	carolFundingKey := alice.RPC.DeriveKey(keyLoc)
	daveFundingKey := bob.RPC.DeriveKey(keyLoc)

	// Now that we have the multi-sig keys for each party, we can manually
	// construct the funding transaction. We'll instruct the backend to
	// immediately create and broadcast a transaction paying out an exact
	// amount. Normally this would reside in the mempool, but we just
	// confirm it now for simplicity.
	var (
		fundingOutput *wire.TxOut
		musig2        bool
		err           error
	)

	if commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT ||
		commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY {

		var carolKey, daveKey *btcec.PublicKey
		carolKey, err = btcec.ParsePubKey(carolFundingKey.RawKeyBytes)
		require.NoError(h, err)
		daveKey, err = btcec.ParsePubKey(daveFundingKey.RawKeyBytes)
		require.NoError(h, err)

		_, fundingOutput, err = input.GenTaprootFundingScript(
			carolKey, daveKey, int64(chanSize),
			fn.None[chainhash.Hash](),
		)
		require.NoError(h, err)

		musig2 = true
	} else {
		_, fundingOutput, err = input.GenFundingPkScript(
			carolFundingKey.RawKeyBytes, daveFundingKey.RawKeyBytes,
			int64(chanSize),
		)
		require.NoError(h, err)
	}

	var txid *chainhash.Hash
	targetOutputs := []*wire.TxOut{fundingOutput}
	if publish {
		txid = h.SendOutputsWithoutChange(targetOutputs, 5)
	} else {
		tx := h.CreateTransaction(targetOutputs, 5)

		txHash := tx.TxHash()
		txid = &txHash
	}

	// At this point, we can being our external channel funding workflow.
	// We'll start by generating a pending channel ID externally that will
	// be used to track this new funding type.
	pendingChanID := h.Random32Bytes()

	// Now that we have the pending channel ID, Dave (our responder) will
	// register the intent to receive a new channel funding workflow using
	// the pending channel ID.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: txid[:],
		},
	}
	chanPointShim := &lnrpc.ChanPointShim{
		Amt:       int64(chanSize),
		ChanPoint: chanPoint,
		LocalKey: &lnrpc.KeyDescriptor{
			RawKeyBytes: daveFundingKey.RawKeyBytes,
			KeyLoc: &lnrpc.KeyLocator{
				KeyFamily: daveFundingKey.KeyLoc.KeyFamily,
				KeyIndex:  daveFundingKey.KeyLoc.KeyIndex,
			},
		},
		RemoteKey:     carolFundingKey.RawKeyBytes,
		PendingChanId: pendingChanID,
		ThawHeight:    thawHeight,
		Musig2:        musig2,
	}
	fundingShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}
	bob.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})

	// If we attempt to register the same shim (has the same pending chan
	// ID), then we should get an error.
	bob.RPC.FundingStateStepAssertErr(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
			ShimRegister: fundingShim,
		},
	})

	// We'll take the chan point shim we just registered for Dave (the
	// responder), and swap the local/remote keys before we feed it in as
	// Carol's funding shim as the initiator.
	fundingShim.GetChanPointShim().LocalKey = &lnrpc.KeyDescriptor{
		RawKeyBytes: carolFundingKey.RawKeyBytes,
		KeyLoc: &lnrpc.KeyLocator{
			KeyFamily: carolFundingKey.KeyLoc.KeyFamily,
			KeyIndex:  carolFundingKey.KeyLoc.KeyIndex,
		},
	}
	fundingShim.GetChanPointShim().RemoteKey = daveFundingKey.RawKeyBytes

	return fundingShim, chanPoint
}
