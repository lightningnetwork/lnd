package lntest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"testing"

	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

// DefaultCSV is the CSV delay (remotedelay) we will start our test nodes with.
const DefaultCSV = 4

// NodeOption is a function for updating a node's configuration.
type NodeOption func(*BaseNodeConfig)

// NetworkHarness is an integration testing harness for the lightning network.
// Building on top of HarnessNode, it is responsible for handling interactions
// among different nodes. The harness by default is created with two active
// nodes on the network:
// Alice and Bob.
type NetworkHarness struct {
	netParams *chaincfg.Params

	// currentTestCase holds the name for the currently run test case.
	currentTestCase string

	// lndBinary is the full path to the lnd binary that was specifically
	// compiled with all required itest flags.
	lndBinary string

	// Miner is a reference to a running full node that can be used to
	// create new blocks on the network.
	Miner *HarnessMiner

	// BackendCfg houses the information necessary to use a node as LND
	// chain backend, such as rpc configuration, P2P information etc.
	BackendCfg BackendConfig

	activeNodes map[int]*HarnessNode

	nodesByPub map[string]*HarnessNode

	// dbBackend sets the database backend to use.
	dbBackend DatabaseBackend

	// Channel for transmitting stderr output from failed lightning node
	// to main process.
	lndErrorChan chan error

	// feeService is a web service that provides external fee estimates to
	// lnd.
	feeService *feeService

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc

	mtx sync.Mutex
}

// NewNetworkHarness creates a new network test harness.
// TODO(roasbeef): add option to use golang's build library to a binary of the
// current repo. This will save developers from having to manually `go install`
// within the repo each time before changes.
func NewNetworkHarness(m *HarnessMiner, b BackendConfig, lndBinary string,
	dbBackend DatabaseBackend) (*NetworkHarness, error) {

	ctxt, cancel := context.WithCancel(context.Background())

	n := NetworkHarness{
		activeNodes: make(map[int]*HarnessNode),
		nodesByPub:  make(map[string]*HarnessNode),
		// We need to use buffered channel here as we don't want to
		// block sending errors.
		lndErrorChan: make(chan error, 10),
		netParams:    m.ActiveNet,
		Miner:        m,
		BackendCfg:   b,
		runCtx:       ctxt,
		cancel:       cancel,
		lndBinary:    lndBinary,
		dbBackend:    dbBackend,
	}
	return &n, nil
}

// LookUpNodeByPub queries the set of active nodes to locate a node according
// to its public key. The error is returned if the node was not found.
func (n *NetworkHarness) LookUpNodeByPub(pubStr string) (*HarnessNode, error) {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	node, ok := n.nodesByPub[pubStr]
	if !ok {
		return nil, fmt.Errorf("unable to find node")
	}

	return node, nil
}

// ProcessErrors returns a channel used for reporting any fatal process errors.
// If any of the active nodes within the harness' test network incur a fatal
// error, that error is sent over this channel.
func (n *NetworkHarness) ProcessErrors() <-chan error {
	return n.lndErrorChan
}

// TearDown tears down all active nodes within the test lightning network.
func (n *NetworkHarness) TearDown() error {
	for _, node := range n.activeNodes {
		if err := n.ShutdownNode(node); err != nil {
			return err
		}
	}

	return nil
}

// Stop stops the test harness.
func (n *NetworkHarness) Stop(t *testing.T) {
	if err := n.TearDown(); err != nil {
		t.Logf("Tearing down harness net got error: %v", err)
	}

	close(n.lndErrorChan)
	n.cancel()

	// feeService may not be created. For instance, running a non-exist
	// test case.
	if n.feeService != nil {
		n.feeService.stop()
	}
}

// SetTestName set the test case name.
func (n *NetworkHarness) SetTestName(name string) {
	n.currentTestCase = name
}

// extraArgsEtcd returns extra args for configuring LND to use an external etcd
// database (for remote channel DB and wallet DB).
func extraArgsEtcd(etcdCfg *etcd.Config, name string, cluster bool,
	leaderSessionTTL int) []string {

	extraArgs := []string{
		"--db.backend=etcd",
		fmt.Sprintf("--db.etcd.host=%v", etcdCfg.Host),
		fmt.Sprintf("--db.etcd.user=%v", etcdCfg.User),
		fmt.Sprintf("--db.etcd.pass=%v", etcdCfg.Pass),
		fmt.Sprintf("--db.etcd.namespace=%v", etcdCfg.Namespace),
	}

	if etcdCfg.InsecureSkipVerify {
		extraArgs = append(extraArgs, "--db.etcd.insecure_skip_verify")
	}

	if cluster {
		clusterArgs := []string{
			"--cluster.enable-leader-election",
			fmt.Sprintf("--cluster.id=%v", name),
			fmt.Sprintf("--cluster.leader-session-ttl=%v",
				leaderSessionTTL),
		}
		extraArgs = append(extraArgs, clusterArgs...)
	}

	return extraArgs
}

