package main

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	flags "github.com/btcsuite/go-flags"
)

const (
	defaultConfigFilename = "lnwallet.conf"
	defaultDataDirname    = "test_wal"
	defaultRPCPort        = 10009
	defaultSPVMode        = false
	defaultPeerPort       = 10011
	defaultBTCDHost       = "localhost:18334"
	defaultBTCDUser       = "user"
	defaultBTCDPass       = "passwd"
	defaultBTCDCACertPath = ""
	defaultUseRegtest     = false
	defaultSPVHostAdr     = "localhost:18333"
	defaultBTCDNoTLS      = false
)

var (
	lnwalletHomeDir   = btcutil.AppDataDir("lnwallet", false)
	defaultConfigFile = filepath.Join(lnwalletHomeDir, defaultConfigFilename)
	defaultDataDir    = filepath.Join(lnwalletHomeDir, defaultDataDirname)
)

type config struct {
	ConfigFile     string `short:"C" long:"configfile" description:"Path to configuration file"`
	DataDir        string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	PeerPort       int    `long:"peerport" description:"The port to listen on for incoming p2p connections"`
	RPCPort        int    `long:"rpcport" description:"The port for the rpc server"`
	SPVMode        bool   `long:"spv" description:"assert to enter spv wallet mode"`
	BTCDHost       string `long:"btcdhost" description:"The BTCD RPC address. "`
	BTCDUser       string `long:"btcduser" description:"The BTCD RPC user"`
	BTCDPass       string `long:"btcdpass" description:"The BTCD RPC password"`
	BTCDNoTLS      bool   `long:"btcdnotls" description:"Do not use TLS for RPC connection to BTCD"`
	BTCDCACertPath string `long:"btcdcacert" description:"Path to certificate for BTCD RPC"`
	UseRegtest     bool   `long:"regtest" description:"Use RegNet. If not specified TestNet3 is used"`
	SPVHostAdr     string `long:"spvhostadr" description:"Address of full bitcoin node. It is used in SPV mode."`
	NetParams      *chaincfg.Params
	BTCDCACert     []byte
}

// loadConfig initializes and parses the config using a config file and command
// line options.
//
// The configuration proceeds as follows:
// 	1) Start with a default config with sane settings
// 	2) Pre-parse the command line to check for an alternative config file
// 	3) Load configuration file overwriting defaults with any specified options
// 	4) Parse CLI options and overwrite/add any specified options
func loadConfig() (*config, error) {
	defaultCfg := config{
		ConfigFile:     defaultConfigFile,
		DataDir:        defaultDataDir,
		PeerPort:       defaultPeerPort,
		RPCPort:        defaultRPCPort,
		SPVMode:        defaultSPVMode,
		BTCDHost:       defaultBTCDHost,
		BTCDUser:       defaultBTCDUser,
		BTCDPass:       defaultBTCDPass,
		BTCDNoTLS:      defaultBTCDNoTLS,
		BTCDCACertPath: defaultBTCDCACertPath,
		UseRegtest:     defaultUseRegtest,
		SPVHostAdr:     defaultSPVHostAdr,
	}
	preCfg := defaultCfg
	_, err := flags.Parse(&preCfg)
	if err != nil {
		return nil, err
	}
	cfg := defaultCfg
	err = flags.IniParse(preCfg.ConfigFile, &cfg)
	if err != nil {
		return nil, err
	}
	_, err = flags.Parse(&cfg)
	//	Determine net parameters
	if cfg.UseRegtest {
		cfg.NetParams = &chaincfg.RegressionNetParams
	} else {
		cfg.NetParams = &chaincfg.TestNet3Params
	}
	//	Read certificate if needed
	if cfg.BTCDCACertPath != "" {
		f, err := os.Open(cfg.BTCDCACertPath)
		defer f.Close()
		if err != nil {
			return nil, err
		}
		cert, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}
		cfg.BTCDCACert = cert
	}

	return &cfg, nil
}
