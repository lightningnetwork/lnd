package lntemp

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntemp/rpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
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
	feeService WebFeeService

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
func NewHarnessTest(t *testing.T, lndBinary string, feeService WebFeeService,
	dbBackend lntest.DatabaseBackend) *HarnessTest {

	// Create the run context.
	ctxt, cancel := context.WithCancel(context.Background())

	manager := newNodeManager(lndBinary, dbBackend)
	return &HarnessTest{
		T:          t,
		manager:    manager,
		feeService: feeService,
		runCtx:     ctxt,
		cancel:     cancel,
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
	err := h.feeService.Start()
	require.NoError(h, err, "failed to start fee service")

	// Assemble the node manager with chainBackend and feeServiceURL.
	h.manager.chainBackend = chain
	h.manager.feeServiceURL = h.feeService.URL()

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
		h.manager.standbyNodes[hn.Cfg.NodeID] = hn
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
			h.Miner.SendOutput(output, defaultMinerFeeRate)
		}
	}

	// We generate several blocks in order to give the outputs created
	// above a good number of confirmations.
	h.MineBlocks(numBlocksSendOutput)

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
	err := h.feeService.Stop()
	require.NoError(h, err, "failed to stop fee service")

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
func (h *HarnessTest) Subtest(t *testing.T) *HarnessTest {
	t.Helper()

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

	// Inherit the subtest for the miner.
	st.Miner.T = st.T

	// Reset the standby nodes.
	st.resetStandbyNodes(t)

	// Reset fee estimator.
	st.SetFeeEstimate(DefaultFeeRateSatPerKw)

	// Record block height.
	_, startHeight := h.Miner.GetBestBlock()

	st.Cleanup(func() {
		_, endHeight := h.Miner.GetBestBlock()

		st.Logf("finished test: %s, start height=%d, end height=%d, "+
			"mined blocks=%d", st.manager.currentTestCase,
			startHeight, endHeight, endHeight-startHeight)

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
	})

	return st
}