func (n *NetworkHarness) newNodeWithSeed(name string, extraArgs []string,
	password, entropy []byte, statelessInit bool, dbBackend DatabaseBackend) (
	*HarnessNode, []string, []byte, error) {

	node, err := n.newNode(
		name, extraArgs, true, password, dbBackend, true,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create new node: %w", err)
	}

	// Create a request to generate a new aezeed. The new seed will have the
	// same password as the internal wallet.
	genSeedReq := &lnrpc.GenSeedRequest{
		AezeedPassphrase: password,
		SeedEntropy:      entropy,
	}

	ctxt, cancel := context.WithTimeout(n.runCtx, DefaultTimeout)
	defer cancel()

	var genSeedResp *lnrpc.GenSeedResponse
	if err := wait.NoError(func() error {
		genSeedResp, err = node.rpc.WalletUnlocker.GenSeed(
			ctxt, genSeedReq,
		)
		return err
	}, DefaultTimeout); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to gen seed: %w", err)
	}

	// With the seed created, construct the init request to the node,
	// including the newly generated seed.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: genSeedResp.CipherSeedMnemonic,
		AezeedPassphrase:   password,
		StatelessInit:      statelessInit,
	}

	// Pass the init request via rpc to finish unlocking the node. This will
	// also initialize the macaroon-authenticated LightningClient.
	response, err := node.Init(initReq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed init node: %w", err)
	}

	// With the node started, we can now record its public key within the
	// global mapping.
	n.RegisterNode(node)

	// In stateless initialization mode we get a macaroon back that we have
	// to return to the test, otherwise gRPC calls won't be possible since
	// there are no macaroon files created in that mode.
	// In stateful init the admin macaroon will just be nil.
	return node, genSeedResp.CipherSeedMnemonic, response.AdminMacaroon, nil
}

func (n *NetworkHarness) NewNodeRemoteSigner(name string, extraArgs []string,
	password []byte, watchOnly *lnrpc.WatchOnly) (*HarnessNode, error) {

	node, err := n.newNode(
		name, extraArgs, true, password, n.dbBackend, true,
	)
	if err != nil {
		return nil, err
	}

	// With the seed created, construct the init request to the node,
	// including the newly generated seed.
	initReq := &lnrpc.InitWalletRequest{
		WalletPassword: password,
		WatchOnly:      watchOnly,
	}

	// Pass the init request via rpc to finish unlocking the node. This will
	// also initialize the macaroon-authenticated LightningClient.
	_, err = node.Init(initReq)
	if err != nil {
		return nil, err
	}

	// With the node started, we can now record its public key within the
	// global mapping.
	n.RegisterNode(node)

	return node, nil
}

