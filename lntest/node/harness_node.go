package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
)

const (
	// logPubKeyBytes is the number of bytes of the node's PubKey that will
	// be appended to the log file name. The whole PubKey is too long and
	// not really necessary to quickly identify what node produced which
	// log file.
	logPubKeyBytes = 4

	// trickleDelay is the amount of time in milliseconds between each
	// release of announcements by AuthenticatedGossiper to the network.
	trickleDelay = 50

	postgresDsn = "postgres://postgres:postgres@localhost:" +
		"6432/%s?sslmode=disable"

	// commitInterval specifies the maximum interval the graph database
	// will wait between attempting to flush a batch of modifications to
	// disk(db.batch-commit-interval).
	commitInterval = 10 * time.Millisecond
)

// HarnessNode represents an instance of lnd running within our test network
// harness. It's responsible for managing the lnd process, grpc connection, and
// wallet auth. A HarnessNode is built upon its rpc clients, represented in
// `HarnessRPC`. It also has a `State` which holds its internal state, and a
// `Watcher` that keeps track of its topology updates.
type HarnessNode struct {
	*testing.T

	// Cfg holds the config values for the node.
	Cfg *BaseNodeConfig

	// RPC holds a list of RPC clients.
	RPC *rpc.HarnessRPC

	// State records the current state of the node.
	State *State

	// Watcher watches the node's topology updates.
	Watcher *nodeWatcher

	// PubKey is the serialized compressed identity public key of the node.
	// This field will only be populated once the node itself has been
	// started via the start() method.
	PubKey    [33]byte
	PubKeyStr string

	// conn is the underlying connection to the grpc endpoint of the node.
	conn *grpc.ClientConn

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context //nolint:containedctx
	cancel context.CancelFunc

	// filename is the log file's name.
	filename string

	cmd     *exec.Cmd
	logFile *os.File
}

// NewHarnessNode creates a new test lightning node instance from the passed
// config.
func NewHarnessNode(t *testing.T, cfg *BaseNodeConfig) (*HarnessNode, error) {
	if err := cfg.GenBaseDir(); err != nil {
		return nil, err
	}

	cfg.DataDir = filepath.Join(cfg.BaseDir, "data")
	cfg.LogDir = filepath.Join(cfg.BaseDir, "logs")
	cfg.TLSCertPath = filepath.Join(cfg.BaseDir, "tls.cert")
	cfg.TLSKeyPath = filepath.Join(cfg.BaseDir, "tls.key")

	networkDir := filepath.Join(
		cfg.DataDir, "chain", lnd.BitcoinChainName, cfg.NetParams.Name,
	)
	cfg.AdminMacPath = filepath.Join(networkDir, "admin.macaroon")
	cfg.ReadMacPath = filepath.Join(networkDir, "readonly.macaroon")
	cfg.InvoiceMacPath = filepath.Join(networkDir, "invoice.macaroon")

	cfg.GenerateListeningPorts()

	// Create temporary database.
	var dbName string
	if cfg.DBBackend == BackendPostgres {
		var err error
		dbName, err = createTempPgDB(t.Context())
		if err != nil {
			return nil, err
		}
		cfg.PostgresDsn = postgresDatabaseDsn(dbName)
	}

	cfg.OriginalExtraArgs = cfg.ExtraArgs
	cfg.postgresDBName = dbName

	return &HarnessNode{
		T:   t,
		Cfg: cfg,
	}, nil
}

// Initialize creates a list of new RPC clients using the passed connection,
// initializes the node's internal state and creates a topology watcher.
func (hn *HarnessNode) Initialize(c *grpc.ClientConn) {
	hn.conn = c

	// Init all the rpc clients.
	hn.RPC = rpc.NewHarnessRPC(hn.runCtx, hn.T, c, hn.Name())

	// Init the node's state.
	//
	// If we already have a state, it means we are restarting the node and
	// we will only reset its internal states. Otherwise we'll create a new
	// state.
	if hn.State != nil {
		hn.State.resetEphermalStates(hn.RPC)
	} else {
		hn.State = newState(hn.RPC)
	}

	// Init the topology watcher.
	hn.Watcher = newNodeWatcher(hn.RPC, hn.State)
}