// shutdownNonStandbyNodes will shutdown any non-standby nodes.
func (h *HarnessTest) shutdownNonStandbyNodes() {
	for nid, node := range h.manager.activeNodes {
		// If it's a standby node, skip.
		_, ok := h.manager.standbyNodes[nid]
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

	node, err := h.manager.newNode(h.T, name, extraArgs, nil, false)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	// Start the node.
	err = node.Start(h.runCtx)
	require.NoError(h, err, "failed to start node %s", node.Name())

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

// SuspendNode stops the given node and returns a callback that can be used to
// start it again.
func (h *HarnessTest) SuspendNode(node *node.HarnessNode) func() error {
	err := node.Stop()
	require.NoErrorf(h, err, "failed to stop %s", node.Name())

	// Remove the node from active nodes.
	delete(h.manager.activeNodes, node.Cfg.NodeID)

	return func() error {
		h.manager.registerNode(node)

		return node.Start(h.runCtx)
	}
}

// RestartNode restarts a given node and asserts.
func (h *HarnessTest) RestartNode(hn *node.HarnessNode,
	chanBackups ...*lnrpc.ChanBackupSnapshot) {

	err := h.manager.restartNode(h.runCtx, hn, nil, chanBackups...)
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
	err = node.StartWithSeed(h.runCtx)
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
	recoveryWindow int32, chanBackups *lnrpc.ChanBackupSnapshot,
	opts ...node.Option) *node.HarnessNode {

	node, err := h.manager.newNode(h.T, name, extraArgs, password, true)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	// Start the node with seed only, which will only create the `State`
	// and `WalletUnlocker` clients.
	err = node.StartWithSeed(h.runCtx)
	require.NoErrorf(h, err, "failed to start node %s", node.Name())

	// Create the wallet.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic,
		AezeedPassphrase:   password,
		ExtendedMasterKey:  rootKey,
		RecoveryWindow:     recoveryWindow,
		ChannelBackups:     chanBackups,
	}
	_, err = h.manager.initWalletAndNode(node, initReq)
	require.NoErrorf(h, err, "failed to unlock and init node %s",
		node.Name())

	return node
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

// GetChanPointFundingTxid takes a channel point and converts it into a chain
// hash.
func (h *HarnessTest) GetChanPointFundingTxid(
	cp *lnrpc.ChannelPoint) *chainhash.Hash {

	txid, err := lnrpc.GetChanPointFundingTxid(cp)
	require.NoError(h, err, "unable to get txid")

	return txid
}

// OutPointFromChannelPoint creates an outpoint from a given channel point.
func (h *HarnessTest) OutPointFromChannelPoint(
	cp *lnrpc.ChannelPoint) wire.OutPoint {

	txid := h.GetChanPointFundingTxid(cp)
	return wire.OutPoint{
		Hash:  *txid,
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

	// Prepare the request.
	return &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(p.Amt),
		PushSat:            int64(p.PushAmt),
		Private:            p.Private,
		MinConfs:           minConfs,
		SpendUnconfirmed:   p.SpendUnconfirmed,
		MinHtlcMsat:        int64(p.MinHtlc),
		RemoteMaxHtlcs:     uint32(p.RemoteMaxHtlcs),
		FundingShim:        p.FundingShim,
		SatPerByte:         int64(p.SatPerVByte),
		CommitmentType:     p.CommitmentType,
		ZeroConf:           p.ZeroConf,
		ScidAlias:          p.ScidAlias,
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
// extended from Alice to Bob. Additionally, the following items are asserted,
//   - 6 blocks will be mined so the channel will be announced if it's public.
//   - the funding transaction should be found in the first block.
//   - both nodes should see the channel edge update in their network graph.
//   - both nodes can report the status of the new channel from ListChannels.
func (h *HarnessTest) OpenChannel(alice, bob *node.HarnessNode,
	p OpenChannelParams) *lnrpc.ChannelPoint {

	chanOpenUpdate := h.OpenChannelAssertStream(alice, bob, p)

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := h.MineBlocksAndAssertNumTxes(numBlocksOpenChannel, 1)[0]

	// Wait for the channel open event.
	fundingChanPoint := h.WaitForChannelOpenEvent(chanOpenUpdate)

	// Check that the funding tx is found in the first block.
	fundingTxID := h.GetChanPointFundingTxid(fundingChanPoint)
	h.Miner.AssertTxInBlock(block, fundingTxID)

	// Check that both alice and bob have seen the channel from their
	// network topology.
	h.AssertTopologyChannelOpen(alice, fundingChanPoint)
	h.AssertTopologyChannelOpen(bob, fundingChanPoint)

	// Check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, fundingChanPoint)
	h.AssertChannelExists(bob, fundingChanPoint)

	// Finally, check the blocks are synced.
	h.WaitForBlockchainSync(alice)
	h.WaitForBlockchainSync(bob)

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

	// Use string comparison here as we haven't codified all the RPC errors
	// yet.
	require.Containsf(h, err.Error(), expectedErr.Error(), "unexpected "+
		"error returned, want %v, got %v", expectedErr, err)
}

// CloseChannelAssertPending attempts to close the channel indicated by the
// passed channel point, initiated by the passed node. Once the CloseChannel
// rpc is called, it will consume one event and assert it's a close pending
// event. In addition, it will check that the closing tx can be found in the
// mempool.
func (h *HarnessTest) CloseChannelAssertPending(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint,
	force bool) (rpc.CloseChanClient, *chainhash.Hash) {

	// Calls the rpc to close the channel.
	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: cp,
		Force:        force,
	}
	stream := hn.RPC.CloseChannel(closeReq)

	// Consume the "channel close" update in order to wait for the closing
	// transaction to be broadcast, then wait for the closing tx to be seen
	// within the network.
	event, err := h.ReceiveCloseChannelUpdate(stream)
	require.NoError(h, err)

	pendingClose, ok := event.Update.(*lnrpc.CloseStatusUpdate_ClosePending)
	require.Truef(h, ok, "expected channel close update, instead got %v",
		pendingClose)

	closeTxid, err := chainhash.NewHash(pendingClose.ClosePending.Txid)
	require.NoErrorf(h, err, "unable to decode closeTxid: %v",
		pendingClose.ClosePending.Txid)

	// Assert the closing tx is in the mempool.
	h.Miner.AssertTxInMempool(closeTxid)

	return stream, closeTxid
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
	cp *lnrpc.ChannelPoint) *chainhash.Hash {

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
	cp *lnrpc.ChannelPoint) *chainhash.Hash {

	stream, _ := h.CloseChannelAssertPending(hn, cp, true)

	closingTxid := h.AssertStreamChannelForceClosed(hn, cp, false, stream)

	// Cleanup the force close.
	h.CleanupForceClose(hn, cp)

	return closingTxid
}

// CloseChannelAssertErr closes the given channel and asserts an error
// returned.
func (h *HarnessTest) CloseChannelAssertErr(hn *node.HarnessNode,
	cp *lnrpc.ChannelPoint, force bool) error {

	// Calls the rpc to close the channel.
	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: cp,
		Force:        force,
	}
	stream := hn.RPC.CloseChannel(closeReq)

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
	addrType lnrpc.AddressType, confirmed bool) {

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
	h.Miner.SendOutput(output, defaultMinerFeeRate)

	// Encode the pkScript in hex as this the format that it will be
	// returned via rpc.
	expPkScriptStr := hex.EncodeToString(addrScript)

	// Now, wait for ListUnspent to show the unconfirmed transaction
	// containing the correct pkscript.
	//
	// Since neutrino doesn't support unconfirmed outputs, skip this check.
	if !h.IsNeutrinoBackend() {
		utxos := h.AssertNumUTXOsUnconfirmed(target, 1)

		// Assert that the lone unconfirmed utxo contains the same
		// pkscript as the output generated above.
		pkScriptStr := utxos[0].PkScript
		require.Equal(h, pkScriptStr, expPkScriptStr,
			"pkscript mismatch")
	}

	// If the transaction should remain unconfirmed, then we'll wait until
	// the target node's unconfirmed balance reflects the expected balance
	// and exit.
	if !confirmed && !h.IsNeutrinoBackend() {
		expectedBalance := btcutil.Amount(
			initialBalance.UnconfirmedBalance,
		) + amt
		h.WaitForBalanceUnconfirmed(target, expectedBalance)

		return
	}

	// Otherwise, we'll generate 1 new blocks to ensure the output gains a
	// sufficient number of confirmations and wait for the balance to
	// reflect what's expected.
	h.MineBlocks(1)

	expectedBalance := btcutil.Amount(initialBalance.ConfirmedBalance) + amt
	h.WaitForBalanceConfirmed(target, expectedBalance)
}

// FundCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node using a P2WKH address. 2 blocks are mined after in
// order to confirm the transaction.
func (h *HarnessTest) FundCoins(amt btcutil.Amount, hn *node.HarnessNode) {
	h.fundCoins(amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, true)
}

// FundCoinsUnconfirmed attempts to send amt satoshis from the internal mining
// node to the targeted lightning node using a P2WKH address. No blocks are
// mined after and the UTXOs are unconfirmed.
func (h *HarnessTest) FundCoinsUnconfirmed(amt btcutil.Amount,
	hn *node.HarnessNode) {

	h.fundCoins(amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, false)
}

// FundCoinsNP2WKH attempts to send amt satoshis from the internal mining node
// to the targeted lightning node using a NP2WKH address.
func (h *HarnessTest) FundCoinsNP2WKH(amt btcutil.Amount,
	target *node.HarnessNode) {

	h.fundCoins(amt, target, lnrpc.AddressType_NESTED_PUBKEY_HASH, true)
}

// FundCoinsP2TR attempts to send amt satoshis from the internal mining node to
// the targeted lightning node using a P2TR address.
func (h *HarnessTest) FundCoinsP2TR(amt btcutil.Amount,
	target *node.HarnessNode) {

	h.fundCoins(amt, target, lnrpc.AddressType_TAPROOT_PUBKEY, true)
}

// CompletePaymentRequests sends payments from a node to complete all payment
// requests. This function does not return until all payments successfully
// complete without errors.
func (h *HarnessTest) CompletePaymentRequests(hn *node.HarnessNode,
	paymentRequests []string) {

	var wg sync.WaitGroup

	// send sends a payment and asserts if it doesn't succeeded.
	send := func(payReq string) {
		defer wg.Done()

		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: defaultPaymentTimeout,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		stream := hn.RPC.SendPayment(req)
		h.AssertPaymentStatusFromStream(stream, lnrpc.Payment_SUCCEEDED)
	}

	// Launch all payments simultaneously.
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		wg.Add(1)
		go send(payReqCopy)
	}

	// Wait for all payments to report success.
	wg.Wait()
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

	// send sends a payment and asserts if it doesn't succeeded.
	send := func(payReq string) {
		req := &routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: defaultPaymentTimeout,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		hn.RPC.SendPayment(req)
	}

	// Launch all payments simultaneously.
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		go send(payReqCopy)
	}

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
	}
	respStream := srcNode.RPC.OpenChannel(req)

	// Consume the "PSBT funding ready" update. This waits until the node
	// notifies us that the PSBT can now be funded.
	resp := h.ReceiveOpenChannelUpdate(respStream)
	upd, ok := resp.Update.(*lnrpc.OpenStatusUpdate_PsbtFund)
	require.Truef(h, ok, "expected PSBT funding update, got %v", resp)

	return respStream, upd.PsbtFund.Psbt
}

// CleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func (h *HarnessTest) CleanupForceClose(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	// Wait for the channel to be marked pending force close.
	h.AssertNumPendingForceClose(hn, 1)

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	//
	// The commit sweep resolver is able to broadcast the sweep tx up to
	// one block before the CSV elapses, so wait until defaulCSV-1.
	h.MineBlocks(lntest.DefaultCSV - 1)

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	h.MineBlocksAndAssertNumTxes(1, 1)

	// Mine blocks to get any second level HTLC resolved. If there are no
	// HTLCs, this will behave like h.AssertNumPendingCloseChannels.
	h.mineTillForceCloseResolved(hn)
}

// mineTillForceCloseResolved asserts that the number of pending close channels
// are zero. Each time it checks, a new block is mined using MineBlocksSlow to
// give the node some time to catch up the chain.
//
// NOTE: this method is a workaround to make sure we have a clean mempool at
// the end of a channel force closure. We cannot directly mine blocks and
// assert channels being fully closed because the subsystems in lnd don't share
// the same block height. This is especially the case when blocks are produced
// too fast.
// TODO(yy): remove this workaround when syncing blocks are unified in all the
// subsystems.
func (h *HarnessTest) mineTillForceCloseResolved(hn *node.HarnessNode) {
	_, startHeight := h.Miner.GetBestBlock()

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		total := len(resp.PendingForceClosingChannels)
		if total != 0 {
			h.MineBlocks(1)

			return fmt.Errorf("expected num of pending force " +
				"close channel to be zero")
		}

		_, height := h.Miner.GetBestBlock()
		h.Logf("Mined %d blocks while waiting for force closed "+
			"channel to be resolved", height-startHeight)

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "assert force close resolved timeout")
}

