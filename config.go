// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	flags "github.com/btcsuite/go-flags"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

const (
	defaultConfigFilename     = "lnd.conf"
	defaultDataDirname        = "data"
	defaultTLSCertFilename    = "tls.cert"
	defaultTLSKeyFilename     = "tls.key"
	defaultAdminMacFilename   = "admin.macaroon"
	defaultReadMacFilename    = "readonly.macaroon"
	defaultLogLevel           = "info"
	defaultLogDirname         = "logs"
	defaultLogFilename        = "lnd.log"
	defaultRPCPort            = 10009
	defaultRESTPort           = 8080
	defaultPeerPort           = 9735
	defaultRPCHost            = "localhost"
	defaultMaxPendingChannels = 1
	defaultNumChanConfs       = 3
	defaultNoEncryptWallet    = false
	defaultTrickleDelay       = 30 * 1000
)

var (
	// TODO(roasbeef): base off of datadir instead?
	lndHomeDir          = btcutil.AppDataDir("lnd", false)
	defaultConfigFile   = filepath.Join(lndHomeDir, defaultConfigFilename)
	defaultDataDir      = filepath.Join(lndHomeDir, defaultDataDirname)
	defaultTLSCertPath  = filepath.Join(lndHomeDir, defaultTLSCertFilename)
	defaultTLSKeyPath   = filepath.Join(lndHomeDir, defaultTLSKeyFilename)
	defaultAdminMacPath = filepath.Join(lndHomeDir, defaultAdminMacFilename)
	defaultReadMacPath  = filepath.Join(lndHomeDir, defaultReadMacFilename)
	defaultLogDir       = filepath.Join(lndHomeDir, defaultLogDirname)

	btcdHomeDir            = btcutil.AppDataDir("btcd", false)
	defaultBtcdRPCCertFile = filepath.Join(btcdHomeDir, "rpc.cert")

	ltcdHomeDir            = btcutil.AppDataDir("ltcd", false)
	defaultLtcdRPCCertFile = filepath.Join(ltcdHomeDir, "rpc.cert")
)

type chainConfig struct {
	Active   bool   `long:"active" description:"If the chain should be active or not."`
	ChainDir string `long:"chaindir" description:"The directory to store the chains's data within."`

	RPCHost    string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser    string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass    string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	RPCCert    string `long:"rpccert" description:"File containing the daemon's certificate file"`
	RawRPCCert string `long:"rawrpccert" description:"The raw bytes of the daemon's PEM-encoded certificate chain which will be used to authenticate the RPC connection."`

	TestNet3 bool `long:"testnet" description:"Use the test network"`
	SimNet   bool `long:"simnet" description:"Use the simulation test network"`
	RegTest  bool `long:"regtest" description:"Use the regression test network"`
}

type neutrinoConfig struct {
	Active       bool          `long:"active" description:"If SPV mode should be active or not."`
	AddPeers     []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers     int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration  time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
}

type autoPilotConfig struct {
	// TODO(roasbeef): add
	Active      bool    `long:"active" description:"If the autopilot agent should be active or not."`
	MaxChannels int     `long:"maxchannels" description:"The maximum number of channels that should be created"`
	Allocation  float64 `long:"allocation" description:"The percentage of total funds that should be committed to automatic channel establishment"`
}