// Name returns the name of this node set during initialization.
func (hn *HarnessNode) Name() string {
	return hn.Cfg.Name
}

// UpdateState updates the node's internal state.
func (hn *HarnessNode) UpdateState() {
	hn.State.updateState()
}

// String gives the internal state of the node which is useful for debugging.
func (hn *HarnessNode) String() string {
	type nodeCfg struct {
		LogFilenamePrefix string
		ExtraArgs         []string
		SkipUnlock        bool
		Password          []byte
		P2PPort           int
		RPCPort           int
		RESTPort          int
		AcceptKeySend     bool
		FeeURL            string
	}

	nodeState := struct {
		NodeID  uint32
		Name    string
		PubKey  string
		State   *State
		NodeCfg nodeCfg
	}{
		NodeID: hn.Cfg.NodeID,
		Name:   hn.Cfg.Name,
		PubKey: hn.PubKeyStr,
		State:  hn.State,
		NodeCfg: nodeCfg{
			SkipUnlock:        hn.Cfg.SkipUnlock,
			Password:          hn.Cfg.Password,
			LogFilenamePrefix: hn.Cfg.LogFilenamePrefix,
			ExtraArgs:         hn.Cfg.ExtraArgs,
			P2PPort:           hn.Cfg.P2PPort,
			RPCPort:           hn.Cfg.RPCPort,
			RESTPort:          hn.Cfg.RESTPort,
		},
	}

	stateBytes, err := json.MarshalIndent(nodeState, "", "\t")
	if err != nil {
		return fmt.Sprintf("\n encode node state with err: %v", err)
	}

	return fmt.Sprintf("\nnode state: %s", stateBytes)
}

// WaitUntilStarted waits until the wallet state flips from "WAITING_TO_START".
func (hn *HarnessNode) WaitUntilStarted() error {
	return hn.waitTillServerState(func(s lnrpc.WalletState) bool {
		return s != lnrpc.WalletState_WAITING_TO_START
	})
}

// WaitUntilServerActive waits until the lnd daemon is fully started.
func (hn *HarnessNode) WaitUntilServerActive() error {
	return hn.waitTillServerState(func(s lnrpc.WalletState) bool {
		return s == lnrpc.WalletState_SERVER_ACTIVE
	})
}

// WaitUntilLeader attempts to finish the start procedure by initiating an RPC
// connection and setting up the wallet unlocker client. This is needed when
// a node that has recently been started was waiting to become the leader and
// we're at the point when we expect that it is the leader now (awaiting
// unlock).
func (hn *HarnessNode) WaitUntilLeader(timeout time.Duration) error {
	var (
		conn    *grpc.ClientConn
		connErr error
	)

	if err := wait.NoError(func() error {
		conn, connErr = hn.ConnectRPCWithMacaroon(nil)
		return connErr
	}, timeout); err != nil {
		return err
	}

	// Since the conn is not authed, only the `WalletUnlocker` and `State`
	// clients can be inited from this conn.
	hn.conn = conn
	hn.RPC = rpc.NewHarnessRPC(hn.runCtx, hn.T, conn, hn.Name())

	// Wait till the server is starting.
	return hn.WaitUntilStarted()
}

// Unlock attempts to unlock the wallet of the target HarnessNode. This method
// should be called after the restart of a HarnessNode that was created with a
// seed+password. Once this method returns, the HarnessNode will be ready to
// accept normal gRPC requests and harness command.
func (hn *HarnessNode) Unlock(unlockReq *lnrpc.UnlockWalletRequest) error {
	// Otherwise, we'll need to unlock the node before it's able to start
	// up properly.
	hn.RPC.UnlockWallet(unlockReq)

	// Now that the wallet has been unlocked, we'll wait for the RPC client
	// to be ready, then establish the normal gRPC connection.
	return hn.InitNode(nil)
}

// AddToLogf adds a line of choice to the node's logfile. This is useful
// to interleave test output with output from the node.
func (hn *HarnessNode) AddToLogf(format string, a ...interface{}) {
	// If this node was not set up with a log file, just return early.
	if hn.logFile == nil {
		return
	}

	desc := fmt.Sprintf("itest: %s\n", fmt.Sprintf(format, a...))
	if _, err := hn.logFile.WriteString(desc); err != nil {
		hn.printErrf("write to log err: %v", err)
	}
}