// newNode initializes a new HarnessNode, supporting the ability to initialize a
// wallet with or without a seed. If hasSeed is false, the returned harness node
// can be used immediately. Otherwise, the node will require an additional
// initialization phase where the wallet is either created or restored.
func (n *NetworkHarness) newNode(name string, extraArgs []string, hasSeed bool,
	password []byte, dbBackend DatabaseBackend, wait bool, opts ...NodeOption) (
	*HarnessNode, error) {

	cfg := &BaseNodeConfig{
		Name:              name,
		LogFilenamePrefix: n.currentTestCase,
		HasSeed:           hasSeed,
		Password:          password,
		BackendCfg:        n.BackendCfg,
		NetParams:         n.netParams,
		ExtraArgs:         extraArgs,
		FeeURL:            n.feeService.url,
		DbBackend:         dbBackend,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	node, err := newNode(cfg)
	if err != nil {
		return nil, err
	}

	// Put node in activeNodes to ensure Shutdown is called even if Start
	// returns an error.
	n.mtx.Lock()
	n.activeNodes[node.NodeID] = node
	n.mtx.Unlock()

	err = node.start(n.lndBinary, n.lndErrorChan, wait)
	if err != nil {
		return nil, fmt.Errorf("failed to start: %w", err)
	}

	// If this node is to have a seed, it will need to be unlocked or
	// initialized via rpc. Delay registering it with the network until it
	// can be driven via an unlocked rpc connection.
	if node.Cfg.HasSeed {
		return node, nil
	}

	// With the node started, we can now record its public key within the
	// global mapping.
	n.RegisterNode(node)

	return node, nil
}

// RegisterNode records a new HarnessNode in the NetworkHarnesses map of known
// nodes. This method should only be called with nodes that have successfully
// retrieved their public keys via FetchNodeInfo.
func (n *NetworkHarness) RegisterNode(node *HarnessNode) {
	n.mtx.Lock()
	n.nodesByPub[node.PubKeyStr] = node
	n.mtx.Unlock()
}

func (n *NetworkHarness) connect(ctx context.Context,
	req *lnrpc.ConnectPeerRequest, a *HarnessNode) error {

	err := wait.NoError(func() error {
		_, err := a.rpc.LN.ConnectPeer(ctx, req)
		if err == nil {
			return nil
		}
		return fmt.Errorf("%s:connect node failed: %w", a.Name(), err)
	}, DefaultTimeout)

	if err != nil {
		return fmt.Errorf("timeout connecting nodes: %v", err)
	}
	return nil
}

// DisconnectNodes disconnects node a from node b by sending RPC message
// from a node to b node.
func (n *NetworkHarness) DisconnectNodes(a, b *HarnessNode) error {
	ctx, cancel := context.WithTimeout(n.runCtx, DefaultTimeout)
	defer cancel()

	bobInfo, err := b.rpc.LN.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	req := &lnrpc.DisconnectPeerRequest{
		PubKey: bobInfo.IdentityPubkey,
	}

	if _, err := a.rpc.LN.DisconnectPeer(ctx, req); err != nil {
		return err
	}

	return nil
}

// RestartNode attempts to restart a lightning node by shutting it down
// cleanly, then restarting the process. This function is fully blocking. Upon
// restart, the RPC connection to the node will be re-attempted, continuing iff
// the connection attempt is successful. If the callback parameter is non-nil,
// then the function will be executed after the node shuts down, but *before*
// the process has been started up again.
//
// This method can be useful when testing edge cases such as a node broadcast
// and invalidated prior state, or persistent state recovery, simulating node
// crashes, etc. Additionally, each time the node is restarted, the caller can
// pass a set of SCBs to pass in via the Unlock method allowing them to restore
// channels during restart.
func (n *NetworkHarness) RestartNode(node *HarnessNode, callback func() error,
	chanBackups ...*lnrpc.ChanBackupSnapshot) error {

	err := n.RestartNodeNoUnlock(node, callback, true)
	if err != nil {
		return err
	}

	// If the node doesn't have a password set, then we can exit here as we
	// don't need to unlock it.
	if len(node.Cfg.Password) == 0 {
		return nil
	}

	// Otherwise, we'll unlock the wallet, then complete the final steps
	// for the node initialization process.
	unlockReq := &lnrpc.UnlockWalletRequest{
		WalletPassword: node.Cfg.Password,
	}
	if len(chanBackups) != 0 {
		unlockReq.ChannelBackups = chanBackups[0]
		unlockReq.RecoveryWindow = 1000
	}

	if err := node.Unlock(unlockReq); err != nil {
		return err
	}

	// Give the node some time to catch up with the chain before we
	// continue with the tests.
	return node.WaitForBlockchainSync()
}

// RestartNodeNoUnlock attempts to restart a lightning node by shutting it down
// cleanly, then restarting the process. In case the node was setup with a seed,
// it will be left in the unlocked state. This function is fully blocking. If
// the callback parameter is non-nil, then the function will be executed after
// the node shuts down, but *before* the process has been started up again.
func (n *NetworkHarness) RestartNodeNoUnlock(node *HarnessNode,
	callback func() error, wait bool) error {

	if err := node.stop(); err != nil {
		return err
	}

	if callback != nil {
		if err := callback(); err != nil {
			return err
		}
	}

	return node.start(n.lndBinary, n.lndErrorChan, wait)
}

// SuspendNode stops the given node and returns a callback that can be used to
// start it again.
func (n *NetworkHarness) SuspendNode(node *HarnessNode) (func() error, error) {
	if err := node.stop(); err != nil {
		return nil, err
	}

	restart := func() error {
		return node.start(n.lndBinary, n.lndErrorChan, true)
	}

	return restart, nil
}

// ShutdownNode stops an active lnd process and returns when the process has
// exited and any temporary directories have been cleaned up.
func (n *NetworkHarness) ShutdownNode(node *HarnessNode) error {
	if err := node.shutdown(); err != nil {
		return err
	}

	delete(n.activeNodes, node.NodeID)
	return nil
}

// KillNode kills the node (but won't wait for the node process to stop).
func (n *NetworkHarness) KillNode(node *HarnessNode) error {
	if err := node.kill(); err != nil {
		return err
	}

	delete(n.activeNodes, node.NodeID)
	return nil
}

// StopNode stops the target node, but doesn't yet clean up its directories.
// This can be used to temporarily bring a node down during a test, to be later
// started up again.
func (n *NetworkHarness) StopNode(node *HarnessNode) error {
	return node.stop()
}

// SaveProfilesPages hits profiles pages of all active nodes and writes it to
// disk using a similar naming scheme as to the regular set of logs.
func (n *NetworkHarness) SaveProfilesPages(t *testing.T) {
	// Only write gorutine dumps if flag is active.
	if !(*goroutineDump) {
		return
	}

	for _, node := range n.activeNodes {
		if err := saveProfilesPage(node); err != nil {
			t.Logf("Logging follow-up error only, see rest of "+
				"the log for actual cause: %v\n", err)
		}
	}
}

// saveProfilesPage saves the profiles page for the given node to file.
func saveProfilesPage(node *HarnessNode) error {
	resp, err := http.Get(
		fmt.Sprintf(
			"http://localhost:%d/debug/pprof/goroutine?debug=1",
			node.Cfg.ProfilePort,
		),
	)
	if err != nil {
		return fmt.Errorf("failed to get profile page "+
			"(node_id=%d, name=%s): %v",
			node.NodeID, node.Cfg.Name, err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read profile page "+
			"(node_id=%d, name=%s): %v",
			node.NodeID, node.Cfg.Name, err)
	}

	fileName := fmt.Sprintf(
		"pprof-%d-%s-%s.log", node.NodeID, node.Cfg.Name,
		hex.EncodeToString(node.PubKey[:logPubKeyBytes]),
	)

	logFile, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("failed to create file for profile page "+
			"(node_id=%d, name=%s): %v",
			node.NodeID, node.Cfg.Name, err)
	}
	defer logFile.Close()

	_, err = logFile.Write(body)
	if err != nil {
		return fmt.Errorf("failed to save profile page "+
			"(node_id=%d, name=%s): %v",
			node.NodeID, node.Cfg.Name, err)
	}
	return nil
}

