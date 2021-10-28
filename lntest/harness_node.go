package lntest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
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

	postgresDsn = "postgres://postgres:postgres@localhost:6432/%s?sslmode=disable"

	// commitInterval specifies the maximum interval the graph database
	// will wait between attempting to flush a batch of modifications to
	// disk(db.batch-commit-interval).
	commitInterval = 10 * time.Millisecond
)

var (
	// numActiveNodes is the number of active nodes within the test network.
	numActiveNodes    = 0
	numActiveNodesMtx sync.Mutex
)

func postgresDatabaseDsn(dbName string) string {
	return fmt.Sprintf(postgresDsn, dbName)
}

// BackendConfig is an interface that abstracts away the specific chain backend
// node implementation.
type BackendConfig interface {
	// GenArgs returns the arguments needed to be passed to LND at startup
	// for using this node as a chain backend.
	GenArgs() []string

	// ConnectMiner is called to establish a connection to the test miner.
	ConnectMiner() error

	// DisconnectMiner is called to disconnect the miner.
	DisconnectMiner() error

	// Name returns the name of the backend type.
	Name() string

	// Credentials returns the rpc username, password and host for the
	// backend.
	Credentials() (string, string, string, error)
}

// NodeConfig is the basic interface a node configuration must implement.
type NodeConfig interface {
	// BaseConfig returns the base node configuration struct.
	BaseConfig() *BaseNodeConfig

	// GenerateListeningPorts generates the ports to listen on designated
	// for the current lightning network test.
	GenerateListeningPorts()

	// GenArgs generates a slice of command line arguments from the
	// lightning node config struct.
	GenArgs() []string
}

// BaseNodeConfig is the base node configuration.
type BaseNodeConfig struct {
	Name string

	// LogFilenamePrefix is used to prefix node log files. Can be used
	// to store the current test case for simpler postmortem debugging.
	LogFilenamePrefix string

	BackendCfg BackendConfig
	NetParams  *chaincfg.Params
	BaseDir    string
	ExtraArgs  []string

	DataDir        string
	LogDir         string
	TLSCertPath    string
	TLSKeyPath     string
	AdminMacPath   string
	ReadMacPath    string
	InvoiceMacPath string

	HasSeed  bool
	Password []byte

	P2PPort     int
	RPCPort     int
	RESTPort    int
	ProfilePort int

	AcceptKeySend bool
	AcceptAMP     bool

	FeeURL string

	DbBackend   DatabaseBackend
	PostgresDsn string
}

func (cfg BaseNodeConfig) P2PAddr() string {
	return fmt.Sprintf(ListenerFormat, cfg.P2PPort)
}

func (cfg BaseNodeConfig) RPCAddr() string {
	return fmt.Sprintf(ListenerFormat, cfg.RPCPort)
}

func (cfg BaseNodeConfig) RESTAddr() string {
	return fmt.Sprintf(ListenerFormat, cfg.RESTPort)
}

// DBDir returns the holding directory path of the graph database.
func (cfg BaseNodeConfig) DBDir() string {
	return filepath.Join(cfg.DataDir, "graph", cfg.NetParams.Name)
}

func (cfg BaseNodeConfig) DBPath() string {
	return filepath.Join(cfg.DBDir(), "channel.db")
}

func (cfg BaseNodeConfig) ChanBackupPath() string {
	return filepath.Join(
		cfg.DataDir, "chain", "bitcoin",
		fmt.Sprintf(
			"%v/%v", cfg.NetParams.Name,
			chanbackup.DefaultBackupFileName,
		),
	)
}

// GenerateListeningPorts generates the ports to listen on designated for the
// current lightning network test.
func (cfg *BaseNodeConfig) GenerateListeningPorts() {
	if cfg.P2PPort == 0 {
		cfg.P2PPort = NextAvailablePort()
	}
	if cfg.RPCPort == 0 {
		cfg.RPCPort = NextAvailablePort()
	}
	if cfg.RESTPort == 0 {
		cfg.RESTPort = NextAvailablePort()
	}
	if cfg.ProfilePort == 0 {
		cfg.ProfilePort = NextAvailablePort()
	}
}

// BaseConfig returns the base node configuration struct.
func (cfg *BaseNodeConfig) BaseConfig() *BaseNodeConfig {
	return cfg
}

