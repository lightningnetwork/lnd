package lntest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

// nodeManager is responsible for handling the start and stop of a given node.
// It also keeps track of the running nodes.
type nodeManager struct {
	sync.Mutex

	// chainBackend houses the information necessary to use a node as LND
	// chain backend, such as rpc configuration, P2P information etc.
	chainBackend node.BackendConfig

	// currentTestCase holds the name for the currently run test case.
	currentTestCase string

	// lndBinary is the full path to the lnd binary that was specifically
	// compiled with all required itest flags.
	lndBinary string

	// dbBackend sets the database backend to use.
	dbBackend node.DatabaseBackend

	// nativeSQL sets the database backend to use native SQL when
	// applicable.
	nativeSQL bool

	// activeNodes is a map of all running nodes, format:
	// {pubkey: *HarnessNode}.
	activeNodes map[uint32]*node.HarnessNode

	// nodeCounter is a monotonically increasing counter that's used as the
	// node's unique ID.
	nodeCounter atomic.Uint32

	// feeServiceURL is the url of the fee service.
	feeServiceURL string
}

// newNodeManager creates a new node manager instance.
func newNodeManager(lndBinary string, dbBackend node.DatabaseBackend,
	nativeSQL bool) *nodeManager {

	return &nodeManager{
		lndBinary:   lndBinary,
		dbBackend:   dbBackend,
		nativeSQL:   nativeSQL,
		activeNodes: make(map[uint32]*node.HarnessNode),
	}
}

// nextNodeID generates a unique sequence to be used as the node's ID.
func (nm *nodeManager) nextNodeID() uint32 {
	nodeID := nm.nodeCounter.Add(1)
	return nodeID
}

// newNode initializes a new HarnessNode, supporting the ability to initialize
// a wallet with or without a seed. If useSeed is false, the returned harness
// node can be used immediately. Otherwise, the node will require an additional
// initialization phase where the wallet is either created or restored.
func (nm *nodeManager) newNode(t *testing.T, name string, extraArgs []string,
	password []byte, noAuth bool) (*node.HarnessNode, error) {

	cfg := &node.BaseNodeConfig{
		Name:              name,
		LogFilenamePrefix: nm.currentTestCase,
		Password:          password,
		BackendCfg:        nm.chainBackend,
		ExtraArgs:         extraArgs,
		FeeURL:            nm.feeServiceURL,
		DBBackend:         nm.dbBackend,
		NativeSQL:         nm.nativeSQL,
		NodeID:            nm.nextNodeID(),
		LndBinary:         nm.lndBinary,
		NetParams:         miner.HarnessNetParams,
		SkipUnlock:        noAuth,
	}

	node, err := node.NewHarnessNode(t, cfg)
	if err != nil {
		return nil, err
	}

	// Put node in activeNodes to ensure Shutdown is called even if start
	// returns an error.
	nm.registerNode(node)

	return node, nil
}

// RegisterNode records a new HarnessNode in the NetworkHarnesses map of known
// nodes. This method should only be called with nodes that have successfully
// retrieved their public keys via FetchNodeInfo.
func (nm *nodeManager) registerNode(node *node.HarnessNode) {
	nm.Lock()
	nm.activeNodes[node.Cfg.NodeID] = node
	nm.Unlock()
}

// ShutdownNode stops an active lnd process and returns when the process has
// exited and any temporary directories have been cleaned up.
func (nm *nodeManager) shutdownNode(node *node.HarnessNode) error {
	// Remove the node from the active nodes map even if the shutdown
	// fails as the shutdown cannot be retried in that case.
	delete(nm.activeNodes, node.Cfg.NodeID)

	if err := node.Shutdown(); err != nil {
		return err
	}

	return nil
}

// restartNode attempts to restart a lightning node by shutting it down
// cleanly, then restarting the process. This function is fully blocking. Upon
// restart, the RPC connection to the node will be re-attempted, continuing iff
// the connection attempt is successful. If the callback parameter is non-nil,
// then the function will be executed after the node shuts down, but *before*
// the process has been started up again.
func (nm *nodeManager) restartNode(ctxt context.Context,
	hn *node.HarnessNode, callback func() error) error {

	// Stop the node.
	if err := hn.Stop(); err != nil {
		return fmt.Errorf("restart node got error: %w", err)
	}

	if callback != nil {
		if err := callback(); err != nil {
			return err
		}
	}

	// Start the node without unlocking the wallet.
	if hn.Cfg.SkipUnlock {
		return hn.StartWithNoAuth(ctxt)
	}

	return hn.Start(ctxt)
}

// unlockNode unlocks the node's wallet if the password is configured.
// Additionally, each time the node is unlocked, the caller can pass a set of
// SCBs to pass in via the Unlock method allowing them to restore channels
// during restart.
func (nm *nodeManager) unlockNode(hn *node.HarnessNode,
	chanBackups ...*lnrpc.ChanBackupSnapshot) error {

	// If the node doesn't have a password set, then we can exit here as we
	// don't need to unlock it.
	if len(hn.Cfg.Password) == 0 {
		return nil
	}

	// Otherwise, we'll unlock the wallet, then complete the final steps
	// for the node initialization process.
	unlockReq := &lnrpc.UnlockWalletRequest{
		WalletPassword: hn.Cfg.Password,
	}
	if len(chanBackups) != 0 {
		unlockReq.ChannelBackups = chanBackups[0]
		unlockReq.RecoveryWindow = 100
	}

	err := wait.NoError(func() error {
		return hn.Unlock(unlockReq)
	}, DefaultTimeout)
	if err != nil {
		return fmt.Errorf("%s: failed to unlock: %w", hn.Name(), err)
	}

	return nil
}

// initWalletAndNode will unlock the node's wallet and finish setting up the
// node so it's ready to take RPC requests.
func (nm *nodeManager) initWalletAndNode(hn *node.HarnessNode,
	req *lnrpc.InitWalletRequest) ([]byte, error) {

	// Pass the init request via rpc to finish unlocking the node.
	resp := hn.RPC.InitWallet(req)

	// Now that the wallet is unlocked, before creating an authed
	// connection we will close the old unauthed connection.
	if err := hn.CloseConn(); err != nil {
		return nil, fmt.Errorf("close unauthed conn failed")
	}

	// Init the node, which will create the authed grpc conn and all its
	// rpc clients.
	err := hn.InitNode(resp.AdminMacaroon)

	// In stateless initialization mode we get a macaroon back that we have
	// to return to the test, otherwise gRPC calls won't be possible since
	// there are no macaroon files created in that mode.
	// In stateful init the admin macaroon will just be nil.
	return resp.AdminMacaroon, err
}
