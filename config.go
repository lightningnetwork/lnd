package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	flags "github.com/btcsuite/go-flags"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

const (
	defaultConfigFilename     = "lnd.conf"
	defaultDataDirname        = "data"
	defaultLogLevel           = "info"
	defaultLogDirname         = "logs"
	defaultLogFilename        = "lnd.log"
	defaultRPCPort            = 10009
	defaultSPVMode            = false
	defaultPeerPort           = 10011
	defaultRPCHost            = "localhost"
	defaultRPCUser            = "user"
	defaultRPCPass            = "passwd"
	defaultSPVHostAdr         = "localhost:18333"
	defaultMaxPendingChannels = 1
)

var (
	lndHomeDir        = btcutil.AppDataDir("lnd", false)
	defaultConfigFile = filepath.Join(lndHomeDir, defaultConfigFilename)
	defaultDataDir    = filepath.Join(lndHomeDir, defaultDataDirname)
	defaultLogDir     = filepath.Join(lndHomeDir, defaultLogDirname)

	btcdHomeDir        = btcutil.AppDataDir("btcd", false)
	defaultRPCCertFile = filepath.Join(btcdHomeDir, "rpc.cert")
)

// config defines the configuration options for lnd.
//
// See loadConfig for further details regarding the configuration
// loading+parsing process.
type config struct {
	ShowVersion bool `short:"V" long:"version" description:"Display version information and exit"`

	ConfigFile string `long:"C" long:"configfile" description:"Path to configuration file"`
	DataDir    string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	LogDir     string `long:"logdir" description:"Directory to log output."`

	Listeners   []string `long:"listen" description:"Add an interface/port to listen for connections (default all interfaces port: 10011)"`
	ExternalIPs []string `long:"externalip" description:"Add an ip to the list of local addresses we claim to listen on to peers"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	Profile string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65536"`

	PeerPort int    `long:"peerport" description:"The port to listen on for incoming p2p connections"`
	RPCPort  int    `long:"rpcport" description:"The port for the rpc server"`
	SPVMode  bool   `long:"spv" description:"assert to enter spv wallet mode"`
	RPCHost  string `long:"btcdhost" description:"The btcd rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser  string `short:"u" long:"rpcuser" description:"Username for RPC connections"`
	RPCPass  string `short:"P" long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`

	RPCCert            string `long:"rpccert" description:"File containing btcd's certificate file"`
	RawRPCCert         string `long:"rawrpccert" description:"The raw bytes of btcd's PEM-encoded certificate chain which will be used to authenticate the RPC connection."`
	SPVHostAdr         string `long:"spvhostadr" description:"Address of full bitcoin node. It is used in SPV mode."`
	TestNet3           bool   `long:"testnet" description:"Use the test network"`
	SimNet             bool   `long:"simnet" description:"Use the simulation test network"`
	DebugHTLC          bool   `long:"debughtlc" description:"Activate the debug htlc mode. With the debug HTLC mode, all payments sent use a pre-determined R-Hash. Additionally, all HTLCs sent to a node with the debug HTLC R-Hash are immediately settled in the next available state transition."`
	MaxPendingChannels int    `long:"maxpendingchannels" description:"The maximum number of incoming pending channels permitted per peer."`
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
		ConfigFile:         defaultConfigFile,
		DataDir:            defaultDataDir,
		DebugLevel:         defaultLogLevel,
		LogDir:             defaultLogDir,
		PeerPort:           defaultPeerPort,
		RPCPort:            defaultRPCPort,
		SPVMode:            defaultSPVMode,
		RPCHost:            defaultRPCHost,
		RPCUser:            defaultRPCUser,
		RPCPass:            defaultRPCPass,
		RPCCert:            defaultRPCCertFile,
		SPVHostAdr:         defaultSPVHostAdr,
		MaxPendingChannels: defaultMaxPendingChannels,
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
	cfg := defaultCfg
	if err := flags.IniParse(preCfg.ConfigFile, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	if _, err := flags.Parse(&cfg); err != nil {
		return nil, err
	}

	// Multiple networks can't be selected simultaneously.
	// Count number of network flags passed; assign active network params
	// while we're at it.
	numNets := 0
	if cfg.TestNet3 {
		numNets++
		activeNetParams = testNetParams
	}
	if cfg.SimNet {
		numNets++
		activeNetParams = simNetParams
	}
	if numNets > 1 {
		str := "%s: The testnet, segnet, and simnet params can't be " +
			"used together -- choose one of the three"
		err := fmt.Errorf(str, funcName)
		return nil, err
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

	// Append the network type to the data directory so it is "namespaced"
	// per network. In addition to the block database, there are other
	// pieces of data that are saved to disk such as address manager state.
	// All data is specific to a network, so namespacing the data directory
	// means each individual piece of serialized data does not have to
	// worry about changing names per network and such.
	cfg.DataDir = cleanAndExpandPath(cfg.DataDir)
	cfg.DataDir = filepath.Join(cfg.DataDir, activeNetParams.Name)

	// Append the network type to the log directory so it is "namespaced"
	// per network in the same fashion as the data directory.
	cfg.LogDir = cleanAndExpandPath(cfg.LogDir)
	cfg.LogDir = filepath.Join(cfg.LogDir, activeNetParams.Name)

	// Initialize logging at the default logging level.
	initSeelogLogger(filepath.Join(cfg.LogDir, defaultLogFilename))
	setLogLevels(defaultLogLevel)

	// Parse, validate, and set debug log level(s).
	if err := parseAndSetDebugLevels(cfg.DebugLevel); err != nil {
		err := fmt.Errorf("%s: %v", funcName, err.Error())
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, err
	}

	return &cfg, nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
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