// OpenChannel attempts to open a channel between srcNode and destNode with the
// passed channel funding parameters. If the passed context has a timeout, then
// if the timeout is reached before the channel pending notification is
// received, an error is returned. The confirmed boolean determines whether we
// should fund the channel with confirmed outputs or not.
func (n *NetworkHarness) OpenChannel(srcNode, destNode *HarnessNode,
	p OpenChannelParams) (lnrpc.Lightning_OpenChannelClient, error) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	if err := srcNode.WaitForBlockchainSync(); err != nil {
		return nil, fmt.Errorf("unable to sync srcNode chain: %v", err)
	}
	if err := destNode.WaitForBlockchainSync(); err != nil {
		return nil, fmt.Errorf("unable to sync destNode chain: %v", err)
	}

	minConfs := int32(1)
	if p.SpendUnconfirmed {
		minConfs = 0
	}

	openReq := &lnrpc.OpenChannelRequest{
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
	}

	// We need to use n.runCtx here to keep the response stream alive after
	// the function is returned.
	respStream, err := srcNode.rpc.LN.OpenChannel(n.runCtx, openReq)
	if err != nil {
		return nil, fmt.Errorf("unable to open channel between "+
			"%s and %s: %v", srcNode.Name(), destNode.Name(), err)
	}

	chanOpen := make(chan struct{})
	errChan := make(chan error)
	go func() {
		// Consume the "channel pending" update. This waits until the
		// node notifies us that the final message in the channel
		// funding workflow has been sent to the remote node.
		resp, err := respStream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		_, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		if !ok {
			errChan <- fmt.Errorf("expected channel pending: "+
				"update, instead got %v", resp)
			return
		}

		close(chanOpen)
	}()

	select {
	case <-time.After(ChannelOpenTimeout):
		return nil, fmt.Errorf("timeout reached before chan pending "+
			"update sent: %v", err)
	case err := <-errChan:
		return nil, err
	case <-chanOpen:
		return respStream, nil
	}
}

