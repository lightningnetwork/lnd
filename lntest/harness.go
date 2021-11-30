package lntest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/grpclog"
)

const (
	// noFeeLimitMsat is used to specify we will put no requirements on fee
	// charged when choosing a route path.
	noFeeLimitMsat = math.MaxInt64

	slowMineDelay = 20 * time.Millisecond
)

// TestCase defines a test case that's been used in the integration test.
type TestCase struct {
	// Name specifies the test name.
	Name string

	// TestFunc is the test case wrapped in a function.
	TestFunc func(t *HarnessTest)
}

// nodes are a list of nodes which are created during the initialization
// of the test and used across all test cases.
type nodes struct {
	// Alice and Bob are the initial seeder nodes that are automatically
	// created to be the initial participants of the test network.
	Alice *HarnessNode
	Bob   *HarnessNode
}

// HarnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aid in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type HarnessTest struct {
	*testing.T
	nodes

	// net is a reference to the current network harness.
	net *NetworkHarness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc

	// standbyNodes is a map of all the standby nodes.
	standbyNodes map[string]*HarnessNode
}

// NewHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func NewHarnessTest(t *testing.T, net *NetworkHarness) *HarnessTest {
	ctxt, cancel := context.WithCancel(context.Background())
	return &HarnessTest{
		T:            t,
		net:          net,
		runCtx:       ctxt,
		cancel:       cancel,
		standbyNodes: make(map[string]*HarnessNode),
	}
}

