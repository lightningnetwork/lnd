package lntest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/go-errors/errors"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const (
	// defaultNodePort is the start of the range for listening ports of
	// harness nodes. Ports are monotonically increasing starting from this
	// number and are determined by the results of nextAvailablePort().
	defaultNodePort = 5555

	// logPubKeyBytes is the number of bytes of the node's PubKey that will
	// be appended to the log file name. The whole PubKey is too long and
	// not really necessary to quickly identify what node produced which
	// log file.
	logPubKeyBytes = 4

	// trickleDelay is the amount of time in milliseconds between each
	// release of announcements by AuthenticatedGossiper to the network.
	trickleDelay = 50

	// listenerFormat is the format string that is used to generate local
	// listener addresses.
	listenerFormat = "127.0.0.1:%d"

	// NeutrinoBackendName is the name of the neutrino backend.
	NeutrinoBackendName = "neutrino"

	postgresDsn = "postgres://postgres:postgres@localhost:6432/%s?sslmode=disable"
)

var (
	// numActiveNodes is the number of active nodes within the test network.
	numActiveNodes    = 0
	numActiveNodesMtx sync.Mutex

	// lastPort is the last port determined to be free for use by a new
	// node. It should be used atomically.
	lastPort uint32 = defaultNodePort

	// logOutput is a flag that can be set to append the output from the
	// seed nodes to log files.
	logOutput = flag.Bool("logoutput", false,
		"log output from node n to file output-n.log")

	// logSubDir is the default directory where the logs are written to if
	// logOutput is true.
	logSubDir = flag.String("logdir", ".", "default dir to write logs to")

	// goroutineDump is a flag that can be set to dump the active
	// goroutines of test nodes on failure.
	goroutineDump = flag.Bool("goroutinedump", false,
		"write goroutine dump from node n to file pprof-n.log")

	// btcdExecutable is the full path to the btcd binary.
	btcdExecutable = flag.String(
		"btcdexec", "", "full path to btcd binary",
	)
)

func postgresDatabaseDsn(dbName string) string {
	return fmt.Sprintf(postgresDsn, dbName)
}

// NextAvailablePort returns the first port that is available for listening by
// a new node. It panics if no port is found and the maximum available TCP port
// is reached.
func NextAvailablePort() int {
	port := atomic.AddUint32(&lastPort, 1)
	for port < 65535 {
		// If there are no errors while attempting to listen on this
		// port, close the socket and return it as available. While it
		// could be the case that some other process picks up this port
		// between the time the socket is closed and it's reopened in
		// the harness node, in practice in CI servers this seems much
		// less likely than simply some other process already being
		// bound at the start of the tests.
		addr := fmt.Sprintf(listenerFormat, port)
		l, err := net.Listen("tcp4", addr)
		if err == nil {
			err := l.Close()
			if err == nil {
				return int(port)
			}
		}
		port = atomic.AddUint32(&lastPort, 1)
	}

	// No ports available? Must be a mistake.
	panic("no ports available for listening")
}

// ApplyPortOffset adds the given offset to the lastPort variable, making it
// possible to run the tests in parallel without colliding on the same ports.
func ApplyPortOffset(offset uint32) {
	_ = atomic.AddUint32(&lastPort, offset)
}

// GetLogDir returns the passed --logdir flag or the default value if it wasn't
// set.
func GetLogDir() string {
	if logSubDir != nil && *logSubDir != "" {
		return *logSubDir
	}
	return "."
}

// GetBtcdBinary returns the full path to the binary of the custom built btcd
// executable or an empty string if none is set.
func GetBtcdBinary() string {
	if btcdExecutable != nil {
		return *btcdExecutable
	}

	return ""
}

// GenerateBtcdListenerAddresses is a function that returns two listener
// addresses with unique ports and should be used to overwrite rpctest's default
// generator which is prone to use colliding ports.
func GenerateBtcdListenerAddresses() (string, string) {
	return fmt.Sprintf(listenerFormat, NextAvailablePort()),
		fmt.Sprintf(listenerFormat, NextAvailablePort())
}

// generateListeningPorts returns four ints representing ports to listen on
// designated for the current lightning network test. This returns the next
// available ports for the p2p, rpc, rest and profiling services.
func generateListeningPorts(cfg *NodeConfig) {
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
}

