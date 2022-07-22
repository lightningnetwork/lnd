package lntemp

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestCase defines a test case that's been used in the integration test.
type TestCase struct {
	// Name specifies the test name.
	Name string

	// TestFunc is the test case wrapped in a function.
	TestFunc func(t *HarnessTest)
}

// standbyNodes are a list of nodes which are created during the initialization
// of the test and used across all test cases.
type standbyNodes struct {
	// Alice and Bob are the initial seeder nodes that are automatically
	// created to be the initial participants of the test network.
	Alice *node.HarnessNode
	Bob   *node.HarnessNode
}

// HarnessTest builds on top of a testing.T with enhanced error detection. It
// is responsible for managing the interactions among different nodes, and
// providing easy-to-use assertions.
type HarnessTest struct {
	*testing.T

	// Embed the standbyNodes so we can easily access them via `ht.Alice`.
	standbyNodes

	// Miner is a reference to a running full node that can be used to
	// create new blocks on the network.
	Miner *HarnessMiner

	// manager handles the start and stop of a given node.
	manager *nodeManager

	// feeService is a web service that provides external fee estimates to
	// lnd.
	feeService *feeService

	// Channel for transmitting stderr output from failed lightning node
	// to main process.
	lndErrorChan chan error

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc

	// stopChainBackend points to the cleanup function returned by the
	// chainBackend.
	stopChainBackend func()

	// cleaned specifies whether the cleanup has been applied for the
	// current HarnessTest.
	cleaned bool
}

// NewHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func NewHarnessTest(t *testing.T, lndBinary string,
	dbBackend lntest.DatabaseBackend) *HarnessTest {

	// Create the run context.
	ctxt, cancel := context.WithCancel(context.Background())

	manager := newNodeManager(lndBinary, dbBackend)
	return &HarnessTest{
		T:       t,
		manager: manager,
		runCtx:  ctxt,
		cancel:  cancel,
		// We need to use buffered channel here as we don't want to
		// block sending errors.
		lndErrorChan: make(chan error, 10),
	}
}

// Start will assemble the chain backend and the miner for the HarnessTest. It
// also starts the fee service and watches lnd process error.
func (h *HarnessTest) Start(chain node.BackendConfig, miner *HarnessMiner) {
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
	h.feeService = startFeeService(h.T)

	// Assemble the node manager with chainBackend and feeServiceURL.
	h.manager.chainBackend = chain
	h.manager.feeServiceURL = h.feeService.url

	// Assemble the miner.
	h.Miner = miner
}

// ChainBackendName returns the chain backend name used in the test.
func (h *HarnessTest) ChainBackendName() string {
	return h.manager.chainBackend.Name()
}