// OpenPendingChannel attempts to open a channel between srcNode and destNode
// with the passed channel funding parameters. If the passed context has a
// timeout, then if the timeout is reached before the channel pending
// notification is received, an error is returned.
func (n *NetworkHarness) OpenPendingChannel(srcNode, destNode *HarnessNode,
	amt btcutil.Amount,
	pushAmt btcutil.Amount) (*lnrpc.PendingUpdate, error) {

	// Wait until srcNode and destNode have blockchain synced
	if err := srcNode.WaitForBlockchainSync(); err != nil {
		return nil, fmt.Errorf("unable to sync srcNode chain: %v", err)
	}
	if err := destNode.WaitForBlockchainSync(); err != nil {
		return nil, fmt.Errorf("unable to sync destNode chain: %v", err)
	}

	openReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(amt),
		PushSat:            int64(pushAmt),
		Private:            false,
	}

	// We need to use n.runCtx here to keep the response stream alive after
	// the function is returned.
	respStream, err := srcNode.rpc.LN.OpenChannel(n.runCtx, openReq)
	if err != nil {
		return nil, fmt.Errorf("unable to open channel between "+
			"alice and bob: %v", err)
	}

	chanPending := make(chan *lnrpc.PendingUpdate)
	errChan := make(chan error)
	go func() {
		// Consume the "channel pending" update. This waits until the
		// node notifies us that the final message in the channel
		// funding workflow has been sent to the remote node.
		resp, err := respStream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		pendingResp, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		if !ok {
			errChan <- fmt.Errorf("expected channel pending "+
				"update, instead got %v", resp)
			return
		}

		chanPending <- pendingResp.ChanPending
	}()

	select {
	case <-time.After(ChannelOpenTimeout):
		return nil, fmt.Errorf("timeout reached before chan pending " +
			"update sent")
	case err := <-errChan:
		return nil, err
	case pendingChan := <-chanPending:
		return pendingChan, nil
	}
}

// WaitForChannelOpen waits for a notification that a channel is open by
// consuming a message from the past open channel stream. If the passed context
// has a timeout, then if the timeout is reached before the channel has been
// opened, then an error is returned.
func (n *NetworkHarness) WaitForChannelOpen(
	openChanStream lnrpc.Lightning_OpenChannelClient) (
	*lnrpc.ChannelPoint, error) {

	ctx, cancel := context.WithTimeout(n.runCtx, ChannelOpenTimeout)
	defer cancel()

	errChan := make(chan error)
	respChan := make(chan *lnrpc.ChannelPoint)
	go func() {
		resp, err := openChanStream.Recv()
		if err != nil {
			errChan <- fmt.Errorf("unable to read rpc resp: %v", err)
			return
		}
		fundingResp, ok := resp.Update.(*lnrpc.OpenStatusUpdate_ChanOpen)
		if !ok {
			errChan <- fmt.Errorf("expected channel open update, "+
				"instead got %v", resp)
			return
		}

		respChan <- fundingResp.ChanOpen.ChannelPoint
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached while waiting for " +
			"channel open")
	case err := <-errChan:
		return nil, err
	case chanPoint := <-respChan:
		return chanPoint, nil
	}
}