type NodeConfig struct {
	Name string

	// LogFilenamePrefix is is used to prefix node log files. Can be used
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

func (cfg NodeConfig) P2PAddr() string {
	return fmt.Sprintf(listenerFormat, cfg.P2PPort)
}

func (cfg NodeConfig) RPCAddr() string {
	return fmt.Sprintf(listenerFormat, cfg.RPCPort)
}

func (cfg NodeConfig) RESTAddr() string {
	return fmt.Sprintf(listenerFormat, cfg.RESTPort)
}

// DBDir returns the holding directory path of the graph database.
func (cfg NodeConfig) DBDir() string {
	return filepath.Join(cfg.DataDir, "graph", cfg.NetParams.Name)
}

func (cfg NodeConfig) DBPath() string {
	return filepath.Join(cfg.DBDir(), "channel.db")
}

func (cfg NodeConfig) ChanBackupPath() string {
	return filepath.Join(
		cfg.DataDir, "chain", "bitcoin",
		fmt.Sprintf(
			"%v/%v", cfg.NetParams.Name,
			chanbackup.DefaultBackupFileName,
		),
	)
}

// genArgs generates a slice of command line arguments from the lightning node
// config struct.
func (cfg NodeConfig) genArgs() []string {
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
	args = append(args, "--bitcoin.active")
	args = append(args, "--nobootstrap")
	args = append(args, "--debuglevel=debug")
	args = append(args, "--bitcoin.defaultchanconfs=1")
	args = append(args, fmt.Sprintf("--db.batch-commit-interval=%v", 10*time.Millisecond))
	args = append(args, fmt.Sprintf("--bitcoin.defaultremotedelay=%v", DefaultCSV))
	args = append(args, fmt.Sprintf("--rpclisten=%v", cfg.RPCAddr()))
	args = append(args, fmt.Sprintf("--restlisten=%v", cfg.RESTAddr()))
	args = append(args, fmt.Sprintf("--restcors=https://%v", cfg.RESTAddr()))
	args = append(args, fmt.Sprintf("--listen=%v", cfg.P2PAddr()))
	args = append(args, fmt.Sprintf("--externalip=%v", cfg.P2PAddr()))
	args = append(args, fmt.Sprintf("--logdir=%v", cfg.LogDir))
	args = append(args, fmt.Sprintf("--datadir=%v", cfg.DataDir))
	args = append(args, fmt.Sprintf("--tlscertpath=%v", cfg.TLSCertPath))
	args = append(args, fmt.Sprintf("--tlskeypath=%v", cfg.TLSKeyPath))
	args = append(args, fmt.Sprintf("--configfile=%v", cfg.DataDir))
	args = append(args, fmt.Sprintf("--adminmacaroonpath=%v", cfg.AdminMacPath))
	args = append(args, fmt.Sprintf("--readonlymacaroonpath=%v", cfg.ReadMacPath))
	args = append(args, fmt.Sprintf("--invoicemacaroonpath=%v", cfg.InvoiceMacPath))
	args = append(args, fmt.Sprintf("--trickledelay=%v", trickleDelay))
	args = append(args, fmt.Sprintf("--profile=%d", cfg.ProfilePort))

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
// }
type policyUpdateMap map[string]map[string][]*lnrpc.RoutingPolicy

// HarnessNode represents an instance of lnd running within our test network
// harness. Each HarnessNode instance also fully embeds an RPC client in
// order to pragmatically drive the node.
type HarnessNode struct {
	Cfg *NodeConfig

	// NodeID is a unique identifier for the node within a NetworkHarness.
	NodeID int

	// PubKey is the serialized compressed identity public key of the node.
	// This field will only be populated once the node itself has been
	// started via the start() method.
	PubKey    [33]byte
	PubKeyStr string

	cmd     *exec.Cmd
	pidFile string
	logFile *os.File

	// processExit is a channel that's closed once it's detected that the
	// process this instance of HarnessNode is bound to has exited.
	processExit chan struct{}

	chanWatchRequests chan *chanWatchRequest

	// For each outpoint, we'll track an integer which denotes the number of
	// edges seen for that channel within the network. When this number
	// reaches 2, then it means that both edge advertisements has propagated
	// through the network.
	openChans        map[wire.OutPoint]int
	openChanWatchers map[wire.OutPoint][]chan struct{}

	closedChans       map[wire.OutPoint]struct{}
	closeChanWatchers map[wire.OutPoint][]chan struct{}

	// policyUpdates stores a slice of seen polices by each advertising
	// node and the outpoint.
	policyUpdates policyUpdateMap

	quit chan struct{}
	wg   sync.WaitGroup

	lnrpc.LightningClient

	lnrpc.WalletUnlockerClient

	invoicesrpc.InvoicesClient

	// SignerClient cannot be embedded because the name collisions of the
	// methods SignMessage and VerifyMessage.
	SignerClient signrpc.SignerClient

	// conn is the underlying connection to the grpc endpoint of the node.
	conn *grpc.ClientConn

	// RouterClient, WalletKitClient, WatchtowerClient cannot be embedded,
	// because a name collision would occur with LightningClient.
	RouterClient     routerrpc.RouterClient
	WalletKitClient  walletrpc.WalletKitClient
	Watchtower       watchtowerrpc.WatchtowerClient
	WatchtowerClient wtclientrpc.WatchtowerClientClient
	StateClient      lnrpc.StateClient

	// backupDbDir is the path where a database backup is stored, if any.
	backupDbDir string

	// postgresDbName is the name of the postgres database where lnd data is
	// stored in.
	postgresDbName string
}

// Assert *HarnessNode implements the lnrpc.LightningClient interface.
var _ lnrpc.LightningClient = (*HarnessNode)(nil)
var _ lnrpc.WalletUnlockerClient = (*HarnessNode)(nil)
var _ invoicesrpc.InvoicesClient = (*HarnessNode)(nil)

// newNode creates a new test lightning node instance from the passed config.
func newNode(cfg NodeConfig) (*HarnessNode, error) {
	if cfg.BaseDir == "" {
		var err error
		cfg.BaseDir, err = ioutil.TempDir("", "lndtest-node")
		if err != nil {
			return nil, err
		}
	}
	cfg.DataDir = filepath.Join(cfg.BaseDir, "data")
	cfg.LogDir = filepath.Join(cfg.BaseDir, "log")
	cfg.TLSCertPath = filepath.Join(cfg.DataDir, "tls.cert")
	cfg.TLSKeyPath = filepath.Join(cfg.DataDir, "tls.key")

	networkDir := filepath.Join(
		cfg.DataDir, "chain", "bitcoin", cfg.NetParams.Name,
	)
	cfg.AdminMacPath = filepath.Join(networkDir, "admin.macaroon")
	cfg.ReadMacPath = filepath.Join(networkDir, "readonly.macaroon")
	cfg.InvoiceMacPath = filepath.Join(networkDir, "invoice.macaroon")

	generateListeningPorts(&cfg)

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

	numActiveNodesMtx.Lock()
	nodeNum := numActiveNodes
	numActiveNodes++
	numActiveNodesMtx.Unlock()

	return &HarnessNode{
		Cfg:               &cfg,
		NodeID:            nodeNum,
		chanWatchRequests: make(chan *chanWatchRequest),
		openChans:         make(map[wire.OutPoint]int),
		openChanWatchers:  make(map[wire.OutPoint][]chan struct{}),

		closedChans:       make(map[wire.OutPoint]struct{}),
		closeChanWatchers: make(map[wire.OutPoint][]chan struct{}),

		policyUpdates: policyUpdateMap{},

		postgresDbName: dbName,
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

// NewMiner creates a new miner using btcd backend. The baseLogDir specifies
// the miner node's log dir. When tests are finished, during clean up, its log
// files, including any compressed log files from logrotate, are copied to
// logDir as logFilename.
func NewMiner(baseLogDir, logFilename string, netParams *chaincfg.Params,
	handler *rpcclient.NotificationHandlers,
	btcdBinary string) (*rpctest.Harness, func() error, error) {

	args := []string{
		"--rejectnonstd",
		"--txindex",
		"--nowinservice",
		"--nobanning",
		"--debuglevel=debug",
		"--logdir=" + baseLogDir,
		"--trickleinterval=100ms",
		// Don't disconnect if a reply takes too long.
		"--nostalldetect",
	}

	miner, err := rpctest.New(netParams, handler, args, btcdBinary)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"unable to create mining node: %v", err,
		)
	}

	cleanUp := func() error {
		if err := miner.TearDown(); err != nil {
			return fmt.Errorf(
				"failed to tear down miner, got error: %s", err,
			)
		}

		// After shutting down the miner, we'll make a copy of
		// the log files before deleting the temporary log dir.
		logDir := fmt.Sprintf("%s/%s", baseLogDir, netParams.Name)
		files, err := ioutil.ReadDir(logDir)
		if err != nil {
			return fmt.Errorf("unable to read log directory: %v", err)
		}

		for _, file := range files {
			logFile := fmt.Sprintf(
				"%s/%s", logDir, file.Name(),
			)
			newFilename := strings.Replace(file.Name(), "btcd.log", logFilename, 1)
			copyPath := fmt.Sprintf(
				"%s/../%s", baseLogDir, newFilename,
			)

			err := CopyFile(filepath.Clean(copyPath), logFile)
			if err != nil {
				return fmt.Errorf("unable to copy file: %v", err)
			}
		}

		if err = os.RemoveAll(baseLogDir); err != nil {
			return fmt.Errorf(
				"cannot remove dir %s: %v", baseLogDir, err,
			)
		}
		return nil
	}

	return miner, cleanUp, nil
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
		ClosedChans map[string]struct{}
		NodeCfg     nodeCfg
	}{
		NodeID:      hn.NodeID,
		Name:        hn.Cfg.Name,
		PubKey:      hn.PubKeyStr,
		OpenChans:   make(map[string]int),
		ClosedChans: make(map[string]struct{}),
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
	for outpoint, count := range hn.closedChans {
		nodeState.ClosedChans[outpoint.String()] = count
	}

	bytes, err := json.MarshalIndent(nodeState, "", "\t")
	if err != nil {
		return fmt.Sprintf("\n encode node state with err: %v", err)
	}

	return fmt.Sprintf("\nnode state: %s", bytes)
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

// renameFile is a helper to rename (log) files created during integration tests.
func renameFile(fromFileName, toFileName string) {
	err := os.Rename(fromFileName, toFileName)
	if err != nil {
		fmt.Printf("could not rename %s to %s: %v\n",
			fromFileName, toFileName, err)
	}
}

// Start launches a new process running lnd. Additionally, the PID of the
// launched process is saved in order to possibly kill the process forcibly
// later.
//
// This may not clean up properly if an error is returned, so the caller should
// call shutdown() regardless of the return value.
func (hn *HarnessNode) start(lndBinary string, lndError chan<- error,
	wait bool) error {

	hn.quit = make(chan struct{})

	args := hn.Cfg.genArgs()
	hn.cmd = exec.Command(lndBinary, args...)

	// Redirect stderr output to buffer
	var errb bytes.Buffer
	hn.cmd.Stderr = &errb

	// Make sure the log file cleanup function is initialized, even
	// if no log file is created.
	var finalizeLogfile = func() {
		if hn.logFile != nil {
			hn.logFile.Close()
		}
	}

	getFinalizedLogFilePrefix := func() string {
		pubKeyHex := hex.EncodeToString(
			hn.PubKey[:logPubKeyBytes],
		)

		return fmt.Sprintf("%s/%d-%s-%s-%s",
			GetLogDir(), hn.NodeID,
			hn.Cfg.LogFilenamePrefix,
			hn.Cfg.Name, pubKeyHex)
	}

	finalizeEtcdLog := func() {
		if hn.Cfg.DbBackend != BackendEtcd {
			return
		}

		etcdLogFileName := fmt.Sprintf("%s/etcd.log", hn.Cfg.LogDir)
		newEtcdLogFileName := fmt.Sprintf("%v-etcd.log",
			getFinalizedLogFilePrefix(),
		)

		renameFile(etcdLogFileName, newEtcdLogFileName)
	}

	// If the logoutput flag is passed, redirect output from the nodes to
	// log files.
	if *logOutput {
		dir := GetLogDir()
		fileName := fmt.Sprintf("%s/%d-%s-%s-%s.log", dir, hn.NodeID,
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

		// Once the node has done its work, the log file can be
		// renamed.
		finalizeLogfile = func() {
			if hn.logFile != nil {
				hn.logFile.Close()

				newFileName := fmt.Sprintf("%v.log",
					getFinalizedLogFilePrefix(),
				)

				renameFile(fileName, newFileName)
			}
		}

		// Create file if not exists, otherwise append.
		file, err := os.OpenFile(fileName,
			os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
		if err != nil {
			return err
		}

		// Pass node's stderr to both errb and the file.
		w := io.MultiWriter(&errb, file)
		hn.cmd.Stderr = w

		// Pass the node's stdout only to the file.
		hn.cmd.Stdout = file

		// Let the node keep a reference to this file, such
		// that we can add to it if necessary.
		hn.logFile = file
	}

	if err := hn.cmd.Start(); err != nil {
		return err
	}

	// Launch a new goroutine which that bubbles up any potential fatal
	// process errors to the goroutine running the tests.
	hn.processExit = make(chan struct{})
	hn.wg.Add(1)
	go func() {
		defer hn.wg.Done()

		err := hn.cmd.Wait()
		if err != nil {
			lndError <- errors.Errorf("%v\n%v\n", err, errb.String())
		}

		// Signal any onlookers that this process has exited.
		close(hn.processExit)

		// Make sure log file is closed and renamed if necessary.
		finalizeLogfile()

		// Rename the etcd.log file if the node was running on embedded
		// etcd.
		finalizeEtcdLog()
	}()

	// Write process ID to a file.
	if err := hn.writePidFile(); err != nil {
		hn.cmd.Process.Kill()
		return err
	}

	// We may want to skip waiting for the node to come up (eg. the node
	// is waiting to become the leader).
	if !wait {
		return nil
	}

	// Since Stop uses the LightningClient to stop the node, if we fail to get a
	// connected client, we have to kill the process.
	useMacaroons := !hn.Cfg.HasSeed
	conn, err := hn.ConnectRPC(useMacaroons)
	if err != nil {
		hn.cmd.Process.Kill()
		return err
	}

	if err := hn.WaitUntilStarted(conn, DefaultTimeout); err != nil {
		return err
	}

	// If the node was created with a seed, we will need to perform an
	// additional step to unlock the wallet. The connection returned will
	// only use the TLS certs, and can only perform operations necessary to
	// unlock the daemon.
	if hn.Cfg.HasSeed {
		hn.WalletUnlockerClient = lnrpc.NewWalletUnlockerClient(conn)
		return nil
	}

	return hn.initLightningClient(conn)
}

// WaitUntilStarted waits until the wallet state flips from "WAITING_TO_START".
func (hn *HarnessNode) WaitUntilStarted(conn grpc.ClientConnInterface,
	timeout time.Duration) error {

	return hn.waitForState(conn, timeout, func(s lnrpc.WalletState) bool {
		return s != lnrpc.WalletState_WAITING_TO_START
	})
}

// WaitUntilStateReached waits until the given wallet state (or one of the
// states following it) has been reached.
func (hn *HarnessNode) WaitUntilStateReached(conn grpc.ClientConnInterface,
	timeout time.Duration, desiredState lnrpc.WalletState) error {

	return hn.waitForState(conn, timeout, func(s lnrpc.WalletState) bool {
		return s >= desiredState
	})
}

// waitForState waits until the current node state fulfills the given
// predicate.
func (hn *HarnessNode) waitForState(conn grpc.ClientConnInterface,
	timeout time.Duration,
	predicate func(state lnrpc.WalletState) bool) error {

	stateClient := lnrpc.NewStateClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stateStream, err := stateClient.SubscribeState(
		ctx, &lnrpc.SubscribeStateRequest{},
	)
	if err != nil {
		return err
	}

	errChan := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		for {
			resp, err := stateStream.Recv()
			if err != nil {
				errChan <- err
				return
			}

			if predicate(resp.State) {
				close(started)
				return
			}
		}
	}()

	select {

	case <-started:
	case err = <-errChan:

	case <-time.After(timeout):
		return fmt.Errorf("WaitUntilLeader timed out")
	}

	return err
}

// WaitUntilLeader attempts to finish the start procedure by initiating an RPC
// connection and setting up the wallet unlocker client. This is needed when
// a node that has recently been started was waiting to become the leader and
// we're at the point when we expect that it is the leader now (awaiting unlock).
func (hn *HarnessNode) WaitUntilLeader(timeout time.Duration) error {
	var (
		conn    *grpc.ClientConn
		connErr error
	)

	startTs := time.Now()
	if err := wait.NoError(func() error {
		conn, connErr = hn.ConnectRPC(!hn.Cfg.HasSeed)
		return connErr
	}, timeout); err != nil {
		return err
	}
	timeout -= time.Since(startTs)

	if err := hn.WaitUntilStarted(conn, timeout); err != nil {
		return err
	}

	// If the node was created with a seed, we will need to perform an
	// additional step to unlock the wallet. The connection returned will
	// only use the TLS certs, and can only perform operations necessary to
	// unlock the daemon.
	if hn.Cfg.HasSeed {
		hn.WalletUnlockerClient = lnrpc.NewWalletUnlockerClient(conn)
		return nil
	}

	return hn.initLightningClient(conn)
}

// initClientWhenReady waits until the main gRPC server is detected as active,
// then complete the normal HarnessNode gRPC connection creation. This can be
// used it a node has just been unlocked, or has its wallet state initialized.
func (hn *HarnessNode) initClientWhenReady(timeout time.Duration) error {
	var (
		conn    *grpc.ClientConn
		connErr error
	)
	if err := wait.NoError(func() error {
		conn, connErr = hn.ConnectRPC(true)
		return connErr
	}, timeout); err != nil {
		return err
	}

	return hn.initLightningClient(conn)
}

// Init initializes a harness node by passing the init request via rpc. After
// the request is submitted, this method will block until a
// macaroon-authenticated RPC connection can be established to the harness node.
// Once established, the new connection is used to initialize the
// LightningClient and subscribes the HarnessNode to topology changes.
func (hn *HarnessNode) Init(ctx context.Context,
	initReq *lnrpc.InitWalletRequest) (*lnrpc.InitWalletResponse, error) {

	ctxt, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	response, err := hn.InitWallet(ctxt, initReq)
	if err != nil {
		return nil, err
	}

	// Wait for the wallet to finish unlocking, such that we can connect to
	// it via a macaroon-authenticated rpc connection.
	var conn *grpc.ClientConn
	if err = wait.Predicate(func() bool {
		// If the node has been initialized stateless, we need to pass
		// the macaroon to the client.
		if initReq.StatelessInit {
			adminMac := &macaroon.Macaroon{}
			err := adminMac.UnmarshalBinary(response.AdminMacaroon)
			if err != nil {
				return false
			}
			conn, err = hn.ConnectRPCWithMacaroon(adminMac)
			return err == nil
		}

		// Normal initialization, we expect a macaroon to be in the
		// file system.
		conn, err = hn.ConnectRPC(true)
		return err == nil
	}, DefaultTimeout); err != nil {
		return nil, err
	}

	return response, hn.initLightningClient(conn)
}

// InitChangePassword initializes a harness node by passing the change password
// request via RPC. After the request is submitted, this method will block until
// a macaroon-authenticated RPC connection can be established to the harness
// node. Once established, the new connection is used to initialize the
// LightningClient and subscribes the HarnessNode to topology changes.
func (hn *HarnessNode) InitChangePassword(ctx context.Context,
	chngPwReq *lnrpc.ChangePasswordRequest) (*lnrpc.ChangePasswordResponse,
	error) {

	ctxt, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	response, err := hn.ChangePassword(ctxt, chngPwReq)
	if err != nil {
		return nil, err
	}

	// Wait for the wallet to finish unlocking, such that we can connect to
	// it via a macaroon-authenticated rpc connection.
	var conn *grpc.ClientConn
	if err = wait.Predicate(func() bool {
		// If the node has been initialized stateless, we need to pass
		// the macaroon to the client.
		if chngPwReq.StatelessInit {
			adminMac := &macaroon.Macaroon{}
			err := adminMac.UnmarshalBinary(response.AdminMacaroon)
			if err != nil {
				return false
			}
			conn, err = hn.ConnectRPCWithMacaroon(adminMac)
			return err == nil
		}

		// Normal initialization, we expect a macaroon to be in the
		// file system.
		conn, err = hn.ConnectRPC(true)
		return err == nil
	}, DefaultTimeout); err != nil {
		return nil, err
	}

	return response, hn.initLightningClient(conn)
}

// Unlock attempts to unlock the wallet of the target HarnessNode. This method
// should be called after the restart of a HarnessNode that was created with a
// seed+password. Once this method returns, the HarnessNode will be ready to
// accept normal gRPC requests and harness command.
func (hn *HarnessNode) Unlock(ctx context.Context,
	unlockReq *lnrpc.UnlockWalletRequest) error {

	ctxt, _ := context.WithTimeout(ctx, DefaultTimeout)

	// Otherwise, we'll need to unlock the node before it's able to start
	// up properly.
	if _, err := hn.UnlockWallet(ctxt, unlockReq); err != nil {
		return err
	}

	// Now that the wallet has been unlocked, we'll wait for the RPC client
	// to be ready, then establish the normal gRPC connection.
	return hn.initClientWhenReady(DefaultTimeout)
}

// waitTillServerStarted makes a subscription to the server's state change and
// blocks until the server is in state ServerActive.
func (hn *HarnessNode) waitTillServerStarted() error {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, NodeStartTimeout)
	defer cancel()

	client, err := hn.StateClient.SubscribeState(
		ctxt, &lnrpc.SubscribeStateRequest{},
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe to state: %w", err)
	}

	for {
		resp, err := client.Recv()
		if err != nil {
			return fmt.Errorf("failed to receive state "+
				"client stream: %w", err)
		}

		if resp.State == lnrpc.WalletState_SERVER_ACTIVE {
			return nil
		}
	}

}

// initLightningClient constructs the grpc LightningClient from the given client
// connection and subscribes the harness node to graph topology updates.
// This method also spawns a lightning network watcher for this node,
// which watches for topology changes.
func (hn *HarnessNode) initLightningClient(conn *grpc.ClientConn) error {
	// Construct the LightningClient that will allow us to use the
	// HarnessNode directly for normal rpc operations.
	hn.conn = conn
	hn.LightningClient = lnrpc.NewLightningClient(conn)
	hn.InvoicesClient = invoicesrpc.NewInvoicesClient(conn)
	hn.RouterClient = routerrpc.NewRouterClient(conn)
	hn.WalletKitClient = walletrpc.NewWalletKitClient(conn)
	hn.Watchtower = watchtowerrpc.NewWatchtowerClient(conn)
	hn.WatchtowerClient = wtclientrpc.NewWatchtowerClientClient(conn)
	hn.SignerClient = signrpc.NewSignerClient(conn)
	hn.StateClient = lnrpc.NewStateClient(conn)

	// Wait until the server is fully started.
	if err := hn.waitTillServerStarted(); err != nil {
		return err
	}

	// Set the harness node's pubkey to what the node claims in GetInfo.
	// Since the RPC might not be immediately active, we wrap the call in a
	// wait.NoError.
	if err := wait.NoError(hn.FetchNodeInfo, DefaultTimeout); err != nil {
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
	ctxb := context.Background()
	info, err := hn.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
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

// AddToLog adds a line of choice to the node's logfile. This is useful
// to interleave test output with output from the node.
func (hn *HarnessNode) AddToLog(format string, a ...interface{}) {
	// If this node was not set up with a log file, just return early.
	if hn.logFile == nil {
		return
	}

	desc := fmt.Sprintf("itest: %s\n", fmt.Sprintf(format, a...))
	if _, err := hn.logFile.WriteString(desc); err != nil {
		hn.PrintErr("write to log err: %v", err)
	}
}

// writePidFile writes the process ID of the running lnd process to a .pid file.
func (hn *HarnessNode) writePidFile() error {
	filePath := filepath.Join(hn.Cfg.BaseDir, fmt.Sprintf("%v.pid", hn.NodeID))

	pid, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer pid.Close()

	_, err = fmt.Fprintf(pid, "%v\n", hn.cmd.Process.Pid)
	if err != nil {
		return err
	}

	hn.pidFile = filePath
	return nil
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
			return fmt.Errorf("error reading macaroon file: %v", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
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
			return fmt.Errorf("unable to remove backup dir: %v", err)
		}
	}

	return os.RemoveAll(hn.Cfg.BaseDir)
}

// Stop attempts to stop the active lnd process.
func (hn *HarnessNode) stop() error {
	// Do nothing if the process is not running.
	if hn.processExit == nil {
		return nil
	}

	// If start() failed before creating a client, we will just wait for the
	// child process to die.
	if hn.LightningClient != nil {
		// Don't watch for error because sometimes the RPC connection gets
		// closed before a response is returned.
		req := lnrpc.StopRequest{}
		ctx := context.Background()

		err := wait.NoError(func() error {
			_, err := hn.LightningClient.StopDaemon(ctx, &req)
			switch {
			case err == nil:
				return nil

			// Try again if a recovery/rescan is in progress.
			case strings.Contains(err.Error(), "recovery in progress"):
				return err

			default:
				return nil
			}
		}, DefaultTimeout)
		if err != nil {
			return err
		}
	}

	// Wait for lnd process and other goroutines to exit.
	select {
	case <-hn.processExit:
	case <-time.After(DefaultTimeout * 2):
		return fmt.Errorf("process did not exit")
	}

	close(hn.quit)
	hn.wg.Wait()

	hn.quit = nil
	hn.processExit = nil
	hn.LightningClient = nil
	hn.WalletUnlockerClient = nil
	hn.Watchtower = nil
	hn.WatchtowerClient = nil

	// Close any attempts at further grpc connections.
	if hn.conn != nil {
		err := hn.conn.Close()
		if err != nil &&
			!strings.Contains(err.Error(), "connection is closing") {

			return fmt.Errorf("error attempting to stop grpc "+
				"client: %v", err)
		}
	}

	return nil
}

// shutdown stops the active lnd process and cleans up any temporary directories
// created along the way.
func (hn *HarnessNode) shutdown() error {
	if err := hn.stop(); err != nil {
		return err
	}
	if err := hn.cleanup(); err != nil {
		return err
	}
	return nil
}

// kill kills the lnd process
func (hn *HarnessNode) kill() error {
	return hn.cmd.Process.Kill()
}

type chanWatchType uint8

const (
	// watchOpenChannel specifies that this is a request to watch an open
	// channel event.
	watchOpenChannel chanWatchType = iota

	// watchCloseChannel specifies that this is a request to watch a close
	// channel event.
	watchCloseChannel

	// watchPolicyUpdate specifies that this is a request to watch a policy
	// update event.
	watchPolicyUpdate
)

// closeChanWatchRequest is a request to the lightningNetworkWatcher to be
// notified once it's detected within the test Lightning Network, that a
// channel has either been added or closed.
type chanWatchRequest struct {
	chanPoint wire.OutPoint

	chanWatchType chanWatchType

	eventChan chan struct{}

	advertisingNode    string
	policy             *lnrpc.RoutingPolicy
	includeUnannounced bool
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

func checkChanPointInGraph(ctx context.Context,
	node *HarnessNode, chanPoint wire.OutPoint) bool {

	ctxt, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	chanGraph, err := node.DescribeGraph(ctxt, &lnrpc.ChannelGraphRequest{})
	if err != nil {
		return false
	}

	targetChanPoint := chanPoint.String()
	for _, chanEdge := range chanGraph.Edges {
		candidateChanPoint := chanEdge.ChanPoint
		if targetChanPoint == candidateChanPoint {
			return true
		}
	}

	return false
}

// lightningNetworkWatcher is a goroutine which is able to dispatch
// notifications once it has been observed that a target channel has been
// closed or opened within the network. In order to dispatch these
// notifications, the GraphTopologySubscription client exposed as part of the
// gRPC interface is used.
func (hn *HarnessNode) lightningNetworkWatcher() {
	defer hn.wg.Done()

	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate)

	// Start a goroutine to receive graph updates.
	hn.wg.Add(1)
	go func() {
		defer hn.wg.Done()
		err := hn.receiveTopologyClientStream(graphUpdates)
		if err != nil {
			hn.PrintErr("receive topology client stream "+
				"got err:%v", err)
		}
	}()

	for {
		select {

		// A new graph update has just been received, so we'll examine
		// the current set of registered clients to see if we can
		// dispatch any requests.
		case graphUpdate := <-graphUpdates:
			hn.handleChannelEdgeUpdates(graphUpdate.ChannelUpdates)
			hn.handleClosedChannelUpdate(graphUpdate.ClosedChans)
			// TODO(yy): handle node updates too

		// A new watch request, has just arrived. We'll either be able
		// to dispatch immediately, or need to add the client for
		// processing later.
		case watchRequest := <-hn.chanWatchRequests:
			switch watchRequest.chanWatchType {
			case watchOpenChannel:
				// TODO(roasbeef): add update type also, checks
				// for multiple of 2
				hn.handleOpenChannelWatchRequest(watchRequest)

			case watchCloseChannel:
				hn.handleCloseChannelWatchRequest(watchRequest)

			case watchPolicyUpdate:
				hn.handlePolicyUpdateWatchRequest(watchRequest)
			}

		case <-hn.quit:
			return
		}
	}
}

// WaitForNetworkChannelOpen will block until a channel with the target
// outpoint is seen as being fully advertised within the network. A channel is
// considered "fully advertised" once both of its directional edges has been
// advertised within the test Lightning Network.
func (hn *HarnessNode) WaitForNetworkChannelOpen(ctx context.Context,
	chanPoint *lnrpc.ChannelPoint) error {

	eventChan := make(chan struct{})

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return fmt.Errorf("failed to create outpoint for %v "+
			"got err: %v", chanPoint, err)
	}

	hn.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchOpenChannel,
	}

	select {
	case <-eventChan:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("channel:%s not opened before timeout: %s",
			op, hn)
	}
}

// WaitForNetworkChannelClose will block until a channel with the target
// outpoint is seen as closed within the network. A channel is considered
// closed once a transaction spending the funding outpoint is seen within a
// confirmed block.
func (hn *HarnessNode) WaitForNetworkChannelClose(ctx context.Context,
	chanPoint *lnrpc.ChannelPoint) error {

	eventChan := make(chan struct{})

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return fmt.Errorf("failed to create outpoint for %v "+
			"got err: %v", chanPoint, err)
	}

	hn.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchCloseChannel,
	}

	select {
	case <-eventChan:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("channel:%s not closed before timeout: "+
			"%s", op, hn)
	}
}

