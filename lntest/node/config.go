package node

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

const (
	// ListenerFormat is the format string that is used to generate local
	// listener addresses.
	ListenerFormat = "127.0.0.1:%d"

	// DefaultCSV is the CSV delay (remotedelay) we will start our test
	// nodes with.
	DefaultCSV = 4

	// defaultNodePort is the start of the range for listening ports of
	// harness nodes. Ports are monotonically increasing starting from this
	// number and are determined by the results of NextAvailablePort().
	defaultNodePort = 5555
)

var (
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

	// btcdExecutable is the full path to the btcd binary.
	btcdExecutable = flag.String(
		"btcdexec", "", "full path to btcd binary",
	)
)

type DatabaseBackend int

const (
	BackendBbolt DatabaseBackend = iota
	BackendEtcd
	BackendPostgres
	BackendSqlite
)

// Option is a function for updating a node's configuration.
type Option func(*BaseNodeConfig)

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

// BaseNodeConfig is the base node configuration.
type BaseNodeConfig struct {
	Name string

	// LogFilenamePrefix is used to prefix node log files. Can be used to
	// store the current test case for simpler postmortem debugging.
	LogFilenamePrefix string

	NetParams         *chaincfg.Params
	BackendCfg        BackendConfig
	BaseDir           string
	ExtraArgs         []string
	OriginalExtraArgs []string

	DataDir        string
	LogDir         string
	TLSCertPath    string
	TLSKeyPath     string
	AdminMacPath   string
	ReadMacPath    string
	InvoiceMacPath string

	SkipUnlock bool
	Password   []byte

	P2PPort     int
	RPCPort     int
	RESTPort    int
	ProfilePort int

	FeeURL string

	DBBackend   DatabaseBackend
	PostgresDsn string

	// NodeID is a unique ID used to identify the node.
	NodeID uint32

	// LndBinary is the full path to the lnd binary that was specifically
	// compiled with all required itest flags.
	LndBinary string

	// backupDBDir is the path where a database backup is stored, if any.
	backupDBDir string

	// postgresDBName is the name of the postgres database where lnd data
	// is stored in.
	postgresDBName string
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
		"--debuglevel=debug,DISC=trace",
		"--bitcoin.defaultchanconfs=1",
		"--accept-keysend",
		"--keep-failed-payment-attempts",
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

		// Use a small batch window so we can broadcast our sweep
		// transactions faster.
		"--sweeper.batchwindowduration=5s",

		// Use a small batch delay so we can broadcast the
		// announcements quickly in the tests.
		"--gossip.sub-batch-delay=5ms",

		// Use a small cache duration so the `DescribeGraph` can be
		// updated quicker.
		"--caches.rpc-graph-cache-duration=100ms",

		// Speed up the tests for bitcoind backend.
		"--bitcoind.blockpollinginterval=100ms",
		"--bitcoind.txpollinginterval=100ms",
	}

	args = append(args, nodeArgs...)

	if cfg.Password == nil {
		args = append(args, "--noseedbackup")
	}

	switch cfg.DBBackend {
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

	case BackendSqlite:
		args = append(args, "--db.backend=sqlite")
		args = append(args, fmt.Sprintf("--db.sqlite.busytimeout=%v",
			wait.SqliteBusyTimeout))
	}

	if cfg.FeeURL != "" {
		args = append(args, "--feeurl="+cfg.FeeURL)
	}

	// Put extra args in the end so the args can be overwritten.
	if cfg.ExtraArgs != nil {
		args = append(args, cfg.ExtraArgs...)
	}

	return args
}

// ExtraArgsEtcd returns extra args for configuring LND to use an external etcd
// database (for remote channel DB and wallet DB).
func ExtraArgsEtcd(etcdCfg *etcd.Config, name string, cluster bool,
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
		addr := fmt.Sprintf(ListenerFormat, port)
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

// GetLogDir returns the passed --logdir flag or the default value if it wasn't
// set.
func GetLogDir() string {
	if logSubDir != nil && *logSubDir != "" {
		return *logSubDir
	}

	return "."
}

// CopyFile copies the file src to dest.
func CopyFile(dest, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}

	return d.Close()
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
// addresses with unique ports and should be used to overwrite rpctest's
// default generator which is prone to use colliding ports.
func GenerateBtcdListenerAddresses() (string, string) {
	return fmt.Sprintf(ListenerFormat, NextAvailablePort()),
		fmt.Sprintf(ListenerFormat, NextAvailablePort())
}

// ApplyPortOffset adds the given offset to the lastPort variable, making it
// possible to run the tests in parallel without colliding on the same ports.
func ApplyPortOffset(offset uint32) {
	_ = atomic.AddUint32(&lastPort, offset)
}
