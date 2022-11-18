package node

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/lntest"
)

const (
	// ListenerFormat is the format string that is used to generate local
	// listener addresses.
	ListenerFormat = "127.0.0.1:%d"
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

	HasSeed  bool
	Password []byte

	P2PPort     int
	RPCPort     int
	RESTPort    int
	ProfilePort int

	FeeURL string

	DbBackend   lntest.DatabaseBackend
	PostgresDsn string

	// NodeID is a unique ID used to identify the node.
	NodeID uint32

	// LndBinary is the full path to the lnd binary that was specifically
	// compiled with all required itest flags.
	LndBinary string

	// backupDbDir is the path where a database backup is stored, if any.
	backupDbDir string

	// postgresDbName is the name of the postgres database where lnd data
	// is stored in.
	postgresDbName string
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
		cfg.P2PPort = lntest.NextAvailablePort()
	}
	if cfg.RPCPort == 0 {
		cfg.RPCPort = lntest.NextAvailablePort()
	}
	if cfg.RESTPort == 0 {
		cfg.RESTPort = lntest.NextAvailablePort()
	}
	if cfg.ProfilePort == 0 {
		cfg.ProfilePort = lntest.NextAvailablePort()
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
		"--accept-keysend",
		"--keep-failed-payment-attempts",
		fmt.Sprintf("--db.batch-commit-interval=%v", commitInterval),
		fmt.Sprintf("--bitcoin.defaultremotedelay=%v",
			lntest.DefaultCSV),
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

		// Use a small batch window so we can broadcast our sweep
		// transactions faster.
		"--sweeper.batchwindowduration=5s",
	}
	args = append(args, nodeArgs...)

	if !cfg.HasSeed {
		args = append(args, "--noseedbackup")
	}

	switch cfg.DbBackend {
	case lntest.BackendEtcd:
		args = append(args, "--db.backend=etcd")
		args = append(args, "--db.etcd.embedded")
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_client_port=%v",
				lntest.NextAvailablePort(),
			),
		)
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_peer_port=%v",
				lntest.NextAvailablePort(),
			),
		)
		args = append(
			args, fmt.Sprintf(
				"--db.etcd.embedded_log_file=%v",
				path.Join(cfg.LogDir, "etcd.log"),
			),
		)

	case lntest.BackendPostgres:
		args = append(args, "--db.backend=postgres")
		args = append(args, "--db.postgres.dsn="+cfg.PostgresDsn)
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