// ReadMacaroon waits a given duration for the macaroon file to be created. If
// the file is readable within the timeout, its content is de-serialized as a
// macaroon and returned.
func (hn *HarnessNode) ReadMacaroon(macPath string, timeout time.Duration) (
	*macaroon.Macaroon, error) {

	// Wait until macaroon file is created and has valid content before
	// using it.
	var mac *macaroon.Macaroon
	err := wait.NoError(func() error {
		macBytes, err := os.ReadFile(macPath)
		if err != nil {
			return fmt.Errorf("error reading macaroon file: %w",
				err)
		}

		newMac := &macaroon.Macaroon{}
		if err = newMac.UnmarshalBinary(macBytes); err != nil {
			return fmt.Errorf("error unmarshalling macaroon "+
				"file: %w", err)
		}
		mac = newMac

		return nil
	}, timeout)

	return mac, err
}

// ConnectRPCWithMacaroon uses the TLS certificate and given macaroon to
// create a gRPC client connection.
func (hn *HarnessNode) ConnectRPCWithMacaroon(mac *macaroon.Macaroon) (
	*grpc.ClientConn, error) {

	// Wait until TLS certificate is created and has valid content before
	// using it, up to 30 sec.
	var tlsCreds credentials.TransportCredentials
	err := wait.NoError(func() error {
		var err error
		tlsCreds, err = credentials.NewClientTLSFromFile(
			hn.Cfg.TLSCertPath, "",
		)
		return err
	}, wait.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithTransportCredentials(tlsCreds),
	}

	ctx, cancel := context.WithTimeout(hn.runCtx, wait.DefaultTimeout)
	defer cancel()

	if mac == nil {
		return grpc.DialContext(ctx, hn.Cfg.RPCAddr(), opts...)
	}
	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error cloning mac: %w", err)
	}
	opts = append(opts, grpc.WithPerRPCCredentials(macCred))

	return grpc.DialContext(ctx, hn.Cfg.RPCAddr(), opts...)
}

// ConnectRPC uses the TLS certificate and admin macaroon files written by the
// lnd node to create a gRPC client connection.
func (hn *HarnessNode) ConnectRPC() (*grpc.ClientConn, error) {
	// If we should use a macaroon, always take the admin macaroon as a
	// default.
	mac, err := hn.ReadMacaroon(hn.Cfg.AdminMacPath, wait.DefaultTimeout)
	if err != nil {
		return nil, err
	}

	return hn.ConnectRPCWithMacaroon(mac)
}

// SetExtraArgs assigns the ExtraArgs field for the node's configuration. The
// changes will take effect on restart.
func (hn *HarnessNode) SetExtraArgs(extraArgs []string) {
	hn.Cfg.ExtraArgs = extraArgs
}

// StartLndCmd handles the startup of lnd, creating log files, and possibly
// kills the process when needed.
func (hn *HarnessNode) StartLndCmd(ctxb context.Context) error {
	// Init the run context.
	hn.runCtx, hn.cancel = context.WithCancel(ctxb)

	args := hn.Cfg.GenArgs()
	hn.cmd = exec.Command(hn.Cfg.LndBinary, args...)

	// Redirect stderr output to buffer
	var errb bytes.Buffer
	hn.cmd.Stderr = &errb

	// If the logoutput flag is passed, redirect output from the nodes to
	// log files.
	if *logOutput {
		err := addLogFile(hn)
		if err != nil {
			return err
		}
	}

	// Start the process.
	if err := hn.cmd.Start(); err != nil {
		return err
	}

	pid := hn.cmd.Process.Pid
	hn.T.Logf("Starting node (name=%v) with PID=%v", hn.Cfg.Name, pid)

	return nil
}

