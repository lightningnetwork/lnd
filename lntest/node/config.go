package node

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/wait"
)

const (
	// ListenerFormat is the format string that is used to generate local
	// listener addresses.
	ListenerFormat = "127.0.0.1:%d"

	// DefaultCSV is the CSV delay (remotedelay) we will start our test
	// nodes with.
	DefaultCSV = 4
)

var (
	// baseDirFlag is the default directory where all the node's data are
	// saved. If not set, a temporary dir will be created.
	baseDirFlag = flag.String("basedir", "", "default dir to save data")

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

	// CfgLegacy specifies the config used to create a node that uses the
	// legacy channel format.
	CfgLegacy = []string{"--protocol.legacy.committweak"}

	// CfgStaticRemoteKey specifies the config used to create a node that
	// uses the static remote key feature.
	CfgStaticRemoteKey = []string{}

	// CfgAnchor specifies the config used to create a node that uses the
	// anchor output feature.
	CfgAnchor = []string{"--protocol.anchors"}

	// CfgLeased specifies the config used to create a node that uses the
	// leased channel feature.
	CfgLeased = []string{
		"--protocol.anchors",
		"--protocol.script-enforced-lease",
	}

	// CfgSimpleTaproot specifies the config used to create a node that
	// uses the simple taproot feature.
	CfgSimpleTaproot = []string{
		"--protocol.anchors",
		"--protocol.simple-taproot-chans",
	}

	// CfgRbfCoopClose specifies the config used to create a node that
	// supports the new RBF close protocol.
	CfgRbfClose = []string{
		"--protocol.rbf-coop-close",
	}

	// CfgZeroConf specifies the config used to create a node that uses the
	// zero-conf channel feature.
	CfgZeroConf = []string{
		"--protocol.anchors",
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
	}
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

	// P2PAddr return bitcoin p2p ip:port.
	P2PAddr() (string, error)
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

	SkipUnlock        bool
	Password          []byte
	WithPeerBootstrap bool

	P2PPort     int
	RPCPort     int
	RESTPort    int
	ProfilePort int

	FeeURL string

	DBBackend   DatabaseBackend
	PostgresDsn string
	NativeSQL   bool

	// NodeID is a unique ID used to identify the node.
	NodeID uint32

	// LndBinary is the full path to the lnd binary that was specifically
	// compiled with all required itest flags.
	LndBinary string

	// SkipCleanup specifies whether the harness will remove the base dir or
	// not when the test finishes. When using customized BaseDir, the
	// cleanup will be skipped.
	SkipCleanup bool

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
		cfg.DataDir, "chain", lnd.BitcoinChainName,
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
		cfg.P2PPort = port.NextAvailablePort()
	}
	if cfg.RPCPort == 0 {
		cfg.RPCPort = port.NextAvailablePort()
	}
	if cfg.RESTPort == 0 {
		cfg.RESTPort = port.NextAvailablePort()
	}
	if cfg.ProfilePort == 0 {
		cfg.ProfilePort = port.NextAvailablePort()
	}
}

// BaseConfig returns the base node configuration struct.
func (cfg *BaseNodeConfig) BaseConfig() *BaseNodeConfig {
	return cfg
}

// GenBaseDir creates a base dir that's used for the test.
func (cfg *BaseNodeConfig) GenBaseDir() error {
	// Exit early if the BaseDir is already set.
	if cfg.BaseDir != "" {
		return nil
	}

	dirBaseName := fmt.Sprintf("itest-%v-%v-%v-%v", cfg.LogFilenamePrefix,
		cfg.Name, cfg.NodeID, time.Now().Unix())

	// Create a temporary directory for the node's data and logs. Use dash
	// suffix as a separator between base name and node ID.
	if *baseDirFlag == "" {
		var err error

		cfg.BaseDir, err = os.MkdirTemp("", dirBaseName)

		return err
	}

	// Create the customized base dir.
	if err := os.MkdirAll(*baseDirFlag, 0700); err != nil {
		return err
	}

	// Use customized base dir and skip the cleanups.
	cfg.BaseDir = filepath.Join(*baseDirFlag, dirBaseName)
	cfg.SkipCleanup = true

	return nil
}

// GenArgs generates a slice of command line arguments from the lightning node
// config struct.
func (cfg *BaseNodeConfig) GenArgs() []string {
	var args []string

	switch cfg.NetParams {
	case &chaincfg.TestNet3Params:
		args = append(args, "--bitcoin.testnet")
	case &chaincfg.TestNet4Params:
		args = append(args, "--bitcoin.testnet4")
	case &chaincfg.SimNetParams:
		args = append(args, "--bitcoin.simnet")
	case &chaincfg.RegressionNetParams:
		args = append(args, "--bitcoin.regtest")
	}

	backendArgs := cfg.BackendCfg.GenArgs()
	args = append(args, backendArgs...)

	nodeArgs := []string{
		"--debuglevel=debug",
		"--bitcoin.defaultchanconfs=1",
		"--accept-keysend",
		"--keep-failed-payment-attempts",
		"--logging.no-commit-hash",
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

		// Use a small batch delay so we can broadcast the
		// announcements quickly in the tests.
		"--gossip.sub-batch-delay=5ms",

		// Use a small cache duration so the `DescribeGraph` can be
		// updated quicker.
		"--caches.rpc-graph-cache-duration=100ms",

		// Speed up the tests for bitcoind backend.
		"--bitcoind.blockpollinginterval=100ms",
		"--bitcoind.txpollinginterval=100ms",

		// Allow unsafe disconnect in itest.
		"--dev.unsafedisconnect",
	}

	args = append(args, nodeArgs...)

	if cfg.Password == nil {
		args = append(args, "--noseedbackup")
	}

	if !cfg.WithPeerBootstrap {
		args = append(args, "--nobootstrap")
	}

	switch cfg.DBBackend {
	case BackendEtcd:
		args = append(args, "--db.backend=etcd")
		args = append(args, "--db.etcd.embedded")
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_client_port=%v",
				port.NextAvailablePort(),
			),
		)
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_peer_port=%v",
				port.NextAvailablePort(),
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
		if cfg.NativeSQL {
			args = append(args, "--db.use-native-sql")
		}

	case BackendSqlite:
		args = append(args, "--db.backend=sqlite")
		args = append(args, fmt.Sprintf("--db.sqlite.busytimeout=%v",
			wait.SqliteBusyTimeout))
		if cfg.NativeSQL {
			args = append(args, "--db.use-native-sql")
		}
	}

	if cfg.FeeURL != "" {
		args = append(args, "--fee.url="+cfg.FeeURL)
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
		extraArgs = append(
			extraArgs, "--healthcheck.leader.interval=10s",
		)
	}

	return extraArgs
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

func init() {
	// Before we start any node, we need to make sure that any btcd or
	// bitcoind node that is started through the RPC harness uses a unique
	// port as well to avoid any port collisions.
	rpctest.ListenAddressGenerator =
		port.GenerateSystemUniqueListenerAddresses
}