// WaitForChannelPolicyUpdate will block until a channel policy with the target
// outpoint and advertisingNode is seen within the network.
func (hn *HarnessNode) WaitForChannelPolicyUpdate(ctx context.Context,
	advertisingNode string, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) error {

	eventChan := make(chan struct{})

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return fmt.Errorf("failed to create outpoint for %v"+
			"got err: %v", chanPoint, err)
	}

	ticker := time.NewTicker(wait.PollInterval)
	defer ticker.Stop()

	for {
		select {
		// Send a watch request every second.
		case <-ticker.C:
			// Did the event can close in the meantime? We want to
			// avoid a "close of closed channel" panic since we're
			// re-using the same event chan for multiple requests.
			select {
			case <-eventChan:
				return nil
			default:
			}

			hn.chanWatchRequests <- &chanWatchRequest{
				chanPoint:          op,
				eventChan:          eventChan,
				chanWatchType:      watchPolicyUpdate,
				policy:             policy,
				advertisingNode:    advertisingNode,
				includeUnannounced: includeUnannounced,
			}

		case <-eventChan:
			return nil

		case <-ctx.Done():
			return fmt.Errorf("channel:%s policy not updated "+
				"before timeout: [%s:%v] %s", op,
				advertisingNode, policy, hn.String())
		}
	}
}