// config defines the configuration options for lnd.
//
// See loadConfig for further details regarding the configuration
// loading+parsing process.
type config struct {
	ShowVersion bool `short:"V" long:"version" description:"Display version information and exit"`

	ConfigFile   string `long:"C" long:"configfile" description:"Path to configuration file"`
	DataDir      string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	TLSCertPath  string `long:"tlscertpath" description:"Path to TLS certificate for lnd's RPC and REST services"`
	TLSKeyPath   string `long:"tlskeypath" description:"Path to TLS private key for lnd's RPC and REST services"`
	NoMacaroons  bool   `long:"no-macaroons" description:"Disable macaroon authentication"`
	AdminMacPath string `long:"adminmacaroonpath" description:"Path to write the admin macaroon for lnd's RPC and REST services if it doesn't exist"`
	ReadMacPath  string `long:"readonlymacaroonpath" description:"Path to write the read-only macaroon for lnd's RPC and REST services if it doesn't exist"`
	LogDir       string `long:"logdir" description:"Directory to log output."`

	Listeners   []string `long:"listen" description:"Add an interface/port to listen for connections (default all interfaces port: 9735)"`
	ExternalIPs []string `long:"externalip" description:"Add an ip to the list of local addresses we claim to listen on to peers"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	CPUProfile string `long:"cpuprofile" description:"Write CPU profile to the specified file"`

	Profile string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65536"`

	PeerPort           int  `long:"peerport" description:"The port to listen on for incoming p2p connections"`
	RPCPort            int  `long:"rpcport" description:"The port for the rpc server"`
	RESTPort           int  `long:"restport" description:"The port for the REST server"`
	DebugHTLC          bool `long:"debughtlc" description:"Activate the debug htlc mode. With the debug HTLC mode, all payments sent use a pre-determined R-Hash. Additionally, all HTLCs sent to a node with the debug HTLC R-Hash are immediately settled in the next available state transition."`
	HodlHTLC           bool `long:"hodlhtlc" description:"Activate the hodl HTLC mode.  With hodl HTLC mode, all incoming HTLCs will be accepted by the receiving node, but no attempt will be made to settle the payment with the sender."`
	MaxPendingChannels int  `long:"maxpendingchannels" description:"The maximum number of incoming pending channels permitted per peer."`

	Litecoin *chainConfig `group:"Litecoin" namespace:"litecoin"`
	Bitcoin  *chainConfig `group:"Bitcoin" namespace:"bitcoin"`

	DefaultNumChanConfs int `long:"defaultchanconfs" description:"The default number of confirmations a channel must have before it's considered open."`

	NeutrinoMode *neutrinoConfig `group:"neutrino" namespace:"neutrino"`

	Autopilot *autoPilotConfig `group:"autopilot" namespace:"autopilot"`

	NoNetBootstrap bool `long:"nobootstrap" description:"If true, then automatic network bootstrapping will not be attempted."`

	NoEncryptWallet bool `long:"noencryptwallet" description:"If set, wallet will be encrypted using the default passphrase."`

	TrickleDelay int `long:"trickledelay" description:"Time in milliseconds between each release of announcements to the network"`
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
		ConfigFile:          defaultConfigFile,
		DataDir:             defaultDataDir,
		DebugLevel:          defaultLogLevel,
		TLSCertPath:         defaultTLSCertPath,
		TLSKeyPath:          defaultTLSKeyPath,
		AdminMacPath:        defaultAdminMacPath,
		ReadMacPath:         defaultReadMacPath,
		LogDir:              defaultLogDir,
		PeerPort:            defaultPeerPort,
		RPCPort:             defaultRPCPort,
		RESTPort:            defaultRESTPort,
		MaxPendingChannels:  defaultMaxPendingChannels,
		DefaultNumChanConfs: defaultNumChanConfs,
		NoEncryptWallet:     defaultNoEncryptWallet,
		Bitcoin: &chainConfig{
			RPCHost: defaultRPCHost,
			RPCCert: defaultBtcdRPCCertFile,
		},
		Litecoin: &chainConfig{
			RPCHost: defaultRPCHost,
			RPCCert: defaultLtcdRPCCertFile,
		},
		Autopilot: &autoPilotConfig{
			MaxChannels: 5,
			Allocation:  0.6,
		},
		TrickleDelay: defaultTrickleDelay,
	}

	// Pre-parse the command line options to pick up an alternative config
	// file.
	preCfg := defaultCfg
	if _, err := flags.Parse(&preCfg); err != nil {
		return nil, err
	}

	// Show the version and exit if the version flag was specified.
	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	usageMessage := fmt.Sprintf("Use %s -h to show usage", appName)
	if preCfg.ShowVersion {
		fmt.Println(appName, "version", version())
		os.Exit(0)
	}

	// Create the home directory if it doesn't already exist.
	funcName := "loadConfig"
	if err := os.MkdirAll(lndHomeDir, 0700); err != nil {
		// Show a nicer error message if it's because a symlink is
		// linked to a directory that does not exist (probably because
		// it's not mounted).
		if e, ok := err.(*os.PathError); ok && os.IsExist(err) {
			if link, lerr := os.Readlink(e.Path); lerr == nil {
				str := "is symlink %s -> %s mounted?"
				err = fmt.Errorf(str, e.Path, link)
			}
		}

		str := "%s: Failed to create home directory: %v"
		err := fmt.Errorf(str, funcName, err)
		fmt.Fprintln(os.Stderr, err)
		return nil, err
	}

	// Next, load any additional configuration options from the file.
	var configFileError error
	cfg := defaultCfg
	if err := flags.IniParse(preCfg.ConfigFile, &cfg); err != nil {
		configFileError = err
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	if _, err := flags.Parse(&cfg); err != nil {
		return nil, err
	}

	// At this moment, multiple active chains are not supported.
	if cfg.Litecoin.Active && cfg.Bitcoin.Active {
		str := "%s: Currently both Bitcoin and Litecoin cannot be " +
			"active together"
		err := fmt.Errorf(str, funcName)
		return nil, err
	}

	switch {
	// The SPV mode implemented currently doesn't support Litecoin, so the
	// two modes are incompatible.
	case cfg.NeutrinoMode.Active && cfg.Litecoin.Active:
		str := "%s: The light client mode currently supported does " +
			"not yet support execution on the Litecoin network"
		err := fmt.Errorf(str, funcName)
		return nil, err

	// Either Bitcoin must be active, or Litecoin must be active.
	// Otherwise, we don't know which chain we're on.
	case !cfg.Bitcoin.Active && !cfg.Litecoin.Active:
		return nil, fmt.Errorf("either bitcoin.active or " +
			"litecoin.active must be set to 1 (true)")

	case cfg.Litecoin.Active:
		if cfg.Litecoin.SimNet {
			str := "%s: simnet mode for litecoin not currently supported"
			return nil, fmt.Errorf(str, funcName)
		}

		// The litecoin chain is the current active chain. However
		// throughout the codebase we required chiancfg.Params. So as a
		// temporary hack, we'll mutate the default net params for
		// bitcoin with the litecoin specific information.
		paramCopy := bitcoinTestNetParams
		applyLitecoinParams(&paramCopy)
		activeNetParams = paramCopy

		if !cfg.NeutrinoMode.Active {
			// Attempt to parse out the RPC credentials for the
			// litecoin chain if the information wasn't specified
			err := parseRPCParams(cfg.Litecoin, litecoinChain, funcName)
			if err != nil {
				err := fmt.Errorf("unable to load RPC credentials for "+
					"ltcd: %v", err)
				return nil, err
			}
		}

		cfg.Litecoin.ChainDir = filepath.Join(cfg.DataDir, litecoinChain.String())

		// Finally we'll register the litecoin chain as our current
		// primary chain.
		registeredChains.RegisterPrimaryChain(litecoinChain)

	case cfg.Bitcoin.Active:
		// Multiple networks can't be selected simultaneously.  Count
		// number of network flags passed; assign active network params
		// while we're at it.
		numNets := 0
		if cfg.Bitcoin.TestNet3 {
			numNets++
			activeNetParams = bitcoinTestNetParams
		}
		if cfg.Bitcoin.RegTest {
			numNets++
			activeNetParams = regTestNetParams
		}
		if cfg.Bitcoin.SimNet {
			numNets++
			activeNetParams = bitcoinSimNetParams
		}
		if numNets > 1 {
			str := "%s: The testnet, segnet, and simnet params can't be " +
				"used together -- choose one of the three"
			err := fmt.Errorf(str, funcName)
			return nil, err
		}

		if !cfg.NeutrinoMode.Active {
			// If needed, we'll attempt to automatically configure
			// the RPC control plan for the target btcd node.
			err := parseRPCParams(cfg.Bitcoin, bitcoinChain, funcName)
			if err != nil {
				err := fmt.Errorf("unable to load RPC credentials for "+
					"btcd: %v", err)
				return nil, err
			}
		}

		cfg.Bitcoin.ChainDir = filepath.Join(cfg.DataDir, bitcoinChain.String())

		// Finally we'll register the bitcoin chain as our current
		// primary chain.
		registeredChains.RegisterPrimaryChain(bitcoinChain)
	}

	// Validate profile port number.
	if cfg.Profile != "" {
		profilePort, err := strconv.Atoi(cfg.Profile)
		if err != nil || profilePort < 1024 || profilePort > 65535 {
			str := "%s: The profile port must be between 1024 and 65535"
			err := fmt.Errorf(str, funcName)
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, usageMessage)
			return nil, err
		}
	}

	// At this point, we'll save the base data directory in order to ensure
	// we don't store the macaroon database within any of the chain
	// namespaced directories.
	macaroonDatabaseDir = cfg.DataDir

	// If a custom macaroon directory wasn't specified and the data
	// directory has changed from the default path, then we'll also update
	// the path for the macaroons to be generated.
	if cfg.DataDir != defaultDataDir && cfg.AdminMacPath == defaultAdminMacPath {
		cfg.AdminMacPath = filepath.Join(cfg.DataDir, defaultAdminMacFilename)
	}
	if cfg.DataDir != defaultDataDir && cfg.ReadMacPath == defaultReadMacPath {
		cfg.ReadMacPath = filepath.Join(cfg.DataDir, defaultReadMacFilename)
	}

	// Append the network type to the data directory so it is "namespaced"
	// per network. In addition to the block database, there are other
	// pieces of data that are saved to disk such as address manager state.
	// All data is specific to a network, so namespacing the data directory
	// means each individual piece of serialized data does not have to
	// worry about changing names per network and such.
	// TODO(roasbeef): when we go full multi-chain remove the additional
	// namespacing on the target chain.
	cfg.DataDir = cleanAndExpandPath(cfg.DataDir)
	cfg.DataDir = filepath.Join(cfg.DataDir, activeNetParams.Name)
	cfg.DataDir = filepath.Join(cfg.DataDir,
		registeredChains.primaryChain.String())

	// Append the network type to the log directory so it is "namespaced"
	// per network in the same fashion as the data directory.
	cfg.LogDir = cleanAndExpandPath(cfg.LogDir)
	cfg.LogDir = filepath.Join(cfg.LogDir, activeNetParams.Name)
	cfg.LogDir = filepath.Join(cfg.LogDir,
		registeredChains.primaryChain.String())

	// Ensure that the paths to the TLS key and certificate files are
	// expanded and cleaned.
	cfg.TLSCertPath = cleanAndExpandPath(cfg.TLSCertPath)
	cfg.TLSKeyPath = cleanAndExpandPath(cfg.TLSKeyPath)

	// Initialize logging at the default logging level.
	initLogRotator(filepath.Join(cfg.LogDir, defaultLogFilename))

	// Parse, validate, and set debug log level(s).
	if err := parseAndSetDebugLevels(cfg.DebugLevel); err != nil {
		err := fmt.Errorf("%s: %v", funcName, err.Error())
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, err
	}

	// Warn about missing config file only after all other configuration is
	// done.  This prevents the warning on help messages and invalid
	// options.  Note this should go directly before the return.
	if configFileError != nil {
		ltndLog.Warnf("%v", configFileError)
	}

	return &cfg, nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		homeDir := filepath.Dir(lndHomeDir)
		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}