// GenArgs generates a slice of command line arguments from the lightning node
// config struct.
func (cfg *BaseNodeConfig) GenArgs() []string {
	var args []string

	switch cfg.NetParams {
	case &chaincfg.TestNet3Params:
		args = append(args, "--bitcoin.testnet")
	case &chaincfg.SimNetParams:
		args = append(args, "--bitcoin.simnet")
	case &chaincfg.RegressionNetParams:
		args = append(args, "--bitcoin.regtest")
	}

	backendArgs := cfg.BackendCfg.GenArgs()
	args = append(args, backendArgs...)

	nodeArgs := []string{
		"--bitcoin.active",
		"--nobootstrap",
		"--debuglevel=debug",
		"--bitcoin.defaultchanconfs=1",
		fmt.Sprintf("--db.batch-commit-interval=%v", commitInterval),
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v", DefaultCSV),
		fmt.Sprintf("--rpclisten=%v", cfg.RPCAddr()),
		fmt.Sprintf("--restlisten=%v", cfg.RESTAddr()),
		fmt.Sprintf("--restcors=https://%v", cfg.RESTAddr()),
		fmt.Sprintf("--listen=%v", cfg.P2PAddr()),
		fmt.Sprintf("--externalip=%v", cfg.P2PAddr()),
		fmt.Sprintf("--lnddir=%v", cfg.BaseDir),
		fmt.Sprintf("--adminmacaroonpath=%v", cfg.AdminMacPath),
		fmt.Sprintf("--readonlymacaroonpath=%v", cfg.ReadMacPath),
		fmt.Sprintf("--invoicemacaroonpath=%v", cfg.InvoiceMacPath),
		fmt.Sprintf("--trickledelay=%v", trickleDelay),
		fmt.Sprintf("--profile=%d", cfg.ProfilePort),
		fmt.Sprintf("--caches.rpc-graph-cache-duration=%d", 0),
	}
	args = append(args, nodeArgs...)

	if !cfg.HasSeed {
		args = append(args, "--noseedbackup")
	}

	if cfg.ExtraArgs != nil {
		args = append(args, cfg.ExtraArgs...)
	}

	if cfg.AcceptKeySend {
		args = append(args, "--accept-keysend")
	}

	if cfg.AcceptAMP {
		args = append(args, "--accept-amp")
	}

	switch cfg.DbBackend {
	case BackendEtcd:
		args = append(args, "--db.backend=etcd")
		args = append(args, "--db.etcd.embedded")
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_client_port=%v",
				NextAvailablePort(),
			),
		)
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_peer_port=%v",
				NextAvailablePort(),
			),
		)
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_log_file=%v",
				path.Join(cfg.LogDir, "etcd.log"),
			),
		)

	case BackendPostgres:
		args = append(args, "--db.backend=postgres")
		args = append(args, "--db.postgres.dsn="+cfg.PostgresDsn)
	}

	if cfg.FeeURL != "" {
		args = append(args, "--feeurl="+cfg.FeeURL)
	}

	return args
}

// policyUpdateMap defines a type to store channel policy updates. It has the
// format,
// {
//  "chanPoint1": {
//       "advertisingNode1": [
//              policy1, policy2, ...
//       ],
//       "advertisingNode2": [
//              policy1, policy2, ...
//       ]
//  },
//  "chanPoint2": ...
// }.
type policyUpdateMap map[string]map[string][]*lnrpc.RoutingPolicy

// HarnessNode represents an instance of lnd running within our test network
// harness. Each HarnessNode instance also fully embeds an RPC client in
// order to pragmatically drive the node.
type HarnessNode struct {
	mu  sync.RWMutex
	Cfg *BaseNodeConfig

	// NodeID is a unique identifier for the node within a NetworkHarness.
	NodeID int

	// PubKey is the serialized compressed identity public key of the node.
	// This field will only be populated once the node itself has been
	// started via the start() method.
	PubKey    [33]byte
	PubKeyStr string

	// rpc holds a list of RPC clients.
	rpc *RPCClients

	// chanWatchRequests receives a request for watching a particular event
	// for a given channel.
	chanWatchRequests chan *chanWatchRequest

	// For each outpoint, we'll track an integer which denotes the number of
	// edges seen for that channel within the network. When this number
	// reaches 2, then it means that both edge advertisements has propagated
	// through the network.
	openChans        map[wire.OutPoint]int
	openChanWatchers map[wire.OutPoint][]chan struct{}

	closedChans       map[wire.OutPoint]*lnrpc.ClosedChannelUpdate
	closeChanWatchers map[wire.OutPoint][]chan struct{}

	// numChanUpdates records the number of channel updates seen by each
	// channel.
	numChanUpdates map[wire.OutPoint]int

	// nodeUpdates records the node announcements seen by each node.
	nodeUpdates map[string][]*lnrpc.NodeUpdate

	// policyUpdates stores a slice of seen polices by each advertising
	// node and the outpoint.
	policyUpdates policyUpdateMap

	// backupDbDir is the path where a database backup is stored, if any.
	backupDbDir string

	// postgresDbName is the name of the postgres database where lnd data is
	// stored in.
	postgresDbName string

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc

	wg      sync.WaitGroup
	cmd     *exec.Cmd
	logFile *os.File
}