// WaitForBlockchainSync waits for the target node to be fully synchronized with
// the blockchain. If the passed context object has a set timeout, it will
// continually poll until the timeout has elapsed. In the case that the chain
// isn't synced before the timeout is up, this function will return an error.
func (hn *HarnessNode) WaitForBlockchainSync(ctx context.Context) error {
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()

	for {
		resp, err := hn.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			return err
		}
		if resp.SyncedToChain {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout while waiting for " +
				"blockchain sync")
		case <-hn.quit:
			return nil
		case <-ticker.C:
		}
	}
}

// WaitForBalance waits until the node sees the expected confirmed/unconfirmed
// balance within their wallet.
func (hn *HarnessNode) WaitForBalance(expectedBalance btcutil.Amount, confirmed bool) error {
	ctx := context.Background()
	req := &lnrpc.WalletBalanceRequest{}

	var lastBalance btcutil.Amount
	doesBalanceMatch := func() bool {
		balance, err := hn.WalletBalance(ctx, req)
		if err != nil {
			return false
		}

		if confirmed {
			lastBalance = btcutil.Amount(balance.ConfirmedBalance)
			return btcutil.Amount(balance.ConfirmedBalance) == expectedBalance
		}

		lastBalance = btcutil.Amount(balance.UnconfirmedBalance)
		return btcutil.Amount(balance.UnconfirmedBalance) == expectedBalance
	}

	err := wait.Predicate(doesBalanceMatch, DefaultTimeout)
	if err != nil {
		return fmt.Errorf("balances not synced after deadline: "+
			"expected %v, only have %v", expectedBalance, lastBalance)
	}

	return nil
}