// SetUp starts the initial seeder nodes within the test harness. The initial
// node's wallets will be funded wallets with ten 1 BTC outputs each. Finally
// rpc clients capable of communicating with the initial seeder nodes are
// created. Nodes are initialized with the given extra command line flags,
// which should be formatted properly - "--arg=value".
func (h *HarnessTest) SetUp(lndArgs []string) {
	// Swap out grpc's default logger with out fake logger which drops the
	// statements on the floor.
	fakeLogger := grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard)
	grpclog.SetLoggerV2(fakeLogger)
	h.net.feeService = startFeeService(h.T)

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
	nodes := []*HarnessNode{h.Alice, h.Bob}
	for _, hn := range nodes {
		h.standbyNodes[hn.Name()] = hn
		for i := 0; i < 10; i++ {
			// TODO(yy): There is a weird behavior if we use
			// h.NewAddress instead, which will cause the test
			// psbt_channel_funding_external to fail. Need to
			// further investigate whether it's a bug inside lnd or
			// in the itest.
			resp, err := hn.rpc.LN.NewAddress(h.runCtx, addrReq)
			require.NoError(h, err, "failed to create new address")

			addr, err := btcutil.DecodeAddress(
				resp.Address, h.net.netParams,
			)
			require.NoError(h, err)

			addrScript, err := txscript.PayToAddrScript(addr)
			require.NoError(h, err)

			output := &wire.TxOut{
				PkScript: addrScript,
				Value:    10 * btcutil.SatoshiPerBitcoin,
			}
			_, err = h.Miner().SendOutputs(
				[]*wire.TxOut{output}, 7500,
			)
			require.NoError(h, err, "send output failed")
		}
	}

	// We generate several blocks in order to give the outputs created
	// above a good number of confirmations.
	h.MineBlocks(2)

	// Now we want to wait for the nodes to catch up.
	h.WaitForBlockchainSync(h.Alice)
	h.WaitForBlockchainSync(h.Bob)

	// Now block until both wallets have fully synced up.
	expectedBalance := int64(btcutil.SatoshiPerBitcoin * 100)
	err := wait.NoError(func() error {
		aliceResp := h.WalletBalance(h.Alice)
		bobResp := h.WalletBalance(h.Bob)

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

// FailNow is called by require at the very end if a test failed.
func (h *HarnessTest) FailNow() {
	h.T.FailNow()

	// Whenever the test fails, we also cancel the running context.
	h.cancel()
}

func (h *HarnessTest) Logf(format string, args ...interface{}) {
	// Append test name.
	testName := fmt.Sprintf("from test %s: ", h.net.currentTestCase)
	h.T.Logf(testName+format, args...)
}

func (h *HarnessTest) Miner() *HarnessMiner {
	return h.net.Miner
}

// cleanupStandbyNode is a function should be called with defer whenever a
// subtest is created. It will reset the standby nodes configs, snapshot the
// states, and validate the node has a clean state.
func (h *HarnessTest) cleanupStandbyNode(hn *HarnessNode) {
	// Remove connections made from this test.
	h.removeConnectionns(hn)

	// Reset the config so the node will be using the default config for
	// the coming test.
	h.resetArgsAndRestart(hn)

	// Delete all payments made from this test.
	h.DeleteAllPayments(hn)

	// Update the node's internal state.
	err := hn.updateState()
	require.NoErrorf(h, err, "failed to snapshot state for %s", hn.Name())

	// Finally, check the node is in a clean state for the following tests.
	h.validateNodeState(hn)
}

// resetArgsAndRestart will restart the node with its original extra args if
// it's changed in the test.
func (h *HarnessTest) resetArgsAndRestart(hn *HarnessNode) {
	// If the config is not changed, exit early.
	if equalStringSlices(hn.Cfg.ExtraArgs, hn.Cfg.OriginalExtraArgs) {
		return
	}

	// Otherwise, reset the config and restart the node.
	h.RestartNodeWithExtraArgs(hn, hn.Cfg.OriginalExtraArgs)
}

// removeConnectionns will remove all connections made on the standby nodes
// expect the connections between Alice and Bob.
func (h *HarnessTest) removeConnectionns(hn *HarnessNode) {
	resp := h.ListPeers(hn)
	for _, peer := range resp.Peers {
		// Skip disconnecting Alice and Bob.
		switch peer.PubKey {
		case h.Alice.PubKeyStr:
			continue
		case h.Bob.PubKeyStr:
			continue
		}

		h.DisconnectPeer(hn, peer.PubKey)
	}
}

// SnapshotNodeStates creates a snapshot of all the standby nodes' internal
// states.
func (h *HarnessTest) SnapshotNodeStates() {
	for _, node := range h.standbyNodes {
		err := node.updateState()
		require.NoErrorf(h, err, "failed to snapshot state for %s",
			node.Name())
	}
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

// RPCClients returns all the rpc clients of a given node. This should only be
// used for RPC related tests.
func (h *HarnessTest) RPCClients(hn *HarnessNode) *RPCClients {
	return hn.rpc
}

// IsNeutrinoBackend returns a bool indicating whether the node is using a
// neutrino as its backend. This is useful when we want to skip certain tests
// which cannot be done with a neutrino backend.
func (h *HarnessTest) IsNeutrinoBackend() bool {
	return h.net.BackendCfg.Name() == NeutrinoBackendName
}

// Subtest creates a child HarnessTest, which inhertis the harness net and
// stand by nodes created by the parent test. It will return a cleanup function
// which resets  all the standby nodes' configs back to its original state and
// create snapshots of each nodes' internal state.
func (h *HarnessTest) Subtest(t *testing.T) (*HarnessTest, func()) {
	st := NewHarnessTest(t, h.net)
	st.nodes = h.nodes
	st.standbyNodes = h.standbyNodes

	// Inherit context from the main test.
	st.runCtx, st.cancel = context.WithCancel(h.runCtx)

	cleanup := func() {
		// Don't bother run the cleanups if the test is failed.
		if st.Failed() {
			st.Log("test failed, skipped cleanup")
			return
		}

		// We require the mempool to be cleaned from the test.
		require.Empty(st, st.GetRawMempool(), "mempool not cleaned, "+
			"please mine blocks to clean them all.")

		// When we finish the test, reset the nodes' configs and take a
		// snapshot of each of the nodes' internal states.
		for _, node := range st.standbyNodes {
			st.cleanupStandbyNode(node)
		}

		// If found running nodes, shut them down.
		st.shutdownNonStandbyNodes()

		// Assert that mempool is cleaned
		st.AssertNumTxsInMempool(0)

		// Finally, cancel the run context. We have to do it here
		// because we need to keep the context alive for the above
		// assertions used in cleanup.
		st.cancel()
	}

	return st, cleanup
}

// shutdownNonStandbyNodes will shutdown any non-standby nodes.
func (h *HarnessTest) shutdownNonStandbyNodes() {
	for _, node := range h.net.activeNodes {
		// If it's a standby node, skip.
		standby, ok := h.standbyNodes[node.Name()]
		if ok && node.PubKeyStr == standby.PubKeyStr {
			continue
		}

		// Otherwise shut it down.
		h.Shutdown(node)
	}
}

// validateNodeState checks that the node doesn't have any uncleaned states
// which will affect its following tests.
func (h *HarnessTest) validateNodeState(hn *HarnessNode) {
	errStr := func(subject string) string {
		return fmt.Sprintf("%s: found %s channels, please close "+
			"them properly", hn.Name(), subject)
	}
	// If the node still has open channels, it's most likely that the
	// current test didn't close it properly.
	require.Zerof(h, hn.state.OpenChannel.Active, errStr("active"))
	require.Zerof(h, hn.state.OpenChannel.Public, errStr("public"))
	require.Zerof(h, hn.state.OpenChannel.Private, errStr("private"))
	require.Zerof(h, hn.state.OpenChannel.Pending, errStr("pending open"))

	// The number of pending force close channels should be zero.
	require.Zerof(h, hn.state.CloseChannel.PendingForceClose,
		errStr("pending force"))

	// The number of waiting close channels should be zero.
	require.Zerof(h, hn.state.CloseChannel.WaitingClose,
		errStr("waiting close"))

	// Ths number of payments should be zero.
	// TODO(yy): no need to check since it's deleted in the cleanup? Or
	// check it in a wait?
	require.Zerof(h, hn.state.Payment.Total, "%s: found "+
		"uncleaned payments, please delete all of them properly",
		hn.Name())
}

// SetTestName set the test case name.
func (h *HarnessTest) SetTestName(name string) {
	h.net.SetTestName(name)

	// Overwrite the old log filename so we can create new log files.
	for _, node := range h.standbyNodes {
		node.Cfg.LogFilenamePrefix = name
	}
}

// NewNode creates a new node and asserts its creation. The node is guaranteed
// to have finished its initialization and all its subservers are started.
func (h *HarnessTest) NewNode(name string, extraArgs []string) *HarnessNode {
	node, err := h.net.newNode(
		name, extraArgs, false, nil, h.net.dbBackend, true,
	)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	return node
}

// NewNodeRemoteSigner creates a new remote signer node and asserts its
// creation.
func (h *HarnessTest) NewNodeRemoteSigner(name string, extraArgs []string,
	password []byte, watchOnly *lnrpc.WatchOnly) *HarnessNode {

	node, err := h.net.NewNodeRemoteSigner(
		name, extraArgs, password, watchOnly,
	)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	return node
}

// KillNode stops the given node abruptly without waiting.
func (h *HarnessTest) KillNode(hn *HarnessNode) {
	err := h.net.KillNode(hn)
	require.NoErrorf(h, err, "unable to kill node %s", hn.Name())
}

// NewNodeWithSeedEtcd starts a new node with seed that'll use an external
// etcd database as its (remote) channel and wallet DB. The passsed cluster
// flag indicates that we'd like the node to join the cluster leader election.
func (h *HarnessTest) NewNodeWithSeedEtcd(name string, etcdCfg *etcd.Config,
	password []byte, entropy []byte, statelessInit, cluster bool,
	leaderSessionTTL int) (*HarnessNode, []string, []byte) {

	// We don't want to use the embedded etcd instance.
	const dbBackend = BackendBbolt

	extraArgs := extraArgsEtcd(etcdCfg, name, cluster, leaderSessionTTL)
	node, seed, mac, err := h.net.newNodeWithSeed(
		h.runCtx, name, extraArgs, password,
		entropy, statelessInit, dbBackend,
	)
	require.NoError(h, err, "failed to create new node with seed etcd")

	return node, seed, mac
}

// NewNodeEtcd starts a new node with seed that'll use an external etcd
// database as its (remote) channel and wallet DB. The passsed cluster flag
// indicates that we'd like the node to join the cluster leader election.  If
// the wait flag is false then we won't wait until RPC is available (this is
// useful when the node is not expected to become the leader right away).
func (h *HarnessTest) NewNodeEtcd(name string, etcdCfg *etcd.Config,
	password []byte, cluster, wait bool,
	leaderSessionTTL int) *HarnessNode {

	// We don't want to use the embedded etcd instance.
	const dbBackend = BackendBbolt

	extraArgs := extraArgsEtcd(etcdCfg, name, cluster, leaderSessionTTL)
	node, err := h.net.newNode(
		name, extraArgs, true, password, dbBackend, wait,
	)
	require.NoError(h, err, "failed to create new node with etcd")

	return node
}

// NewNodeWithSeed fully initializes a new HarnessNode after creating a fresh
// aezeed. The provided password is used as both the aezeed password and the
// wallet password. The generated mnemonic is returned along with the
// initialized harness node.
func (h *HarnessTest) NewNodeWithSeed(name string,
	extraArgs []string, password []byte,
	statelessInit bool) (*HarnessNode, []string, []byte) {

	node, seed, mac, err := h.net.newNodeWithSeed(
		h.runCtx, name, extraArgs, password,
		nil, statelessInit, h.net.dbBackend,
	)
	require.NoError(h, err, "failed to create new node with seed")

	return node, seed, mac
}

// RestoreNodeWithSeed fully initializes a HarnessNode using a chosen mnemonic,
// password, recovery window, and optionally a set of static channel backups.
// After providing the initialization request to unlock the node, this method
// will finish initializing the LightningClient such that the HarnessNode can
// be used for regular rpc operations.
func (h *HarnessTest) RestoreNodeWithSeed(name string, extraArgs []string,
	password []byte, mnemonic []string, rootKey string,
	recoveryWindow int32, chanBackups *lnrpc.ChanBackupSnapshot,
	opts ...NodeOption) *HarnessNode {

	node, err := h.net.newNode(
		name, extraArgs, true, password, h.net.dbBackend, true, opts...,
	)
	require.NoError(h, err, "restore node failed to create new node")

	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic,
		AezeedPassphrase:   password,
		ExtendedMasterKey:  rootKey,
		RecoveryWindow:     recoveryWindow,
		ChannelBackups:     chanBackups,
	}

	err = wait.NoError(func() error {
		_, err := node.Init(initReq)
		return err
	}, DefaultTimeout)
	require.NoError(h, err, "restore node failed to init node")

	// With the node started, we can now record its public key within the
	// global mapping.
	h.net.RegisterNode(node)

	return node
}

// Shutdown shuts down the given node and asserts that no errors occur.
func (h *HarnessTest) Shutdown(node *HarnessNode) {
	// The process may not be in a state to always shutdown immediately, so
	// we'll retry up to a hard limit to ensure we eventually shutdown.
	err := wait.NoError(func() error {
		return h.net.ShutdownNode(node)
	}, DefaultTimeout)
	require.NoErrorf(h, err, "unable to shutdown %v", node.Name())
}

// ConnectNodes creates a connection between the two nodes and asserts the
// connection is succeeded.
func (h *HarnessTest) ConnectNodes(a, b *HarnessNode) {
	ctx, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	bobInfo := h.GetInfo(b)

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
	}
	err := h.net.connect(ctx, req, a)
	require.NoErrorf(h, err, "unable to connect %s to %s",
		a.Name(), b.Name())

	h.assertPeerConnected(a, b)
}

// ConnectNodesPerm creates a persistent connection between the two nodes and
// asserts the connection is succeeded.
func (h *HarnessTest) ConnectNodesPerm(a, b *HarnessNode) {
	ctx, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	bobInfo := h.GetInfo(b)

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
		Perm: true,
	}
	err := h.net.connect(ctx, req, a)
	require.NoErrorf(h, err, "unable to connect %s to %s",
		a.Name(), b.Name())

	h.assertPeerConnected(a, b)
}