// RPCClients wraps a list of RPC clients into a single struct for easier
// access.
type RPCClients struct {
	// conn is the underlying connection to the grpc endpoint of the node.
	conn *grpc.ClientConn

	LN               lnrpc.LightningClient
	WalletUnlocker   lnrpc.WalletUnlockerClient
	Invoice          invoicesrpc.InvoicesClient
	Signer           signrpc.SignerClient
	Router           routerrpc.RouterClient
	WalletKit        walletrpc.WalletKitClient
	Watchtower       watchtowerrpc.WatchtowerClient
	WatchtowerClient wtclientrpc.WatchtowerClientClient
	State            lnrpc.StateClient
	ChainClient      chainrpc.ChainNotifierClient
	Peer             peersrpc.PeersClient
}

// nextNodeID generates a unique sequence to be used as the node's ID.
func nextNodeID() int {
	numActiveNodesMtx.Lock()
	defer numActiveNodesMtx.Unlock()
	nodeNum := numActiveNodes
	numActiveNodes++

	return nodeNum
}

// newNode creates a new test lightning node instance from the passed config.
func newNode(cfg *BaseNodeConfig) (*HarnessNode, error) {
	if cfg.BaseDir == "" {
		var err error
		cfg.BaseDir, err = ioutil.TempDir("", "lndtest-node")
		if err != nil {
			return nil, err
		}
	}
	cfg.DataDir = filepath.Join(cfg.BaseDir, "data")
	cfg.LogDir = filepath.Join(cfg.BaseDir, "logs")
	cfg.TLSCertPath = filepath.Join(cfg.BaseDir, "tls.cert")
	cfg.TLSKeyPath = filepath.Join(cfg.BaseDir, "tls.key")

	networkDir := filepath.Join(
		cfg.DataDir, "chain", "bitcoin", cfg.NetParams.Name,
	)
	cfg.AdminMacPath = filepath.Join(networkDir, "admin.macaroon")
	cfg.ReadMacPath = filepath.Join(networkDir, "readonly.macaroon")
	cfg.InvoiceMacPath = filepath.Join(networkDir, "invoice.macaroon")

	cfg.GenerateListeningPorts()

	// Run all tests with accept keysend. The keysend code is very isolated
	// and it is highly unlikely that it would affect regular itests when
	// enabled.
	cfg.AcceptKeySend = true

	// Create temporary database.
	var dbName string
	if cfg.DbBackend == BackendPostgres {
		var err error
		dbName, err = createTempPgDb()
		if err != nil {
			return nil, err
		}
		cfg.PostgresDsn = postgresDatabaseDsn(dbName)
	}

	return &HarnessNode{
		Cfg:               cfg,
		NodeID:            nextNodeID(),
		chanWatchRequests: make(chan *chanWatchRequest),
		openChans:         make(map[wire.OutPoint]int),
		openChanWatchers:  make(map[wire.OutPoint][]chan struct{}),
		closedChans: make(
			map[wire.OutPoint]*lnrpc.ClosedChannelUpdate,
		),
		closeChanWatchers: make(map[wire.OutPoint][]chan struct{}),
		numChanUpdates:    make(map[wire.OutPoint]int),
		nodeUpdates:       make(map[string][]*lnrpc.NodeUpdate),
		policyUpdates:     policyUpdateMap{},
		postgresDbName:    dbName,
	}, nil
}

func createTempPgDb() (string, error) {
	// Create random database name.
	randBytes := make([]byte, 8)
	_, err := rand.Read(randBytes)
	if err != nil {
		return "", err
	}
	dbName := "itest_" + hex.EncodeToString(randBytes)

	// Create database.
	err = executePgQuery("CREATE DATABASE " + dbName)
	if err != nil {
		return "", err
	}

	return dbName, nil
}