// StartWithNoAuth will start the lnd process, creates the grpc connection
// without macaroon auth, and waits until the server is reported as waiting to
// start.
//
// NOTE: caller needs to take extra step to create and unlock the wallet.
func (hn *HarnessNode) StartWithNoAuth(ctxt context.Context) error {
	// Start lnd process and prepare logs.
	if err := hn.StartLndCmd(ctxt); err != nil {
		return fmt.Errorf("start lnd error: %w", err)
	}

	// Create an unauthed connection.
	conn, err := hn.ConnectRPCWithMacaroon(nil)
	if err != nil {
		return fmt.Errorf("ConnectRPCWithMacaroon err: %w", err)
	}

	// Since the conn is not authed, only the `WalletUnlocker` and `State`
	// clients can be inited from this conn.
	hn.conn = conn
	hn.RPC = rpc.NewHarnessRPC(hn.runCtx, hn.T, conn, hn.Name())

	// Wait till the server is starting.
	return hn.WaitUntilStarted()
}

// Start will start the lnd process, creates the grpc connection, and waits
// until the server is fully started.
func (hn *HarnessNode) Start(ctxt context.Context) error {
	// Start lnd process and prepare logs.
	if err := hn.StartLndCmd(ctxt); err != nil {
		return fmt.Errorf("start lnd error: %w", err)
	}

	// Since Stop uses the LightningClient to stop the node, if we fail to
	// get a connected client, we have to kill the process.
	conn, err := hn.ConnectRPC()
	if err != nil {
		err = fmt.Errorf("ConnectRPC err: %w", err)
		cmdErr := hn.Kill()
		if cmdErr != nil {
			err = fmt.Errorf("kill process got err: %w: %v",
				cmdErr, err)
		}
		return err
	}

	// Init the node by creating the RPC clients, initializing node's
	// internal state and watcher.
	hn.Initialize(conn)

	// Wait till the server is starting.
	if err := hn.WaitUntilStarted(); err != nil {
		return fmt.Errorf("waiting for start got: %w", err)
	}

	// Subscribe for topology updates.
	return hn.initLightningClient()
}

// InitNode waits until the main gRPC server is detected as active, then
// complete the normal HarnessNode gRPC connection creation. A non-nil
// `macBytes` indicates the node is initialized stateless, otherwise it will
// use the admin macaroon.
func (hn *HarnessNode) InitNode(macBytes []byte) error {
	var (
		conn *grpc.ClientConn
		err  error
	)

	// If the node has been initialized stateless, we need to pass the
	// macaroon to the client.
	if macBytes != nil {
		adminMac := &macaroon.Macaroon{}
		err := adminMac.UnmarshalBinary(macBytes)
		if err != nil {
			return fmt.Errorf("unmarshal failed: %w", err)
		}
		conn, err = hn.ConnectRPCWithMacaroon(adminMac)
		if err != nil {
			return err
		}
	} else {
		// Normal initialization, we expect a macaroon to be in the
		// file system.
		conn, err = hn.ConnectRPC()
		if err != nil {
			return err
		}
	}

	// Init the node by creating the RPC clients, initializing node's
	// internal state and watcher.
	hn.Initialize(conn)

	// Wait till the server is starting.
	if err := hn.WaitUntilStarted(); err != nil {
		return fmt.Errorf("waiting for start got: %w", err)
	}

	return hn.initLightningClient()
}

// InitChangePassword initializes a harness node by passing the change password
// request via RPC. After the request is submitted, this method will block until
// a macaroon-authenticated RPC connection can be established to the harness
// node. Once established, the new connection is used to initialize the
// RPC clients and subscribes the HarnessNode to topology changes.
func (hn *HarnessNode) ChangePasswordAndInit(
	req *lnrpc.ChangePasswordRequest) (
	*lnrpc.ChangePasswordResponse, error) {

	response := hn.RPC.ChangePassword(req)
	return response, hn.InitNode(response.AdminMacaroon)
}

// waitTillServerState makes a subscription to the server's state change and
// blocks until the server is in the targeted state.
func (hn *HarnessNode) waitTillServerState(
	predicate func(state lnrpc.WalletState) bool) error {

	client := hn.RPC.SubscribeState()

	errChan := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		for {
			resp, err := client.Recv()
			if err != nil {
				errChan <- err
				return
			}

			if predicate(resp.State) {
				close(done)
				return
			}
		}
	}()

	for {
		select {
		case <-time.After(wait.NodeStartTimeout):
			return fmt.Errorf("timeout waiting for server state")
		case err := <-errChan:
			return fmt.Errorf("receive server state err: %w", err)

		case <-done:
			return nil
		}
	}
}