// DisconnectNodes disconnects the given two nodes and asserts the
// disconnection is succeeded. The request is made from node a and sent to node
// b.
func (h *HarnessTest) DisconnectNodes(a, b *HarnessNode) {
	err := h.net.DisconnectNodes(h.runCtx, a, b)
	require.NoError(h, err, "failed to disconnect nodes")
}

// SuspendNode suspends a running node and assert.
func (h *HarnessTest) SuspendNode(hn *HarnessNode) func() error {
	restartFunc, err := h.net.SuspendNode(hn)
	require.NoError(h, err)
	return restartFunc
}

// RestartNode restarts a given node and asserts.
func (h *HarnessTest) RestartNode(hn *HarnessNode,
	chanBackups ...*lnrpc.ChanBackupSnapshot) {

	err := h.net.RestartNode(hn, nil, chanBackups...)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// RestartNodeWithExtraArgs updates the node's config and restarts it.
func (h *HarnessTest) RestartNodeWithExtraArgs(hn *HarnessNode,
	extraArgs []string) {

	hn.SetExtraArgs(extraArgs)

	err := h.net.RestartNode(hn, nil, nil)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// RestartNodeNoUnlock restarts a given node without unlocking it and asserts.
func (h *HarnessTest) RestartNodeNoUnlock(hn *HarnessNode, wait bool) {
	err := h.net.RestartNodeNoUnlock(hn, nil, wait)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// RestartNodeAndRestoreDb restarts a given node with a callback to restore the
// db.
func (h *HarnessTest) RestartNodeAndRestoreDb(hn *HarnessNode) {
	cb := func() error { return h.net.RestoreDb(hn) }
	err := h.net.RestartNode(hn, cb)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// StopNode stops a given node and asserts.
func (h *HarnessTest) StopNode(hn *HarnessNode) {
	err := h.net.StopNode(hn)
	require.NoErrorf(h, err, "failed to stop node %s", hn.Name())
}

// SetFeeEstimate sets a fee rate to be returned from fee estimator.
func (h *HarnessTest) SetFeeEstimate(fee chainfee.SatPerKWeight) {
	h.net.feeService.setFee(fee)
}

// SetFeeEstimateWithConf sets a fee rate of a specified conf target to be
// returned from fee estimator.
func (h *HarnessTest) SetFeeEstimateWithConf(
	fee chainfee.SatPerKWeight, conf uint32) {

	h.net.feeService.setFeeWithConf(fee, conf)
}

// EnsureConnected will try to connect to two nodes, returning no error if they
// are already connected. If the nodes were not connected previously, this will
// behave the same as ConnectNodes. If a pending connection request has already
// been made, the method will block until the two nodes appear in each other's
// peers list, or until the DefaultTimeout expires.
func (h *HarnessTest) EnsureConnected(a, b *HarnessNode) {
	// errConnectionRequested is used to signal that a connection was
	// requested successfully, which is distinct from already being
	// connected to the peer.
	errConnectionRequested := "connection request in progress"

	tryConnect := func(a, b *HarnessNode) error {
		bInfo := h.GetInfo(b)

		req := &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: bInfo.IdentityPubkey,
				Host:   b.Cfg.P2PAddr(),
			},
		}

		ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
		defer cancel()

		_, err := a.rpc.LN.ConnectPeer(ctxt, req)

		// Request was successful.
		if err == nil {
			return nil
		}

		// If the connection is in process, we return no error.
		if strings.Contains(err.Error(), errConnectionRequested) {
			return nil
		}

		// If the two are already connected, we return early with no
		// error.
		if strings.Contains(err.Error(), "already connected to peer") {
			return nil
		}

		// We may get connection refused error if we happens to be in
		// the middle of a previous node disconnection, e.g., a restart
		// from one of the nodes.
		if strings.Contains(err.Error(), "connection refused") {
			return nil
		}

		return err
	}

	// Return any critical errors returned by either alice or bob.
	require.NoError(h, tryConnect(a, b), "connection failed between %s "+
		"and %s", a.Cfg.Name, b.Cfg.Name)
	require.NoError(h, tryConnect(b, a), "connection failed between %s "+
		"and %s", a.Cfg.Name, b.Cfg.Name)

	// Otherwise one or both requested a connection, so we wait for the
	// peers lists to reflect the connection.
	h.AssertConnected(a, b)
}

// SendCoinsOfType attempts to send amt satoshis from the internal mining node
// to the targeted lightning node. The confirmed boolean indicates whether the
// transaction that pays to the target should confirm.
func (h *HarnessTest) SendCoinsOfType(amt btcutil.Amount, hn *HarnessNode,
	addrType lnrpc.AddressType, confirmed bool) {

	err := h.sendCoins(amt, hn, addrType, confirmed)
	require.NoErrorf(h, err, "unable to send coins for %s", hn.Cfg.Name)
}

// SendCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node using a P2WKH address. 6 blocks are mined after in
// order to confirm the transaction.
func (h *HarnessTest) SendCoins(amt btcutil.Amount, hn *HarnessNode) {
	err := h.sendCoins(
		amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, true,
	)
	require.NoErrorf(h, err, "unable to send coins for %s", hn.Cfg.Name)
}

// SendCoinsUnconfirmed sends coins from the internal mining node to the target
// lightning node using a P2WPKH address. No blocks are mined after, so the
// transaction remains unconfirmed.
func (h *HarnessTest) SendCoinsUnconfirmed(amt btcutil.Amount,
	hn *HarnessNode) {

	err := h.sendCoins(
		amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
	require.NoErrorf(h, err, "unable to send unconfirmed coins for %s",
		hn.Cfg.Name)
}

// SendCoinsNP2WKH attempts to send amt satoshis from the internal mining node
// to the targeted lightning node using a NP2WKH address.
func (h *HarnessTest) SendCoinsNP2WKH(amt btcutil.Amount, target *HarnessNode) {
	err := h.sendCoins(
		amt, target, lnrpc.AddressType_NESTED_PUBKEY_HASH, true,
	)
	require.NoErrorf(h, err, "unable to send NP2WKH coins for %s",
		target.Cfg.Name)
}

// SendCoinsP2TR attempts to send amt satoshis from the internal mining node to
// the targeted lightning node using a P2TR address.
func (h *HarnessTest) SendCoinsP2TR(amt btcutil.Amount, target *HarnessNode) {
	err := h.sendCoins(
		amt, target, lnrpc.AddressType_TAPROOT_PUBKEY, true,
	)
	require.NoErrorf(h, err, "unable to send P2TR coins for %s",
		target.Cfg.Name)
}

// OpenPendingChannel opens a channel between the two nodes and asserts it's
// pending.
func (h *HarnessTest) OpenPendingChannel(from, to *HarnessNode,
	chanAmt, pushAmt btcutil.Amount) *lnrpc.PendingUpdate {

	update, err := h.net.OpenPendingChannel(
		h.runCtx, from, to, chanAmt, pushAmt,
	)
	require.NoError(h, err, "unable to open channel")
	return update
}

// OpenChannel attempts to open a channel with the specified parameters
// extended from Alice to Bob. Additionally, the following items are asserted,
//   * the funding transaction should be found within a block
//   * Alice and Bob have seen the channel edge update.
//   * Alice and Bob can report the status of the new channel.
func (h *HarnessTest) OpenChannel(alice, bob *HarnessNode,
	p OpenChannelParams) *lnrpc.ChannelPoint {

	chanOpenUpdate := h.OpenChannelStreamAndAssert(alice, bob, p)

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := h.MineBlocksAndAssertTx(6, 1)[0]

	fundingChanPoint := h.WaitForChannelOpen(chanOpenUpdate)

	fundingTxID := h.GetChanPointFundingTxid(fundingChanPoint)
	h.AssertTxInBlock(block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.

	// Check that both alice and bob have seen the channel from
	// their channel watch request.
	h.AssertChannelOpen(alice, fundingChanPoint)
	h.AssertChannelOpen(bob, fundingChanPoint)

	// Finally, check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, fundingChanPoint)
	h.AssertChannelExists(bob, fundingChanPoint)

	return fundingChanPoint
}

// OpenChannelAssertErr opens a channel between node a and b, asserts that the
// expected error is returned from the channel opening.
func (h *HarnessTest) OpenChannelAssertErr(a, b *HarnessNode,
	p OpenChannelParams, expectedErr error) {

	err := wait.NoError(func() error {
		_, err := h.net.OpenChannel(h.runCtx, a, b, p)
		if err == nil {
			return fmt.Errorf("no error returned")
		}

		// Use string comparison here as we haven't codified all the
		// RPC errors yet.
		if strings.Contains(err.Error(), expectedErr.Error()) {
			return nil
		}

		return fmt.Errorf("unexpected error returned, want %v, got %v",
			expectedErr, err)
	}, ChannelOpenTimeout)

	require.NoError(h, err, "timeout checking error from open channel")
}

// OpenChannelPsbt attempts to open a channel between srcNode and destNode with
// the passed channel funding parameters. It will assert if the expected step
// of funding the PSBT is not received from the source node.
func (h *HarnessTest) OpenChannelPsbt(srcNode, destNode *HarnessNode,
	p OpenChannelParams) (OpenChanClient, []byte) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	err := srcNode.WaitForBlockchainSync()
	require.NoError(h, err, "unable to sync srcNode chain")
	err = destNode.WaitForBlockchainSync()
	require.NoError(h, err, "unable to sync destNode chain")

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
	respStream, err := srcNode.rpc.LN.OpenChannel(h.runCtx, req)
	require.NoErrorf(h, err, "unable to open channel between "+
		"%s and %s", srcNode.Name(), destNode.Name())

	// Consume the "PSBT funding ready" update. This waits until the node
	// notifies us that the PSBT can now be funded.
	resp := h.ReceiveChanUpdate(respStream)
	upd, ok := resp.Update.(*lnrpc.OpenStatusUpdate_PsbtFund)
	require.Truef(h, ok, "expected PSBT funding update, got %v", resp)

	return respStream, upd.PsbtFund.Psbt
}

// MineBlocksAndAssertTx mine 'num' of blocks and check that blocks are present
// in node blockchain. numTxs should be set to the number of transactions
// (excluding the coinbase) we expect to be included in the first mined block.
func (h *HarnessTest) MineBlocksAndAssertTx(num uint32,
	numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	var txids []*chainhash.Hash
	if numTxs > 0 {
		txids = h.AssertNumTxsInMempool(numTxs)
	}

	blocks := h.MineBlocks(num)

	// Finally, assert that all the transactions were included in the first
	// block.
	for _, txid := range txids {
		h.AssertTxInBlock(blocks[0], txid)
	}

	return blocks
}

// MineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func (h *HarnessTest) MineBlocks(num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)

	blockHashes := h.GenerateBlocks(num)

	for i, blockHash := range blockHashes {
		block, err := h.net.Miner.Client.GetBlock(blockHash)
		require.NoError(h, err, "unable to get block")

		blocks[i] = block
	}

	return blocks
}

// MineBlocksSlow mines 'num' of blocks. Between each mined block an artificial
// delay is introduced to give all network participants time to catch up.
func (h *HarnessTest) MineBlocksSlow(num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)
	blockHashes := make([]*chainhash.Hash, 0, num)

	for i := uint32(0); i < num; i++ {
		generatedHashes := h.GenerateBlocks(1)
		blockHashes = append(blockHashes, generatedHashes...)

		time.Sleep(slowMineDelay)
	}

	for i, blockHash := range blockHashes {
		block, err := h.net.Miner.Client.GetBlock(blockHash)
		require.NoError(h, err, "get blocks")

		blocks[i] = block
	}

	return blocks
}

// ConnectMiner connects the miner with the chain backend in the network.
func (h *HarnessTest) ConnectMiner() {
	err := h.net.BackendCfg.ConnectMiner()
	require.NoError(h, err, "failed to connect miner")
}

// DisconnectMiner removes the connection between the miner and the chain
// backend in the network.
func (h *HarnessTest) DisconnectMiner() {
	err := h.net.BackendCfg.DisconnectMiner()
	require.NoError(h, err, "failed to disconnect miner")
}

// Random32Bytes generates a random 32 bytes which can be used as a pay hash,
// preimage, etc.
func (h *HarnessTest) Random32Bytes() []byte {
	randBuf := make([]byte, 32)

	_, err := rand.Read(randBuf)
	require.NoErrorf(h, err, "internal error, cannot generate random bytes")

	return randBuf
}

// SendPaymentAndAssertStatus sends a payment from the passed node and asserts
// the desired status is reached.
func (h *HarnessTest) SendPaymentAndAssertStatus(hn *HarnessNode,
	req *routerrpc.SendPaymentRequest,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	stream := h.SendPayment(hn, req)
	return h.AssertPaymentStatusFromStream(stream, status)
}

// SendPaymentAndAssert sends a payment from the passed node and asserts the
// payment being succeeded.
func (h *HarnessTest) SendPaymentAndAssert(hn *HarnessNode,
	req *routerrpc.SendPaymentRequest) *lnrpc.Payment {

	return h.SendPaymentAndAssertStatus(hn, req, lnrpc.Payment_SUCCEEDED)
}

// SendPaymentAssertFail sends a payment from the passed node and asserts the
// payment is failed with the specified failure reason .
func (h *HarnessTest) SendPaymentAssertFail(hn *HarnessNode,
	req *routerrpc.SendPaymentRequest,
	reason lnrpc.PaymentFailureReason) *lnrpc.Payment {

	payment := h.SendPaymentAndAssertStatus(hn, req, lnrpc.Payment_FAILED)
	require.Equal(h, reason, payment.FailureReason,
		"payment failureReason not matched")

	return payment
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

// closeChannel attempts to close a channel identified by the passed channel
// point owned by the passed Lightning node. A fully blocking channel closure
// is attempted, therefore the passed context should be a child derived via
// timeout from a base parent. Additionally, once the channel has been detected
// as closed, an assertion checks that the transaction is found within a block.
// Finally, this assertion verifies that the node always sends out a disable
// update when closing the channel if the channel was previously enabled.
//
// NOTE: This method assumes that the provided funding point is confirmed
// on-chain AND that the edge exists in the node's channel graph. If the funding
// transactions was reorged out at some point, use closeReorgedChannelAndAssert.
func (h *HarnessTest) CloseChannel(node *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	return h.CloseChannelAndAssertType(node, fundingChanPoint, false, force)
}

func (h *HarnessTest) CloseChannelAndAssertType(node *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint,
	anchors, force bool) *chainhash.Hash {

	// Fetch the current channel policy. If the channel is currently
	// enabled, we will register for graph notifications before closing to
	// assert that the node sends out a disabling update as a result of the
	// channel being closed.
	curPolicy := h.getChannelPolicies(
		node, node.PubKeyStr, fundingChanPoint,
	)[0]
	expectDisable := !curPolicy.Disabled

	closeUpdates, _, err := h.net.CloseChannel(
		h.runCtx, node, fundingChanPoint, force,
	)
	require.NoError(h, err, "unable to close channel")

	// If the channel policy was enabled prior to the closure, wait until we
	// received the disabled update.
	if expectDisable {
		curPolicy.Disabled = true
		h.AssertChannelPolicyUpdate(
			node, node,
			curPolicy, fundingChanPoint, false,
		)
	}

	return h.assertChannelClosed(
		node, fundingChanPoint, anchors, closeUpdates,
	)
}

// CloseChannelAssertErr closes the given channel and asserts an error
// returned.
func (h *HarnessTest) CloseChannelAssertErr(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) {

	_, _, err := h.net.CloseChannel(h.runCtx, hn, fundingChanPoint, force)
	require.Error(h, err, "expect close channel to return an error")
}

// closeReorgedChannelAndAssert attempts to close a channel identified by the
// passed channel point owned by the passed Lightning node. Once the channel
// has been detected as closed, an assertion checks that the transaction is
// found within a block.
//
// NOTE: This method does not verify that the node sends a disable update for
// the closed channel.
func (h *HarnessTest) CloseReorgedChannel(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := h.net.CloseChannel(
		h.runCtx, hn, fundingChanPoint, force,
	)
	require.NoError(h, err, "unable to close channel")

	return h.assertChannelClosed(
		hn, fundingChanPoint, false, closeUpdates,
	)
}

// assertChannelClosed asserts that the channel is properly cleaned up after
// initiating a cooperative or local close.
func (h *HarnessTest) assertChannelClosed(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, anchors bool,
	closeUpdates lnrpc.Lightning_CloseChannelClient) *chainhash.Hash {

	txid := h.GetChanPointFundingTxid(fundingChanPoint)
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If the channel appears in list channels, ensure that its state
	// contains ChanStatusCoopBroadcasted.
	listChansResp := h.ListChannels(hn)

	for _, channel := range listChansResp.Channels {
		// Skip other channels.
		if channel.ChannelPoint != chanPointStr {
			continue
		}

		// Assert that the channel is in coop broadcasted.
		require.Contains(
			h, channel.ChanStatusFlags,
			channeldb.ChanStatusCoopBroadcasted.String(),
			"channel not coop broadcasted",
		)
	}

	// At this point, the channel should now be marked as being in the
	// state of "waiting close".
	pendingChanResp := h.GetPendingChannels(hn)

	var found bool
	for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
		if pendingClose.Channel.ChannelPoint == chanPointStr {
			found = true
			break
		}
	}
	require.True(h, found, "channel not marked as waiting close")

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block. If there are anchors, we also expect an anchor sweep.
	expectedTxes := 1
	if anchors {
		expectedTxes = 2
	}

	block := h.MineBlocksAndAssertTx(1, expectedTxes)[0]

	closingTxid := h.WaitForChannelClose(closeUpdates)
	h.AssertTxInBlock(block, closingTxid)

	// Finally, the transaction should no longer be in the waiting close
	// state as we've just mined a block that should include the closing
	// transaction.
	err := wait.NoError(func() error {
		pendingChanResp := h.GetPendingChannels(hn)

		for _, pending := range pendingChanResp.WaitingCloseChannels {
			if pending.Channel.ChannelPoint == chanPointStr {
				return fmt.Errorf("found channel %s still in "+
					"waiting closing", chanPointStr)
			}
		}

		return nil
	}, DefaultTimeout)
	require.NoError(
		h, err, "closing transaction not marked as fully closed",
	)

	return closingTxid
}

// getChannelPolicies queries the channel graph and retrieves the current edge
// policies for the provided channel points.
func (h *HarnessTest) getChannelPolicies(hn *HarnessNode,
	advertisingNode string,
	chanPoints ...*lnrpc.ChannelPoint) []*lnrpc.RoutingPolicy {

	chanGraph := h.DescribeGraph(hn, true)

	var policies []*lnrpc.RoutingPolicy
	err := wait.NoError(func() error {
	out:
		for _, chanPoint := range chanPoints {
			for _, e := range chanGraph.Edges {
				if e.ChanPoint != txStr(chanPoint) {
					continue
				}

				if e.Node1Pub == advertisingNode {
					policies = append(policies,
						e.Node1Policy)
				} else {
					policies = append(policies,
						e.Node2Policy)
				}

				continue out
			}

			// If we've iterated over all the known edges and we
			// weren't able to find this specific one, then we'll
			// fail.
			return fmt.Errorf("did not find edge %v",
				txStr(chanPoint))
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err)

	return policies
}

// findPayment queries the payment from the node's ListPayments which matches
// the specified preimage hash.
func (h *HarnessTest) findPayment(hn *HarnessNode,
	preimage lntypes.Preimage) *lnrpc.Payment {

	paymentsResp := h.ListPayments(hn, true)

	payHash := preimage.Hash()
	for _, p := range paymentsResp.Payments {
		if p.PaymentHash != payHash.String() {
			continue
		}
		return p
	}

	require.Fail(h, "payment: %v not found", payHash)
	return nil
}

// txStr returns the string representation of the channel's funding transaction.
func txStr(chanPoint *lnrpc.ChannelPoint) string {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	return cp.String()
}

// CleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func (h *HarnessTest) CleanupForceClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	// Wait for the channel to be marked pending force close.
	h.WaitForChannelPendingForceClose(hn, chanPoint)

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	//
	// The commit sweep resolver is able to broadcast the sweep tx up to
	// one block before the CSV elapses, so wait until defaulCSV-1.
	h.MineBlocks(DefaultCSV - 1)

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	h.MineBlocksAndAssertTx(1, 1)

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
func (h *HarnessTest) mineTillForceCloseResolved(hn *HarnessNode) {
	_, startHeight := h.GetBestBlock()

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)
		total := len(resp.WaitingCloseChannels)

		if total != 0 {
			h.MineBlocksSlow(1)
			return fmt.Errorf("expected num of waiting close " +
				"channel to be zero")
		}

		total = len(resp.PendingForceClosingChannels)
		if total != 0 {
			h.MineBlocksSlow(1)
			return fmt.Errorf("expected num of pending force " +
				"close channel to be zero")
		}

		_, height := h.GetBestBlock()
		h.Logf("Mined %d blocks while waiting for force closed "+
			"channel to be resolved", height-startHeight)

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "assert force close resolved timeout")
}

// GetChanPointFundingTxid takes a channel point and converts it into a chain
// hash.
func (h *HarnessTest) GetChanPointFundingTxid(
	cp *lnrpc.ChannelPoint) *chainhash.Hash {

	txid, err := lnrpc.GetChanPointFundingTxid(cp)
	require.NoError(h, err, "unable to get txid")

	return txid
}

// findChannel tries to find a target channel in the node using the given
// channel point.
func (h *HarnessTest) findChannel(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) (*lnrpc.Channel, error) {

	// Get the funding point.
	fp := h.OutPointFromChannelPoint(chanPoint)
	channelInfo := h.ListChannels(hn)

	// Find the target channel first.
	for _, channel := range channelInfo.Channels {
		if channel.ChannelPoint == fp.String() {
			return channel, nil
		}
	}

	return nil, fmt.Errorf("channel not found")
}

// CreatePayReqs is a helper method that will create a slice of payment
// requests for the given node.
func (h *HarnessTest) CreatePayReqs(hn *HarnessNode, paymentAmt btcutil.Amount,
	numInvoices int) ([]string, [][]byte, []*lnrpc.Invoice) {

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
		resp := h.AddInvoice(invoice, hn)

		// Set the payment address in the invoice so the caller can
		// properly use it.
		invoice.PaymentAddr = resp.PaymentAddr

		payReqs[i] = resp.PaymentRequest
		rHashes[i] = resp.RHash
		invoices[i] = invoice
	}

	return payReqs, rHashes, invoices
}

// CompletePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function does
// not return until all payments successfully complete without errors.
func (h *HarnessTest) CompletePaymentRequests(hn *HarnessNode,
	paymentRequests []string, awaitResponse bool) {

	// We start by getting the current state of the client's channels. This
	// is needed to ensure the payments actually have been committed before
	// we return.
	listResp := h.ListChannels(hn)

	// send sends a payment and asserts if it doesn't succeeded.
	send := func(payReq string) {
		payStream := h.SendPayment(
			hn,
			&routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)

		// If we are not waiting for response, exit early.
		if !awaitResponse {
			return
		}

		h.AssertPaymentStatusFromStream(
			payStream, lnrpc.Payment_SUCCEEDED,
		)
	}

	// Launch all payments sequentially.
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		send(payReqCopy)
	}

	// We are not waiting for feedback in the form of a response, but we
	// should still wait long enough for the server to receive and handle
	// the send before cancelling the request. We wait for the number of
	// updates to one of our channels has increased before we return.
	err := wait.NoError(func() error {
		newListResp := h.ListChannels(hn)

		// If the number of open channels is now lower than before
		// attempting the payments, it means one of the payments
		// triggered a force closure (for example, due to an incorrect
		// preimage). Return early since it's clear the payment was
		// attempted.
		if len(newListResp.Channels) < len(listResp.Channels) {
			return nil
		}

		var (
			cp  string
			err error
		)

		for _, c1 := range listResp.Channels {
			cp = c1.ChannelPoint
			for _, c2 := range newListResp.Channels {
				if c1.ChannelPoint != c2.ChannelPoint {
					continue
				}

				// If this channel has an increased numbr of
				// updates, we assume the payments are
				// committed, and we can return.
				if c2.NumUpdates > c1.NumUpdates {
					return nil
				}
				// If we reach this line, there are channels
				// that have matched but NumUpdates are not
				// increased. We don't want to fail here as
				// there might be other channels which do have
				// NumUpdates increased.
				// TODO(yy): refactor this method to be channel
				// specific.
				err = fmt.Errorf("%s: channel:%v not updated "+
					"after sending payments, old "+
					"updates: %v, new updates: %v",
					hn.Name(), c2.ChannelPoint,
					c1.NumUpdates, c2.NumUpdates)
			}
		}

		// If the err is not nil, it means we've checked all the
		// channels and got at least one channel that had no increased
		// NumUpdates.
		if err != nil {
			return err
		}

		// Otherwise, we didn't find a matched channel at all.
		return fmt.Errorf("%s: channel:%v not found in newListResp",
			hn.Name(), cp)
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking for channel updates")
}

// BackupDb created a db backup for the specified node and asserts.
func (h *HarnessTest) BackupDb(hn *HarnessNode) {
	require.NoError(h, h.net.BackupDb(hn), "failed to copy db files")
}

// RestoreDb restores a db backup for the specified node and asserts.
func (h *HarnessTest) RestoreDb(hn *HarnessNode) {
	require.NoError(h, h.net.RestoreDb(hn), "failed to restore db")
}

// MineEmptyBlocks mines a given number of empty blocks.
func (h *HarnessTest) MineEmptyBlocks(num int) []*wire.MsgBlock {
	var emptyTime time.Time

	blocks := make([]*wire.MsgBlock, num)
	for i := 0; i < num; i++ {
		// Generate an empty block.
		b, err := h.net.Miner.GenerateAndSubmitBlock(nil, -1, emptyTime)
		require.NoError(h, err, "unable to mine empty block")

		block, err := h.net.Miner.Client.GetBlock(b.Hash())
		require.NoError(h, err, "unable to get block")

		blocks[i] = block
	}

	return blocks
}

// MineBlocksWithTxes mines a single block to include the specifies
// transactions only.
func (h *HarnessTest) MineBlockWithTxes(txes []*btcutil.Tx) *wire.MsgBlock {
	var emptyTime time.Time

	// Generate a block.
	b, err := h.net.Miner.GenerateAndSubmitBlock(txes, -1, emptyTime)
	require.NoError(h, err, "unable to mine blocks")

	block, err := h.net.Miner.Client.GetBlock(b.Hash())
	require.NoError(h, err, "unable to get block")

	return block
}

// sendCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node. The confirmed boolean indicates whether the
// transaction that pays to the target should confirm.
func (h *HarnessTest) sendCoins(amt btcutil.Amount, target *HarnessNode,
	addrType lnrpc.AddressType, confirmed bool) error {

	initialBalance := h.WalletBalance(target)

	// First, obtain an address from the target lightning node, preferring
	// to receive a p2wkh address s.t the output can immediately be used as
	// an input to a funding transaction.
	resp := h.NewAddress(target, addrType)
	addr, err := btcutil.DecodeAddress(resp.Address, h.net.netParams)
	if err != nil {
		return err
	}
	addrScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return err
	}

	// Generate a transaction which creates an output to the target
	// pkScript of the desired amount.
	output := &wire.TxOut{
		PkScript: addrScript,
		Value:    int64(amt),
	}
	_, err = h.net.Miner.SendOutputs([]*wire.TxOut{output}, 7500)
	if err != nil {
		return err
	}

	// Encode the pkScript in hex as this the format that it will be
	// returned via rpc.
	expPkScriptStr := hex.EncodeToString(addrScript)

	// Now, wait for ListUnspent to show the unconfirmed transaction
	// containing the correct pkscript.
	//
	// Since neutrino doesn't support unconfirmed outputs, skip this check.
	if !h.IsNeutrinoBackend() {
		utxos := h.AssertNumUTXOs(target, 1, 0, 0)

		// Assert that the lone unconfirmed utxo contains the same
		// pkscript as the output generated above.
		pkScriptStr := utxos[0].PkScript
		if strings.Compare(pkScriptStr, expPkScriptStr) != 0 {
			return fmt.Errorf("pkscript mismatch, want: %s, "+
				"found: %s", expPkScriptStr, pkScriptStr)
		}

	}

	// If the transaction should remain unconfirmed, then we'll wait until
	// the target node's unconfirmed balance reflects the expected balance
	// and exit.
	if !confirmed && !h.IsNeutrinoBackend() {
		expectedBalance := btcutil.Amount(
			initialBalance.UnconfirmedBalance) + amt
		return target.WaitForBalance(expectedBalance, false)
	}

	// Otherwise, we'll generate 2 new blocks to ensure the output gains a
	// sufficient number of confirmations and wait for the balance to
	// reflect what's expected.
	h.MineBlocks(2)

	expectedBalance := btcutil.Amount(initialBalance.ConfirmedBalance) + amt
	return target.WaitForBalance(expectedBalance, true)
}