// SetUp starts the initial seeder nodes within the test harness. The initial
// node's wallets will be funded wallets with 10x10 BTC outputs each.
func (h *HarnessTest) SetupStandbyNodes() {
	h.Log("Setting up standby nodes Alice and Bob...")
	defer h.Log("Finshed the setup, now running tests...")

	lndArgs := []string{
		"--default-remote-max-htlcs=483",
		"--dust-threshold=5000000",
	}
	// Start the initial seeder nodes within the test network, then connect
	// their respective RPC clients.
	h.Alice = h.NewNode("Alice", lndArgs)
	h.Bob = h.NewNode("Bob", lndArgs)

	// First, make a connection between the two nodes. This will wait until
	// both nodes are fully started since the Connect RPC is guarded behind
	// the server.Started() flag that waits for all subsystems to be ready.
	h.ConnectNodes(h.Alice, h.Bob)

	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}

	// Load up the wallets of the seeder nodes with 10 outputs of 10 BTC
	// each.
	nodes := []*node.HarnessNode{h.Alice, h.Bob}
	for _, hn := range nodes {
		h.manager.standbyNodes[hn.PubKeyStr] = hn
		for i := 0; i < 10; i++ {
			resp := hn.RPC.NewAddress(addrReq)

			addr, err := btcutil.DecodeAddress(
				resp.Address, h.Miner.ActiveNet,
			)
			require.NoError(h, err)

			addrScript, err := txscript.PayToAddrScript(addr)
			require.NoError(h, err)

			output := &wire.TxOut{
				PkScript: addrScript,
				Value:    10 * btcutil.SatoshiPerBitcoin,
			}
			_, err = h.Miner.SendOutputs(
				[]*wire.TxOut{output}, 7500,
			)
			require.NoError(h, err, "send output failed")
		}
	}

	// We generate several blocks in order to give the outputs created
	// above a good number of confirmations.
	h.Miner.MineBlocks(2)

	// Now we want to wait for the nodes to catch up.
	h.WaitForBlockchainSync(h.Alice)
	h.WaitForBlockchainSync(h.Bob)

	// Now block until both wallets have fully synced up.
	expectedBalance := int64(btcutil.SatoshiPerBitcoin * 100)
	err := wait.NoError(func() error {
		aliceResp := h.Alice.RPC.WalletBalance()
		bobResp := h.Bob.RPC.WalletBalance()

		if aliceResp.ConfirmedBalance != expectedBalance {
			return fmt.Errorf("expected 10 BTC, instead "+
				"alice has %d", aliceResp.ConfirmedBalance)
		}

		if bobResp.ConfirmedBalance != expectedBalance {
			return fmt.Errorf("expected 10 BTC, instead "+
				"bob has %d", bobResp.ConfirmedBalance)
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout checking balance for node")
}

// Stop stops the test harness.
func (h *HarnessTest) Stop() {
	// Do nothing if it's not started.
	if h.runCtx == nil {
		h.Log("HarnessTest is not started")
		return
	}

	// Stop all running nodes.
	for _, node := range h.manager.activeNodes {
		h.Shutdown(node)
	}

	close(h.lndErrorChan)

	// Stop the fee service.
	h.feeService.stop()

	// Stop the chainBackend.
	h.stopChainBackend()

	// Stop the miner.
	h.Miner.Stop()
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *HarnessTest) RunTestCase(testCase *TestCase) {
	defer func() {
		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.Fatalf("Failed: (%v) panic with: \n%v",
				testCase.Name, description)
		}
	}()

	testCase.TestFunc(h)
}

// resetStandbyNodes resets all standby nodes by attaching the new testing.T
// and restarting them with the original config.
func (h *HarnessTest) resetStandbyNodes(t *testing.T) {
	for _, hn := range h.manager.standbyNodes {
		// Inherit the testing.T.
		h.T = t

		// Reset the config so the node will be using the default
		// config for the coming test. This will also inherit the
		// test's running context.
		h.RestartNodeWithExtraArgs(hn, hn.Cfg.OriginalExtraArgs)
	}
}

// Subtest creates a child HarnessTest, which inherits the harness net and
// stand by nodes created by the parent test. It will return a cleanup function
// which resets  all the standby nodes' configs back to its original state and
// create snapshots of each nodes' internal state.
func (h *HarnessTest) Subtest(t *testing.T) (*HarnessTest, func()) {
	st := &HarnessTest{
		T:            t,
		manager:      h.manager,
		Miner:        h.Miner,
		standbyNodes: h.standbyNodes,
		feeService:   h.feeService,
		lndErrorChan: make(chan error, 10),
	}

	// Inherit context from the main test.
	st.runCtx, st.cancel = context.WithCancel(h.runCtx)

	// Reset the standby nodes.
	st.resetStandbyNodes(t)

	cleanup := func() {
		// Don't bother run the cleanups if the test is failed.
		if st.Failed() {
			st.Log("test failed, skipped cleanup")
			return
		}

		// Don't run cleanup if it's already done. This can happen if
		// we have multiple level inheritance of the parent harness
		// test. For instance, a `Subtest(st)`.
		if st.cleaned {
			st.Log("test already cleaned, skipped cleanup")
			return
		}

		// We require the mempool to be cleaned from the test.
		require.Empty(st, st.Miner.GetRawMempool(), "mempool not "+
			"cleaned, please mine blocks to clean them all.")

		// When we finish the test, reset the nodes' configs and take a
		// snapshot of each of the nodes' internal states.
		for _, node := range st.manager.standbyNodes {
			st.cleanupStandbyNode(node)
		}

		// If found running nodes, shut them down.
		st.shutdownNonStandbyNodes()

		// Assert that mempool is cleaned
		st.Miner.AssertNumTxsInMempool(0)

		// Finally, cancel the run context. We have to do it here
		// because we need to keep the context alive for the above
		// assertions used in cleanup.
		st.cancel()

		// We now want to mark the parent harness as cleaned to avoid
		// running cleanup again since its internal state has been
		// cleaned up by its child harness tests.
		h.cleaned = true
	}

	return st, cleanup
}

// shutdownNonStandbyNodes will shutdown any non-standby nodes.
func (h *HarnessTest) shutdownNonStandbyNodes() {
	for pks, node := range h.manager.activeNodes {
		// If it's a standby node, skip.
		_, ok := h.manager.standbyNodes[pks]
		if ok {
			continue
		}

		// The process may not be in a state to always shutdown
		// immediately, so we'll retry up to a hard limit to ensure we
		// eventually shutdown.
		err := wait.NoError(func() error {
			return h.manager.shutdownNode(node)
		}, DefaultTimeout)
		require.NoErrorf(h, err, "unable to shutdown %s", node.Name())
	}
}

// cleanupStandbyNode is a function should be called with defer whenever a
// subtest is created. It will reset the standby nodes configs, snapshot the
// states, and validate the node has a clean state.
func (h *HarnessTest) cleanupStandbyNode(hn *node.HarnessNode) {
	// Remove connections made from this test.
	h.removeConnectionns(hn)

	// Delete all payments made from this test.
	hn.RPC.DeleteAllPayments()

	// Update the node's internal state.
	hn.UpdateState()

	// Finally, check the node is in a clean state for the following tests.
	h.validateNodeState(hn)
}

// removeConnectionns will remove all connections made on the standby nodes
// expect the connections between Alice and Bob.
func (h *HarnessTest) removeConnectionns(hn *node.HarnessNode) {
	resp := hn.RPC.ListPeers()
	for _, peer := range resp.Peers {
		// Skip disconnecting Alice and Bob.
		switch peer.PubKey {
		case h.Alice.PubKeyStr:
			continue
		case h.Bob.PubKeyStr:
			continue
		}

		hn.RPC.DisconnectPeer(peer.PubKey)
	}
}

// SetTestName set the test case name.
func (h *HarnessTest) SetTestName(name string) {
	h.manager.currentTestCase = name

	// Overwrite the old log filename so we can create new log files.
	for _, node := range h.manager.standbyNodes {
		node.Cfg.LogFilenamePrefix = name
	}
}

// NewNode creates a new node and asserts its creation. The node is guaranteed
// to have finished its initialization and all its subservers are started.
func (h *HarnessTest) NewNode(name string,
	extraArgs []string) *node.HarnessNode {

	node, err := h.manager.newNode(h.T, name, extraArgs, false, nil, false)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	return node
}

// Shutdown shuts down the given node and asserts that no errors occur.
func (h *HarnessTest) Shutdown(node *node.HarnessNode) {
	// The process may not be in a state to always shutdown immediately, so
	// we'll retry up to a hard limit to ensure we eventually shutdown.
	err := wait.NoError(func() error {
		return h.manager.shutdownNode(node)
	}, DefaultTimeout)
	require.NoErrorf(h, err, "unable to shutdown %v", node.Name())
}

// RestartNode restarts a given node and asserts.
func (h *HarnessTest) RestartNode(hn *node.HarnessNode,
	chanBackups ...*lnrpc.ChanBackupSnapshot) {

	err := h.manager.restartNode(hn, nil, chanBackups...)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())

	// Give the node some time to catch up with the chain before we
	// continue with the tests.
	h.WaitForBlockchainSync(hn)
}