// initLightningClient blocks until the lnd server is fully started and
// subscribes the harness node to graph topology updates. This method also
// spawns a lightning network watcher for this node, which watches for topology
// changes.
func (hn *HarnessNode) initLightningClient() error {
	// Wait until the server is fully started.
	if err := hn.WaitUntilServerActive(); err != nil {
		return fmt.Errorf("waiting for server active: %w", err)
	}

	// Set the harness node's pubkey to what the node claims in GetInfo.
	// The RPC must have been started at this point.
	if err := hn.attachPubKey(); err != nil {
		return err
	}

	// Launch the watcher that will hook into graph related topology change
	// from the PoV of this node.
	started := make(chan error, 1)
	go hn.Watcher.topologyWatcher(hn.runCtx, started)

	select {
	// First time reading the channel indicates the topology client is
	// started.
	case err := <-started:
		if err != nil {
			return fmt.Errorf("create topology client stream "+
				"got err: %v", err)
		}

	case <-time.After(wait.DefaultTimeout):
		return fmt.Errorf("timeout creating topology client stream")
	}

	// Catch topology client stream error inside a goroutine.
	go func() {
		select {
		case err := <-started:
			hn.printErrf("topology client: %v", err)

		case <-hn.runCtx.Done():
		}
	}()

	return nil
}

// attachPubKey queries an unlocked node to retrieve its public key.
func (hn *HarnessNode) attachPubKey() error {
	// Obtain the lnid of this node for quick identification purposes.
	info := hn.RPC.GetInfo()
	hn.PubKeyStr = info.IdentityPubkey

	pubkey, err := hex.DecodeString(info.IdentityPubkey)
	if err != nil {
		return err
	}
	copy(hn.PubKey[:], pubkey)

	return nil
}

// cleanup cleans up all the temporary files created by the node's process.
func (hn *HarnessNode) cleanup() error {
	if hn.Cfg.backupDBDir != "" {
		err := os.RemoveAll(hn.Cfg.backupDBDir)
		if err != nil {
			return fmt.Errorf("unable to remove backup dir: %w",
				err)
		}
	}

	return os.RemoveAll(hn.Cfg.BaseDir)
}

// waitForProcessExit Launch a new goroutine which that bubbles up any
// potential fatal process errors to the goroutine running the tests.
func (hn *HarnessNode) WaitForProcessExit() error {
	var errReturned error

	errChan := make(chan error, 1)
	go func() {
		errChan <- hn.cmd.Wait()
	}()

	select {
	case err := <-errChan:
		if err == nil {
			break
		}

		// If the process has already been canceled, we can exit early
		// as the logs have already been saved.
		if strings.Contains(err.Error(), "Wait was already called") {
			return nil
		}

		// The process may have already been killed in the test, in
		// that case we will skip the error and continue processing
		// the logs.
		if strings.Contains(err.Error(), "signal: killed") {
			break
		}

		// Otherwise, we print the error, break the select and save
		// logs.
		hn.printErrf("wait process exit got err: %v", err)
		errReturned = err

	case <-time.After(wait.DefaultTimeout):
		hn.printErrf("timeout waiting for process to exit")
	}

	// If the node has an open log file handle, inspect the log file
	// to verify that the node shut down correctly.
	if hn.logFile != nil {
		// Make sure log file is closed and renamed if necessary.
		filename := finalizeLogfile(hn)

		// Assert the node has shut down from the log file.
		err1 := assertNodeShutdown(filename)
		if err1 != nil {
			return fmt.Errorf("[%s]: assert shutdown failed in "+
				"log[%s]: %w", hn.Name(), filename, err1)
		}
	}

	// Rename the etcd.log file if the node was running on embedded etcd.
	finalizeEtcdLog(hn)

	return errReturned
}