// CloseChannel attempts to close the channel indicated by the
// passed channel point, initiated by the passed lnNode. If the passed context
// has a timeout, an error is returned if that timeout is reached before the
// channel close is pending.
func (n *NetworkHarness) CloseChannel(lnNode *HarnessNode,
	cp *lnrpc.ChannelPoint, force bool) (lnrpc.Lightning_CloseChannelClient,
	*chainhash.Hash, error) {

	// The cancel is intentionally left out here because the returned
	// item(close channel client) relies on the context being active. This
	// will be fixed once we finish refactoring the NetworkHarness.
	ctxt, cancel := context.WithTimeout(n.runCtx, ChannelCloseTimeout)
	defer cancel()

	// Create a channel outpoint that we can use to compare to channels
	// from the ListChannelsResponse.
	txidHash, err := getChanPointFundingTxid(cp)
	if err != nil {
		return nil, nil, err
	}
	fundingTxID, err := chainhash.NewHash(txidHash)
	if err != nil {
		return nil, nil, err
	}
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: cp.OutputIndex,
	}

	// We'll wait for *both* nodes to read the channel as active if we're
	// performing a cooperative channel closure.
	if !force {
		timeout := DefaultTimeout
		listReq := &lnrpc.ListChannelsRequest{}

		// We define two helper functions, one two locate a particular
		// channel, and the other to check if a channel is active or
		// not.
		filterChannel := func(node *HarnessNode,
			op wire.OutPoint) (*lnrpc.Channel, error) {

			listResp, err := node.rpc.LN.ListChannels(ctxt, listReq)
			if err != nil {
				return nil, err
			}

			for _, c := range listResp.Channels {
				if c.ChannelPoint == op.String() {
					return c, nil
				}
			}

			return nil, fmt.Errorf("unable to find channel")
		}
		activeChanPredicate := func(node *HarnessNode) func() bool {
			return func() bool {
				channel, err := filterChannel(node, chanPoint)
				if err != nil {
					return false
				}

				return channel.Active
			}
		}

		// Next, we'll fetch the target channel in order to get the
		// harness node that will be receiving the channel close
		// request.
		targetChan, err := filterChannel(lnNode, chanPoint)
		if err != nil {
			return nil, nil, err
		}
		receivingNode, err := n.LookUpNodeByPub(targetChan.RemotePubkey)
		if err != nil {
			return nil, nil, err
		}

		// Before proceeding, we'll ensure that the channel is active
		// for both nodes.
		err = wait.Predicate(activeChanPredicate(lnNode), timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("channel of closing " +
				"node not active in time")
		}
		err = wait.Predicate(
			activeChanPredicate(receivingNode), timeout,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("channel of receiving " +
				"node not active in time")
		}
	}

	var (
		closeRespStream lnrpc.Lightning_CloseChannelClient
		closeTxid       *chainhash.Hash
	)

	err = wait.NoError(func() error {
		closeReq := &lnrpc.CloseChannelRequest{
			ChannelPoint: cp, Force: force,
		}
		// We need to use n.runCtx to keep the client stream alive
		// after the function has returned.
		closeRespStream, err = lnNode.rpc.LN.CloseChannel(
			n.runCtx, closeReq,
		)
		if err != nil {
			return fmt.Errorf("unable to close channel: %v", err)
		}

		// Consume the "channel close" update in order to wait for the
		// closing transaction to be broadcast, then wait for the
		// closing tx to be seen within the network.
		closeResp, err := closeRespStream.Recv()
		if err != nil {
			return fmt.Errorf("unable to recv() from close "+
				"stream: %v", err)
		}
		pendingClose, ok := closeResp.Update.(*lnrpc.CloseStatusUpdate_ClosePending)
		if !ok {
			return fmt.Errorf("expected channel close update, "+
				"instead got %v", pendingClose)
		}

		closeTxid, err = chainhash.NewHash(
			pendingClose.ClosePending.Txid,
		)
		if err != nil {
			return fmt.Errorf("unable to decode closeTxid: "+
				"%v", err)
		}
		if err := n.Miner.waitForTxInMempool(*closeTxid); err != nil {
			return fmt.Errorf("error while waiting for "+
				"broadcast tx: %v", err)
		}
		return nil
	}, ChannelCloseTimeout)
	if err != nil {
		return nil, nil, err
	}

	return closeRespStream, closeTxid, nil
}

// WaitForChannelClose waits for a notification from the passed channel close
// stream that the node has deemed the channel has been fully closed. If the
// passed context has a timeout, then if the timeout is reached before the
// notification is received then an error is returned.
func (n *NetworkHarness) WaitForChannelClose(
	closeChanStream lnrpc.Lightning_CloseChannelClient) (
	*chainhash.Hash, error) {

	errChan := make(chan error)
	updateChan := make(chan *lnrpc.CloseStatusUpdate_ChanClose)
	go func() {
		closeResp, err := closeChanStream.Recv()
		if err != nil {
			errChan <- err
			return
		}

		closeFin, ok := closeResp.Update.(*lnrpc.CloseStatusUpdate_ChanClose)
		if !ok {
			errChan <- fmt.Errorf("expected channel close update, "+
				"instead got %v", closeResp)
			return
		}

		updateChan <- closeFin
	}()

	// Wait until either the deadline for the context expires, an error
	// occurs, or the channel close update is received.
	select {
	case <-time.After(ChannelCloseTimeout):
		return nil, fmt.Errorf("timeout reached before update sent")
	case err := <-errChan:
		return nil, err
	case update := <-updateChan:
		return chainhash.NewHash(update.ChanClose.ClosingTxid)
	}
}