// OpenChannelRequest is used to open a channel using the method
// OpenMultiChannelsAsync.
type OpenChannelRequest struct {
	// Local is the funding node.
	Local *HarnessNode

	// Remote is the receiving node.
	Remote *HarnessNode

	// Param is the open channel params.
	Param OpenChannelParams

	// stream is the client created after calling OpenChannel RPC.
	stream OpenChanClient

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
// it does make debugging the logs more difficult as messages are interwined.
func (h *HarnessTest) OpenMultiChannelsAsync(
	reqs []*OpenChannelRequest) []*lnrpc.ChannelPoint {

	// assertChannelOpen is a helper closure that asserts a channel being
	// open inside a goroutine.
	assertChannelOpen := func(stream OpenChanClient,
		a, b *HarnessNode, chanPoint chan *lnrpc.ChannelPoint) {

		go func() {
			cp := h.WaitForChannelOpen(stream)

			// Check that both alice and bob have seen the channel
			// from their channel watch request.
			h.AssertChannelOpen(a, cp)
			h.AssertChannelOpen(b, cp)

			// Finally, check that the channel can be seen in their
			// ListChannels.
			h.AssertChannelExists(a, cp)
			h.AssertChannelExists(b, cp)

			chanPoint <- cp
		}()
	}

	// Go through the requests and make the OpenChannel RPC call.
	for _, req := range reqs {
		stream := h.OpenChannelStreamAndAssert(
			req.Local, req.Remote, req.Param,
		)
		req.stream = stream
		req.result = make(chan *lnrpc.ChannelPoint, 1)
	}

	// Once the RPC calls are sent, we fire goroutines for each of the
	// request to watch for the channel openning. They won't succeed until
	// the required blocks are mined.
	for _, r := range reqs {
		assertChannelOpen(r.stream, r.Local, r.Remote, r.result)
	}

	// Mine 6 blocks so all the public channels are announced to the
	// network. We expect to see len(reqs) funding transactions in the
	// mempool.
	h.MineBlocksAndAssertTx(6, len(reqs))

	// Finally, collect the results.
	channelPoints := make([]*lnrpc.ChannelPoint, 0)
	for _, r := range reqs {
		select {
		case cp := <-r.result:
			channelPoints = append(channelPoints, cp)
		case <-time.After(ChannelOpenTimeout):
			require.Fail(h, "wait channel point timeout")
		}
	}

	// Assert that we have the expected num of channel points.
	require.Len(h, channelPoints, len(reqs),
		"returned channel points not match")

	return channelPoints
}