// Stop attempts to stop the active lnd process.
func (hn *HarnessNode) Stop() error {
	// Do nothing if the process is not running.
	if hn.runCtx == nil {
		hn.printErrf("found nil run context")
		return nil
	}

	// Stop the runCtx.
	hn.cancel()

	// If we ever reaches the state where `Watcher` is initialized, it
	// means the node has an authed connection and all its RPC clients are
	// ready for use. Thus we will try to stop it via the RPC.
	if hn.Watcher != nil {
		// Don't watch for error because sometimes the RPC connection
		// gets closed before a response is returned.
		req := lnrpc.StopRequest{}

		// We have to use context.Background(), because both hn.Context
		// and hn.runCtx have been canceled by this point. We canceled
		// hn.runCtx just few lines above. hn.Context() is canceled by
		// Go before calling cleanup callbacks; HarnessNode.Stop() is
		// called from shutdownAllNodes which is called from a cleanup.
		ctxt, cancel := context.WithCancel(context.Background())
		defer cancel()

		err := wait.NoError(func() error {
			_, err := hn.RPC.LN.StopDaemon(ctxt, &req)
			if err == nil {
				return nil
			}

			// If the connection is already closed, we can exit
			// early as the node has already been shut down in the
			// test, e.g., in etcd leader health check test.
			if strings.Contains(err.Error(), "connection refused") {
				return nil
			}

			return err
		}, wait.DefaultTimeout)
		if err != nil {
			return fmt.Errorf("shutdown timeout: %w", err)
		}

		// Wait for goroutines to be finished.
		done := make(chan struct{})
		go func() {
			hn.Watcher.wg.Wait()
			close(done)
			hn.Watcher = nil
		}()

		// If the goroutines fail to finish before timeout, we'll print
		// the error to console and continue.
		select {
		case <-time.After(wait.DefaultTimeout):
			hn.printErrf("timeout on wait group")
		case <-done:
		}
	} else {
		// If the rpc clients are not initiated, we'd kill the process
		// manually.
		hn.printErrf("found nil RPC clients")
		if err := hn.Kill(); err != nil {
			// Skip the error if the process is already dead.
			if !strings.Contains(
				err.Error(), "process already finished",
			) {

				return fmt.Errorf("killing process got: %w",
					err)
			}
		}
	}

	// Close any attempts at further grpc connections.
	if hn.conn != nil {
		if err := hn.CloseConn(); err != nil {
			return err
		}
	}

	// Wait for lnd process to exit in the end.
	return hn.WaitForProcessExit()
}

// CloseConn closes the grpc connection.
func (hn *HarnessNode) CloseConn() error {
	err := status.Code(hn.conn.Close())
	switch err {
	case codes.OK:
		return nil

	// When the context is canceled above, we might get the
	// following error as the context is no longer active.
	case codes.Canceled:
		return nil

	case codes.Unknown:
		return fmt.Errorf("unknown error attempting to stop "+
			"grpc client: %v", err)

	default:
		return fmt.Errorf("error attempting to stop "+
			"grpc client: %v", err)
	}
}

// Shutdown stops the active lnd process and cleans up any temporary
// directories created along the way.
func (hn *HarnessNode) Shutdown() error {
	if err := hn.Stop(); err != nil {
		return err
	}

	// Exit if we want to skip the cleanup, which happens when a customized
	// base dir is used.
	if hn.Cfg.SkipCleanup {
		return nil
	}

	if err := hn.cleanup(); err != nil {
		return err
	}
	return nil
}

// Kill kills the lnd process.
func (hn *HarnessNode) Kill() error {
	return hn.cmd.Process.Kill()
}

// KillAndWait kills the lnd process and waits for it to finish.
func (hn *HarnessNode) KillAndWait() error {
	err := hn.cmd.Process.Kill()
	if err != nil {
		return err
	}

	_, err = hn.cmd.Process.Wait()

	return err
}

// printErrf prints an error to the console.
func (hn *HarnessNode) printErrf(format string, a ...interface{}) {
	fmt.Printf("%v: itest error from [%s:%s]: %s\n", //nolint:forbidigo
		time.Now().UTC(), hn.Cfg.LogFilenamePrefix, hn.Cfg.Name,
		fmt.Sprintf(format, a...))
}