// PrintErr prints an error to the console.
func (hn *HarnessNode) PrintErr(format string, a ...interface{}) {
	fmt.Printf("itest error from [node:%s]: %s\n",
		hn.Cfg.Name, fmt.Sprintf(format, a...))
}

// MakeOutpoint returns the outpoint of the channel's funding transaction.
func MakeOutpoint(chanPoint *lnrpc.ChannelPoint) (wire.OutPoint, error) {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return wire.OutPoint{}, err
	}

	return wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}, nil
}

// handleChannelEdgeUpdates takes a series of channel edge updates, extracts
// the outpoints, and saves them to harness node's internal state.
func (hn *HarnessNode) handleChannelEdgeUpdates(
	updates []*lnrpc.ChannelEdgeUpdate) {

	// For each new channel, we'll increment the number of
	// edges seen by one.
	for _, newChan := range updates {
		op, err := MakeOutpoint(newChan.ChanPoint)
		if err != nil {
			hn.PrintErr("failed to create outpoint for %v "+
				"got err: %v", newChan.ChanPoint, err)
			return
		}
		hn.openChans[op]++

		// For this new channel, if the number of edges seen is less
		// than two, then the channel hasn't been fully announced yet.
		if numEdges := hn.openChans[op]; numEdges < 2 {
			return
		}

		// Otherwise, we'll notify all the registered watchers and
		// remove the dispatched watchers.
		for _, eventChan := range hn.openChanWatchers[op] {
			close(eventChan)
		}
		delete(hn.openChanWatchers, op)

		// Check whether there's a routing policy update. If so, save
		// it to the node state.
		if newChan.RoutingPolicy == nil {
			continue
		}

		// Append the policy to the slice.
		node := newChan.AdvertisingNode
		policies := hn.policyUpdates[op.String()]

		// If the map[op] is nil, we need to initialize the map first.
		if policies == nil {
			policies = make(map[string][]*lnrpc.RoutingPolicy)
		}
		policies[node] = append(
			policies[node], newChan.RoutingPolicy,
		)
		hn.policyUpdates[op.String()] = policies
	}
}