// RestartNodeWithExtraArgs updates the node's config and restarts it.
func (h *HarnessTest) RestartNodeWithExtraArgs(hn *node.HarnessNode,
	extraArgs []string) {

	hn.SetExtraArgs(extraArgs)
	h.RestartNode(hn, nil)
}

// SetFeeEstimate sets a fee rate to be returned from fee estimator.
func (h *HarnessTest) SetFeeEstimate(fee chainfee.SatPerKWeight) {
	h.feeService.setFee(fee)
}

// validateNodeState checks that the node doesn't have any uncleaned states
// which will affect its following tests.
func (h *HarnessTest) validateNodeState(hn *node.HarnessNode) {
	errStr := func(subject string) string {
		return fmt.Sprintf("%s: found %s channels, please close "+
			"them properly", hn.Name(), subject)
	}
	// If the node still has open channels, it's most likely that the
	// current test didn't close it properly.
	require.Zerof(h, hn.State.OpenChannel.Active, errStr("active"))
	require.Zerof(h, hn.State.OpenChannel.Public, errStr("public"))
	require.Zerof(h, hn.State.OpenChannel.Private, errStr("private"))
	require.Zerof(h, hn.State.OpenChannel.Pending, errStr("pending open"))

	// The number of pending force close channels should be zero.
	require.Zerof(h, hn.State.CloseChannel.PendingForceClose,
		errStr("pending force"))

	// The number of waiting close channels should be zero.
	require.Zerof(h, hn.State.CloseChannel.WaitingClose,
		errStr("waiting close"))

	// Ths number of payments should be zero.
	// TODO(yy): no need to check since it's deleted in the cleanup? Or
	// check it in a wait?
	require.Zerof(h, hn.State.Payment.Total, "%s: found "+
		"uncleaned payments, please delete all of them properly",
		hn.Name())
}