// BackupDB creates a backup of the current database.
func (hn *HarnessNode) BackupDB() error {
	if hn.Cfg.backupDBDir != "" {
		return fmt.Errorf("backup already created")
	}

	if hn.Cfg.postgresDBName != "" {
		// Backup database.
		backupDBName := hn.Cfg.postgresDBName + "_backup"
		err := executePgQuery(
			hn.Context(), "CREATE DATABASE "+backupDBName+
				" WITH TEMPLATE "+hn.Cfg.postgresDBName,
		)
		if err != nil {
			return err
		}
	} else {
		// Backup files.
		tempDir, err := os.MkdirTemp("", "past-state")
		if err != nil {
			return fmt.Errorf("unable to create temp db folder: %w",
				err)
		}

		if err := copyAll(tempDir, hn.Cfg.DBDir()); err != nil {
			return fmt.Errorf("unable to copy database files: %w",
				err)
		}

		hn.Cfg.backupDBDir = tempDir
	}

	return nil
}

// RestoreDB restores a database backup.
func (hn *HarnessNode) RestoreDB() error {
	if hn.Cfg.postgresDBName != "" {
		// Restore database.
		backupDBName := hn.Cfg.postgresDBName + "_backup"
		err := executePgQuery(
			hn.Context(), "DROP DATABASE "+hn.Cfg.postgresDBName,
		)
		if err != nil {
			return err
		}
		err = executePgQuery(
			hn.Context(), "ALTER DATABASE "+backupDBName+
				" RENAME TO "+hn.Cfg.postgresDBName,
		)
		if err != nil {
			return err
		}
	} else {
		// Restore files.
		if hn.Cfg.backupDBDir == "" {
			return fmt.Errorf("no database backup created")
		}

		err := copyAll(hn.Cfg.DBDir(), hn.Cfg.backupDBDir)
		if err != nil {
			return fmt.Errorf("unable to copy database files: %w",
				err)
		}

		if err := os.RemoveAll(hn.Cfg.backupDBDir); err != nil {
			return fmt.Errorf("unable to remove backup dir: %w",
				err)
		}
		hn.Cfg.backupDBDir = ""
	}

	return nil
}

// UpdateGlobalPolicy updates a node's global channel policy.
func (hn *HarnessNode) UpdateGlobalPolicy(policy *lnrpc.RoutingPolicy) {
	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat: policy.FeeBaseMsat,
		FeeRate: float64(policy.FeeRateMilliMsat) /
			float64(1_000_000),
		TimeLockDelta: policy.TimeLockDelta,
		Scope:         &lnrpc.PolicyUpdateRequest_Global{Global: true},
		MaxHtlcMsat:   policy.MaxHtlcMsat,
	}
	hn.RPC.UpdateChannelPolicy(updateFeeReq)
}

func postgresDatabaseDsn(dbName string) string {
	return fmt.Sprintf(postgresDsn, dbName)
}

// createTempPgDB creates a temp postgres database.
func createTempPgDB(ctx context.Context) (string, error) {
	// Create random database name.
	randBytes := make([]byte, 8)
	_, err := rand.Read(randBytes)
	if err != nil {
		return "", err
	}
	dbName := "itest_" + hex.EncodeToString(randBytes)

	// Create database.
	err = executePgQuery(ctx, "CREATE DATABASE "+dbName)
	if err != nil {
		return "", err
	}

	return dbName, nil
}