// CreatePayReqs is a helper method that will create a slice of payment
// requests for the given node.
func (h *HarnessTest) CreatePayReqs(hn *node.HarnessNode,
	paymentAmt btcutil.Amount, numInvoices int) ([]string,
	[][]byte, []*lnrpc.Invoice) {

	payReqs := make([]string, numInvoices)
	rHashes := make([][]byte, numInvoices)
	invoices := make([]*lnrpc.Invoice, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := h.Random32Bytes()

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     int64(paymentAmt),
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

	// Give the node some time to catch up with the chain before we
	// continue with the tests.
	h.WaitForBlockchainSync(hn)
}

// MineBlocks mines blocks and asserts all active nodes have synced to the
// chain.
//
// NOTE: this differs from miner's `MineBlocks` as it requires the nodes to be
// synced.
func (h *HarnessTest) MineBlocks(num uint32) []*wire.MsgBlock {
	// Mining the blocks slow to give `lnd` more time to sync.
	blocks := h.Miner.MineBlocksSlow(num)

	// Make sure all the active nodes are synced.
	bestBlock := blocks[len(blocks)-1]
	h.AssertActiveNodesSyncedTo(bestBlock)

	return blocks
}

// MineBlocksAndAssertNumTxes mines blocks and asserts the number of
// transactions are found in the first block. It also asserts all active nodes
// have synced to the chain.
//
// NOTE: this differs from miner's `MineBlocks` as it requires the nodes to be
// synced.
func (h *HarnessTest) MineBlocksAndAssertNumTxes(num uint32,
	numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	txids := h.Miner.AssertNumTxsInMempool(numTxs)

	// Mine blocks.
	blocks := h.Miner.MineBlocksSlow(num)

	// Assert that all the transactions were included in the first block.
	for _, txid := range txids {
		h.Miner.AssertTxInBlock(blocks[0], txid)
	}

	// Finally, make sure all the active nodes are synced.
	bestBlock := blocks[len(blocks)-1]
	h.AssertActiveNodesSyncedTo(bestBlock)

	return blocks
}

// QueryChannelByChanPoint tries to find a channel matching the channel point
// and asserts. It returns the channel found.
func (h *HarnessTest) QueryChannelByChanPoint(hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.Channel {

	channel, err := h.findChannel(hn, chanPoint)
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

		// Check that both alice and bob have seen the channel
		// from their channel watch request.
		h.AssertTopologyChannelOpen(req.Local, cp)
		h.AssertTopologyChannelOpen(req.Remote, cp)

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

		case <-time.After(lntest.ChannelOpenTimeout):
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
		rawTx := h.Miner.GetRawTransaction(&parentHash)
		parent := rawTx.MsgTx()
		balance += btcutil.Amount(
			parent.TxOut[in.PreviousOutPoint.Index].Value,
		)
	}

	for _, out := range tx.TxOut {
		balance -= btcutil.Amount(out.Value)
	}

	return balance
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

type SweptOutput struct {
	OutPoint wire.OutPoint
	SweepTx  *wire.MsgTx
}

// FindCommitAndAnchor looks for a commitment sweep and anchor sweep in the
// mempool. Our anchor output is identified by having multiple inputs in its
// sweep transition, because we have to bring another input to add fees to the
// anchor. Note that the anchor swept output may be nil if the channel did not
// have anchors.
func (h *HarnessTest) FindCommitAndAnchor(sweepTxns []*wire.MsgTx,
	closeTx string) (*SweptOutput, *SweptOutput) {

	var commitSweep, anchorSweep *SweptOutput

	for _, tx := range sweepTxns {
		txHash := tx.TxHash()
		sweepTx := h.Miner.GetRawTransaction(&txHash)

		// We expect our commitment sweep to have a single input, and,
		// our anchor sweep to have more inputs (because the wallet
		// needs to add balance to the anchor amount). We find their
		// sweep txids here to setup appropriate resolutions. We also
		// need to find the outpoint for our resolution, which we do by
		// matching the inputs to the sweep to the close transaction.
		inputs := sweepTx.MsgTx().TxIn
		if len(inputs) == 1 {
			commitSweep = &SweptOutput{
				OutPoint: inputs[0].PreviousOutPoint,
				SweepTx:  tx,
			}
		} else {
			// Since we have more than one input, we run through
			// them to find the one whose previous outpoint matches
			// the closing txid, which means this input is spending
			// the close tx. This will be our anchor output.
			for _, txin := range inputs {
				op := txin.PreviousOutPoint.Hash.String()
				if op == closeTx {
					anchorSweep = &SweptOutput{
						OutPoint: txin.PreviousOutPoint,
						SweepTx:  tx,
					}
				}
			}
		}
	}

	return commitSweep, anchorSweep
}

// AssertSweepFound looks up a sweep in a nodes list of broadcast sweeps and
// asserts it's found.
//
// NOTE: Does not account for node's internal state.
func (h *HarnessTest) AssertSweepFound(hn *node.HarnessNode,
	sweep string, verbose bool) {

	// List all sweeps that alice's node had broadcast.
	sweepResp := hn.RPC.ListSweeps(verbose)

	var found bool
	if verbose {
		found = findSweepInDetails(h, sweep, sweepResp)
	} else {
		found = findSweepInTxids(h, sweep, sweepResp)
	}

	require.Truef(h, found, "%s: sweep: %v not found", sweep, hn.Name())
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