// handleOpenChannelWatchRequest processes a watch open channel request by
// checking the number of the edges seen for a given channel point. If the
// number is no less than 2 then the channel is considered open. Otherwise, we
// will attempt to find it in its channel graph. If neither can be found, the
// request is added to a watch request list than will be handled by
// handleChannelEdgeUpdates.
func (hn *HarnessNode) handleOpenChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is an open request, then it can be dispatched if the number
	// of edges seen for the channel is at least two.
	if numEdges := hn.openChans[targetChan]; numEdges >= 2 {
		close(req.eventChan)
		return
	}

	// Before we add the channel to our set of open clients, we'll check to
	// see if the channel is already in the channel graph of the target
	// node. This lets us handle the case where a node has already seen a
	// channel before a notification has been requested, causing us to miss
	// it.
	chanFound := checkChanPointInGraph(context.Background(), hn, targetChan)
	if chanFound {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of open channel watchers for
	// this out point.
	hn.openChanWatchers[targetChan] = append(
		hn.openChanWatchers[targetChan],
		req.eventChan,
	)
}

// handleClosedChannelUpdate takes a series of closed channel updates, extracts
// the outpoints, saves them to harness node's internal state, and notifies all
// registered clients.
func (hn *HarnessNode) handleClosedChannelUpdate(
	updates []*lnrpc.ClosedChannelUpdate) {

	// For each channel closed, we'll mark that we've detected a channel
	// closure while lnd was pruning the channel graph.
	for _, closedChan := range updates {
		op, err := MakeOutpoint(closedChan.ChanPoint)
		if err != nil {
			hn.PrintErr("failed to create outpoint for %v "+
				"got err: %v", closedChan.ChanPoint, err)
			return
		}

		hn.closedChans[op] = struct{}{}

		// As the channel has been closed, we'll notify all register
		// watchers.
		for _, eventChan := range hn.closeChanWatchers[op] {
			close(eventChan)
		}
		delete(hn.closeChanWatchers, op)
	}
}