func executePgQuery(query string) error {
	pool, err := pgxpool.Connect(
		context.Background(),
		postgresDatabaseDsn("postgres"),
	)
	if err != nil {
		return fmt.Errorf("unable to connect to database: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(context.Background(), query)
	return err
}

// String gives the internal state of the node which is useful for debugging.
func (hn *HarnessNode) String() string {
	type nodeCfg struct {
		LogFilenamePrefix string
		ExtraArgs         []string
		HasSeed           bool
		P2PPort           int
		RPCPort           int
		RESTPort          int
		ProfilePort       int
		AcceptKeySend     bool
		AcceptAMP         bool
		FeeURL            string
	}

	nodeState := struct {
		NodeID      int
		Name        string
		PubKey      string
		OpenChans   map[string]int
		ClosedChans map[string]int
		NodeCfg     nodeCfg
	}{
		NodeID:      hn.NodeID,
		Name:        hn.Cfg.Name,
		PubKey:      hn.PubKeyStr,
		OpenChans:   make(map[string]int),
		ClosedChans: make(map[string]int),
		NodeCfg: nodeCfg{
			LogFilenamePrefix: hn.Cfg.LogFilenamePrefix,
			ExtraArgs:         hn.Cfg.ExtraArgs,
			HasSeed:           hn.Cfg.HasSeed,
			P2PPort:           hn.Cfg.P2PPort,
			RPCPort:           hn.Cfg.RPCPort,
			RESTPort:          hn.Cfg.RESTPort,
			AcceptKeySend:     hn.Cfg.AcceptKeySend,
			AcceptAMP:         hn.Cfg.AcceptAMP,
			FeeURL:            hn.Cfg.FeeURL,
		},
	}

	for outpoint, count := range hn.openChans {
		nodeState.OpenChans[outpoint.String()] = count
	}
	for outpoint := range hn.closedChans {
		nodeState.ClosedChans[outpoint.String()] = 1
	}

	stateBytes, err := json.MarshalIndent(nodeState, "", "\t")
	if err != nil {
		return fmt.Sprintf("\n encode node state with err: %v", err)
	}

	return fmt.Sprintf("\nnode state: %s", stateBytes)
}

// DBPath returns the filepath to the channeldb database file for this node.
func (hn *HarnessNode) DBPath() string {
	return hn.Cfg.DBPath()
}

// DBDir returns the path for the directory holding channeldb file(s).
func (hn *HarnessNode) DBDir() string {
	return hn.Cfg.DBDir()
}

// Name returns the name of this node set during initialization.
func (hn *HarnessNode) Name() string {
	return hn.Cfg.Name
}

// TLSCertStr returns the path where the TLS certificate is stored.
func (hn *HarnessNode) TLSCertStr() string {
	return hn.Cfg.TLSCertPath
}

// TLSKeyStr returns the path where the TLS key is stored.
func (hn *HarnessNode) TLSKeyStr() string {
	return hn.Cfg.TLSKeyPath
}

// ChanBackupPath returns the fielpath to the on-disk channel.backup file for
// this node.
func (hn *HarnessNode) ChanBackupPath() string {
	return hn.Cfg.ChanBackupPath()
}

// AdminMacPath returns the filepath to the admin.macaroon file for this node.
func (hn *HarnessNode) AdminMacPath() string {
	return hn.Cfg.AdminMacPath
}

// ReadMacPath returns the filepath to the readonly.macaroon file for this node.
func (hn *HarnessNode) ReadMacPath() string {
	return hn.Cfg.ReadMacPath
}

// InvoiceMacPath returns the filepath to the invoice.macaroon file for this
// node.
func (hn *HarnessNode) InvoiceMacPath() string {
	return hn.Cfg.InvoiceMacPath
}

// startLnd handles the startup of lnd, creating log files, and possibly kills
// the process when needed.
func (hn *HarnessNode) startLnd(lndBinary string, lndError chan<- error) error {
	args := hn.Cfg.GenArgs()
	hn.cmd = exec.Command(lndBinary, args...)

	// Redirect stderr output to buffer
	var errb bytes.Buffer
	hn.cmd.Stderr = &errb

	// If the logoutput flag is passed, redirect output from the nodes to
	// log files.
	var (
		fileName string
		err      error
	)
	if *logOutput {
		fileName, err = addLogFile(hn)
		if err != nil {
			return err
		}
	}

	if err := hn.cmd.Start(); err != nil {
		return err
	}

	// Launch a new goroutine which that bubbles up any potential fatal
	// process errors to the goroutine running the tests.
	hn.wg.Add(1)
	go func() {
		defer hn.wg.Done()

		err := hn.cmd.Wait()
		if err != nil {
			lndError <- fmt.Errorf("%v\n%v", err, errb.String())
		}

		// Make sure log file is closed and renamed if necessary.
		finalizeLogfile(hn, fileName)

		// Rename the etcd.log file if the node was running on embedded
		// etcd.
		finalizeEtcdLog(hn)
	}()

	return nil
}

// Start launches a new process running lnd. Additionally, the PID of the
// launched process is saved in order to possibly kill the process forcibly
// later.
//
// This may not clean up properly if an error is returned, so the caller should
// call shutdown() regardless of the return value.
func (hn *HarnessNode) start(lndBinary string, lndError chan<- error,
	wait bool) error {

	// Init the runCtx.
	ctxt, cancel := context.WithCancel(context.Background())
	hn.runCtx = ctxt
	hn.cancel = cancel

	// Start lnd and prepare logs.
	if err := hn.startLnd(lndBinary, lndError); err != nil {
		return err
	}

	// We may want to skip waiting for the node to come up (eg. the node
	// is waiting to become the leader).
	if !wait {
		return nil
	}

	// Since Stop uses the LightningClient to stop the node, if we fail to
	// get a connected client, we have to kill the process.
	useMacaroons := !hn.Cfg.HasSeed
	conn, err := hn.ConnectRPC(useMacaroons)
	if err != nil {
		err = fmt.Errorf("ConnectRPC err: %w", err)
		cmdErr := hn.cmd.Process.Kill()
		if cmdErr != nil {
			err = fmt.Errorf("kill process got err: %w: %v",
				cmdErr, err)
		}
		return err
	}

	// Init all the RPC clients.
	hn.InitRPCClients(conn)

	if err := hn.WaitUntilStarted(); err != nil {
		return err
	}

	// If the node was created with a seed, we will need to perform an
	// additional step to unlock the wallet. The connection returned will
	// only use the TLS certs, and can only perform operations necessary to
	// unlock the daemon.
	if hn.Cfg.HasSeed {
		return nil
	}

	return hn.initLightningClient()
}

// WaitUntilStarted waits until the wallet state flips from "WAITING_TO_START".
func (hn *HarnessNode) WaitUntilStarted() error {
	return hn.waitTillServerState(func(s lnrpc.WalletState) bool {
		return s != lnrpc.WalletState_WAITING_TO_START
	})
}

// WaitUntilStateReached waits until the given wallet state (or one of the
// states following it) has been reached.
func (hn *HarnessNode) WaitUntilStateReached(
	desiredState lnrpc.WalletState) error {

	return hn.waitTillServerState(func(s lnrpc.WalletState) bool {
		return s >= desiredState
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
		conn, connErr = hn.ConnectRPC(!hn.Cfg.HasSeed)
		return connErr
	}, timeout); err != nil {
		return err
	}

	// Init all the RPC clients.
	hn.InitRPCClients(conn)

	if err := hn.WaitUntilStarted(); err != nil {
		return err
	}

	// If the node was created with a seed, we will need to perform an
	// additional step to unlock the wallet. The connection returned will
	// only use the TLS certs, and can only perform operations necessary to
	// unlock the daemon.
	if hn.Cfg.HasSeed {
		return nil
	}

	return hn.initLightningClient()
}

// initClientWhenReady waits until the main gRPC server is detected as active,
// then complete the normal HarnessNode gRPC connection creation. If the node
// is initialized stateless, the macaroon is returned so that the client can
// use it.
func (hn *HarnessNode) initClientWhenReady(stateless bool,
	macBytes []byte) error {

	// Wait for the wallet to finish unlocking, such that we can connect to
	// it via a macaroon-authenticated rpc connection.
	var (
		conn *grpc.ClientConn
		err  error
	)
	if err = wait.NoError(func() error {
		// If the node has been initialized stateless, we need to pass
		// the macaroon to the client.
		if stateless {
			adminMac := &macaroon.Macaroon{}
			err := adminMac.UnmarshalBinary(macBytes)
			if err != nil {
				return fmt.Errorf("unmarshal failed: %w", err)
			}
			conn, err = hn.ConnectRPCWithMacaroon(adminMac)
			return err
		}

		// Normal initialization, we expect a macaroon to be in the
		// file system.
		conn, err = hn.ConnectRPC(true)
		return err
	}, DefaultTimeout); err != nil {
		return fmt.Errorf("timeout while init client: %w", err)
	}

	// Init all the RPC clients.
	hn.InitRPCClients(conn)

	return hn.initLightningClient()
}

// Init initializes a harness node by passing the init request via rpc. After
// the request is submitted, this method will block until a
// macaroon-authenticated RPC connection can be established to the harness
// node. Once established, the new connection is used to initialize the
// LightningClient and subscribes the HarnessNode to topology changes.
func (hn *HarnessNode) Init(
	initReq *lnrpc.InitWalletRequest) (*lnrpc.InitWalletResponse, error) {

	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	response, err := hn.rpc.WalletUnlocker.InitWallet(ctxt, initReq)
	if err != nil {
		return nil, fmt.Errorf("failed to init wallet: %w", err)
	}

	err = hn.initClientWhenReady(
		initReq.StatelessInit, response.AdminMacaroon,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to init: %w", err)
	}

	return response, nil
}

// InitChangePassword initializes a harness node by passing the change password
// request via RPC. After the request is submitted, this method will block until
// a macaroon-authenticated RPC connection can be established to the harness
// node. Once established, the new connection is used to initialize the
// LightningClient and subscribes the HarnessNode to topology changes.
func (hn *HarnessNode) InitChangePassword(
	chngPwReq *lnrpc.ChangePasswordRequest) (*lnrpc.ChangePasswordResponse,
	error) {

	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	response, err := hn.rpc.WalletUnlocker.ChangePassword(ctxt, chngPwReq)
	if err != nil {
		return nil, err
	}
	err = hn.initClientWhenReady(
		chngPwReq.StatelessInit, response.AdminMacaroon,
	)
	if err != nil {
		return nil, err
	}

	return response, nil
}

// Unlock attempts to unlock the wallet of the target HarnessNode. This method
// should be called after the restart of a HarnessNode that was created with a
// seed+password. Once this method returns, the HarnessNode will be ready to
// accept normal gRPC requests and harness command.
func (hn *HarnessNode) Unlock(unlockReq *lnrpc.UnlockWalletRequest) error {
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	// Otherwise, we'll need to unlock the node before it's able to start
	// up properly.
	_, err := hn.rpc.WalletUnlocker.UnlockWallet(ctxt, unlockReq)
	if err != nil {
		return err
	}

	// Now that the wallet has been unlocked, we'll wait for the RPC client
	// to be ready, then establish the normal gRPC connection.
	return hn.initClientWhenReady(false, nil)
}

// waitTillServerState makes a subscription to the server's state change and
// blocks until the server is in the targeted state.
func (hn *HarnessNode) waitTillServerState(
	predicate func(state lnrpc.WalletState) bool) error {

	ctxt, cancel := context.WithTimeout(hn.runCtx, NodeStartTimeout)
	defer cancel()

	client, err := hn.rpc.State.SubscribeState(
		ctxt, &lnrpc.SubscribeStateRequest{},
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe to state: %w", err)
	}

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

	var lastErr error
	timer := time.After(NodeStartTimeout)
	for {
		select {
		case err := <-errChan:
			lastErr = err

		case <-done:
			return nil

		case <-timer:
			return fmt.Errorf("timeout waiting for state, "+
				"got err from stream: %v", lastErr)
		}
	}
}

// InitRPCClients initializes a list of RPC clients for the node.
func (hn *HarnessNode) InitRPCClients(c *grpc.ClientConn) {
	hn.rpc = &RPCClients{
		conn:             c,
		LN:               lnrpc.NewLightningClient(c),
		Invoice:          invoicesrpc.NewInvoicesClient(c),
		Router:           routerrpc.NewRouterClient(c),
		WalletKit:        walletrpc.NewWalletKitClient(c),
		WalletUnlocker:   lnrpc.NewWalletUnlockerClient(c),
		Watchtower:       watchtowerrpc.NewWatchtowerClient(c),
		WatchtowerClient: wtclientrpc.NewWatchtowerClientClient(c),
		Signer:           signrpc.NewSignerClient(c),
		State:            lnrpc.NewStateClient(c),
		ChainClient:      chainrpc.NewChainNotifierClient(c),
		Peer:             peersrpc.NewPeersClient(c),
	}
}

// initLightningClient blocks until the lnd server is fully started and
// subscribes the harness node to graph topology updates. This method also
// spawns a lightning network watcher for this node, which watches for topology
// changes.
func (hn *HarnessNode) initLightningClient() error {
	// Wait until the server is fully started.
	if err := hn.WaitUntilServerActive(); err != nil {
		return err
	}

	// Set the harness node's pubkey to what the node claims in GetInfo.
	// The RPC must have been started at this point.
	if err := hn.FetchNodeInfo(); err != nil {
		return err
	}

	// Launch the watcher that will hook into graph related topology change
	// from the PoV of this node.
	hn.wg.Add(1)
	go hn.lightningNetworkWatcher()

	return nil
}

// FetchNodeInfo queries an unlocked node to retrieve its public key.
func (hn *HarnessNode) FetchNodeInfo() error {
	// Obtain the lnid of this node for quick identification purposes.
	info, err := hn.rpc.LN.GetInfo(hn.runCtx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	hn.PubKeyStr = info.IdentityPubkey

	pubkey, err := hex.DecodeString(info.IdentityPubkey)
	if err != nil {
		return err
	}
	copy(hn.PubKey[:], pubkey)

	return nil
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
		hn.PrintErrf("write to log err: %v", err)
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
		macBytes, err := ioutil.ReadFile(macPath)
		if err != nil {
			return fmt.Errorf("error reading macaroon file: %v",
				err)
		}

		newMac := &macaroon.Macaroon{}
		if err = newMac.UnmarshalBinary(macBytes); err != nil {
			return fmt.Errorf("error unmarshalling macaroon "+
				"file: %v", err)
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
	}, DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert: %v", err)
	}

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithTransportCredentials(tlsCreds),
	}

	ctx, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	if mac == nil {
		return grpc.DialContext(ctx, hn.Cfg.RPCAddr(), opts...)
	}
	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error cloning mac: %v", err)
	}
	opts = append(opts, grpc.WithPerRPCCredentials(macCred))

	return grpc.DialContext(ctx, hn.Cfg.RPCAddr(), opts...)
}