// parseAndSetDebugLevels attempts to parse the specified debug level and set
// the levels accordingly. An appropriate error is returned if anything is
// invalid.
func parseAndSetDebugLevels(debugLevel string) error {
	// When the specified string doesn't have any delimters, treat it as
	// the log level for all subsystems.
	if !strings.Contains(debugLevel, ",") && !strings.Contains(debugLevel, "=") {
		// Validate debug log level.
		if !validLogLevel(debugLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, debugLevel)
		}

		// Change the logging level for all subsystems.
		setLogLevels(debugLevel)

		return nil
	}

	// Split the specified string into subsystem/level pairs while detecting
	// issues and update the log levels accordingly.
	for _, logLevelPair := range strings.Split(debugLevel, ",") {
		if !strings.Contains(logLevelPair, "=") {
			str := "The specified debug level contains an invalid " +
				"subsystem/level pair [%v]"
			return fmt.Errorf(str, logLevelPair)
		}

		// Extract the specified subsystem and log level.
		fields := strings.Split(logLevelPair, "=")
		subsysID, logLevel := fields[0], fields[1]

		// Validate subsystem.
		if _, exists := subsystemLoggers[subsysID]; !exists {
			str := "The specified subsystem [%v] is invalid -- " +
				"supported subsytems %v"
			return fmt.Errorf(str, subsysID, supportedSubsystems())
		}

		// Validate log level.
		if !validLogLevel(logLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, logLevel)
		}

		setLogLevel(subsysID, logLevel)
	}

	return nil
}