// DumpLogs reads the current logs generated by the passed node, and returns
// the logs as a single string. This function is useful for examining the logs
// of a particular node in the case of a test failure.
// Logs from lightning node being generated with delay - you should
// add time.Sleep() in order to get all logs.
func (n *NetworkHarness) DumpLogs(node *HarnessNode) (string, error) {
	logFile := fmt.Sprintf("%v/simnet/lnd.log", node.Cfg.LogDir)

	buf, err := ioutil.ReadFile(logFile)
	if err != nil {
		return "", err
	}

	return string(buf), nil
}

// copyAll copies all files and directories from srcDir to dstDir recursively.
// Note that this function does not support links.
func copyAll(dstDir, srcDir string) error {
	entries, err := ioutil.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())

		info, err := os.Stat(srcPath)
		if err != nil {
			return err
		}

		if info.IsDir() {
			err := os.Mkdir(dstPath, info.Mode())
			if err != nil && !os.IsExist(err) {
				return err
			}

			err = copyAll(dstPath, srcPath)
			if err != nil {
				return err
			}
		} else if err := CopyFile(dstPath, srcPath); err != nil {
			return err
		}
	}

	return nil
}

// BackupDb creates a backup of the current database.
func (n *NetworkHarness) BackupDb(hn *HarnessNode) error {
	if hn.backupDbDir != "" {
		return errors.New("backup already created")
	}

	restart, err := n.SuspendNode(hn)
	if err != nil {
		return err
	}

	if hn.postgresDbName != "" {
		// Backup database.
		backupDbName := hn.postgresDbName + "_backup"
		err := executePgQuery(
			"CREATE DATABASE " + backupDbName + " WITH TEMPLATE " +
				hn.postgresDbName,
		)
		if err != nil {
			return err
		}
	} else {
		// Backup files.
		tempDir, err := ioutil.TempDir("", "past-state")
		if err != nil {
			return fmt.Errorf("unable to create temp db folder: %v",
				err)
		}

		if err := copyAll(tempDir, hn.DBDir()); err != nil {
			return fmt.Errorf("unable to copy database files: %v",
				err)
		}

		hn.backupDbDir = tempDir
	}

	err = restart()
	if err != nil {
		return err
	}

	return nil
}

// RestoreDb restores a database backup.
func (n *NetworkHarness) RestoreDb(hn *HarnessNode) error {
	if hn.postgresDbName != "" {
		// Restore database.
		backupDbName := hn.postgresDbName + "_backup"
		err := executePgQuery(
			"DROP DATABASE " + hn.postgresDbName,
		)
		if err != nil {
			return err
		}
		err = executePgQuery(
			"ALTER DATABASE " + backupDbName + " RENAME TO " + hn.postgresDbName,
		)
		if err != nil {
			return err
		}
	} else {
		// Restore files.
		if hn.backupDbDir == "" {
			return errors.New("no database backup created")
		}

		if err := copyAll(hn.DBDir(), hn.backupDbDir); err != nil {
			return fmt.Errorf("unable to copy database files: %v", err)
		}

		if err := os.RemoveAll(hn.backupDbDir); err != nil {
			return fmt.Errorf("unable to remove backup dir: %v", err)
		}
		hn.backupDbDir = ""
	}

	return nil
}

// getChanPointFundingTxid returns the given channel point's funding txid in
// raw bytes.
func getChanPointFundingTxid(chanPoint *lnrpc.ChannelPoint) ([]byte, error) {
	var txid []byte

	// A channel point's funding txid can be get/set as a byte slice or a
	// string. In the case it is a string, decode it.
	switch chanPoint.GetFundingTxid().(type) {
	case *lnrpc.ChannelPoint_FundingTxidBytes:
		txid = chanPoint.GetFundingTxidBytes()
	case *lnrpc.ChannelPoint_FundingTxidStr:
		s := chanPoint.GetFundingTxidStr()
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}

		txid = h[:]
	}

	return txid, nil
}