// ConnectRPC uses the TLS certificate and admin macaroon files written by the
// lnd node to create a gRPC client connection.
func (hn *HarnessNode) ConnectRPC(useMacs bool) (*grpc.ClientConn, error) {
	// If we don't want to use macaroons, just pass nil, the next method
	// will handle it correctly.
	if !useMacs {
		return hn.ConnectRPCWithMacaroon(nil)
	}

	// If we should use a macaroon, always take the admin macaroon as a
	// default.
	mac, err := hn.ReadMacaroon(hn.Cfg.AdminMacPath, DefaultTimeout)
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

// cleanup cleans up all the temporary files created by the node's process.
func (hn *HarnessNode) cleanup() error {
	if hn.backupDbDir != "" {
		err := os.RemoveAll(hn.backupDbDir)
		if err != nil {
			return fmt.Errorf("unable to remove backup dir: %v",
				err)
		}
	}

	return os.RemoveAll(hn.Cfg.BaseDir)
}

// Stop attempts to stop the active lnd process.
func (hn *HarnessNode) stop() error {
	// Do nothing if the process is not running.
	if hn.runCtx == nil {
		return nil
	}

	// If start() failed before creating clients, we will just wait for the
	// child process to die.
	if hn.rpc != nil && hn.rpc.LN != nil {
		// Don't watch for error because sometimes the RPC connection
		// gets closed before a response is returned.
		req := lnrpc.StopRequest{}

		err := wait.NoError(func() error {
			_, err := hn.rpc.LN.StopDaemon(hn.runCtx, &req)
			switch {
			case err == nil:
				return nil

			// Try again if a recovery/rescan is in progress.
			case strings.Contains(
				err.Error(), "recovery in progress",
			):
				return err

			default:
				return nil
			}
		}, DefaultTimeout)
		if err != nil {
			return err
		}
	}

	// Stop the runCtx and wait for goroutines to finish.
	hn.cancel()

	// Wait for lnd process to exit.
	err := wait.NoError(func() error {
		if hn.cmd.ProcessState == nil {
			return fmt.Errorf("process did not exit")
		}

		if !hn.cmd.ProcessState.Exited() {
			return fmt.Errorf("process did not exit")
		}

		// Wait for goroutines to be finished.
		hn.wg.Wait()

		return nil
	}, DefaultTimeout*2)
	if err != nil {
		return err
	}

	// Close any attempts at further grpc connections.
	if hn.rpc.conn != nil {
		err := status.Code(hn.rpc.conn.Close())
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

	return nil
}

// shutdown stops the active lnd process and cleans up any temporary
// directories created along the way.
func (hn *HarnessNode) shutdown() error {
	if err := hn.stop(); err != nil {
		return err
	}
	if err := hn.cleanup(); err != nil {
		return err
	}
	return nil
}

// kill kills the lnd process.
func (hn *HarnessNode) kill() error {
	return hn.cmd.Process.Kill()
}

// WaitForBlockchainSync waits for the target node to be fully synchronized
// with the blockchain. If the passed context object has a set timeout, it will
// continually poll until the timeout has elapsed. In the case that the chain
// isn't synced before the timeout is up, this function will return an error.
func (hn *HarnessNode) WaitForBlockchainSync() error {
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()

	for {
		resp, err := hn.rpc.LN.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
		if err != nil {
			return err
		}
		if resp.SyncedToChain {
			return nil
		}

		select {
		case <-ctxt.Done():
			return fmt.Errorf("timeout while waiting for " +
				"blockchain sync")
		case <-hn.runCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// WaitForBalance waits until the node sees the expected confirmed/unconfirmed
// balance within their wallet.
func (hn *HarnessNode) WaitForBalance(expectedBalance btcutil.Amount,
	confirmed bool) error {

	req := &lnrpc.WalletBalanceRequest{}

	var lastBalance btcutil.Amount
	doesBalanceMatch := func() bool {
		balance, err := hn.rpc.LN.WalletBalance(hn.runCtx, req)
		if err != nil {
			return false
		}

		if confirmed {
			lastBalance = btcutil.Amount(balance.ConfirmedBalance)
			return btcutil.Amount(balance.ConfirmedBalance) ==
				expectedBalance
		}

		lastBalance = btcutil.Amount(balance.UnconfirmedBalance)
		return btcutil.Amount(balance.UnconfirmedBalance) ==
			expectedBalance
	}

	err := wait.Predicate(doesBalanceMatch, DefaultTimeout)
	if err != nil {
		return fmt.Errorf("balances not synced after deadline: "+
			"expected %v, only have %v", expectedBalance,
			lastBalance)
	}

	return nil
}

// PrintErrf prints an error to the console.
func (hn *HarnessNode) PrintErrf(format string, a ...interface{}) {
	fmt.Printf("itest error from [node:%s]: %s\n", // nolint:forbidigo
		hn.Cfg.Name, fmt.Sprintf(format, a...))
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

	return fmt.Sprintf("%s/%d-%s-%s-%s",
		GetLogDir(), hn.NodeID,
		hn.Cfg.LogFilenamePrefix,
		hn.Cfg.Name, pubKeyHex)
}

// finalizeLogfile makes sure the log file cleanup function is initialized,
// even if no log file is created.
func finalizeLogfile(hn *HarnessNode, fileName string) {
	if hn.logFile != nil {
		hn.logFile.Close()

		// If logoutput flag is not set, return early.
		if !*logOutput {
			return
		}

		newFileName := fmt.Sprintf("%v.log",
			getFinalizedLogFilePrefix(hn),
		)

		renameFile(fileName, newFileName)
	}
}

func finalizeEtcdLog(hn *HarnessNode) {
	if hn.Cfg.DbBackend != BackendEtcd {
		return
	}

	etcdLogFileName := fmt.Sprintf("%s/etcd.log", hn.Cfg.LogDir)
	newEtcdLogFileName := fmt.Sprintf("%v-etcd.log",
		getFinalizedLogFilePrefix(hn),
	)

	renameFile(etcdLogFileName, newEtcdLogFileName)
}

func addLogFile(hn *HarnessNode) (string, error) {
	var fileName string

	dir := GetLogDir()
	fileName = fmt.Sprintf("%s/%d-%s-%s-%s.log", dir, hn.NodeID,
		hn.Cfg.LogFilenamePrefix, hn.Cfg.Name,
		hex.EncodeToString(hn.PubKey[:logPubKeyBytes]))

	// If the node's PubKey is not yet initialized, create a
	// temporary file name. Later, after the PubKey has been
	// initialized, the file can be moved to its final name with
	// the PubKey included.
	if bytes.Equal(hn.PubKey[:4], []byte{0, 0, 0, 0}) {
		fileName = fmt.Sprintf("%s/%d-%s-%s-tmp__.log", dir,
			hn.NodeID, hn.Cfg.LogFilenamePrefix,
			hn.Cfg.Name)
	}

	// Create file if not exists, otherwise append.
	file, err := os.OpenFile(fileName,
		os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		return fileName, err
	}

	// Pass node's stderr to both errb and the file.
	w := io.MultiWriter(hn.cmd.Stderr, file)
	hn.cmd.Stderr = w

	// Pass the node's stdout only to the file.
	hn.cmd.Stdout = file

	// Let the node keep a reference to this file, such
	// that we can add to it if necessary.
	hn.logFile = file

	return fileName, nil
}

// waitForInvoiceAccepted waits until the specified invoice moved to the
// specified state by the node or timeout.
func (hn *HarnessNode) waitForInvoiceState(payHash lntypes.Hash,
	state lnrpc.Invoice_InvoiceState) (*lnrpc.Invoice, error) {

	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	invoiceUpdates, err := hn.rpc.Invoice.SubscribeSingleInvoice(
		ctxt, &invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: payHash[:],
		},
	)
	if err != nil {
		return nil, fmt.Errorf("subscribe to invoice:%v failed: %w",
			payHash, err)
	}

	for {
		// The update stream will give an error when the context is
		// timed out.
		update, err := invoiceUpdates.Recv()
		if err != nil {
			return nil, fmt.Errorf("receiving invoice update got "+
				"error: %w", err)
		}

		if update.State == state {
			return update, nil
		}
	}
}