// validLogLevel returns whether or not logLevel is a valid debug log level.
func validLogLevel(logLevel string) bool {
	switch logLevel {
	case "trace":
		fallthrough
	case "debug":
		fallthrough
	case "info":
		fallthrough
	case "warn":
		fallthrough
	case "error":
		fallthrough
	case "critical":
		return true
	}
	return false
}

// supportedSubsystems returns a sorted slice of the supported subsystems for
// logging purposes.
func supportedSubsystems() []string {
	// Convert the subsystemLoggers map keys to a slice.
	subsystems := make([]string, 0, len(subsystemLoggers))
	for subsysID := range subsystemLoggers {
		subsystems = append(subsystems, subsysID)
	}

	// Sort the subsystems for stable display.
	sort.Strings(subsystems)
	return subsystems
}

// noiseDial is a factory function which creates a connmgr compliant dialing
// function by returning a closure which includes the server's identity key.
func noiseDial(idPriv *btcec.PrivateKey) func(net.Addr) (net.Conn, error) {
	return func(a net.Addr) (net.Conn, error) {
		lnAddr := a.(*lnwire.NetAddress)
		return brontide.Dial(idPriv, lnAddr)
	}
}

func parseRPCParams(cConfig *chainConfig, net chainCode, funcName string) error {
	// If the rpcuser and rpcpass parameters aren't set, then we'll attempt
	// to automatically obtain the proper credentials for btcd and set
	// them within the configuration.
	if cConfig.RPCUser != "" || cConfig.RPCPass != "" {
		return nil
	}

	// If we're in simnet mode, then the running btcd instance won't read
	// the RPC credentials from the configuration. So if lnd wasn't
	// specified the parameters, then we won't be able to start.
	if cConfig.SimNet {
		str := "%v: rpcuser and rpcpass must be set to your btcd " +
			"node's RPC parameters for simnet mode"
		return fmt.Errorf(str, funcName)
	}

	daemonName := "btcd"
	if net == litecoinChain {
		daemonName = "ltcd"
	}
	fmt.Println("Attempting automatic RPC configuration to " + daemonName)

	homeDir := btcdHomeDir
	if net == litecoinChain {
		homeDir = ltcdHomeDir
	}
	confFile := filepath.Join(homeDir, fmt.Sprintf("%v.conf", daemonName))
	rpcUser, rpcPass, err := extractRPCParams(confFile)
	if err != nil {
		return fmt.Errorf("unable to extract RPC "+
			"credentials: %v, cannot start w/o RPC connection",
			err)
	}

	fmt.Printf("Automatically obtained %v's RPC credentials\n", daemonName)
	cConfig.RPCUser, cConfig.RPCPass = rpcUser, rpcPass
	return nil
}