// handleCloseChannelWatchRequest processes a watch close channel request by
// checking whether the given channel point can be found in the node's internal
// state. If not, the request is added to a watch request list than will be
// handled by handleCloseChannelWatchRequest.
func (hn *HarnessNode) handleCloseChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is a close request, then it can be immediately dispatched if
	// we've already seen a channel closure for this channel.
	if _, ok := hn.closedChans[targetChan]; ok {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of close channel watchers for
	// this out point.
	hn.closeChanWatchers[targetChan] = append(
		hn.closeChanWatchers[targetChan],
		req.eventChan,
	)
}

type topologyClient lnrpc.Lightning_SubscribeChannelGraphClient

// newTopologyClient creates a topology client.
func (hn *HarnessNode) newTopologyClient(
	ctx context.Context) (topologyClient, error) {

	req := &lnrpc.GraphTopologySubscription{}
	client, err := hn.SubscribeChannelGraph(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s(%d): unable to create topology "+
			"client: %v (%s)", hn.Name(), hn.NodeID, err,
			time.Now().String())
	}

	return client, nil
}

// receiveTopologyClientStream initializes a topologyClient to subscribe
// topology update events. Due to a race condition between the ChannelRouter
// starting and us making the subscription request, it's possible for our graph
// subscription to fail. In that case, we will retry the subscription until it
// succeeds or fail after 10 seconds.
//
// NOTE: must be run as a goroutine.
func (hn *HarnessNode) receiveTopologyClientStream(
	receiver chan *lnrpc.GraphTopologyUpdate) error {

	ctxb := context.Background()

	// Create a topology client to receive graph updates.
	client, err := hn.newTopologyClient(ctxb)
	if err != nil {
		return fmt.Errorf("create topologyClient failed: %v", err)
	}

	// We use the context to time out when retrying graph subscription.
	ctxt, cancel := context.WithTimeout(ctxb, DefaultTimeout)
	defer cancel()

	for {
		update, err := client.Recv()

		switch {
		case err == nil:
			// Good case. We will send the update to the receiver.

		case strings.Contains(err.Error(), "router not started"):
			// If the router hasn't been started, we will retry
			// every 200 ms until it has been started or fail
			// after the ctxt is timed out.
			select {
			case <-ctxt.Done():
				return fmt.Errorf("graph subscription: " +
					"router not started before timeout")
			case <-time.After(wait.PollInterval):
			case <-hn.quit:
				return nil
			}

			// Re-create the topology client.
			client, err = hn.newTopologyClient(ctxb)
			if err != nil {
				return fmt.Errorf("create topologyClient "+
					"failed: %v", err)
			}

			continue

		case strings.Contains(err.Error(), "EOF"):
			// End of subscription stream. Do nothing and quit.
			return nil

		default:
			// An expected error is returned, return and leave it
			// to be handled by the caller.
			return fmt.Errorf("graph subscription err: %v", err)
		}

		// Send the update or quit.
		select {
		case receiver <- update:
		case <-hn.quit:
			return nil
		}
	}
}

