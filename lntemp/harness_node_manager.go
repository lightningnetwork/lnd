package lntemp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

// nodeManager is responsible for hanlding the start and stop of a given node.
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
	dbBackend lntest.DatabaseBackend

	// activeNodes is a map of all running nodes, format:
	// {pubkey: *HarnessNode}.
	activeNodes map[uint32]*node.HarnessNode

	// standbyNodes is a map of all the standby nodes, format:
	// {pubkey: *HarnessNode}.
	standbyNodes map[uint32]*node.HarnessNode

	// nodeCounter is a monotonically increasing counter that's used as the
	// node's unique ID.
	nodeCounter uint32

	// feeServiceURL is the url of the fee service.
	feeServiceURL string
}

// newNodeManager creates a new node manager instance.
func newNodeManager(lndBinary string,
	dbBackend lntest.DatabaseBackend) *nodeManager {

	return &nodeManager{
		lndBinary:    lndBinary,
		dbBackend:    dbBackend,
		activeNodes:  make(map[uint32]*node.HarnessNode),
		standbyNodes: make(map[uint32]*node.HarnessNode),
	}
}

// nextNodeID generates a unique sequence to be used as the node's ID.
func (nm *nodeManager) nextNodeID() uint32 {
	nodeID := atomic.AddUint32(&nm.nodeCounter, 1)
	return nodeID - 1
}

// newNode initializes a new HarnessNode, supporting the ability to initialize
// a wallet with or without a seed. If useSeed is false, the returned harness
// node can be used immediately. Otherwise, the node will require an additional
// initialization phase where the wallet is either created or restored.
func (nm *nodeManager) newNode(t *testing.T, name string, extraArgs []string,
	useSeed bool, password []byte, cmdOnly bool,
	opts ...node.Option) (*node.HarnessNode, error) {

	cfg := &node.BaseNodeConfig{
		Name:              name,
		LogFilenamePrefix: nm.currentTestCase,
		Password:          password,
		BackendCfg:        nm.chainBackend,
		ExtraArgs:         extraArgs,
		FeeURL:            nm.feeServiceURL,
		DbBackend:         nm.dbBackend,
		NodeID:            nm.nextNodeID(),
		LndBinary:         nm.lndBinary,
		NetParams:         harnessNetParams,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	node, err := node.NewHarnessNode(t, cfg)
	if err != nil {
		return nil, err
	}

	// Put node in activeNodes to ensure Shutdown is called even if start
	// returns an error.
	defer nm.registerNode(node)

	switch {
	// If the node uses seed to start, we'll need to create the wallet and
	// unlock the wallet later.
	case useSeed:
		err = node.StartWithSeed()

	// Start the node only with the lnd process without creating the grpc
	// connection, which is used in testing etcd leader selection.
	case cmdOnly:
		err = node.StartLndCmd()

	// By default, we'll create a node with wallet being unlocked.
	default:
		err = node.Start()
	}

	if err != nil {
		return nil, fmt.Errorf("failed to start: %w", err)
	}

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
	if err := node.Shutdown(); err != nil {
		return err
	}

	delete(nm.activeNodes, node.Cfg.NodeID)
	return nil
}

// restartNode attempts to restart a lightning node by shutting it down
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
func (nm *nodeManager) restartNode(node *node.HarnessNode,
	callback func() error, chanBackups ...*lnrpc.ChanBackupSnapshot) error {

	err := nm.restartNodeNoUnlock(node, callback)
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

	err = wait.NoError(func() error {
		return node.Unlock(unlockReq)
	}, DefaultTimeout)
	if err != nil {
		return fmt.Errorf("%s: failed to unlock: %w", node.Name(), err)
	}

	return nil
}

// restartNodeNoUnlock attempts to restart a lightning node by shutting it down
// cleanly, then restarting the process. In case the node was setup with a
// seed, it will be left in the unlocked state. This function is fully
// blocking. If the callback parameter is non-nil, then the function will be
// executed after the node shuts down, but *before* the process has been
// started up again.
func (nm *nodeManager) restartNodeNoUnlock(node *node.HarnessNode,
	callback func() error) error {

	if err := node.Stop(); err != nil {
		return fmt.Errorf("restart node got error: %w", err)
	}

	if callback != nil {
		if err := callback(); err != nil {
			return err
		}
	}

	return node.Start()
}