// extractRPCParams attempts to extract the RPC credentials for an existing
// btcd instance. The passed path is expected to be the location of btcd's
// application data directory on the target system.
func extractRPCParams(btcdConfigPath string) (string, string, error) {
	// First, we'll open up the btcd configuration file found at the target
	// destination.
	btcdConfigFile, err := os.Open(btcdConfigPath)
	if err != nil {
		return "", "", err
	}
	defer btcdConfigFile.Close()

	// With the file open extract the contents of the configuration file so
	// we can attempt o locate the RPC credentials.
	configContents, err := ioutil.ReadAll(btcdConfigFile)
	if err != nil {
		return "", "", err
	}

	// Attempt to locate the RPC user using a regular expression. If we
	// don't have a match for our regular expression then we'll exit with
	// an error.
	rpcUserRegexp, err := regexp.Compile(`(?m)^\s*rpcuser=([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	userSubmatches := rpcUserRegexp.FindSubmatch(configContents)
	if userSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	// Similarly, we'll use another regular expression to find the set
	// rpcpass (if any). If we can't find the pass, then we'll exit with an
	// error.
	rpcPassRegexp, err := regexp.Compile(`(?m)^\s*rpcpass=([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	passSubmatches := rpcPassRegexp.FindSubmatch(configContents)
	if passSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	return string(userSubmatches[1]), string(passSubmatches[1]), nil
}