// CheckChannelPolicy checks that the policy matches the expected one.
func CheckChannelPolicy(policy, expectedPolicy *lnrpc.RoutingPolicy) error {
	if policy.FeeBaseMsat != expectedPolicy.FeeBaseMsat {
		return fmt.Errorf("expected base fee %v, got %v",
			expectedPolicy.FeeBaseMsat, policy.FeeBaseMsat)
	}
	if policy.FeeRateMilliMsat != expectedPolicy.FeeRateMilliMsat {
		return fmt.Errorf("expected fee rate %v, got %v",
			expectedPolicy.FeeRateMilliMsat,
			policy.FeeRateMilliMsat)
	}
	if policy.TimeLockDelta != expectedPolicy.TimeLockDelta {
		return fmt.Errorf("expected time lock delta %v, got %v",
			expectedPolicy.TimeLockDelta,
			policy.TimeLockDelta)
	}
	if policy.MinHtlc != expectedPolicy.MinHtlc {
		return fmt.Errorf("expected min htlc %v, got %v",
			expectedPolicy.MinHtlc, policy.MinHtlc)
	}
	if policy.MaxHtlcMsat != expectedPolicy.MaxHtlcMsat {
		return fmt.Errorf("expected max htlc %v, got %v",
			expectedPolicy.MaxHtlcMsat, policy.MaxHtlcMsat)
	}
	if policy.Disabled != expectedPolicy.Disabled {
		return errors.New("edge should be disabled but isn't")
	}

	return nil
}

// handlePolicyUpdateWatchRequest checks that if the expected policy can be
// found either in the node's interval state or describe graph response. If
// found, it will signal the request by closing the event channel. Otherwise it
// does nothing but returns nil.
func (hn *HarnessNode) handlePolicyUpdateWatchRequest(req *chanWatchRequest) {
	op := req.chanPoint

	// Get a list of known policies for this chanPoint+advertisingNode
	// combination. Start searching in the node state first.
	policies, ok := hn.policyUpdates[op.String()][req.advertisingNode]

	if !ok {
		// If it cannot be found in the node state, try searching it
		// from the node's DescribeGraph.
		policyMap := hn.getChannelPolicies(req.includeUnannounced)
		policies, ok = policyMap[op.String()][req.advertisingNode]
		if !ok {
			return
		}
	}

	// Check if there's a matched policy.
	for _, policy := range policies {
		if CheckChannelPolicy(policy, req.policy) == nil {
			close(req.eventChan)
			return
		}
	}
}

// getChannelPolicies queries the channel graph and formats the policies into
// the format defined in type policyUpdateMap.
func (hn *HarnessNode) getChannelPolicies(include bool) policyUpdateMap {

	ctxt, cancel := context.WithTimeout(
		context.Background(), DefaultTimeout,
	)
	defer cancel()

	graph, err := hn.DescribeGraph(ctxt, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: include,
	})
	if err != nil {
		hn.PrintErr("DescribeGraph got err: %v", err)
		return nil
	}

	policyUpdates := policyUpdateMap{}

	for _, e := range graph.Edges {

		policies := policyUpdates[e.ChanPoint]

		// If the map[op] is nil, we need to initialize the map first.
		if policies == nil {
			policies = make(map[string][]*lnrpc.RoutingPolicy)
		}

		if e.Node1Policy != nil {
			policies[e.Node1Pub] = append(
				policies[e.Node1Pub], e.Node1Policy,
			)
		}

		if e.Node2Policy != nil {
			policies[e.Node2Pub] = append(
				policies[e.Node2Pub], e.Node2Policy,
			)
		}

		policyUpdates[e.ChanPoint] = policies
	}

	return policyUpdates
}
