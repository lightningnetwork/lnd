package lntemp

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
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