// executePgQuery executes a SQL statement in a postgres db.
func executePgQuery(ctx context.Context, query string) error {
	pool, err := pgxpool.Connect(ctx, postgresDatabaseDsn("postgres"))
	if err != nil {
		return fmt.Errorf("unable to connect to database: %w", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, query)
	return err
}

// renameFile is a helper to rename (log) files created during integration
// tests.
func renameFile(fromFileName, toFileName string) {
	err := os.Rename(fromFileName, toFileName)
	if err != nil {
		fmt.Printf("could not rename %s to %s: %v\n", // nolint:forbidigo
			fromFileName, toFileName, err)
	}
}

// getFinalizedLogFilePrefix returns the finalize log filename.
func getFinalizedLogFilePrefix(hn *HarnessNode) string {
	pubKeyHex := hex.EncodeToString(
		hn.PubKey[:logPubKeyBytes],
	)

	return fmt.Sprintf("%s/%d-%s-%s-%s", GetLogDir(), hn.Cfg.NodeID,
		hn.Cfg.LogFilenamePrefix, hn.Cfg.Name, pubKeyHex)
}

// finalizeLogfile makes sure the log file cleanup function is initialized,
// even if no log file is created.
func finalizeLogfile(hn *HarnessNode) string {
	// Exit early if there's no log file.
	if hn.logFile == nil {
		return ""
	}

	hn.logFile.Close()

	// If logoutput flag is not set, return early.
	if !*logOutput {
		return ""
	}

	newFileName := fmt.Sprintf("%v.log", getFinalizedLogFilePrefix(hn))
	renameFile(hn.filename, newFileName)

	return newFileName
}

// assertNodeShutdown asserts that the node has shut down properly by checking
// the last lines of the log file for the shutdown message "Shutdown complete".
func assertNodeShutdown(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// Read more than one line to make sure we get the last line.
	// const linesSize = 200
	//
	// NOTE: Reading 200 bytes of lines should be more than enough to find
	// the `Shutdown complete` message. However, this is only true if the
	// message is printed the last, which means `lnd` will properly wait
	// for all its subsystems to shut down before exiting. Unfortunately
	// there is at least one bug in the shutdown process where we don't
	// wait for the chain backend to fully quit first, which can be easily
	// reproduced by turning on `RPCC=trace` and use a linesSize of 200.
	//
	// TODO(yy): fix the shutdown process and remove this workaround by
	// refactoring the lnd to use only one rpcclient, which requires quite
	// some work on the btcwallet front.
	const linesSize = 1000

	buf := make([]byte, linesSize)
	stat, statErr := file.Stat()
	if statErr != nil {
		return err
	}

	start := stat.Size() - linesSize
	_, err = file.ReadAt(buf, start)
	if err != nil {
		return err
	}

	// Exit early if the shutdown line is found.
	if bytes.Contains(buf, []byte("Shutdown complete")) {
		return nil
	}

	// For etcd tests, we need to check for the line where the node is
	// blocked at wallet unlock since we are testing how such a behavior is
	// handled by etcd.
	if bytes.Contains(buf, []byte("wallet and unlock")) {
		return nil
	}

	return fmt.Errorf("node did not shut down properly: found log "+
		"lines: %s", buf)
}

// finalizeEtcdLog saves the etcd log files when test ends.
func finalizeEtcdLog(hn *HarnessNode) {
	// Exit early if this is not etcd backend.
	if hn.Cfg.DBBackend != BackendEtcd {
		return
	}

	etcdLogFileName := fmt.Sprintf("%s/etcd.log", hn.Cfg.LogDir)
	newEtcdLogFileName := fmt.Sprintf("%v-etcd.log",
		getFinalizedLogFilePrefix(hn),
	)

	renameFile(etcdLogFileName, newEtcdLogFileName)
}

// addLogFile creates log files used by this node.
func addLogFile(hn *HarnessNode) error {
	var fileName string

	dir := GetLogDir()
	fileName = fmt.Sprintf("%s/%d-%s-%s-%s.log", dir, hn.Cfg.NodeID,
		hn.Cfg.LogFilenamePrefix, hn.Cfg.Name,
		hex.EncodeToString(hn.PubKey[:logPubKeyBytes]))

	// If the node's PubKey is not yet initialized, create a temporary file
	// name. Later, after the PubKey has been initialized, the file can be
	// moved to its final name with the PubKey included.
	if bytes.Equal(hn.PubKey[:4], []byte{0, 0, 0, 0}) {
		fileName = fmt.Sprintf("%s/%d-%s-%s-tmp__.log", dir,
			hn.Cfg.NodeID, hn.Cfg.LogFilenamePrefix,
			hn.Cfg.Name)
	}

	// Create file if not exists, otherwise append.
	file, err := os.OpenFile(fileName,
		os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		return err
	}

	// Pass node's stderr to both errb and the file.
	w := io.MultiWriter(hn.cmd.Stderr, file)
	hn.cmd.Stderr = w

	// Pass the node's stdout only to the file.
	hn.cmd.Stdout = file

	// Let the node keep a reference to this file, such that we can add to
	// it if necessary.
	hn.logFile = file

	hn.filename = fileName

	return nil
}

// copyAll copies all files and directories from srcDir to dstDir recursively.
// Note that this function does not support links.
func copyAll(dstDir, srcDir string) error {
	entries, err := os.ReadDir(srcDir)
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
